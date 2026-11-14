// rocket web UI. Vanilla Alpine component. Talks to the local rocket server:
// peers come from UDP-beacon discovery, transfers are pushed with an accept
// prompt (trust-on-first-use), and progress arrives over an SSE event stream.
function app() {
  return {
    self: { name: '', fingerprint: '', downloadDir: '' },
    theme: document.documentElement.getAttribute('data-theme') || 'dark',
    peers: [],
    offers: [],
    xfers: {}, // name -> {sent,total,dir}
    logLines: [],
    dragOver: false,
    target: null, // peer chosen for "Send file…"

    init() {
      this.loadSelf();
      this.pollPeers();
      this.loadIncoming();
      setInterval(() => this.pollPeers(), 2000);
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
        case 'offer':
          this.addOffer(d);
          this.log(`incoming offer: ${d.name} from ${d.sender_name}`);
          break;
        case 'progress':
          this.xfers[d.name] = { sent: d.sent, total: d.total, dir: d.dir };
          break;
        case 'done':
          delete this.xfers[d.name];
          this.log(`received ${d.name} from ${d.from}`);
          break;
        case 'sent':
          this.log(`sent ${d.name} to ${d.to}`);
          break;
        case 'error':
          delete this.xfers[d.name];
          this.log(`error on ${d.name}: ${d.error}`);
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
        await fetch('api/accept', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id: o.id, accept, trust: !!o._trust }),
        });
        this.log(accept ? `accepted ${o.name}` : `declined ${o.name}`);
      } catch (e) { this.log(`could not answer offer: ${e}`); }
    },

    // Send file to a chosen peer: the browser uploads to this host, which
    // relays it on to the peer, so no local file path is needed.
    pickFor(p) { this.target = p; this.$refs.file.click(); },
    async onPicked() {
      const f = this.$refs.file.files[0];
      if (!f || !this.target) return;
      const p = this.target;
      this.$refs.file.value = '';
      const fd = new FormData();
      fd.append('file', f);
      this.xfers[f.name] = { sent: 0, total: f.size, dir: 'out' };
      this.log(`sending ${f.name} to ${p.name}…`);
      try {
        const r = await fetch(`api/relay?peer=${encodeURIComponent(p.addr || p.Addr || '')}&size=${f.size}`, { method: 'POST', body: fd });
        if (!r.ok) throw new Error(await r.text());
      } catch (e) {
        delete this.xfers[f.name];
        this.log(`send failed: ${e.message || e}`);
      }
    },

    // Drag/drop or choose files to send to THIS computer (drop page).
    onDrop(ev) {
      this.dragOver = false;
      [...ev.dataTransfer.files].forEach((f) => this.dropHere(f));
    },
    onDropPick() { [...this.$refs.drop.files].forEach((f) => this.dropHere(f)); this.$refs.drop.value = ''; },
    async dropHere(f) {
      const fd = new FormData();
      fd.append('file', f);
      this.xfers[f.name] = { sent: 0, total: f.size, dir: 'in' };
      this.log(`uploading ${f.name} to this computer…`);
      try {
        const r = await fetch('drop', { method: 'POST', body: fd });
        if (!r.ok) throw new Error(await r.text());
      } catch (e) {
        delete this.xfers[f.name];
        this.log(`upload failed: ${e.message || e}`);
      }
    },

    async quit() {
      try { await fetch('api/quit', { method: 'POST' }); } catch (e) { /* ignore */ }
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
