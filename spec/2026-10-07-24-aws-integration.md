---
status: approved
issue: 24
intent: intent/2026-10-07-24-aws-integration.md
---

# Spec: AWS integration with short-lived credentials

## Design

### How it fits #23

#23 runs a stdio MCP server that needs secrets in its own sandboxed unit:
- the server is `siphon-mcp@<id>`;
- its secrets are written 0600 to `bridge-secrets/<id>` and read through
  `LoadCredential`;
- the agent reaches it only with a per-run bearer token.

AWS reuses all of that. The only new part is **where the secret comes
from**:
- the daemon fetches temporary AWS credentials for each run;
- it writes them into the bridge secrets as ordinary environment
  variables.

The base credential never leaves the daemon.

```
agent run ──bearer──▶ siphon-mcp@<id> (CloudWatch MCP server)
                         env: AWS_ACCESS_KEY_ID / SECRET / SESSION_TOKEN (temporary), AWS_REGION
                         egress: logs.<region>, monitoring.<region> only
siphon daemon ──STS──▶ AssumeRole / profile / SSO ──▶ temporary credentials (≤ 1 h)
```

### 1. The `aws` credential

The `credentials:` provider gets a new value, `aws`:

```yaml
credentials:
  aws-ro:
    provider: aws
    region: eu-west-1
    # Either: a profile in the siphon user's AWS config (SSO, credential_process,
    # or role_arn + source_profile). Recommended.
    profile: siphon-readonly
    # Or: a role to assume, from a base credential Siphon holds (or, without
    # base keys, the daemon's default chain, e.g. an EC2 instance role).
    role_arn: arn:aws:iam::123456789012:role/siphon-readonly
    external_id: env:AWS_EXTERNAL_ID          # optional
    access_key_id: file:/run/credentials/siphon.service/aws-id         # optional
    secret_access_key: file:/run/credentials/siphon.service/aws-secret # optional
```

**Validation:**
- exactly one of `profile` or `role_arn`;
- `region` must match `^[a-z]{2}(-[a-z]+)+-\d$`;
- `role_arn` must match `^arn:aws[a-z-]*:iam::\d{12}:role/[\w+=,.@/-]+$`;
- `access_key_id` and `secret_access_key` come as a pair, and only with
  `role_arn`;
- the secrets are `Secret` references, under the same rules as today
  (`file:`/`env:`, write-only in the portal).

**Fetching the credentials:**
- **New dependencies:** `aws-sdk-go-v2` `config`, `credentials`,
  `credentials/stscreds` and `service/sts`, behind a small
  `internal/awscred`:
  - `Get(ctx, cred, duration, session)` returns `(akid, secret, token, expires)`;
  - `Identity(ctx, ...)` returns the caller ARN, through
    `sts:GetCallerIdentity`.
