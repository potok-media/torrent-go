'use strict';

const $ = (s, r = document) => r.querySelector(s);
const api = (p, o) => fetch(p, o).then(r => { if (!r.ok) throw new Error(r.status); return r; });
const esc = s => (s || '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

function fmtBytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; n = Number(n);
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)) + ' ' + u[i];
}
const mbps = n => (Number(n) / 1e6).toFixed(1);

/* ---- inline lucide icons ---- */
const ICONS = {
  'cloud-download': '<path d="M12 13v8"/><path d="m8 17 4 4 4-4"/><path d="M20.88 18.09A5 5 0 0 0 18 9h-1.26A8 8 0 1 0 3 16.29"/>',
  grid: '<rect width="7" height="7" x="3" y="3" rx="1"/><rect width="7" height="7" x="14" y="3" rx="1"/><rect width="7" height="7" x="14" y="14" rx="1"/><rect width="7" height="7" x="3" y="14" rx="1"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>',
  'arrow-down': '<path d="M12 5v14"/><path d="m19 12-7 7-7-7"/>',
  'arrow-up': '<path d="M12 19V5"/><path d="m5 12 7-7 7 7"/>',
  peers: '<circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><line x1="8.59" x2="15.42" y1="13.51" y2="17.49"/><line x1="15.41" x2="8.59" y1="6.51" y2="10.49"/>',
  users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>',
  pin: '<path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z"/>',
  trash: '<path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" x2="10" y1="11" y2="17"/><line x1="14" x2="14" y1="11" y2="17"/>',
  info: '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>',
  'arrow-left': '<path d="m12 19-7-7 7-7"/><path d="M19 12H5"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  folder: '<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>',
  file: '<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/>',
  film: '<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M7 3v18"/><path d="M17 3v18"/><path d="M3 7.5h4"/><path d="M17 7.5h4"/><path d="M3 12h18"/><path d="M3 16.5h4"/><path d="M17 16.5h4"/>',
  music: '<path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  clock: '<circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>',
  activity: '<path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/>',
  'chevron-right': '<path d="m9 18 6-6-6-6"/>',
  'chevron-down': '<path d="m6 9 6 6 6-6"/>',
};
const icon = (n, sz = 16, cls = '') =>
  `<svg class="${cls}" width="${sz}" height="${sz}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-.15em" aria-hidden="true">${ICONS[n] || ''}</svg>`;

/* ---- sparklines ---- */
const hist = { down: [], up: [], byHash: {} };
function push(arr, v, cap = 40) { arr.push(v); if (arr.length > cap) arr.shift(); }
function sparkline(vals, w, h, color, fill) {
  if (!vals || vals.length < 2) return `<svg width="${w}" height="${h}"></svg>`;
  const max = Math.max(...vals, 1);
  const pts = vals.map((v, i) => `${((i / (vals.length - 1)) * w).toFixed(1)},${(h - (v / max) * (h - 2) - 1).toFixed(1)}`).join(' ');
  const area = fill ? `<polygon points="0,${h} ${pts} ${w},${h}" fill="${color}" opacity=".13"/>` : '';
  return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">${area}<polyline points="${pts}" fill="none" stroke="${color}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>`;
}

/* ---- posters ---- */
function hueOf(hash) { let h = 0; for (const c of (hash || '')) h = (h * 31 + c.charCodeAt(0)) % 360; return h; }
function posterCss(t) {
  if (t.poster) return `background-image:url('${t.poster.replace(/'/g, '')}')`;
  const h = hueOf(t.hash);
  return `background:linear-gradient(165deg,hsl(${h} 45% 42%),hsl(${(h + 30) % 360} 50% 16%))`;
}
const posterPt = t => t.poster ? '' : `<div class="pt">${esc((t.name || t.hash).slice(0, 26))}</div>`;
function ambientCss(t) { const h = hueOf(t.hash); return `background:linear-gradient(150deg,hsl(${h} 55% 40%),hsl(${(h + 30) % 360} 50% 18%))`; }

const stateInfo = s => s === 'Seeding' ? { p: 'p-seed', c: '#16a06a' } : s === 'Metadata' ? { p: 'p-meta', c: '#d97a06' } : s === 'Downloading' ? { p: 'p-dl', c: '#6459f5' } : s === 'Saved' ? { p: 'p-saved', c: '#b9bcc9' } : { p: 'p-idle', c: '#9aa0b0' };

let query = '';

/* ================= ROUTER ================= */
function currentRoute() {
  if (location.hash === '#/diag') return { name: 'diag' };
  return { name: 'dashboard' };
}

function renderRoute() {
  const r = currentRoute();
  $('#ri-dash').classList.toggle('on', r.name === 'dashboard');
  $('#ri-diag').classList.toggle('on', r.name === 'diag');
  if (r.name === 'diag') renderDiagShell();
  else renderDashboardShell();
  refresh();
}

