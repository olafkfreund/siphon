package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"

	"github.com/olafkfreund/siphon/internal/config"
)

// connectionAPI mounts JSON wrappers over the portal's service and
// connection setup. Same functions, same checks; secrets arrive in the body,
// are stored write-only, and never come back (except the one-time webhook
// secret in a setup reply, which is never cached).
func (s *server) connectionAPI(mux *http.ServeMux) {
	route := func(pattern string, f func(r *http.Request, actor string) (any, int, error)) {
		mux.HandleFunc(pattern, s.api(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			s.reply(w, r, func(r *http.Request) (any, int, error) {
				actor, err := apiActor(r)
				if err != nil {
					return nil, 400, err
				}
				v, code, err := f(r, actor)
				if err != nil && code == 0 {
					code, err = apiError(err)
				}
				return v, code, err
			})
		}))
	}
	decode := func(r *http.Request, v any) error {
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(v); err != nil {
			return errMsg("bad JSON body")
		}
		return nil
	}
	// done is a setup reply: the result, or the refusal.
	done := func(d *serviceDone, err error) (any, int, error) {
		if err != nil {
			return nil, 0, err
		}
		return d, 200, nil
	}
	route("POST /api/services/github", func(r *http.Request, actor string) (any, int, error) {
		var b struct {
			Name, Token, Mode string
			Webhook           bool
		}
		if err := decode(r, &b); err != nil {
			return nil, 400, err
		}
		return done(s.addGitHub(actor, b.Name, b.Token, b.Mode, b.Webhook))
	})
	route("POST /api/services/gitlab", func(r *http.Request, actor string) (any, int, error) {
		var b struct {
			Name, Base, Project, Token string
			Webhook                    bool
		}
		if err := decode(r, &b); err != nil {
			return nil, 400, err
		}
		return done(s.addGitLab(actor, b.Name, b.Base, b.Project, b.Token, b.Webhook))
	})
	route("POST /api/services/aws", func(r *http.Request, actor string) (any, int, error) {
		var b struct {
			Name, Region, Mode, Profile string
			RoleARN                     string `json:"role_arn"`
			ExternalID                  string `json:"external_id"`
			AccessKeyID                 string `json:"access_key_id"`
			SecretAccessKey             string `json:"secret_access_key"`
			Servers                     []string
			Webhook                     bool
		}
		if err := decode(r, &b); err != nil {
			return nil, 400, err
		}
		return done(s.addAWS(actor, b.Name, b.Region, b.Mode, b.Profile, b.RoleARN, b.ExternalID, b.AccessKeyID, b.SecretAccessKey, b.Servers, b.Webhook))
	})
	route("POST /api/services/{name}/test", func(r *http.Request, _ string) (any, int, error) {
		if s.Config().Sources[r.PathValue("name")] == nil {
			return nil, 404, errMsg("no such source")
		}
		return s.testService(r.Context(), r.PathValue("name")), 200, nil
	})
	route("POST /api/connections/models", func(r *http.Request, actor string) (any, int, error) {
		var b struct {
			Name, Preset, URL string
			APIKey            string `json:"api_key"`
		}
		if err := decode(r, &b); err != nil {
			return nil, 400, err
		}
		if err := s.addModelConn(actor, b.Name, b.Preset, b.URL, b.APIKey); err != nil {
			return nil, 0, err
		}
		return map[string]string{"name": b.Name}, 200, nil
	})
	route("POST /api/connections/models/{name}/test", func(r *http.Request, _ string) (any, int, error) {
		name := r.PathValue("name")
		if c := s.Config().Credentials[name]; c == nil || (c.Provider != "ollama" && c.Provider != "openai") {
			return nil, 404, errMsg("no such model connection")
		}
		return s.listModels(r.Context(), name, true), 200, nil
	})
	route("GET /api/connections", func(*http.Request, string) (any, int, error) {
		type login struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			Type     string `json:"type"`
			Status   string `json:"status"`
			Expiry   string `json:"expiry"`
			Written  string `json:"written"`
		}
		type model struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			Preset   string `json:"preset"`
			URL      string `json:"url"`
		}
		out := struct {
			Logins []login `json:"logins"`
			Models []model `json:"models"`
		}{[]login{}, []model{}}
		for _, l := range s.logins() {
			out.Logins = append(out.Logins, login{l.Name, l.Key, l.Type, l.Status, l.Expiry, l.Written})
		}
		for _, m := range s.modelConns() {
			out.Models = append(out.Models, model{m.Name, m.Provider, m.Preset, redactURL(s.Config().Credentials[m.Name])})
		}
		return out, 200, nil
	})
	route("POST /api/connections/logins", func(r *http.Request, actor string) (any, int, error) {
		var b struct{ Name, Provider, Kind, Value string }
		if err := decode(r, &b); err != nil {
			return nil, 400, err
		}
		if err := s.addLogin(actor, b.Name, b.Provider, b.Kind, b.Value); err != nil {
			return nil, 0, err
		}
		return map[string]string{"name": b.Name}, 200, nil
	})
	route("DELETE /api/connections/logins/{name}", func(r *http.Request, actor string) (any, int, error) {
		if err := s.removeLogin(actor, r.PathValue("name")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, 404, errMsg("no such login")
			}
			return nil, 0, err
		}
		return map[string]bool{"ok": true}, 200, nil
	})
}

// redactURL is a model connection's URL without any userinfo.
func redactURL(c *config.Credential) string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}
