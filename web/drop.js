// rocket drop page for browser guests on the LAN. Two steps: POST drop/offer
// with the file list (the person at the computer accepts, unless this browser
// was accepted before), then POST drop/upload?token= with the files as one
// multipart body, in offer order. Plain script, no framework: this page and
// its assets are all a LAN guest may load.
(function () {
  'use strict';
  var zone = document.getElementById('zone');
  var picker = document.getElementById('picker');
  var choose = document.getElementById('choose');
  var list = document.getElementById('files');
  var statusEl = document.getElementById('status');
  var busy = false;

  // Each file state is a word plus a shape (never colour alone).
  var STATES = {
    waiting: { word: 'Waiting', icon: 'clock', tone: 'warn' },
    sending: { word: 'Sending', icon: 'arrow-up', tone: 'info' },
    sent: { word: 'Sent', icon: 'circle-check', tone: 'ok' },
    notsent: { word: 'Not sent', icon: 'circle-x', tone: 'warn' },
    failed: { word: 'Failed', icon: 'triangle-alert', tone: 'bad' },
  };
  // Status line tones: alert class and icon.
  var TONES = {
    ok: ['ok', 'circle-check'], info: ['info', 'arrow-up'], warn: ['warn', 'clock'], bad: ['error', 'triangle-alert'],
  };

  // say shows the status line: tone is ok, info, warn or bad.
  function say(msg, tone) {
    var t = TONES[tone] || TONES.info;
    statusEl.hidden = false;
    statusEl.className = 'alert ' + t[0];
    statusEl.textContent = '';
    statusEl.appendChild(icon(t[1], tone));
    var span = document.createElement('span');
    span.textContent = msg;
    statusEl.appendChild(span);
  }

  var SVG = 'http://www.w3.org/2000/svg';
  function icon(name, tone) {
    var svg = document.createElementNS(SVG, 'svg');
    svg.setAttribute('class', 'ico' + (tone ? ' s-' + tone : ''));
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    var use = document.createElementNS(SVG, 'use');
    use.setAttribute('href', 'icons.svg#i-' + name);
    svg.appendChild(use);
    return svg;
  }

  function fmtSize(n) {
    var u = ['B', 'KB', 'MB', 'GB', 'TB'];
    var i = 0; var v = n;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(i ? 1 : 0) + ' ' + u[i];
  }

  function row(f) {
    var li = document.createElement('li');
    li.className = 'xfer';
    var head = document.createElement('div');
    head.className = 'xhead';
    var chip = document.createElement('span');
    var name = document.createElement('span');
    name.className = 'xname';
    var strong = document.createElement('strong');
    strong.textContent = f.name;
    var size = document.createElement('span');
    size.className = 'muted';
    size.textContent = ' ' + fmtSize(f.size);
    name.appendChild(strong);
    name.appendChild(size);
    head.appendChild(chip);
    head.appendChild(name);
    var bar = document.createElement('div');
    bar.className = 'xbar';
    var track = document.createElement('div');
    track.className = 'track';
    track.setAttribute('role', 'progressbar');
    track.setAttribute('aria-label', f.name);
    track.setAttribute('aria-valuemin', '0');
    track.setAttribute('aria-valuemax', '100');
    var fill = document.createElement('div');
    fill.className = 'fill in';
    fill.style.width = '0%';
    track.appendChild(fill);
    var pct = document.createElement('span');
    pct.className = 'pct';
    pct.setAttribute('aria-hidden', 'true');
    bar.appendChild(track);
    bar.appendChild(pct);
    li.appendChild(head);
    li.appendChild(bar);
    list.appendChild(li);
    var r = { chip: chip, track: track, fill: fill, pct: pct, size: f.size, state: '' };
    setState(r, 'waiting');
    setPct(r, 0);
    return r;
  }

  function setState(r, st) {
    if (r.state === st) return;
    r.state = st;
    var s = STATES[st];
    r.chip.className = 'state ' + s.tone;
    r.chip.textContent = '';
    r.chip.appendChild(icon(s.icon));
    var w = document.createElement('span');
    w.textContent = s.word;
    r.chip.appendChild(w);
  }

  function setPct(r, p) {
    r.fill.style.width = p + '%';
    r.pct.textContent = p + '%';
    r.track.setAttribute('aria-valuenow', String(p));
  }

  function setBusy(b) {
    busy = b;
    choose.disabled = b;
    choose.textContent = b ? 'Sending…' : 'Choose files';
  }

  function plural(n) { return n + (n === 1 ? ' file' : ' files'); }

  async function send(files) {
    if (busy || !files.length) return;
    setBusy(true);
    list.textContent = '';
    var rows = files.map(row);
    try {
      say('Waiting for the person at the computer to accept ' + plural(files.length) + '…', 'warn');
      var r = await fetch('drop/offer', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ files: files.map(function (f) { return { name: f.name, size: f.size }; }) }),
      });
      if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
      var reply = await r.json();
      if (!reply.accepted) {
        rows.forEach(function (x) { setState(x, 'notsent'); });
        say('Not sent: ' + (reply.reason || 'the person at the computer said no') + '.', 'warn');
        return;
      }
      say('Accepted. Sending ' + plural(files.length) + '…', 'info');
      await upload(files, rows, reply.token);
      rows.forEach(function (x) { setState(x, 'sent'); setPct(x, 100); });
      say('Done. ' + plural(files.length) + ' sent.', 'ok');
    } catch (e) {
      rows.forEach(function (x) { if (x.state !== 'sent') setState(x, 'failed'); });
      say('Not sent: ' + (e.message || e) + '. You can try again.', 'bad');
    } finally {
      setBusy(false);
    }
  }

  function upload(files, rows, token) {
    return new Promise(function (resolve, reject) {
      var fd = new FormData();
      files.forEach(function (f) { fd.append('file', f, f.name); });
      var xhr = new XMLHttpRequest();
      xhr.open('POST', 'drop/upload?token=' + encodeURIComponent(token));
      xhr.upload.onprogress = function (ev) {
        // Spread overall progress across the files in order (the multipart
        // overhead makes this approximate).
        var left = ev.loaded;
        rows.forEach(function (x) {
          var got = Math.max(0, Math.min(x.size, left));
          left -= x.size;
          var p = x.size ? Math.floor((got * 100) / x.size) : (left >= 0 ? 100 : 0);
          setPct(x, p);
          if (p > 0) setState(x, 'sending');
        });
      };
      xhr.onload = function () {
        if (xhr.status === 200) resolve();
        else reject(new Error((xhr.responseText || xhr.statusText).trim()));
      };
      xhr.onerror = function () { reject(new Error('the connection was lost')); };
      xhr.send(fd);
    });
  }

  // Name the computer: GET api/hello is open to the LAN.
  fetch('api/hello').then(function (r) { return r.ok ? r.json() : null; }).then(function (h) {
    if (!h || !h.name) return;
    document.getElementById('host').textContent = h.name;
    document.getElementById('host2').textContent = h.name;
    document.title = 'rocket - send files to ' + h.name;
  }).catch(function () { /* keep "this computer" */ });

  choose.addEventListener('click', function () { picker.click(); });
  picker.addEventListener('change', function () {
    send(Array.prototype.slice.call(picker.files));
    picker.value = '';
  });
  zone.addEventListener('dragover', function (ev) { ev.preventDefault(); zone.classList.add('over'); });
  zone.addEventListener('dragleave', function () { zone.classList.remove('over'); });
  zone.addEventListener('drop', function (ev) {
    ev.preventDefault();
    zone.classList.remove('over');
    send(Array.prototype.slice.call(ev.dataTransfer.files));
  });
})();
