package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/olafkfreund/siphon/internal/catalog"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// The Services page: what is connected (grouped by connection), then the
// catalogue to explore, and one focused page per service to connect it.

var categoryLabels = []struct{ ID, Label string }{
	{"code", "Code hosting"}, {"issues", "Issue tracking"}, {"chat", "Chat"}, {"monitoring", "Monitoring & alerting"},
	{"cloud", "Cloud"}, {"payments", "Payments"}, {"home", "Home & IoT"}, {"generic", "Generic"},
}

var capLabels = map[string]string{"tools": "Tools", "polling": "Polling", "webhooks": "Webhooks"}

var availLabels = map[string]string{"available": "Available", "needs-package": "Needs package", "not-yet": "Not yet"}

type connItem struct{ Kind, Name, Href string }

type connRow struct {
	Name, Service, ServiceName, Mark string
	Caps                             []string
	Status, StatusLabel, StatusTitle string // unchecked | working | attention
	LastEvent                        string
	CanTest                          bool
	Hint                             string // why there is no Test
	Items                            []connItem
}

type svcTile struct {
	ID, Name, Mark, Summary, Avail, AvailLabel, Why string
	Caps                                            []string
}

type catLink struct {
	Label, Href string
	Selected    bool
}

type servicesView struct {
	Connected []connRow
	Tiles     []svcTile
	Q, Cat    string
	Cats      []catLink
}

// legacyKey groups an unlabelled service item with its siblings: the
// webhook and the AWS servers share the name of their setup.
func legacyKey(name string) string {
	for _, suf := range []string{"-hooks", "-cloudwatch", "-docs"} {
		if k, ok := strings.CutSuffix(name, suf); ok {
			return k
		}
	}
	return name
}

func (s *server) connectedRows() []connRow {
	cfg := s.Config()
	type grp struct {
		svc  string
		srcs []string
		cred []string
	}
	groups := map[string]*grp{}
	get := func(k string) *grp {
		if groups[k] == nil {
			groups[k] = &grp{}
		}
		return groups[k]
	}
	for _, name := range sortedKeysOf(cfg.Sources) {
		src := cfg.Sources[name]
		svc := serviceFor(src)
		key := src.Connection
		if key == "" {
			if svc == "" {
				continue // not made by a service
			}
			key = legacyKey(name)
		}
		g := get(key)
		if g.svc == "" {
			g.svc = svc
		}
		g.srcs = append(g.srcs, name)
	}
	for _, name := range sortedKeysOf(cfg.Credentials) {
		cr := cfg.Credentials[name]
		key := cr.Connection
		if key == "" && cr.Provider == "aws" && groups[name] != nil {
			key = name // a legacy aws credential joins its servers
		}
		if key == "" {
			continue
		}
		g := get(key)
		if g.svc == "" {
			g.svc = cr.Service
			if g.svc == "" && cr.Provider == "aws" {
				g.svc = "aws"
			}
		}
		g.cred = append(g.cred, name)
	}
	keys := sortedKeysOf(groups)
	out := make([]connRow, 0, len(keys))
	for _, key := range keys {
		g := groups[key]
		r := connRow{Name: key, Service: g.svc, ServiceName: "Connection", Mark: "··"}
		e := catalog.Get(g.svc)
		if e != nil {
			r.ServiceName, r.Mark = e.Name, e.Mark
		}
		caps := map[string]bool{}
		var newest *store.SourceDiag
		for _, n := range g.srcs {
			src := cfg.Sources[n]
			switch src.Type {
			case "mcp":
				caps["tools"] = true
			case "http":
				caps["polling"] = true
			case "webhook":
				caps["webhooks"] = true
			}
			if src.AWS != "" {
				r.CanTest = true
			}
			r.Items = append(r.Items, connItem{"sources", n, "/config/sources/" + url.PathEscape(n)})
			if d, err := store.SourceDiagnostics(s.Store.DB, n); err == nil && d.EventAt != nil && (newest == nil || d.EventAt.After(*newest.EventAt)) {
				newest = &d
			}
		}
		for _, n := range g.cred {
			r.Items = append(r.Items, connItem{"credentials", n, "/config/credentials/" + url.PathEscape(n)})
		}
		for _, c := range []string{"tools", "polling", "webhooks"} {
			if caps[c] {
				r.Caps = append(r.Caps, capLabels[c])
			}
		}
		if e != nil && e.Test != nil { // only when a token is stored to test with
			tok, _ := connInputs(cfg, g.srcs, e.Test)
			r.CanTest = r.CanTest || tok != ""
		}
		if !r.CanTest && caps["webhooks"] {
			r.Hint = "Webhooks: send an event to check"
		}
		r.LastEvent = "No events yet"
		if newest != nil {
			r.LastEvent = newest.EventAt.Local().Format("2006-01-02 15:04")
		}
		r.Status, r.StatusLabel = "unchecked", "Not checked"
		if c, err := store.GetConnectionCheck(s.Store.DB, key); err == nil && c != nil {
			r.StatusTitle = c.At.Local().Format("2006-01-02 15:04") + " · " + c.Detail
			if c.OK {
				r.Status, r.StatusLabel = "working", "Working"
			} else {
				r.Status, r.StatusLabel = "attention", "Needs attention"
			}
		}
		out = append(out, r)
	}
	return out
}

