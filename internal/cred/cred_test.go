package cred

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func claudeJSON(tok string, exp int64) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"r-%s","expiresAt":%d},"mcpOAuth":{"x":{"accessToken":"MCP-SECRET"}}}`, tok, tok, exp))
}

func TestValidateShapes(t *testing.T) {
	name, b, err := Validate("claude", ImportFile, claudeJSON("a1", 1000))
	if err != nil || name != "credentials.json" || strings.Contains(string(b), "MCP-SECRET") || strings.Contains(string(b), "mcpOAuth") || !strings.Contains(string(b), "r-a1") {
		t.Fatalf("claude: %s %s %v", name, b, err)
	}
	if name, b, err := Validate("claude", ImportToken, []byte(" sk-ant-oat \n")); err != nil || name != "oauth-token" || string(b) != "sk-ant-oat" {
		t.Fatalf("token: %s %q %v", name, b, err)
	}
	for _, c := range []struct {
		prov string
		kind ImportKind
		in   string
	}{
		{"claude", ImportFile, `{"claudeAiOauth":{"accessToken":"a"}}`},
		{"claude", ImportFile, `{"other":1}`},
		{"claude", ImportFile, `not json`},
		{"claude", ImportToken, "  \n"},
		{"codex", ImportToken, "tok"},
		{"codex", ImportFile, `{"tokens":{"access_token":"a"}}`},
		{"codex", ImportFile, `{"OPENAI_API_KEY":null}`},
		{"agy", ImportFile, `{"token":{"access_token":"a"}}`},
		{"gpt", ImportFile, `{}`},
	} {
		if _, _, err := Validate(c.prov, c.kind, []byte(c.in)); err == nil {
			t.Errorf("want error for %s %q", c.prov, c.in)
		}
	}
	for prov, in := range map[string]string{
		"codex": `{"tokens":{"access_token":"a","refresh_token":"r"}}`,
		"agy":   `{"token":{"access_token":"a","refresh_token":"r"}}`,
	} {
		if _, _, err := Validate(prov, ImportFile, []byte(in)); err != nil {
			t.Errorf("%s: %v", prov, err)
		}
	}
	if _, _, err := Validate("codex", ImportFile, []byte(`{"OPENAI_API_KEY":"sk-1"}`)); err != nil {
		t.Errorf("codex api key: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	if e := Expiry("claude", "credentials.json", claudeJSON("a", 1700000000000)); e.UnixMilli() != 1700000000000 {
		t.Fatalf("claude %v", e)
	}
	jwt := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1800000000}`)) + ".s"
	if e := Expiry("codex", "auth.json", []byte(`{"tokens":{"access_token":"`+jwt+`"}}`)); e.Unix() != 1800000000 {
		t.Fatalf("codex %v", e)
	}
	if e := Expiry("agy", "antigravity-oauth-token", []byte(`{"token":{"expiry":"2030-01-02T03:04:05Z"}}`)); e.Year() != 2030 {
		t.Fatalf("agy %v", e)
	}
	if !Expiry("codex", "auth.json", []byte(`junk`)).IsZero() || !Expiry("claude", "oauth-token", []byte("tok")).IsZero() {
		t.Fatal("unknown must be zero")
	}
}

func TestSaveCAS(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "credentials")}
	v1, v2, v3 := claudeJSON("a1", 1000), claudeJSON("a2", 2000), claudeJSON("a3", 3000)
	if err := s.Put("c", "credentials.json", v1); err != nil {
		t.Fatal(err)
	}
	get := func() string { f, _ := s.Load("c"); return string(f["credentials.json"]) }
	// still at starting bytes: swap
	if w, err := s.Save("c", "credentials.json", v1, v2); err != nil || !w || get() != string(v2) {
		t.Fatalf("cas swap: %v", err)
	}
	// stale writer (old=v1) with older expiry than stored v2: keep v2
	if w, err := s.Save("c", "credentials.json", v1, claudeJSON("a0", 500)); err != nil || w || get() != string(v2) {
		t.Fatalf("older must lose: %v", err)
	}
	// stale writer with later expiry: wins
	if w, err := s.Save("c", "credentials.json", v1, v3); err != nil || !w || get() != string(v3) {
		t.Fatalf("newer must win: %v", err)
	}
	prev, _ := os.ReadFile(filepath.Join(s.Dir, "c", "credentials.json.prev"))
	if string(prev) != string(v2) {
		t.Fatalf(".prev = %s", prev)
	}
	if fi, _ := os.Stat(filepath.Join(s.Dir, "c", "credentials.json.prev")); fi.Mode().Perm() != 0o600 {
		t.Fatal(".prev mode")
	}
	if _, err := s.Save("c", "../evil", nil, v1); err == nil {
		t.Fatal("bad file name accepted")
	}
}

