# AWS

Agents get read-only AWS tools (CloudWatch logs, metrics, alarms, and AWS
documentation) with **short-lived credentials for each run**. They never
get a long-lived key. EventBridge can send events in.

## Before you start (operator, in siphon.yaml and NixOS)

```nix
services.siphon.aws.enable = true;                  # the pinned AWS MCP servers
services.siphon.aws.configFile = "/etc/siphon/aws-config";   # only for profile/SSO mode
```

```yaml
server:
  aws:
    profiles: [siphon-readonly]                      # profiles the portal and CLI may use
    role_arns: [arn:aws:iam::123456789012:role/siphon-readonly]
```

Create a read-only role from [`examples/aws/`](../../examples/aws/).

## CLI

```sh
# Profile or SSO (recommended)
siphon connect aws --name aws --region eu-west-1 --mode profile --profile siphon-readonly --servers cloudwatch,docs --webhook

# Or assume a role, optionally from base keys Siphon holds
siphon connect aws --name aws --region eu-west-1 --mode role --role-arn arn:aws:iam::123456789012:role/siphon-readonly \
  --access-key-id @aws-id --secret-access-key @aws-secret
```

```text
connected AWS: "aws"
webhook source "aws-hooks"
  URL:    https://siphon.example.com/hook/aws-hooks
  header: X-Siphon-Key: <the secret>
  secret: 9c1e…
          (shown once, copy it now)
test: ok (arn:aws:sts::123456789012:assumed-role/siphon-readonly/siphon-test, keys expire 15:47, 19 tools)
```

**EventBridge:**
1. Create a **connection** with API key auth. The key name is
   `X-Siphon-Key`, and its value is the secret shown.
2. Create an **API destination**: `POST` to the URL shown.
3. Create a rule (for example on "CloudWatch Alarm State Change") that
   targets it.

## Good to know

- Each run's keys last its timeout plus 5 minutes (15 minutes to 1 hour).
  An agent with an AWS source may run 55 minutes at most.
- Keys go only to the CloudWatch server's own sandbox. The agent talks to
  it with a per-run token.
- Logs Insights queries are read-only, but **billed per GB scanned**.

## Tasks that use it

`aws-cloudwatch-alarm`: an alarm fires, and an agent investigates with the
CloudWatch tools.

## Verify in the portal

**Services → AWS → Test** shows the assumed-role ARN, the key expiry and
the tool count.

## More

The README's AWS section covers the SSO login as the `siphon` user, the
trust policy, and the allowlist rules.
