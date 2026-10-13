# Demo: a bad deploy, start to finish

A five-minute story you can run on any Linux machine with Nix and KVM. A CI
run fails, an agent reads the log and explains why (after you approve it),
the site goes down, and a morning digest goes out. Nothing leaves your
machine, and it doesn't touch your own Siphon.

## Run it

Two terminals.

```sh
nix run github:olafkfreund/siphon#demo         # 1: boots the demo VM
nix run github:olafkfreund/siphon#demo-play    # 2: plays the three acts
```

- The first run creates `~/.config/siphon-demo/config/` with the demo
  `siphon.yaml`, a portal token and the webhook secrets. Change the location
  with `SIPHON_DEMO_DIR`.
- The portal is at **http://127.0.0.1:8091**. The token is in
  `~/.config/siphon-demo/config/token`.
- The "phone" is at **http://127.0.0.1:8092**: a plain-text log of every
  notification Siphon sends, one JSON line each. Keep it open in a second tab
  and reload it after each act.

## The model: Ollama or the built-in stub

At every start the demo looks for an Ollama on the host with `llama3.2`
(`ollama pull llama3.2`). If it finds one, the agent uses it. If not, a small
built-in stub answers with a fixed diagnosis, so the story reads the same
either way. The terminal that runs `demo` says which one it chose.

## The three acts

`demo-play` pauses for Enter between acts.

**Act 1: CI fails on main.**
- Say: "A GitHub run just failed. Siphon got the webhook and started an agent
  to read the logs. Agents wait for a human first."
- Click: the phone tab shows *Approval needed*. In the portal open
  **Approvals** and press **Approve**.
- See: the job page shows the diagnosis (failing package, test, cause, fix).

**Act 2: the site goes down.**
- Say: "Not everything needs an agent. This rule just pushes a notification."
- See: a plain notification on the phone tab, and a finished job.

**Act 3: the morning digest.**
- Say: "A routine: fetch, summarise, notify."
- See: the job's steps (`fetch`, `digest`, `notify`) and a five-bullet digest
  on the phone tab.

## Replay

Run `demo-play` again. Every run sends new ids, so nothing is skipped, with
one exception: the `ci-failed` rule has a 5-minute cooldown, so Act 1 does
nothing if you replay within 5 minutes of the last run.

## Reset

Stop the VM (Ctrl-C) and delete the directory:

```sh
rm -rf ~/.config/siphon-demo
```

## Known issue

`siphon test <rule> --last` and `siphon why` can say a rule doesn't match
when it does, for rules that read a header (`ci-failed` reads
`x-github-event`): the stored event is replayed without its headers. This
is tracked in #65. The rule itself works.