/* ================= DASHBOARD ================= */
function renderDashboardShell() {
  $('#view').innerHTML = `
    <header class="top">
      <div><div class="eyebrow">Library</div><div class="h1">Torrents</div></div>
      <div class="spark"><span id="dl-spark"></span><div><div class="sv" id="dl-val">—</div><div class="sl">${icon('arrow-down', 13)} MB/s down</div></div></div>
      <div class="spark"><span id="ul-spark"></span><div><div class="sv" id="ul-val">—</div><div class="sl">${icon('arrow-up', 13)} MB/s up</div></div></div>
      <div class="cachebox"><div class="row1"><span>Cache</span><span class="sv mono" id="cache-val" style="font-size:13px">—</span></div><div class="meter"><span id="cache-bar" style="width:0"></span></div></div>
      <div class="search-wrap">${icon('search')}<input id="search" class="search" placeholder="Search" value="${esc(query)}"/></div>
      <button class="add" data-add>${icon('plus')} Add magnet</button>
    </header>
    <div class="body">
      <div id="hero"></div>
      <div class="seclabel">Active <span class="cnt" id="active-cnt">0</span></div>
      <div class="rows" id="rows"></div>
      <div class="empty" id="empty" hidden>No torrents yet — add a magnet to begin.</div>
    </div>`;
  $('#search').addEventListener('input', e => { query = e.target.value.trim().toLowerCase(); refresh(); });
}

function heroHTML(t) {
  const pct = Math.round((t.progress || 0) * 100);
  return `<section class="hero">
    <div class="hback" style="${ambientCss(t)}"></div><div class="hscrim"></div>
    <div class="poster" style="width:120px;height:180px;${posterCss(t)}" data-hash="${t.hash}" data-nav-card>${posterPt(t)}</div>
    <div style="flex:1;min-width:0">
      <span class="badge"><span class="live"></span> ${t.watchers > 0 ? t.watchers + ' watching' : 'Featured'} · ${esc(t.state)}</span>
      <div class="htitle">${esc(t.name || t.hash.slice(0, 12))}</div>
      <div class="hmeta">${esc([t.mediaType === 'tv' ? 'Series' : t.mediaType === 'movie' ? 'Movie' : '', t.downloadMode === 'disk' ? 'disk' : 'stream'].filter(Boolean).join(' · '))}</div>
      <div class="track"><span style="width:${pct}%"></span></div>
      <div class="hstats">
        <span style="color:var(--acc)">${icon('arrow-down')} ${mbps(t.downloadSpeed)} MB/s</span>
        <span>${fmtBytes(t.completedBytes)} / ${fmtBytes(t.sizeBytes)}</span>
        <span style="color:var(--acc)">${pct}%</span>
      </div>
    </div>
    <div style="display:flex;flex-direction:column;gap:8px">
      <button class="iconbtn on" data-info="${t.hash}" title="Info / files">${icon('info', 18)}</button>
      <button class="iconbtn ${t.pinned ? 'on' : ''}" data-pin="${t.hash}" title="Pin">${icon('pin', 18)}</button>
    </div>
  </section>`;
}

function rowHTML(t) {
  const st = stateInfo(t.state);
  const pct = Math.round((t.progress || 0) * 100);
  const dl = t.downloadSpeed > 0 ? `<div class="rstat" style="color:var(--acc)">${icon('arrow-down')} ${mbps(t.downloadSpeed)}</div>` : '';
  const prog = t.state === 'Seeding' ? fmtBytes(t.sizeBytes) + ' · complete' : `${fmtBytes(t.completedBytes)}/${fmtBytes(t.sizeBytes)} · ${pct}%`;
  const sub = [t.mediaType === 'tv' ? 'Series' : t.mediaType === 'movie' ? 'Movie' : null, t.currentFile].filter(Boolean).join(' · ') || t.hash.slice(0, 16);
  if (t.state === 'Saved') {
    return `<div class="row saved-row" data-hash="${t.hash}">
      <span class="stripe" style="background:${st.c}"></span>
      <div class="th" style="${posterCss(t)}">${posterPt(t)}</div>
      <div style="flex:1;min-width:0"><div class="rtitle">${esc(t.name || t.hash.slice(0, 12))}</div><div class="rsub">${esc([t.mediaType === 'tv' ? 'Series' : t.mediaType === 'movie' ? 'Movie' : 'Saved', 'metadata only'].filter(Boolean).join(' · '))}</div></div>
      <span class="rpill ${st.p}">Saved</span>
      <button class="btn" data-download="${t.hash}" style="padding:8px 14px">${icon('arrow-down')} Download</button>
      <button class="ract danger" data-del="${t.hash}" title="Remove">${icon('trash')}</button>
    </div>`;
  }
  return `<div class="row" data-hash="${t.hash}" data-nav-card>
    <span class="stripe" style="background:${st.c}"></span>
    <div class="th" style="${posterCss(t)}"></div>
    <div style="flex:1;min-width:0"><div class="rtitle">${esc(t.name || t.hash.slice(0, 12))}</div><div class="rsub">${esc(sub)}</div></div>
    <span class="rpill ${st.p}">${esc(t.state)}</span>
    <div style="width:170px"><div class="meter" style="width:100%"><span style="width:${pct}%;background:${st.c}"></span></div><div class="rprog">${prog}</div></div>
    ${dl}
    <div class="rstat">${icon('peers')} ${t.activePeers}/${t.seeders}</div>
    <div class="rstat" style="color:${t.watchers ? 'var(--acc)' : 'var(--tx3)'}">${icon('users')} ${t.watchers}</div>
    <button class="ract ${t.pinned ? 'on' : ''}" data-pin="${t.hash}" title="Pin">${icon('pin')}</button>
    <button class="ract danger" data-del="${t.hash}" title="Remove">${icon('trash')}</button>
  </div>`;
}

