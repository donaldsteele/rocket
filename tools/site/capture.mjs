// Captures the project-site screenshots (site/assets/shots) from real rocket
// instances. Windows; Node 22+; Edge or Chrome. No npm packages.
//
// Three rocket hosts run on this computer (Studio-PC, Laptop, Living-Room-PC)
// on ports 29310-29339, with --no-mdns and their own data and download folders
// under a work folder. They find each other through beacon packets this script
// sends straight to their beacon ports on this computer, each advertising a
// throttling proxy in front of the real port so that transfers run slowly
// enough to photograph. Offers, transfers, history and settings come from the
// real `rocket send` CLI and the real loopback API, and a headless browser
// driven over the DevTools protocol takes the shots (2x scale).
//
// For rocket 1.1 the beacons also announce a fourth computer, Den-PC, as a
// rocket 1.0 host (no "proto"), so the peer list shows "Needs update"; nothing
// listens on its port. A send from Studio-PC to Laptop goes through
// POST /api/send (which retries), and Laptop's proxy then resets every
// connection, so the send shows "Reconnecting n/5" and finally "Interrupted"
// with Resume in History.
//
// usage:
//   go build -o <tmp>\rocket.exe -ldflags "-X github.com/donaldsteele/rocket/internal/buildinfo.Version=1.1.0" ./cmd/rocket
//   subst R: <work dir>\drive & set CAPTURE_DRIVE=R:   (optional: the UI shows "R:\Downloads\rocket")
//   node tools/site/capture.mjs <tmp>\rocket.exe <work dir> site\assets\shots [browser exe]
//   subst R: /d
// The whole run takes about three minutes: one offer is left unanswered until
// it times out, for the "No answer" line in History, and the cut-off send
// needs about 70 seconds of retries before it counts as interrupted.

import { spawn, execFileSync } from 'node:child_process';
import dgram from 'node:dgram';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

const [BIN, WORK, OUT, BROWSER = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'] = process.argv.slice(2);
// The address the hosts reach each other on. A host-only virtual adapter (Hyper-V/WSL)
// looks like a LAN address in the UI without touching the real network; else loopback.
const HOSTIP = process.env.CAPTURE_IP || Object.entries(os.networkInterfaces())
  .filter(([n]) => /vEthernet/i.test(n)).flatMap(([, a]) => a)
  .find((a) => a.family === 'IPv4' && /^(172\.(1[6-9]|2\d|3[01])|10)\./.test(a.address))?.address || '127.0.0.1';
if (!BIN || !WORK || !OUT) { console.error('usage: node capture.mjs <rocket> <work> <out> [browser]'); process.exit(2); }
const DRIVE = process.env.CAPTURE_DRIVE || path.join(WORK, 'drive');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);
fs.mkdirSync(OUT, { recursive: true });

// ---- hosts ----
const hosts = {
  S: { name: 'Studio-PC', port: 29310, beacon: 29311, proxy: 29312, dl: `${DRIVE}\\Downloads\\rocket` },
  L: { name: 'Laptop', port: 29320, beacon: 29321, proxy: 29322, dl: `${DRIVE}\\Laptop\\Downloads\\rocket` },
  K: { name: 'Living-Room-PC', port: 29330, beacon: 29331, proxy: 29332, dl: `${DRIVE}\\LivingRoom\\Downloads\\rocket` },
};
const procs = [];
for (const h of Object.values(hosts)) {
  h.data = path.join(WORK, 'data', h.name);
  fs.mkdirSync(h.data, { recursive: true });
  // Start from a clean slate but keep node.key, so a rerun with the same work
  // folder keeps the fingerprints the site shows (no trust, history or kept
  // transfers left over from the last run).
  for (const e of fs.readdirSync(h.data)) if (e !== 'node.key') fs.rmSync(path.join(h.data, e), { recursive: true, force: true });
  const out = fs.openSync(path.join(WORK, `${h.name}.log`), 'w');
  h.proc = spawn(BIN, ['ui', '--no-browser', '--no-mdns', '--port', h.port, '--beacon-port', h.beacon,
    '--data-dir', h.data, '--download-dir', h.dl, '--name', h.name].map(String), { stdio: ['ignore', out, out] });
  procs.push(h.proc);
}

