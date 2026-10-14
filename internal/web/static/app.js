// Siphon portal glue. No framework; htmx does the requests. Everything here is
// progressive: the portal works without it.
(function () {
  "use strict";
  var root = document.documentElement;
  var stored = null;
  try { stored = localStorage.getItem("siphon-theme"); } catch (e) {}
  if (stored === "light" || stored === "dark") root.dataset.theme = stored;

  function dark() {
    return root.dataset.theme === "dark" ||
      (!root.dataset.theme && window.matchMedia("(prefers-color-scheme: dark)").matches);
  }

  document.addEventListener("click", function (e) {
    var t = e.target.closest("[data-action]");
    if (!t) return;
    var a = t.dataset.action;
    if (a === "theme") {
      root.dataset.theme = dark() ? "light" : "dark";
      try { localStorage.setItem("siphon-theme", root.dataset.theme); } catch (e2) {}
    } else if (a === "menu") {
      setMenu(t, !document.querySelector(".app").classList.contains("menu-open"));
    } else if (a === "copy") {
      var src = document.getElementById(t.dataset.target);
      if (src && navigator.clipboard) navigator.clipboard.writeText(src.textContent);
    }
  });

  function setMenu(btn, open) {
    var app = document.querySelector(".app");
    if (!app || !btn) return;
    app.classList.toggle("menu-open", open);
    btn.setAttribute("aria-expanded", open ? "true" : "false");
    if (open) { var first = document.querySelector("#sidenav a"); if (first) first.focus(); }
    else btn.focus();
  }

  // Model endpoint tiles show the provider's default URL as the placeholder.
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t.name !== "preset" || !t.form) return;
    var u = t.form.querySelector("input[name=url]");
    if (u) u.placeholder = t.dataset.url || "https://…/v1";
  });

  // Forms that delete something ask first (data-confirm holds the question).
  document.addEventListener("submit", function (e) {
    var q = e.target.getAttribute && e.target.getAttribute("data-confirm");
    if (q && !window.confirm(q)) e.preventDefault();
  });

  // g d / g j / g r / g a jump between pages; / focuses the first search box.
  var g = false;
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && document.querySelector(".app.menu-open")) {
      setMenu(document.querySelector("[data-action=menu]"), false);
      return;
    }
    if (e.target.closest("input, textarea, select") || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === "/") {
      var s = document.querySelector("input[type=search]");
      if (s) { e.preventDefault(); s.focus(); }
      return;
    }
    if (g) {
      var to = { d: "/", j: "/jobs", r: "/rules", a: "/approvals", s: "/sources" }[e.key];
      g = false;
      if (to) location.href = to;
      return;
    }
    if (e.key === "g") { g = true; setTimeout(function () { g = false; }, 800); }
  });

  // List filters: a bar[data-filter-scope=X] (search + chips) narrows the items of
  // [data-filter-list=X] ([data-filter-item]); state lives per scope for the session.
  var kept = null; // the focused search across an htmx swap
  function initFilters() {
    document.querySelectorAll(".filter-bar[data-filter-scope]").forEach(function (bar) {
      if (bar.dataset.bound) return; // bound once per rendered bar
      bar.dataset.bound = "1";
      var scope = bar.dataset.filterScope, key = "siphon-filter-" + scope;
      var list = document.querySelector('[data-filter-list="' + scope + '"]');
      var none = document.querySelector('.filter-none[data-filter-scope="' + scope + '"]');
      var q = bar.querySelector(".filter-search"), chips = bar.querySelectorAll(".chip"), chip = "all";
      var items = list ? list.querySelectorAll("[data-filter-item]") : [];
      try { var st = JSON.parse(sessionStorage.getItem(key) || "{}"); q.value = st.q || ""; chip = st.chip || "all"; } catch (e) {}
      bar.hidden = false;
      var fields = ["kind", "status", "state", "health"];
      function has(it, f) { return fields.some(function (k) { return it.dataset[k] === f; }); }
      chips.forEach(function (c) {
        var f = c.dataset.filter;
        if (f === "all" || f === "hot") return;
        if (!Array.prototype.some.call(items, function (it) { return has(it, f); }) && f !== "on" && f !== "off") c.hidden = true;
      });
      var saved = bar.querySelector('.chip[data-filter="' + chip + '"]');
      if (!saved || saved.hidden) chip = "all"; // the saved chip may match nothing any more
      function apply() {
        var words = q.value.toLowerCase().split(/\s+/).filter(Boolean), shown = 0;
        items.forEach(function (it) {
          var ok = chip === "all" || has(it, chip) || (chip === "hot" && it.dataset.hot === "true");
          var text = (it.dataset.text || "").toLowerCase();
          it.hidden = !(ok && words.every(function (w) { return text.indexOf(w) >= 0; }));
          if (!it.hidden) shown++;
        });
        chips.forEach(function (c) { c.setAttribute("aria-pressed", c.dataset.filter === chip ? "true" : "false"); });
        if (none) none.hidden = shown > 0;
        try { sessionStorage.setItem(key, JSON.stringify({ q: q.value, chip: chip })); } catch (e) {}
      }
      q.addEventListener("input", apply);
      bar.addEventListener("click", function (e) { var c = e.target.closest(".chip"); if (c) { chip = c.dataset.filter; apply(); } });
      if (none) none.addEventListener("click", function (e) {
        if (e.target.closest("[data-action=clear-filter]")) { q.value = ""; chip = "all"; apply(); q.focus(); }
      });
      apply();
      if (kept && kept.scope === scope) { q.focus(); try { q.setSelectionRange(kept.s, kept.e); } catch (e) {} kept = null; }
    });
  }
  document.addEventListener("DOMContentLoaded", initFilters);
  document.addEventListener("htmx:beforeSwap", function () {
    var a = document.activeElement, bar = a && a.closest && a.closest(".filter-bar[data-filter-scope]");
    kept = bar && a.classList.contains("filter-search") ? { scope: bar.dataset.filterScope, s: a.selectionStart, e: a.selectionEnd } : null;
  });
  document.addEventListener("htmx:afterSwap", initFilters);

  // YAML editor: line numbers, indent keys, error line. The textarea stays the form field.
  function initCodeEditors() {
    document.querySelectorAll("textarea.code-input").forEach(function (ta) {
      if (ta.dataset.editor) return;
      ta.dataset.editor = "1";
      var wrap = document.createElement("div"), gutter = document.createElement("div"), leave = false;
      wrap.className = "code-editor";
      gutter.className = "code-gutter";
      gutter.setAttribute("aria-hidden", "true");
      ta.parentNode.insertBefore(wrap, ta);
      wrap.appendChild(gutter);
      wrap.appendChild(ta);
      function number() {
        var n = ta.value.split("\n").length;
        while (gutter.children.length < n) gutter.appendChild(document.createElement("div")).textContent = gutter.children.length + 1;
        while (gutter.children.length > n) gutter.removeChild(gutter.lastChild);
      }
      function edit(from, to, text, selStart, selEnd) {
        ta.setRangeText(text, from, to, "preserve");
        ta.setSelectionRange(selStart, selEnd);
        ta.dispatchEvent(new Event("input", { bubbles: true }));
      }
      ta.addEventListener("input", function () { number(); gutter.scrollTop = ta.scrollTop; });
      ta.addEventListener("scroll", function () { gutter.scrollTop = ta.scrollTop; });
      ta.addEventListener("keydown", function (e) {
        if (e.ctrlKey || e.altKey || e.metaKey) return;
        var v = ta.value, a = ta.selectionStart, b = ta.selectionEnd;
        if (e.key === "Escape") { leave = true; return; }
        var skip = leave; leave = false;
        if (e.key === "Tab" && !skip) {
          e.preventDefault();
          var ls = v.lastIndexOf("\n", a - 1) + 1;
          if (a === b && !e.shiftKey) { edit(a, b, "  ", a + 2, a + 2); return; }
          var end = b > a && v[b - 1] === "\n" ? b - 1 : b, first = 0, total = 0;
          var out = v.slice(ls, end).split("\n").map(function (l, i) {
            var r = e.shiftKey ? l.replace(/^ {1,2}/, "") : "  " + l;
            if (i === 0) first = r.length - l.length;
            total += r.length - l.length;
            return r;
          }).join("\n");
          edit(ls, end, out, Math.max(ls, a + first), b + total);
        } else if (e.key === "Enter" && a === b && !e.shiftKey) {
          e.preventDefault();
          var cur = v.slice(v.lastIndexOf("\n", a - 1) + 1, a), ind = cur.match(/^ */)[0];
          if (/:\s*$/.test(cur)) ind += "  ";
          edit(a, b, "\n" + ind, a + 1 + ind.length, a + 1 + ind.length);
        }
      });
      number();
    });
    markErrorLine();
  }
  function markErrorLine() {
    var st = document.getElementById("yaml-status"), l = st ? parseInt(st.dataset.line, 10) : 0;
    document.querySelectorAll(".code-gutter").forEach(function (g) {
      Array.prototype.forEach.call(g.children, function (d, i) { d.classList.toggle("is-err", i + 1 === l); });
    });
  }
  document.addEventListener("DOMContentLoaded", initCodeEditors);
  document.addEventListener("htmx:afterSwap", initCodeEditors); // also re-marks the error line after a status swap

  // Announce htmx swaps for screen readers.
  document.addEventListener("htmx:afterSwap", function (e) {
    var live = document.getElementById("live");
    if (live && e.detail && e.detail.target && e.detail.target.id !== "main") live.textContent = "Updated";
  });
})();
