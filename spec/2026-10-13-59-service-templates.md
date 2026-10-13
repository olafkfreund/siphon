---
status: draft
issue: 59
intent: intent/2026-10-13-59-service-templates.md
---

# Spec: templates for the catalog services that have none

## Design

Five new files in `docs/templates/`, one per service. Each is a complete
apply file with the usual header (`title`, `category`, `needs`, `apply`, and
`secrets` where needed), written in the style of the existing templates. The
source names are the ones the catalog creates under its default names
(`internal/catalog/services.yaml`): `<name>` for the MCP source and
`<name>-hooks` for the webhook source. Datadog's only source is `datadog`.

Header names are matched in lower case, as in `github-pr-review.yaml`.

### 1. `bitbucket-pr-review.yaml` (Code review & CI)

- **Needs:** `sources/bitbucket`, `sources/bitbucket-hooks`
  (`siphon connect bitbucket --webhook`; tick "Pull request: Created"),
  and `credentials/claude-max`.
- **Rule `review-new-prs`:** source `bitbucket-hooks`,
  `when: 'headers["x-event-key"] == "pullrequest:created"'`.
- **Dedupe:** `id: event.repository.full_name + "/" + string(event.pullrequest.id)`.
  Bitbucket's `pullrequest.id` is numbered per repository, so it isn't
  unique on its own.
- **Action:** agent `pr-reviewer`: `kind: claude`, `mcp: [bitbucket]`,
  `allowed_tools: [mcp__bitbucket__bitbucketPullRequest, mcp__bitbucket__bitbucketRepoContent]`,
  `max_turns: 15`, `max_budget_usd: 1`, `timeout: 15m`. Approval stays on
  (the default). The prompt says never to merge, approve or comment.
- **Cooldown:** `1m`.

### 2. `gitea-pr-review.yaml` (Code review & CI)

- **Needs:** `sources/gitea`, `sources/gitea-hooks`
  (`siphon connect gitea --webhook`; `services.siphon.catalogPackages = [ "gitea" ]`),
  and `credentials/claude-max`.
- **Rule `review-new-prs`:** source `gitea-hooks`,
  `when: 'headers["x-gitea-event"] == "pull_request" && event.action in ["opened", "reopened"]'`.
- **Dedupe:** `id: event.pull_request.id`.
- **Action:** agent `pr-reviewer`: `kind: claude`, `mcp: [gitea]`,
  `allowed_tools: [mcp__gitea__pull_request_read, mcp__gitea__get_file_contents]`.
  The catalog already starts `gitea-mcp` with `-r` (read-only), which hides
  every write tool, so naming the two tools is defence in depth. Approval
  stays on.
- **Cooldown:** `1m`.

### 3. `forgejo-pr-review.yaml` (Code review & CI)

- **Needs:** `sources/forgejo`, `sources/forgejo-hooks`
  (`siphon connect forgejo --webhook`; `services.siphon.catalogPackages = [ "forgejo" ]`),
  and `credentials/claude-max`.
- **Rule `review-new-prs`:** source `forgejo-hooks`,
  `when: 'headers["x-forgejo-event"] == "pull_request" && event.action in ["opened", "reopened"]'`.
- **Dedupe:** `id: event.pull_request.id`.
- **Action:** agent `pr-reviewer`: `kind: claude`, `mcp: [forgejo]`,
  `allowed_tools: [mcp__forgejo__get_pull_request_by_index, mcp__forgejo__get_pull_request_diff, mcp__forgejo__list_pull_request_files]`.
  `forgejo-mcp` has no read-only mode, so this list is the only tool
  boundary, together with the read-only token the catalog asks for.
  Approval stays on.
- **Cooldown:** `1m`.

### 4. `datadog-monitor-alert.yaml` (Ops & monitoring)

- **Needs:** `sources/datadog` (`siphon connect datadog`) and
  `credentials/ollama-local`.
- **Datadog payload:** Datadog sends whatever payload the user defines, so
  the header gives one to paste into the webhook's Payload field:
  `{"id":"$ID","title":"$EVENT_TITLE","transition":"$ALERT_TRANSITION","monitor":"$ALERT_ID","link":"$LINK","body":"$EVENT_MSG"}`.
- **Rule `monitor-triggered`:** source `datadog`,
  `when: 'event.transition in ["Triggered", "Re-Triggered"]'`.
- **Dedupe:** `id: event.id`.
- **Action:** routine `alert-to-phone`. Step one is agent `monitor-summary`
  (`kind: model`, `llama3.2`, no tools, `approve: false`, because it is a
  read-only summary). Step two is the ntfy `curl`. Both are copied from
  `alertmanager-summary.yaml`.
- **Cooldown:** `2m`.

### 5. `cloudflare-notification.yaml` (Ops & monitoring)

- **Needs:** `sources/cloudflare`, `sources/cloudflare-hooks`
  (`siphon connect cloudflare --webhook`) and `credentials/claude-max`.
- **Rule `cloudflare-alert`:** source `cloudflare-hooks`,
  `when: 'event.alert_event != "ALERT_STATE_EVENT_END"'`. This drops the
  "resolved" half of start/end pairs. Alert types without `alert_event`
  still match.
- **Dedupe:** `id: event.policy_id + "/" + string(event.ts)`.
- **Action:** routine `explain-and-notify`. Step one is agent
  `cf-explainer` (`kind: claude`, `mcp: [cloudflare]`,
  `allowed_tools: [mcp__cloudflare__search, mcp__cloudflare__execute]`,
  `max_turns: 15`, `max_budget_usd: 1`, `timeout: 15m`). The prompt is
  built from `event.name`, `event.alert_type` and `event.text`, and tells
  the agent to make read-only API calls only. Approval stays on. Step two
  is the ntfy `curl` with the agent's output.
