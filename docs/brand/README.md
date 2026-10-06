# Siphon brand

> **Siphon:** draws events in, jets agents out.

An octopus moves by drawing water in through its mantle and jetting it out
through its **siphon**. Siphon does the same with events: it draws them in
from MCP servers, APIs and webhooks, and jets out agents, commands and
routines.

Like an octopus arm, each action senses and acts on its own while one
gateway coordinates. The brand comes from that idea: **flow in, flow out**.
Its look is deep water lit by bioluminescence.

## Logo

| File | Use |
|---|---|
| `mark.svg` | App icon, favicon and avatar. The arm on a rounded "deep water" tile |
| `mark-bare.svg` | The arm alone, on any background (portal header, docs) |

- **The mark.** A tentacle that is thin where it draws in (bottom left),
  curls into an **S**, and ends in the siphon's **jet** of three bubbles
  (top right). Suckers sit along the arm.
- **The wordmark** is lowercase **siphon**, set in the UI font at weight 650,
  with letter-spacing -0.02em. It sits to the right of the mark, its cap
  height level with the top of the S.
- **Clear space** is at least the width of the largest bubble on every
  side. Minimum size is 16 px (favicon); below 24 px, use `mark.svg`
  (with the tile).
- **Don't:**
  - recolour the arm in a single flat colour, except monochrome print;
  - rotate it;
  - add a face.

  The arm is the character.

## Colour: "Deep Current"

| Token | Hex | Role |
|---|---|---|
| `abyss` | `#06111F` | Dark background, text on light |
| `deep` | `#0E2A47` | Dark surfaces, tile gradient |
| `ink` | `#7C5CFF` | Brand violet: start of the arm gradient, focus |
| `current` | `#3B82F6` | Brand blue: links, primary actions |
| `biolume` | `#2EE6D6` | Brand teal: the jet, highlights, live/flowing |
| `kelp` | `#34D399` | Success / done |
| `amber` | `#FFB547` | Waiting / needs you |
| `coral` | `#FF5D73` | Failure / blocked |
| `shallows` | `#F3F8FB` | Light background |

- **Gradient.** The brand gradient is `ink → current → biolume`, at 135°.
  It is used sparingly: the mark, the primary button, the active flow line
  and the hero numbers.
- **Light theme ("Shallows"):** pale aqua background, white cards, abyss
  text.
- **Dark theme ("Abyss"):** abyss background, `deep` cards, soft glows from
  `biolume` on live elements.

## Visual language

- **Flow.** Everything that moves is drawn as a current: rules appear as
  source → condition → action, linked by a line that **flows** (animated
  dashes) while the rule is live and **pulses** when it fires.
- **Bubbles.** These are the jet's three bubbles, reused as the "live"
  indicator, the loading state and the empty-state motif (bubbles rising
  from an empty card).
- **Shape.** Soft and organic: 14 px card radius, pill buttons and
  badges, no hard edges.
- **Motion.** Slow and liquid (200–400 ms, ease-out), never bouncy.
  Everything is off under `prefers-reduced-motion`.
- **Voice.** Clear first, a little ocean second.
  - Use plain labels: "Rules", "Jobs", "Approve".
  - Flavour goes only where it costs nothing: empty states ("Nothing in
    the current yet") and the tagline.
  - Never use puns in error messages.
