package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// notifyEnv serves a CLI env whose config lists a local receiver as a private endpoint.
func notifyEnv(t *testing.T) (*cliEnv, *atomic.Int32, string) {
	t.Helper()
	var hits atomic.Int32
	status := new(atomic.Int32)
	status.Store(200)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(hook.Close)
	host := strings.TrimPrefix(hook.URL, "http://")
	e := newCLIEnvCfg(t, ", services: {private_endpoints: ['"+host+"']}", "")
	return e, &hits, hook.URL
}

func TestCLINotifyAddTestLog(t *testing.T) {
	e, hits, hookURL := notifyEnv(t)
	secret := hookURL + "/topic-xyz"

	// A secret on the command line is refused, before anything is sent.
	if code, _, er := e.do("notify", "add", "hook", "--type", "webhook", "--url", secret, "--yes"); code != 2 || strings.Contains(er, "topic-xyz") {
		t.Fatalf("plain url: %d %s", code, er)
	}
	if code, _, _ := e.do("notify", "add", "hook", "--type", "webhook", "--url", "-", "--token", "-", "--yes"); code != 2 {
		t.Fatalf("two stdin secrets: %d", code)
	}
	if code, _, _ := e.do("notify", "add", "hook", "--type", "sms", "--url", "-"); code != 2 {
		t.Fatalf("bad type: %d", code)
	}

	// Dry run shows the diff and changes nothing; -o json without --yes is not consent.
	e.stdin = secret + "\n"
	code, out, er := e.do("notify", "add", "hook", "--type", "webhook", "--url", "-", "--events", "failed,approval", "--dry-run")
	if code != 0 || !strings.Contains(out, "notify:") || strings.Contains(out+er, "topic-xyz") {
		t.Fatalf("dry run: %d %s %s", code, out, er)
	}
	e.stdin = secret + "\n"
	if code, _, _ := e.do("notify", "add", "hook", "--type", "webhook", "--url", "-", "-o", "json"); code != 2 {
		t.Fatalf("-o json without --yes: %d", code)
	}
	if out := e.ok("get", "notify"); strings.Contains(out, "hook") {
		t.Fatalf("something was stored: %s", out)
	}

	// The token comes from a file, the url from stdin.
	tokf := e.write("tok", "bearer-1\n")
	e.stdin = secret + "\n"
	e.ok("notify", "add", "hook", "--type", "webhook", "--url", "-", "--token", "@"+tokf, "--yes")
	if b, _ := os.ReadFile(filepath.Join(e.dir, "secrets", "notify--hook+url")); string(b) != secret {
		t.Fatalf("stored url: %q", b)
	}
	if out := e.ok("get", "notify"); !strings.Contains(out, "hook") {
		t.Fatalf("get notify: %s", out)
	}
	if out := e.ok("get", "notify", "hook"); strings.Contains(out, "topic-xyz") || strings.Contains(out, "bearer-1") || !strings.Contains(out, "type: webhook") {
		t.Fatalf("get one: %s", out)
	}

	if out := e.ok("notify", "test", "hook"); !strings.Contains(out, "sent (HTTP 200)") || hits.Load() != 1 {
		t.Fatalf("test: %s hits=%d", out, hits.Load())
	}
	if out := e.ok("notify", "test", "hook", "-o", "json"); !strings.Contains(out, `"ok": true`) {
		t.Fatalf("test json: %s", out)
	}
	if code, _, _ := e.do("notify", "test", "nope"); code != 4 {
		t.Fatalf("unknown channel: %d", code)
	}

	e.st.DB.Exec(`INSERT INTO notifications(channel,event,key,title,body,created_at,next_at,state) VALUES ('hook','failed','job:9','t','b',1,1,'sent')`)
	out = e.ok("notify", "log", "--channel", "hook")
	if !strings.Contains(out, "failed") || !strings.Contains(out, "sent") {
		t.Fatalf("log: %s", out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(e.ok("notify", "log", "-o", "json", "--channel", "other")), &rows); err != nil || len(rows) != 0 {
		t.Fatalf("log json: %v %v", rows, err)
	}

	// A failing channel (an unlisted private address is refused): exit 1, and the message does not carry the url.
	e.ok("delete", "notify", "hook")
	if out := e.ok("get", "notify"); strings.Contains(out, "hook") {
		t.Fatalf("delete: %s", out)
	}
	e.stdin = "https://localhost:1/topic-xyz\n"
	e.ok("notify", "add", "far", "--type", "ntfy", "--url", "-", "--yes")
	code, out, er = e.do("notify", "test", "far")
	if code != 1 || strings.Contains(out+er, "topic-xyz") {
		t.Fatalf("failing test: %d %s %s", code, out, er)
	}
}

func TestMCPNotifyTestGated(t *testing.T) {
	e, hits, hookURL := notifyEnv(t)
	e.stdin = hookURL + "/t\n"
	e.ok("notify", "add", "hook", "--type", "webhook", "--url", "-", "--yes")
	off := newMCPEnvFrom(t, e, false, false)
	if out, isErr := off.call(t, "notify_test", map[string]any{"name": "hook"}); isErr || !strings.Contains(out, "writes are disabled") || hits.Load() != 0 {
		t.Fatalf("gated: %s hits=%d", out, hits.Load())
	}
	on := newMCPEnvFrom(t, e, true, false)
	if out, isErr := on.call(t, "notify_test", map[string]any{"name": "hook"}); isErr || !strings.Contains(out, `"ok": true`) || hits.Load() != 1 {
		t.Fatalf("allowed: %s hits=%d", out, hits.Load())
	}
	// Secret values through apply need --allow-secrets, as for every kind.
	if out, isErr := on.call(t, "apply", map[string]any{"yaml": "notify:\n  x: {type: ntfy}\n", "secrets": map[string]string{"notify/x.url": "https://ntfy.sh/t"}}); !isErr || !strings.Contains(out, "secret values are not accepted") {
		t.Fatalf("secrets: %s", out)
	}
	if out, isErr := on.call(t, "get", map[string]any{"kind": "notify"}); isErr || !strings.Contains(out, "hook") {
		t.Fatalf("get notify: %s", out)
	}
}
