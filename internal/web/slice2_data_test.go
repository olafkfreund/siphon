package web

import (
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func viewOf(t *testing.T, e *env, name string) sourceView {
	t.Helper()
	for _, v := range sourceViews(t, e) {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no source %s", name)
	return sourceView{}
}

func TestSourceViewSlice2(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Sources["tick"] = &config.Source{Type: "schedule", At: "0 6 * * *", Timezone: "UTC"}
		o.Cfg.Sources["stdio"] = &config.Source{Type: "mcp", Command: []string{"npx", "-y", "secret-arg"}}
	})
	if v := viewOf(t, e, "disk"); v.Rules != 2 || v.Target != "" {
		t.Errorf("rules/target: %+v", v)
	}
	if v := viewOf(t, e, "tick"); v.Target != "0 6 * * *" {
		t.Errorf("schedule target: %q", v.Target)
	}
	if v := viewOf(t, e, "stdio"); v.Target != "stdio: npx" {
		t.Errorf("stdio target: %q", v.Target)
	}
	// the webhook URL: path only, then with server.public_url
	if v := viewOf(t, e, "gh"); v.Target != "/hook/gh" {
		t.Errorf("no public_url: %q", v.Target)
	}
	e.srv.Config().Server.PublicURL = "https://siphon.example/"
	if v := viewOf(t, e, "gh"); v.Target != "https://siphon.example/hook/gh" {
		t.Errorf("public_url: %q", v.Target)
	}
}

func TestSourceViewStaleAndHot(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Sources["tick"] = &config.Source{Type: "schedule", At: "0 6 * * *", Timezone: "UTC"}
	})
	must(t, store.SetSourceEvent(e.st.DB, "gh", map[string]any{"a": 1}, nil))
	if v := viewOf(t, e, "gh"); v.Health != "ok" {
		t.Fatalf("delivered: %+v", v)
	}
	// a refusal newer than the delivery: stale, not hot
	e.now = e.now.Add(time.Hour)
	must(t, store.SetSourceReject(e.st.DB, "gh", e.now, 401, "bad signature"))
	if v := viewOf(t, e, "gh"); v.Health != "stale" || v.Hot {
		t.Errorf("refusing: %+v", v)
	}
	// the schedule's last run counts toward hot
	last := e.now.Add(-time.Minute)
	must(t, store.SetSourceEvent(e.st.DB, "tick", map[string]string{"scheduled_at": last.UTC().Format(time.RFC3339)}, nil))
	if _, err := e.st.DB.Exec(`UPDATE source_state SET last_poll_at=? WHERE source='tick'`, e.now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if v := viewOf(t, e, "tick"); v.LastRunAt == nil || !v.Hot {
		t.Errorf("schedule hot via LastRunAt: %+v", v)
	}
}

func TestApprovalActionSourceAndSoon(t *testing.T) {
	e := newEnv(t, nil)
	as := []store.PendingApproval{
		{JobID: 1, Rule: "disk-full", ExpiresAt: e.now.Add(10 * time.Minute)},
		{JobID: 2, Rule: "other", ExpiresAt: e.now.Add(time.Hour)},
		{JobID: 3, Rule: "gone", ExpiresAt: e.now.Add(time.Hour)},
	}
	got := e.srv.approvalActions(nil, as)
	if g := got[1]; g.Source != "disk" || g.SourceType != "http" || !g.Soon {
		t.Errorf("job 1: %+v", g)
	}
	if g := got[2]; g.Soon {
		t.Errorf("job 2 not soon: %+v", g)
	}
	if g := got[3]; g.Source != "" || g.SourceType != "" {
		t.Errorf("unknown rule: %+v", g)
	}
}
