---
status: approved
issue: 119
intent: intent/2026-10-14-119-persist-session-key.md
---

# Spec: a portal restart doesn't sign everyone out

## Design

### 1. The stored key

- **The file:** `<state dir>/portal-session.key`, where the state dir is
  `filepath.Dir(cfg.Server.DB)`.
- **Why there:** `siphon backup` archives `state.db`, `secrets/` and
  `credentials/` (`cmd/siphon/backup.go:175-192`). A file outside those is
  left out, which is decision 2. `siphon restore` replaces only those
  three (`:311-356`), so a restore on the same host keeps the key file and
  its sessions. A new host restored from a backup makes a fresh key, and
  everyone signs in once.
- **The contents:** 32 random bytes, raw, mode 0600, created with
  `O_CREATE|O_EXCL`. A loser of a creation race reads the winner's file.
  It's never logged.
- **Errors:**
  - a file with the wrong size or unreadable: log `portal session key
    unusable, generating a new one` (no content), replace it atomically
    (write a temp file, then rename), and carry on. Everyone signs in
    again once;
  - no file can be written (a read-only state dir): fall back to an
    in-memory random key, as today, with one warning.

### 2. Tie it to the token (decision 1), in `internal/web/web.go`

`server.key` becomes `HMAC-SHA256(stored, "siphon-portal-session|" + tokHash)`,
where `tokHash = sha256(Token)` is already computed at `:100`. Rotating
`server.token` (and restarting, because `server.*` takes effect on
restart) changes the key, so every old cookie fails `parseSession`. A
restart with the same token keeps every cookie valid until its TTL.

### 3. Wiring

- **`web.Options`** gains `SessionKey []byte`. When it's nil (tests and
  embedded uses), `New` keeps today's per-process random key.
- **`cmd/siphon/main.go:524`** calls a new
  `web.LoadSessionKey(filepath.Dir(cfg.Server.DB))` and passes the result
  in.
- **The function** lives in `internal/web/sessionkey.go`, about 40 lines.
- **The comment at `web.go:270-274`** changes to: valid until it expires,
  or until `server.token` changes.

### 4. Docs

- **`docs/tasks/backup-and-monitoring.md`:** the key file isn't backed
  up; a restore on a new host signs everyone out once.
- **The security notes:** rotating `server.token` is how to sign everyone
  out. Logout clears only that browser's cookie, as before.

## Alternatives rejected

- **Putting the key in `secrets/`:** it would be backed up, which goes
  against decision 2, and it would mix a generated key with the
  operator's secrets.
- **Deriving the key from the token alone:** anyone who knows the token
  could forge sessions offline for any role and actor. The random part
  stops that.
- **Server-side session storage:** more state and migrations. The
  stateless HMAC design stays.

## Risks

- **A stolen cookie now stays valid across restarts,** until its TTL (24
  hours for a token login, 12 hours for OIDC). Before, a restart killed
  it. The mitigation is documented: rotate `server.token`.
- **The NixOS module:** the state dir is the service's `StateDirectory`,
  already writable. The microVM and OCI image use the same state path.
- **Tests that assume a random key per server instance** keep it, because
  `SessionKey` is nil in tests.

## Verification

- **`internal/web`, `sessionkey_test.go`:**
  - the first load creates a 0600 file of 32 bytes, and a second load
    returns the same bytes;
  - a 5-byte file is replaced;
  - a read-only directory falls back with no error;
  - nothing is written to the log beyond the fixed message.
- **`internal/web`:**
  - two `New(...)` servers with the same `SessionKey` and `Token` accept
    each other's session cookie, which is the restart case;
  - with a different `Token`, the cookie is rejected;
  - with `SessionKey` nil, the cookie is rejected, as today.
- **`cmd/siphon`:** `siphon backup` doesn't include
  `portal-session.key`.
- **`go vet ./... && go test -race ./...`,** plus the NixOS VM test in
  CI.
- **Live on siphon-dev:** sign in, restart, and the browser is still
  signed in.
