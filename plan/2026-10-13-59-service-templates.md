---
status: draft
issue: 59
spec: spec/2026-10-13-59-service-templates.md
---

# Plan: templates for the catalog services that have none

## Approved decisions

These are copied from the spec and its "Decisions at approval". Nothing
else needs to be opened to implement this plan.

### Five new templates

All five go in `docs/templates/`. Each is a complete apply file. Its header
gives `# title:`, `# category:`, `# needs:` (each a `kind/name` ref, with the
connect command in parentheses), `# secrets:` where needed, and
`# apply:    siphon apply -f <name>.yaml`. Model each one on its sibling:
`github-pr-review.yaml` for the agents, `alertmanager-summary.yaml` for the
model, routine and ntfy pattern.

- **Source names:** the catalog defaults (`internal/catalog/services.yaml`):
  `<name>` is the MCP source and `<name>-hooks` is the webhook source.
  Datadog has only `datadog`.
- **Headers in `when:`:** matched in lower case.
- **Routine notify steps:** reuse the existing ntfy `curl`, writing
  `https://ntfy.sh/siphon-CHANGE-ME` as in the other templates.

| File | Category | Needs | Rule `when:` | `id:` | Action | approve | cooldown |
|---|---|---|---|---|---|---|---|
| `bitbucket-pr-review` | Code review & CI | `sources/bitbucket`, `sources/bitbucket-hooks` (`siphon connect bitbucket --webhook`; tick "Pull request: Created"), `credentials/claude-max` | `headers["x-event-key"] == "pullrequest:created"` | `event.repository.full_name + "/" + string(event.pullrequest.id)` | agent `pr-reviewer` | true (default) | `1m` |
| `gitea-pr-review` | Code review & CI | `sources/gitea`, `sources/gitea-hooks` (`siphon connect gitea --webhook`; `services.siphon.catalogPackages = [ "gitea" ]`), `credentials/claude-max` | `headers["x-gitea-event"] == "pull_request" && event.action in ["opened", "reopened"]` | `event.pull_request.id` | agent `pr-reviewer` | true | `1m` |
| `forgejo-pr-review` | Code review & CI | `sources/forgejo`, `sources/forgejo-hooks` (`siphon connect forgejo --webhook`; `services.siphon.catalogPackages = [ "forgejo" ]`), `credentials/claude-max` | `headers["x-forgejo-event"] == "pull_request" && event.action in ["opened", "reopened"]` | `event.pull_request.id` | agent `pr-reviewer` | true | `1m` |
| `datadog-monitor-alert` | Ops & monitoring | `sources/datadog` (`siphon connect datadog`), `credentials/ollama-local` | `event.transition in ["Triggered", "Re-Triggered"]` | `event.id` | routine `alert-to-phone` (model summary, then ntfy) | false (read-only summary) | `2m` |
| `cloudflare-notification` | Ops & monitoring | `sources/cloudflare`, `sources/cloudflare-hooks` (`siphon connect cloudflare --webhook`), `credentials/claude-max` | `event.alert_event != "ALERT_STATE_EVENT_END"` | `event.policy_id + "/" + string(event.ts)` | routine `explain-and-notify` (agent, then ntfy) | true | `10m` |

### The agents

- **Bitbucket:** `kind: claude`, `credential: claude-max`, `mcp: [bitbucket]`,
  `allowed_tools: [mcp__bitbucket__bitbucketPullRequest, mcp__bitbucket__bitbucketRepoContent]`,
  `max_turns: 15`, `max_budget_usd: 1`, `timeout: 15m`. The prompt says
  never to merge, approve or comment.
- **Gitea:** the same shape, with `mcp: [gitea]` and
  `allowed_tools: [mcp__gitea__pull_request_read, mcp__gitea__get_file_contents]`.
  The catalog already runs `gitea-mcp -r` (read-only).
- **Forgejo:** the same shape, with `mcp: [forgejo]` and
  `allowed_tools: [mcp__forgejo__get_pull_request_by_index, mcp__forgejo__get_pull_request_diff, mcp__forgejo__list_pull_request_files]`.
  `forgejo-mcp` has no read-only mode, so this list is the boundary.
- **Datadog:** `kind: model`, `credential: ollama-local`, `model: llama3.2`,
  no tools, `max_turns: 2`, `timeout: 5m`, `approve: false`.
  - The header gives the payload to paste into Datadog's webhook Payload
    field:
    `{"id":"$ID","title":"$EVENT_TITLE","transition":"$ALERT_TRANSITION","monitor":"$ALERT_ID","link":"$LINK","body":"$EVENT_MSG"}`.
  - The header also says to mention `@webhook-<name>` in the monitor.
- **Cloudflare:** `kind: claude`, `mcp: [cloudflare]`,
  `allowed_tools: [mcp__cloudflare__search, mcp__cloudflare__execute]`,
  `max_turns: 15`, `max_budget_usd: 1`, `timeout: 15m`. The prompt uses
  `event.name`, `event.alert_type` and `event.text`, and says to make
  read-only API calls only.

### Warnings in the Bitbucket and Cloudflare headers

These two keep their agent with `approve: true`, because neither can be
held to read-only by tool name: Rovo's `bitbucketPullRequest` can merge, and
Cloudflare's `execute` can write. Each header adds one warning line and a
read-only token hint:

- Bitbucket: scopes `pullrequest:read` and `repository:read`;
- Cloudflare: a read-only API token.

### Supporting changes

- **README:** rows in `docs/templates/README.md`, in alphabetical order:
  - under "Code review & CI": bitbucket, forgejo, gitea;
  - under "Ops & monitoring": cloudflare, datadog.
- **Test fixture:** add the sources each template needs to `baseConfig` in
  `cmd/siphon/describe_test.go`.
