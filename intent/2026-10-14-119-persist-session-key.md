---
status: approved
issue: 119
author: olafkfreund
---

# Intent: a portal restart doesn't sign everyone out

## Problem

The portal signs its session cookies with a key that `web.New` generates
randomly at every start (`internal/web/web.go:100-103`). So every restart,
upgrade or config-driven redeploy signs every browser out, and each user
has to paste the access token again, or go through OIDC again. During
#115, siphon-dev did this twice in one hour.

The same randomness is also the portal's only global sign-out. The
session code says a cookie "stays valid until it expires or the process
restarts" (`web.go:270-274`): logout clears only that browser's cookie,
and nothing revokes sessions server-side. A persistent key would remove
that accidental revocation.

## Proposed outcome

- **Sessions survive a restart.** A signed-in browser stays signed in
  across restarts and upgrades, until the session expires (24 hours for a
  token login, 12 hours for OIDC, as today).
- **Rotating `server.token` still signs everyone out.** That's the
  deliberate "revoke all sessions" action, and it's documented as such.
- **The key never reaches** the config, the logs, the backups or the API.

## Affected users and systems

- Everyone who uses the portal across restarts. The OIDC and token login
  flows are affected.
- `internal/web/web.go` (key setup), `cmd/siphon/main.go:524` (where
  `web.New` is built, to pass a key file or key), and the state directory
  next to the database, where `credentials/` already lives.
- **The backup and monitoring docs:** say whether the key is backed up,
  and that rotating the token signs everyone out.

## Constraints

- **The key file** is created once, with mode 0600, in the state
  directory, never in the Nix store or the config. A missing or unreadable
  file regenerates it, which signs everyone out once. It never weakens
  security.
- **A token rotation invalidates the old sessions.** For example, the
  signing key is derived from both the stored random key and the token's
  hash. The spec settles the exact derivation.
- **No secret in a log or an error.**
- **The NixOS module:** the state directory is already the service's
  writable `StateDirectory`, so there's no new path to declare.

## Open questions

1. **Tie the key to the token?** If you rotate `server.token`, every
   session should die. I'd do it by deriving the signing key from the
   stored random key and the token's hash. The alternative is a separate
   "sign everyone out" button, which is more UI. OK?
2. **Backups.** Should `siphon backup` include the key file? I'd **leave it
   out**. A restored host then makes a fresh key and signs everyone out
   once, which is the safe default after a restore. OK?

## Decisions (at approval)

1. **The signing key is tied to the token.** It's derived from the stored
   random key and the hash of `server.token`, so rotating the token signs
   everyone out. There's no separate button.
2. **Backups:** `siphon backup` leaves the key file out, so a restored host
   makes a fresh key and signs everyone out once.
