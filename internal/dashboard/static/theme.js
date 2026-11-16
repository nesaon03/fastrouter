// Apply the saved theme before first paint (no flash). Mirrors 9Router's layout script.
(function () {
  try {
    var q = /[?&]theme=(dark|light)/.exec(location.search);
    var t = (q && q[1]) || localStorage.getItem("fr-theme") || "system";
    var dark = t === "dark" || (t === "system" && matchMedia("(prefers-color-scheme: dark)").matches);
    if (dark) document.documentElement.classList.add("dark");
  } catch (e) {}
  var f = function () { document.documentElement.classList.add("fonts-loaded"); };
  if (document.fonts && document.fonts.load) {
    document.fonts.load('24px "Material Symbols Outlined"').then(f, f);
    setTimeout(f, 3000);
  } else f();
})();
