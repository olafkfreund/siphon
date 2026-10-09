package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
)

// asRole returns a session cookie and CSRF token for a signed role.
func (e *env) asRole(r role, actor string) (*http.Cookie, string) {
	c := &http.Cookie{Name: cookieName, Value: e.srv.sessionValue(r, actor)}
	return c, e.srv.csrfFor(c.Value)
}

func (e *env) postAs(c *http.Cookie, csrf, path string) int {
	return e.do("POST", path, url.Values{"csrf": {csrf}}, func(r *http.Request) { r.AddCookie(c) }).Code
}

func TestRolesGateWrites(t *testing.T) {
	e := newEnv(t, nil)
	for _, tc := range []struct {
		role   role
		path   string
		denied bool
	}{
		{roleViewer, "/rules/disk-full/disable", true}, // operator route
		{roleViewer, "/config/rules/x/delete", true},   // admin route
		{roleOperator, "/rules/disk-full/disable", false},
		{roleOperator, "/config/rules/x/delete", true},
		{roleAdmin, "/rules/disk-full/disable", false},
		{roleAdmin, "/config/rules/x/delete", false},
	} {
		c, csrf := e.asRole(tc.role, "u")
		code := e.postAs(c, csrf, tc.path)
		if (code == 403) != tc.denied {
			t.Errorf("%s %s: %d", tc.role, tc.path, code)
		}
	}
}

func TestSessionRejectsForgeryAndOld(t *testing.T) {
	e := newEnv(t, nil)
	good := e.srv.sessionValue(roleViewer, "u")
	forged := strings.Replace(good, "|viewer|", "|admin|", 1)
	old := good[:strings.Index(good, "|")] + "|" + e.srv.mac("session:"+good[:strings.Index(good, "|")])
	for name, v := range map[string]string{"forged role": forged, "old format": old, "garbage": "x"} {
		if _, ok := e.srv.parseSession(v); ok {
			t.Errorf("%s accepted", name)
		}
	}
	if se, ok := e.srv.parseSession(good); !ok || se.role != roleViewer || se.actor != "u" {
		t.Errorf("good session: %+v %v", se, ok)
	}
}

func TestOIDCSessionExpiresAt12h(t *testing.T) {
	e := newEnv(t, nil)
	o, a := e.srv.sessionValue(roleViewer, "oidc:a@x"), e.srv.sessionValue(roleAdmin, "portal")
	e.now = e.now.Add(12*time.Hour + time.Second)
	if _, ok := e.srv.parseSession(o); ok {
		t.Error("oidc session valid after 12h")
	}
	if _, ok := e.srv.parseSession(a); !ok {
		t.Error("token session should last 24h")
	}
	e.now = e.now.Add(12 * time.Hour)
	if _, ok := e.srv.parseSession(a); ok {
		t.Error("token session valid after 24h")
	}
}

func TestTokenLoginOff(t *testing.T) {
	off := false
	e := newEnv(t, func(o *Options) {
		o.Cfg.Server.OIDC = &config.OIDC{TokenLogin: &off}
	})
	if w := e.do("POST", "/login", url.Values{"token": {tok}}, nil); w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestActorFromSession(t *testing.T) {
	e := newEnv(t, nil)
	c, csrf := e.asRole(roleOperator, "oidc:a@x")
	if code := e.postAs(c, csrf, "/rules/disk-full/disable"); code != 303 {
		t.Fatalf("got %d", code)
	}
	var by string
	if err := e.st.DB.QueryRow(`SELECT actor FROM audit WHERE event='rule_disable'`).Scan(&by); err != nil || by != "oidc:a@x" {
		t.Fatalf("actor %q %v", by, err)
	}
}

// Every portal POST route is classified: operator, logout, or admin-only.
func TestPortalPOSTRoles(t *testing.T) {
	operator := regexp.MustCompile(`^/(approvals/\d+/approve|rules/r/enable|rules/test|agents/egress-preview|connections/n/test|services/n/test|services/c/n/test|notifications/n/test|sources/n/oauth)$`)
	files, _ := filepath.Glob("*.go")
	pat := regexp.MustCompile(`"POST (/[^"]*)"`)
	sub := regexp.MustCompile(`\{[^}]*\}`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range pat.FindAllStringSubmatch(string(b), -1) {
			p := m[1]
			if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/hook/") || p == "/login" {
				continue
			}
			if strings.HasSuffix(p, "/") { // built with + verb in configRoutes
				for _, v := range []string{"check", "save", "delete", "reset"} {
					seen[strings.Replace(sub.ReplaceAllString(p, "n"), "n/n/", "rules/n/", 1)+v] = true
				}
				continue
			}
			p = sub.ReplaceAllStringFunc(p, func(x string) string {
				switch x {
				case "{id}":
					return "1"
				case "{verb}":
					if strings.HasPrefix(p, "/approvals") {
						return "approve"
					}
					return "enable"
				case "{kind}":
					return "rules"
				case "{name}":
					if strings.HasPrefix(p, "/rules") {
						return "r"
					}
				}
				return "n"
			})
			seen[p] = true
		}
	}
	if len(seen) < 15 {
		t.Fatalf("route scan found only %d", len(seen))
	}
	e := newEnv(t, nil)
	for p := range seen {
		for _, r := range []role{roleViewer, roleOperator} {
			c, csrf := e.asRole(r, "u")
			want403 := p != "/logout" && (r == roleViewer || !operator.MatchString(p))
			if code := e.postAs(c, csrf, p); (code == 403) != want403 {
				t.Errorf("%s POST %s: %d, want403=%v", r, p, code, want403)
			}
		}
	}
}

// Controls a role can't use are hidden; the state they show is not.
func TestRolesHideControls(t *testing.T) {
	e := newEnv(t, nil)
	for _, tc := range []struct {
		role    role
		toggle  bool
		newRule bool
	}{
		{roleViewer, false, false},
		{roleOperator, true, false},
		{roleAdmin, true, true},
	} {
		c, _ := e.asRole(tc.role, "u")
		b := e.do("GET", "/rules", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
		if got := strings.Contains(b, `action="/rules/disk-full/`); got != tc.toggle {
			t.Errorf("%s: toggle form %v", tc.role, got)
		}
		if got := strings.Contains(b, `href="/config/rules/new"`); got != tc.newRule {
			t.Errorf("%s: new rule link %v", tc.role, got)
		}
		if !strings.Contains(b, `role="switch"`) {
			t.Errorf("%s: rule state not shown", tc.role)
		}
	}
}
