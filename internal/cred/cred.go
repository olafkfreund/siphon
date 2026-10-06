// Package cred is the on-disk store for subscription logins (Claude Code,
// Codex, Antigravity). It never logs or prints token values.
package cred

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
)

// Store keeps each credential in <Dir>/<name>/, one file per login file.
type Store struct{ Dir string }

// StoreFor places the store next to the database, inside the state directory.
func StoreFor(cfg *config.Config) Store {
	return Store{Dir: filepath.Join(filepath.Dir(cfg.Server.DB), "credentials")}
}

// ImportKind says how Validate should read its input.
type ImportKind int

const (
	ImportFile  ImportKind = iota // the CLI's own login file
	ImportToken                   // a bare token (claude setup-token)
)

// provider of each store file; also the allowlist of names Save accepts.
var fileProvider = map[string]string{
	"credentials.json":        "claude",
	"oauth-token":             "claude",
	"auth.json":               "codex",
	"antigravity-oauth-token": "agy",
}

const maxImport = 1 << 20

// ReadLimited reads r, failing if it exceeds the import size cap.
func ReadLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxImport+1))
	if err == nil && len(b) > maxImport {
		err = errors.New("input too large")
	}
	return b, err
}

// Validate checks a login's shape and returns the store file name and the
// bytes to keep. Claude keeps only claudeAiOauth (MCP OAuth entries are dropped).
func Validate(provider string, kind ImportKind, raw []byte) (name string, normalized []byte, err error) {
	if kind == ImportToken {
		t := bytes.TrimSpace(raw)
		if provider != "claude" || len(t) == 0 {
			return "", nil, errors.New("--token-stdin needs a claude credential and a non-empty token")
		}
		return "oauth-token", t, nil
	}
	switch provider {
	case "claude":
		var top map[string]json.RawMessage
		if json.Unmarshal(raw, &top) != nil || top["claudeAiOauth"] == nil {
			return "", nil, errors.New("claude: want a JSON file with a claudeAiOauth object")
		}
		var o struct{ AccessToken, RefreshToken string }
		if json.Unmarshal(top["claudeAiOauth"], &o) != nil || o.AccessToken == "" || o.RefreshToken == "" {
			return "", nil, errors.New("claude: claudeAiOauth needs accessToken and refreshToken")
		}
		b, _ := json.Marshal(map[string]json.RawMessage{"claudeAiOauth": top["claudeAiOauth"]})
		return "credentials.json", b, nil
	case "codex":
		var a codexAuth
		if json.Unmarshal(raw, &a) != nil || !((a.Tokens.Access != "" && a.Tokens.Refresh != "") || (a.APIKey != nil && *a.APIKey != "")) {
			return "", nil, errors.New("codex: auth.json needs tokens.access_token and refresh_token, or OPENAI_API_KEY")
		}
		return "auth.json", raw, nil
	case "agy":
		var a agyToken
		if json.Unmarshal(raw, &a) != nil || a.Token.Access == "" || a.Token.Refresh == "" {
			return "", nil, errors.New("agy: want token.access_token and token.refresh_token")
		}
		return "antigravity-oauth-token", raw, nil
	}
	return "", nil, fmt.Errorf("unknown provider %q", provider)
}

type codexAuth struct {
	Tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	} `json:"tokens"`
	APIKey *string `json:"OPENAI_API_KEY"`
}

type agyToken struct {
	Token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expiry  string `json:"expiry"`
	} `json:"token"`
}

// Expiry returns the access-token expiry of file's contents, or zero if unknown.
func Expiry(provider, file string, b []byte) time.Time {
	switch provider {
	case "claude":
		var c struct {
			O struct {
				ExpiresAt int64 `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(b, &c) == nil && c.O.ExpiresAt > 0 {
			return time.UnixMilli(c.O.ExpiresAt)
		}
	case "codex":
		var a codexAuth
		if json.Unmarshal(b, &a) != nil {
			return time.Time{}
		}
		parts := strings.Split(a.Tokens.Access, ".")
		if len(parts) != 3 {
			return time.Time{}
		}
		p, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		var c struct {
			Exp float64 `json:"exp"`
		}
		if err == nil && json.Unmarshal(p, &c) == nil && c.Exp > 0 {
			return time.Unix(int64(c.Exp), 0)
		}
	case "agy":
		var a agyToken
		if json.Unmarshal(b, &a) == nil {
			if t, err := time.Parse(time.RFC3339, a.Token.Expiry); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

func (s Store) dir(name string) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("bad credential name %q", name)
	}
	return filepath.Join(s.Dir, name), nil
}

// Load returns every login file of credential name, keyed by file name.
func (s Store) Load(name string) (map[string][]byte, error) {
	d, err := s.dir(name)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for f := range fileProvider {
		b, err := os.ReadFile(filepath.Join(d, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		out[f] = b
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("credential %q has no imported login (agentgw credentials import)", name)
	}
	return out, nil
}

// lock runs fn holding the credential's flock, creating its 0700 dir.
func (s Store) lock(name string, fn func(dir string) error) error {
	d, err := s.dir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	l, err := os.OpenFile(filepath.Join(d, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
	return fn(d)
}

func writeAtomic(dir, file string, b []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*") // 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, file))
}

// Put stores file unconditionally (import) and drops the provider's other login files.
func (s Store) Put(name, file string, b []byte) error {
	prov, ok := fileProvider[file]
	if !ok {
		return fmt.Errorf("unknown credential file %q", file)
	}
	return s.lock(name, func(d string) error {
		for f, p := range fileProvider {
			if p == prov && f != file {
				os.Remove(filepath.Join(d, f))
			}
		}
		return writeAtomic(d, file, b)
	})
}

// Save writes back a refreshed login file. If the store still holds old it
// replaces it with new; otherwise someone else wrote meanwhile and the copy
// with the later token expiry wins (ties keep the stored one).
func (s Store) Save(name, file string, old, new []byte) error {
	prov, ok := fileProvider[file]
	if !ok {
		return fmt.Errorf("unknown credential file %q", file)
	}
	return s.lock(name, func(d string) error {
		cur, err := os.ReadFile(filepath.Join(d, file))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !bytes.Equal(cur, old) && !Expiry(prov, file, new).After(Expiry(prov, file, cur)) {
			return nil
		}
		return writeAtomic(d, file, new)
	})
}

// Info reports the latest token expiry and file mtime across a credential's files.
func (s Store) Info(name string) (expiry, written time.Time, err error) {
	files, err := s.Load(name)
	if err != nil {
		return
	}
	d, _ := s.dir(name)
	for f, b := range files {
		if e := Expiry(fileProvider[f], f, b); e.After(expiry) {
			expiry = e
		}
		if fi, err := os.Stat(filepath.Join(d, f)); err == nil && fi.ModTime().After(written) {
			written = fi.ModTime()
		}
	}
	return
}

var (
	semMu sync.Mutex
	sems  = map[string]chan struct{}{}
)

// Acquire takes one of n slots for credential name, waiting for ctx. The
// first call for a name fixes its size (restart to change concurrency).
func Acquire(ctx context.Context, name string, n int) (release func(), err error) {
	semMu.Lock()
	ch, ok := sems[name]
	if !ok {
		ch = make(chan struct{}, max(n, 1))
		sems[name] = ch
	}
	semMu.Unlock()
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
