# Logins: Claude, Codex and agy subscriptions, and API keys

Agents of `kind: claude`, `codex` or `agy` run the real vendor CLI in
Siphon's sandbox, on a login that **Siphon owns**: a separate copy, never
your own session.

## CLI

```sh
# Claude (recommended): a long-lived setup token, separate from your own login.
# Run `claude setup-token` in your own terminal, then paste the token:
siphon connect login claude --name claude-max --setup-token -

# Codex: import the login file the Codex CLI wrote after `codex login`
siphon connect login codex --name chatgpt --file ~/.codex/auth.json

# Any of them with a pay-as-you-go API key instead of a subscription:
siphon connect login claude --name claude-api --api-key @anthropic.key
```

Pass exactly one of `--setup-token`, `--file` or `--api-key`. Secrets are
only read from `-` (stdin) or `@file`, never from a plain flag value. After
connecting, Siphon shows the stored status and expiry:

```sh
siphon get connections
```

## Good to know

- **Use a setup token for Claude.** Importing your own login file means two
  places refreshing the same session, and one of them will get logged out.
- **Siphon refreshes and writes back logins itself.** Each login runs one
  job at a time by default (`concurrency: 1`), so two runs never fight
  over a refresh.
- **The login lives inside the run's sandbox,** never in the agent's prompt
  or tools.

## Using it

```yaml
agents:
  pr-reviewer:
    kind: claude
    credential: claude-max
    prompt: "…"
```

Templates that use it: `github-pr-review`, `github-ci-failure`,
`mcp-task-triage`, `aws-cloudwatch-alarm`.

## Verify in the portal

**Connections → Subscriptions** shows each login: its status, expiry, last
write-back, and a **Connect** form (the same choices as the CLI).

## YAML (siphon.yaml)

```yaml
credentials:
  claude-max: { provider: claude }   # then import the login with the CLI or portal
  chatgpt:    { provider: codex }
```

On the host itself, `siphon credentials import -config … <name>` imports a
login file directly. It's the local equivalent.
