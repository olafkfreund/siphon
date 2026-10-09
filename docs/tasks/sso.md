# Sign in to the portal with SSO

**Goal:** people sign in to the portal with their own account from your
identity provider (Keycloak, Authentik, Google, Entra ID, GitHub through
Dex, or any other OpenID Connect provider). Each person gets a role, and
the audit log names them.

Sign-in with SSO is off until you configure it. The admin token keeps
working for the CLI, the API and scripts.

## Roles

| Role | Can |
|---|---|
| **viewer** | read every page. Secrets in `env:` or `file:` refs are never shown, but plain values in the config are, including header values, and so is the config export |
| **operator** | a viewer who can also approve and deny jobs, turn rules on and off, run rule tests and the egress preview, test connections, services and notifications, and start an MCP OAuth login |
| **admin** | everything, including config edits, logins, notifications and services |

The server checks the role on every request. Controls a role can't use are
also hidden, but hiding them is only a convenience.

## 1. Register a client with your provider

Create an OpenID Connect client (often called an "application"):

- **Type:** a web application, with the authorization code flow. Siphon
  always uses PKCE, so a public client with no secret also works.
- **Redirect URI:** `<public_url>/login/oidc/callback`, for example
  `https://siphon.example.com/login/oidc/callback`.
- **Scopes:** `openid`, `email` and `profile`, plus whatever your provider
  needs to send groups (see the provider notes below).

Note the issuer URL, the client ID and, if there is one, the client secret.

## 2. Configure Siphon

This is an operator setting, in `siphon.yaml`:

```yaml
server:
  public_url: https://siphon.example.com   # https, or http on localhost
  oidc:
    issuer: https://id.example.com/realms/main
    client_id: siphon
    client_secret: file:/run/credentials/siphon.service/oidc-secret   # optional
    scopes: [openid, email, profile, groups]   # default [openid, email, profile]
    groups_claim: groups                       # default "groups"
    roles:
      admin:    { groups: [siphon-admins], emails: [alice@example.com] }
      operator: { groups: [oncall] }
      viewer:   { groups: [engineering] }
```

**How a role is chosen:**
- **Highest wins:** a person in both `oncall` and `siphon-admins` is an
  admin.
- **Emails need verification:** an email only counts when the provider
  says it is verified (`email_verified: true`).
- **No match, no access:** someone who matches no role is refused. The
  refusal is logged in the audit log as `oidc_refused`.
- **`groups_claim` must be a claim only your provider's admins control,**
  such as `groups` or `roles`. Never point it at `name`,
  `preferred_username` or anything else people can edit about themselves:
  they could give themselves a group like `siphon-admins`.

**The other settings:**
- **The issuer** must be `https`, or `http` on localhost.
- **`allow_private: true`** is for a provider on a private address, such
  as a LAN Keycloak. Siphon follows no redirects when it talks to the
  provider.
- **The client secret** is an `env:` or `file:` reference, never a literal.
  With the NixOS module, pass it as a credential:

  ```nix
  services.siphon.credentials.oidc-secret = "/run/agenix/siphon-oidc-secret";
  services.siphon.settings.server.oidc.client_secret =
    "file:/run/credentials/siphon.service/oidc-secret";
  ```

Restart Siphon. The login page now shows **Sign in with SSO**. After any
later change to `server.oidc`, restart Siphon again.

## 3. Sign in

Open the portal and choose **Sign in with SSO**. After the provider sends
you back, Siphon shows "Signed in" and opens the dashboard. Your name and
role are shown next to **Sign out**.

**Sessions:**
- **Length:** an SSO session lasts 12 hours, and a token login lasts 24.
- **What's in it:** nothing secret. The provider's tokens are not kept
  after sign-in.
- **The audit log** records SSO actions as `oidc:<email>`, or
  `oidc:<subject>` when the email isn't verified. Token logins are
  recorded as `portal`.

## Provider notes

- **Keycloak:** the issuer is `https://<host>/realms/<realm>`. Add a
  "Group Membership" mapper to the client, with the token claim name
  `groups` and "Full group path" off.
- **Authentik:** the issuer is
  `https://<host>/application/o/<application-slug>/`. Groups arrive in the
  `groups` claim with the default `profile` scope.
- **Google:** the issuer is `https://accounts.google.com`. Google sends no
  groups, so map people by email.
- **Entra ID:** the issuer is
  `https://login.microsoftonline.com/<tenant-id>/v2.0`.
  - **Groups:** add a groups claim under "Token configuration". It sends
    group object IDs, not names.
  - **Large tenants:** past about 200 groups, Entra ID leaves groups out of
    the token. Use app roles with `groups_claim: roles`.
  - **No email mapping:** Entra ID doesn't send `email_verified`, so
    `emails:` never match. Map by groups or app roles only.
- **GitHub, through Dex:** GitHub alone isn't an OpenID Connect provider.
  Put Dex in front of it with the GitHub connector.
  - Add `groups` to `scopes`.
  - Teams arrive as `<org>:<team>`, for example
    `roles.operator.groups: [acme:oncall]`.

## SSO only

To hide the token form, set `token_login: false`:

```yaml
server:
  oidc:
    token_login: false
```

The admin token still works for the CLI and the API. Leave the form on
unless you have another way in when the provider is down.

## Break-glass access

When the provider is down, or a wrong mapping locks everyone out, sign in
with the admin token (`server.token`), or use the CLI.

## Taking access away

A session keeps the role it was signed in with until it ends, at most 12
hours later. That holds after you remove someone from a group at the
provider, and after you take their group or email out of `roles`.

**To cut access at once,** remove them at the provider, then restart
Siphon. A restart ends every portal session.

Changes to `server.oidc` apply when Siphon restarts, and a restart also
ends every session. Siphon also checks each request against the config it
is running with, so an SSO session stops working once `server.oidc` is
gone, and a token session once `token_login` is `false`.

## Upgrading

The session cookie format changed in this version, so everyone signs in
once more after the upgrade.

## Troubleshooting

- **"SSO provider unreachable":** Siphon couldn't read the provider's
  discovery document at `<issuer>/.well-known/openid-configuration`. Check
  the issuer URL. For a provider on a private address, set
  `allow_private: true`.
- **"SSO sign-in failed (…)":** the words in brackets name the step that
  failed. The full reason is in the Siphon log (`journalctl -u siphon`).
  "state mismatch" or "sign-in expired" means more than 10 minutes passed,
  or the sign-in was started in another browser.
- **"… has no role in Siphon":** the person signed in, but matched no
  mapping. Check the groups claim name and what the provider sends. Some
  providers only send groups when a scope or a mapper asks for them.
