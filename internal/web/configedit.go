package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/olafkfreund/siphon/internal/catalog"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// itemName limits names to what is safe in a file name and a URL path.
var itemName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

var (
	errStale     = errors.New("changed since you opened it, reload")
	errNoEditing = errors.New("config editing is not available")
)

// errInvalid is a candidate config that fails to parse or validate.
type errInvalid struct{ msg string }

func (e errInvalid) Error() string { return e.msg }

// list is every problem: errors.Join separates them with newlines.
func (e errInvalid) list() []string { return strings.Split(e.msg, "\n") }

type itemKey = config.Key

func toItems(cs []store.ConfigItem) []config.Item {
	out := make([]config.Item, len(cs))
	for i, c := range cs {
		out[i] = config.Item{Kind: c.Kind, Name: c.Name, YAML: c.YAML, Deleted: c.Deleted}
	}
	return out
}

func (s *server) readFile() ([]byte, error) {
	if s.ConfigPath == "" {
		return nil, errNoEditing
	}
	return os.ReadFile(s.ConfigPath)
}

// overlay is the stored portal edits and the latest revision id (0 if none).
func (s *server) overlay() ([]store.ConfigItem, int64, error) {
	items, err := store.ConfigItems(s.Store.DB)
	if err != nil {
		return nil, 0, err
	}
	r, ok, err := store.LatestRevision(s.Store.DB)
	if !ok {
		return items, 0, err
	}
	return items, r.ID, err
}

// state is the effective YAML and the provenance of every item.
func (s *server) state() (eff []byte, prov map[itemKey]config.Provenance, items []store.ConfigItem, rev int64, err error) {
	file, err := s.readFile()
	if err != nil {
		return
	}
	if items, rev, err = s.overlay(); err != nil {
		return
	}
	eff, prov, err = config.Effective(file, toItems(items))
	return
}

// itemYAML is the YAML of one effective item (a rule without its name key).
func itemYAML(eff []byte, kind, name string) (string, bool) {
	var top map[string]any
	if yaml.Unmarshal(eff, &top) != nil {
		return "", false
	}
	var body any
	found := false
	switch sec := top[kind].(type) {
	case map[string]any:
		body, found = sec[name]
	case []any:
		for _, e := range sec {
			if m, ok := e.(map[string]any); ok && m["name"] == name {
				cp := map[string]any{}
				for k, v := range m {
					if k != "name" {
						cp[k] = v
					}
				}
				body, found = cp, true
			}
		}
	}
	if !found {
		return "", false
	}
	if body == nil {
		body = map[string]any{}
	}
	b, _ := yaml.Marshal(body)
	return string(b), true
}

// edit is a prepared change: the overlay after it, and what it does.
type edit struct {
	items []store.ConfigItem
	diff  string
	cfg   *config.Config
}

// prepare applies mutate to a copy of cur and checks the result the way
// startup would: overlay merge, Parse, Validate. Nothing is stored.
func (s *server) prepare(cur []store.ConfigItem, mutate func(map[itemKey]store.ConfigItem), pending []pendingSecret) (*edit, error) {
	file, err := s.readFile()
	if err != nil {
		return nil, err
	}
	m := map[itemKey]store.ConfigItem{}
	for _, c := range cur {
		m[itemKey{Kind: c.Kind, Name: c.Name}] = c
	}
	mutate(m)
	next := make([]store.ConfigItem, 0, len(m))
	for _, c := range m {
		next = append(next, c)
	}
	sort.Slice(next, func(i, j int) bool {
		return next[i].Kind+"\x00"+next[i].Name < next[j].Kind+"\x00"+next[j].Name
	})
	before, _, err := config.Effective(file, toItems(cur))
	if err != nil {
		return nil, err
	}
	// Pasted secrets are not on disk yet: validate with stand-in values.
	stub := map[string]string{}
	for _, p := range pending {
		stub["file:"+p.path(secretsDir(s.Config().Server.DB))] = "https://placeholder.invalid/" // a valid url for notify; other secrets only need a value
	}
	cfg, _, err := config.LoadWithOverlayStub(s.ConfigPath, toItems(next), stub, toItems(cur))
	if err != nil {
		return nil, errInvalid{err.Error()}
	}
	if err := cfg.Validate(); err != nil {
		return nil, errInvalid{err.Error()}
	}
	for n, src := range cfg.Sources {
		if src.Service != "" && catalog.Get(src.Service) == nil {
			return nil, errInvalid{"sources." + n + ".service: unknown catalogue service " + src.Service}
		}
	}
	for n, cr := range cfg.Credentials {
		if cr.Service != "" && catalog.Get(cr.Service) == nil {
			return nil, errInvalid{"credentials." + n + ".service: unknown catalogue service " + cr.Service}
		}
	}
	after, _, err := config.Effective(file, toItems(next))
	if err != nil {
		return nil, err
	}
	return &edit{items: next, diff: unifiedDiff(string(before), string(after)), cfg: cfg}, nil
}

