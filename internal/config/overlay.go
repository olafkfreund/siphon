package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Item is one portal edit layered over the config file. YAML is the item body
// (the value under its name); Deleted is a tombstone that removes it.
// Callers map store.ConfigItem to Item (config does not import store).
type Item struct {
	Kind, Name, YAML string
	Deleted          bool
}

type Key struct{ Kind, Name string }

type Provenance string

const (
	FromFile     Provenance = "file"     // only in the file
	FromPortal   Provenance = "portal"   // only in the overlay
	FromOverride Provenance = "override" // in the file, replaced by the overlay
)

// Kinds are the editable top-level sections; server, limits and units never are.
// "rules" is a list keyed by name, the rest are maps.
var Kinds = map[string]bool{"sources": true, "agents": true, "routines": true, "credentials": true, "rules": true}

// Effective applies items to the file's YAML node tree (keeping key order and
// comments) and returns the merged YAML plus the provenance of every item.
// Items that tombstone a file item drop out of the map.
func Effective(file []byte, items []Item) ([]byte, map[Key]Provenance, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	dec := yaml.NewDecoder(bytes.NewReader(file))
	var doc yaml.Node
	if err := dec.Decode(&doc); err == nil {
		if doc.Kind != yaml.DocumentNode || doc.Content[0].Kind != yaml.MappingNode {
			return nil, nil, errors.New("config: top level must be a mapping")
		}
		root = doc.Content[0]
		if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
			return nil, nil, errors.New("parse config: multiple YAML documents are not supported")
		}
	} else if !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("parse config: %w", err)
	}

	prov := map[Key]Provenance{}
	for kind := range Kinds {
		if sec := mapGet(root, kind); sec != nil {
			for _, n := range itemNames(kind, sec) {
				prov[Key{kind, n}] = FromFile
			}
		}
	}
	for _, it := range items {
		if !Kinds[it.Kind] {
			return nil, nil, fmt.Errorf("overlay: kind %q is not editable", it.Kind)
		}
		k := Key{it.Kind, it.Name}
		var body *yaml.Node
		if !it.Deleted {
			var d yaml.Node
			if err := yaml.Unmarshal([]byte(it.YAML), &d); err != nil || d.Kind != yaml.DocumentNode {
				return nil, nil, fmt.Errorf("overlay: %s %q: invalid YAML %v", it.Kind, it.Name, err)
			}
			body = d.Content[0]
			if hasAnchorOrAlias(body) {
				// An anchor in an item could rebind one the file defines and
				// change server, limits or units when the tree is written out.
				return nil, nil, fmt.Errorf("overlay: %s %q: YAML anchors and aliases are not allowed in portal edits", it.Kind, it.Name)
			}
			if n := mapGet(body, "name"); it.Kind == "rules" && n != nil && n.Value != it.Name {
				return nil, nil, fmt.Errorf("overlay: rule name %q does not match item %q", n.Value, it.Name)
			}
		}
		sec := mapGet(root, it.Kind)
		if sec == nil || sec.Kind == yaml.ScalarNode { // absent or `kind:` (null)
			kind := yaml.MappingNode
			tag := "!!map"
			if it.Kind == "rules" {
				kind, tag = yaml.SequenceNode, "!!seq"
			}
			sec = &yaml.Node{Kind: kind, Tag: tag}
			mapSet(root, it.Kind, sec)
		}
		_, known := prov[k]
		if it.Kind == "rules" {
			setRule(sec, it.Name, body)
		} else {
			mapSetOrDel(sec, it.Name, body)
		}
		switch {
		case body == nil:
			delete(prov, k)
		case known && prov[k] != FromPortal:
			prov[k] = FromOverride
		default:
			prov[k] = FromPortal
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), prov, enc.Close()
}

// LoadWithOverlay is Load with items applied to the file first. Items may only
// change what the sandbox already bounds (see checkOverlay).
func LoadWithOverlay(path string, items []Item) (*Config, map[Key]Provenance, error) {
	return LoadWithOverlayStub(path, items, nil)
}

// LoadWithOverlayStub is LoadWithOverlay where stub maps secret refs to
// stand-in values, for validating items whose secret files are not written yet.
func LoadWithOverlayStub(path string, items []Item, stub map[string]string) (*Config, map[Key]Provenance, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	eff, prov, err := Effective(b, items)
	if err != nil {
		return nil, nil, err
	}
	fileCfg, err := Parse(b)
	if err != nil {
		return nil, nil, err
	}
	fileCfg.resolveDB(path)
	if err := checkOverlay(fileCfg, items, filepath.Join(filepath.Dir(fileCfg.Server.DB), "secrets")); err != nil {
		return nil, nil, err
	}
	c, err := parse(eff, stub)
	if err != nil {
		return nil, nil, err
	}
	c.resolveDB(path)
	if err := sameFixedSections(fileCfg, c); err != nil {
		return nil, nil, err
	}
	return c, prov, nil
}