async function updateDashboard() {
  const [s, list] = await Promise.all([
    api('/api/manage/stats').then(r => r.json()),
    api('/api/manage/torrents').then(r => r.json()),
  ]);
  push(hist.down, s.totalDownload); push(hist.up, s.totalUpload);
  const seen = {};
  (list || []).forEach(t => { hist.byHash[t.hash] = hist.byHash[t.hash] || []; push(hist.byHash[t.hash], t.downloadSpeed); seen[t.hash] = 1; });
  Object.keys(hist.byHash).forEach(h => { if (!seen[h]) delete hist.byHash[h]; });

  $('#dl-val').textContent = mbps(s.totalDownload);
  $('#ul-val').textContent = mbps(s.totalUpload);
  $('#dl-spark').innerHTML = sparkline(hist.down, 80, 30, '#6459f5', false);
  $('#ul-spark').innerHTML = sparkline(hist.up, 80, 30, '#e8890c', false);
  $('#cache-val').innerHTML = `${fmtBytes(s.cacheFilled)}<span style="color:var(--tx3)">/${fmtBytes(s.cacheCapacity)}</span>`;
  $('#cache-bar').style.width = s.cacheCapacity ? Math.min(100, Math.round(s.cacheFilled / s.cacheCapacity * 100)) + '%' : '0';

  let items = (list || []).filter(t => !query || (t.name || '').toLowerCase().includes(query));
  const hero = items.find(t => t.watchers > 0) || items.find(t => t.state === 'Downloading') || items.find(t => t.state !== 'Saved');
  $('#hero').innerHTML = hero ? heroHTML(hero) : '';
  const rest = hero ? items.filter(t => t.hash !== hero.hash) : items;
  $('#rows').innerHTML = rest.map(rowHTML).join('');
  $('#active-cnt').textContent = items.length;
  $('#empty').hidden = items.length > 0;
}

/* ================= TORRENT INFO POPUP ================= */
function buildTree(files) {
  const root = { dirs: {}, files: [] };
  for (const f of files) {
    const parts = f.path.split('/').filter(Boolean);
    let node = root;
    for (let i = 0; i < parts.length - 1; i++) { node.dirs[parts[i]] = node.dirs[parts[i]] || { dirs: {}, files: [] }; node = node.dirs[parts[i]]; }
    node.files.push({ name: parts[parts.length - 1] || f.path, size: f.sizeBytes, done: f.completedBytes });
  }
  return root;
}
function fileIcon(name) {
  if (/\.(mkv|mp4|avi|ts|mov|m2ts)$/i.test(name)) return icon('film');
  if (/\.(mka|aac|ac3|eac3|dts|flac|opus|mp3|m4a|wav)$/i.test(name)) return icon('music');
  return icon('file');
}
// Collapsible tree with indent guides. Folder open/closed state lives in expandedDirs (keyed by full
// path) so it survives the live re-render on every poll tick. Folders are collapsed by default.
function renderTree(node, path) {
  let html = '';
  for (const name of Object.keys(node.dirs).sort()) {
    const key = path ? path + '/' + name : name;
    const open = expandedDirs.has(key);
    html += `<div class="tnode folder" data-dir="${esc(key)}"><span class="chev">${icon(open ? 'chevron-down' : 'chevron-right', 14)}</span><span style="color:#e6b566;display:inline-flex">${icon('folder')}</span><span class="tname">${esc(name)}</span></div>`;
    if (open) html += `<div class="tchildren">${renderTree(node.dirs[name], key)}</div>`;
  }
  for (const f of node.files.slice().sort((a, b) => a.name.localeCompare(b.name))) {
    const complete = f.size > 0 && f.done >= f.size;
    const pct = f.size ? Math.round(f.done / f.size * 100) : 0;
    const status = complete ? `<span style="color:var(--ok)">${icon('check', 14)}</span>` : `<span style="color:var(--acc)">${pct}%</span>`;
    html += `<div class="tnode file"><span class="chev"></span><span style="color:var(--tx3);display:inline-flex">${fileIcon(f.name)}</span><span class="tname">${esc(f.name)}</span><span class="sz">${status} ${fmtBytes(f.size)}</span></div>`;
  }
  return html;
}

let infoHash = null, tinfoData = null;
const expandedDirs = new Set();

