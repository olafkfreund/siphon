# Azure DevOps

Azure DevOps events (builds, releases, work items, pull requests) reach Siphon
as **service hooks**. Agents can also read builds, work items, pull requests
and code through Microsoft's local MCP server: see [Agent tools](#agent-tools).

## Webhooks

```sh
siphon connect azure-devops
```

It creates the webhook source `azure-devops-hooks` and shows the URL and the
secret once. In *Project settings → Service hooks → + → Web Hooks*, choose the
event, set the URL, add the HTTP header `X-Siphon-Key:<the secret>`, leave Basic
authentication empty, and send *Resource details: All*. `server.public_url` must be
set. The catalog's setup text has the full steps.

## Agent tools

Give `siphon connect` the organization and a personal access token (PAT) and
Siphon adds an `azure-devops` MCP source next to the webhook. It runs the
pinned `@azure-devops/mcp` server (v2.10.0) from a Nix package; there is no
`npx` at run time.

### The PAT

Create it in *User settings → Personal access tokens*. Scopes:

- **Work Items: Read**, **Build: Read**, **Code: Read** for the read-only tools;
- **Read & write** on a scope only if agents may use the write tools for it.

A PAT expires (30 days by default, one year at most). Renew it before then:
`connect` again with the new one. The agent acts as the PAT's owner.

```sh
printf '%s' "$AZURE_DEVOPS_PAT" | siphon connect azure-devops \
  --organization <org> --pat -
```

After saving, the connection's **Test** shows the PAT owner's display name.

### Installing the server

On NixOS:

```nix
services.siphon.catalogPackages = [ "azure-devops" ];
```

Elsewhere, add `azure-devops` to `server.mcp_packages`, with `command` set to
`siphon-azure-devops-mcp` from this flake's `siphon-azure-devops-mcp` package, `env`
`[AZURE_DEVOPS_ORG, AZURE_DEVOPS_PAT]` and `hosts`
`[dev.azure.com, vssps.dev.azure.com, almsearch.dev.azure.com]`.

The wrapper reads the two variables, passes the PAT to the server through its
environment only (never in the command line), and starts it in PAT mode.

### The tools

Agents list the source in `mcp:`; the tools are named
`mcp__azure-devops__<tool>`. The server has 40 tools. The groups:

| Tools | Does | Writes |
|---|---|---|
| `pipelines_build`, `pipelines_build_log`, `pipelines_run`, `pipelines_definition`, `pipelines_artifact` | builds, logs, runs, definitions, artifacts | no |
| `wit_work_item`, `wit_query`, `wit_backlog`, `wit_work_item_attachment` | work items, queries, backlogs, attachment download (with `savePath` it writes a file on the Siphon host, not in Azure DevOps) | no |
| `repo_repository`, `repo_pull_request`, `repo_pull_request_thread`, `repo_branch`, `repo_file`, `repo_search_commits` | repositories, pull requests, files, commits | no |
| `search_code`, `search_workitem`, `search_wiki`, `wiki` | search, wikis | no |
| `core_list_projects`, `core_list_project_teams`, `core_get_identity_ids`, `work` | projects, teams, iterations | no |
| `testplan`, `testplan_show_test_results_from_build_id`, `advsec_get_alerts`, `advsec_get_alert_details` | test plans and security alerts | no |
| `pipelines_write`, `work_iteration_write`, `work_capacity_write`, `repo_pull_request_write`, `repo_pull_request_thread_write`, `wit_work_item_write`, `wit_work_item_comment_write`, `wit_work_item_link_write`, `wiki_upsert_page`, `testplan_test_plan_write`, `testplan_test_suite_write`, `testplan_test_case_write` | change things | **yes** |
| `repo_create_branch` | create a branch | **yes** |


The ready-made template lists only read tools. Approval gates the whole run:
an agent action waits for a human before it starts (the default), and the
approver does not see what the agent will write. Keep writing agents on the
default `approve`, and limit them with `allowed_tools`.

```yaml
agents:
  build-reader:
    kind: claude
    credential: claude-max
    mcp: [azure-devops]
    allowed_tools: [mcp__azure-devops__pipelines_build, mcp__azure-devops__pipelines_build_log]
    prompt: "Why did build {{.event.resource.buildNumber}} fail?"
```

Ready-made: `siphon template azure-devops-build-triage`.

### Proxies

The server reaches Azure DevOps through Siphon's egress proxy:
- **The API calls** go through `azure-devops-node-api` and
  `typed-rest-client`, which honour `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY`.
- **The search, wiki and some write tools** use Node's built-in `fetch`.
  Node only sends that through the proxy when `NODE_USE_ENV_PROXY=1` is
  set, and the wrapper sets it.

Both paths were checked against a test proxy. It saw CONNECTs to
`dev.azure.com`, `vssps.dev.azure.com` and `almsearch.dev.azure.com`, which
are the three hosts the package allows.

### Check it live

1. `siphon connect azure-devops --organization <org> --pat -`
2. `siphon test service azure-devops` shows the PAT owner.
3. Run an agent that calls one read tool, such as `pipelines_build`.

### The remote server

Microsoft also runs a hosted MCP server with Entra sign-in. Siphon can't use it
yet: [issue 107](https://github.com/olafkfreund/siphon/issues/107).
