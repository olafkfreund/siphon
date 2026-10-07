---
status: approved
issue: 24
spec: spec/2026-10-07-24-aws-integration.md
---

# Plan: AWS integration with short-lived credentials

## Approved decisions (self-contained)

- **Credential:** `credentials.<name>` gets `provider: aws`, with these fields:
  - `region`;
  - exactly one of `profile` or `role_arn`;
  - optional `external_id`;
  - optional `access_key_id` with `secret_access_key` (a pair, `Secret`
    refs, only with `role_arn`).

  Validation:
  - region `^[a-z]{2}(-[a-z]+)+-\d$`;
  - role ARN `^arn:aws[a-z-]*:iam::\d{12}:role/[\w+=,.@/-]+$`.

  An agent may not name an `aws` credential in `credential:`.
- **Fetching** (daemon only, `aws-sdk-go-v2`):
  - **profile:** `config.LoadDefaultConfig(WithSharedConfigProfile, WithRegion)`.
    Refuse credentials that cannot expire, with "profile gives long-lived
    keys; use SSO, a role, or credential_process".
  - **`role_arn`:** `stscreds.AssumeRole`, from the base keys or else the
    default chain:
    - `RoleSessionName` = `siphon-<job id>`, and `ExternalId` if set;
    - regional STS.
  - **Duration** = clamp(agent timeout + 5 min, 15 min, 1 h), with no
    renewal.
  - **Identity:** `GetCallerIdentity` returns the ARN.
