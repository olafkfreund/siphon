# An agent that reviews pull requests

**Goal:** when a pull request opens, an AI agent reads it through GitHub's
tools and reports findings. You approve each run.

## CLI

```sh
siphon connect github --token @github.token --webhook     # tools + events; add the webhook in GitHub
siphon connect login claude --setup-token -              # or: siphon connect model ollama
siphon template github-pr-review > pr.yaml
siphon apply -f pr.yaml --dry-run && siphon apply -f pr.yaml --yes
```

Open a pull request, then:

```sh
siphon get approvals            # the run waits for you
siphon approve <job>
siphon jobs show <job>          # the agent's findings
```

## What keeps it safe

- **Tools:** the agent sees only `mcp: [github]` and the exact tools in
  `allowed_tools`, which are read-only in the template.
- **Limits:** `max_turns`, `max_budget_usd` and `timeout` cap each run.
- **Approval:** `approve` defaults to true for agents. `cooldown` is
  required, so a flood of events can't start a flood of agents.
- **Network:** the agent's network is limited to its model and GitHub,
  through Siphon's egress proxy.

## Variations

- `github-ci-failure`: diagnose failed Actions runs.
- `github-issue-triage`: label suggestions on a free local model.
- `gitlab-mr-review`: the GitLab equivalent.

## Verify

- **Portal:** **Approvals**, then **Jobs**.
- `siphon why review-new-prs` if a PR didn't trigger a run. Draft PRs are
  skipped on purpose.
