# GitLab

Merge request and pipeline events by webhook, and polling a project's open
merge requests. Works with gitlab.com and self-hosted GitLab.

## CLI

1. Create a **project access token** with the `read_api` scope.
2. Connect:

```sh
siphon connect gitlab --name gitlab --project group/project --token @gitlab.token --webhook
siphon connect gitlab --name work --base https://gitlab.example.com --project team/app --token - --webhook
```

3. In GitLab, go to **Project → Settings → Webhooks → Add new webhook**.
   Set the **URL** to the one shown, and the **Secret token** to the one
   shown. Choose the triggers (Merge request events, Pipeline events).

A self-hosted GitLab on your LAN is a private address. The operator lists
it once in `siphon.yaml`:
`server: { services: { private_endpoints: ["gitlab.example.com:443"] } }`.

## Tasks that use it

`gitlab-mr-review` (local model) and `gitlab-pipeline-failed` (phone
notification). Rules read the event type from `headers["x-gitlab-event"]`,
for example `"Merge Request Hook"` or `"Pipeline Hook"`.

## Verify in the portal

**Services** lists the GitLab source with a **Test** button. **Sources**
shows `gitlab-hooks` and its last delivery.

## YAML (siphon.yaml)

```yaml
sources:
  gitlab-hooks:
    type: webhook
    signature: token
    token_header: X-Gitlab-Token
    secret: file:/run/credentials/siphon.service/gitlab-webhook
```
