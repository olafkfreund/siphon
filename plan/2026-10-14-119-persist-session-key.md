---
status: draft
issue: 119
spec: spec/2026-10-14-119-persist-session-key.md
---

# Plan: a portal restart doesn't sign everyone out

## Approved decisions, carried over

- **The key file:** `<filepath.Dir(cfg.Server.DB)>/portal-session.key`.
  - It holds 32 raw random bytes, with mode 0600, and is created with
    `O_CREATE|O_EXCL`. A creation race reads the winner's file.
  - **A bad file** (the wrong size, or unreadable) is replaced
    atomically, with a temp file and a rename, and logged with a fixed
    message and no content.
  - **When it can't be written,** an in-memory random key is used, with
    one warning.
- **The key sits outside `secrets/` and `credentials/`,** so `siphon
  backup` (`cmd/siphon/backup.go:175-192`) leaves it out (intent decision
  2). A same-host restore keeps it.
- **The signing key** is `HMAC-SHA256(stored, "siphon-portal-session|" + tokHash)`,
  with `tokHash` as computed at `web.go:100`. A token rotation signs
  everyone out (intent decision 1).
- **`web.Options.SessionKey []byte`:** when it's nil, `New` keeps today's
  random per-process key, so the tests are unchanged.

## Steps

1. **The new `internal/web/sessionkey.go`:**
   `func LoadSessionKey(dir string) ([]byte, error)`, under about 40
   lines.
   - It reads `dir/portal-session.key`. If the file is missing, it
     creates it with O_EXCL and mode 0600; on EEXIST it reads the file
     again.
   - If the file has the wrong size, it writes a temp file in `dir`,
     renames it over, and logs `portal session key unusable, generating a
     new one`.
   - It returns the error only when nothing could be written. The caller
     then falls back.
   - **The test, `sessionkey_test.go`, uses a `t.TempDir()`:**
     - creating a file of mode 0600 and 32 bytes;
     - loading it again gives the same bytes;
     - a 5-byte file is replaced;
     - in a read-only directory (`chmod 0500`, skipped as root) it returns
       an error, and nothing is written.

   → verify: `go test -race ./internal/web -run SessionKey`.

   Traps: never log the key, and never put it in an error.

2. **`internal/web/web.go`:**
   - `Options` gains `SessionKey []byte`.
   - In `New` (`:100-103`), when `len(o.SessionKey) == 32`, set
     `s.key = hmac(o.SessionKey, "siphon-portal-session|"+hex(tokHash))`.
     Otherwise keep the `rand.Read`.
   - Update the comment at `:270-274`: valid until it expires or
     `server.token` changes.
   - **The tests:**
     - two servers with the same `SessionKey` and `Token` accept each
       other's cookie;
     - with a different `Token`, the cookie is rejected;
     - with `SessionKey` nil, it's rejected. Use the existing test helpers
       for signing in and getting a cookie.

   → verify: `go test -race ./internal/web`.

3. **`cmd/siphon/main.go` (`:524`).**
   - Before `web.New`, call `key, err := web.LoadSessionKey(filepath.Dir(cfg.Server.DB))`.
   - On an error, `slog.Warn("portal sessions will not survive a restart", "err", err)`,
     and `key = nil`.
   - Pass `SessionKey: key`.
   - **The test, in `cmd/siphon/backup_test.go`:** a state dir with
     `portal-session.key` gives a backup archive that doesn't contain it.

   → verify: `go test -race ./cmd/siphon`.

   Traps: the error message must not include the key path's contents.

4. **Docs.**
   - **`docs/tasks/backup-and-monitoring.md`:**
     - the key file isn't backed up, and a restore on a new host signs
       everyone out once;
     - rotating `server.token` signs everyone out.
   - **Where the docs describe the portal's sign-in or security,** add the
     same sentence about rotating the token. Find the spot with
     `grep -rn "sign out\|Sign out\|logout" docs`.
   - Run `go generate ./docs`.

   → verify: `go test -race ./docs/...`.

## Tests

```sh
go vet ./... && go test -race ./...
```

**Live, on siphon-dev (mine):** sign in, restart by PID, and still be
signed in. **Model split:** 4 steps, so the `coder` does them one at a
time. A fresh Opus agent reviews the diff.

## Rollback

Revert the PR. An old binary ignores the key file, which goes back to
being signed out on restart. The file can be deleted.
