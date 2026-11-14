// rocket drop page for browser guests on the LAN. Two steps: POST drop/offer
// with the file list (the person at the computer accepts, unless this browser
// was accepted before), then POST drop/upload?token= with the files as one
// multipart body, in offer order.
(function () {
  'use strict';
  var zone = document.getElementById('zone');
  var picker = document.getElementById('picker');
  var list = document.getElementById('files');
  var statusEl = document.getElementById('status');
  var busy = false;

  function say(msg) { statusEl.textContent = msg; }

  function fmtSize(n) {
    var u = ['B', 'KB', 'MB', 'GB', 'TB'];
    var i = 0; var v = n;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(i ? 1 : 0) + ' ' + u[i];
  }

  function row(f) {
    var li = document.createElement('li');
    li.className = 'xfer';
    var label = document.createElement('div');
    label.className = 'label';
    var name = document.createElement('span');
    name.textContent = f.name + ' (' + fmtSize(f.size) + ')';
    var state = document.createElement('span');
    state.className = 'mono';
    state.textContent = 'waiting';
    label.appendChild(name);
    label.appendChild(state);
    var track = document.createElement('div');
    track.className = 'track';
    var fill = document.createElement('div');
    fill.className = 'fill in';
    fill.style.width = '0%';
    track.appendChild(fill);
    li.appendChild(label);
    li.appendChild(track);
    list.appendChild(li);
    return { state: state, fill: fill, size: f.size };
  }

  async function send(files) {
    if (busy || !files.length) return;
    busy = true;
    list.textContent = '';
    var rows = files.map(row);
    try {
      say('Waiting for the person at this computer to accept...');
      var r = await fetch('drop/offer', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ files: files.map(function (f) { return { name: f.name, size: f.size }; }) }),
      });
      if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
      var reply = await r.json();
      if (!reply.accepted) {
        say('Not sent: ' + (reply.reason || 'declined') + '.');
        rows.forEach(function (x) { x.state.textContent = 'not sent'; });
        return;
      }
      say('Accepted. Sending...');
      await upload(files, rows, reply.token);
      rows.forEach(function (x) { x.state.textContent = 'done'; x.fill.style.width = '100%'; });
      say('Done. ' + files.length + (files.length === 1 ? ' file' : ' files') + ' sent.');
    } catch (e) {
      rows.forEach(function (x) { if (x.state.textContent !== 'done') x.state.textContent = 'failed'; });
      say('Failed: ' + (e.message || e));
    } finally {
      busy = false;
    }
  }

  function upload(files, rows, token) {
    return new Promise(function (resolve, reject) {
      var fd = new FormData();
      files.forEach(function (f) { fd.append('file', f, f.name); });
      var xhr = new XMLHttpRequest();
      xhr.open('POST', 'drop/upload?token=' + encodeURIComponent(token));
      xhr.upload.onprogress = function (ev) {
        // Spread overall progress across the files in order.
        var left = ev.loaded;
        rows.forEach(function (x) {
          var got = Math.max(0, Math.min(x.size, left));
          left -= x.size;
          var pct = x.size ? Math.floor((got * 100) / x.size) : (left >= 0 ? 100 : 0);
          x.fill.style.width = pct + '%';
          x.state.textContent = pct >= 100 ? 'sent' : pct + '%';
        });
      };
      xhr.onload = function () {
        if (xhr.status === 200) resolve();
        else reject(new Error((xhr.responseText || xhr.statusText).trim()));
      };
      xhr.onerror = function () { reject(new Error('connection lost')); };
      xhr.send(fd);
    });
  }

  document.getElementById('choose').addEventListener('click', function () { picker.click(); });
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
