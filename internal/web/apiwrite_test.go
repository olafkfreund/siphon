package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/store"
)

const apiRule = `{"yaml":"source: gh\nwhen: \"true\"\naction: {cmd: [echo]}\n"}`

func (ce *cfgEnv) apiAs(method, path, body, actor string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("X-Siphon-Actor", actor)
	w := httptest.NewRecorder()
	ce.h.ServeHTTP(w, r)
	return w
}

func (ce *cfgEnv) unchanged(t *testing.T, revs int64, applied int32) {
	t.Helper()
	if items, _ := store.ConfigItems(ce.st.DB); ce.latest() != revs || ce.applied.Load() != applied || len(items) != 0 {
		t.Fatalf("dry run changed state: rev %d applied %d items %d", ce.latest(), ce.applied.Load(), len(items))
	}
}

func TestAPIDryRun(t *testing.T) {
	ce := newCfgEnv(t)
	revs, applied := ce.latest(), ce.applied.Load()
	w := ce.api("PUT", "/api/config/rules/dry?dry_run=1", apiRule)
	var got struct {
		Diff     string
		Errors   []string
		Warnings []string
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || !strings.Contains(got.Diff, "+") || got.Errors == nil || got.Warnings == nil {
		t.Fatalf("dry put: %d %s", w.Code, w.Body.String())
	}
	ce.unchanged(t, revs, applied)
	for _, c := range [][2]string{{"DELETE", "/api/config/rules/r1?dry_run=1"}, {"POST", "/api/config/rules/r1/reset?dry_run=1"}} {
		if w := ce.api(c[0], c[1], ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"diff"`) {
			t.Fatalf("%v: %d %s", c, w.Code, w.Body.String())
		}
	}
	ce.unchanged(t, revs, applied)
	w = ce.api("PUT", "/api/config/rules/bad?dry_run=1", `{"yaml":"source: nope\nwhen: \"true\"\naction: {cmd: [x]}\n"}`)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "unknown source") {
		t.Fatalf("dry invalid: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("PUT", "/api/config/rules/dry?dry_run=1", `{"rev":99,"yaml":"a: 1"}`); w.Code != 409 {
		t.Fatalf("dry stale: %d", w.Code)
	}
	ce.unchanged(t, revs, applied)
}

func TestAPIInvalidListsEveryError(t *testing.T) {
	ce := newCfgEnv(t)
	w := ce.api("PUT", "/api/config/rules/bad", `{"yaml":"source: nope\nwhen: \"(\"\naction: {cmd: []}\n"}`)
	var got struct {
		Error    string
		Errors   []string
		Warnings []string
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 422 || len(got.Errors) < 2 || got.Warnings == nil || strings.Join(got.Errors, "\n") != got.Error {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestAPISecrets(t *testing.T) {
	ce := newCfgEnv(t)
	const val = "whsec-VERY-SECRET"
	put := func(path, y string, sec map[string]string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"yaml": y, "secrets": sec})
		return ce.api("PUT", path, string(b))
	}
	y := "type: webhook\nsignature: github\n"
	f := filepath.Join(ce.dir, "secrets", "sources-hk-secret")
	if w := put("/api/config/sources/hk?dry_run=1", y, map[string]string{"secret": val}); w.Code != 200 || strings.Contains(w.Body.String(), val) {
		t.Fatalf("dry: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(f); err == nil {
		t.Fatal("dry run wrote the secret")
	}
	w := put("/api/config/sources/hk", y, map[string]string{"secret": val})
	if w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	fi, err := os.Stat(f)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(f); string(b) != val {
		t.Fatalf("secret content %q", b)
	}
	all := w.Body.String()
	for _, p := range []string{"/api/config/sources/hk", "/api/config/sources", "/api/config/export", "/api/config/history", "/api/config/history/1", "/api/audit"} {
		all += ce.api("GET", p, "").Body.String()
	}
	if strings.Contains(all, val) || !strings.Contains(all, "file:") {
		t.Fatalf("secret leaked or ref missing:\n%s", all)
	}
	revs := ce.latest()
	for _, k := range []string{"nope", "env.", "env.A.B", "type", "../x"} {
		if w := put("/api/config/sources/hk2", y, map[string]string{k: val}); w.Code != 422 || strings.Contains(w.Body.String(), val) {
			t.Fatalf("path %q: %d %s", k, w.Code, w.Body.String())
		}
	}
	if w := put("/api/config/sources/hk2", y, map[string]string{"secret": ""}); w.Code != 422 {
		t.Fatalf("empty: %d", w.Code)
	}
	if ce.latest() != revs {
		t.Fatal("a refused secret changed state")
	}
	// A nested path is set under its parent.
	if w := put("/api/config/sources/web", "type: http\nurl: https://example.com\npoll: 1m\n", map[string]string{"auth.bearer": val, "headers.X-Key": val}); w.Code != 200 {
		t.Fatalf("nested: %d %s", w.Code, w.Body.String())
	}
}

func TestAPIApplyError(t *testing.T) {
	ce := newCfgEnv(t)
	ce.applyFail.Store(true)
	w := ce.api("PUT", "/api/config/rules/ae", apiRule)
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got["applied"] != false || got["apply_error"] != "apply boom" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestAPIActor(t *testing.T) {
	ce := newCfgEnv(t)
	if w := ce.apiAs("PUT", "/api/config/rules/a1", apiRule, "ci@build:1"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := ce.apiAs("PUT", "/api/config/rules/a2", apiRule, "bad label!"); w.Code != 400 {
		t.Fatalf("bad label: %d", w.Code)
	}
	if w := ce.apiAs("POST", "/api/config/history/1/restore", "", "ci"); w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("DELETE", "/api/config/rules/a1", ""); w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	revs, _ := store.Revisions(ce.st.DB, 10)
	var actors []string
	for _, r := range revs {
		actors = append(actors, r.Actor)
	}
	if strings.Join(actors, ",") != "api,api:ci,api:ci@build:1" { // newest first
		t.Fatalf("actors %v", actors)
	}
	// The portal's restore keeps its own actor.
	if w := ce.post("/history/1/restore", map[string][]string{"rev": {itoa(int(ce.latest()))}}); w.Code != 303 {
		t.Fatalf("portal restore: %d", w.Code)
	}
	if revs, _ := store.Revisions(ce.st.DB, 1); revs[0].Actor != "portal" {
		t.Fatalf("portal restore actor %q", revs[0].Actor)
	}
}

func TestAPIJSONErrors(t *testing.T) {
	ce := newCfgEnv(t)
	check := func(code int, w *httptest.ResponseRecorder) {
		t.Helper()
		var b map[string]string
		if w.Code != code || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &b) != nil || b["error"] == "" {
			t.Fatalf("want JSON %d, got %d %q %s", code, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	check(404, ce.api("GET", "/api/nope", ""))
	check(404, ce.api("GET", "/api/config/rules/missing", ""))
	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("GET", "/api/rules", nil)
		r.RemoteAddr = "192.0.2.9:1"
		r.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		ce.h.ServeHTTP(w, r)
		if i == 0 {
			check(http.StatusUnauthorized, w)
		}
		if w.Code == 429 {
			check(429, w)
			return
		}
	}
	t.Fatal("never rate limited")
}

func TestAPIListSortedWithTombstones(t *testing.T) {
	ce := newCfgEnv(t)
	for _, n := range []string{"zed", "alpha"} {
		if w := ce.api("PUT", "/api/config/rules/"+n, apiRule); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if w := ce.api("DELETE", "/api/config/rules/r1", ""); w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	var list []apiItem
	json.Unmarshal(ce.api("GET", "/api/config/rules", "").Body.Bytes(), &list)
	if len(list) != 3 || list[0].Name != "alpha" || list[1].Name != "r1" || !list[1].Deleted || list[2].Name != "zed" || list[0].Deleted {
		t.Fatalf("%+v", list)
	}
}
