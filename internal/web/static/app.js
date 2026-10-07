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

  // Announce htmx swaps for screen readers.
  document.addEventListener("htmx:afterSwap", function (e) {
    var live = document.getElementById("live");
    if (live && e.detail && e.detail.target && e.detail.target.id !== "main") live.textContent = "Updated";
  });
})();
