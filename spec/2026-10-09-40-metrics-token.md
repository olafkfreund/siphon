---
status: draft
issue: 40
intent: intent/2026-10-09-40-metrics-token.md
---

# Spec: scrape-only token for /metrics

## Design

**Summary:**
- a new optional secret `server.metrics.token`;
- `GET /metrics` accepts it or the admin token;
- every other route still accepts only the admin token.

There is no migration, no new NixOS option and no new route.

### Config (`internal/config/config.go`)

```go
type Server struct {
	…
	Metrics MetricsServer `yaml:"metrics"`
}

// MetricsServer configures GET /metrics.
type MetricsServer struct {
	// Token opens /metrics and nothing else; the admin token works there too.
	Token Secret `yaml:"token"`
}
```

- **Secret refs only.** `secretPtrs()` (`config.go:648`) gains
  `&c.Server.Metrics.Token`, so the value must be an `env:` or `file:`
  ref. Then:
  - the existing resolve loops (`config.go:620`, `:700`) read it;
  - a plain literal is rejected, as for `server.token`;
  - it is masked wherever resolved secrets are masked.
- **`Validate`,** next to the `server.token` length check (`config.go:938`).
  When the token is set and resolved:
  - `server.metrics.token: must be at least 32 characters`;
  - `server.metrics.token: must differ from server.token`. A shared value
    would turn the scrape credential back into the admin token without
    anyone noticing.
- **Operator-only.** The overlay `Kinds` exclude `server`, so the portal, the
  API and `siphon apply` can't set or read it. That's unchanged.
- **Schema:** regenerate `schema/siphon.schema.json` and
  `schema/agentgw.schema.json`.

### Web (`internal/web`)

- **`web.go`:**
  - `Options` gains `MetricsToken string`. An empty value means only the
    admin token works.
  - `server` gains `metricsHash [32]byte`, set in `New` like `tokHash`.
  - New:

    ```go
    // metricsOK: the scrape token or the admin token.
    func (s *server) metricsOK(t string) bool {
    	sum := sha256.Sum256([]byte(t))
    	return s.tokenOK(t) || s.MetricsToken != "" && subtle.ConstantTimeCompare(sum[:], s.metricsHash[:]) == 1
    }
    ```
- **`api.go:131`:** the body of `api` moves into
  `bearer(ok func(string) bool, h http.HandlerFunc)`. Then:
  - `api(h)` becomes `bearer(s.tokenOK, h)`;
  - `/metrics` is registered with `s.bearer(s.metricsOK, s.metrics)`.

  So `/metrics` keeps the same 401, `WWW-Authenticate` header and failed-auth
  limiter. Nothing else calls `metricsOK`. The portal login
  (`portal.go:217`) and every `/api` route stay on `tokenOK`.
- **`cmd/siphon/main.go:527`:** passes
  `MetricsToken: cfg.Server.Metrics.Token.Value`.
- **When a change applies:** like `server.token`, the value is read at
  start. A change needs a restart, and the docs say so.

### NixOS

No module change (resolved question 3). An operator writes:

```nix
services.siphon.credentials.metrics-token = "/run/agenix/siphon-metrics-token";
services.siphon.settings.server.metrics.token = "file:/run/credentials/siphon.service/metrics-token";
```

### Docs (`docs/tasks/backup-and-monitoring.md`, Metrics section)

- **The scrape example:** it uses the scrape token, and a short block shows
  how to set it, in YAML and in Nix.
- **The "admin token" bullet** is replaced with:
  - use the scrape token;
  - the admin token still works but gives the scraper everything;
  - a change needs a restart.
- **Generated docs:** regenerate `llms-full.txt`.

## Alternatives rejected

- **A separate unauthenticated metrics listener.** Rejected in #38 (question
  1), and the intent says no new port.
- **Scoped tokens or roles.** Out of scope: they belong to users and roles
  with OIDC.
- **A token stored in the db and set from the portal.** That would make an
  operator credential editable by anyone with the admin token, and
  `server` is operator-only.
- **Dropping the admin token on `/metrics`.** It breaks every existing
  scrape config (resolved question 2).

## Risks

- **A wrong route gets `metricsOK`.** That widens the scrape token's reach.
  The mitigations:
  - there is one call site;
  - a unit test sends the scrape token to an `/api` route and to the portal
    login, and expects both to be refused;
  - the VM test checks it end to end.
- **The token leaks through output.** It is a `secretPtrs` entry, so it is
  masked like `server.token`, and it is never logged. Same handling.
- **Upgrade and downgrade.** Without `server.metrics`, nothing changes.
  v0.4.0 rejects the `server.metrics` key (strict YAML), so a downgrade means
  removing it first. The release notes say so.

## Verification

- **`go test -race ./internal/web`:**
  - the scrape token gets 200 on `/metrics`;
  - it gets 401 on `GET /api/inventory`;
  - it fails the portal login;
  - the admin token still gets 200 on `/metrics`;
  - a wrong token gets 401;
  - no `MetricsToken` set: an empty bearer gets 401.
- **`go test ./internal/config`:**
  - a plain literal is rejected;
  - a value under 32 characters is rejected;
  - a value equal to `server.token` is rejected;
  - a valid `file:` ref resolves;
  - the schema is current.
- **`nix flake check`:** the VM test sets the token through
  `services.siphon.credentials`. It checks:
  - `curl` with the scrape token: 200 on `/metrics` and 401 on `/api/inventory`;
  - the admin token: still 200.
- **`go vet ./...`** and **`devenv test`** pass.
