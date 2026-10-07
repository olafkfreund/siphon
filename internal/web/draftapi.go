package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/applyfile"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/draft"
	"github.com/olafkfreund/siphon/internal/store"
)

// draftAPI mounts POST /api/draft: a model drafts an apply file from a
// request. It never commits; the result goes through the normal apply.
func (s *server) draftAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/draft", s.api(func(w http.ResponseWriter, r *http.Request) {
		// up to three model rounds: more than the server's usual write timeout
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(7 * time.Minute))
		s.reply(w, r, s.draft)
	}))
}

func (s *server) draft(r *http.Request) (any, int, error) {
	var body struct {
		Request    string `json:"request"`
		Connection string `json:"connection"`
		Model      string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<10)).Decode(&body); err != nil {
		return nil, 400, errMsg("bad JSON body")
	}
	if strings.TrimSpace(body.Request) == "" || len(body.Request) > 4000 {
		return nil, 400, errMsg("request is required (at most 4000 characters)")
	}
	actor, err := apiActor(r)
	if err != nil {
		return nil, 400, err
	}
	cfg := s.Config()
	var models []string
	for n, c := range cfg.Credentials {
		if c.IsModel() {
			models = append(models, n)
		}
	}
	sort.Strings(models)
	name := body.Connection
	switch {
	case name == "" && len(models) == 0:
		return nil, 0, errInvalid{"no model connection is configured: run `siphon connect model ollama`"}
	case name == "":
		name = models[0]
	}
	cred := cfg.Credentials[name]
	if cred == nil || !cred.IsModel() {
		return nil, 0, errInvalid{"no model connection named " + name + " (connections: " + strings.Join(models, ", ") + ")"}
	}
	model := body.Model
	if model == "" {
		t := s.listModels(r.Context(), name, false)
		if t.Err != "" || len(t.Models) == 0 {
			return nil, 0, errInvalid{"could not pick a model for " + name + ": pass one with model (" + firstNonEmpty(t.Err, "the endpoint lists none") + ")"}
		}
		model = t.Models[0]
	}
	hc, closeHC, err := draft.GuardedClient(r.Context(), cfg, cred.URL)
	if err != nil {
		return nil, 0, errInvalid{err.Error()}
	}
	defer closeHC()
	res, err := draft.Run(r.Context(), draft.Params{
		Request:   body.Request,
		Conn:      draft.Conn{Name: name, BaseURL: cred.BaseURL(), APIKey: cred.APIKey.Value, Model: model},
		Inventory: s.inventory(),
		Check:     s.draftCheck,
		HTTP:      hc,
	})
	if err != nil {
		return nil, 0, errInvalid{err.Error()} // the model side failed: no secrets in these messages
	}
	req := body.Request
	if len(req) > 500 {
		req = req[:500] + "…"
	}
	s.audit(actor, "draft", name+" "+model+": "+req)
	return res, 200, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// draftCheck is apply's dry-run over a drafted file. Webhook sources get a
// stand-in secret, since the real one is generated when the file is applied.
func (s *server) draftCheck(_ context.Context, y string) (string, []string, []string) {
	items, err := applyfile.Parse([]byte(y))
	if err != nil {
		return "", []string{err.Error()}, nil
	}
	needs := map[string]bool{}
	for _, n := range draft.WebhookNeedsSecret(items) {
		needs[n] = true
	}
	var muts []func(map[itemKey]store.ConfigItem)
	var pending []pendingSecret
	var bad []string
	for _, it := range items {
		if !config.Kinds[it.Kind] || !itemName.MatchString(it.Name) {
			bad = append(bad, it.Kind+"/"+it.Name+": bad kind or name")
			continue
		}
		bad = append(bad, itemShape(it)...)
		text := it.YAML
		if needs[it.Kind+"/"+it.Name] {
			var ps []pendingSecret
			if text, ps, err = withSecrets(it.Kind, it.Name, text, map[string]string{"secret": "placeholder"}, s.Config().Server.DB); err != nil {
				bad = append(bad, it.Kind+"/"+it.Name+": "+err.Error())
				continue
			}
			pending = append(pending, ps...)
		}
		muts = append(muts, putItem(it.Kind, it.Name, text))
	}
	if len(bad) > 0 {
		return "", bad, nil
	}
	e, err := s.dryRun(nil, func(m map[itemKey]store.ConfigItem) {
		for _, f := range muts {
			f(m)
		}
	}, pending)
	if err != nil {
		if inv, ok := err.(errInvalid); ok {
			lines := inv.list()
			for i, l := range lines {
				lines[i] = tagItem(l, toApplyItems(items))
			}
			return "", lines, nil
		}
		return "", []string{"internal error"}, nil
	}
	return e.diff, nil, nil
}

func toApplyItems(items []applyfile.Item) []applyItem {
	out := make([]applyItem, len(items))
	for i, it := range items {
		out[i] = applyItem{Kind: it.Kind, Name: it.Name, YAML: it.YAML}
	}
	return out
}

// itemShape decodes one item strictly, so a wrong type or an unknown field is
// reported against the item (the merged-config errors don't name it).
func itemShape(it applyfile.Item) []string {
	var into any
	switch it.Kind {
	case "sources":
		into = &config.Source{}
	case "agents":
		into = &config.Agent{}
	case "routines":
		into = &config.Routine{}
	case "credentials":
		into = &config.Credential{}
	default:
		into = &config.Rule{}
	}
	dec := yaml.NewDecoder(strings.NewReader(it.YAML))
	dec.KnownFields(true)
	err := dec.Decode(into)
	if err == nil || err == io.EOF {
		return nil
	}
	var out []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "yaml:")); l != "" && l != "unmarshal errors:" {
			out = append(out, it.Kind+"/"+it.Name+": "+l)
		}
	}
	return out
}
