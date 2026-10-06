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
      var app = document.querySelector(".app");
      if (app) app.classList.toggle("menu-open");
    } else if (a === "copy") {
      var src = document.getElementById(t.dataset.target);
      if (src && navigator.clipboard) navigator.clipboard.writeText(src.textContent);
    }
  });

  // g d / g j / g r / g a jump between pages; / focuses the first search box.
  var g = false;
  document.addEventListener("keydown", function (e) {
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
