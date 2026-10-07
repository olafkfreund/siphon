# GitHub

One command gives your agents GitHub tools (through GitHub's MCP server),
and optionally a webhook for pull request, issue and CI events.

## CLI

1. Create a **fine-grained personal access token** for only the repositories
   agents need: Contents, Pull requests and Issues set to **read**, Metadata
   set to read. Add write only if agents should comment.
2. Connect:

```sh
siphon connect github --name github --token @github.token --webhook
```

```text
connected GitHub: "github"
webhook source "github-hooks"
  URL:    https://siphon.example.com/hook/github-hooks
  secret: 3f9a…
          (shown once, copy it now)
test: ok (octocat, 120ms)
```

3. In GitHub, go to **Repository → Settings → Webhooks → Add webhook**. Set
   the **Payload URL** to the one shown, the content type to
   `application/json`, and the **Secret** to the one shown. Choose the
   events your tasks need (Pull requests, Issues, Workflow runs).

**`--mode`:**
- `remote` (the default) uses GitHub's hosted MCP server.
- `local` runs the Nix-pinned `github-mcp-server` in its own sandbox,
  which is needed for GitHub Enterprise. The token reaches only that
  server, never the agent.

## Webhooks from the internet

GitHub must reach Siphon over HTTPS, usually through your reverse proxy.
Set `server.public_url` in `siphon.yaml` so Siphon shows full URLs. To test
locally without exposing anything:

```sh
gh webhook forward --repo you/repo --events pull_request --url http://127.0.0.1:8080/hook/github-hooks --secret <the secret>
```

## Tasks that use it

`github-pr-review`, `github-ci-failure`, `github-issue-triage`, from
`siphon template`.

Agents name the source in `mcp: [github]`, and list exact tools in
`allowed_tools`. Read-only tools to start from:
`mcp__github__get_pull_request`, `mcp__github__get_pull_request_diff`,
`mcp__github__get_pull_request_files`, `mcp__github__get_issue`,
`mcp__github__get_workflow_run`, `mcp__github__list_workflow_jobs`,
`mcp__github__get_job_logs`.

## Verify in the portal

**Services** lists `github` with a **Test** button. **Sources** shows
`github-hooks` and when it last received an event.

## YAML (siphon.yaml)

```yaml
sources:
  github:
    type: mcp
    url: https://api.githubcopilot.com/mcp/
    auth: { bearer: file:/run/credentials/siphon.service/github-token }
  github-hooks:
    type: webhook
    signature: github
    secret: file:/run/credentials/siphon.service/github-webhook
```
