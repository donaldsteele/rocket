// Theme toggle, copy buttons and the screenshot lightbox. No dependencies.
(function () {
  var root = document.documentElement;
  var btn = document.getElementById('themebtn');
  var mq = window.matchMedia('(prefers-color-scheme: light)');
  function current() { return root.dataset.theme || (mq.matches ? 'light' : 'dark'); }
  function paint() {
    var light = current() === 'light';
    btn.setAttribute('aria-label', light ? 'Switch to dark theme' : 'Switch to light theme');
    btn.querySelector('use').setAttribute('href', 'assets/icons.svg#i-' + (light ? 'moon' : 'sun'));
  }
  btn.addEventListener('click', function () {
    var next = current() === 'light' ? 'dark' : 'light';
    root.dataset.theme = next;
    try { localStorage.setItem('rocket-site-theme', next); } catch (e) {}
    paint();
  });
  mq.addEventListener('change', paint);
  paint();

  document.querySelectorAll('.copybtn[data-copy]').forEach(function (b) {
    b.addEventListener('click', function () {
      if (!navigator.clipboard) return;
      navigator.clipboard.writeText(b.dataset.copy).then(function () {
        b.textContent = 'Copied'; b.classList.add('done');
        setTimeout(function () { b.textContent = 'Copy'; b.classList.remove('done'); }, 1600);
      });
    });
  });

  var box = document.getElementById('lightbox');
  var big = box.querySelector('img');
  document.querySelectorAll('.zoom').forEach(function (z) {
    z.addEventListener('click', function () {
      var img = Array.prototype.find.call(z.querySelectorAll('img'), function (i) { return i.offsetParent !== null; }) || z.querySelector('img');
      big.src = img.currentSrc || img.src;
      big.alt = img.alt;
      // Phone screenshots open narrow, so they fit the screen height.
      box.classList.toggle('tall', img.naturalHeight > img.naturalWidth);
      box.showModal();
    });
  });
  box.addEventListener('click', function (e) { if (e.target === box) box.close(); });
})();
