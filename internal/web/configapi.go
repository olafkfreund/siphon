package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// apiItem is one config item as the API shows it.
type apiItem struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	YAML       string `json:"yaml,omitempty"`
	Provenance string `json:"provenance"`
	Deleted    bool   `json:"deleted,omitempty"` // a portal tombstone over a file item
}

// apiError maps a config error to a status and a client-safe message.
func apiError(err error) (int, error) {
	var inv errInvalid
	switch {
	case errors.As(err, &inv):
		return 422, inv
	case errors.Is(err, errStale):
		return 409, errMsg(err.Error())
	case errors.Is(err, errNoEditing):
		return 503, errMsg(err.Error())
	}
	return 500, err
}

// configAPI mounts /api/config/...: the same validation and save path as the
// portal, behind the bearer token. Kinds outside config.Kinds give 400.
func (s *server) configAPI(mux *http.ServeMux) {
	route := func(pattern string, f func(r *http.Request) (any, int, error)) {
		mux.HandleFunc(pattern, s.api(func(w http.ResponseWriter, r *http.Request) {
			s.reply(w, r, func(r *http.Request) (any, int, error) {
				if s.ConfigPath == "" {
					return nil, 503, errMsg(errNoEditing.Error())
				}
				if k := r.PathValue("kind"); k != "" && !config.Kinds[k] {
					return nil, 400, errMsg("kind is not editable")
				}
				if n := r.PathValue("name"); n != "" && !itemName.MatchString(n) {
					return nil, 400, errMsg("bad name")
				}
				v, code, err := f(r)
				if err != nil && code == 0 {
					code, err = apiError(err)
				}
				return v, code, err
			})
		}))
	}
	route("GET /api/config/history", func(*http.Request) (any, int, error) {
		revs, err := store.Revisions(s.Store.DB, 100)
		if revs == nil {
			revs = []store.ConfigRevision{}
		}
		for i := range revs {
			revs[i].ItemsJSON, revs[i].Diff = "", ""
		}
		return revs, 200, err
	})
	route("GET /api/config/history/{id}", func(r *http.Request) (any, int, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return nil, 400, errMsg("bad id")
		}
		rev, err := store.Revision(s.Store.DB, id)
		if err != nil {
			return nil, 404, errMsg("no such revision")
		}
		rev.ItemsJSON = ""
		return rev, 200, nil
	})
	route("POST /api/config/history/{id}/restore", func(r *http.Request) (any, int, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return nil, 400, errMsg("bad id")
		}
		rev, err := s.bodyRev(r)
		if err != nil {
			return nil, 400, err
		}
		actor, err := apiActor(r)
		if err != nil {
			return nil, 400, err
		}
		if err := s.restore(actor, id, rev); err != nil {
			return nil, 0, err
		}
		return map[string]bool{"ok": true}, 200, nil
	})
	route("GET /api/config/export", func(*http.Request) (any, int, error) {
		eff, _, _, _, err := s.state()
		return map[string]string{"yaml": string(eff)}, 200, err
	})
	route("GET /api/config/{kind}", func(r *http.Request) (any, int, error) {
		kind := r.PathValue("kind")
		_, prov, items, _, err := s.state()
		out := []apiItem{}
		for k, p := range prov {
			if k.Kind == kind {
				out = append(out, apiItem{Kind: k.Kind, Name: k.Name, Provenance: string(p)})
			}
		}
		for _, c := range items {
			if c.Kind == kind && c.Deleted {
				out = append(out, apiItem{Kind: kind, Name: c.Name, Provenance: "deleted", Deleted: true})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, 200, err
	})
	route("GET /api/config/{kind}/{name}", func(r *http.Request) (any, int, error) {
		kind, name := r.PathValue("kind"), r.PathValue("name")
		eff, prov, _, rev, err := s.state()
		if err != nil {
			return nil, 0, err
		}
		y, ok := itemYAML(eff, kind, name)
		if !ok {
			return nil, 404, errMsg("no such item")
		}
		return map[string]any{"kind": kind, "name": name, "yaml": y, "provenance": prov[itemKey{Kind: kind, Name: name}], "rev": rev}, 200, nil
	})
	route("PUT /api/config/{kind}/{name}", func(r *http.Request) (any, int, error) {
		kind, name := r.PathValue("kind"), r.PathValue("name")
		var body struct {
			YAML    string            `json:"yaml"`
			Rev     *int64            `json:"rev"`
			Secrets map[string]string `json:"secrets"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(&body); err != nil {
			return nil, 400, errMsg("bad JSON body")
		}
		actor, err := apiActor(r)
		if err != nil {
			return nil, 400, err
		}
		_, prov, _, _, err := s.state()
		if err != nil {
			return nil, 0, err
		}
		verb := "created"
		if _, ok := prov[itemKey{Kind: kind, Name: name}]; ok {
			verb = "updated"
		}
		y, pending, err := withSecrets(kind, name, body.YAML, body.Secrets, s.Config().Server.DB)
		if err != nil {
			return nil, 0, err
		}
		return s.write(r, actor, kind+"/"+name+" "+verb, body.Rev, putItem(kind, name, y), pending)
	})
	route("POST /api/config/apply", s.applyBatch)
	route("DELETE /api/config/{kind}/{name}", func(r *http.Request) (any, int, error) {
		kind, name := r.PathValue("kind"), r.PathValue("name")
		rev, err := s.queryRev(r)
		if err != nil {
			return nil, 400, err
		}
		actor, err := apiActor(r)
		if err != nil {
			return nil, 400, err
		}
		mutate, err := s.deleteItem(kind, name)
		if err != nil {
			return nil, 0, err
		}
		return s.write(r, actor, kind+"/"+name+" deleted", rev, mutate, nil)
	})
	route("POST /api/config/{kind}/{name}/reset", func(r *http.Request) (any, int, error) {
		kind, name := r.PathValue("kind"), r.PathValue("name")
		rev, err := s.queryRev(r)
		if err != nil {
			return nil, 400, err
		}
		actor, err := apiActor(r)
		if err != nil {
			return nil, 400, err
		}
		return s.write(r, actor, kind+"/"+name+" reset to file", rev, resetItem(kind, name), nil)
	})
}

// write is one API change: with ?dry_run=1 only the check and the diff, else
// the commit. apply_error says the revision is saved but not live.
func (s *server) write(r *http.Request, actor, summary string, rev *int64, mutate func(map[itemKey]store.ConfigItem), pending []pendingSecret) (any, int, error) {
	if r.URL.Query().Get("dry_run") != "" {
		e, err := s.dryRun(rev, mutate, pending)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"diff": e.diff, "errors": []string{}, "warnings": newWarnings(s.Config(), e.cfg)}, 200, nil
	}
	id, _, applyErr, err := s.commit(actor, summary, rev, mutate, pending)
	if err != nil {
		return nil, 0, err
	}
	out := map[string]any{"rev": id, "applied": applyErr == nil}
	if applyErr != nil {
		out["apply_error"] = applyErr.Error()
	}
	return out, 200, nil
}

// secretPath are the fields a PUT may fill with a write-only value.
var secretPath = regexp.MustCompile(`^(api_key|secret|auth\.bearer|access_key_id|secret_access_key|(env|headers)\.[A-Za-z0-9_-]+)$`)

// withSecrets points each secret field of the item at its stored file and
// returns the values to write (commit does, once the config checks pass).
// Values never appear in an error.
func withSecrets(kind, name, y string, secrets map[string]string, db string) (string, []pendingSecret, error) {
	if len(secrets) == 0 {
		return y, nil, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		return "", nil, errInvalid{err.Error()}
	}
	if m == nil {
		m = map[string]any{}
	}
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var pending []pendingSecret
	var bad []error
	for _, k := range keys {
		switch {
		case !secretPath.MatchString(k):
			bad = append(bad, fmt.Errorf("secrets: %q is not a secret field", k))
		case secrets[k] == "":
			bad = append(bad, fmt.Errorf("secrets: %s is empty", k))
		default:
			ps := pendingSecret{Kind: kind, Name: name, Key: k, Value: secrets[k]}
			pending = append(pending, ps)
			setPath(m, k, "file:"+ps.path(secretsDir(db)))
		}
	}
	if len(bad) > 0 {
		return "", nil, errInvalid{errors.Join(bad...).Error()}
	}
	b, err := yaml.Marshal(m)
	return string(b), pending, err
}

// queryRev reads the optional ?rev= stale-check (absent: no check).
func (s *server) queryRev(r *http.Request) (*int64, error) {
	v := r.URL.Query().Get("rev")
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, errMsg("bad rev")
	}
	return &n, nil
}

func (s *server) bodyRev(r *http.Request) (*int64, error) {
	var body struct {
		Rev *int64 `json:"rev"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&body); err != nil {
			return nil, errMsg("bad JSON body")
		}
	}
	return body.Rev, nil
}

type applyItem struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	YAML string `json:"yaml"`
	Rev  *int64 `json:"rev"`
}

// applyBatch is POST /api/config/apply: every item and delete in one prepare
// and one commit (one revision), or none of them.
func (s *server) applyBatch(r *http.Request) (any, int, error) {
	var body struct {
		Items   []applyItem       `json:"items"`
		Delete  []applyItem       `json:"delete"`
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&body); err != nil {
		return nil, 400, errMsg("bad JSON body")
	}
	actor, err := apiActor(r)
	if err != nil {
		return nil, 400, err
	}
	if len(body.Items)+len(body.Delete) == 0 {
		return nil, 400, errMsg("nothing to apply")
	}
	var bad []error
	seen := map[itemKey]bool{}
	for _, it := range append(slices.Clone(body.Items), body.Delete...) {
		k := itemKey{Kind: it.Kind, Name: it.Name}
		switch {
		case !config.Kinds[it.Kind] || !itemName.MatchString(it.Name):
			bad = append(bad, fmt.Errorf("%s/%s: bad kind or name", it.Kind, it.Name))
		case seen[k]:
			bad = append(bad, fmt.Errorf("%s/%s: listed more than once", it.Kind, it.Name))
		}
		seen[k] = true
	}
	// A secret key is <kind>/<name>.<field>; names may hold dots, so match the items.
	bySecret := map[int]map[string]string{}
	for k, v := range body.Secrets {
		best, field := -1, ""
		for i, it := range body.Items {
			if f, ok := strings.CutPrefix(k, it.Kind+"/"+it.Name+"."); ok && (best < 0 || len(it.Name) > len(body.Items[best].Name)) {
				best, field = i, f // the longest item name wins: a-b.c is a-b's c, not a's b.c
			}
		}
		if best < 0 {
			bad = append(bad, fmt.Errorf("secrets: %q is not for an item in this request", k))
			continue
		}
		if bySecret[best] == nil {
			bySecret[best] = map[string]string{}
		}
		bySecret[best][field] = v
	}
	var pending []pendingSecret
	var muts []func(map[itemKey]store.ConfigItem)
	var rev *int64
	for i, it := range body.Items {
		y, ps, err := withSecrets(it.Kind, it.Name, it.YAML, bySecret[i], s.Config().Server.DB)
		var inv errInvalid
		if errors.As(err, &inv) {
			for _, l := range inv.list() {
				bad = append(bad, errors.New(it.Kind+"/"+it.Name+": "+l))
			}
			continue
		} else if err != nil {
			return nil, 0, err
		}
		pending = append(pending, ps...)
		muts = append(muts, putItem(it.Kind, it.Name, y))
		if it.Rev != nil {
			if rev != nil && *rev != *it.Rev {
				return nil, 0, errStale // two different revs: one of them is stale
			}
			rev = it.Rev
		}
	}
	for _, d := range body.Delete {
		if !config.Kinds[d.Kind] || !itemName.MatchString(d.Name) {
			continue // reported above
		}
		m, err := s.deleteItem(d.Kind, d.Name)
		if err != nil {
			return nil, 0, err
		}
		muts = append(muts, m)
		if d.Rev != nil {
			if rev != nil && *rev != *d.Rev {
				return nil, 0, errStale
			}
			rev = d.Rev
		}
	}
	if len(bad) > 0 {
		return nil, 0, errInvalid{errors.Join(bad...).Error()}
	}
	mutate := func(m map[itemKey]store.ConfigItem) {
		for _, f := range muts {
			f(m)
		}
	}
	v, code, err := s.write(r, actor, fmt.Sprintf("apply: %d items, %d deleted", len(body.Items), len(body.Delete)), rev, mutate, pending)
	var inv errInvalid
	if errors.As(err, &inv) {
		lines := inv.list()
		for i, l := range lines {
			lines[i] = tagItem(l, body.Items)
		}
		return nil, 0, errInvalid{strings.Join(lines, "\n")}
	}
	return v, code, err
}

// tagItem prefixes an error line with the batch item it names, if any: the
// config checks word them as sources.NAME or rules[N] "NAME".
func tagItem(line string, items []applyItem) string {
	for _, it := range items {
		ref := it.Kind + "." + it.Name
		if strings.Contains(line, ref+":") || strings.Contains(line, ref+".") ||
			(it.Kind == "rules" && strings.Contains(line, "rules[") && strings.Contains(line, `"`+it.Name+`":`)) {
			return it.Kind + "/" + it.Name + ": " + line
		}
	}
	return line
}

// newWarnings are the warnings the change adds: those of next that the live
// config does not already have.
func newWarnings(live, next *config.Config) []string {
	have := map[string]bool{}
	for _, w := range live.Warnings() {
		have[w] = true
	}
	out := []string{}
	for _, w := range next.Warnings() {
		if !have[w] {
			out = append(out, w)
		}
	}
	return out
}
