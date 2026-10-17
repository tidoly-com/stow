// Light or dark: the system's, unless one chose otherwise on this device (the account menu). Loaded in the
// head, before the page is drawn, so that it never flashes the other way.
(function () {
  "use strict";
  const root = document.documentElement;
  const dark = window.matchMedia("(prefers-color-scheme: dark)");
  const chosen = () => {
    try { return localStorage.getItem("theme") || "system"; } catch (e) { return "system"; }
  };
  const apply = () => {
    const c = chosen();
    root.dataset.theme = c === "system" ? (dark.matches ? "dark" : "light") : c;
    document.querySelectorAll("[data-theme-set]").forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.themeSet === c)));
  };
  apply();
  dark.addEventListener("change", apply);
  document.addEventListener("DOMContentLoaded", apply);
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-theme-set]");
    if (!b) return;
    try { localStorage.setItem("theme", b.dataset.themeSet); } catch (err) { /* this device will not remember */ }
    apply();
  });
})();
