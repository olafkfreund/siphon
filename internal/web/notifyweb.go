package web

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/notify"
	"github.com/olafkfreund/siphon/internal/store"
)

// notifyView is the Notifications page.
type notifyView struct {
	Channels []notifyChan
	Recent   []store.Notification
	Form     notifyForm
	Notice   string
	Editable bool
}

// AllEvents lists the events the form offers.
func (*notifyView) AllEvents() []string { return config.NotifyEvents }

type notifyChan struct {
	Name, Type string
	Events     []string
	Last       *store.Notification
}

// notifyForm is the add form. The url and token are write-only: they are never
// part of what is shown again, even after an error.
type notifyForm struct {
	Name, Type string
	Events     []string
	Err        string
}

type notifyTest struct {
	OK     bool
	Status int
	Err    string
}

func (s *server) notifyPage(notice string, f notifyForm) (*notifyView, error) {
	v := &notifyView{Form: f, Notice: notice, Editable: s.ConfigPath != ""}
	if v.Form.Type == "" {
		v.Form.Type = "ntfy"
	}
	if v.Form.Events == nil {
		v.Form.Events = slices.Clone(config.NotifyEvents)
	}
	cfg := s.Config()
	for _, name := range sortedNotify(cfg) {
		ch := cfg.Notify[name]
		c := notifyChan{Name: name, Type: ch.Type, Events: ch.Events}
		if len(c.Events) == 0 {
			c.Events = config.NotifyEvents
		}
		last, err := store.ListNotifications(s.Store.DB, store.NotificationFilter{Channel: name, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(last) > 0 {
			c.Last = &last[0]
		}
		v.Channels = append(v.Channels, c)
	}
	var err error
	v.Recent, err = store.ListNotifications(s.Store.DB, store.NotificationFilter{Limit: 50})
	return v, err
}

func sortedNotify(cfg *config.Config) []string {
	var out []string
	for n, ch := range cfg.Notify {
		if ch != nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// addNotify saves a channel through the normal edit path, so validation (type,
// https or a listed private endpoint) and write-only secret storage are the
// same as for every other edit.
func (s *server) addNotify(actor, name, typ, rawURL, token string, events []string) error {
	if s.ConfigPath == "" {
		return errNoEditing
	}
	rawURL = strings.TrimSpace(rawURL)
	if !itemName.MatchString(name) || !slices.Contains(config.NotifyTypes, typ) || rawURL == "" {
		return errInvalid{"choose a type, a name (letters, digits, . _ -) and give the URL"}
	}
	if len(events) == 0 {
		return errInvalid{"choose at least one event"}
	}
	for _, e := range events {
		if !slices.Contains(config.NotifyEvents, e) {
			return errInvalid{"unknown event " + fmt.Sprintf("%q", e)}
		}
	}
	dir := secretsDir(s.Config().Server.DB)
	pending := []pendingSecret{{Kind: "notify", Name: name, Key: "url", Value: rawURL}}
	y := fmt.Sprintf("type: %s\nurl: file:%s\n", typ, pending[0].path(dir))
	if token = strings.TrimSpace(token); token != "" {
		ps := pendingSecret{Kind: "notify", Name: name, Key: "token", Value: token}
		y += "token: file:" + ps.path(dir) + "\n"
		pending = append(pending, ps)
	}
	if len(events) < len(config.NotifyEvents) {
		y += "events: [" + strings.Join(events, ", ") + "]\n"
	}
	_, _, _, err := s.commit(actor, "notify/"+name+" saved", nil, putItem("notify", name, y), pending)
	return err
}

func (s *server) removeNotify(actor, name string) error {
	if s.ConfigPath == "" {
		return errNoEditing
	}
	if s.Config().Notify[name] == nil {
		return os.ErrNotExist
	}
	mutate, err := s.deleteItem("notify", name)
	if err != nil {
		return err
	}
	if _, _, _, err := s.commit(actor, "notify/"+name+" deleted", nil, mutate, nil); err != nil {
		return err
	}
	return nil // commit removes the secret files
}

func (s *server) notifyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /notifications", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		r.ParseForm()
		name, typ, events := r.PostFormValue("name"), r.PostFormValue("type"), r.PostForm["events"]
		err := s.addNotify(s.actor(r), name, typ, r.PostFormValue("url"), r.PostFormValue("token"), events)
		var inv errInvalid
		if errors.As(err, &inv) {
			v := view{CSRF: csrf}
			v.Notify, _ = s.notifyPage("", notifyForm{Name: name, Type: typ, Events: events, Err: inv.msg})
			s.pageStatus(w, r, "notify", v, http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, "/notifications?added="+name, http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /notifications/{name}/delete", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		if err := s.removeNotify(s.actor(r), r.PathValue("name")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /notifications/{name}/test", s.portalAs(roleOperator, func(w http.ResponseWriter, r *http.Request, _ string) {
		name := r.PathValue("name")
		if s.Config().Notify[name] == nil {
			http.NotFound(w, r)
			return
		}
		status, err := notify.New(s.Store, s.Config, s.Now).Send(r.Context(), name, notify.TestMessage(), s.actor(r))
		t := notifyTest{OK: err == nil, Status: status}
		if err != nil {
			t.Err = err.Error()
		}
		s.render(w, "notifytest", t)
	}))
}