- **Profile:**
  - `config.LoadDefaultConfig(WithSharedConfigProfile)`. The SDK handles
    SSO (from the siphon user's SSO cache), `credential_process`, and
    `role_arn`/`source_profile` chains.
  - **If the profile yields credentials that never expire** (static keys),
    Siphon refuses: "profile gives long-lived keys; use SSO, a role, or
    credential_process".
- **`role_arn`:**
  - `stscreds.AssumeRole` from the base keys, or from the default chain if
    there are none;
  - `RoleSessionName` = `siphon-<run id>`, and `ExternalId` if set;
  - regional STS (`sts.<region>.amazonaws.com`).

**Session length:**
- clamp(agent timeout + 5 min, 15 min, 1 h);
- one session per run, with no renewal;
- `validate` rejects an agent with an AWS source and a timeout over 55
  minutes, and says why.

### 2. Sources and packages

An MCP source names the credential with `aws:`:

```yaml
sources:
  cloudwatch: { type: mcp, package: aws-cloudwatch, aws: aws-ro }
  aws-docs:   { type: mcp, package: aws-docs }      # no AWS credentials needed
```

**Validation:**
- `aws:` needs `package:` (never a free command);
- the credential must exist and have `provider: aws`;
- the package must list the AWS environment names below in its `env`;
- **a source with `aws:` is agent tools only:** a `poll` or `read` on it is
  a `validate` error ("AWS sources are agent tools only for now").

**At bridge start:** `startBridges`, for each such source:
1. Fetch the credentials.
2. Add them to the bridge secrets as:
   - `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`;
   - `AWS_REGION` and `AWS_DEFAULT_REGION`;
   - `AWS_EC2_METADATA_DISABLED=true`;
   - `AWS_CONFIG_FILE=/dev/null` and `AWS_SHARED_CREDENTIALS_FILE=/dev/null`,
     so the server cannot fall back to another credential source.
3. Add the three secret values to the run's redaction list.

If fetching fails, the run fails before the agent starts, with the STS error
(the secrets are already redacted from it).

**Hosts:** `MCPPackage.hosts` gains a `{region}` placeholder, filled from the
source's credential. `BridgeEgress` expands it. A package with `{region}`
used without `aws:` is a `validate` error.

### 3. Packaging: two pinned servers

`nix/pkgs/` gets two `buildPythonApplication` derivations, built from the PyPI
sdists with hashes in the flake. Each is checked by a smoke test: start it
over stdio, `initialize`, `tools/list`, with fake credentials and no network.

| Package | Version | Why this version | Hosts |
|---|---|---|---|
| `aws-cloudwatch-mcp-server` | 0.1.8 | last release on `mcp` 1.x; nixpkgs has `mcp` 1.29 | `logs.{region}.amazonaws.com`, `monitoring.{region}.amazonaws.com` |
| `aws-documentation-mcp-server` | 1.1.30 | same | `docs.aws.amazon.com`, `proxy.search.docs.aws.com`, `api.contentrecs.docs.aws.com` |

`services.siphon.aws.enable` (default `false`, because the CloudWatch closure
pulls in pandas, numpy and statsmodels):
- adds both servers to `services.siphon.mcpPackages` as `aws-cloudwatch` and
  `aws-docs`, with their env names and hosts;
- adds `services.siphon.aws.configFile` (`nullOr str`). It is set as
  `AWS_CONFIG_FILE` for the siphon service. The SSO cache stays under the
  siphon user's home (`/var/lib/siphon/.aws`).

### 4. Read-only IAM templates (docs only)

`examples/aws/` contains:
- `siphon-readonly-policy.json`: CloudWatch Logs and metrics read only:
  - `logs:Describe*`, `logs:Get*`, `logs:FilterLogEvents`,
    `logs:StartQuery`, `logs:GetQueryResults`, `logs:StopQuery`;
  - `cloudwatch:Describe*`, `cloudwatch:Get*`, `cloudwatch:List*`.
- `siphon-trust-policy.json`: trust for the base principal, with an
  `sts:ExternalId` condition.
- A README section on creating the role (console and CLI) and setting up the
  SSO profile for the siphon user (`sudo -u siphon aws sso login --profile …`).

Siphon never calls IAM.

### 5. EventBridge in

No new code. The docs and the Services tile set up a `type: webhook` source
with #23's `token` mode:
- `signature: token`, `token_header: X-Siphon-Key`;
- Siphon generates the key and shows it once, with the URL and header;
- you create the EventBridge connection (API key auth) and the API
  destination with them.

Rules can de-duplicate on the event's own `id` (`id: event.id`).

### 6. The Services page: AWS tile

**Fields:**
- name and region;
- mode: **Profile/SSO** (recommended), or **Assume role** (role ARN, optional
  external ID, optional base keys, which are write-only);
- which servers: CloudWatch, Documentation;
- an optional EventBridge webhook.

**Saving** creates ordinary overlay items:
- the `aws` credential;
- one source per chosen server;
- the webhook source, if chosen.

As in #23, the stdio command stays file-only: the tile only picks
`package:` names that the module already allows.

**Test:**
1. Fetch the credentials.
2. `GetCallerIdentity` must return an assumed-role or SSO ARN.
3. The page shows the ARN and the expiry.
4. Then the tile starts the CloudWatch bridge exactly as a run would, with a
   30 s cap, and lists its tools.

## Alternatives rejected

- **Shelling out to `aws configure export-credentials`:** pulls the awscli2
  Python closure into the daemon, and means parsing a subprocess. The Go SDK
  does the same chains in process, and is testable against a fake STS.
- **A hand-written SigV4/STS client:** small, but no SSO or
  `credential_process`. Owner question 2 wants profiles and SSO.
- **The latest AWS servers (`mcp` 2.x), packaging `mcp` 2.x ourselves:** a
  second MCP SDK to maintain in the flake. The pinned 1.x releases are a
  month old. Upgrade when nixpkgs ships `mcp` 2.x (tracked in the README).
- **`aws-api-mcp-server` first:** it runs any AWS CLI command, so IAM is
  the only limit. Deferred (owner question 1).
- **A credential endpoint the bridge refreshes from
  (`AWS_CONTAINER_CREDENTIALS_FULL_URI`):** it would allow renewal, but
  adds a server and its auth. A fixed session that covers the run is
  enough (owner question 3).
- **AWS's managed remote MCP server (SigV4 through a local proxy):** still
  in preview, and needs another proxy program in the sandbox.
- **Session policies:** not supported for profiles. Read-only roles
  achieve the same, more visibly in IAM.

## Risks

- **SSO is awkward on a headless host.** The SSO login must be done as the
  siphon user, and it expires (typically 8–12 h). When it has expired,
  runs fail with a clear "SSO session expired: run sudo -u siphon aws sso
  login --profile X" message, and Test shows the same.
- **Pinned older servers miss upstream fixes.** Mitigation: they're
  read-only with a narrow scope, and the upgrade path is noted.
- **Closure size:** about 1 GB for pandas, numpy and statsmodels. That is
  why it's opt-in through `aws.enable`.
- **Agents with a timeout over 55 minutes can't use AWS sources.** This is
  stated in the `validate` error. Lift it later with renewal if needed.
- **Default-chain role assumption on EC2:** the daemon may use the
  instance role as the base. That's intended, and documented. Runs still
  can't reach the metadata endpoint (the existing deny).
- **Clock skew breaks STS.** NixOS has timesyncd by default, and the error
  is shown as it comes.

The changes are all in Siphon; no host changes beyond what the owner
enables.

## Verification

- **Unit tests** (`internal/awscred`, against an `httptest` fake STS through
  `AWS_ENDPOINT_URL_STS`):
  - AssumeRole is sent with the clamped duration, the session name and the
    external ID;
  - a static-key profile is refused;
  - `GetCallerIdentity` works;
  - errors are redacted.
- **Unit tests** (config): every validation rule above, including the
  55-minute rule, `{region}` expansion, and polling being refused.
- **Unit tests** (action): the bridge secrets contain the temporary
  credentials and the fallback-blocking variables. The agent's job, argv
  and environment contain none of them, and nothing about the base key.
- **NixOS VM test subtest:**
  - a fake STS on loopback (`AWS_ENDPOINT_URL_STS` on the siphon unit) and a
    stub MCP package that records its environment;
  - an agent run proves the bridge got the temporary credentials and region;
  - the base key appears nowhere in the bridge, the agent, or any log or
    job output;
  - the bridge's egress allows only the expanded hosts.
- **Nix:**
  - `nix build .#aws-cloudwatch-mcp-server .#aws-documentation-mcp-server`;
  - their `tools/list` smoke checks;
  - an eval check that `aws.enable` adds both packages and `configFile` sets
    `AWS_CONFIG_FILE`.
- **Web:** tests for the tile form, the items it creates, and the Test
  handler (against the fake STS). `TestTemplatesAreCSPClean`.
- **Owner live:**
  - a read-only role in a non-production account;
  - Test shows the assumed-role ARN;
  - an agent lists CloudWatch alarms;
  - an EventBridge rule's events arrive through the webhook.
