(function () {
  'use strict';
  let theme = 'light';
  try { if (localStorage.getItem('k8gb-ui-theme') === 'dark') theme = 'dark'; } catch (_) { /* Storage may be restricted. */ }
  document.documentElement.dataset.theme = theme;
})();