// ---- content to send ----
const C = path.join(WORK, 'content');
function mk(rel, size) {
  const p = path.join(C, rel);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  if (!fs.existsSync(p) || fs.statSync(p).size !== size) {
    const fd = fs.openSync(p, 'w');
    // A little non-zero data up front, the rest zero-filled; content does not matter.
    fs.writeSync(fd, Buffer.from(rel.repeat(8)));
    fs.ftruncateSync(fd, size);
    fs.closeSync(fd);
  }
  return p;
}
const MB = 1024 * 1024;
for (let i = 0; i < 24; i++) mk(`vacation-photos/IMG_${2041 + i * 3}.jpg`, Math.round((2.2 + ((i * 7) % 11) / 4) * MB));
['W-2.pdf', '1099-INT.pdf', 'receipts.xlsx', 'donations.pdf', 'property-tax.pdf', 'notes.txt']
  .forEach((n, i) => mk(`tax-documents-2026/${n}`, 90000 + i * 37000));
const drone = mk('drone-footage.mp4', 640 * MB);
const mix = mk('mix-final.wav', 96 * MB);
const backup = mk('old-backup.zip', 12 * MB);
const session = mk('studio-session.mov', 220 * MB);
const recipes = mk('recipes.pdf', Math.round(1.3 * MB));
const phoneFiles = [mk('phone/IMG_4410.HEIC', Math.round(2.8 * MB)), mk('phone/IMG_4411.HEIC', Math.round(3.1 * MB)),
  mk('phone/receipt-hardware-store.pdf', 240000)];

// ---- throttling proxies: client->server bytes at `rate` bytes/s ----
// cut() resets every open connection and, from then on, every new one, as if
// the computer had dropped off the network.
function proxy(listen, target) {
  const ctl = { rate: Infinity, cutOff: false, conns: new Set() };
  ctl.cut = () => { ctl.cutOff = true; for (const c of ctl.conns) c.resetAndDestroy(); };
  const srv = net.createServer((c) => {
    if (ctl.cutOff) { c.resetAndDestroy(); return; }
    ctl.conns.add(c);
    c.on('close', () => ctl.conns.delete(c));
    const s = net.connect({ port: target, host: HOSTIP, localAddress: HOSTIP });
    let allow = 0; let last = Date.now();
    c.on('data', (chunk) => {
      s.write(chunk);
      if (!isFinite(ctl.rate)) return;
      const now = Date.now();
      allow = Math.min(allow + ((now - last) / 1000) * ctl.rate, ctl.rate / 4) - chunk.length;
      last = now;
      if (allow < 0) { c.pause(); setTimeout(() => c.resume(), (-allow / ctl.rate) * 1000); }
    });
    s.pipe(c);
    const end = () => { c.destroy(); s.destroy(); };
    c.on('error', end); s.on('error', end); c.on('close', end); s.on('close', end);
  });
  srv.listen(listen, HOSTIP);
  ctl.srv = srv;
  return ctl;
}
for (const h of Object.values(hosts)) h.throttle = proxy(h.proxy, h.port);

// ---- API helpers ----
async function api(h, p, body, method) {
  const opts = { method: method || (body === undefined ? 'GET' : 'POST'), headers: {} };
  if (opts.method !== 'GET') opts.headers['X-Rocket-CSRF'] = '1';
  if (body !== undefined && !(body instanceof FormData)) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  else if (body instanceof FormData) opts.body = body;
  const r = await fetch(`http://127.0.0.1:${h.port}/${p}`, opts);
  const t = await r.text();
  if (!r.ok) throw new Error(`${h.name} ${p}: ${r.status} ${t}`);
  try { return JSON.parse(t); } catch { return t; }
}
async function until(fn, what, ms = 30000) {
  const end = Date.now() + ms;
  for (;;) {
    try { const v = await fn(); if (v) return v; } catch (e) { /* not yet */ }
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await sleep(250);
  }
}
const offerFor = (h, pred) => until(async () => (await api(h, 'api/incoming')).find(pred), `offer on ${h.name}`);
const decide = (h, o, accept, trust = false) => api(h, 'api/accept', { id: o.id, accept, trust });