- **Unchanged:** the catalog, the categories and all code.

### Out of scope

The Forgejo `signature: github` question. Step 1 checks it, and if Forgejo
doesn't send `X-Hub-Signature-256`, a separate issue is opened. It is not
fixed here.

## Steps

1. **Check the unverified facts (no file edits unless a fact is wrong).**
   - Check these against vendor docs, a vendor payload sample, or the
     server's own source:
     - Bitbucket `pullrequest.id` and `repository.full_name` in
       `pullrequest:created`;
     - Gitea `pull_request.id` in `pull_request`;
     - Cloudflare `policy_id` and `ts`;
     - Gitea tool names `pull_request_read` and `get_file_contents` (e.g.
       start `nix run nixpkgs#gitea-mcp-server -- -t stdio -r` with dummy
       env and send `tools/list`, or read the pinned source);
     - Cloudflare's `search`/`execute` tool names, if the docs allow it;
     - whether Forgejo sends `X-Hub-Signature-256` (from the Forgejo
       source).
   - **If a fact is wrong:** correct the "Approved decisions" table in this
     plan, in a `docs(plan): …` commit, before writing the template.
   - **Forgejo signature:** if Forgejo lacks the header, open an issue and
     link it here.
   - Record each result in "Deviations" below.
   - Verify: each fact has a source written in this plan.
   - Traps: none.

2. **`docs/templates/bitbucket-pr-review.yaml` (new).**
   - Write the template from the table and the agent notes, with the header
     warning and token hint.
   - `cmd/siphon/describe_test.go` (`baseConfig`, the `sources:` block,
     lines 21–37, before `rules:`): add
     - `bitbucket: { type: mcp, url: "https://mcp.atlassian.com/v2/mcp", headers: { Authorization: env:AGW_HOOK } }`
     - `bitbucket-hooks: { type: webhook, signature: sha256, signature_header: X-Hub-Signature, secret: env:AGW_HOOK }`
   - Verify:
     ```sh
     go generate ./docs && go test ./docs ./cmd/siphon -run 'Template' && go test ./internal/web -run Gallery
     ```
   - Traps:
     - `cooldown:` on the rule (mandatory for agent actions);
     - no secret values;
     - `llms-full.txt` and `docs/connections/README.md` are generated, so
       run `go generate ./docs` and commit them;
     - `needs` refs contain no `() ,`.

3. **`docs/templates/gitea-pr-review.yaml` (new).**
   - Write it as in step 2, with the Gitea tools.
   - `baseConfig`: add
     - `gitea: { type: mcp, url: "https://gitea.example.com/mcp", headers: { Authorization: env:AGW_HOOK } }`
     - `gitea-hooks: { type: webhook, signature: github, secret: env:AGW_HOOK }`
   - Verify: the step 2 command.
   - Traps: the same as step 2.

4. **`docs/templates/forgejo-pr-review.yaml` (new).**
   - Write it as in step 2, with the Forgejo tools.
   - `baseConfig`: add
     - `forgejo: { type: mcp, url: "https://forgejo.example.com/mcp", headers: { Authorization: env:AGW_HOOK } }`
     - `forgejo-hooks: { type: webhook, signature: github, secret: env:AGW_HOOK }`
   - Verify: the step 2 command.
   - Traps: the same as step 2.

5. **`docs/templates/datadog-monitor-alert.yaml` (new).**
   - Write the model agent and the routine `alert-to-phone`. Copy the shape
     from `alertmanager-summary.yaml`, without its own `sources:` block,
     because the catalog creates `datadog`.
   - `baseConfig`: add
     `datadog: { type: webhook, signature: token, token_header: X-Siphon-Key, secret: env:AGW_HOOK }`.
   - Verify: the step 2 command.
   - Traps:
     - `cooldown:` is required, because a routine that holds an agent
       counts as an agent action;
     - the generated files, as in step 2.

6. **`docs/templates/cloudflare-notification.yaml` (new).**
   - Write the agent and the routine `explain-and-notify`, with the header
     warning and token hint.
   - `baseConfig`: add
     - `cloudflare: { type: mcp, url: "https://mcp.cloudflare.com/mcp", auth: { bearer: env:AGW_HOOK } }`
     - `cloudflare-hooks: { type: webhook, signature: token, token_header: cf-webhook-auth, secret: env:AGW_HOOK }`
   - Verify: the step 2 command.
   - Traps: the same as step 5.

7. **`docs/templates/README.md`: the five rows.**
   - "Code review & CI" (after line 20, alphabetical): `bitbucket-pr-review`,
     `forgejo-pr-review`, `gitea-pr-review`.
   - "Ops & monitoring" (after line 31, alphabetical):
     `cloudflare-notification`, `datadog-monitor-alert`.
   - "Connect first" is the connect command, plus
     `siphon connect login claude` or `siphon connect model ollama`.
   - Then run `go generate ./docs` (it embeds `templates/README.md` in
     `llms-full.txt`).
   - Verify:
     ```sh
     go test ./docs && go run ./cmd/siphon template | grep -E 'bitbucket|gitea|forgejo|datadog|cloudflare'
     ```
     All five should be listed under the right categories.
   - Traps: the generated files.

8. **The full run.**
   - Verify:
     ```sh
     go vet ./... && go test -race ./...
     ```
     It should be green.
   - Traps: none (no Go code and no `go.mod` changes, so no `vendorHash`).

## Deviations

None yet. Step 1 records its findings here.

## Tests

```sh
go generate ./docs && git diff --exit-code llms-full.txt docs/connections/README.md   # clean after commit
go vet ./... && go test -race ./...
```

## Rollback

Revert the branch's commits. Nothing but `docs/templates/`, the generated
docs and the test fixture changes, and no stored state is touched.
