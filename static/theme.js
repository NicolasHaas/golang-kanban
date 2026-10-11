// Loaded in <head> without defer, so the theme is set before the first paint.
(function () {
  var saved = null;
  try { saved = localStorage.getItem('theme'); } catch (e) { /* storage disabled */ }
  if (saved === 'dark' || (!saved && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
    document.documentElement.classList.add('dark');
  }
})();