// cliSend runs `rocket send` as host `from` to a host:port.
function cliSend(from, addr, paths) {
  return new Promise((resolve) => {
    const p = spawn(BIN, ['send', '--no-mdns', '--data-dir', from.data, '--name', from.name, addr, ...paths], { stdio: 'ignore' });
    procs.push(p);
    p.on('exit', (code) => resolve(code));
  });
}
// relay sends through host h's own UI path (POST api/relay), as the browser does.
async function relay(h, to, files) {
  const fd = new FormData();
  fd.append('manifest', JSON.stringify({ files: files.map((f) => ({ name: f.name, size: fs.statSync(f.path).size })) }));
  for (const f of files) fd.append('file', await fs.openAsBlob(f.path), f.name);
  return api(h, `api/relay?peer=${to.fp}`, fd).catch((e) => log('relay ended:', e.message));
}

// ---- discovery through local beacon packets ----
for (const h of Object.values(hosts)) {
  const hello = await until(() => api(h, 'api/hello'), `${h.name} to start`);
  Object.assign(h, { fp: hello.fp, ver: hello.version, os: hello.os });
}
// Den-PC still runs rocket 1.0: its beacon has no "proto". Nothing listens on its port.
const den = { name: 'Den-PC', fp: 'fc2409036877eb7e', proxy: 29339, ver: '1.0.0', os: 'windows' };
const udp = dgram.createSocket('udp4');
await new Promise((r) => udp.bind({ address: HOSTIP, port: 0 }, r));
function beacons() {
  for (const to of Object.values(hosts)) {
    for (const p of Object.values(hosts)) {
      if (p === to) continue;
      const pkt = Buffer.from(JSON.stringify({ name: p.name, fp: p.fp, port: p.proxy, ver: p.ver, os: p.os, proto: 2 }));
      udp.send(pkt, to.beacon, HOSTIP);
    }
    udp.send(Buffer.from(JSON.stringify({ name: den.name, fp: den.fp, port: den.proxy, ver: den.ver, os: den.os })), to.beacon, HOSTIP);
  }
}
beacons();
const beaconTimer = setInterval(beacons, 1500);
const { S, L, K } = hosts;
await until(async () => (await api(S, 'api/peers')).length === 3, 'peers on Studio-PC');
log('peers visible');

// ---- browser over the DevTools protocol ----
const CDP_PORT = 29390;
const edge = spawn(BROWSER, ['--headless=new', `--remote-debugging-port=${CDP_PORT}`, `--user-data-dir=${path.join(WORK, 'browser')}`,
  '--no-first-run', '--no-default-browser-check', '--disable-extensions', '--hide-scrollbars', '--lang=en-US',
  '--disable-features=msEdgeSidebarV2,msUndersideButton', 'about:blank'], { stdio: 'ignore' });
