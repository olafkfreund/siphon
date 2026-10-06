# agentgw

Self-hosted gateway that watches MCP servers, REST APIs and webhooks, evaluates
rules, and on a match runs an AI agent, a routine or a specific command.

Status: under construction. See `intent/`, `spec/` and `plan/` for the design.

```sh
nix develop -c go test ./...
nix build && ./result/bin/agentgw version
```

Licence: Apache 2.0.
