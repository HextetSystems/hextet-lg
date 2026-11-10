// Applies the visitor's stored theme choice before first paint. It is loaded
// synchronously in <head>, after the stylesheets, so there is no flash of the
// wrong theme. Without a stored choice (or without JavaScript) the generated
// stylesheet applies the configured default: auto, light or dark.
(function () {
  try {
    var t = localStorage.getItem('theme');
    if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  } catch (e) {}
})();