const ver = await until(async () => (await fetch(`http://127.0.0.1:${CDP_PORT}/json/version`)).json(), 'browser');
const ws = new WebSocket(ver.webSocketDebuggerUrl);
await new Promise((r) => { ws.onopen = r; });
let seq = 0; const waiting = new Map();
ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  if (m.id && waiting.has(m.id)) { const w = waiting.get(m.id); waiting.delete(m.id); m.error ? w.rej(new Error(m.error.message)) : w.res(m.result); }
};
const cdp = (method, params = {}, sessionId) => new Promise((res, rej) => {
  const id = ++seq; waiting.set(id, { res, rej }); ws.send(JSON.stringify({ id, method, params, sessionId }));
});
async function tab(url, theme) {
  const { targetId } = await cdp('Target.createTarget', { url: 'about:blank' });
  const { sessionId } = await cdp('Target.attachToTarget', { targetId, flatten: true });
  const t = { sessionId, send: (m, p) => cdp(m, p, sessionId) };
  await t.send('Page.enable');
  await t.send('Runtime.enable');
  await desktop(t);
  // The saved theme, read by the page's own theme.js before first paint.
  await t.send('Page.addScriptToEvaluateOnNewDocument', { source: `try{localStorage.setItem('rocket.theme','${theme}');localStorage.setItem('rocket.view','send')}catch(e){}` });
  await t.send('Page.navigate', { url });
  await sleep(1500);
  return t;
}
const desktop = (t) => t.send('Emulation.setDeviceMetricsOverride', { width: 1280, height: 800, deviceScaleFactor: 2, mobile: false });
const phone = (t) => t.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
async function js(t, expr) {
  const r = await t.send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(`${expr}: ${r.exceptionDetails.text}`);
  return r.result.value;
}
async function shot(t, name) {
  await t.send('Page.bringToFront');
  await js(t, 'document.fonts.ready.then(() => { if (document.activeElement) document.activeElement.blur(); window.scrollTo(0, 0); return 1; })');
  await sleep(500);
  const { data } = await t.send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(path.join(OUT, `${name}.png`), Buffer.from(data, 'base64'));
  log('shot', name);
}
const view = (t, v) => js(t, `document.getElementById('nav-${v}').click(), 1`);
const theme = (t, want) => js(t, `(() => { if (document.documentElement.getAttribute('data-theme') !== '${want}') document.querySelector('.themebtn').click(); return 1; })()`);