async function openTinfo(hash) {
  infoHash = hash; tinfoData = null; expandedDirs.clear();
  $('#tinfo-body').innerHTML = `<div class="empty" style="padding:44px">Loading…</div>`;
  $('#tinfo-modal').hidden = false;
  await refreshTinfo();
}
async function refreshTinfo() {
  if (!infoHash) return;
  const hash = infoHash;
  let d, st;
  try {
    [d, st] = await Promise.all([
      api('/api/manage/torrents/' + hash + '/files').then(r => r.json()),
      api('/api/torrents/' + hash).then(r => r.json()).catch(() => ({})),
    ]);
  } catch { if (infoHash === hash) $('#tinfo-body').innerHTML = `<div class="empty" style="padding:44px">Torrent not found.</div>`; return; }
  if (infoHash !== hash) return; // closed / switched while fetching
  tinfoData = { d, st };
  renderTinfoBody();
}
// Re-render from cached data (poll tick or a folder toggle), preserving scroll position.
function renderTinfoBody() {
  if (!infoHash || !tinfoData) return;
  const sc = document.querySelector('#tinfo-body .tinfo-scroll');
  const top = sc ? sc.scrollTop : 0;
  $('#tinfo-body').innerHTML = tinfoHTML(infoHash, tinfoData.d, tinfoData.st);
  const ns = document.querySelector('#tinfo-body .tinfo-scroll');
  if (ns) ns.scrollTop = top;
}
function tinfoHTML(hash, d, st) {
  const t = { hash, ...d, ...st, poster: d.poster };
  const pct = Math.round((t.progress || 0) * 100);
  const total = (d.files || []).reduce((a, f) => a + (f.sizeBytes || 0), 0);
  const doneB = (d.files || []).reduce((a, f) => a + (f.completedBytes || 0), 0);
  const stt = stateInfo(t.state || 'Metadata');
  const tree = buildTree(d.files || []);
  return `
    <div class="tinfo-head">
      <div class="eyebrow" style="flex:1">Torrent</div>
      <button class="btn ${t.pinned ? 'primary' : ''}" data-pin="${hash}">${icon('pin')} ${t.pinned ? 'Pinned' : 'Pin'}</button>
      <button class="btn" data-del="${hash}">${icon('trash')} Delete</button>
      <button class="iconbtn" data-close title="Close">${icon('x', 18)}</button>
    </div>
    <div class="tinfo-scroll">
      <div class="dhero">
        <div class="hback" style="${ambientCss(t)}"></div><div class="hscrim"></div>
        <div class="poster" style="width:96px;height:144px;${posterCss(t)}">${posterPt(t)}</div>
        <div style="flex:1;min-width:0">
          <span class="rpill ${stt.p}">${esc(t.state || '—')}</span>
          <div class="htitle" style="font-size:21px">${esc(d.name || hash.slice(0, 12))}</div>
          <div class="hmeta">${esc([t.mediaType === 'tv' ? 'Series' : t.mediaType === 'movie' ? 'Movie' : '', (d.files || []).length + ' files', t.downloadMode === 'disk' ? 'disk' : 'stream'].filter(Boolean).join(' · '))}</div>
          <div class="track" style="max-width:520px"><span style="width:${pct}%;background:${stt.c}"></span></div>
          <div class="hstats"><span style="color:var(--acc)">${icon('arrow-down')} ${mbps(t.downloadSpeed || 0)} MB/s</span><span>${fmtBytes(doneB)} / ${fmtBytes(total)}</span><span style="color:var(--acc)">${pct}%</span></div>
        </div>
      </div>
      <div class="dgrid">
        <div class="panel">
          <h3>File tree</h3>
          <div class="tree">${d.ready ? renderTree(tree, '') : '<div class="empty">Waiting for metadata…</div>'}</div>
        </div>
        <div class="panel">
          <h3>Details</h3>
          <div class="kv"><span class="k">State</span><span class="v">${esc(t.state || '—')}</span></div>
          <div class="kv"><span class="k">Size</span><span class="v">${fmtBytes(total)}</span></div>
          <div class="kv"><span class="k">Downloaded</span><span class="v">${fmtBytes(doneB)} · ${pct}%</span></div>
          <div class="kv"><span class="k">Download</span><span class="v" style="color:var(--acc)">${mbps(t.downloadSpeed || 0)} MB/s</span></div>
          <div class="kv"><span class="k">Upload</span><span class="v" style="color:var(--up)">${mbps(t.uploadSpeed || 0)} MB/s</span></div>
          <div class="kv"><span class="k">Peers</span><span class="v">${t.peers != null ? t.peers : '—'}</span></div>
          <div class="kv"><span class="k">Watching</span><span class="v">${t.watchers != null ? t.watchers : 0}</span></div>
          <div class="kv"><span class="k">Mode</span><span class="v">${esc(t.downloadMode || 'stream')}</span></div>
          <div class="kv"><span class="k">Pinned</span><span class="v">${t.pinned ? 'yes' : 'no'}</span></div>
          <div class="kv"><span class="k">Hash</span><span class="v" style="font-size:11px">${hash.slice(0, 16)}…</span></div>
        </div>
      </div>
    </div>`;
}

/* ================= DIAGNOSTICS ================= */
function renderDiagShell() { $('#view').innerHTML = `<div class="detail" id="diag"><div class="empty">Loading…</div></div>`; }