// sameFixedSections is the backstop behind the item checks: server, limits and
// units can never differ from the file, whatever the overlay did to the YAML.
func sameFixedSections(file, eff *Config) error {
	if !reflect.DeepEqual(file.Server, eff.Server) || !reflect.DeepEqual(file.Limits, eff.Limits) || !reflect.DeepEqual(file.Units, eff.Units) {
		return errors.New("overlay: server, limits and units can only be changed in siphon.yaml")
	}
	return nil
}

func hasAnchorOrAlias(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return true
	}
	for _, c := range n.Content {
		if hasAnchorOrAlias(c) {
			return true
		}
	}
	return false
}

// A portal or API token holder must not widen the host's privileges, so an
// overlay item may not do what siphon.yaml alone is trusted to do:
// set a stdio MCP command, allow private addresses, turn agent egress
// restriction off, or point a secret at anything but its own stored secret.
// Each is allowed only if the file's same item already has the same value.
func checkOverlay(file *Config, items []Item, secretsDir string) error {
	dir := filepath.Clean(secretsDir)
	var errs []error // every problem, not just the first
	add := func(e error) { errs = append(errs, e) }
	for _, it := range items {
		if it.Deleted {
			continue
		}
		var refs, fileRefs map[string]string
		moved := false // the item now talks to a different provider or URL than the file's
		switch it.Kind {
		case "sources":
			var s Source
			if yaml.Unmarshal([]byte(it.YAML), &s) != nil {
				continue // Parse reports it
			}
			fs := file.Sources[it.Name]
			if fs == nil {
				fs = &Source{}
			}
			if len(s.Command) > 0 && !slices.Equal(s.Command, fs.Command) {
				add(errors.New("command (stdio MCP) can only be set in siphon.yaml"))
			}
			if _, ok := file.Server.MCPPackages[s.Package]; s.Package != "" && !ok {
				add(errors.New("package must be one listed in server.mcp_packages in siphon.yaml"))
			}
			// A package source's keys are bounded by the package; others must match the file's.
			for _, k := range sortedKeys(s.Env) {
				if s.Package != "" && !slices.Contains(file.Server.MCPPackages[s.Package].Env, k) {
					add(fmt.Errorf("env name %q is not one of package %q's env", k, s.Package))
				}
			}
			if s.Package == "" && !slices.Equal(sortedKeys(s.Env), sortedKeys(fs.Env)) {
				add(errors.New("env names (stdio MCP) can only be set in siphon.yaml; the portal may change their values"))
			}
			if s.AllowPrivate && !fs.AllowPrivate && !file.ServiceEndpoint(s.URL) {
				add(errors.New("allow_private can only be set in siphon.yaml, or for a host:port listed in server.services.private_endpoints"))
			}
			moved = !sameEndpoint(s.URL, fs.URL) || s.Package != fs.Package || (len(fs.Command) > 0 && !fs.cmdFromPkg && s.Package != "")
			refs, fileRefs = sourceRefs(&s), sourceRefs(fs)
		case "credentials":
			var c Credential
			if yaml.Unmarshal([]byte(it.YAML), &c) != nil {
				continue
			}
			fc := file.Credentials[it.Name]
			if fc == nil {
				fc = &Credential{}
			}
			if c.Provider == "aws" {
				// The daemon's own login is the portal's to spend only on the operator's list.
				if c.Profile != "" && c.Profile != fc.Profile && !slices.Contains(file.Server.AWS.Profiles, c.Profile) {
					add(fmt.Errorf("profile %s is not in server.aws.profiles in siphon.yaml", c.Profile))
				}
				ownKeys := c.AccessKeyID.isSet() && c.SecretAccessKey.isSet()
				if c.RoleARN != "" && c.RoleARN != fc.RoleARN && !ownKeys && !slices.Contains(file.Server.AWS.RoleARNs, c.RoleARN) {
					add(fmt.Errorf("role_arn %s is not in server.aws.role_arns in siphon.yaml; add it there, or give this credential its own access keys", c.RoleARN))
				}
			}
			moved = c.Provider != fc.Provider || !sameEndpoint(c.URL, fc.URL) ||
				c.Region != fc.Region || c.Profile != fc.Profile || c.RoleARN != fc.RoleARN || c.ExternalID != fc.ExternalID
			refs = map[string]string{"api_key": c.APIKey.Ref, "access_key_id": c.AccessKeyID.Ref, "secret_access_key": c.SecretAccessKey.Ref}
			fileRefs = map[string]string{"api_key": fc.APIKey.Ref, "access_key_id": fc.AccessKeyID.Ref, "secret_access_key": fc.SecretAccessKey.Ref}
		case "agents":
			var a Agent
			if yaml.Unmarshal([]byte(it.YAML), &a) != nil {
				continue
			}
			fa := file.Agents[it.Name]
			if fa == nil {
				fa = &Agent{}
			}
			if a.Egress.Enabled != nil && !*a.Egress.Enabled && (fa.Egress.Enabled == nil || *fa.Egress.Enabled) {
				add(errors.New("egress.enabled: false can only be set in siphon.yaml"))
			}
			// api_key_file is a path the runner reads: treat it as a file ref.
			refs, fileRefs = map[string]string{"api_key_file": fileRef(a.APIKeyFile)}, map[string]string{"api_key_file": fileRef(fa.APIKeyFile)}
		default:
			continue
		}
		for _, path := range sortedKeys(refs) {
			ref := refs[path]
			if moved && ref != "" && ref == fileRefs[path] {
				// Keeping the file's secret is fine, sending it somewhere new is not.
				add(errors.New("this key comes from siphon.yaml and can't be moved to another provider or URL"))
				continue
			}
			if !refAllowed(ref, fileRefs[path], it.Kind, it.Name, dir) {
				add(errors.New("secret refs can only point at this item's stored secrets or keep the value from siphon.yaml"))
			}
		}
	}
	return errors.Join(errs...)
}

