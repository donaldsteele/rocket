// rocket web UI. Vanilla Alpine component. Talks to the local rocket server:
// peers come from UDP-beacon discovery, transfers are pushed with an accept
// prompt (trust-on-first-use), and progress arrives over an SSE event stream.

// api calls the loopback control API. Every non-GET call carries the
// X-Rocket-CSRF header the server requires.
function api(path, opts) {
  const o = Object.assign({}, opts || {});
  const method = (o.method || 'GET').toUpperCase();
  if (method !== 'GET' && method !== 'HEAD') {
    o.headers = Object.assign({}, o.headers || {}, { 'X-Rocket-CSRF': '1' });
  }
  return fetch(path, o);
}

function app() {
  return {
    self: { name: '', fingerprint: '', downloadDir: '' },
    theme: document.documentElement.getAttribute('data-theme') || 'dark',
    peers: [],
    offers: [],
    xfers: {}, // transfer_id -> {id,name,peer,dir,state,sent,total,file,fileIndex,files}
    ended: {}, // transfer_id -> true once finished, so a late poll cannot revive it
    history: [],
    logLines: [],
    dragOver: false,
    target: null, // peer chosen for "Send files…" / "Send folder…"

    init() {
      this.loadSelf();
      this.pollPeers();
      this.loadIncoming();
      this.loadTransfers();
      this.loadHistory();
      setInterval(() => { this.pollPeers(); this.loadTransfers(); }, 2000);
      this.connectEvents();
    },

    async loadSelf() {
      try { this.self = await (await fetch('api/self')).json(); } catch (e) { /* server starting */ }
    },
    async pollPeers() {
      try {
        const list = await (await fetch('api/peers')).json();
        this.peers = Array.isArray(list) ? list : [];
      } catch (e) { /* transient */ }
    },
    async loadIncoming() {
      try {
        const list = await (await fetch('api/incoming')).json();
        (list || []).forEach((o) => this.addOffer(o));
      } catch (e) { /* none */ }
    },

    // The server is the source of truth for transfers in flight (both
    // directions, keyed by transfer_id); SSE progress fills in between polls.
    async loadTransfers() {
      try {
        const list = await (await fetch('api/transfers')).json();
        const next = {};
        (list || []).forEach((t) => {
          if (this.ended[t.transfer_id]) return;
          next[t.transfer_id] = Object.assign({}, this.xfers[t.transfer_id] || {}, {
            id: t.transfer_id, name: t.name, peer: t.peer_name, dir: t.dir, state: t.state,
            sent: t.sent, total: t.total, file: t.file, fileIndex: t.file_index, files: t.files,
            local: false,
          });
        });
        // Keep local placeholders (a browser upload about to start).
        Object.values(this.xfers).forEach((x) => { if (x.local && !next[x.id]) next[x.id] = x; });
        this.xfers = next;
      } catch (e) { /* transient */ }
    },
    async loadHistory() {
      try {
        const list = await (await fetch('api/history?limit=100')).json();
        this.history = Array.isArray(list) ? list : [];
      } catch (e) { /* transient */ }
    },
    async clearHistory() {
      try {
        const r = await api('api/history/clear', { method: 'POST' });
        if (!r.ok) throw new Error(await r.text());
        this.history = [];
        this.log('history cleared');
      } catch (e) { this.log(`could not clear history: ${e.message || e}`); }
    },
    async cancel(x) {
      try {
        const r = await api('api/cancel', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ transfer_id: x.id }),
        });
        if (!r.ok) throw new Error(await r.text());
        this.log(`cancelling ${this.countWhat(x.name, x.files)}…`);
      } catch (e) { this.log(`could not cancel: ${e.message || e}`); }
    },
    // finish forgets a transfer that ended (any outcome) and refreshes the
    // history, where its entry now is.
    finish(id) {
      this.ended[id] = true;
      delete this.xfers[id];
      this.loadHistory();
    },

    connectEvents() {
      const es = new EventSource('api/events');
      es.onmessage = (ev) => {
        let m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        this.onEvent(m);
      };
      es.onerror = () => { /* browser auto-reconnects */ };
    },
    onEvent(m) {
      const d = m.data || {};
      switch (m.type) {
        case 'offer_gone':
          this.offers = this.offers.filter((x) => x.id !== d.transfer_id);
          if (d.reason !== 'accepted') this.loadHistory();
          break;
        case 'offer':
          this.addOffer(d);
          this.log(`incoming offer: ${this.offerWhat(d)} from ${d.sender_name}`);
          break;
        case 'progress': {
          if (this.ended[d.transfer_id]) break;
          const prev = this.xfers[d.transfer_id] || {};
          this.xfers[d.transfer_id] = Object.assign({}, prev, {
            id: d.transfer_id, name: d.name, dir: d.dir, state: 'active', local: false,
            sent: d.sent, total: d.total, file: d.file, fileIndex: d.file_index, files: d.files,
          });
          break;
        }
        case 'done':
          this.finish(d.transfer_id);
          this.log(`received ${this.countWhat(d.name, d.files)} from ${d.from}`);
          break;
        case 'sent':
          this.finish(d.transfer_id);
          this.log(`sent ${this.countWhat(d.name, d.files)} to ${d.to}`);
          break;
        case 'cancelled':
          this.finish(d.transfer_id);
          this.log(`cancelled ${d.name || 'transfer'}${d.by === 'peer' ? ' (by the other computer)' : ''}`);
          break;
        case 'error':
          this.finish(d.transfer_id);
          this.log(`error on ${d.name || 'transfer'}: ${d.error}`);
          break;
      }
    },
    addOffer(o) {
      if (this.offers.some((x) => x.id === o.id)) return;
      o._trust = false;
      this.offers.push(o);
    },

    async decide(o, accept) {
      this.offers = this.offers.filter((x) => x.id !== o.id);
      try {
        await api('api/accept', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id: o.id, accept, trust: !!o._trust }),
        });
        this.log(`${accept ? 'accepted' : 'declined'} ${this.offerWhat(o)}`);
      } catch (e) { this.log(`could not answer offer: ${e}`); }
    },

    // Send files or a whole folder to a chosen peer: the browser uploads to
    // this host, which relays them on to the peer as one transfer (a tar for
    // several files), so no local file path is needed. The multipart body is
    // the "manifest" field (names, with folder-relative paths, and sizes)
    // followed by the files in the same order.
    pickFor(p, folder) {
      this.target = p;
      (folder ? this.$refs.folder : this.$refs.file).click();
    },
    async onPicked(ref) {
      const input = this.$refs[ref];
      const files = [...input.files];
      const p = this.target;
      input.value = '';
      if (!files.length || !p) return;
      const named = files.map((f) => ({ f, name: f.webkitRelativePath || f.name }));
      const fd = new FormData();
      fd.append('manifest', JSON.stringify({ files: named.map((x) => ({ name: x.name, size: x.f.size })) }));
      named.forEach((x) => fd.append('file', x.f, x.name));
      const label = named[0].name.split('/')[0];
      this.log(`sending ${this.countWhat(label, files.length)} to ${p.name}…`);
      // The transfer (waiting for the receiver, then moving) shows up in
      // GET /api/transfers under its server-side ID, with a Cancel button.
      setTimeout(() => this.loadTransfers(), 300);
      try {
        const r = await api(`api/relay?peer=${encodeURIComponent(p.fp)}`, { method: 'POST', body: fd });
        // 410: cancelled; the "cancelled" event already said so.
        if (!r.ok && r.status !== 410) throw new Error(await r.text());
      } catch (e) {
        this.log(`send failed: ${e.message || e}`);
      }
    },

    // Drag/drop or choose files to send to THIS computer (drop page).
    onDrop(ev) {
      this.dragOver = false;
      this.dropHere([...ev.dataTransfer.files]);
    },
    onDropPick() { this.dropHere([...this.$refs.drop.files]); this.$refs.drop.value = ''; },
    // Same two-step flow as the LAN drop page, all files in one offer; from
    // this computer's own UI (CSRF header present) the server skips the
    // accept prompt.
    async dropHere(files) {
      if (!files.length) return;
      const label = files[0].name;
      const total = files.reduce((n, f) => n + f.size, 0);
      this.log(`uploading ${this.countWhat(label, files.length)} to this computer…`);
      let id = '';
      try {
        const o = await api('drop/offer', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ files: files.map((f) => ({ name: f.name, size: f.size })) }),
        });
        if (!o.ok) throw new Error(await o.text());
        const reply = await o.json();
        if (!reply.accepted) throw new Error(reply.reason || 'declined');
        id = reply.transfer_id;
        if (!this.xfers[id] && !this.ended[id]) {
          this.xfers[id] = { id, name: label, dir: 'in', state: 'active', local: true, sent: 0, total, files: files.length, fileIndex: 0 };
        }
        const fd = new FormData();
        files.forEach((f) => fd.append('file', f, f.name));
        const r = await fetch(`drop/upload?token=${encodeURIComponent(reply.token)}`, { method: 'POST', body: fd });
        if (!r.ok && r.status !== 410) throw new Error(await r.text());
        if (id) delete this.xfers[id]; // the placeholder, if no event removed it
      } catch (e) {
        if (id) delete this.xfers[id];
        this.log(`upload failed: ${e.message || e}`);
      }
    },

    // "photos (12 files)" or just "report.pdf".
    countWhat(name, files) {
      return files > 1 ? `${name} (${files} files)` : name;
    },
    xferList() {
      return Object.values(this.xfers);
    },
    // History result as a word plus a shape (never colour alone).
    resultClass(r) {
      return r === 'ok' ? 'ok' : (r === 'error' ? 'bad' : 'warn');
    },
    resultWord(r) {
      return {
        ok: '✓ done', declined: '✕ declined', cancelled: '■ cancelled', error: '! failed', timeout: '◷ no answer',
      }[r] || r;
    },
    when(ts) {
      const d = new Date(ts);
      return isNaN(d) ? '' : d.toLocaleString();
    },
    offerWhat(o) {
      return o.files > 1 ? `${o.files} files, ${this.fmtSize(o.size) || '0 B'}` : o.name;
    },

    async quit() {
      try { await api('api/quit', { method: 'POST' }); } catch (e) { /* ignore */ }
      this.log('rocket is shutting down. You can close this tab.');
    },

    toggleTheme() {
      this.theme = this.theme === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', this.theme);
      try { localStorage.setItem('rocket.theme', this.theme); } catch (e) { /* blocked */ }
    },

    log(m) {
      const t = new Date().toLocaleTimeString();
      this.logLines.unshift({ t, m });
      if (this.logLines.length > 200) this.logLines.pop();
    },
    pct(a, b) { return b > 0 ? Math.floor((a * 100) / b) : 0; },
    fmtSize(n) {
      if (!n) return '';
      const u = ['B', 'KB', 'MB', 'GB', 'TB'];
      let i = 0; let v = n;
      while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
      return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
    },
  };
}
