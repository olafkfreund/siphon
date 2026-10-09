---
status: approved
issue: 40
spec: spec/2026-10-09-40-metrics-token.md
---

# Plan: scrape-only token for /metrics

## Approved decisions (from the intent and the spec)

- **The config:** a new optional secret, `server.metrics.token`, in
  `type MetricsServer struct{ Token Secret }`, stored at `Server.Metrics`
  (yaml `metrics`).
  - It must be an `env:` or `file:` ref, because it is in `secretPtrs()`.
  - It is at least 32 characters.
  - It must differ from `server.token`.
- **Where it works:** `GET /metrics` accepts it or the admin token. Every
  `/api` route and the portal login accept only the admin token.
- **The auth path:** the same 401, `WWW-Authenticate` header and failed-auth
  limiter as the API. The body of `api` moves into
  `bearer(ok func(string) bool, h)`.
- **When a change applies:** the token is read at start, so a change needs a
  restart.
- **Out of scope:**
  - no NixOS option: operators use `services.siphon.credentials`;
  - no migration;
  - no new route or port;
  - no roles.
- **Operator-only:** overlay `Kinds` already exclude `server`, and that stays
  unchanged.

## Steps

1. **Config.** Done by the coder.
   - **Changes:**
     - `internal/config/config.go`: in `Server` (after `Retention`,
       around line 120), add `Metrics MetricsServer \`yaml:"metrics"\``,
       commented. Add `type MetricsServer struct { Token Secret \`yaml:"token"\` }`
       next to `type Retention`.
     - `secretPtrs()` (line 648): add `&c.Server.Metrics.Token` after
       `&c.Server.Token`.
     - `Validate`: after the `server.token` length check (around line 938),
       when `m := c.Server.Metrics.Token; m.isSet() && m.Value != ""`:
       - `len < 32` → `server.metrics.token: must be at least 32 characters`;
       - `m.Value == c.Server.Token.Value` → `server.metrics.token: must
         differ from server.token`.
   - **Tests:** in `internal/config/config_test.go`, add
     `TestMetricsTokenValidation`, table-driven like
     `TestRetentionValidation` (line 1006), using `t.Setenv`. It covers:
     - a valid `env:` ref, which resolves into `.Value`;
     - a plain literal (`server: {metrics: {token: abc…}}`) → `inline secret`;
     - a short value → `at least 32`;
     - the same value as `server.token` → `must differ`;
     - unset → ok.

     It also asserts that the value appears in `c.Secrets()`.
   - **Then:** regenerate the schemas with
     `UPDATE_SCHEMA=1 go test ./internal/config` (`schema/siphon.schema.json`
     and `schema/agentgw.schema.json`).
   - **Verify:** `go vet ./... && go test -race ./internal/config`.
   - **Traps:**
     - the YAML decoder is strict (`KnownFields`);
     - never log the value.
2. **Web and wiring.** Done by the coder.
   - **Changes:**
     - `internal/web/web.go`:
       - `Options` gains `MetricsToken string`, commented as "opens
         /metrics only; empty: admin token only".
       - `server` gains `metricsHash [32]byte`, set in `New` (line 78) as
         `sha256.Sum256([]byte(o.MetricsToken))`.
       - Add `metricsOK(t string) bool` after `tokenOK` (line 177). It
         returns `s.tokenOK(t)`, or `s.MetricsToken != ""` and a
         constant-time compare of `sha256(t)` with `metricsHash`.
     - `internal/web/api.go:131`: rename the body to
       `func (s *server) bearer(ok func(string) bool, h http.HandlerFunc) http.HandlerFunc`,
       with `!ok(tok)` in place of `!s.tokenOK(tok)`. Then
       `func (s *server) api(h http.HandlerFunc) http.HandlerFunc { return s.bearer(s.tokenOK, h) }`.
     - `api.go:24`: `mux.HandleFunc("GET /metrics", s.bearer(s.metricsOK, s.metrics))`.
     - `cmd/siphon/main.go:527`: add
       `MetricsToken: cfg.Server.Metrics.Token.Value`.
   - **Tests:** in `internal/web/metrics_test.go`, add `TestMetricsToken`
     with `newEnv(t, func(o *Options){ o.MetricsToken = scrape })`, where
     `scrape` is a const of 32 or more characters. It checks:
     - scrape → 200 on `/metrics`;
     - scrape → 401 on `GET /api/inventory`;
     - `POST /login` with `token=scrape` → 401;
     - admin `bearer` → 200 on `/metrics`;
     - a wrong token → 401.

     A second env with no `MetricsToken` checks that `Bearer ` (empty) →
     401 on `/metrics`.
   - **Verify:** `go vet ./... && go test -race ./...`.
   - **Traps:**
     - `metricsOK` must not be used anywhere but `/metrics`;
     - the limiter is shared, so after 5 failures the test IP gets 429.
       Order the failing requests last, or count them.