- **Cooldown:** `10m`.

### Supporting changes

- **`docs/templates/README.md`:** three rows go under "Code review & CI"
  (bitbucket, forgejo, gitea) and two under "Ops & monitoring" (cloudflare,
  datadog), in alphabetical order. "Connect first" is the `siphon connect`
  command, plus `siphon connect login claude` or
  `siphon connect model ollama`.
- **`cmd/siphon/describe_test.go` `baseConfig`:** add the sources the new
  templates need, so `TestTemplatesValidate` dry-runs them:
  - `bitbucket`, `gitea`, `forgejo` and `cloudflare` as URL MCP sources
    (only the names matter here);
  - `bitbucket-hooks`, `gitea-hooks`, `forgejo-hooks`, `cloudflare-hooks`
    and `datadog` as webhook sources, with the catalog's signature settings.
- **Nothing else changes:** no catalog entries, no categories, no code.

## Alternatives rejected

- **A model agent with no tools for the forges** (like
  `gitlab-mr-review`): a PR webhook carries no diff, so the review could
  only guess from the title. A forge connection already brings its MCP
  tools, so use them.
- **One template for both Gitea and Forgejo:** decided at intent approval.
  Two templates, so each is found by name.
- **`allowed_tools: [mcp__forgejo]` (the whole server):** it would allow
  `merge_pull_request` and other writes, because `forgejo-mcp` has no
  read-only mode.
- **A Datadog agent with Datadog tools:** the catalog marks Datadog's MCP
  URL as unverified and connects webhooks only.

## Risks

- **Bitbucket can't be held to read-only by tool name.** Rovo exposes one
  `bitbucketPullRequest` tool whose actions include merge, approve and
  comment ([Atlassian blog][bb-mcp]). Read-only depends on the token's scopes
  (the catalog asks for read) and on approval being on. The template header
  says so.
- **Cloudflare's `execute` tool can write.** The hosted server is "Code
  Mode", with two tools, `search` and `execute`. Read-only depends on the
  API token (the catalog asks for read) and on approval. The tool names come
  from third-party listings and aren't verified against the live server.
- **Unverified payload fields:**
  - Bitbucket `pullrequest.id` and `repository.full_name`
    ([Hookdeck][bb-hd], secondary source);
  - Gitea `pull_request.id` (Gitea's docs list `action`, `number`,
    `pull_request` and `repository`, but not the fields inside them
    ([Gitea webhooks][gitea-wh]));
  - Cloudflare `policy_id` and `ts` ([Cloudflare payload schema][cf-wh]).

  A wrong field makes the dedupe id fail to evaluate. `siphon test --last`
  shows it at the first real event.
- **Forgejo signatures:** the catalog's Forgejo hook uses
  `signature: github`. Forgejo's docs only show `X-Forgejo-Signature`
  ([Forgejo webhooks][fj-wh]). If Forgejo doesn't also send
  `X-Hub-Signature-256`, every Forgejo webhook is rejected. That is a
  catalog question outside this task, so I'm raising it here rather than
  fixing it.

## Verification

- `go test ./docs ./cmd/siphon ./internal/web`: `TestTemplatesParse`
  (header, category, order), `TestTemplatesValidate` (each template passes
  `apply --dry-run` against `baseConfig`), and the gallery test (one card
  per template, CSP-clean).
- `go run ./cmd/siphon template` lists the five under the right categories.
- `go vet ./... && go test -race ./...` is green.

### How the facts were checked

| Fact | Source |
|---|---|
| Gitea tool names and `-r` | gitea-mcp README ([gitea.com][gitea-mcp]) |
| Forgejo tool names | `forgejo-mcp --cli list`, run from nixpkgs (forgejo-mcp 3.2.0, 2026-10-13) |
| Gitea and Forgejo headers | [Gitea webhooks][gitea-wh], [Forgejo webhooks][fj-wh] |
| Datadog variables | [Datadog webhooks][dd-wh] |
| Cloudflare payload and `cf-webhook-auth` | [Cloudflare payload schema][cf-wh] |
| Cloudflare MCP tools | third-party listings only (unverified, see Risks) |

[bb-mcp]: https://www.atlassian.com/blog/bitbucket/the-atlassian-rovo-mcp-server-now-supports-bitbucket-cloud
[bb-hd]: https://hookdeck.com/webhooks/platforms/guide-to-bitbucket-webhooks-features-and-best-practices
[gitea-wh]: https://docs.gitea.com/usage/webhooks
[gitea-mcp]: https://gitea.com/gitea/gitea-mcp
[fj-wh]: https://forgejo.org/docs/latest/user/webhooks/
[dd-wh]: https://docs.datadoghq.com/integrations/webhooks/
[cf-wh]: https://developers.cloudflare.com/notifications/reference/webhook-payload-schema

## Decisions at approval

- **Bitbucket and Cloudflare keep their agent, with `approve: true`.**
  Neither can be limited to read-only by tool name (Rovo's
  `bitbucketPullRequest` can merge; Cloudflare's `execute` can write). Each
  template's header says so in one line, and tells the user to give the
  connection a read-only token (Bitbucket: `pullrequest:read`,
  `repository:read`; Cloudflare: a read-only API token).
- **The Forgejo `signature: github` question is out of scope here.**
  Implementation checks it against a real Forgejo (or the source) and opens
  a separate issue if Forgejo doesn't send `X-Hub-Signature-256`.