function fmtDuration(s) {
  s = Math.floor(s || 0);
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  if (d) return `${d}d ${h}h`; if (h) return `${h}h ${m}m`; if (m) return `${m}m`; return `${s}s`;
}
const metricCard = (l, v, sub) => `<div class="panel" style="padding:16px 18px"><div style="font-size:12px;color:var(--tx3)">${l}</div><div class="mono" style="font-size:22px;font-weight:600;margin-top:5px">${v}</div><div style="font-size:11px;color:var(--tx3);margin-top:3px">${esc(sub || '')}</div></div>`;
function memBar(label, bytes, max, color) {
  return `<div style="margin-bottom:13px"><div style="display:flex;justify-content:space-between;font-size:13px;margin-bottom:6px"><span>${esc(label)}</span><span class="mono" style="color:var(--tx2)">${fmtBytes(bytes)}</span></div><div class="meter" style="width:100%"><span style="width:${Math.min(100, Math.round(bytes / (max || 1) * 100))}%;background:${color}"></span></div></div>`;
}

async function updateDiag() {
  const d = await api('/api/manage/diagnostics').then(r => r.json());
  const rt = d.runtime, pc = d.pieceCache, disk = d.disk, ss = d.sessions;
  const consumers = [
    ['Piece cache (RAM)', pc.filled, '#6459f5'],
    ['HLS segments', d.hlsCache.bytes, '#8b6cff'],
    ['AAC transcoders (≈est)', d.transcoders.estBytes, '#e8890c'],
    ['Thumbnails', d.thumbCache.bytes, '#16a06a'],
    ['Go heap (in use)', rt.heapAlloc, '#9a9db0'],
  ];
  const maxc = Math.max(...consumers.map(c => c[1]), 1);
  const rows = (d.torrents || []).sort((a, b) => (b.cacheBytes + b.diskBytes) - (a.cacheBytes + a.diskBytes)).map(t => {
    const pct = t.numPieces ? Math.round(t.piecesComplete / t.numPieces * 100) : 0;
    return `<tr>
      <td style="padding:7px 0;max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(t.name)}</td>
      <td class="mono" style="text-align:right;color:var(--acc)">${fmtBytes(t.cacheBytes)}</td>
      <td class="mono" style="text-align:right;color:var(--tx2)">${t.diskBytes ? fmtBytes(t.diskBytes) : '—'}</td>
      <td class="mono" style="text-align:right">${t.activePeers}</td>
      <td class="mono" style="text-align:right">${pct}%</td>
      <td class="mono" style="text-align:right;color:${t.watchers ? 'var(--acc)' : 'var(--tx3)'}">${t.watchers}</td>
      <td style="text-align:right;color:var(--tx3)">${esc(t.downloadMode)}${t.pinned ? ' · pin' : ''}</td>
    </tr>`;
  }).join('');

  $('#diag').innerHTML = `
    <div class="dtop"><div class="h1" style="font-size:22px">Diagnostics</div><div style="flex:1"></div>
      <div class="rstat">${icon('clock')} uptime ${fmtDuration(rt.uptimeSec)}</div></div>
    <div style="display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:14px">
      ${metricCard('Go heap', fmtBytes(rt.heapAlloc), 'in-use objects')}
      ${metricCard('Reserved from OS', fmtBytes(rt.sys), 'runtime total (≈RSS)')}
      ${metricCard('Piece cache', fmtBytes(pc.filled) + ' / ' + fmtBytes(pc.capacity), 'global RAM budget')}
      ${metricCard('Disk used', fmtBytes(disk.used), disk.free ? fmtBytes(disk.free) + ' free' : (disk.dir || 'disk off'))}
    </div>
    <div class="dgrid">
      <div class="panel"><h3>Where memory goes</h3>${consumers.map(c => memBar(c[0], c[1], maxc, c[2])).join('')}
        <div style="font-size:11px;color:var(--tx3);margin-top:6px">Piece cache is the only hard-capped pool; the rest are bounded by count/derived limits.</div>
      </div>
      <div class="panel"><h3>Runtime &amp; limits</h3>
        <div class="kv"><span class="k">Goroutines</span><span class="v">${rt.numGoroutine}</span></div>
        <div class="kv"><span class="k">GC cycles</span><span class="v">${rt.numGC}</span></div>
        <div class="kv"><span class="k">GC CPU</span><span class="v">${(rt.gcCPUFraction * 100).toFixed(2)}%</span></div>
        <div class="kv"><span class="k">Soft mem limit</span><span class="v">${rt.memLimitBytes ? fmtBytes(rt.memLimitBytes) : 'none'}</span></div>
        <div class="kv"><span class="k">HLS seg cache</span><span class="v">${fmtBytes(d.hlsCache.bytes)} · ${d.hlsCache.count} segs</span></div>
        <div class="kv"><span class="k">Thumbnails</span><span class="v">${fmtBytes(d.thumbCache.bytes)} · ${d.thumbCache.count}/${d.thumbCache.max}</span></div>
        <div class="kv"><span class="k">AAC transcoders</span><span class="v">${d.transcoders.count}/${d.transcoders.max}</span></div>
        <div class="kv"><span class="k">Streams</span><span class="v">${ss.active}/${ss.maxStreams} · ${fmtBytes(ss.perStreamBytes)}/ea</span></div>
        <div class="kv"><span class="k">Download dir</span><span class="v" style="font-size:11px">${esc(disk.dir || '—')}</span></div>
      </div>
    </div>
    <div class="panel"><h3>Per-torrent footprint</h3>
      <table style="width:100%;font-size:13px;border-collapse:collapse">
        <tr style="color:var(--tx3);text-align:left;font-weight:400">
          <th style="font-weight:400">Torrent</th><th style="font-weight:400;text-align:right">RAM</th><th style="font-weight:400;text-align:right">Disk</th><th style="font-weight:400;text-align:right">Peers</th><th style="font-weight:400;text-align:right">Pieces</th><th style="font-weight:400;text-align:right">Watch</th><th style="font-weight:400;text-align:right">Mode</th>
        </tr>
        ${rows || '<tr><td colspan="7" style="color:var(--tx3);padding:14px 0">No active torrents.</td></tr>'}
      </table>
    </div>`;
}

