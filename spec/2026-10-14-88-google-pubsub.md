---
status: approved
issue: 88
intent: intent/2026-10-14-88-google-pubsub.md
---

# Spec: Google Cloud Pub/Sub push subscriptions trigger rules

## Design

```mermaid
sequenceDiagram
  participant P as Pub/Sub push
  participant S as Siphon /hook/<name>
  participant G as googleapis.com certs
  P->>S: POST envelope, Authorization: Bearer <Google ID token>
  S->>G: JWKS (cached; refetched on an unknown kid), guarded client
  S->>S: verify RS256, exp, iss, aud, email, email_verified
  S->>S: decode message.data (base64, JSON if possible)
  S-->>P: 204 (delivered, or a duplicate by messageId)
```

### Config (`internal/config/config.go`)

The webhook `Source` gains:

```go
Audience       string `yaml:"audience"`        // google-oidc: expected aud (default server.public_url + /hook/<name>)
ServiceAccount string `yaml:"service_account"` // google-oidc: required email of the push identity
```

**Validation:**
- **`signature: google-oidc`:**
  - `service_account` is required and looks like an email
    (`^[^@\s]+@[^@\s]+$`);
  - `secret`, `signature_header`, `token_header`, `timestamp_header`,
    `token_field` and `id` aren't allowed;
  - `audience`, if set, is an absolute `https://` URL or any non-empty
    string (Pub/Sub accepts custom audiences);
  - if `audience` is empty, `server.public_url` must be set, or Siphon
    can't know the default.
- **The required-secret check** skips `google-oidc`.
- **`format` gains `pubsub`,** which needs `google-oidc`.
- **`audience` and `service_account`** are refused on other signature
  modes.

The schema and `explain` are updated.

### The verifier (`internal/source/googleoidc.go`, new)

```go
// GoogleVerifier checks Google-signed ID tokens (Pub/Sub push auth).
type GoogleVerifier struct{ v *oidc.IDTokenVerifier }

func NewGoogleVerifier(hc *http.Client) *GoogleVerifier
// Verify returns the email claim when the token is valid for aud.
func (g *GoogleVerifier) Verify(ctx context.Context, raw, aud string, now func() time.Time) (email string, err error)
```

- **The keys:** `oidc.NewRemoteKeySet(oidc.ClientContext(ctx, hc), "https://www.googleapis.com/oauth2/v3/certs")`.
  go-oidc caches the keys and refetches on an unknown key id.
- **The verifier:** `oidc.NewVerifier("https://accounts.google.com", keySet, &oidc.Config{SkipClientIDCheck: true, SkipIssuerCheck: true, Now: now})`.
- **Checked by hand:**
  - `iss` is `https://accounts.google.com` or `accounts.google.com` (Google
    issues both);
  - `aud` contains the expected audience;
  - `email_verified` is true.

  Siphon then returns `email`.
- **One verifier per daemon,** created in `internal/job` with
  `source.HTTPClient(false, 10*time.Second, 1<<20)`: public hosts only,
  and no redirects. It's shared by every google-oidc source.

### Handler (`internal/source/webhook.go`)

`WebhookOptions` gains `Audience`, `ServiceAccount` and
`Google *GoogleVerifier`, set in `hooks.go`. The audience defaults to
`server.public_url` + `/hook/<name>`.

**A new case, `"google-oidc"`:**
- **The header:** `Authorization` must start with `Bearer `. The verify
  call runs with a 5-second context.
- **Checks:** the verified email must equal `ServiceAccount` (an exact,
  case-insensitive match).
- **Any failure** gets `reject(401, "token invalid")`, with a fixed
  reason. The token is never logged.
- **`header = "Authorization"`,** so it's dropped from the event headers.

**`format: pubsub`** (a `servePubSub` after verification, like the other
formats):
- **The envelope:** decode
  `{"message":{"data","attributes","messageId","publishTime"},"subscription"}`.
  Missing `message`, or a missing `messageId`, gets 400.
- **`data`:** base64 standard decoding (an error gets 400). If the result
  is valid JSON, it's the parsed value; otherwise a UTF-8 string.
- **The event:** `{data, attributes, message_id, publish_time,
  subscription}`.
- **Delivery:** run `Limit.Allow` once (429), then `deliver` keyed by
  `messageId` (500 on error). Answer `204`; a duplicate also gets 204.

**`google-oidc` without the format** delivers the raw JSON body as today,
with a body-hash key and 202, for other Google OIDC push senders.

### Catalog (`internal/catalog/services.yaml`)

`google-pubsub` replaces the not-yet line (`:930`).
- **Fields:**
  - `name`;
  - `service_account` (required, an email);
  - `audience` (optional; help: "leave empty to use this hook's URL, which
    is Pub/Sub's default").
- **Creates `{{.name}}-hooks`:** `hook: true`, `type: webhook`,
  `signature: google-oidc`, `format: pubsub`, `service_account`, and
  `audience` when set.
- **Setup:**
  ```sh
  gcloud pubsub subscriptions create <sub> --topic=<topic> \
    --push-endpoint=<the URL above> \
    --push-auth-service-account=<service_account>
  ```
  It also notes that `server.public_url` must be set, and that the service
  account needs the `iam.serviceAccountTokenCreator` role granted to the
  Pub/Sub service agent, per Google's docs.

### Template: `docs/templates/gcp-cloud-build-failed.yaml`

"Ops & monitoring":
- the rule on `google-pubsub-hooks`, with
  `when: 'event.data.status in ["FAILURE", "TIMEOUT", "INTERNAL_ERROR"]'`,
  `on: each`, `id: event.message_id`;
- an ntfy notification: "Cloud Build {{.event.data.id}} {{.event.data.status}}".

### Docs

The webhook docs gain the `google-oidc` and `pubsub` rows, `docs/llm.md`
gets a line, and `go generate ./docs`.

## Alternatives rejected

- **A hand-written JWT and JWKS parser.** go-oidc is already a dependency,
  and it's audited.
- **A shared token in the push URL** (`?token=`). Secrets in URLs end up in
  logs, and Google's OIDC auth is the supported way.
- **Not requiring the service account.** Any Google-issued token for the
  same audience would pass. That's the owner's decision: required.

## Risks

- **Google's certs endpoint being slow or down** blocks verification. The
  5-second timeout gives 401, and Pub/Sub retries later. go-oidc's key
  cache keeps working while it's down.
- **The audience must match exactly what the subscription uses.** If
  `public_url` changes, the subscription has to be updated, or a custom
  audience used. The docs say so.
- **Large messages:** Pub/Sub messages are up to 10 MB, and the source's
  `MaxBody` applies (413). The docs mention `limits.http_max_body`.

## Verification

- **`internal/source` tests:** a test RSA key and a local JWKS `httptest`
  server, through a test-only constructor that takes the certs URL. Cover:
  - a valid token with the right `aud` and email, which delivers the
    decoded event, keyed by `messageId`, and answers 204;
  - a duplicate gets 204 and is delivered once;
  - wrong `aud`, wrong email, `email_verified` false, expired, a bad
    signature, an unknown issuer, or no bearer all get 401;
  - base64 `data` that's JSON becomes fields, and non-JSON stays a string;
  - a bad envelope gets 400;
  - `google-oidc` without the format gets 202 with the raw body.
- **`internal/config`** cases, including the `public_url` requirement when
  `audience` is empty.
- **`internal/catalog`** render; `TestTemplatesValidate`; the docs tests;
  `go vet ./... && go test -race ./...`.
- **Manual:** the same signed-token flow with curl against a throwaway
  Siphon pointed at a local JWKS. Real Google isn't needed.
