# Run Siphon behind a reverse proxy

**Goal:** reach the portal and the API over HTTPS, with a reverse proxy
such as Caddy or nginx in front of Siphon.

Siphon serves plain HTTP. It warns at start when `server.listen` isn't a
loopback address, because tokens and session cookies would then cross the
network in clear text. The usual fix is:

- Siphon listens on loopback;
- a proxy on the same host terminates TLS and forwards to it.

## 1. Listen on loopback

```yaml
server:
  listen: 127.0.0.1:8080
  public_url: https://siphon.example.com
```

`public_url` is the address people and providers use. Webhook URLs, OAuth
logins and SSO all send the browser back to it.

## 2. Configure the proxy

The proxy must pass on the client's address in `X-Forwarded-For`, and the
scheme in `X-Forwarded-Proto`. Both examples below do.

**Caddy** sets both headers by itself, and gets a certificate
automatically:

```
siphon.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

**nginx:**

```nginx
server {
    listen 443 ssl;
    server_name siphon.example.com;
    # ssl_certificate and ssl_certificate_key here

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## 3. Tell Siphon which proxies to trust

Siphon limits failed sign-ins per client: 5 a minute. Behind a proxy,
every request comes from the proxy's address, so without this step all
your users share one limit. Anyone can then lock everyone out of:

- the token login;
- SSO;
- the API and the CLI;
- the MCP OAuth callback.

List the proxies' addresses:

```yaml
server:
  trusted_proxies: [127.0.0.1]   # the proxy on this host
```

**What Siphon does with the list:**
- **Entries** are addresses or ranges, such as `10.0.0.0/8` or
  `fd00::/8`.
- **Trusted requests only:** Siphon reads `X-Forwarded-For` only on
  requests that come from a listed address. It reads the header from the
  right, skipping listed proxies, and uses the first address that isn't
  one. A client can't pick its own address by sending the header itself.
- **No trusted proxies, no header:** with no `trusted_proxies`, Siphon
  ignores `X-Forwarded-For` entirely.

**Never list a range that clients can connect from directly.** Anyone
inside it could choose their own address and get unlimited sign-in
attempts. List only the proxies themselves.

The setting is read at start, so restart Siphon after changing it.

**Chains of proxies.** With a load balancer in front of nginx, list both.
Siphon skips every listed hop.

## NixOS

```nix
services.siphon.settings.server = {
  listen = "127.0.0.1:8080";
  public_url = "https://siphon.example.com";
  trusted_proxies = [ "127.0.0.1" ];
};
```

Then configure `services.caddy` or `services.nginx` as above.

## Checking it

- **The start-up warning** about `server.listen` is gone.
- **Five bad sign-ins** from one browser block that browser for a minute,
  and nobody else.
