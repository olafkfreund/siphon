package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

type rec struct {
	hdr  http.Header
	body string
}

type env struct {
	t      *testing.T
	st     *store.Store
	now    time.Time
	n      *Notifier
	mu     sync.Mutex
	got    []rec
	status int
	srv    *httptest.Server
}

// newEnv has one channel "hook" (type given) pointing at an httptest server
// that is listed as a private endpoint, so the real guard is used.
func newEnv(t *testing.T, typ, extra string) *env {
	e := &env{t: t, now: time.UnixMilli(1_700_000_000_000), status: 200}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.got = append(e.got, rec{r.Header.Clone(), string(b)})
		code := e.status
		e.mu.Unlock()
		if code/100 == 3 {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(code)
		io.WriteString(w, "echo s3cr3t-token")
	}))
	t.Cleanup(e.srv.Close)
	host := strings.TrimPrefix(e.srv.URL, "http://")
	t.Setenv("N_URL", e.srv.URL+"/hook-secret-path")
	t.Setenv("N_TOK", "s3cr3t-token")
	cfg, err := config.Parse([]byte("server: {services: {private_endpoints: ['" + host + "']}, public_url: 'https://siphon.example'}\nnotify:\n  hook: {type: " + typ + ", url: 'env:N_URL', token: 'env:N_TOK'" + extra + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	e.n = New(st, func() *config.Config { return cfg }, func() time.Time { return e.now })
	return e
}

func (e *env) count() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.got) }