func TestModesAndPutReplacesVariant(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "credentials")}
	s.Put("c", "credentials.json", claudeJSON("a", 1))
	s.Put("c", "oauth-token", []byte("tok"))
	f, _ := s.Load("c")
	if len(f) != 1 || string(f["oauth-token"]) != "tok" {
		t.Fatalf("variant not replaced: %v", f)
	}
	for p, want := range map[string]os.FileMode{s.Dir: 0o700, filepath.Join(s.Dir, "c"): 0o700, filepath.Join(s.Dir, "c", "oauth-token"): 0o600, filepath.Join(s.Dir, "c", ".lock"): 0o600} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v", p, fi, err)
		}
	}
	if _, err := s.Load("missing"); err == nil {
		t.Fatal("load of unimported credential must fail")
	}
	if _, err := s.Load("../x"); err == nil {
		t.Fatal("path traversal in name")
	}
}

func TestAcquireSerialises(t *testing.T) {
	var cur, peak atomic.Int32
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			rel, err := Acquire(context.Background(), "sem1", 1)
			if err != nil {
				t.Error(err)
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			cur.Add(-1)
			rel()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if peak.Load() != 1 {
		t.Fatalf("peak concurrency %d", peak.Load())
	}
	rel, _ := Acquire(context.Background(), "sem2", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, "sem2", 1); err == nil {
		t.Fatal("second acquire should wait for ctx")
	}
	rel()
	rel() // idempotent
}

func TestNormalizeDropsUnknownFields(t *testing.T) {
	_, b, _ := Validate("codex", ImportFile, []byte(`{"tokens":{"access_token":"a","refresh_token":"r","account_id":"acc","evil":"E1"},"extra":"E2"}`))
	_, g, _ := Validate("agy", ImportFile, []byte(`{"token":{"access_token":"a","refresh_token":"r","evil":"E3"},"extra":"E4"}`))
	_, c, _ := Validate("claude", ImportFile, []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","scopes":["x"],"evil":"E5"}}`))
	for _, x := range []string{"E1", "E2", "E3", "E4", "E5"} {
		if strings.Contains(string(b)+string(g)+string(c), x) {
			t.Fatalf("%s survived normalisation", x)
		}
	}
	if !strings.Contains(string(b), `"account_id":"acc"`) || !strings.Contains(string(c), `"scopes":["x"]`) {
		t.Fatalf("known fields lost: %s %s", b, c)
	}
}

func TestValidateForSubscription(t *testing.T) {
	key := []byte(`{"OPENAI_API_KEY":"sk-1"}`)
	if _, _, err := ValidateFor("codex", true, key); err == nil {
		t.Fatal("api-key-only auth.json accepted for subscription")
	}
	if _, _, err := ValidateFor("codex", false, key); err != nil {
		t.Fatal(err)
	}
}

func TestSameAccount(t *testing.T) {
	jwt := func(sub string) string {
		return "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"`+sub+`"}`)) + ".s"
	}
	mk := func(acc, sub string) []byte {
		return []byte(`{"tokens":{"access_token":"a","refresh_token":"r","account_id":"` + acc + `","id_token":"` + jwt(sub) + `"}}`)
	}
	for _, c := range []struct {
		o, n []byte
		want bool
	}{
		{mk("A", "u1"), mk("A", "u1"), true},
		{mk("A", "u1"), mk("B", "u1"), false},
		{mk("", "u1"), mk("", "u1"), true},
		{mk("", "u1"), mk("", "u2"), false},
		{mk("A", "u1"), mk("", "u1"), false},
		{mk("A", "u1"), []byte("junk"), false},
	} {
		if got := SameAccount("codex", c.o, c.n); got != c.want {
			t.Errorf("%s vs %s: %v", c.o, c.n, got)
		}
	}
	if !SameAccount("claude", []byte("a"), []byte("b")) {
		t.Fatal("claude has no identity: must be true")
	}
}
