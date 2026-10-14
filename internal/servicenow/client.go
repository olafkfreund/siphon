// Package servicenow is a small ServiceNow REST client: OAuth client
// credentials or an API key, strict input checks, and errors that never carry
// a secret. The MCP server (mcp.go) and the connection test share it.
package servicenow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable means a 2xx that wasn't JSON: a hibernating or SSO-intercepted instance.
var ErrUnavailable = errors.New("instance returned a non-JSON response (hibernating or behind SSO?)")

const (
	maxBody     = 4 << 20
	maxRetry    = 30 * time.Second
	tokenMargin = 60 * time.Second
)

var (
	tableRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`)
	sysIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	fieldRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Auth is an API key, or an OAuth client ID and secret. The API key wins when set.
type Auth struct{ ClientID, ClientSecret, APIKey string }

type Client struct {
	base  string
	auth  Auth
	hc    *http.Client
	now   func() time.Time
	sleep func(context.Context, time.Duration) error // injectable for tests

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New checks base (https, or http on loopback for tests; no path, query or user info) and auth.
// A 3xx is an error: New copies hc and stops it following redirects.
func New(base string, auth Auth, hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("servicenow: instance URL must be scheme://host with no path, query or user info")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return nil, errors.New("servicenow: instance URL must be https")
	}
	if auth.APIKey == "" && (auth.ClientID == "" || auth.ClientSecret == "") {
		return nil, errors.New("servicenow: need an API key, or both a client ID and a client secret")
	}
	// Copy so the caller's client is untouched. Go would forward x-sn-apikey on a redirect.
	h := *hc
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u.Scheme + "://" + u.Host, auth: auth, hc: &h, now: time.Now, sleep: ctxSleep}, nil
}

// ClientFromEnv builds a Client from the SERVICENOW_* variables.
func ClientFromEnv(getenv func(string) string, hc *http.Client) (*Client, error) {
	inst := getenv("SERVICENOW_INSTANCE_URL")
	if inst == "" {
		return nil, errors.New("servicenow: SERVICENOW_INSTANCE_URL is not set")
	}
	return New(inst, Auth{
		ClientID:     getenv("SERVICENOW_CLIENT_ID"),
		ClientSecret: getenv("SERVICENOW_CLIENT_SECRET"),
		APIKey:       getenv("SERVICENOW_API_KEY"),
	}, hc)
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func checkTable(t string) error {
	if !tableRe.MatchString(t) {
		return fmt.Errorf("servicenow: invalid table name %q", t)
	}
	return nil
}

func checkSysID(id string) error {
	if !sysIDRe.MatchString(id) {
		return errors.New("servicenow: sys_id must be 32 lowercase hex characters")
	}
	return nil
}

func checkField(f string) error {
	if !fieldRe.MatchString(f) {
		return fmt.Errorf("servicenow: invalid field name %q", f)
	}
	return nil
}

func checkFields(fields []string) error {
	for _, f := range fields {
		if err := checkField(f); err != nil {
			return err
		}
	}
	return nil
}

// checkBody also bans sys_* names: writes must not touch system fields. Reads may name them.
func checkBody(body map[string]any) error {
	for k := range body {
		if err := checkField(k); err != nil || strings.HasPrefix(k, "sys_") {
			return fmt.Errorf("servicenow: invalid field name %q", k)
		}
	}
	return nil
}

func readParams(fields []string) url.Values {
	q := url.Values{"sysparm_display_value": {"true"}, "sysparm_exclude_reference_link": {"true"}}
	if len(fields) > 0 {
		q.Set("sysparm_fields", strings.Join(fields, ","))
	}
	return q
}

// Get reads one record. fields may be nil for all of them.
func (c *Client) Get(ctx context.Context, table, sysID string, fields []string) (map[string]any, error) {
	if err := errors.Join(checkTable(table), checkSysID(sysID), checkFields(fields)); err != nil {
		return nil, err
	}
	var out map[string]any
	return out, c.call(ctx, http.MethodGet, "/api/now/table/"+table+"/"+sysID, readParams(fields), nil, &out)
}

// Query lists records matching an encoded query. limit is clamped to 1-50.
func (c *Client) Query(ctx context.Context, table, query string, fields []string, limit int) ([]map[string]any, error) {
	if err := errors.Join(checkTable(table), checkFields(fields)); err != nil {
		return nil, err
	}
	q := readParams(fields)
	q.Set("sysparm_limit", strconv.Itoa(min(max(limit, 1), 50)))
	if query != "" {
		q.Set("sysparm_query", query)
	}
	var out []map[string]any
	return out, c.call(ctx, http.MethodGet, "/api/now/table/"+table, q, nil, &out)
}

func (c *Client) Create(ctx context.Context, table string, fields map[string]any) (map[string]any, error) {
	if err := errors.Join(checkTable(table), checkBody(fields)); err != nil {
		return nil, err
	}
	var out map[string]any
	return out, c.call(ctx, http.MethodPost, "/api/now/table/"+table, nil, fields, &out)
}

func (c *Client) Patch(ctx context.Context, table, sysID string, fields map[string]any) (map[string]any, error) {
	if err := errors.Join(checkTable(table), checkSysID(sysID), checkBody(fields)); err != nil {
		return nil, err
	}
	var out map[string]any
	return out, c.call(ctx, http.MethodPatch, "/api/now/table/"+table+"/"+sysID, nil, fields, &out)
}

// CreateChange posts to the change API. kind is normal, emergency or standard;
// standard needs fields["standard_change_template_id"].
func (c *Client) CreateChange(ctx context.Context, kind string, fields map[string]any) (map[string]any, error) {
	if err := checkBody(fields); err != nil {
		return nil, err
	}
	path := "/api/sn_chg_rest/change/" + kind
	switch kind {
	case "normal", "emergency":
	case "standard":
		id, _ := fields["standard_change_template_id"].(string)
		if err := checkSysID(id); err != nil {
			return nil, fmt.Errorf("servicenow: standard_change_template_id: %w", err)
		}
		path += "/" + id
	default:
		return nil, fmt.Errorf("servicenow: change kind must be normal, emergency or standard, not %q", kind)
	}
	var out map[string]any
	return out, c.call(ctx, http.MethodPost, path, nil, fields, &out)
}

// Whoami returns the user name the credentials act as.
func (c *Client) Whoami(ctx context.Context) (string, error) {
	q := url.Values{"sysparm_query": {"sys_id=javascript:gs.getUserID()"}, "sysparm_fields": {"user_name"}, "sysparm_limit": {"1"}}
	var out []map[string]any
	if err := c.call(ctx, http.MethodGet, "/api/now/table/sys_user", q, nil, &out); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", errors.New("servicenow: the credentials map to no sys_user")
	}
	name, _ := out[0]["user_name"].(string)
	return name, nil
}

// call sends one API request and decodes the "result" member into out.
// It retries once after a 401 (fresh token) and once after a 429.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("servicenow: %s %s: %w", method, path, err)
		}
	}
	target := c.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	fail := func(err error) error { return fmt.Errorf("servicenow: %s %s: %w", method, path, err) }
	var refreshed, waited bool
	for {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return fail(err)
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.auth.APIKey != "" {
			req.Header.Set("x-sn-apikey", c.auth.APIKey)
		} else {
			tok, err := c.bearer(ctx)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		status, header, data, err := c.send(req)
		if err != nil {
			return fail(err)
		}
		switch {
		case status == http.StatusUnauthorized && c.auth.APIKey == "" && !refreshed:
			refreshed = true
			c.dropToken()
			continue
		case status == http.StatusTooManyRequests && !waited:
			waited = true
			wait := 1 * time.Second
			if s, err := strconv.Atoi(header.Get("Retry-After")); err == nil && s >= 0 {
				wait = time.Duration(s) * time.Second
			}
			if err := c.sleep(ctx, min(wait, maxRetry)); err != nil {
				return fail(err)
			}
			continue
		case status < 200 || status > 299:
			return fmt.Errorf("servicenow: %s %s: %d %s", method, path, status, errMessage(status, data))
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(data, &env) != nil {
			return fail(ErrUnavailable)
		}
		if out == nil || len(env.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fail(fmt.Errorf("unexpected result shape: %w", err))
		}
		return nil
	}
}

// send does one round trip and returns the capped body.
func (c *Client) send(req *http.Request) (int, http.Header, []byte, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		// url.Error repeats the URL, which carries no secret but is noise.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(data) > maxBody {
		return 0, nil, nil, errors.New("response body over 4 MiB")
	}
	return resp.StatusCode, resp.Header, data, nil
}

// errMessage is the API's error.message, else the status text. Never the raw body.
func errMessage(status int, data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return http.StatusText(status)
}

func (c *Client) dropToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// bearer returns the cached OAuth token, fetching one when it is missing or near expiry.
func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expiry) {
		return c.token, nil
	}
	const path = "/oauth_token.do"
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.auth.ClientID}, "client_secret": {c.auth.ClientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("servicenow: POST %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, _, data, err := c.send(req)
	if err != nil {
		return "", fmt.Errorf("servicenow: POST %s: %w", path, err)
	}
	// The token endpoint's body can echo the credentials, so report the status only.
	if status < 200 || status > 299 {
		return "", fmt.Errorf("servicenow: POST %s: %d %s", path, status, http.StatusText(status))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(data, &tok) != nil {
		return "", fmt.Errorf("servicenow: POST %s: %w", path, ErrUnavailable)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("servicenow: POST %s: no access_token in the response", path)
	}
	c.token = tok.AccessToken
	c.expiry = c.now().Add(time.Duration(tok.ExpiresIn)*time.Second - tokenMargin)
	return c.token, nil
}
