# Watch an MCP server

**Goal:** poll a resource (or one read-only tool) on an MCP server, and act
on what it returns: a build turning red, tasks failing.

## CLI

```sh
siphon template mcp-build-watch > build.yaml     # a single status
siphon template mcp-task-triage > tasks.yaml     # a list: one fire per failed item
siphon apply -f build.yaml --yes
```

- `read: { resource: "ci://builds/main" }` (or `read: { tool: name, args: {…} }`)
  is **exactly** what Siphon calls. Nothing else on that server is touched.
- **A list:** `for_each: event.tasks` with `id: item.id` turns one poll
  into one fire per item, and rules see each one as `item`.
- **An MCP source without `read`** is never polled. It only gives agents
  tools (that's how the GitHub and AWS servers are used).

MCP servers that need a secret (a token in their environment) run in their
own sandboxed bridge, so the agent never sees the secret. For a
stdio command, see [the README](../../README.md#mcp-servers-with-secrets):
those are operator-only.

## Verify

`siphon test main-broke --last`, the portal's **Sources** (last poll, last
error), and `siphon why main-broke`.