// lookupHost resolves model endpoint hosts; tests replace it.
var lookupHost = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// sameEndpoint compares two URLs by scheme, host:port and path.
func sameEndpoint(a, b string) bool {
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil || eb != nil {
		return a == b
	}
	return ua.Scheme == ub.Scheme && strings.EqualFold(ua.Host, ub.Host) && ua.Path == ub.Path && ua.RawQuery == ub.RawQuery
}

// CheckModelEndpoint is run when a portal save changes a credentials item
// (never at load, so startup and validate do no DNS). It refuses an
// ollama/openai item whose host is (or does not provably stop being) private,
// loopback or link-local, unless this config lists its host:port. Unresolvable
// names fail closed. The runtime guard is the real control.
func (file *Config) CheckModelEndpoint(itemYAML string) error {
	var c Credential
	if yaml.Unmarshal([]byte(itemYAML), &c) != nil || !slices.Contains(modelProviders, c.Provider) {
		return nil // Parse reports it
	}
	host, port, err := ModelURL(c.URL)
	if err != nil {
		return nil // Validate reports it
	}
	if file.PrivateEndpoint(host, port) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := lookupHost(ctx, host)
	refuse := errors.New(`this endpoint is on a private network; add "host:port" to server.models.private_endpoints in siphon.yaml`)
	if err != nil || len(addrs) == 0 {
		return refuse
	}
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return refuse
		}
		ip = ip.Unmap()
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return refuse
		}
	}
	return nil
}

func fileRef(path string) string {
	if path == "" {
		return ""
	}
	return "file:" + path
}

func sourceRefs(s *Source) map[string]string {
	m := map[string]string{"secret": s.Secret.Ref}
	if s.Auth != nil {
		m["auth.bearer"] = s.Auth.Bearer.Ref
	}
	for k, v := range s.Headers {
		m["headers."+k] = v.Ref
	}
	for k, v := range s.Env {
		m["env."+k] = v.Ref
	}
	return m
}

// refAllowed: not a ref at all (Validate rejects inline values), the same ref
// the file has at this path, or a file in dir named <kind>-<name>-*.
func refAllowed(ref, fileRef, kind, name, dir string) bool {
	if !strings.HasPrefix(ref, "env:") && !strings.HasPrefix(ref, "file:") {
		return true
	}
	if ref == fileRef {
		return true
	}
	p, ok := strings.CutPrefix(ref, "file:")
	return ok && p == filepath.Clean(p) && filepath.Dir(p) == dir && strings.HasPrefix(filepath.Base(p), kind+"-"+name+"-")
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// mapSetOrDel sets key to v, or removes it when v is nil.
func mapSetOrDel(m *yaml.Node, key string, v *yaml.Node) {
	if v != nil {
		mapSet(m, key, v)
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func ruleName(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	if v := mapGet(n, "name"); v != nil {
		return v.Value
	}
	return ""
}

// setRule replaces (in place), appends, or (body nil) removes the rule called name.
func setRule(seq *yaml.Node, name string, body *yaml.Node) {
	if body != nil && mapGet(body, "name") == nil {
		mapSet(body, "name", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
	}
	for i, r := range seq.Content {
		if ruleName(r) == name {
			if body == nil {
				seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
			} else {
				seq.Content[i] = body
			}
			return
		}
	}
	if body != nil {
		seq.Content = append(seq.Content, body)
	}
}

func itemNames(kind string, sec *yaml.Node) (names []string) {
	if kind == "rules" {
		for _, r := range sec.Content {
			if n := ruleName(r); n != "" {
				names = append(names, n)
			}
		}
	} else if sec.Kind == yaml.MappingNode {
		for i := 0; i < len(sec.Content); i += 2 {
			names = append(names, sec.Content[i].Value)
		}
	}
	return names
}