func sortedKeysOf[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (s *server) servicesView(q, cat string) *servicesView {
	v := &servicesView{Connected: s.connectedRows(), Q: q, Cat: cat}
	cfg := s.Config()
	needle := strings.ToLower(strings.TrimSpace(q))
	have := map[string]bool{}
	for _, e := range catalog.All() {
		have[e.Category] = true
		if cat != "" && e.Category != cat {
			continue
		}
		hay := strings.ToLower(e.ID + " " + e.Name + " " + e.Summary + " " + e.Category + " " + strings.Join(e.Capabilities, " "))
		if needle != "" && !strings.Contains(hay, needle) {
			continue
		}
		st, why := e.Availability(cfg)
		t := svcTile{ID: e.ID, Name: e.Name, Mark: e.Mark, Summary: e.Summary, Avail: st, AvailLabel: availLabels[st], Why: why}
		for _, c := range e.Capabilities {
			t.Caps = append(t.Caps, capLabels[c])
		}
		v.Tiles = append(v.Tiles, t)
	}
	href := func(c string) string {
		u := url.Values{}
		if q != "" {
			u.Set("q", q)
		}
		if c != "" {
			u.Set("cat", c)
		}
		if len(u) == 0 {
			return "/services"
		}
		return "/services?" + u.Encode()
	}
	v.Cats = append(v.Cats, catLink{"All", href(""), cat == ""})
	for _, c := range categoryLabels {
		if have[c.ID] {
			v.Cats = append(v.Cats, catLink{c.Label, href(c.ID), cat == c.ID})
		}
	}
	return v
}

// formField is one field of the connect page. Secrets never carry a value.
type formField struct {
	catalog.Field
	Value    string
	Checked  bool
	Selected map[string]bool
	Err      string
	Options  []string        // a select, from the install's allowlist
	List     []string        // suggestions (a datalist)
	Disabled map[string]bool // choices that can't be picked here
	Note     string
}

type formSection struct {
	N      int
	Title  string
	Fields []formField
}

type connectView struct {
	ID, Name, Mark, Summary string
	Sections                []formSection
	Err                     string // not tied to a field
	Available               bool
	Why                     string
}

var sectionTitles = map[int]string{1: "What to connect", 2: "Access", 3: "Events"}

// connectForm builds the connect page. first is the untouched page (defaults
// apply); otherwise the posted values are kept, except secrets.
func (s *server) connectForm(e *catalog.Entry, posted map[string]string, first bool, errMsg, errField string) *connectView {
	cfg := s.Config()
	st, why := e.Availability(cfg)
	v := &connectView{ID: e.ID, Name: e.Name, Mark: e.Mark, Summary: e.Summary, Available: st == "available", Why: why}
	by := map[int][]formField{}
	for _, f := range e.Fields {
		ff := formField{Field: f, Selected: map[string]bool{}, Disabled: map[string]bool{}}
		val := posted[f.Key]
		if first {
			val = f.Default
		}
		switch f.Type {
		case "secret":
		case "bool":
			ff.Checked = val != ""
		case "multi":
			for _, p := range strings.Split(val, ",") {
				ff.Selected[strings.TrimSpace(p)] = true
			}
		default:
			ff.Value = val
		}
		if f.Key == errField {
			ff.Err = errMsg
		}
		sec := f.Section
		if sec == 0 {
			switch f.Type {
			case "bool":
				sec = 3
			case "secret":
				sec = 2
			default:
				sec = 1
			}
		}
		by[sec] = append(by[sec], ff)
	}
	if e.ID == "aws" {
		awsAdjust(by[2], cfg, first)
	}
	n := 0
	for sec := 1; sec <= 3; sec++ {
		if len(by[sec]) == 0 {
			continue
		}
		n++
		v.Sections = append(v.Sections, formSection{n, sectionTitles[sec], by[sec]})
	}
	if errMsg != "" {
		shown := false
		for _, sec := range v.Sections {
			for _, f := range sec.Fields {
				shown = shown || f.Err != ""
			}
		}
		if !shown {
			v.Err = errMsg
		}
	}
	return v
}

// awsAdjust offers the install's allowlists: profile names to pick from,
// role ARNs as suggestions, and no profile mode when none is listed.
func awsAdjust(fs []formField, cfg *config.Config, first bool) {
	for i := range fs {
		switch fs[i].Key {
		case "profile":
			fs[i].Options = cfg.Server.AWS.Profiles
			if len(fs[i].Options) == 0 {
				fs[i].Note = "List profiles in server.aws.profiles in siphon.yaml."
			}
		case "role_arn":
			fs[i].List = cfg.Server.AWS.RoleARNs
		case "mode":
			if len(cfg.Server.AWS.Profiles) == 0 {
				fs[i].Disabled["profile"] = true
				if first || fs[i].Value == "profile" {
					fs[i].Value = "role"
				}
			}
		}
	}
}

func (s *server) servicePages(mux *http.ServeMux) {
	mux.HandleFunc("GET /services", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		cat := r.URL.Query().Get("cat")
		if !slices.ContainsFunc(categoryLabels, func(c struct{ ID, Label string }) bool { return c.ID == cat }) {
			cat = ""
		}
		s.page(w, r, "services", view{CSRF: csrf, Svc: s.servicesView(clip(r.URL.Query().Get("q"), 80), cat)})
	}))
	mux.HandleFunc("GET /services/{id}", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		e := catalog.Get(r.PathValue("id"))
		if e == nil {
			http.NotFound(w, r)
			return
		}
		s.page(w, r, "serviceconnect", view{CSRF: csrf, Connect: s.connectForm(e, nil, true, "", "")})
	}))
	mux.HandleFunc("POST /services/{id}", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		e := catalog.Get(r.PathValue("id"))
		if e == nil {
			http.NotFound(w, r)
			return
		}
		vals := formValues(e, r)
		done, err := s.connect("portal", e.ID, vals)
		var inv errInvalid
		if errors.As(err, &inv) {
			var fe fieldErr
			field := ""
			if errors.As(err, &fe) {
				field = fe.Field
			}
			s.pageStatus(w, r, "serviceconnect", view{CSRF: csrf, Connect: s.connectForm(e, vals, false, inv.msg, field)}, http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store") // the webhook secret is on this page
		s.page(w, r, "servicedone", view{CSRF: csrf, ServiceDone: done})
	}))
	mux.HandleFunc("POST /services/c/{name}/test", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		res, code, err := s.runConnectionTest(r.Context(), r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		s.render(w, "svctest", res.svcTest)
	}))
}