let failed = null;
try {
  // Phase A: an offer to Living-Room-PC that nobody answers (No answer after 2 minutes).
  K.throttle.rate = Infinity;
  const backupDone = relay(S, K, [{ name: 'old-backup.zip', path: backup }]);
  const app = await tab(`http://127.0.0.1:${S.port}/`, 'dark');

  // Phase B: Laptop offers a folder; the prompt on Studio-PC is the hero shot.
  const photos = cliSend(L, `${HOSTIP}:${S.port}`, [path.join(C, 'vacation-photos')]);
  const o1 = await offerFor(S, (o) => o.sender_name === 'Laptop');
  await sleep(1500);
  await theme(app, 'dark'); await shot(app, 'send-dark');
  await theme(app, 'light'); await shot(app, 'send-light');
  await phone(app);
  await shot(app, 'phone-offer-light');
  await theme(app, 'dark'); await shot(app, 'phone-offer-dark');
  await desktop(app);
  await decide(S, o1, true, true); // Always accept from this host
  await photos;
  log('photos sent');

  // Phase C: a folder from Living-Room-PC, declined.
  const tax = cliSend(K, `${HOSTIP}:${S.port}`, [path.join(C, 'tax-documents-2026')]);
  const o2 = await offerFor(S, (o) => o.sender_name === 'Living-Room-PC');
  await sleep(800);
  await decide(S, o2, false);
  await tax;

  // Phase D: transfers in flight, both directions, through the throttles, and
  // a send to Laptop that loses its connection halfway.
  S.throttle.rate = 9 * MB;
  K.throttle.rate = 3 * MB;
  L.throttle.rate = 8 * MB;
  // A fresh page: the Recent activity log restarts empty.
  await app.send('Page.reload'); await sleep(2000);
  const droneDone = cliSend(L, `${HOSTIP}:${S.proxy}`, [drone]); // Laptop is trusted: no prompt
  const mixDone = relay(S, K, [{ name: 'mix-final.wav', path: mix }]);
  const o3 = await offerFor(K, (o) => o.name === 'mix-final.wav');
  await decide(K, o3, true);
  await api(S, 'api/send', { peer: L.fp, paths: [session] });
  const o6 = await offerFor(L, (o) => o.name === 'studio-session.mov');
  await decide(L, o6, true);
  const cutDone = until(async () => {
    const x = (await api(S, 'api/transfers')).find((t) => t.name === 'studio-session.mov');
    if (x && x.sent / x.total > 0.45) { L.throttle.cut(); return true; }
    return false;
  }, 'the send to Laptop to move', 90000);
  await view(app, 'transfers');
  await until(async () => {
    const xs = await api(S, 'api/transfers');
    const d = xs.find((x) => x.name === 'drone-footage.mp4'); const m = xs.find((x) => x.name === 'mix-final.wav');
    const r = xs.find((x) => x.name === 'studio-session.mov');
    return d && m && r && d.sent / d.total > 0.3 && m.sent / m.total > 0.22 && r.state === 'reconnecting' && r.attempt >= 2;
  }, 'transfers to move', 90000);
  await cutDone;
  S.throttle.rate = 1.5 * MB; K.throttle.rate = 0.6 * MB; // hold the bars while shooting
  await theme(app, 'dark'); await shot(app, 'transfers-dark');
  await theme(app, 'light'); await shot(app, 'transfers-light');
  await phone(app);
  await theme(app, 'dark'); await shot(app, 'phone-transfers-dark');
  await desktop(app);
  const dx = (await api(S, 'api/transfers')).find((x) => x.name === 'drone-footage.mp4');
  await api(S, 'api/cancel', { transfer_id: dx.transfer_id });
  K.throttle.rate = Infinity; S.throttle.rate = Infinity;
  await droneDone; await mixDone;
  log('transfers finished');

  // Phase E: the drop page on a phone, through the proxy so it is a LAN guest.
  S.throttle.rate = 0.9 * MB;
  const drop = await tab(`http://${HOSTIP}:${S.proxy}/`, 'dark');
  await phone(drop);
  await drop.send('Page.reload'); await sleep(1500);
  const { root } = await drop.send('DOM.getDocument');
  const { nodeId } = await drop.send('DOM.querySelector', { nodeId: root.nodeId, selector: '#picker' });
  await drop.send('DOM.setFileInputFiles', { nodeId, files: phoneFiles });
  const o4 = await offerFor(S, (o) => o.from_browser);
  await sleep(800);
  await shot(drop, 'phone-drop-waiting-dark');
  await view(app, 'send');
  await theme(app, 'dark'); await shot(app, 'drop-offer-dark');
  await decide(S, o4, true);
  await until(() => js(drop, `document.querySelectorAll('.state.info').length > 0 && parseInt(document.querySelector('.pct').textContent) > 40`), 'drop upload to move');
  S.throttle.rate = 0.2 * MB;
  await js(drop, `document.documentElement.setAttribute('data-theme','light'), 1`);
  await shot(drop, 'phone-drop-sending-light');
  S.throttle.rate = Infinity;
  await until(() => js(drop, `document.querySelector('#status.ok') !== null`), 'drop done');
  await js(drop, `document.documentElement.setAttribute('data-theme','dark'), 1`);
  await shot(drop, 'phone-drop-done-dark');

  // Phase F: a trusted second sender.
  const rec = cliSend(K, `${HOSTIP}:${S.port}`, [recipes]);
  const o5 = await offerFor(S, (o) => o.sender_name === 'Living-Room-PC');
  await decide(S, o5, true, true);
  await rec;

  log('waiting for the unanswered offer to time out');
  await backupDone;
  log('waiting for the send to Laptop to give up');
  await until(async () => (await api(S, 'api/resumable')).outgoing.length > 0, 'an interrupted send', 180000);
  await sleep(1500);

  // History and settings.
  await view(app, 'history'); await sleep(1200);
  await theme(app, 'dark'); await shot(app, 'history-dark');
  await theme(app, 'light'); await shot(app, 'history-light');
  await phone(app); await shot(app, 'phone-history-light');
  await desktop(app);
  await view(app, 'settings'); await sleep(1200);
  await theme(app, 'dark'); await shot(app, 'settings-dark');
  await theme(app, 'light'); await shot(app, 'settings-light');
} catch (e) {
  failed = e;
  console.error(e);
} finally {
  clearInterval(beaconTimer);
  udp.close();
  try { await cdp('Browser.close'); } catch { /* gone */ }
  ws.close();
  for (const h of Object.values(hosts)) {
    try { await api(h, 'api/quit', {}); } catch { /* gone */ }
    h.throttle.srv.close();
  }
  await sleep(1000);
  for (const p of [...procs, edge]) {
    if (p.exitCode === null) { try { execFileSync('taskkill', ['/T', '/F', '/PID', String(p.pid)], { stdio: 'ignore' }); } catch { /* gone */ } }
  }
  process.exit(failed ? 1 : 0);
}