/* ================= REFRESH LOOP ================= */
async function refresh() {
  const r = currentRoute();
  try {
    if (r.name === 'diag') await updateDiag();
    else await updateDashboard();
    if (infoHash) await refreshTinfo(); // keep the open info popup live
  } catch { /* transient */ }
}

/* ================= ACTIONS ================= */
function toast(m) { const t = $('#toast'); t.textContent = m; t.hidden = false; clearTimeout(toast._t); toast._t = setTimeout(() => t.hidden = true, 2600); }

document.addEventListener('click', async e => {
  if (e.target.closest('[data-add]')) { openModal(); return; }
  if (e.target.closest('[data-settings]')) { openSettings(); return; }
  const nav = e.target.closest('[data-nav]');
  if (nav) { location.hash = nav.dataset.nav === 'dashboard' ? '#/' : '#/' + nav.dataset.nav; return; }
  if (e.target.closest('[data-close]') || e.target.classList.contains('modal-backdrop')) { closeModals(); return; }

  const stab = e.target.closest('[data-sset-tab]');
  if (stab) { ssetTab = stab.dataset.ssetTab; renderSset(); return; }

  const dir = e.target.closest('[data-dir]');
  if (dir) { const k = dir.dataset.dir; expandedDirs.has(k) ? expandedDirs.delete(k) : expandedDirs.add(k); renderTinfoBody(); return; }

  const info = e.target.closest('[data-info]'); if (info) { e.stopPropagation(); openTinfo(info.dataset.info); return; }
  const dld = e.target.closest('[data-download]');
  if (dld) {
    e.stopPropagation();
    try { await api(`/api/manage/torrents/${dld.dataset.download}/download`, { method: 'POST' }); toast('Downloading to disk'); refresh(); }
    catch { toast('Download failed'); }
    return;
  }
  const pin = e.target.closest('[data-pin]');
  if (pin) {
    e.stopPropagation();
    const on = pin.classList.contains('on') || pin.classList.contains('primary');
    try { await api(`/api/manage/torrents/${pin.dataset.pin}/pin`, { method: on ? 'DELETE' : 'POST' }); toast(on ? 'Unpinned' : 'Pinned'); refresh(); }
    catch { toast('Pin failed'); }
    return;
  }
  const del = e.target.closest('[data-del]');
  if (del) {
    e.stopPropagation();
    if (!confirm('Remove this torrent and free its cache?')) return;
    try { await api(`/api/torrents/${del.dataset.del}`, { method: 'DELETE' }); toast('Removed'); if (infoHash === del.dataset.del) closeModals(); refresh(); }
    catch { toast('Remove failed'); }
    return;
  }
  const card = e.target.closest('[data-nav-card]'); if (card) { openTinfo(card.dataset.hash); return; }
});

/* ---- modals ---- */
function openModal() {
  $('#modal').hidden = false; $('#add-error').hidden = true;
  ['add-title', 'add-poster', 'add-tmdb'].forEach(id => $('#' + id).value = '');
  $('#add-type').value = 'movie';
  updatePosterPrev();
  $('#add-input').focus();
}
function updatePosterPrev() {
  const u = $('#add-poster').value.trim();
  $('#add-poster-prev').style.backgroundImage = u ? `url('${u.replace(/'/g, '')}')` : '';
}
$('#add-poster').addEventListener('input', updatePosterPrev);
$('#add-tmdb-btn').addEventListener('click', async () => {
  const id = $('#add-tmdb').value.trim();
  if (!/^\d+$/.test(id)) { toast('Enter a numeric TMDB id'); return; }
  const btn = $('#add-tmdb-btn'); btn.disabled = true; btn.textContent = '…';
  try {
    const m = await api(`/api/manage/tmdb?id=${id}&type=${$('#add-type').value}`).then(r => r.json());
    if (m.title && !$('#add-title').value.trim()) $('#add-title').value = m.title;
    if (m.poster) { $('#add-poster').value = m.poster; updatePosterPrev(); }
    toast('Fetched from TMDB');
  } catch { toast('TMDB: not configured or not found'); }
  finally { btn.disabled = false; btn.textContent = 'Fetch'; }
});
function closeModals() { document.querySelectorAll('.modal-backdrop').forEach(m => m.hidden = true); $('#add-input').value = ''; infoHash = null; }

