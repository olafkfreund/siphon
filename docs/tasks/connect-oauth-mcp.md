# Connect an MCP server that uses OAuth

**Goal:** poll a hosted MCP server, or give an agent its tools, when the
server wants an OAuth login instead of a pasted token. Linear, Notion and
Atlassian are examples.

You log in once, in a browser. Siphon keeps the login and refreshes it.

## 1. Set `server.public_url`

After you log in, the authorization server sends your browser back to
`<public_url>/oauth/callback`, so the URL must be one your browser can reach.

```yaml
server:
  public_url: https://siphon.example.com   # https, or http on localhost / 127.0.0.1
```

With the NixOS module, set `services.siphon.settings.server.public_url`.
This is an operator setting, in `siphon.yaml`.

## 2. Add the source

```yaml
sources:
  linear:
    type: mcp
    url: https://mcp.linear.app/mcp
    auth: { oauth: {} }
    read: { tool: list_my_issues }
    poll: 5m
```

- **Registration:** Siphon registers itself with the server (dynamic client
  registration) the first time you log in, and reuses that registration
  afterwards.
- **A server that doesn't allow registration:** register a client in its
  developer settings, with `<public_url>/oauth/callback` as the redirect URI.
  Then set it here:

  ```yaml
  auth:
    oauth:
      issuer: https://auth.example.com    # the authorization server; see "Pin the authorization server"
      client_id: abc123
      client_secret: file:/run/secrets/linear-client-secret   # env: or file: only; optional
  ```
- **Scopes:** `scopes: [read]` replaces the scopes the server advertises.
- **Pin the authorization server:** `issuer:` names the only authorization
  server Siphon will log in through.
  - **Why:** without it, Siphon uses whichever server the MCP server names,
    and a malicious or compromised MCP server can name its own. That
    server would then get your client secret and show you its own login
    page.
  - **With it set,** a login through any other server fails before
    anything is sent there. This works with dynamic registration too.
  - **When to set it:** always when there's a `client_secret`, and for any
    MCP server you don't run yourself. Siphon warns at start about a
    `client_secret` without an `issuer`.
  - **Where to find the value:** the `issuer` field of
    `<authorization server>/.well-known/oauth-authorization-server`.

`auth.oauth` works only with a remote `type: mcp` source. You can't combine
it with `auth.bearer` or an `Authorization` header.

## 3. Log in

You can do it from the portal or the CLI.

- **Portal:** go to **Sources** and click **Log in** on the source. Approve
  access on the server's page. The browser comes back to Siphon, and the tab
  says you're logged in.
- **CLI:**

  ```sh
  siphon connect oauth linear
  ```

  It prints a URL. Open it in a browser, approve access, and the command
  prints `logged in`. `--no-wait` prints the URL and exits.

A login link works once and expires after 10 minutes. A daemon restart
drops a login that's still in progress. Start it again.

## Afterwards

- **Polling** starts on the next poll. Agents that list the source in `mcp:`
  get its tools.
- **Agent runs** get a current access token, never the refresh token. Job
  output hides the token.
- **Refresh:** Siphon refreshes the token when it's about to expire, and
  writes the new one back. A restart doesn't need a new login.

## When the login stops working

The refresh token can be revoked or expire. When that happens:

- the source shows as failing, with "OAuth login required: siphon connect
  oauth linear, or Log in on the Sources page";
- you get the usual "source failing" notification (see
  [notifications](notifications.md));
- the fix is to log in again.

Changing the source's `url`, `client_id` or `issuer` also needs a new
login, because a login is only ever used for the server it was issued for.
Adding an `issuer` to a source that's already logged in can need one new
login too, when the stored login doesn't record its issuer.

**Log out:** run `siphon connect oauth linear --logout`. This deletes the
stored login.

## What is stored where

- **The file:** `credentials/.mcp/<source>/mcp-oauth.json` next to the
  database (`/var/lib/siphon` on NixOS and in the OCI image), mode 0600.
- **The contents:** the tokens, and the client a dynamic registration
  produced.
- **The previous version:** each refresh keeps the previous file as
  `mcp-oauth.json.prev`, also 0600. Logging out deletes both.
- **A pre-registered `client_secret`** is never copied there. It stays an
  `env:` or `file:` reference.
- **Backups:** `siphon backup` includes the file (see
  [backup and monitoring](backup-and-monitoring.md)). If the server rotates
  refresh tokens, restoring an old backup can bring back a spent one. The
  source then asks for a new login.

## Limits

- **Long agent runs.** A run that outlasts the access token (often an hour)
  gets errors from the server's tools from then on.
- **A private authorization server.** One on a private address needs
  `server.services.private_endpoints`, like any other private host.
- **Not supported:** stdio MCP servers and `http` sources. They keep using
  `env`, `auth.bearer` or `headers`.