// dryRun is prepare under the edit lock with the same stale check as commit:
// nothing is stored, written or applied.
func (s *server) dryRun(rev *int64, mutate func(map[itemKey]store.ConfigItem), pending []pendingSecret) (*edit, error) {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	cur, latest, err := s.overlay()
	if err != nil {
		return nil, err
	}
	if rev != nil && *rev != latest {
		return nil, errStale
	}
	e, err := s.prepare(cur, mutate, pending)
	if err == nil {
		_, err = s.credsCheck(cur, e.items)
	}
	return e, err
}

// credsCheck reports whether a credentials item changed. Items a save changes
// are checked for private model endpoints (DNS); loading never does, so a name
// that later turns private can't break startup.
func (s *server) credsCheck(cur, next []store.ConfigItem) (changed bool, err error) {
	for _, c := range next {
		if c.Kind != "credentials" {
			continue
		}
		var prev *store.ConfigItem
		for i := range cur {
			if cur[i].Kind == c.Kind && cur[i].Name == c.Name {
				prev = &cur[i]
			}
		}
		if prev != nil && prev.YAML == c.YAML && prev.Deleted == c.Deleted {
			continue
		}
		changed = true
		if !c.Deleted {
			if cerr := s.Config().CheckModelEndpoint(c.YAML); cerr != nil {
				return changed, errInvalid{cerr.Error()}
			}
		}
	}
	return changed, nil
}