// Gather the media metadata from the Add form (shared by "save to library" and "download to disk").
function addBody() {
  const raw = $('#add-input').value.trim();
  if (!raw) return null;
  const isMagnet = /^magnet:/i.test(raw);
  const dn = isMagnet ? decodeURIComponent((raw.match(/[?&]dn=([^&]+)/) || [])[1] || '') : '';
  const body = { title: ($('#add-title').value.trim() || dn || 'Manual add'), mediaType: $('#add-type').value };
  const poster = $('#add-poster').value.trim(); if (poster) body.poster = poster;
  const tmdb = $('#add-tmdb').value.trim(); if (/^\d+$/.test(tmdb)) body.tmdbId = parseInt(tmdb, 10);
  if (isMagnet) body.magnetUri = raw; else body.link = raw;
  return body;
}
async function addAction(url, extra, okMsg) {
  const body = addBody();
  if (!body) return;
  if (extra) Object.assign(body, extra);
  const err = $('#add-error'); err.hidden = true;
  const sub = $('#add-submit'), dl = $('#add-download'); sub.disabled = dl.disabled = true;
  try {
    await api(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    closeModals(); toast(okMsg); refresh();
  } catch {
    err.textContent = 'Failed — check the link (library save needs a magnet).'; err.hidden = false;
  } finally { sub.disabled = dl.disabled = false; }
}
// Add = save metadata only (library). Download to disk = engage + fetch the whole file.
$('#add-submit').addEventListener('click', () => addAction('/api/manage/library', null, 'Saved to library'));
$('#add-download').addEventListener('click', () => addAction('/api/torrents', { downloadMode: 'disk' }, 'Downloading to disk'));

/* ===== Settings shell (light design system) ===== */
const GB = 1024 * 1024 * 1024;
let perStreamBytes = 80 * 1024 * 1024;
let ssetTab = 'main', ssetBudgetGB = 0.25, ssetData = null;
const RAM_MIN = 0.0625, RAM_MAX = 16;
const defaultMode = () => localStorage.getItem('tg_default_mode') || 'stream';

async function openSettings() {
  ssetTab = 'main';
  try {
    const [s, diag] = await Promise.all([
      api('/api/manage/settings').then(r => r.json()),
      api('/api/manage/diagnostics').then(r => r.json()).catch(() => null),
    ]);
    ssetData = { s, diag };
    perStreamBytes = s.perStreamBytes || perStreamBytes;
    ssetBudgetGB = +(s.cacheBudgetBytes / GB).toFixed(4);
  } catch { ssetData = null; }
  renderSset();
  $('#settings-modal').hidden = false;
}

function ramFill() { return Math.min(100, Math.max(0, Math.round((ssetBudgetGB - RAM_MIN) / (RAM_MAX - RAM_MIN) * 100))); }
function ramStreams() { return Math.max(1, Math.floor((ssetBudgetGB * GB) / perStreamBytes)); }

function renderSset() {
  const diag = ssetData ? ssetData.diag : null;
  const NAV = [['main', 'settings', 'Основные'], ['storage', 'folder', 'Хранилище'], ['about', 'info', 'О программе']];
  let content = '';
  if (ssetTab === 'main') {
    content = `
      <div class="sset-sec">
        <div class="sset-sec-h">${icon('activity', 14)} Память</div>
        <div class="sset-card"><div class="sset-block">
          <div class="sset-label">Бюджет RAM для торрентов: <b id="sset-ram-lab">${(+ssetBudgetGB.toFixed(2))} ГБ</b></div>
          <input type="range" class="sset-range" id="sset-ram" min="${RAM_MIN}" max="${RAM_MAX}" step="0.0625" value="${ssetBudgetGB}" style="background:linear-gradient(90deg,#6459f5 ${ramFill()}%,#dfe3e6 ${ramFill()}%)">
          <input type="number" class="sset-input sset-num" id="sset-ram-num" min="${RAM_MIN}" step="0.25" value="${+ssetBudgetGB.toFixed(2)}">
          <div class="sset-note" style="margin-top:14px" id="sset-ram-note">≈ ${ramStreams()} одновременных стримов. Лимит стримов, транскодеров и per-torrent кэш вычисляются из этого значения автоматически.</div>
        </div></div>
      </div>
      <div class="sset-sec">
        <div class="sset-sec-h">${icon('cloud-download', 14)} Добавление</div>
        <div class="sset-card"><div class="sset-row">
          <div><div class="lbl">По умолчанию качать на диск</div><div class="desc">Новые торренты добавляются в режиме disk (качаются целиком, переживают рестарт). Иначе — stream, только под воспроизведение.</div></div>
          <div class="sset-toggle ${defaultMode() === 'disk' ? 'on' : ''}" id="sset-mode"></div>
        </div></div>
      </div>`;
  } else if (ssetTab === 'storage') {
    const disk = diag && diag.disk ? diag.disk : {};
    content = `
      <div class="sset-sec">
        <div class="sset-sec-h">${icon('folder', 14)} Хранилище</div>
        <div class="sset-card"><div class="sset-block">
          <div class="sset-label">Каталог загрузок (disk-режим)</div>
          <input class="sset-input" value="${esc(disk.dir || '')}" readonly>
          <div class="sset-note" style="margin-top:12px">Задаётся через POTOK_DOWNLOAD_DIR при запуске.${disk.used != null ? ' Занято ' + fmtBytes(disk.used) + (disk.free ? ' · свободно ' + fmtBytes(disk.free) : '') + '.' : ''}</div>
        </div></div>
      </div>
      <div class="sset-sec">
        <div class="sset-sec-h">${icon('info', 14)} Метаданные</div>
        <div class="sset-note">Метаданные торрентов и pin/mode хранятся в JSON (catalog.json) в POTOK_DATA_DIR и переживают рестарт.</div>
      </div>`;
  } else {
    const rt = diag && diag.runtime ? diag.runtime : {};
    content = `
      <div class="sset-sec">
        <div class="sset-sec-h">${icon('cloud-download', 14)} TorrentGo</div>
        <div class="sset-card" style="padding:6px 20px">
          <div class="sset-kv"><span class="k">Движок</span><span class="v">TorrentGo v2 · in-process libav</span></div>
          <div class="sset-kv"><span class="k">Аптайм</span><span class="v">${rt.uptimeSec != null ? fmtDuration(rt.uptimeSec) : '—'}</span></div>
          <div class="sset-kv"><span class="k">Активных торрентов</span><span class="v">${diag ? diag.torrents.length : '—'}</span></div>
        </div>
      </div>`;
  }
  $('#sset').innerHTML = `
    <div class="sset-nav">
      <div class="sset-h">Настройки</div>
      ${NAV.map(([id, ic, label]) => `<div class="sset-navi ${ssetTab === id ? 'on' : ''}" data-sset-tab="${id}">${icon(ic, 18)} ${label}</div>`).join('')}
    </div>
    <div class="sset-main">
      <button class="sset-close" data-close aria-label="Закрыть">${icon('x', 18)}</button>
      <div class="sset-content">${content}</div>
      <div class="sset-footer">
        <button class="sset-btn ghost" id="sset-default">По умолчанию</button>
        <button class="sset-btn grey" data-close>Отмена</button>
        <button class="sset-btn primary" id="sset-save">Сохранить</button>
      </div>
    </div>`;
  wireSset();
}

function syncRam(fromNum) {
  const lab = $('#sset-ram-lab'); if (lab) lab.textContent = (+ssetBudgetGB.toFixed(2)) + ' ГБ';
  const ram = $('#sset-ram'); if (ram) { if (!fromNum) ram.value = ssetBudgetGB; ram.style.background = `linear-gradient(90deg,#6459f5 ${ramFill()}%,#dfe3e6 ${ramFill()}%)`; }
  const num = $('#sset-ram-num'); if (num && !fromNum) num.value = +ssetBudgetGB.toFixed(2);
  const note = $('#sset-ram-note'); if (note) note.textContent = `≈ ${ramStreams()} одновременных стримов. Лимит стримов, транскодеров и per-torrent кэш вычисляются из этого значения автоматически.`;
}

function wireSset() {
  const ram = $('#sset-ram'); if (ram) ram.addEventListener('input', e => { ssetBudgetGB = +e.target.value; syncRam(true); });
  const num = $('#sset-ram-num'); if (num) num.addEventListener('input', e => { const v = parseFloat(e.target.value); if (v > 0) { ssetBudgetGB = v; syncRam(true); } });
  const mode = $('#sset-mode'); if (mode) mode.addEventListener('click', () => { const nx = defaultMode() === 'disk' ? 'stream' : 'disk'; localStorage.setItem('tg_default_mode', nx); mode.classList.toggle('on', nx === 'disk'); });
  const def = $('#sset-default'); if (def) def.addEventListener('click', () => { ssetBudgetGB = 0.25; renderSset(); });
  const save = $('#sset-save'); if (save) save.addEventListener('click', saveSset);
}

async function saveSset() {
  const btn = $('#sset-save'); if (btn) { btn.disabled = true; btn.textContent = 'Сохранение…'; }
  try {
    await api('/api/manage/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ cacheBudgetBytes: Math.round(ssetBudgetGB * GB) }) });
    closeModals(); toast('Настройки сохранены'); refresh();
  } catch { if (btn) { btn.disabled = false; btn.textContent = 'Сохранить'; } toast('Не удалось сохранить'); }
}

/* ---- boot ---- */
$('#nav-logo').innerHTML = icon('cloud-download', 22);
$('#ri-dash').innerHTML = icon('grid', 22);
$('#ri-add').innerHTML = icon('plus', 22);
$('#ri-diag').innerHTML = icon('activity', 22);
$('#ri-set').innerHTML = icon('settings', 22);
window.addEventListener('hashchange', renderRoute);
renderRoute();
setInterval(refresh, 1500);
