package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/olafkfreund/siphon/internal/notify"
	"github.com/olafkfreund/siphon/internal/store"
)

// notifyAPI mounts the notification channel test and the outbox listing.
func (s *server) notifyAPI(mux *http.ServeMux) {
	route := func(pattern string, f func(r *http.Request, actor string) (any, int, error)) {
		mux.HandleFunc(pattern, s.api(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			s.reply(w, r, func(r *http.Request) (any, int, error) {
				actor, err := apiActor(r)
				if err != nil {
					return nil, 400, err
				}
				return f(r, actor)
			})
		}))
	}
	// POST /api/notify/{name}/test sends one test message, bypassing the outbox.
	// A failed send is still a 200: {"ok": false, "error": "..."} (masked).
	route("POST /api/notify/{name}/test", func(r *http.Request, actor string) (any, int, error) {
		name := r.PathValue("name")
		if s.Config().Notify[name] == nil {
			return nil, 404, errMsg("no such notification channel")
		}
		status, err := notify.New(s.Store, s.Config, s.Now).Send(r.Context(), name, notify.TestMessage(), actor)
		res := map[string]any{"ok": err == nil, "status": status}
		if err != nil {
			res["error"] = err.Error()
		}
		return res, 200, nil
	})
	route("GET /api/notifications", func(r *http.Request, _ string) (any, int, error) {
		q := r.URL.Query()
		f := store.NotificationFilter{Channel: q.Get("channel"), Limit: limit(r)}
		if v := q.Get("job"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, 400, errMsg("bad job id")
			}
			f.Job = id
		}
		rows, err := store.ListNotifications(s.Store.DB, f)
		out := []notificationRow{}
		for _, n := range rows {
			o := notificationRow{ID: n.ID, Channel: n.Channel, Event: n.Event, Job: n.JobID, Source: n.Source, Title: n.Title,
				Body: n.Body, State: n.State, Attempts: n.Attempts, Error: n.Error, CreatedAt: n.CreatedAt.UTC().Format(time.RFC3339)}
			if n.SentAt != nil {
				o.SentAt = n.SentAt.UTC().Format(time.RFC3339)
			}
			out = append(out, o)
		}
		return out, 200, err
	})
}

type notificationRow struct {
	ID        int64  `json:"id"`
	Channel   string `json:"channel"`
	Event     string `json:"event"`
	Job       int64  `json:"job,omitempty"`
	Source    string `json:"source,omitempty"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
	SentAt    string `json:"sent_at,omitempty"`
}