3. **Docs.** Done by Opus.
   - **Changes:** in `docs/tasks/backup-and-monitoring.md`, Metrics section:
     - say that `/metrics` takes the scrape token (`server.metrics.token`)
       or the admin token;
     - add a short YAML block and a Nix block
       (`services.siphon.credentials.metrics-token` plus
       `settings.server.metrics.token = "file:/run/credentials/siphon.service/metrics-token"`);
     - point the scrape example's `credentials_file` at the scrape token;
     - replace the "The token is the admin token" bullet with: use the scrape
       token, the admin token still works but gives the scraper everything,
       and a change needs a restart.
   - **Then:** regenerate `llms-full.txt` with the repo's generator; check
     how it was regenerated in #38.
   - **Verify:** `go test ./...` (doc embedding tests).
   - **Traps:** none.
4. **VM test.** Done by Opus.
   - **Changes:** in `nix/vm-test.nix`:
     - add `environment.etc."siphon/metrics-token".text` (32 or more
       characters, a different value);
     - add `credentials.metrics-token = "/etc/siphon/metrics-token"`;
     - add `settings.server.metrics.token = "file:/run/credentials/siphon.service/metrics-token"`.
   - **The subtest:** in the existing "backup, offline restore and metrics"
     subtest, after the admin-token check (around line 916), check:
     - the scrape token → 200 on `/metrics`;
     - the scrape token → 401 on `/api/inventory`.
   - **Verify:** `nix flake check`.
   - **Traps:**
     - the VM script is type-checked;
     - don't put `systemctl restart` in Bash text (the hook): edit with Edit;
     - subtests must clean up.
5. **Security review.** Opus, in a fresh agent given only this plan and
   `git diff main`. Fix each finding or record it here as accepted, in the
   same commit as any fix.

## Tests

- **`go vet ./... && go test -race ./...`:** all pass. This covers the new
  `TestMetricsTokenValidation` and `TestMetricsToken`, and that the schema
  is current.
- **`nix flake check`:** passes, including the VM test with the scrape token.
- **`devenv test`:** passes.
- **The PR's CI:** green.

## Rollback

- **The code:** revert the merge commit. There is no migration and no
  persistent state.
- **The config:** a config with `server.metrics` then fails strict decoding.
  Remove the key, or keep scraping with the admin token, which never stopped
  working.

## Deviations

- **Step 2 (coder):** in `bearer`, the `CutPrefix` result is renamed `found`
  so that it doesn't shadow the new `ok` parameter. Behaviour is unchanged.
- **Step 5 (security review: no High or Medium):**
  - **L1, accepted.** A failing scrape shares the per-IP failed-auth limiter
    with the API and portal. A Prometheus with a stale token can therefore
    get its IP 429'd on the API too. This was already true with the admin
    token, and the plan chose the shared limiter. A separate limiter can
    come later if it bites.
  - **L2, fixed.** `metricsOK` always runs both compares, so timing doesn't
    reveal which token matched.
  - **L3, fixed.** `TestMetricsToken` now checks that the scrape token on
    `GET /` gets a 303 redirect to `/login`.
