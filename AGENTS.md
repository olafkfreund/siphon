# AGENTS.md

Instructions for AI agents (Claude Code, Codex, Cursor, …). There are two
jobs, and this file covers both:

1. **Operating Siphon:** adding and changing tasks for a user through the
   `siphon` CLI.
2. **Working on Siphon's code:** this repository.

## 1. Operating Siphon (using the CLI)

Read [`docs/llm.md`](docs/llm.md) first. `siphon guide` prints it, if you
only have the binary. The short version:

```sh
siphon inventory -o json            # what exists: sources, agents, connections, allowlists
siphon template                     # ~30 ready-made tasks by category
siphon template github-pr-review    # print one; its header says what to connect and which secrets to pass
siphon explain rule                 # every field of a rule (also: source, agent, routine, credential)
siphon apply -f task.yaml --dry-run -o json   # validate + diff; fix every item in "errors"
siphon apply -f task.yaml --yes     # after the user has seen the diff
siphon test <rule> --last           # does it match the last real event?
siphon why <rule> -o json           # why didn't it fire?
siphon draft "<what the user wants>"   # or let a model connection write the task, then review it
```

**Rules:**
- **Never write a secret** into a file, a flag value or your output. Pass
  secrets with `--secret <kind>/<name>.<field>=-` (stdin) or `=@file`.
- **Never approve or deny jobs** (`siphon approve`) unless the user tells you
  to, for that job.
- **Show the user the dry-run diff** before applying.
- **Commands are argv lists, not shell strings.** `cooldown:` is required
  for agent actions.
- **Some changes are operator-only,** in `siphon.yaml`. Tell the user
  instead of working around it:
  - `server.*`, `limits`, `units`;
  - stdio `command`s;
  - new private hosts;
  - AWS profiles or roles not in `server.aws`.
- **Exit codes:** 0 ok, 1 error, 2 usage, 3 validation, 4 not found,
  5 conflict. With `-o json`, errors are `{"error", "errors", "hint"}`;
  follow the `hint`.
- **`siphon mcp`** exposes the same operations as MCP tools. Writes are only
  dry-run unless the user started it with `--allow-write`. Secret or
  plain-header values need `--allow-secrets`, and turning an agent's
  approval off needs `--allow-unapproved`.
- **`-o json` is not consent:** `apply`, `restore` and `draft --apply`
  need an explicit `--yes`.

**More:**
- the task-oriented guide: [`docs/README.md`](docs/README.md);
- the templates: [`docs/templates/`](docs/templates/README.md);
- the concepts: [`docs/concepts.md`](docs/concepts.md);
- the machine-readable index: [`llms.txt`](llms.txt).

## 2. Working on Siphon's code

**Stack:**
- Go 1.26, stdlib first, with `go-sdk` (MCP), `expr-lang`, `modernc`
  SQLite, `golang.org/x/term`;
- the portal: htmx plus `html/template`, under a strict CSP;
- a NixOS module, a microVM, an OCI image;
- devenv.

**Layout:**

| Path | What |
|---|---|
| `cmd/siphon` | the binary: local commands (`serve`, `validate`, …) and the API client (`apply`, `get`, …) |
| `internal/config` | config structs, `Validate`, the portal/API overlay rules (`overlay.go`), the JSON schema |
| `internal/web` | portal and HTTP API (`api.go`, `configapi.go`, `connapi.go`, `diagapi.go`), templates in `templates/` |
| `internal/job` | the pipeline: polling, webhooks, rules → jobs, routines, approvals |
| `internal/action` | running actions: sandboxed units, agents, the MCP bridge |
| `internal/client` | the CLI's API client |
| `internal/store` | SQLite and the migrations (`migrations/NNNN_*.sql`) |
| `nix/` | the NixOS module, the VM test, the microVM, the image, packages |
| `docs/` | the user guide, the templates (embedded in the binary) |

**Commands:**

```sh
go vet ./... && go test -race ./...     # unit tests
devenv test                             # the same, in the devenv shell
nix flake check                         # VM test, smoke checks, eval checks (needs KVM)
nix build --rebuild .#siphon.goModules  # after any go.mod change: confirms vendorHash for real
go run ./cmd/siphon schema > schema/siphon.schema.json   # or UPDATE_SCHEMA=1 go test ./internal/config
```

**Workflow** (the org policy, enforced in review):
- Any task tracked as an issue, or touching more than one file, goes
  through `intent/` → `spec/` → `plan/` (`YYYY-MM-DD-<issue>-<slug>.md`).
- Each file is approved by the owner before the next is written. Never
  self-approve.
- Implement only an approved `plan/`, citing "Plan step N" in each commit.
  A deviation is logged in the plan, in the same commit as the code.
- Commits: `type(scope): summary (#issue)`.

**Traps that have bitten before:**
- **The CSP is strict:** no inline `style=` or `<script>` in templates.
  `TestTemplatesAreCSPClean` fails otherwise.
- **`vendorHash`:** a stale `*-go-modules` store path can make a wrong hash
  look right. Always `--rebuild` the goModules after changing deps.
- **Unix socket paths are ≤ 107 bytes.** CI's `TMPDIR` is long, so
  socket-using tests need a short `/tmp` dir, not `t.TempDir()`.
- **`pkill -f <pattern>`** in a script also kills the shell whose command
  line contains the pattern. Use PIDs.
- **Nix merges:** use `lib.recursiveUpdate`/`mkMerge`, not `//`, for nested
  settings and lists.
- **Time-based tests:** inject a clock. Real-clock rate limiters and timers
  flake under CI load.
- **Secrets:** a value only ever reaches its consumer (a bridge unit, the
  daemon). Mask the job output and errors. Never log tokens.
