package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/cred"
	"github.com/olafkfreund/siphon/internal/store"
)

// cfgView is the config list and editor page.
type cfgView struct {
	Kinds                      []string
	Kind, Name, Prov           string
	New, Exists, HasForm, Done bool // Done: the change was reviewed and is valid
	Rows                       []cfgRow
	Fields                     []fieldView
	YAML, Errors, Diff, Notice string
	Rev                        int64
	// Editor extras (editors.go).
	Base       string // /config/<kind>/<name|new>
	Side       string // which side panel: rules, sources, agents, routines
	F          map[string]fieldView
	ActionKind string
	SourceOpts []optPair
	AgentOpts  []string
	UnitOpts   []string
	RoutineOpt []string
	Health     *sourceView
	LastEvent  string
	Steps      []stepRow
}

type cfgRow struct{ Name, Prov string }

// histView is the revision list or one revision.
type histView struct {
	Revs   []store.ConfigRevision
	Rev    *store.ConfigRevision
	Latest int64
}

var doneText = map[string]string{"save": "Saved and applied.", "delete": "Deleted.", "reset": "Reset to the file.", "restore": "Restored as a new revision."}

func (s *server) configRoutes(mux *http.ServeMux) {
	h := func(pattern string, f func(w http.ResponseWriter, r *http.Request, csrf string)) {
		mux.HandleFunc(pattern, s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
			if s.ConfigPath == "" {
				http.Error(w, errNoEditing.Error(), http.StatusServiceUnavailable)
				return
			}
			if k := r.PathValue("kind"); k != "" && !config.Kinds[k] {
				http.NotFound(w, r)
				return
			}
			f(w, r, csrf)
		}))
	}
	h("GET /config/{kind}", s.cfgList)
	h("GET /config/{kind}/new", s.cfgEdit)
	h("GET /config/{kind}/{name}", s.cfgEdit)
	for _, verb := range []string{"check", "save", "delete", "reset"} {
		h("POST /config/{kind}/{name}/"+verb, func(w http.ResponseWriter, r *http.Request, csrf string) { s.cfgPost(w, r, csrf, verb) })
	}
	h("GET /history", s.histList)
	h("GET /history/export", func(w http.ResponseWriter, _ *http.Request, _ string) {
		eff, _, _, _, err := s.state()
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="siphon.yaml"`)
		w.Write(eff)
	})
	h("GET /history/{id}", s.histItem)
	h("POST /history/{id}/restore", s.histRestore)
	mux.HandleFunc("POST /rules/test", s.portal(s.ruleTest))
	mux.HandleFunc("POST /agents/egress-preview", s.portal(s.egressPreview))
	mux.HandleFunc("POST /logins", s.portal(s.loginAdd))
	s.modelRoutes(mux)
	s.serviceRoutes(mux)
	mux.HandleFunc("POST /logins/{name}/delete", s.portal(s.loginDelete))
	s.configAPI(mux)
}

// pageStatus renders a portal page with a non-200 status.
func (s *server) pageStatus(w http.ResponseWriter, r *http.Request, name string, v view, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	s.page(w, r, name, v)
}

// fail answers a handler error with the status its kind deserves.
func (s *server) fail(w http.ResponseWriter, err error) {
	var inv errInvalid
	switch {
	case errors.As(err, &inv):
		http.Error(w, inv.msg, http.StatusUnprocessableEntity)
	case errors.Is(err, errStale):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errNoEditing):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		slog.Error("config", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *server) cfgList(w http.ResponseWriter, r *http.Request, csrf string) {
	kind := r.PathValue("kind")
	_, prov, items, rev, err := s.state()
	if err != nil {
		s.fail(w, err)
		return
	}
	v := &cfgView{Kinds: sortedKinds(), Kind: kind, Rev: rev, Notice: doneText[r.URL.Query().Get("done")]}
	for k, p := range prov {
		if k.Kind == kind {
			v.Rows = append(v.Rows, cfgRow{k.Name, string(p)})
		}
	}
	for _, it := range items {
		if it.Kind == kind && it.Deleted {
			v.Rows = append(v.Rows, cfgRow{it.Name, "deleted"})
		}
	}
	sort.Slice(v.Rows, func(i, j int) bool { return v.Rows[i].Name < v.Rows[j].Name })
	s.page(w, r, "cfglist", view{CSRF: csrf, Cfg: v})
}

func sortedKinds() []string {
	k := make([]string, 0, len(config.Kinds))
	for n := range config.Kinds {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// editView builds the editor for an item from its YAML.
func (s *server) editView(kind, name string, isNew bool, y string) (*cfgView, error) {
	_, prov, _, rev, err := s.state()
	if err != nil {
		return nil, err
	}
	p, exists := prov[itemKey{Kind: kind, Name: name}]
	v := &cfgView{Kinds: sortedKinds(), Kind: kind, Name: name, New: isNew, Exists: exists, Prov: string(p), YAML: y,
		Rev: rev, Fields: formFields(kind, y)}
	v.HasForm = len(v.Fields) > 0
	if isNew {
		v.Prov = "new"
	}
	if err := s.editExtras(v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *server) cfgEdit(w http.ResponseWriter, r *http.Request, csrf string) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	isNew := name == ""
	y := ""
	if !isNew {
		eff, _, _, _, err := s.state()
		if err != nil {
			s.fail(w, err)
			return
		}
		var ok bool
		if y, ok = itemYAML(eff, kind, name); !ok {
			http.NotFound(w, r)
			return
		}
	}
	v, err := s.editView(kind, name, isNew, y)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.page(w, r, "cfgedit", view{CSRF: csrf, Cfg: v})
}

// itemYAMLFromRequest is the submitted item: the YAML tab's text, or the form
// layered onto the current item.
func (s *server) itemYAMLFromRequest(r *http.Request, kind, name string) (string, []pendingSecret, error) {
	if r.PostFormValue("mode") == "yaml" {
		return r.PostFormValue("yaml"), nil, nil
	}
	eff, _, _, _, err := s.state()
	if err != nil {
		return "", nil, err
	}
	existing, _ := itemYAML(eff, kind, name)
	return applyForm(kind, name, existing, r.PostForm, s.Config().Server.DB)
}

func (s *server) cfgPost(w http.ResponseWriter, r *http.Request, csrf, verb string) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	isNew := name == "new"
	if isNew {
		name = r.PostFormValue("name")
	}
	rev, err := strconv.ParseInt(r.PostFormValue("rev"), 10, 64)
	if err != nil {
		http.Error(w, "missing rev", http.StatusBadRequest)
		return
	}
	bad := func(y, msg string, code int) {
		v, err := s.editView(kind, name, isNew, y)
		if err != nil {
			s.fail(w, err)
			return
		}
		v.Errors = msg
		s.pageStatus(w, r, "cfgedit", view{CSRF: csrf, Cfg: v}, code)
	}
	if !itemName.MatchString(name) || name == "new" {
		bad("", "name: use letters, digits, - _ . (at most 64)", http.StatusUnprocessableEntity)
		return
	}
	var (
		mutate  func(map[itemKey]store.ConfigItem)
		summary string
		y       string
		pending []pendingSecret
	)
	_, prov, _, _, err := s.state()
	if err != nil {
		s.fail(w, err)
		return
	}
	_, exists := prov[itemKey{Kind: kind, Name: name}]
	switch verb {
	case "check", "save":
		if y, pending, err = s.itemYAMLFromRequest(r, kind, name); err != nil {
			bad(r.PostFormValue("yaml"), err.Error(), http.StatusUnprocessableEntity)
			return
		}
		mutate = putItem(kind, name, y)
		summary = fmt.Sprintf("%s/%s %s", kind, name, map[bool]string{true: "updated", false: "created"}[exists])
	case "delete":
		if mutate, err = s.deleteItem(kind, name); err != nil {
			s.fail(w, err)
			return
		}
		summary = fmt.Sprintf("%s/%s deleted", kind, name)
	case "reset":
		mutate, summary = resetItem(kind, name), fmt.Sprintf("%s/%s reset to file", kind, name)
	}
	if verb == "check" {
		cur, latest, err := s.overlay()
		if err == nil && latest != rev {
			err = errStale
		}
		var e *edit
		if err == nil {
			e, err = s.prepare(cur, mutate, pending)
		}
		var inv errInvalid
		switch {
		case errors.As(err, &inv):
			bad(y, inv.msg, http.StatusUnprocessableEntity)
		case errors.Is(err, errStale):
			bad(y, err.Error(), http.StatusConflict)
		case err != nil:
			s.fail(w, err)
		default:
			v, verr := s.editView(kind, name, isNew, y)
			if verr != nil {
				s.fail(w, verr)
				return
			}
			v.Diff, v.Done = e.diff, true
			if e.diff == "" {
				v.Diff = "(no change)"
			}
			s.page(w, r, "cfgedit", view{CSRF: csrf, Cfg: v})
		}
		return
	}
	_, _, applyErr, err := s.commit("portal", summary, &rev, mutate, pending)
	var inv errInvalid
	switch {
	case errors.As(err, &inv):
		if verb == "save" {
			bad(y, inv.msg, http.StatusUnprocessableEntity)
			return
		}
		s.fail(w, err)
	case errors.Is(err, errStale) && verb == "save":
		bad(y, err.Error(), http.StatusConflict)
	case err != nil:
		s.fail(w, err)
	default:
		_ = applyErr // shown through the banner
		http.Redirect(w, r, "/config/"+kind+"?done="+verb, http.StatusSeeOther)
	}
}

func (s *server) histList(w http.ResponseWriter, r *http.Request, csrf string) {
	revs, err := store.Revisions(s.Store.DB, 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	h := &histView{Revs: revs}
	if len(revs) > 0 {
		h.Latest = revs[0].ID
	}
	s.page(w, r, "history", view{CSRF: csrf, Hist: h})
}

func (s *server) histItem(w http.ResponseWriter, r *http.Request, csrf string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rev, err := store.Revision(s.Store.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	_, latest, err := s.overlay()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.page(w, r, "historyitem", view{CSRF: csrf, Hist: &histView{Rev: &rev, Latest: latest}})
}

func (s *server) histRestore(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	cur, perr := strconv.ParseInt(r.PostFormValue("rev"), 10, 64)
	if err != nil || perr != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.restore("portal", id, &cur); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/history", http.StatusSeeOther)
}

// restore replaces the overlay with revision id's snapshot, as a new revision.
func (s *server) restore(actor string, id int64, rev *int64) error {
	old, err := store.Revision(s.Store.DB, id)
	if err != nil {
		return err
	}
	var snap []store.ConfigItem
	if err := json.Unmarshal([]byte(old.ItemsJSON), &snap); err != nil {
		return errInvalid{"revision snapshot is unreadable"}
	}
	_, _, _, err = s.commit(actor, fmt.Sprintf("restored revision %d", id), rev, func(m map[itemKey]store.ConfigItem) {
		clear(m)
		for _, c := range snap {
			m[itemKey{Kind: c.Kind, Name: c.Name}] = c
		}
	}, nil)
	return err
}

var (
	providers  = []string{"claude", "codex", "agy"}
	loginNames = itemName
)

// audit writes one audit row for an action that has no config revision.
func (s *server) audit(actor, event, detail string) {
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return
	}
	if store.Audit(tx, s.Now(), actor, event, 0, detail) == nil {
		tx.Commit()
	} else {
		tx.Rollback()
	}
}

// addLogin stores a login: a pasted subscription login or token goes through
// the same check as `siphon credentials import`; an API key goes to a secret
// file. The credentials/<name> overlay item is created if the config has none.
func (s *server) addLogin(actor, name, provider, kind, value string) error {
	if !loginNames.MatchString(name) || !slices.Contains(providers, provider) || strings.TrimSpace(value) == "" {
		return errInvalid{"name, provider and value are required"}
	}
	cfg := s.Config()
	existing := cfg.Credentials[name]
	switch kind {
	case "apikey":
		if existing != nil && existing.APIKey.Ref == "" {
			return errInvalid{"credential " + name + " is a subscription login, not an API key"}
		}
		ps := pendingSecret{Kind: "credentials", Name: name, Key: "api_key", Value: value}
		y := fmt.Sprintf("provider: %s\napi_key: file:%s\n", provider, ps.path(secretsDir(cfg.Server.DB)))
		_, _, _, err := s.commit(actor, "credentials/"+name+" api key set", nil, putItem("credentials", name, y), []pendingSecret{ps})
		return err
	case "login", "token":
		if existing != nil && existing.APIKey.Ref != "" {
			return errInvalid{"credential " + name + " is an API key, not a subscription login"}
		}
		if existing != nil {
			provider = existing.Provider
		}
		file, b, err := cred.ValidateImport(provider, kind == "token", []byte(value))
		if err != nil {
			return errInvalid{err.Error()}
		}
		if existing == nil {
			if _, _, _, err := s.commit(actor, "credentials/"+name+" created", nil, putItem("credentials", name, "provider: "+provider+"\n"), nil); err != nil {
				return err
			}
		}
		if err := cred.StoreFor(s.Config()).Put(name, file, b); err != nil {
			return err
		}
		s.audit(actor, "login_imported", name)
		return nil
	}
	return errInvalid{"kind must be login, token or apikey"}
}

func (s *server) loginAdd(w http.ResponseWriter, r *http.Request, csrf string) {
	name, provider, kind := r.PostFormValue("name"), r.PostFormValue("provider"), r.PostFormValue("kind")
	err := s.addLogin("portal", name, provider, kind, r.PostFormValue("value"))
	var inv errInvalid
	if errors.As(err, &inv) { // show the problem on the page; the value is never echoed
		v := s.connectionsView(r, csrf)
		v.LoginForm = &loginForm{Name: name, Provider: provider, Kind: kind, Err: inv.msg}
		s.pageStatus(w, r, "logins", v, http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/connections?added="+url.QueryEscape(name), http.StatusSeeOther)
}

func (s *server) loginDelete(w http.ResponseWriter, r *http.Request, _ string) {
	if err := s.removeLogin("portal", r.PathValue("name")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}

func (s *server) removeLogin(actor, name string) error {
	cfg := s.Config()
	if cfg.Credentials[name] == nil || !loginNames.MatchString(name) {
		return os.ErrNotExist
	}
	_, prov, _, _, err := s.state()
	if err != nil {
		return err
	}
	if prov[itemKey{Kind: "credentials", Name: name}] == config.FromPortal {
		if _, _, _, err := s.commit(actor, "credentials/"+name+" deleted", nil, resetItem("credentials", name), nil); err != nil {
			return err
		}
	}
	if err := cred.StoreFor(cfg).Delete(name); err != nil {
		return err
	}
	_ = os.Remove(secretsDir(cfg.Server.DB) + "/credentials-" + name + "-api_key")
	s.audit(actor, "login_deleted", name)
	return nil
}
