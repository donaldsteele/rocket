// Applies the saved colour theme before first paint. Loaded non-deferred in
// <head>. Default is dark; the value is stored under rocket.theme.
(function () {
  var theme = 'dark';
  try {
    var saved = localStorage.getItem('rocket.theme');
    if (saved === 'light' || saved === 'dark') theme = saved;
  } catch (e) { /* storage blocked: keep dark */ }
  document.documentElement.setAttribute('data-theme', theme);
})();
