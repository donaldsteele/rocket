// rocket web UI: one Alpine component behind the netdiag app shell (sidebar
// views Send, Transfers, History, Settings). Talks only to the loopback
// control API of this rocket: peers come from UDP-beacon and mDNS discovery,
// transfers are pushed with an accept prompt (trust-on-first-use), and
// progress arrives over the SSE event stream, keyed by transfer_id.

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

// jsonBody is the fetch options for a JSON request.
function jsonBody(method, body) {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
}

// errText is the server's plain-text error, or the status line.
async function errText(r) {
  let t = '';
  try { t = (await r.text()).trim(); } catch (e) { /* no body */ }
  return t || `HTTP ${r.status}`;
}

const VIEWS = ['send', 'transfers', 'history', 'settings'];

function app() {
  let saved = 'send';
  try { saved = localStorage.getItem('rocket.view') || 'send'; } catch (e) { /* storage blocked */ }
  return {
    navItems: [
      { id: 'send', label: 'Send', icon: 'send' },
      { id: 'transfers', label: 'Transfers', icon: 'arrow-up-down' },
      { id: 'history', label: 'History', icon: 'history' },
      { id: 'settings', label: 'Settings', icon: 'settings' },
    ],
    view: VIEWS.includes(saved) ? saved : 'send',
    navOpen: false,
    self: { name: '', fingerprint: '', downloadDir: '', port: 0 },
    theme: document.documentElement.getAttribute('data-theme') || 'dark',
    peers: [],
    offers: [],
    xfers: {}, // transfer_id -> {id,name,peer,dir,state,sent,total,file,fileIndex,files,local}
    ended: {}, // transfer_id -> true once finished, so a late poll cannot revive it
    history: [],
    historyError: '',
    logLines: [],
    logSeq: 0,
    announce: '',
    dragOver: false,
    target: null, // peer chosen for "Send files" / "Send folder"
    stopped: false, // Quit was pressed
    offline: false, // the server stopped answering
    // Settings view
    settings: { device_name: '', download_dir: '', auto_accept_trusted: true, restart_required: false },
    draft: { device_name: '', download_dir: '', auto_accept_trusted: true },
    settingsLoaded: false,
    trusted: [],
    saving: false,
    settingsError: '',
    settingsMsg: '',

    init() {
      this.loadSelf();
      this.pollPeers();
      this.loadIncoming();
      this.loadTransfers();
      this.loadHistory();
      this.loadSettings();
      setInterval(() => {
        if (this.stopped) return;
        this.pollPeers();
        this.loadTransfers();
      }, 2000);
      this.connectEvents();
    },

    // ---- navigation ----
    showView(v) {
      if (!VIEWS.includes(v)) v = 'send';
      this.view = v;
      this.navOpen = false;
      try { localStorage.setItem('rocket.view', v); } catch (e) { /* storage blocked */ }
      if (v === 'history') this.loadHistory();
      if (v === 'settings') this.loadSettings();
      // Move focus to the view's heading, so keyboard and screen-reader
      // users land on what they just opened.
      this.$nextTick(() => {
        const h = document.getElementById(`h-${v}`);
        if (h) h.focus();
      });
    },
    closeNav() {
      this.navOpen = false;
      this.$nextTick(() => this.$refs.menuBtn && this.$refs.menuBtn.focus());
    },
    navCount(id) {
      if (id === 'transfers') return this.xferList().length + this.offers.length;
      return 0;
    },
    navCountText(id) {
      if (id !== 'transfers') return '';
      const parts = [];
      const n = this.xferList().length;
      if (n) parts.push(`${n} in progress`);
      if (this.offers.length) parts.push(`${this.offers.length} waiting for you`);
      return `: ${parts.join(', ')}`;
    },

    // ---- loading ----
    async get(path) {
      const r = await fetch(path);
      if (!r.ok) throw new Error(await errText(r));
      this.offline = false;
      return r.json();
    },
    async loadSelf() {
      try { this.self = await this.get('api/self'); } catch (e) { /* server starting */ }
    },
    async pollPeers() {
      try {
        const list = await this.get('api/peers');
        this.peers = Array.isArray(list) ? list : [];
      } catch (e) { this.offline = true; }
    },
    async loadIncoming() {
      try {
        const list = await this.get('api/incoming');
        (list || []).forEach((o) => this.addOffer(o));
      } catch (e) { /* none */ }
    },

    // The server is the source of truth for transfers in flight (both
    // directions, keyed by transfer_id); SSE progress fills in between polls.
    async loadTransfers() {
      try {
        const list = await this.get('api/transfers');
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
        const list = await this.get('api/history?limit=200');
        this.history = Array.isArray(list) ? list : [];
        this.historyError = '';
      } catch (e) { this.historyError = `Could not load history: ${e.message || e}`; }
    },
    async clearHistory() {
      if (!window.confirm('Clear the whole transfer history? Received files are kept.')) return;
      try {
        const r = await api('api/history/clear', { method: 'POST' });
        if (!r.ok) throw new Error(await errText(r));
        this.history = [];
        this.say('History cleared.');
      } catch (e) { this.historyError = `Could not clear history: ${e.message || e}`; }
    },

    // ---- transfers ----
    async cancel(x) {
      try {
        const r = await api('api/cancel', jsonBody('POST', { transfer_id: x.id }));
        if (!r.ok) throw new Error(await errText(r));
        this.log(`cancelling ${this.countWhat(x.name, x.files)}…`);
      } catch (e) { this.log(`could not cancel: ${e.message || e}`); }
    },
    // finish forgets a transfer that ended (any outcome) and refreshes the
    // history, where its entry now is.
    finish(id) {
      this.ended[id] = true;
      delete this.xfers[id];
      this.xfers = Object.assign({}, this.xfers);
      this.loadHistory();
    },

    connectEvents() {
      const es = new EventSource('api/events');
      es.onmessage = (ev) => {
        let m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        this.offline = false;
        this.onEvent(m);
      };
      es.onerror = () => {
        // The browser reconnects by itself; a closed stream means the
        // server is gone (or restarting).
        if (es.readyState === EventSource.CLOSED || this.stopped) es.close();
      };
      es.onopen = () => {
        if (this.offline) { this.loadSelf(); this.loadIncoming(); }
        this.offline = false;
      };
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
          this.say(`Received ${this.countWhat(d.name, d.files)} from ${d.from}.`);
          break;
        case 'sent':
          this.finish(d.transfer_id);
          this.log(`sent ${this.countWhat(d.name, d.files)} to ${d.to}`);
          this.say(`Sent ${this.countWhat(d.name, d.files)} to ${d.to}.`);
          break;
        case 'cancelled':
          this.finish(d.transfer_id);
          this.log(`cancelled ${d.name || 'transfer'}${d.by === 'peer' ? ' (by the other computer)' : ''}`);
          this.say(`Cancelled ${d.name || 'the transfer'}.`);
          break;
        case 'error':
          this.finish(d.transfer_id);
          this.log(`error on ${d.name || 'transfer'}: ${d.error}`);
          this.say(`${d.name || 'A transfer'} failed: ${d.error}`);
          break;
      }
    },
    addOffer(o) {
      if (!o || !o.id || this.offers.some((x) => x.id === o.id)) return;
      this.offers.push(o);
      // Heard by screen readers whether the offer came over SSE or was
      // already waiting when the page loaded.
      this.say(`${o.sender_name} wants to send you ${this.offerWhat(o)}. Accept or decline at the top of the page.`);
    },

    // decide answers an offer: accept, accept and trust the sender's key
    // ("Always accept from this host"), or decline.
    async decide(o, accept, trust) {
      this.offers = this.offers.filter((x) => x.id !== o.id);
      try {
        const r = await api('api/accept', jsonBody('POST', { id: o.id, accept, trust: !!(accept && trust) }));
        if (!r.ok) throw new Error(await errText(r));
        this.log(`${accept ? 'accepted' : 'declined'} ${this.offerWhat(o)} from ${o.sender_name}${trust && accept ? ' (now trusted)' : ''}`);
        if (trust && accept) this.loadTrusted();
      } catch (e) { this.log(`could not answer the offer: ${e.message || e}`); }
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
      this.say(`Sending ${this.countWhat(label, files.length)} to ${p.name}. Progress is in Transfers.`);
      // The transfer (waiting for the receiver, then moving) shows up in
      // GET /api/transfers under its server-side ID, with a Cancel button.
      setTimeout(() => this.loadTransfers(), 300);
      try {
        const r = await api(`api/relay?peer=${encodeURIComponent(p.fp)}`, { method: 'POST', body: fd });
        // 410: cancelled; the "cancelled" event already said so.
        if (!r.ok && r.status !== 410) throw new Error(await errText(r));
      } catch (e) {
        this.log(`send failed: ${e.message || e}`);
        this.say(`Sending to ${p.name} failed: ${e.message || e}`);
      }
    },

    // Drag/drop or choose files to copy into THIS computer's download folder.
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
        const o = await api('drop/offer', jsonBody('POST', { files: files.map((f) => ({ name: f.name, size: f.size })) }));
        if (!o.ok) throw new Error(await errText(o));
        const reply = await o.json();
        if (!reply.accepted) throw new Error(reply.reason || 'declined');
        id = reply.transfer_id;
        if (!this.xfers[id] && !this.ended[id]) {
          this.xfers[id] = { id, name: label, dir: 'in', state: 'active', local: true, sent: 0, total, files: files.length, fileIndex: 0 };
        }
        const fd = new FormData();
        files.forEach((f) => fd.append('file', f, f.name));
        const r = await fetch(`drop/upload?token=${encodeURIComponent(reply.token)}`, { method: 'POST', body: fd });
        if (!r.ok && r.status !== 410) throw new Error(await errText(r));
      } catch (e) {
        this.log(`upload failed: ${e.message || e}`);
        this.say(`Upload failed: ${e.message || e}`);
      } finally {
        if (id && this.xfers[id] && this.xfers[id].local) {
          delete this.xfers[id]; // the placeholder, if no event removed it
          this.xfers = Object.assign({}, this.xfers);
        }
      }
    },

    // ---- settings ----
    async loadSettings() {
      try {
        const s = await this.get('api/settings');
        this.settings = s;
        this.draft = { device_name: s.device_name, download_dir: s.download_dir, auto_accept_trusted: !!s.auto_accept_trusted };
        this.settingsLoaded = true;
        this.settingsError = '';
      } catch (e) { this.settingsError = `Could not load settings: ${e.message || e}`; }
      this.loadTrusted();
    },
    async loadTrusted() {
      try {
        const list = await this.get('api/trusted');
        this.trusted = Array.isArray(list) ? list : [];
      } catch (e) { /* transient */ }
    },
    // saveSettings sends only the fields that changed (PUT accepts partial
    // updates), so an untouched download folder set by a flag stays as is.
    async saveSettings() {
      this.settingsError = '';
      this.settingsMsg = '';
      const body = {};
      const name = this.draft.device_name.trim();
      const dir = this.draft.download_dir.trim();
      if (name !== this.settings.device_name) body.device_name = name;
      if (dir !== this.settings.download_dir) body.download_dir = dir;
      if (this.draft.auto_accept_trusted !== !!this.settings.auto_accept_trusted) body.auto_accept_trusted = this.draft.auto_accept_trusted;
      if (!Object.keys(body).length) { this.settingsMsg = 'Nothing to save: no changes.'; return; }
      this.saving = true;
      try {
        const r = await api('api/settings', jsonBody('PUT', body));
        if (!r.ok) throw new Error(await errText(r));
        const s = await r.json();
        this.settings = s;
        this.draft = { device_name: s.device_name, download_dir: s.download_dir, auto_accept_trusted: !!s.auto_accept_trusted };
        this.settingsMsg = s.restart_required ? 'Saved. The new name is used after rocket restarts.' : 'Saved.';
        if (body.download_dir !== undefined) this.loadSelf();
        this.say(this.settingsMsg);
      } catch (e) {
        this.settingsError = `Could not save: ${e.message || e}`;
      } finally {
        this.saving = false;
      }
    },
    async revoke(t) {
      const who = t.name || t.fingerprint;
      if (!window.confirm(`Stop trusting ${who}? Its next transfer asks you first.`)) return;
      this.settingsError = '';
      try {
        const r = await api('api/trusted/revoke', jsonBody('POST', { fingerprint: t.fingerprint }));
        if (!r.ok) throw new Error(await errText(r));
        this.trusted = this.trusted.filter((x) => x.fingerprint !== t.fingerprint);
        this.settingsMsg = `${who} is no longer trusted.`;
        this.say(this.settingsMsg);
      } catch (e) { this.settingsError = `Could not revoke: ${e.message || e}`; }
    },

    // ---- words, shapes and numbers ----
    // "photos (12 files)" or just "report.pdf".
    countWhat(name, files) {
      return files > 1 ? `${name} (${files} files)` : (name || '');
    },
    xferList() {
      return Object.values(this.xfers);
    },
    xferWord(x) {
      if (x.state === 'waiting') return 'Waiting';
      return x.dir === 'in' ? 'Receiving' : 'Sending';
    },
    xferIcon(x) {
      if (x.state === 'waiting') return 'clock';
      return x.dir === 'in' ? 'arrow-down' : 'arrow-up';
    },
    // History result as a word plus a shape (never colour alone).
    resultClass(r) {
      return { ok: 'ok', error: 'bad' }[r] || 'warn';
    },
    resultIcon(r) {
      return {
        ok: 'circle-check', declined: 'circle-x', cancelled: 'square', error: 'triangle-alert', timeout: 'clock',
      }[r] || 'info';
    },
    resultWord(r) {
      return {
        ok: 'Done', declined: 'Declined', cancelled: 'Cancelled', error: 'Failed', timeout: 'No answer',
      }[r] || r;
    },
    when(ts) {
      const d = new Date(ts);
      return (!ts || isNaN(d)) ? '' : d.toLocaleString();
    },
    offerWhat(o) {
      return o.files > 1 ? `${o.files} files` : (o.name || 'a file');
    },
    namesLine(o) {
      const n = o.names || [];
      return n.join(', ') + (n.length >= 5 ? ', …' : '');
    },

    async quit() {
      if (!window.confirm('Stop rocket on this computer? Transfers in progress are cancelled.')) return;
      try {
        const r = await api('api/quit', { method: 'POST' });
        if (!r.ok) throw new Error(await errText(r));
      } catch (e) { this.log(`could not quit: ${e.message || e}`); return; }
      this.stopped = true;
      this.offers = [];
      this.log('rocket is shutting down. You can close this tab.');
      this.say('rocket has stopped. You can close this tab.');
    },

    toggleTheme() {
      this.theme = this.theme === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', this.theme);
      try { localStorage.setItem('rocket.theme', this.theme); } catch (e) { /* blocked */ }
    },

    log(m) {
      const t = new Date().toLocaleTimeString();
      this.logLines.unshift({ id: ++this.logSeq, t, m });
      if (this.logLines.length > 200) this.logLines.pop();
    },
    // say updates the polite live region. Clearing first makes a repeat of
    // the same sentence heard again.
    say(m) {
      this.announce = '';
      this.$nextTick(() => { this.announce = m; });
    },
    // peerMeta is the second line of a peer row: fingerprint, OS and how it
    // was found (beacon, mdns or both).
    peerMeta(p) {
      const parts = [p.fp];
      if (p.os) parts.push(p.os);
      if (Array.isArray(p.via) && p.via.length) parts.push(`via ${p.via.join(' + ')}`);
      return parts.join(' · ');
    },
    pct(a, b) { return b > 0 ? Math.min(100, Math.floor((a * 100) / b)) : 0; },
    fmtSize(n) {
      if (!n) return '';
      const u = ['B', 'KB', 'MB', 'GB', 'TB'];
      let i = 0; let v = n;
      while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
      return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
    },
  };
}
