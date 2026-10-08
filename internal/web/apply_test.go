package web

import (
	"encoding/json"
	"github.com/olafkfreund/siphon/internal/config"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/store"
)

const ruleY = "source: gh\\nwhen: \\\"true\\\"\\naction: {cmd: [echo]}\\n"

func TestDryRunChecksPrivateModelEndpoint(t *testing.T) {
	ce := newCfgEnv(t)
	const b = `{"yaml":"provider: ollama\nurl: http://127.0.0.1:9\n"}`
	d := ce.api("PUT", "/api/config/credentials/priv?dry_run=1", b)
	w := ce.api("PUT", "/api/config/credentials/priv", b)
	if d.Code != 422 || w.Code != 422 || d.Body.String() != w.Body.String() || !strings.Contains(w.Body.String(), "private_endpoints") {
		t.Fatalf("dry %d %s\nreal %d %s", d.Code, d.Body.String(), w.Code, w.Body.String())
	}
}

func TestApplyBatch(t *testing.T) {
	ce := newCfgEnv(t)
	revs := ce.latest()
	body := `{"items":[{"kind":"rules","name":"b1","yaml":"` + ruleY + `"},{"kind":"rules","name":"b2","yaml":"` + ruleY + `"},` +
		`{"kind":"sources","name":"hk","yaml":"type: webhook\nsignature: github\n"}],"delete":[{"kind":"rules","name":"r1"}],` +
		`"secrets":{"sources/hk.secret":"batch-SECRET"}}`
	w := ce.apiAs("POST", "/api/config/apply", body, "ci")
	if w.Code != 200 || ce.latest() != revs+1 || strings.Contains(w.Body.String(), "batch-SECRET") {
		t.Fatalf("%d %s (rev %d -> %d)", w.Code, w.Body.String(), revs, ce.latest())
	}
	if got := strings.Join(ce.rules(), ","); got != "b1,b2" {
		t.Fatalf("rules %s", got)
	}
	if fi, err := os.Stat(filepath.Join(ce.dir, "secrets", "sources--hk+secret")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v", err)
	}
	if r, _ := store.Revisions(ce.st.DB, 1); r[0].Actor != "api:ci" {
		t.Fatalf("actor %q", r[0].Actor)
	}
}

func TestApplyBatchAllOrNothing(t *testing.T) {
	ce := newCfgEnv(t)
	revs, applied := ce.latest(), ce.applied.Load()
	body := `{"items":[{"kind":"rules","name":"ok","yaml":"` + ruleY + `"},` +
		`{"kind":"rules","name":"bad1","yaml":"source: nope\nwhen: \"true\"\naction: {cmd: [x]}\n"},` +
		`{"kind":"rules","name":"bad2","yaml":"source: nada\nwhen: \"true\"\naction: {cmd: [x]}\n"}]}`
	w := ce.api("POST", "/api/config/apply", body)
	var got struct{ Errors []string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 422 || len(got.Errors) != 2 || !strings.HasPrefix(got.Errors[0], "rules/bad") || !strings.HasPrefix(got.Errors[1], "rules/bad") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	ce.unchanged(t, revs, applied)
	// A secret for an item that is not in the request, and a bad secret path.
	w = ce.api("POST", "/api/config/apply", `{"items":[{"kind":"rules","name":"ok","yaml":"`+ruleY+`"}],"secrets":{"sources/ghost.secret":"x","rules/ok.nope":"y"}}`)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "sources/ghost.secret") || !strings.Contains(w.Body.String(), "rules/ok: ") || strings.Contains(w.Body.String(), `"y"`) {
		t.Fatalf("stray secret: %d %s", w.Code, w.Body.String())
	}
	ce.unchanged(t, revs, applied)
	for _, b := range []string{`{}`, `{"items":[{"kind":"server","name":"x","yaml":"a: 1"}]}`,
		`{"items":[{"kind":"rules","name":"d","yaml":"a: 1"}],"delete":[{"kind":"rules","name":"d"}]}`} {
		if w := ce.api("POST", "/api/config/apply", b); w.Code != 400 && w.Code != 422 {
			t.Fatalf("%s: %d", b, w.Code)
		}
	}
	ce.unchanged(t, revs, applied)
}

func TestApplyBatchDryRunAndStale(t *testing.T) {
	ce := newCfgEnv(t)
	revs, applied := ce.latest(), ce.applied.Load()
	body := `{"items":[{"kind":"rules","name":"b1","yaml":"` + ruleY + `"}],"delete":[{"kind":"rules","name":"r1"}]}`
	w := ce.api("POST", "/api/config/apply?dry_run=1", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"diff"`) || !strings.Contains(w.Body.String(), `"errors":[]`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	ce.unchanged(t, revs, applied)
	stale := `{"items":[{"kind":"rules","name":"b1","rev":7,"yaml":"` + ruleY + `"}]}`
	if w := ce.api("POST", "/api/config/apply", stale); w.Code != 409 {
		t.Fatalf("stale: %d %s", w.Code, w.Body.String())
	}
	mixed := `{"items":[{"kind":"rules","name":"b1","rev":0,"yaml":"` + ruleY + `"},{"kind":"rules","name":"b2","rev":1,"yaml":"` + ruleY + `"}]}`
	if w := ce.api("POST", "/api/config/apply", mixed); w.Code != 409 {
		t.Fatalf("mixed revs: %d", w.Code)
	}
	ce.unchanged(t, revs, applied)
	if w := ce.api("POST", "/api/config/apply", `{"items":[{"kind":"rules","name":"b1","rev":0,"yaml":"`+ruleY+`"}]}`); w.Code != 200 {
		t.Fatalf("current rev: %d %s", w.Code, w.Body.String())
	}
}

// A batch delete gets the same stale check as an item, and a secret key goes to
// the longest matching item name.
func TestApplyBatchDeleteRevAndLongestSecretPrefix(t *testing.T) {
	ce := newCfgEnv(t)
	old := ce.latest()
	if w := ce.api("PUT", "/api/config/rules/bump", `{"yaml":"`+ruleY+`"}`); w.Code != 200 {
		t.Fatalf("bump: %d %s", w.Code, w.Body.String())
	}
	revs := ce.latest()
	w := ce.api("POST", "/api/config/apply", `{"delete":[{"kind":"rules","name":"r1","rev":`+strconv.FormatInt(old, 10)+`}]}`)
	if w.Code != 409 || ce.latest() != revs {
		t.Fatalf("stale delete: %d %s", w.Code, w.Body.String())
	}
	body := `{"items":[{"kind":"sources","name":"a","yaml":"type: http\nurl: https://example.com/\npoll: 1m\n"},` +
		`{"kind":"sources","name":"a-b","yaml":"type: webhook\nsignature: github\n"}],"secrets":{"sources/a-b.secret":"v"}}`
	if w := ce.api("POST", "/api/config/apply", body); w.Code != 200 {
		t.Fatalf("longest prefix: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(ce.dir, "secrets", config.SecretFileName("sources", "a-b", "secret"))); err != nil {
		t.Fatal(err)
	}
}
