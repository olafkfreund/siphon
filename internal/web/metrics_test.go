package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/config"
)

func TestMetrics(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Version = "v1"
		o.Cfg.Notify = map[string]*config.Notify{"phone": {}}
	})
	db := e.st.DB
	db.Exec(`INSERT INTO source_state(source, last_poll_at, last_error, failing_since) VALUES
		('disk', 5000, 'LEAKY-ERROR-TEXT https://secret.example/x', 1), ('gone', 7000, 'x', 1), ('gh', 1, '', NULL)`)
	db.Exec(`INSERT INTO rule_error(rule, at, error) VALUES ('other', 1, 'LEAKY-RULE-ERROR'), ('deleted', 1, 'x')`)
	db.Exec(`INSERT INTO notifications(channel, event, key, title, body, created_at, next_at) VALUES
		('phone','test','k','t','b',1,1), ('oldchan','test','k','t','b',1,1)`)
	db.Exec(`INSERT INTO jobs(rule, action_json, state, created_at) VALUES ('disk-full','{}','failed',1)`)

	if w := e.do("GET", "/metrics", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	w := e.do("GET", "/metrics", nil, bearer)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, f := range regexp.MustCompile(`(?m)^# TYPE (\S+) gauge$`).FindAllStringSubmatch(body, -1) {
		if strings.Count(body, "# TYPE "+f[1]+" gauge") != 1 || strings.Count(body, "# HELP "+f[1]+" ") != 1 {
			t.Fatalf("family %s not declared once", f[1])
		}
	}
	for _, want := range []string{
		`siphon_build_info{version="v1"} 1`,
		`siphon_jobs{state="failed"} 1`,
		`siphon_jobs{state="queued"} 0`,
		`siphon_source_failing{source="disk"} 1`,
		`siphon_source_last_poll_timestamp_seconds{source="disk"} 5`,
		`siphon_rule_error{rule="other"} 1`,
		`siphon_rule_error{rule="disk-full"} 0`,
		`siphon_notifications{channel="phone",state="pending"} 1`,
		`siphon_agent_runs_daily_limit 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	for _, bad := range []string{"gone", "gh", "deleted", "oldchan", "LEAKY", "secret.example"} {
		if strings.Contains(body, bad) {
			t.Errorf("%q leaked into\n%s", bad, body)
		}
	}
}

func TestMetricsToken(t *testing.T) {
	const scrape = "scrape-token-0123456789abcdef012345"
	sc := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+scrape) }
	e := newEnv(t, func(o *Options) { o.MetricsToken = scrape })
	if w := e.do("GET", "/metrics", nil, sc); w.Code != 200 {
		t.Fatalf("scrape on /metrics: %d", w.Code)
	}
	if w := e.do("GET", "/metrics", nil, bearer); w.Code != 200 {
		t.Fatalf("admin on /metrics: %d", w.Code)
	}
	if w := e.do("GET", "/", nil, sc); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("scrape on the portal: %d %q", w.Code, w.Header().Get("Location"))
	}
	// failing requests last: five per IP per minute trips the limiter
	if w := e.do("GET", "/api/inventory", nil, sc); w.Code != 401 {
		t.Fatalf("scrape on /api: %d", w.Code)
	}
	if w := e.do("POST", "/login", url.Values{"token": {scrape}}, nil); w.Code != 401 {
		t.Fatalf("scrape on login: %d", w.Code)
	}
	if w := e.do("GET", "/metrics", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }); w.Code != 401 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	e = newEnv(t, nil)
	if w := e.do("GET", "/metrics", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }); w.Code != 401 {
		t.Fatalf("empty token, none configured: %d", w.Code)
	}
}