// commit validates, then stores the changed overlay rows, a revision and an
// audit row in one transaction, then applies the new config. rev (if not nil)
// must be the latest revision id. applyErr is non-nil if the revision was
// stored but the live apply failed.
func (s *server) commit(actor, summary string, rev *int64, mutate func(map[itemKey]store.ConfigItem), pending []pendingSecret) (id int64, e *edit, applyErr error, err error) {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	cur, latest, err := s.overlay()
	if err != nil {
		return 0, nil, nil, err
	}
	if rev != nil && *rev != latest {
		return 0, nil, nil, errStale
	}
	if e, err = s.prepare(cur, mutate, pending); err != nil {
		return 0, nil, nil, err
	}
	credsChanged, err := s.credsCheck(cur, e.items)
	if err != nil {
		return 0, nil, nil, err
	}
	// Only now, with the revision current and the candidate valid, touch disk:
	// stage the secrets under temp names, publish them once the transaction
	// has committed, and remove them on any failure before that.
	type staged struct{ tmp, final string }
	var stagedFiles []staged
	published := false
	defer func() {
		if !published {
			for _, f := range stagedFiles {
				os.Remove(f.tmp)
			}
		}
	}()
	if len(pending) > 0 {
		dir := secretsDir(s.Config().Server.DB)
		real := map[string]string{}
		for _, p := range pending {
			tmp, serr := stageSecret(dir, p)
			if serr != nil {
				return 0, nil, nil, serr
			}
			stagedFiles = append(stagedFiles, staged{tmp, p.path(dir)})
			real["file:"+p.path(dir)] = strings.TrimSpace(p.Value)
		}
		// Re-load with the real values (not on disk until the commit).
		cfg, _, lerr := config.LoadWithOverlayStub(s.ConfigPath, toItems(e.items), real, toItems(cur))
		if lerr == nil {
			lerr = cfg.Validate()
		}
		if lerr != nil {
			return 0, nil, nil, errInvalid{lerr.Error()}
		}
		e.cfg = cfg
	}
	old := map[itemKey]store.ConfigItem{}
	for _, c := range cur {
		old[itemKey{Kind: c.Kind, Name: c.Name}] = c
	}
	snap, _ := json.Marshal(e.items)
	now := s.Now()
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return 0, nil, nil, err
	}
	defer tx.Rollback()
	seen := map[itemKey]bool{}
	for _, c := range e.items {
		k := itemKey{Kind: c.Kind, Name: c.Name}
		seen[k] = true
		if o, ok := old[k]; ok && o.YAML == c.YAML && o.Deleted == c.Deleted {
			continue
		}
		if err = store.PutConfigItem(tx, c.Kind, c.Name, c.YAML, c.Deleted, now); err != nil {
			return 0, nil, nil, err
		}
	}
	for k := range old {
		if !seen[k] {
			if err = store.DeleteConfigItem(tx, k.Kind, k.Name); err != nil {
				return 0, nil, nil, err
			}
		}
	}
	if id, err = store.AddRevision(tx, now, actor, summary, string(snap), e.diff); err != nil {
		return 0, nil, nil, err
	}
	if err = store.Audit(tx, now, actor, "config_changed", 0, summary); err != nil {
		return 0, nil, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, nil, err
	}
	published = true
	for _, f := range stagedFiles {
		if err = publishSecret(f.tmp, f.final); err != nil {
			return 0, nil, nil, err
		}
	}
	for k := range old { // a notify channel that is gone leaves no url/token file behind
		if k.Kind == "notify" && e.cfg.Notify[k.Name] == nil {
			for _, f := range []string{"url", "token"} {
				os.Remove(secretsDir(s.Config().Server.DB) + "/" + config.SecretFileName("notify", k.Name, f))
			}
		}
	}
	if credsChanged || len(pending) > 0 { // a rotated key keeps its ref
		modelCache.clear()
	}
	if s.Apply != nil {
		if applyErr = s.Apply(e.cfg); applyErr != nil {
			slog.Error("apply config", "err", applyErr)
			msg := fmt.Sprintf("Config saved (revision %d) but could not be applied live: %v. Restart siphon to apply it.", id, applyErr)
			s.notice.Store(&msg)
		} else {
			s.notice.Store(nil)
		}
	}
	return id, e, applyErr, nil
}

// banner is the startup notice plus any failed live apply.
func (s *server) banner() string {
	if n := s.notice.Load(); n != nil {
		if s.Banner != "" {
			return s.Banner + " " + *n
		}
		return *n
	}
	return s.Banner
}

// fileKeys are the items present in the config file itself.
func (s *server) fileKeys() (map[itemKey]bool, error) {
	file, err := s.readFile()
	if err != nil {
		return nil, err
	}
	_, prov, err := config.Effective(file, nil)
	out := map[itemKey]bool{}
	for k := range prov {
		out[k] = true
	}
	return out, err
}

// putItem, tombstone and reset are the three mutations of one item.
func putItem(kind, name, y string) func(map[itemKey]store.ConfigItem) {
	return func(m map[itemKey]store.ConfigItem) {
		m[itemKey{Kind: kind, Name: name}] = store.ConfigItem{Kind: kind, Name: name, YAML: y}
	}
}

func resetItem(kind, name string) func(map[itemKey]store.ConfigItem) {
	return func(m map[itemKey]store.ConfigItem) { delete(m, itemKey{Kind: kind, Name: name}) }
}

// deleteItem removes a portal-only item, or tombstones one that is in the file.
func (s *server) deleteItem(kind, name string) (func(map[itemKey]store.ConfigItem), error) {
	fk, err := s.fileKeys()
	if err != nil {
		return nil, err
	}
	if fk[itemKey{Kind: kind, Name: name}] {
		return func(m map[itemKey]store.ConfigItem) {
			m[itemKey{Kind: kind, Name: name}] = store.ConfigItem{Kind: kind, Name: name, Deleted: true}
		}, nil
	}
	return resetItem(kind, name), nil
}
