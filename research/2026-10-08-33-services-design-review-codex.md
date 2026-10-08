<!-- Codex (gpt, read-only sandbox) design review of the Services page, 2026-10-08, from a screenshot plus services.html/style.css/services.go. -->

# Services page review

The current page makes adding a service the main event, while connected services are pushed below three large forms. At 20–30 services, that layout becomes a long form directory. Make **Connected** the first section and turn the rest into a compact catalog. Open one service’s setup on a dedicated page; this fits the existing server-rendered portal and preserves the one-time webhook-secret result page.

## What needs fixing

- **Hierarchy and density:** GitHub and GitLab forms occupy the first screen. AWS leaves a large card for a short “not installed” message. Long token guidance competes with the fields.
- **Checkboxes:** The webhook checkbox and long label wrap awkwardly; in the screenshot the check sits above its text. Put the input and a text `<span>` in one aligned row, with secondary explanation below.
- **Connected list:** It shows underlying *sources*, so one setup can appear as separate MCP and webhook cards. The cards show a type and name, but no clear health or recent activity. “Test” disappears for webhooks without explaining why.
- **Recovery:** A validation error appears at the top of the whole page. The form does not use the submitted name retained by the handler, so recovery is harder than it needs to be.
- **Growth:** Equal weight for every open form gives users no quick way to find a service or distinguish available, configured, and unavailable integrations.

## Proposed structure

1. **Connected** — compact rows grouped by logical connection: service, user chosen name, capabilities (“Tools”, “Polling”, “Webhooks”), status, last event, and actions. Show **Test** only where a test exists; use **Edit** for all. Show “Last event: No events yet” when known. Until health and event data are available, label status **Not checked**; do not infer “Healthy” from a saved configuration.
2. **Explore services** — search followed by category filters: Code hosting, Issue tracking, Chat, Monitoring & alerting, Cloud, Home & IoT, Generic. Cards show name, one-line purpose, capability chips, availability, and **Connect**. Search and filters should leave a clear no-results state.
3. **Connect `/services/{service}`** — one focused form, maximum 640px wide: **1. Choose what to connect**, **2. Provide access**, **3. Enable events**. Keep provider-specific permission help beside the relevant field, not in the catalog. A dedicated page gives keyboard users, validation errors, and browser navigation a straightforward path. Retain the current one-time secret screen as the final step.

A larger catalog should contain only working connectors as **Available**. Show planned providers as **Not available** with a brief reason and no Connect button. Generic HTTP, webhook, and MCP entries can link to existing source creation where they genuinely cover the use case; a catalog tile must not imply a provider-specific setup that the backend cannot perform.

## Visual specification

| Component | Specification |
|---|---|
| Catalog tile | Responsive grid, minimum width **240px**, roughly **150–180px** tall; 16px padding. Name and purpose at top, capability chips below, action anchored at bottom. |
| Mark | **36×36px** rounded square with a two-letter monogram or existing neutral source icon. Use theme tokens rather than vendor colors or trademarks. |
| Category filter | Existing `.pill` shape; selected state needs a visible border and text cue, not color alone. |
| Status | Reuse `.pill` with icon + label: **Working**, **Needs attention**, **Not checked**, **Unavailable**. Reserve “Working” for an actual check. |
| Connected row | Full-width list row, about **72px** minimum height on desktop. Name and capabilities left; status and last event center; Test and Edit right. Stack these on phones with 44px action targets. |
| Form | One column, 12–16px field gaps; 24px between numbered sections. Labels above inputs, hints directly below. Put the primary button at the form end; Back is a link beside it. |
| Themes | Keep `--surface`, `--surface-2`, `--line`, `--text`, and `--muted`. In both light and dark, distinguish tile hover, selected filter, focus, and disabled states by border/outline as well as fill. |

## Template shape

This uses the existing `card`, `grid`, `pill`, `btn`, and `muted` vocabulary; the new classes describe only Services-specific layout.

```html
<section class="services-connected" aria-labelledby="connected-h">
  <h2 id="connected-h">Connected <span class="muted">3</span></h2>
  <div class="card connection-list">
    <article class="connection-row">
      <span class="service-mark" aria-hidden="true">GH</span>
      <div class="connection-main">
        <h3>GitHub · work</h3>
        <span class="muted small">Tools · Webhooks</span>
      </div>
      <span class="pill status-unchecked">Not checked</span>
      <span class="muted small">Last event: No events yet</span>
      <div class="connection-actions">
        <form method="post" action="/services/work/test"><!-- CSRF; existing htmx test --></form>
        <a class="btn sm" href="/config/sources/work">Edit</a>
      </div>
    </article>
  </div>
</section>

<section class="services-catalog" aria-labelledby="catalog-h">
  <h2 id="catalog-h">Explore services</h2>
  <label class="f">Search services
    <input type="search" name="q" placeholder="Search by name or capability">
  </label>
  <nav class="category-filters" aria-label="Service categories"><!-- filter links --></nav>
  <div class="grid service-grid">
    <article class="card service-tile">
      <span class="service-mark" aria-hidden="true">GL</span>
      <h3>GitLab</h3>
      <p class="muted small">Merge requests and project events.</p>
      <div class="service-capabilities"><span class="pill tag">Polling</span></div>
      <a class="btn sm" href="/services/gitlab">Connect</a>
    </article>
  </div>
</section>
```

Suggested CSS selectors: `.service-grid`, `.service-tile`, `.service-mark`, `.category-filters`, `.connection-list`, `.connection-row`, `.connection-main`, `.connection-actions`, `.service-capabilities`. A normal GET search/filter can work without JavaScript; htmx can replace the catalog results later. Any client-side enhancement belongs in `static/app.js`. No inline style or script is needed.

## Priority

1. **Separate catalog from setup** and move Connected above it. Fix the checkbox row and error recovery in the focused form.
2. **Group connected sources into user-facing connections** and clarify Test availability. This needs reliable association between a setup and its MCP, polling, and webhook sources; the current `serviceRows()` identifies individual sources by URL, package, or signature.
3. **Add search, categories, availability, and empty states** before expanding the catalog.
4. **Add providers in batches with working setup and verification paths.** Add health and last-event fields only when the backend can supply trustworthy values.

Skipped: implementation and runtime checks, as requested. Risk: connection grouping and status require backend data beyond the current template; the visual design alone cannot supply them.