func (e *env) run() {
	e.t.Helper()
	if err := e.n.Scan(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	if err := e.n.Deliver(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) job(state string, step int, finished any) int64 {
	r, err := e.st.DB.Exec(`INSERT INTO jobs(rule,action_json,state,finished_at,resume_step,created_at) VALUES ('r1','{"action":{"Agent":"rev"}}',?,?,?,?)`,
		state, finished, step, e.now.UnixMilli())
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

func (e *env) approval(job int64, decided any) {
	e.st.DB.Exec(`INSERT INTO approvals(job_id,token_hash,expires_at,decision) VALUES (?,?,?,?)`, job, []byte("h"), e.now.Add(store.ApprovalTTL).UnixMilli(), decided)
}

func (e *env) rows() []store.Notification {
	l, err := store.ListNotifications(e.st.DB, store.NotificationFilter{Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// An approval made after the channel was added but before the notifier's
// first scan of it (up to one interval) is still sent.
func TestBaselineCoversTheFirstInterval(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.approval(e.job("pending_approval", 0, nil), nil)
	e.now = e.now.Add(scanEvery - time.Second)
	e.run()
	if e.count() != 1 {
		t.Fatalf("approval in the first interval: want 1 sent, got %d", e.count())
	}
}

func TestBaselineThenEachEventOnce(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.now = e.now.Add(-time.Hour) // history: made before the channel exists
	e.job("failed", 0, e.now.UnixMilli())
	e.approval(e.job("pending_approval", 0, nil), nil)
	e.now = e.now.Add(time.Hour)
	e.run() // first scan: baseline only, history is never sent
	if e.count() != 0 {
		t.Fatalf("history sent: %d", e.count())
	}
	e.now = e.now.Add(time.Minute)
	f := e.job("failed", 0, e.now.UnixMilli())
	ap := e.job("pending_approval", 0, nil)
	e.approval(ap, nil)
	e.run()
	e.run() // a second scan adds nothing
	if e.count() != 2 {
		t.Fatalf("want 2 sent, got %d", e.count())
	}
	byEvent := map[string]map[string]any{}
	for _, r := range e.got {
		var m map[string]any
		json.Unmarshal([]byte(r.body), &m)
		byEvent[m["event"].(string)] = m
		if r.hdr.Get("Authorization") != "Bearer s3cr3t-token" {
			t.Error("no bearer")
		}
	}
	if byEvent["failed"]["job"] != float64(f) || byEvent["failed"]["rule"] != "r1" || byEvent["failed"]["url"] != "https://siphon.example/jobs/"+itoa(f) {
		t.Errorf("failed: %v", byEvent["failed"])
	}
	if byEvent["approval"]["job"] != float64(ap) || !strings.Contains(byEvent["approval"]["message"].(string), "agent") {
		t.Errorf("approval: %v", byEvent["approval"])
	}
	if _, err := time.Parse(time.RFC3339, byEvent["failed"]["at"].(string)); err != nil {
		t.Error(err)
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }

func TestEventsFilter(t *testing.T) {
	e := newEnv(t, "webhook", ", events: [failed]")
	e.run()
	e.now = e.now.Add(time.Minute)
	e.job("failed", 0, e.now.UnixMilli())
	e.approval(e.job("pending_approval", 0, nil), nil)
	e.run()
	if e.count() != 1 || !strings.Contains(e.got[0].body, `"event":"failed"`) {
		t.Fatalf("%+v", e.got)
	}
}

func TestReminderWindowAndRePause(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.run()
	e.now = e.now.Add(time.Hour)
	j := e.job("pending_approval", 0, nil)
	e.approval(j, nil)
	e.run()                           // approval only
	e.now = e.now.Add(19 * time.Hour) // 4h left minus an hour of slack: 5h left → no reminder yet
	e.run()
	if e.count() != 1 {
		t.Fatalf("early reminder: %d", e.count())
	}
	e.now = e.now.Add(time.Hour + time.Minute) // under 4h left
	e.run()
	e.run()
	if e.count() != 2 || !strings.Contains(e.got[1].body, `"event":"reminder"`) {
		t.Fatalf("%d %+v", e.count(), e.got)
	}
	// The routine pauses again at its next step: a new key, a new approval message.
	e.st.DB.Exec(`UPDATE approvals SET decision='approved'`)
	e.st.DB.Exec(`UPDATE jobs SET resume_step=2 WHERE id=?`, j)
	e.approval(j, nil)
	e.run()
	if e.count() != 3 { // the new approval has a full TTL left: no reminder
		t.Fatalf("re-pause: %d", e.count())
	}
	last := e.got[len(e.got)-1].body
	if !strings.Contains(last, `"event":"approval"`) {
		t.Fatalf("re-pause message: %s", last)
	}
}

func TestSourceThresholdAndRecovery(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.run()
	t0 := e.now
	store.PutSourceState(e.st.DB, "feed", t0.Add(time.Minute), "boom")
	e.now = t0.Add(10 * time.Minute)
	e.run()
	if e.count() != 0 {
		t.Fatal("sent before 15 min")
	}
	e.now = t0.Add(17 * time.Minute)
	e.run()
	e.run()
	if e.count() != 1 || !strings.Contains(e.got[0].body, `"source":"feed"`) || !strings.Contains(e.got[0].body, `"event":"source"`) {
		t.Fatalf("%+v", e.got)
	}
	store.PutSourceState(e.st.DB, "feed", e.now.Add(time.Minute), "")
	e.now = e.now.Add(time.Minute)
	e.run()
	e.run()
	if e.count() != 2 || !strings.Contains(e.got[1].body, `"event":"source_ok"`) {
		t.Fatalf("%+v", e.got)
	}
	// A source that recovers before 15 min never sends anything.
	store.PutSourceState(e.st.DB, "blip", e.now, "x")
	e.now = e.now.Add(5 * time.Minute)
	store.PutSourceState(e.st.DB, "blip", e.now, "")
	e.now = e.now.Add(time.Hour)
	e.run()
	if e.count() != 2 {
		t.Fatalf("blip notified: %d", e.count())
	}
}

func TestBackoffAndFinalFailure(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.run()
	e.now = e.now.Add(time.Second)
	e.job("failed", 0, e.now.UnixMilli())
	e.status = 503
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}
	for i, d := range want {
		e.run()
		r := e.rows()[0]
		if r.State != "pending" || r.Attempts != i+1 || !r.NextAt.Equal(e.now.Add(d)) {
			t.Fatalf("attempt %d: %+v (want next %v)", i+1, r, e.now.Add(d))
		}
		e.now = e.now.Add(d - time.Second)
		e.run() // not due yet
		if e.count() != i+1 {
			t.Fatalf("sent before due: %d", e.count())
		}
		e.now = e.now.Add(time.Second)
	}
	e.run()
	r := e.rows()[0]
	if r.State != "failed" || r.Attempts != 6 || e.count() != 6 {
		t.Fatalf("%+v %d", r, e.count())
	}
	var detail string
	if e.st.DB.QueryRow(`SELECT detail FROM audit WHERE event='notify_failed'`).Scan(&detail) != nil || strings.Contains(detail, "s3cr3t") {
		t.Fatalf("audit: %q", detail)
	}
}

func TestStatusClasses(t *testing.T) {
	for code, state := range map[int]string{400: "failed", 404: "failed", 408: "pending", 429: "pending", 500: "pending", 302: "failed", 204: "sent"} {
		e := newEnv(t, "webhook", "")
		e.run()
		e.now = e.now.Add(time.Second)
		e.job("failed", 0, e.now.UnixMilli())
		e.status = code
		e.run()
		if got := e.rows()[0].State; got != state {
			t.Errorf("%d: %s, want %s", code, got, state)
		}
		if code == 302 && e.count() != 1 {
			t.Errorf("redirect followed: %d requests", e.count())
		}
	}
}

func TestRateCapAndSuppressedSuffix(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.run()
	for i := 0; i < 32; i++ {
		store.EnqueueNotification(e.st.DB, store.Notification{Channel: "hook", Event: "failed", Key: "k" + itoa(int64(i)), Title: "t", Body: "b"}, e.now)
	}
	e.run()
	if e.count() != 30 {
		t.Fatalf("delivered %d", e.count())
	}
	sup := 0
	for _, r := range e.rows() {
		if r.State == "suppressed" {
			sup++
		}
	}
	if sup != 2 {
		t.Fatalf("suppressed %d", sup)
	}
	e.now = e.now.Add(61 * time.Minute)
	store.EnqueueNotification(e.st.DB, store.Notification{Channel: "hook", Event: "failed", Key: "later", Title: "t", Body: "b"}, e.now)
	e.run()
	if e.count() != 31 || !strings.Contains(e.got[30].body, "b (2 more suppressed)") {
		t.Fatalf("%d %s", e.count(), e.got[len(e.got)-1].body)
	}
	store.EnqueueNotification(e.st.DB, store.Notification{Channel: "hook", Event: "failed", Key: "later2", Title: "t", Body: "b"}, e.now)
	e.run()
	if strings.Contains(e.got[31].body, "suppressed") {
		t.Fatal("suppressed count not reset")
	}
}

type failTransport struct{ err error }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestErrorsAreMasked(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.n.Client = func(*config.Config, string) (*http.Client, func(), error) {
		return &http.Client{Transport: failTransport{errors.New("dial " + e.srv.URL + "/hook-secret-path with s3cr3t-token failed")}}, nil, nil
	}
	e.run()
	e.now = e.now.Add(time.Second)
	e.job("failed", 0, e.now.UnixMilli())
	e.run()
	r := e.rows()[0]
	for _, s := range []string{"s3cr3t-token", "hook-secret-path", e.srv.URL} {
		if strings.Contains(r.Error, s) {
			t.Fatalf("error leaks %q: %s", s, r.Error)
		}
	}
	if r.Error == "" {
		t.Fatal("no error stored")
	}
	_, err := e.n.Send(context.Background(), "hook", TestMessage(), "me")
	if err == nil || strings.Contains(err.Error(), "s3cr3t-token") || strings.Contains(err.Error(), "hook-secret-path") {
		t.Fatalf("Send error: %v", err)
	}
}

func TestPrivateAddressGuard(t *testing.T) {
	e := newEnv(t, "webhook", "")
	// Listed: delivered.
	if st, err := e.n.Send(context.Background(), "hook", TestMessage(), "me"); err != nil || st != 200 || e.count() != 1 {
		t.Fatalf("listed: %d %v", st, err)
	}
	var a string
	if e.st.DB.QueryRow(`SELECT detail FROM audit WHERE event='notify_test'`).Scan(&a) != nil || a != "hook: ok" {
		t.Fatalf("audit %q", a)
	}
	// Not listed: refused before any request.
	cfg, _ := config.Parse([]byte("notify:\n  hook: {type: webhook, url: 'env:N_URL'}\n"))
	e.n.Config = func() *config.Config { return cfg }
	if _, err := e.n.Send(context.Background(), "hook", TestMessage(), "me"); err == nil || e.count() != 1 {
		t.Fatalf("unlisted loopback reached: %v %d", err, e.count())
	}
}

func TestRequestFormats(t *testing.T) {
	cfg, _ := config.Parse([]byte("server: {public_url: 'https://s.example/'}\n"))
	m := Message{Event: "approval", Title: "Approval needed", Body: "Rule r: job 7 (agent) is waiting.", Job: 7, At: time.Unix(0, 0)}
	mk := func(typ, tok string) *http.Request {
		ch := &config.Notify{Type: typ, URL: config.Secret{Value: "https://h.example/x"}, Token: config.Secret{Value: tok}}
		r, err := request(context.Background(), cfg, ch, m)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := mk("ntfy", "tk")
	b, _ := io.ReadAll(r.Body)
	if r.Header.Get("Title") != "Approval needed" || r.Header.Get("Priority") != "high" || r.Header.Get("Click") != "https://s.example/jobs/7" ||
		r.Header.Get("Tags") == "" || r.Header.Get("Authorization") != "Bearer tk" || string(b) != m.Body {
		t.Errorf("ntfy: %v %s", r.Header, b)
	}
	r = mk("slack", "tk")
	b, _ = io.ReadAll(r.Body)
	var sl map[string]string
	json.Unmarshal(b, &sl)
	if r.Header.Get("Authorization") != "" || sl["text"] != "*Approval needed*\nRule r: job 7 (agent) is waiting. <https://s.example/jobs/7|Open in Siphon>" {
		t.Errorf("slack: %v %s", r.Header, b)
	}
	r = mk("webhook", "")
	b, _ = io.ReadAll(r.Body)
	var got map[string]any
	json.Unmarshal(b, &got)
	if r.Header.Get("Authorization") != "" || got["event"] != "approval" || got["url"] != "https://s.example/jobs/7" || got["at"] != "1970-01-01T00:00:00Z" || got["message"] != m.Body {
		t.Errorf("webhook: %v", got)
	}
	// Without public_url: a CLI hint instead of a link.
	cfg, _ = config.Parse([]byte("{}"))
	r = mk("ntfy", "")
	b, _ = io.ReadAll(r.Body)
	if r.Header.Get("Click") != "" || !strings.Contains(string(b), "siphon approve 7") || r.Header.Get("Priority") != "high" {
		t.Errorf("hint: %v %s", r.Header, b)
	}
}

func TestDeletedChannelRestartsFresh(t *testing.T) {
	e := newEnv(t, "webhook", "")
	e.run()
	cfg := e.n.Config()
	e.n.Config = func() *config.Config { return &config.Config{} }
	e.run() // the channel is gone: its baseline goes
	if names, _ := store.ChannelNames(e.st.DB); len(names) != 0 {
		t.Fatalf("baseline kept: %v", names)
	}
	e.n.Config = func() *config.Config { return cfg }
	e.now = e.now.Add(time.Hour)
	e.run()
	if since, _ := store.ChannelSince(e.st.DB, "hook", e.now.Add(time.Hour)); !since.Equal(e.now.Add(-2*scanEvery)) {
		t.Fatalf("not fresh: %v", since)
	}
}
