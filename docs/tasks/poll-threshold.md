# React when a value crosses a threshold

**Goal:** poll a JSON URL every few minutes, and act once when something
becomes true (a disk is full, a provider has an incident). Act again only
if it stays true for a long time.

## CLI

```sh
siphon template disk-full > disk.yaml        # edit the URL and the threshold
siphon apply -f disk.yaml --dry-run
siphon apply -f disk.yaml --yes
```

Or with flags:

```sh
siphon new task --name disk-full --poll disk --url https://metrics.example.com/api/disk --every 5m \
  --when 'event.used_pct > 90' --on edge --repeat 6h \
  --cmd '["curl","-fsS","-d","disk {{.event.used_pct}}%","https://ntfy.sh/your-topic"]' --yes
```

## `on: edge` vs `on: each`

- **`on: edge`** fires when `when` turns **false → true**, then stays quiet
  while it remains true. `repeat: 6h` re-fires every 6 h while it's still
  true. Use it for **states**.
- **`on: each`** fires once per distinct `id`. Use it for **things that
  happen** (deliveries, items).

## Real-life examples

- `upstream-status`: any Atlassian Statuspage (GitHub, Cloudflare, OpenAI)
  goes from `none` to an incident.
- `disk-full`: a metrics endpoint.

## Verify

- `siphon test disk-full --last` evaluates the rule against the last poll.
- **Sources** in the portal shows each poll's time and any error.
- `siphon why disk-full` shows `edge_already_true` while the condition
  stays true and nothing new fires.
