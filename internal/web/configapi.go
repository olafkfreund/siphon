package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// apiItem is one config item as the API shows it.
type apiItem struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	YAML       string `json:"yaml,omitempty"`
	Provenance string `json:"provenance"`
}

// apiError maps a config error to a status and a client-safe message.
func apiError(err error) (int, error) {
	var inv errInvalid
	switch {
	case errors.As(err, &inv):
		return 400, errMsg(inv.msg)
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
		if err := s.restore(id, rev); err != nil {
			return nil, 0, err
		}
		return map[string]bool{"ok": true}, 200, nil
	})
	route("GET /api/config/export", func(*http.Request) (any, int, error) {
		eff, _, _, _, err := s.state()
		return map[string]string{"yaml": string(eff)}, 200, err
	})
	route("GET /api/config/{kind}", func(r *http.Request) (any, int, error) {
		_, prov, _, _, err := s.state()
		out := []apiItem{}
		for k, p := range prov {
			if k.Kind == r.PathValue("kind") {
				out = append(out, apiItem{Kind: k.Kind, Name: k.Name, Provenance: string(p)})
			}
		}
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
			YAML string `json:"yaml"`
			Rev  *int64 `json:"rev"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(&body); err != nil {
			return nil, 400, errMsg("bad JSON body")
		}
		_, prov, _, _, err := s.state()
		if err != nil {
			return nil, 0, err
		}
		verb := "created"
		if _, ok := prov[itemKey{Kind: kind, Name: name}]; ok {
			verb = "updated"
		}
		id, _, applyErr, err := s.commit("api", kind+"/"+name+" "+verb, body.Rev, putItem(kind, name, body.YAML), nil)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"rev": id, "applied": applyErr == nil}, 200, nil
	})
	route("DELETE /api/config/{kind}/{name}", func(r *http.Request) (any, int, error) {
		kind, name := r.PathValue("kind"), r.PathValue("name")
		rev, err := s.queryRev(r)
		if err != nil {
			return nil, 400, err
		}
		mutate, err := s.deleteItem(kind, name)
		if err != nil {
			return nil, 0, err
		}
		id, _, applyErr, err := s.commit("api", kind+"/"+name+" deleted", rev, mutate, nil)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"rev": id, "applied": applyErr == nil}, 200, nil
	})
	route("POST /api/config/{kind}/{name}/reset", func(r *http.Request) (any, int, error) {
		kind, name := r.PathValue("kind"), r.PathValue("name")
		rev, err := s.queryRev(r)
		if err != nil {
			return nil, 400, err
		}
		id, _, applyErr, err := s.commit("api", kind+"/"+name+" reset to file", rev, resetItem(kind, name), nil)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"rev": id, "applied": applyErr == nil}, 200, nil
	})
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
