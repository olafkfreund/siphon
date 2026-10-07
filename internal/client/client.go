// Package client is siphon's HTTP client for its own API: connection
// settings, JSON calls and typed errors with the CLI's exit codes.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Exit codes: 0 ok, 1 error, 2 usage, 3 validation, 4 not found, 5 conflict.
const (
	ExitError    = 1
	ExitUsage    = 2
	ExitInvalid  = 3
	ExitNotFound = 4
	ExitConflict = 5
)

// Error is a refusal or failure with what to do next. It never holds the token.
type Error struct {
	Status int      `json:"-"` // HTTP status; 0 for local problems
	Msg    string   `json:"error"`
	Errors []string `json:"errors"`
	Hint   string   `json:"hint"`
	Code   int      `json:"-"` // exit code override (usage errors)
}

func (e *Error) Error() string { return e.Msg }

// ExitCode maps the error to the CLI's exit code.
func (e *Error) ExitCode() int {
	switch {
	case e.Code != 0:
		return e.Code
	case e.Status == 422:
		return ExitInvalid
	case e.Status == 404:
		return ExitNotFound
	case e.Status == 409:
		return ExitConflict
	}
	return ExitError
}

// Usage is a command-line mistake (exit 2).
func Usage(msg, hint string) *Error { return &Error{Msg: msg, Hint: hint, Code: ExitUsage} }

// Conn is where and how to call the API.
type Conn struct{ URL, Token string }

// Path is the client.yaml location ($XDG_CONFIG_HOME, else ~/.config).
func Path() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "siphon", "client.yaml")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "siphon", "client.yaml")
}

type file struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// Save writes client.yaml (0600, in a 0700 directory).
func Save(c Conn) error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := yaml.Marshal(file{c.URL, c.Token})
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Remove deletes client.yaml; absent is fine.
func Remove() error {
	if err := os.Remove(Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readFile() (f file, found bool, err error) {
	p := Path()
	fi, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return f, true, &Error{Msg: p + " is readable by other users, so its token is not trusted", Hint: "run `chmod 600 " + p + "` (or `siphon logout` and log in again)"}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return f, true, err
	}
	return f, true, yaml.Unmarshal(b, &f)
}

// Resolve picks the connection: flags, then SIPHON_URL / SIPHON_TOKEN_FILE /
// SIPHON_TOKEN, then client.yaml. URL and token are chosen independently.
func Resolve(flagURL, flagTokenFile string) (Conn, error) {
	f, _, err := readFile()
	if err != nil {
		return Conn{}, err
	}
	c := Conn{URL: firstOf(flagURL, os.Getenv("SIPHON_URL"), f.URL)}
	switch {
	case flagTokenFile != "":
		c.Token, err = readToken(flagTokenFile)
	case os.Getenv("SIPHON_TOKEN_FILE") != "":
		c.Token, err = readToken(os.Getenv("SIPHON_TOKEN_FILE"))
	case os.Getenv("SIPHON_TOKEN") != "":
		c.Token = strings.TrimSpace(os.Getenv("SIPHON_TOKEN"))
	default:
		c.Token = f.Token
	}
	if err != nil {
		return Conn{}, err
	}
	if c.URL == "" || c.Token == "" {
		return Conn{}, &Error{Msg: "not logged in", Hint: "run `siphon login <url>`, or set SIPHON_URL and SIPHON_TOKEN_FILE"}
	}
	if c.URL, err = CleanURL(c.URL); err != nil {
		return Conn{}, err
	}
	return c, nil
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", &Error{Msg: "cannot read the token file " + path, Hint: "check the -token-file / SIPHON_TOKEN_FILE path"}
	}
	return strings.TrimSpace(string(b)), nil
}

// CleanURL accepts http(s)://host[:port] and drops a trailing slash. A URL
// may not carry credentials or a query: the token never goes in a URL.
func CleanURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", &Error{Msg: "bad siphon URL: want http(s)://host[:port]", Hint: "for example `siphon login http://127.0.0.1:8080`", Code: ExitUsage}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

var actorBad = regexp.MustCompile(`[^\w.@:-]`)

// Actor is the audit label: cli:<user>.
func Actor() string { return ActorLabel("") }

// ActorLabel is cli:<user><suffix>, kept within the server's 64 characters.
func ActorLabel(suffix string) string {
	u := actorBad.ReplaceAllString(os.Getenv("USER"), "_")
	if u == "" {
		u = "unknown"
	}
	return "cli:" + u[:min(len(u), 60-len(suffix))] + suffix
}

// Client calls the API with the bearer token.
type Client struct {
	Conn
	HTTP  *http.Client
	Label string // audit label; empty means Actor()
}

func New(c Conn) *Client {
	return &Client{Conn: c, HTTP: &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Do sends a JSON request (body nil for none) and decodes the reply into out
// (nil to discard). Non-2xx replies become *Error.
func (c *Client) Do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Siphon-Actor", firstOf(c.Label, Actor()))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is known to the caller; keep the message short
		}
		return &Error{Msg: fmt.Sprintf("cannot reach %s: %v", c.URL, err), Hint: "is siphon running? check SIPHON_URL, or run `siphon login <url>`"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return decodeError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("unexpected reply from the server: %w", err)
		}
	}
	return nil
}

func decodeError(status int, raw []byte) *Error {
	var b struct {
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
	}
	json.Unmarshal(raw, &b)
	e := &Error{Status: status, Msg: b.Error, Errors: b.Errors}
	if e.Msg == "" {
		e.Msg = http.StatusText(status)
	}
	if e.Errors == nil {
		e.Errors = []string{}
	}
	switch status {
	case 401:
		e.Hint = "the token was refused: run `siphon login <url>` with a valid token"
	case 404:
		e.Hint = "check the name with `siphon get <kind>`"
	case 409:
		e.Hint = "it changed on the server: re-read it with `siphon get`, then retry"
	case 422:
		e.Hint = "validation failed: fix the errors, then re-run with --dry-run to check"
	case 429:
		e.Hint = "too many requests: wait a minute and retry"
	case 503:
		e.Hint = "the server has no editable config file (editing is off)"
	default:
		if status >= 500 {
			e.Hint = "the server failed: check its log"
		}
	}
	return e
}
