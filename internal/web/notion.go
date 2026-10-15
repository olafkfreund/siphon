package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
)

// notionAwaiting reports whether name is a notion webhook source with no secret yet.
func (s *server) notionAwaiting(name string) bool {
	src := s.Config().Sources[name]
	return src != nil && src.Type == "webhook" && src.Format == "notion" && src.Secret.Ref == ""
}

// acceptNotion stores the pending verification token as the source's secret.
// seen is the token the operator was shown: if another capture replaced it
// since, nothing is stored. The token leaves the pending store only once the
// change is committed and applied.
func (s *server) acceptNotion(r *http.Request, actor, name, seen string) (any, int, error) {
	if s.ConfigPath == "" {
		return nil, 503, errMsg(errNoEditing.Error())
	}
	src := s.Config().Sources[name]
	if src == nil || src.Type != "webhook" || src.Format != "notion" {
		return nil, 404, errMsg("no such Notion source")
	}
	if src.Secret.Ref != "" {
		return nil, 409, errMsg("source already has a secret")
	}
	if s.NotionPending == nil {
		return nil, 404, errMsg("no verification token pending")
	}
	tok, _, ok := s.NotionPending(name)
	if !ok {
		return nil, 404, errMsg("no verification token pending")
	}
	a, b := sha256.Sum256([]byte(seen)), sha256.Sum256([]byte(tok))
	if seen == "" || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		return nil, 409, errMsg("the pending token changed; reload and check it again")
	}
	eff, _, _, _, err := s.state()
	if err != nil {
		return nil, 500, err
	}
	y, found := itemYAML(eff, "sources", name)
	if !found {
		return nil, 404, errMsg("no such Notion source")
	}
	y, pending, err := withSecrets("sources", name, y, map[string]string{"secret": tok}, s.Config().Server.DB)
	if err != nil {
		code, err := apiError(err)
		return nil, code, err
	}
	out, code, err := s.write(r, actor, "accept Notion verification token", nil, putItem("sources", name, y), pending)
	if err != nil && code == 0 {
		code, err = apiError(err)
	}
	if m, _ := out.(map[string]any); err == nil && m["applied"] == true {
		s.TakeNotionPending(name)
		s.audit(actor, "notion_token_accepted", name)
	}
	return out, code, err
}

func (s *server) notionRoutes(mux *http.ServeMux) {
	// The pending token, for `siphon connect notion`: admin token only, never cached.
	mux.HandleFunc("GET /api/sources/{name}/notion-verification", s.api(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		s.reply(w, r, func(r *http.Request) (any, int, error) {
			src := s.Config().Sources[r.PathValue("name")]
			if src == nil || src.Type != "webhook" || src.Format != "notion" || s.NotionPending == nil {
				return nil, 404, errMsg("no such Notion source")
			}
			tok, at, ok := s.NotionPending(r.PathValue("name"))
			if !ok {
				return nil, 404, errMsg("no verification token pending")
			}
			return map[string]any{"token": tok, "at": at}, 200, nil
		})
	}))
	mux.HandleFunc("POST /api/sources/{name}/notion-verification", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, func(r *http.Request) (any, int, error) {
			actor, err := apiActor(r)
			if err != nil {
				return nil, 400, err
			}
			var body struct {
				Token string `json:"token"`
			}
			if r.ContentLength != 0 {
				if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&body); err != nil {
					return nil, 400, errMsg("bad JSON body")
				}
			}
			return s.acceptNotion(r, actor, r.PathValue("name"), body.Token)
		})
	}))
	mux.HandleFunc("POST /sources/{name}/notion-verification", s.portalAs(roleOperator, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, code, err := s.acceptNotion(r, s.actor(r), r.PathValue("name"), r.FormValue("token"))
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		http.Redirect(w, r, "/sources", http.StatusSeeOther)
	}))
}

// notionView fills a source's pending-token fields for the portal; never in JSON.
func (s *server) notionView(v *sourceView, name string) {
	if !s.notionAwaiting(name) {
		return
	}
	v.NotionAwait = true
	if s.NotionPending != nil {
		if tok, at, ok := s.NotionPending(name); ok {
			v.NotionToken, v.NotionAt = tok, &at
		}
	}
}