- **Source:** `sources.<n>.aws: <credential>`. Validation:
  - it requires `package:`, and the credential must exist with
    `provider: aws`;
  - the package's `env` must list `AWS_ACCESS_KEY_ID`,
    `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`,
    `AWS_DEFAULT_REGION`, `AWS_EC2_METADATA_DISABLED`, `AWS_CONFIG_FILE`
    and `AWS_SHARED_CREDENTIALS_FILE`;
  - **agent tools only:** `poll`/`read` set is an error ("AWS sources are
    agent tools only for now");
  - an agent whose `mcp:` includes such a source must have a timeout of
    55 minutes or less (the error explains the 1 h session cap).
- **Run time:**
  - the agent's AWS sources go through #23's bridge (`siphon-mcp@`,
    LoadCredential, bearer, restricted egress);
  - the bridge env gets the temporary credentials plus the region,
    `AWS_EC2_METADATA_DISABLED=true`, `AWS_CONFIG_FILE=/dev/null` and
    `AWS_SHARED_CREDENTIALS_FILE=/dev/null`;
  - the three credential values are masked everywhere;
  - if fetching fails, the run fails before the agent starts, with a
    masked error.
- **Hosts:** `MCPPackage.hosts` supports `{region}`, filled from the source's
  credential. A `{region}` package without `aws:` is an error.
- **Packages:** both built from the PyPI sdist and pinned in the flake:
  - `aws-cloudwatch-mcp-server` 0.1.8, hosts `logs.{region}.amazonaws.com` and
    `monitoring.{region}.amazonaws.com`, AWS env names;
  - `aws-documentation-mcp-server` 1.1.30, hosts `docs.aws.amazon.com`,
    `proxy.search.docs.aws.com` and `api.contentrecs.docs.aws.com`, env
    `FASTMCP_LOG_LEVEL` only, no `aws:`.

  These are the last releases on `mcp` 1.x (nixpkgs has 1.29).
- **Module:** `services.siphon.aws.enable` (default false) adds both packages
  to `mcpPackages` as `aws-cloudwatch` and `aws-docs`.
  `services.siphon.aws.configFile` (`nullOr str`) sets `AWS_CONFIG_FILE` on
  the siphon service. The SSO cache stays in the siphon user's home.
- **IAM:** templates only (`examples/aws/`); Siphon never calls IAM.
- **EventBridge:** no new code. #23's `signature: token` with
  `token_header: X-Siphon-Key`; generated key shown once; docs and the tile
  explain the connection and the API destination.
- **Services tile:**
  - fields: name, region, mode (Profile/SSO recommended | Assume role:
    ARN, external ID, optional write-only base keys), servers (CloudWatch,
    Documentation), optional EventBridge webhook;
  - Save creates overlay items: the credential, one source per server, and
    the webhook;
  - Test: credentials → `GetCallerIdentity` (shows ARN and expiry) → the
    CloudWatch bridge is started as a run would start it (30 s cap) and its
    tools are listed.

## Steps

Each step is one commit, citing "Plan step N". After Go steps, run
`go test -race ./...` and `devenv test`. After Nix steps, run
`nix flake check`.

| # | Step | Who | Files |
|---|---|---|---|
| 1 | `internal/awscred` and SDK deps | coder | `internal/awscred/`, `go.mod`, `go.sum`, `flake.nix:21` |
| 2 | Config: `aws` credential, `source.aws`, validations, `{region}` | coder | `internal/config/config.go`, `overlay.go`, tests |
| 3 | Run time: fetch credentials into the bridge env | coder | `internal/job/pipeline.go`, tests |
| 4 | `action.ProbeBridge` (list a bridged server's tools) | coder | `internal/action/bridge.go`, tests |
| 5 | Services page: AWS tile and Test | coder | `internal/web/services.go`, `templates/services.html`, tests |
| 6 | Nix packages for both servers and smoke checks | Opus | `nix/pkgs/*.nix`, `flake.nix` |
| 7 | Module `aws.enable` / `aws.configFile` and an eval check | Opus | `nix/module.nix`, `flake.nix` |
| 8 | VM subtest: fake STS, stub package | Opus | `nix/vm-test.nix`, `nix/stub-*` |
| 9 | IAM templates, README AWS and EventBridge section | Opus | `examples/aws/`, `README.md` |
| 10 | Review (fresh Opus), owner live, PR | Opus | none |

1. **`internal/awscred`** (new package):
   - `Get(ctx, Spec, d time.Duration, session string) (Creds, error)`, where
     `Creds{AccessKeyID, SecretAccessKey, SessionToken string; Expires time.Time}`
     and `Spec` mirrors the credential's fields with resolved secret values;
   - `Identity(ctx, Spec, Creds) (arn string, err error)`;
   - `Duration(timeout time.Duration) time.Duration` (the clamp).
   - Deps:
     `github.com/aws/aws-sdk-go-v2/{config,credentials,credentials/stscreds,service/sts}`.
   - **Tests**, against an `httptest` fake STS through
     `t.Setenv("AWS_ENDPOINT_URL_STS", srv.URL)`:
     - AssumeRole form values (`DurationSeconds`, `RoleSessionName`,
       `ExternalId`);
     - the clamp at its edges (0, 10 min, 50 min, 2 h);
     - a static-key profile is refused (use a temp `AWS_CONFIG_FILE`
       and `AWS_SHARED_CREDENTIALS_FILE`);
     - `GetCallerIdentity` parsing;
     - errors never contain the secret key (assert with `strings.Contains`).

   **Traps:**
   - the `flake.nix:21` `vendorHash` must be updated: `nix build .#siphon`
     prints the new hash;
   - keep the dependency set minimal (no `service/sso` import by hand: the
     config package pulls what SSO needs);
   - every test must set `AWS_EC2_METADATA_DISABLED=true`, so the default
     chain never probes the network.
2. **Config** (`internal/config/config.go`):
   - extend `Credential` (around 243) with `Region`, `Profile`, `RoleARN`,
     `ExternalID string`, and `AccessKeyID`, `SecretAccessKey Secret`;
   - add `"aws"` to the provider check (around 816), with its own
     validation;
   - forbid `aws` in agent `credential:` (around 872/885);
   - add `AWS string \`yaml:"aws"\`` to `Source` (around 205), validated
     near the package checks (around 954);
   - `packageHosts` (1224) expands `{region}` using
     `c.Credentials[src.AWS].Region`;
   - the agent timeout rule goes where agents' `mcp:` are validated.

   `overlay.go` (around 209): the portal may set `region`, `profile`,
   `role_arn` and `external_id`; the keys are secrets, under the existing
   F-rules (refs confined to the item's own secrets).

   **Tests:** every rule, plus expansion, plus an overlay round trip.

   **Trap:** `Secret` fields must be resolved by the existing secret
   loader, so check that the new fields are walked in the same place as
   `APIKey` (grep `APIKey` in `config.go` and `overlay.go`).
3. **Run time** (`internal/job/pipeline.go:420-436`):
   - the bridge branch condition becomes `len(s.Env) > 0 || s.AWS != ""`;
   - for `s.AWS`: build an `awscred.Spec` from the credential, call
     `awscred.Get(ctx, spec, awscred.Duration(a.Timeout), fmt.Sprintf("siphon-%d", j.ID))`,
     and add the eight env vars;
   - on error: `return "failed", -1, "aws credentials for <source>: " + masked err, nil`;
   - make sure the job's output masking includes the three values (follow
     how `env` values reach `action.Mask`: `maps.Values(s.Env)` in
     `bridge.go` covers the bridge; check the job-level output path too).

   **Tests**, with a fake STS and the bridge fake from `bridge_test.go`:
   - the bridge env has the temporary credentials;
   - the agent job (`f.snap()`) has none of them and no base key;
   - a failing STS fails the run with a masked message.

   **Trap:** use `sockDir(t)`, not `t.TempDir()`, for bridge fakes (unix
   socket path ≤ 107 bytes in CI).
4. **`action.ProbeBridge(ctx, o AgentOptions, name string) ([]string, error)`**
   (`internal/action/bridge.go`):
   - runs `startBridges` for the one server;
   - connects a go-sdk client to the returned loopback URL with the
     bearer header and calls `ListTools`;
   - stops everything;
   - caps at 30 s.

   **Tests:** with the bridge fake, and a stub that serves `tools/list`
   (reuse `stub-mcp` from `TestBridgeSandboxNoneEndToEnd`).
5. **Services tile** (`internal/web/services.go`, `templates/services.html`):
   - `addAWS(actor, name, region, mode, profile, roleARN, externalID, keyID, secretKey string, servers []string, hook bool)`
     creates the items through `putItems`, like `addGitHub`;
   - the route goes in `serviceRoutes`;
   - `testService` gets an `aws` branch: `awscred.Get` (15 min), then
     `Identity`, then `ProbeBridge` on the CloudWatch source when present;
   - the result shows the ARN, the expiry and the tool count;
   - the tile only offers packages that are present in
     `server.mcp_packages` (otherwise it says "enable services.siphon.aws").

   **Tests:** form → items, the Test handler with a fake STS, and the
   tile hidden or explained without the packages.

   **Trap:** `TestTemplatesAreCSPClean`: no inline style or script. Copy
   the GitHub tile's markup.
6. **Packages:** `nix/pkgs/aws-cloudwatch-mcp-server.nix` and
   `aws-documentation-mcp-server.nix` (`python3Packages.buildPythonApplication`,
   `fetchPypi` sdist plus hash, `pythonRelaxDeps` only where nixpkgs versions
   differ).
   - `flake.nix` gets `packages.aws-cloudwatch-mcp-server` and
     `aws-documentation-mcp-server`.
   - **Checks** `checks.aws-mcp-smoke`: a script sends `initialize` and
     `tools/list` over stdio, with fake `AWS_*` env, `AWS_REGION=eu-west-1`
     and no network, and asserts at least one tool per server.

   **Traps:**
   - the build sandbox has no network, so tests that call AWS must be
     disabled (`doCheck = false` with a comment, as the smoke check
     covers start-up);
   - check `pandas` 3.x against cloudwatch's `pandas>=2.2.3` at import time.
7. **Module:**
   - `services.siphon.aws.enable`, `aws.configFile`;
   - `mcpPackages.aws-cloudwatch` and `aws-docs` (args `[]`: these servers
     speak stdio by default; verify with the smoke check), with their env
     and hosts;
   - `systemd.services.siphon.environment.AWS_CONFIG_FILE` when set;
   - `checks.aws-module`: an eval asserts both packages are in the
     generated `server.mcp_packages`, and the env var is in the unit.

   **Trap:** `recursiveUpdate`, not `//`, when merging into
   `server.mcp_packages` (the earlier shallow-merge bug).
8. **VM subtest:**
   - a fake STS (a small Python `http.server` on 127.0.0.1, answering
     `AssumeRole` and `GetCallerIdentity` XML) and `AWS_ENDPOINT_URL_STS` on
     the siphon unit;
   - a stub MCP package (extend `nix/stub-mcp-stdio.py`) that writes its
     `AWS_*` env to a file in its run dir;
   - an agent run, then assert:
     - the bridge saw the temporary credentials and region;
     - the base key from `credentials` appears in no file under
       `/var/lib/siphon-actions`, no job output, and not in
       `journalctl -u siphon`;
     - the bridge's allowlist is the expanded hosts.

   **Trap:** `pkill -f` in test scripts must never match the calling
   shell. Use pids.
9. **Docs:**
   - `examples/aws/siphon-readonly-policy.json` (CloudWatch read actions as
     in the spec) and `siphon-trust-policy.json` (`sts:ExternalId`
     condition);
   - a README "AWS" section:
     - role setup (console and CLI);
     - the SSO profile for the siphon user
       (`sudo -u siphon aws sso login --profile …`);
     - the 55-minute limit;
     - EventBridge connection and API destination steps;
     - the `mcp` 2.x upgrade note.
10. **Review and PR:**
    - a fresh Opus review (credentials exposure, egress, masking, the
      tile);
    - owner live:
      - a read-only role in a non-production account;
      - Test shows the assumed-role ARN;
      - an agent lists CloudWatch alarms;
      - an EventBridge event arrives;
    - then the PR.

## Tests

- `go test -race ./...` and `devenv test`: unit tests from steps 1–5.
- `nix flake check`: smoke checks, module eval, and the VM test with the
  new subtest.
- CI: all existing jobs stay green.
- The owner's live checks (step 10).

## Handoff

- **Coder:** steps 1–5 (Go), one agent, steps sent in order with
  SendMessage.
- **Opus:** steps 6–10. Steps 6–7 can run in parallel with steps 1–3.
  Step 8 needs steps 3 and 7.

## Rollback

Revert the merge. Nothing changes unless `provider: aws`, `aws:` or
`services.siphon.aws.enable` is used. The new Go dependencies go with the
revert.

## Deviations log
- **Step 1 (coder + Opus):** `awscred` errors are scrubbed of the secret key (coder). A profile that assumes a role (`role_arn` + `source_profile`) also gets the run's duration and session name, through `WithAssumeRoleCredentialOptions` (Opus).
- **Step 2 (coder):**
  - Added `Source.Polled()` (false for webhook and AWS sources), so AWS sources skip the default `poll: 1m` and the `read` requirement. **Step 3 also changes the poll-loop callers** (`internal/job/queue.go:87`, `internal/job/pipeline.go:603`, `internal/web/data.go:59`) to use `Polled()`.
  - Extra refusals beyond the plan: `url`/`api_key` on an aws credential, aws-only fields on other providers, and non-`env:`/`file:` key refs.
  - The overlay "moved" rule also covers aws: a credential's file-provided keys are not kept if `region`, `profile`, `role_arn` or `external_id` changes.
  - The JSON schemas were regenerated (`TestSchemaUpToDate`).
