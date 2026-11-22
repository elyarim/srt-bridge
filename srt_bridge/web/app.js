"use strict";
// Stream IDs, names and addresses come from outside: they are only ever put
// into the page as text (textContent), never as HTML.

const $ = (id) => document.getElementById(id);
let state = null;
let settingsLoaded = false;
const drawn = {}; // what each part showed last, so it is rebuilt only when it changed

const I18N = {
  en: {
    nav_main: "Main",
    nav_matrix: "Matrix",
    nav_outputs: "Outputs",
    nav_remote: "Remote access",
    nav_setup: "Setup",
    nav_log: "Log",
    nav_help: "Help",
    nav_logout: "Log out",
    lbl_ports: "SRT ports",
    lbl_port: "SRT port",
    lbl_live: "Live",
    lbl_recv: "Receivers",
    lbl_in: "In",
    lbl_out: "Out",
    btn_copy: "Copy",
    btn_links: "Links",
    btn_edit: "Edit",
    btn_reset: "Reset",
    btn_off: "✕ Off",
    btn_switch_on: "Switch on",
    lamp_live: "LIVE",
    lamp_waiting: "WAITING",
    lamp_free: "FREE",
    lamp_off: "OFF",
    qr_title: "QR code",
    qr_tab_raw: "Direct / Raw",
    qr_tab_larix: "Larix App",
    qr_tab_haivision: "Haivision Web",
    qr_hint_raw: "Direct SRT URI. For phone camera scanner, select 'Larix App' or 'Haivision Web' above.",
    qr_hint_larix: "Open Larix Broadcaster and scan this QR code to auto-import connection settings.",
    qr_hint_haivision: "Standard web URL for mobile camera and Haivision Play Pro."
  },
  es: {
    nav_main: "Principal",
    nav_matrix: "Matriz",
    nav_outputs: "Salidas",
    nav_remote: "Acceso remoto",
    nav_setup: "Configuración",
    nav_log: "Bitácora",
    nav_help: "Ayuda",
    nav_logout: "Cerrar sesión",
    lbl_ports: "Puertos SRT",
    lbl_port: "Puerto SRT",
    lbl_live: "En vivo",
    lbl_recv: "Receptores",
    lbl_in: "Entrada",
    lbl_out: "Salida",
    btn_copy: "Copiar",
    btn_links: "Enlaces",
    btn_edit: "Editar",
    btn_reset: "Liberar",
    btn_off: "✕ Cortar",
    btn_switch_on: "Activar",
    lamp_live: "EN VIVO",
    lamp_waiting: "ESPERANDO",
    lamp_free: "LIBRE",
    lamp_off: "APAGADO",
    qr_title: "Código QR",
    qr_tab_raw: "SRT Puro",
    qr_tab_larix: "Larix App",
    qr_tab_haivision: "Haivision Web",
    qr_hint_raw: "URI srt:// directo. Para celular selecciona 'Larix App' o 'Haivision Web' arriba.",
    qr_hint_larix: "Abre Larix Broadcaster y escanea este QR para importar la conexión automáticamente.",
    qr_hint_haivision: "Enlace web compatible con Haivision Play Pro y la cámara nativa del celular."
  }
};

let currentLang = "en";
try { currentLang = localStorage.getItem("srt_lang") || "en"; } catch {}

function tr(key, fallback) {
  return (I18N[currentLang] && I18N[currentLang][key]) || fallback || key;
}

function updateLangSwitchUI() {
  const enBtn = $("langEn");
  const esBtn = $("langEs");
  if (enBtn) {
    enBtn.style.background = currentLang === "en" ? "#2158c7" : "transparent";
    enBtn.style.color = currentLang === "en" ? "#fff" : "#8fa0bf";
  }
  if (esBtn) {
    esBtn.style.background = currentLang === "es" ? "#2158c7" : "transparent";
    esBtn.style.color = currentLang === "es" ? "#fff" : "#8fa0bf";
  }
}

function applyLanguageToDOM() {
  updateLangSwitchUI();
  const navMap = {
    main: "nav_main",
    matrix: "nav_matrix",
    outputs: "nav_outputs",
    remote: "nav_remote",
    setup: "nav_setup",
    log: "nav_log",
    help: "nav_help"
  };
  for (const [page, k] of Object.entries(navMap)) {
    const btn = document.querySelector(`[data-go="${page}"]`);
    if (btn) btn.textContent = tr(k, btn.textContent);
  }
  if ($("logoutBtn")) $("logoutBtn").textContent = tr("nav_logout", "Log out");
  if ($("qrTabRaw")) $("qrTabRaw").textContent = tr("qr_tab_raw", "Direct / Raw");
  if ($("qrTabLarix")) $("qrTabLarix").textContent = tr("qr_tab_larix", "Larix App");
  if ($("qrTabHaivision")) $("qrTabHaivision").textContent = tr("qr_tab_haivision", "Haivision Web");
  for (const tile of tiles) {
    if (tile.copyBtn) tile.copyBtn.textContent = tr("btn_copy", "Copy");
    if (tile.links) tile.links.textContent = tr("btn_links", "Links");
    if (tile.edit) tile.edit.textContent = tr("btn_edit", "Edit");
    if (tile.reset && !tile.reset.dataset.armed) tile.reset.textContent = tr("btn_reset", "Reset");
  }
}

function setLanguage(lang) {
  currentLang = lang === "es" ? "es" : "en";
  try { localStorage.setItem("srt_lang", currentLang); } catch {}
  applyLanguageToDOM();
  if (state) render(state);
}

function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) e.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null) continue;
    e.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return e;
}

// Text is only written when it differs, so nothing flickers once a second.
function put(node, text, cls) {
  if (node.textContent !== text) node.textContent = text;
  if (cls !== undefined && node.className !== cls) node.className = cls;
}

// A note for two seconds in a place that is always there: it never moves the layout.
const noteTimers = {};
function note(id, text) {
  const n = $(id);
  n.textContent = text;
  clearTimeout(noteTimers[id]);
  if (text) noteTimers[id] = setTimeout(() => { n.textContent = ""; }, id.endsWith("Err") ? 6000 : 2000);
}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({ error: "unexpected answer" }));
  if (res.status === 401 && data.login) showLogin();
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// ---- login, only over remote access (HTTPS); the bridge PC itself needs none ----
let loginOpen = false;
function showLogin() {
  if (loginOpen) return;
  loginOpen = true;
  for (const d of document.querySelectorAll("dialog[open]")) d.close();
  $("loginErr").textContent = "";
  $("loginPass").value = "";
  $("loginDlg").showModal();
  $("loginPass").focus();
}
// Escape must not close it: behind it there is nothing to see.
$("loginDlg").addEventListener("cancel", (ev) => ev.preventDefault());
$("loginForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    const res = await fetch("/api/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ password: $("loginPass").value }) });
    const data = await res.json().catch(() => ({ error: "unexpected answer" }));
    if (!res.ok) throw new Error(data.error || res.statusText);
    loginOpen = false;
    $("loginDlg").close();
    settingsLoaded = false;
  } catch (e) {
    $("loginErr").textContent = e.message;
    $("loginPass").select();
  }
});
$("logoutBtn").addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST" }).catch(() => {});
  showLogin();
});

async function change(method, path, body, okNote, errNote) {
  try {
    render(await api(method, path, body));
    if (okNote) note(okNote[0], okNote[1]);
    return true;
  } catch (e) {
    if (okNote) note(okNote[0], "");
    note(errNote, e.message);
    return false;
  }
}

const label = (p) => p.name || "Line " + p.slot;
// The WHIP input in the matrix: a source number above every signal.
const WHIP_SOURCE = 100;
const whipOn = () => !!(state.whip && state.whip.enabled);
const whipPlace = () => ({ slot: state.whip.slot, name: state.whip.name || "WHIP" });
// The signal (or the WHIP input) a forward output gets; null = nothing.
function srcOf(source) {
  if (source === WHIP_SOURCE) return state.whip ? whipPlace() : null;
  return source ? state.places[source - 1] || null : null;
}
// The stream ID receivers use for the forward output: the receiver key, or sender key*forward.
const recvName = (p) => p.recvId || p.id + "*" + state.suffixes[0];
// One port per signal: the port says which signal, stream IDs are not used.
const perSignal = () => state.portMode === "signal";
const inUse = (p) => perSignal() || !!p.id;
const range = (r) => String(r).replace("-", "–");
const kbps = (v) => Math.round(v * 1024) + " kbit/s";
const mbps = (v) => (v >= 10 ? v.toFixed(1) : v.toFixed(2)) + " Mbit/s";
const count = (n, one, many) => n + " " + (n === 1 ? one : many);
function pct(part, all) {
  if (!all) return "0 %";
  const v = part / all * 100;
  return (v === 0 ? "0" : v < 0.1 ? "<0.1" : v.toFixed(1)) + " %";
}
function since(t) {
  let s = Math.max(0, Math.floor(Date.now() / 1000) - t);
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  const p = (n) => String(n).padStart(2, "0");
  return h + ":" + p(m) + ":" + p(s);
}
const clock = (t) => new Date(t * 1000).toLocaleTimeString("de-CH");

function changed(key, value) {
  const s = JSON.stringify(value);
  if (drawn[key] === s) return false;
  drawn[key] = s;
  return true;
}

async function copy(text, btn) {
  const was = btn.textContent;
  try { await navigator.clipboard.writeText(text); btn.textContent = "Copied"; }
  catch { btn.textContent = "Failed"; }
  setTimeout(() => { btn.textContent = was; }, 1500);
}

function render(st) {
  state = st;
  renderSide();
  renderTiles();
  renderMatrix();
  renderReceivers();
  renderRejected();
  renderEvents();
  renderNet();
  renderRemote();
  renderAutostart();
  renderShutdown();
  renderOutputs();
  renderKeys();
  renderWhip();
  put($("logWhere"), "· newest first · log file " + state.logFile + " · settings " + state.configFile);
  if (!settingsLoaded) { fillSettings(); settingsLoaded = true; }
}

function renderSide() {
  const used = state.places.filter(inUse).length;
  const live = state.places.filter((p) => p.sender).length;
  let recv = 0, out = 0, inp = 0;
  for (const p of state.places) {
    if (p.sender) inp += p.sender.mbps;
    for (const c of p.receivers.concat(p.forward)) { recv++; out += c.mbps; }
  }
  let wUsed = 0, wLive = 0;
  if (whipOn()) {
    wUsed = 1;
    if (state.whip.live) { wLive = 1; inp += state.whip.kbps / 1024; }
    for (const c of state.whip.receivers) { recv++; out += c.mbps; }
  }
  put($("sidePortLbl"), perSignal() ? tr("lbl_ports", "SRT ports") : tr("lbl_port", "SRT port"));
  put($("sidePort"), range(state.portRange));
  $("sidePort").title = "UDP " + range(state.portRange);
  put($("sideSrt"), state.srtVersion);
  $("sideSrt").parentNode.title = "SRT " + state.srtVersion + " (" + state.srtLibrary + "); devices need SRT " + state.srtVersion + " or newer";
  const sys = state.sys;
  put($("sideCpu"), sys.cpu.toFixed(0) + " %");
  put($("sideCpuProc"), sys.procCpu.toFixed(1) + " %");
  put($("sideRam"), sys.ramPct + " % of " + sys.ramGb.toFixed(0) + " GB");
  put($("sideRamProc"), sys.procMb.toFixed(0) + " MB");
  put($("sideLive"), (live + wLive) + " of " + (used + wUsed));
  put($("sideRecv"), String(recv));
  put($("sideIn"), mbps(inp));
  put($("sideRate"), mbps(out));
  put($("credit"), "Version " + state.version + " · Peter Aellig");
  if (state.srtError) put($("portBadge"), "Not open: " + state.srtError, "chBadge bad");
  else put($("portBadge"), "Listening on UDP " + range(state.portRange), "chBadge ok");
}

// ---- tiles: built once per number of signals, then only their texts change ----
let tiles = [];
function buildTile(slot) {
  const t = { slot };
  const dd = (key, cls) => (t[key] = el("dd", { class: cls || "" }, "–"));
  t.name = el("span", { class: "name" });
  t.lamp = el("span", { class: "lamp" }, el("i"));
  t.lampText = el("span");
  t.lamp.append(t.lampText);
  t.sid = el("span", { class: "sid" });
  t.copyBtn = el("button", { type: "button", class: "sm mini", onclick: () => copy(perSignal() ? String(state.places[slot - 1].port) : state.places[slot - 1].id, t.copyBtn) }, tr("btn_copy", "Copy"));
  t.links = el("button", { type: "button", class: "sm narrow", onclick: () => openLinks(slot) }, tr("btn_links", "Links"));
  t.edit = el("button", { type: "button", class: "sm narrow", onclick: () => openEdit(slot) }, tr("btn_edit", "Edit"));
  t.reset = el("button", { type: "button", class: "sm wide resetBtn", onclick: () => resetSignal(slot, t.reset) }, tr("btn_reset", "Reset"));
  t.reset.title = "clears stream ID, name, passphrases and receiver key of this signal (click twice)";
  t.power = el("button", { type: "button", class: "sm wide powerBtn", onclick: () => togglePower(slot) }, tr("btn_off", "✕ Off"));
  t.root = el("div", { class: "tile" },
    el("div", { class: "tileHead" }, el("span", { class: "no" }, "# " + slot), t.name, t.lamp),
    el("div", { class: "idRow" }, t.sid, t.copyBtn),
    el("dl", { class: "stats" },
      el("dt", null, "bitrate"), dd("rate", "big"),
      el("dt", null, "video"), dd("video"),
      el("dt", null, "audio"), dd("audio"),
      el("dt", null, "loss · drops"), dd("loss"),
      el("dt", null, "RTT · latency"), dd("rtt"),
      el("dt", null, "on air"), dd("air"),
      el("dt", null, "from"), dd("from"),
      el("dt", null, "error"), dd("err"),
      el("div", { class: "sep" }),
      el("div", { class: "out" }, (t.origLbl = el("span", null, "original ")), (t.orig = el("b"))),
      el("div", { class: "out" }, (t.fwdLbl = el("span", null, "forward ")), (t.fwd = el("b")))),
    el("div", { class: "tileActs" }, t.links, t.edit, t.reset, t.power));
  return t;
}

// The power button: on = «✕ Off» switches the signal off; off = «Switch on».
function showPower(btn, off, can, what) {
  put(btn, off ? tr("btn_switch_on", "Switch on") : tr("btn_off", "✕ Off"), "sm wide powerBtn" + (off ? " isOff" : ""));
  btn.disabled = !can;
  btn.title = off ? "switched off: every device of this " + what + " is turned away. Click to switch it on again"
    : "switch this " + what + " off: its devices are disconnected and turned away while they keep calling";
}

function renderTiles() {
  const wOn = whipOn();
  if (tiles.length !== state.places.length || drawn.whipTile !== wOn) {
    tiles = state.places.map((p) => buildTile(p.slot));
    if (!whipTile) whipTile = buildWhipTile();
    $("tiles").replaceChildren(...tiles.map((t) => t.root), ...(wOn ? [whipTile.root] : []));
    $("tiles").classList.toggle("withWhip", wOn);
    $("tiles").style.setProperty("--whipRows", String(Math.max(1, Math.ceil(state.places.length / 4))));
    drawn.whipTile = wOn;
  }
  if (wOn) fillWhipTile();
  for (const p of state.places) {
    const t = tiles[p.slot - 1], s = p.sender;
    const ps = perSignal(), used = inUse(p);
    t.root.className = "tile" + (p.off ? " off" : p.sender ? " live" : used ? "" : " free");
    put(t.name, label(p));
    t.name.title = label(p);
    put(t.lampText, p.off ? tr("lamp_off", "OFF") : s ? tr("lamp_live", "LIVE") : used ? tr("lamp_waiting", "WAITING") : tr("lamp_free", "FREE"));
    t.lamp.className = "lamp" + (p.off ? " bad" : s ? " live" : used ? " wait" : "");
    showPower(t.power, p.off, used || p.off, "signal");
    if (!t.reset.dataset.armed) t.reset.disabled = !p.id && !p.name && !p.passIn;
    put(t.sid, ps ? "UDP " + p.port : p.id || "no stream ID", "sid" + (used ? "" : " none"));
    t.sid.title = ps ? "port of this signal, no stream ID needed" : p.id;
    t.copyBtn.title = ps ? "Copy the port" : "Copy the stream ID";
    t.copyBtn.disabled = t.links.disabled = !used;
    put(t.rate, s ? kbps(s.mbps) : "–");
    const m = s && s.media;
    put(t.video, m ? [m.video, m.videoCodec].filter(Boolean).join(" · ") || "not known" : "–");
    put(t.audio, m ? audioText(m) : "–");
    t.audio.title = m && m.audio.length > 1 ? "channels per track: " + m.audio.map((c) => c || "?").join(" + ") : "";
    // Loss is repaired by SRT; drops are what is really missing in the picture.
    // The last minute only: errors at the start go away by themselves, the totals stay in the tooltip.
    put(t.loss, s ? s.recentLoss + " (" + pct(s.recentLoss, s.recentPkts) + ") · " + s.recentDrops + " (" + pct(s.recentDrops, s.recentPkts) + ")" : "–", s && s.recentDrops ? "bad" : "");
    t.loss.title = s ? "last minute · since connected: loss " + s.loss + " packets (" + pct(s.loss, s.pkts) + ") resent by SRT, drops " + s.drops + " packets (" + pct(s.drops, s.pkts) + ") really missing" : "";
    put(t.rtt, s ? s.rttMs.toFixed(0) + " ms · " + s.latencyMs + " ms" + (s.encrypted ? " · encrypted" : "") : "–");
    put(t.air, s ? since(s.since) + (p.starts > 1 ? " · " + count(p.starts - 1, "restart", "restarts") : "") : p.starts ? count(p.starts, "start", "starts") + ", now off" : "–", !s && p.starts ? "warn" : "");
    put(t.from, s ? s.addr : "–");
    const e = signalError(p.slot);
    put(t.err, e ? e.short : "–", e ? "bad" : "");
    t.err.title = e ? e.reason + " · from " + e.addr + " · " + count(e.count, "try", "tries") : "";
    const src = srcOf(p.source);
    // With a port per signal there is one kind of receiver: whoever listens on the port gets the matrix output.
    const list = ps ? p.forward : p.receivers;
    let o = 0;
    for (const c of list) o += c.mbps;
    put(t.origLbl, ps ? "receivers " : "original out ");
    put(t.fwdLbl, ps ? "they get " : "forward out ");
    put(t.orig, !ps && state.noOriginal ? "switched off" : list.length ? (ps ? list.length : count(list.length, "device fetches", "devices fetch") + " it") + " · " + mbps(o) : ps ? "none" : "nobody fetches it");
    if (ps) put(t.fwd, src ? "#" + src.slot + " " + label(src) : "nothing (not assigned)");
    else put(t.fwd, (src ? "gets #" + src.slot + " " + label(src) : "not assigned") + (p.forward.length ? " · " + count(p.forward.length, "receiver", "receivers") : ""));
    t.fwd.parentNode.title = t.fwd.textContent;
    t.orig.parentNode.title = ps ? t.orig.textContent : "devices that fetch this signal unchanged, with its stream ID (first link in Links): " + t.orig.textContent;
  }
}

// "8 ch · 4 tracks AAC": what arrives, all tracks together.
function audioText(m) {
  if (!m.audio.length) return "none";
  const known = m.audio.every((c) => c > 0);
  const total = m.audio.reduce((a, c) => a + c, 0);
  const tracks = m.audio.length > 1 ? m.audio.length + " tracks " : "";
  return (known ? total + " ch" : total ? "at least " + total + " ch" : "channels not known") + " · " + tracks + m.audioCodec;
}

// The newest attempt for this signal that was turned away in the last 10 seconds.
// Devices try again every second or so, so an error that stays is still going on.
function signalError(slot) {
  const now = Date.now() / 1000;
  return state.rejected.find((r) => r.slot === slot && r.short && now - r.time <= 10) || null;
}

async function togglePower(slot) {
  const p = state.places[slot - 1];
  await change("PUT", "/api/places", { slot, off: !p.off }, ["mainNote", p.off ? "signal #" + slot + " on" : "signal #" + slot + " off"], "mainErr");
}

// Reset needs two clicks on the same button (no dialog: a dialog can be suppressed,
// then nothing would happen). The first click arms it for 3 seconds.
async function resetSignal(slot, btn) {
  if (!btn.dataset.armed) {
    btn.dataset.armed = "1";
    put(btn, "Sure? Reset", "sm wide resetBtn armed");
    btn.disabled = false;
    setTimeout(() => { delete btn.dataset.armed; put(btn, "Reset", "sm wide resetBtn"); }, 3000);
    return;
  }
  delete btn.dataset.armed;
  put(btn, "Reset", "sm wide resetBtn");
  await change("PUT", "/api/places", { slot, id: "", name: "", passIn: "", off: false }, ["mainNote", "signal #" + slot + " reset"], "mainErr");
}

// ---- generated keys: the settings live on this computer (browser storage of the window), not in config.json ----
const KEY_DEFAULTS = { len: 16, letters: "mixed", exclude: "0O1lI" };
function keySettings() {
  let s = {};
  try { s = JSON.parse(localStorage.getItem("srtBridgeKey") || "{}") || {}; } catch { /* storage not available: defaults */ }
  const len = Math.round(Number(s.len));
  return {
    len: len >= 10 && len <= 70 ? len : KEY_DEFAULTS.len,
    letters: ["mixed", "upper", "lower"].includes(s.letters) ? s.letters : KEY_DEFAULTS.letters,
    exclude: typeof s.exclude === "string" ? s.exclude : KEY_DEFAULTS.exclude,
  };
}
function keyAlphabet(s) {
  const lower = "abcdefghijklmnopqrstuvwxyz", upper = lower.toUpperCase();
  const all = (s.letters === "upper" ? upper : s.letters === "lower" ? lower : upper + lower) + "0123456789";
  return [...all].filter((c) => !s.exclude.includes(c));
}
function randomKey(s) {
  const chars = keyAlphabet(s);
  if (chars.length < 8) throw new Error("too few characters are left for a key: allow more in Setup");
  const limit = 256 - (256 % chars.length); // no character is more likely than another
  let key = "";
  while (key.length < s.len) {
    for (const b of crypto.getRandomValues(new Uint8Array(64))) {
      if (b < limit && key.length < s.len) key += chars[b % chars.length];
    }
  }
  return key;
}
// Fills a key field of the Edit window; Save sets it (with the usual question if devices are connected).
function generateKey(field) {
  try { $(field).value = randomKey(keySettings()); $("editErr").textContent = ""; }
  catch (e) { $("editErr").textContent = e.message; }
}
$("editGen").addEventListener("click", () => generateKey("editId"));
$("editRecvGen").addEventListener("click", () => generateKey("editRecv"));
function fillKeyForm() {
  const s = keySettings();
  $("keyLen").value = s.len;
  $("keyCase").value = s.letters;
  $("keyExclude").value = s.exclude;
}
$("keyForm").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const s = { len: Number($("keyLen").value), letters: $("keyCase").value, exclude: $("keyExclude").value.replace(/\s/g, "") };
  if (keyAlphabet(s).length < 8) { note("keyNote", ""); note("keyErr", "too few characters are left: allow more"); return; }
  try { localStorage.setItem("srtBridgeKey", JSON.stringify(s)); }
  catch { note("keyErr", "could not be saved on this computer"); return; }
  fillKeyForm();
  note("keyErr", "");
  note("keyNote", "saved on this computer");
  newKey("genKey", "genErr");
  newKey("rxKey", "rxErr");
});
fillKeyForm();

// ---- the Generate key page: sender key, receiver key, original on or off ----
function newKey(field, err) {
  try { put($(field), randomKey(keySettings())); note(err, ""); }
  catch (e) { put($(field), "–"); note(err, e.message); }
}
function fillSlots(sel, text) {
  const keep = sel.value;
  sel.replaceChildren(...state.places.map((p) => el("option", { value: p.slot }, "#" + p.slot + " " + label(p) + " · " + text(p))));
  if ([...sel.options].some((o) => o.value === keep)) sel.value = keep;
}
function renderKeys() {
  const ps = perSignal();
  $("genSet").disabled = $("genSlot").disabled = ps;
  $("rxSet").disabled = $("rxClear").disabled = $("rxSlot").disabled = $("origSel").disabled = ps;
  if (document.activeElement !== $("origSel")) $("origSel").value = state.noOriginal ? "off" : "allow";
  const view = [ps, state.suffixes[0], state.places.map((p) => [p.slot, label(p), p.id, p.recvId])];
  if (!changed("keys", view)) return;
  fillSlots($("genSlot"), (p) => (p.id ? "now " + p.id : "free"));
  fillSlots($("rxSlot"), (p) => (!p.id ? "free" : p.recvId ? "now " + p.recvId : "none, uses " + recvName(p)));
  put($("genHelp"), ps
    ? "With a port per signal there is no stream ID: the key can only be copied, for example as a passphrase."
    : "A new sender key disconnects the devices of that signal; they must use the new one.");
  put($("rxHelp"), ps
    ? "With a port per signal there is no stream ID: receivers use the port of the signal."
    : "With a receiver key, receivers do not need to know the sender key, and «sender key*" + state.suffixes[0] + "» no longer works. A new key disconnects the receivers of that forward output.");
  const body = el("tbody");
  for (const p of state.places) {
    const what = ps ? "UDP " + p.port : !p.id ? "– (free)" : recvName(p);
    body.append(el("tr", null,
      el("td", null, el("span", { class: "pid" }, "#" + p.slot + " " + label(p))),
      el("td", null, el("code", null, what)),
      el("td", { class: "dim" }, ps || !p.id ? "" : p.recvId ? "receiver key" : "no receiver key: sender key*" + state.suffixes[0])));
  }
  $("rxTable").replaceChildren(body);
}
$("genNew").addEventListener("click", () => newKey("genKey", "genErr"));
$("rxNew").addEventListener("click", () => newKey("rxKey", "rxErr"));
$("genCopy").addEventListener("click", () => { if ($("genKey").textContent !== "–") copy($("genKey").textContent, $("genCopy")); });
$("rxCopy").addEventListener("click", () => { if ($("rxKey").textContent !== "–") copy($("rxKey").textContent, $("rxCopy")); });
$("genSet").addEventListener("click", async () => {
  const id = $("genKey").textContent, slot = Number($("genSlot").value);
  if (id === "–") return;
  const p = state.places[slot - 1];
  const n = (p.sender ? 1 : 0) + p.receivers.length + p.forward.length;
  // The same question as in the Edit window: a new stream ID disconnects the devices of this signal.
  if (n && !confirm(count(n, "device is", "devices are") + " connected to #" + slot + ".\nA new sender key disconnects " + (n === 1 ? "it" : "them") + ". Set it now?")) return;
  await change("PUT", "/api/places", { slot, id }, ["genNote", "set for #" + slot], "genErr");
});
// No question here: only the receivers of that forward output reconnect, and they need the new key anyway.
$("rxSet").addEventListener("click", async () => {
  const recvId = $("rxKey").textContent, slot = Number($("rxSlot").value);
  if (recvId === "–") return;
  await change("PUT", "/api/places", { slot, recvId }, ["rxNote", "set for #" + slot], "rxErr");
});
$("rxClear").addEventListener("click", async () => {
  const slot = Number($("rxSlot").value);
  await change("PUT", "/api/places", { slot, recvId: "" }, ["rxNote", "removed for #" + slot], "rxErr");
});
$("origSel").addEventListener("change", async () => {
  const allow = $("origSel").value === "allow";
  await change("PUT", "/api/original", { allow }, ["genNote", allow ? "original allowed" : "original off"], "genErr");
});
newKey("genKey", "genErr");
newKey("rxKey", "rxErr");

// ---- matrix: always all signals ----
function renderMatrix() {
  const view = state.places.map((p) => [p.id, p.name, p.source, !!p.sender, p.forward.length]);
  const wView = whipOn() ? [state.whip.name, state.whip.live, state.whip.id] : null;
  if (!changed("mx", [view, wView, state.suffixes[0], state.portMode, state.srtPort])) return;
  const ps = perSignal(), outWord = ps ? "output" : "forward output";
  put($("mxSub"), "· row = " + outWord + ", column = input · one input per row");
  put($("mxLeg"), "Set the matrix before the show. Changing a crosspoint reconnects the receivers of that " + outWord + ". «all ↓» gives one input to every " + outWord + "."
    + (ps ? " A row is what a device gets when it listens on the port of that signal." : " A row is fetched with the stream ID of that signal plus *" + state.suffixes[0] + "."));
  // The columns: every signal, and the WHIP input after them when it is on.
  const inputs = state.places.map((p) => ({ src: p.slot, p, live: !!p.sender, st: p.sender ? "LIVE" : inUse(p) ? "no input" : "free", where: ps ? "UDP " + p.port : p.id }));
  if (whipOn()) inputs.push({ src: WHIP_SOURCE, p: whipPlace(), live: state.whip.live, st: state.whip.live ? "LIVE" : "no input", where: "WHIP", whip: true });
  const head = el("tr", null, el("th", { class: "corner" }, outWord + " ↓  ·  input →"));
  for (const inp of inputs) {
    const p = inp.p;
    head.append(el("th", { class: (inp.live ? "live" : "") + (inp.whip ? " whipCol" : ""), title: label(p) + (inp.where ? " · " + inp.where : "") },
      el("span", { class: "no" }, "#" + p.slot), label(p), el("span", { class: "st" }, inp.st),
      el("button", { type: "button", class: "allBtn", title: "give #" + p.slot + " " + label(p) + " to every " + outWord, onclick: () => setColumn(inp.src) }, "all ↓")));
  }
  const body = el("tbody");
  for (const out of state.places) {
    const n = out.forward.length;
    const addr = ps ? "UDP " + out.port : out.id ? recvName(out) : "";
    const tr = el("tr", null, el("th", { class: n ? "live" : "", title: addr },
      el("span", { class: "no" }, "#" + out.slot), label(out),
      el("span", { class: "st" }, addr ? addr + (n ? " · " + count(n, "receiver", "receivers") : "") : "free")));
    for (const inp of inputs) {
      const on = out.source === inp.src;
      tr.append(el("td", { class: out.slot === inp.src ? "self" : inp.whip ? "whipCol" : "" }, el("button", {
        type: "button", class: "cell" + (on ? " on" : ""),
        "aria-pressed": on ? "true" : "false",
        title: (ps ? "output" : "forward") + " of #" + out.slot + " " + label(out) + (on ? " gets #" : " ← #") + inp.p.slot + " " + label(inp.p),
        onclick: () => setSource(out, on ? 0 : inp.src),
      }, on ? "●" : "")));
    }
    body.append(tr);
  }
  $("mx").replaceChildren(el("thead", null, head), body);
}

async function setSource(out, source) {
  // No question first: a dialog can be suppressed, and then nothing would switch.
  await change("PUT", "/api/places", { slot: out.slot, source }, ["mxNote", "saved"], "mxErr");
}

// One input to every forward output. Rows that have it already are left alone.
async function setColumn(source) {
  for (const out of state.places.filter((o) => o.source !== source)) {
    if (!await change("PUT", "/api/places", { slot: out.slot, source }, ["mxNote", "saved"], "mxErr")) return;
  }
}

// ---- receivers: rows are rebuilt when a device comes or goes, otherwise only the figures change ----
const recvCells = new Map();
function renderReceivers() {
  const rows = [];
  for (const p of state.places) {
    for (const c of p.receivers) rows.push([p, "original", c]);
    for (const c of p.forward) rows.push([p, "forward", c]);
  }
  const fill = (c, cells) => {
    put(cells[0], kbps(c.mbps));
    put(cells[1], c.rttMs.toFixed(0) + " ms");
    put(cells[2], c.latencyMs + " ms");
    put(cells[3], c.recentLoss + " pkt (" + pct(c.recentLoss, c.recentPkts) + ")");
    put(cells[4], c.recentDrops + " pkt (" + pct(c.recentDrops, c.recentPkts) + ")", "num" + (c.recentDrops ? " bad" : ""));
    cells[3].title = "last minute · since connected: " + c.loss + " pkt (" + pct(c.loss, c.pkts) + ")";
    cells[4].title = "last minute · since connected: " + c.drops + " pkt (" + pct(c.drops, c.pkts) + ")";
    put(cells[5], since(c.since));
  };
  const ps = perSignal();
  if (!changed("recv", [state.portMode, state.srtPort, rows.map(([p, kind, c]) => [p.slot, p.name, p.id, p.source, kind, c.id])])) {
    for (const [, , c] of rows) if (recvCells.has(c.id)) fill(c, recvCells.get(c.id));
    return;
  }
  recvCells.clear();
  const tb = $("recvTable");
  if (!rows.length) { tb.replaceChildren(el("tr", null, el("td", { class: "empty" }, "No receiver is connected."))); return; }
  const body = el("tbody");
  for (const [p, kind, c] of rows) {
    const cells = [0, 1, 2, 3, 4, 5].map(() => el("td", { class: "num" }));
    recvCells.set(c.id, cells);
    fill(c, cells);
    const src = kind === "forward" ? srcOf(p.source) : null;
    body.append(el("tr", null,
      el("td", null, el("span", { class: "pid" }, "#" + p.slot + " " + label(p)), el("span", { class: "sub" }, ps ? "UDP " + p.port : kind === "forward" ? recvName(p) : p.id)),
      el("td", null, kind === "forward" ? (ps ? "" : "forward: ") + (src ? "#" + src.slot + " " + label(src) : "not assigned") : "original"),
      el("td", null, c.addr + (c.encrypted ? " · encrypted" : "")), cells));
  }
  tb.replaceChildren(el("thead", null, el("tr", null, ...["Signal", "Gets", "Device", "Bitrate", "RTT", "Latency", "Resent · 1 min", "Drops · 1 min", "Connected"].map((h, i) => el("th", { class: i > 2 ? "num" : "" }, h)))), body);
}

function renderRejected() {
  $("rejCard").hidden = !state.rejected.length;
  if (!changed("rej", state.rejected)) return;
  const body = el("tbody");
  for (const r of state.rejected) {
    body.append(el("tr", null,
      el("td", null, clock(r.time)), el("td", null, el("span", { class: "pid" }, r.streamId || "(none)")),
      el("td", null, r.addr), el("td", null, r.reason), el("td", { class: "num" }, r.count),
      el("td", { class: "acts" }, r.canAdd ? el("button", {
        type: "button", class: "sm primary",
        onclick: () => change("POST", "/api/places", { id: placeOf(r.streamId) }, ["mainNote", "added"], "mainErr"),
      }, "Use a free signal") : null)));
  }
  $("rejected").replaceChildren(el("thead", null, el("tr", null,
    ...["Last", "Stream ID", "From", "Reason", "Tries", ""].map((h) => el("th", null, h)))), body);
}

// The ID a stream ID belongs to: without the forward suffix.
function placeOf(streamId) {
  const i = streamId.lastIndexOf("*");
  if (i >= 0 && state.suffixes.includes(streamId.slice(i + 1).toLowerCase())) return streamId.slice(0, i);
  return streamId;
}

function renderEvents() {
  if (!changed("events", state.events.length ? [state.events.length, state.events[0]] : 0)) return;
  const tb = $("events");
  if (!state.events.length) { tb.replaceChildren(el("tr", null, el("td", { class: "empty" }, "Nothing yet."))); return; }
  tb.replaceChildren(...state.events.map((e) => el("tr", null, el("td", { class: "num" }, clock(e.time)), el("td", null, e.text))));
}

// ---- settings ----
function fillSettings() {
  $("setMode").value = state.portMode;
  $("setPort").value = state.srtPort;
  $("setLatency").value = state.latencyMs;
  $("setSlots").value = state.slots;
  $("setSuffixes").value = state.suffixes.join(", ");
  $("setAccept").value = state.acceptAny ? "any" : "listed";
  settingsMode();
  firewallButtons();
}

// The form follows the port mode that is chosen in it, before it is saved.
// Fields that do not apply are switched off, not removed: nothing moves.
function settingsMode() {
  const ps = $("setMode").value === "signal";
  const first = Number($("setPort").value), n = Number($("setSlots").value);
  put($("setPortLbl"), ps ? "First UDP port (signal 1)" : "UDP port (sending and receiving)");
  $("setSuffixes").disabled = $("setAccept").disabled = ps;
  put($("setSuffixesLbl"), ps ? "Forward suffixes · not used with a port per signal" : "Forward suffixes, separated by commas (the first one is used in the links)");
  put($("setAcceptLbl"), ps ? "Unknown stream IDs · not used with a port per signal" : "Unknown stream IDs");
  const span = first > 0 && n > 0 ? "UDP " + first + (n > 1 ? "–" + (first + n - 1) : "") : "the ports";
  put($("setHelp"), ps
    ? "Signals 1 to " + (n || "n") + " listen on " + span + ", one port each. A device needs no stream ID: whoever sends to a port is the input of that signal, whoever only listens there gets what the matrix assigns to it. A device that asks for a higher latency gets the higher one. A new mode, port, number of signals or latency reconnects every device; the firewall rule and the port forwarding must cover all these ports."
    : "All signals share this one port; the stream ID says which signal a device means, and the stream ID plus *forward fetches what the matrix assigns. A device that asks for a higher latency gets the higher one. A new mode, port or latency reconnects every device.");
}
for (const id of ["setMode", "setPort", "setSlots"]) $(id).addEventListener("input", settingsMode);

$("setForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const port = Number($("setPort").value), latency = Number($("setLatency").value), mode = $("setMode").value;
  const restart = port !== state.srtPort || latency !== state.latencyMs || mode !== state.portMode
    || (mode === "signal" && Number($("setSlots").value) !== state.slots);
  if (restart && !confirm("This reconnects every device. Save now?")) return;
  const oldPorts = state.portRange;
  const ok = await change("PUT", "/api/settings", {
    srtPort: port, portMode: mode, latencyMs: latency, slots: Number($("setSlots").value),
    suffixes: $("setSuffixes").value.split(","),
    acceptAny: $("setAccept").value === "any",
  }, ["setNote", "saved"], "setErr");
  if (ok) {
    fillSettings();
    if (state.portRange !== oldPorts) { note("setNote", "saved · now set the firewall rule for UDP " + range(state.portRange)); checkFirewall(); }
  }
});

// ---- internet access: firewall rule, port forwarding hint, public IP ----
// The firewall is read while the Setup page is open, not all the time.
let detectedIp = "";
async function checkFirewall() {
  if (document.querySelector('section[data-page="setup"]').hidden) return;
  try {
    const f = await api("GET", "/api/firewall");
    const ports = "UDP " + range(f.ports) + (f.webPort ? " and TCP " + f.webPort : "") + (f.whipPort ? ", WHIP TCP/UDP " + f.whipPort : "");
    // The same state twice: in the card «Internet access» and next to the port in the card «SRT».
    for (const n of ["", "2"]) {
      const lamp = $("fwLamp" + n), text = $("fwText" + n);
      if (f.match) { lamp.className = "lamp live"; put(text, "OPEN: rules for " + ports + " in place"); }
      else if (f.udpMatch && f.webPort && !f.webMatch) { lamp.className = "lamp wait"; put(text, "TCP " + f.webPort + " for remote access not open yet: set the rule again"); }
      else if (f.udpMatch && f.whipPort && !f.whipMatch) { lamp.className = "lamp wait"; put(text, "WHIP port " + f.whipPort + " (TCP and UDP) not open yet: set the rule again"); }
      else if (f.rule) { lamp.className = "lamp wait"; put(text, "OTHER PORTS: rule does not open UDP " + range(f.ports)); }
      else if (f.detail) { lamp.className = "lamp"; put(text, "could not be read: " + f.detail); }
      else { lamp.className = "lamp bad"; put(text, "CLOSED: no rule for " + ports); }
      put($("fwBtn" + n), f.match ? "Set firewall rule again" : "Set firewall rule");
    }
  } catch { /* the badge at the top already says that the bridge does not answer */ }
}

// The rule is set for the saved ports: with a new port in the form, save first.
function firewallButtons() {
  const ps = $("setMode").value === "signal";
  const unsaved = Number($("setPort").value) !== state.srtPort || $("setMode").value !== state.portMode
    || (ps && Number($("setSlots").value) !== state.slots);
  $("fwBtn2").disabled = unsaved || viewerRemote();
  $("fwBtn2").title = unsaved ? "Save the new port first; then set the rule for it" : "Opens the saved SRT port in the Windows Firewall; Windows asks for confirmation as administrator";
}
for (const id of ["setMode", "setPort", "setSlots"]) $(id).addEventListener("input", firewallButtons);

async function setFirewall(okNote, errNote) {
  try {
    await api("POST", "/api/firewall", {});
    note(okNote, "confirm in the Windows window");
  } catch (e) { note(errNote, e.message); }
}
setInterval(checkFirewall, 3000);

function renderNet() {
  const ps = perSignal(), ports = "UDP " + range(state.portRange);
  const web = state.remote && state.remote.enabled;
  const wp = whipOn() ? state.whip.port : 0;
  put($("fwdPort"), ports + (web ? " and TCP " + state.srtPort : "") + (wp ? ", TCP and UDP " + wp : ""));
  put($("fwdOnly"), [web ? "TCP " + state.srtPort + " is for remote access." : "", wp ? "TCP and UDP " + wp + " are for the WHIP input." : "", !web && !wp ? "Only UDP is needed." : ""].filter(Boolean).join(" "));
  put($("fwdHost"), (state.addresses.length ? state.addresses.join(" or ") : "its local address") + (ps ? ", same ports " : ", port ") + range(state.portRange));
  put($("fwHelp"), ps
    ? "Opens " + ports + " for incoming connections, one port per signal. Windows asks for confirmation as administrator. After a new first port or number of signals, add the rule again."
    : "Opens the SRT port (" + ports + ") for incoming connections. Windows asks for confirmation as administrator. After a new port, add the rule again.");
  put($("fwdHelp"), ps
    ? "All " + state.slots + " ports are needed, as a range: the port a device calls says which signal it means. Signal 1 is " + state.srtPort + ", signal " + state.slots + " is " + (state.srtPort + state.slots - 1) + "."
    : "One port is enough: every signal uses it, the stream ID tells them apart.");
  const ip = $("publicIp");
  if (!ip.dataset.edited && document.activeElement !== ip) ip.value = state.publicIp;
  const moved = state.publicIp && detectedIp && state.publicIp !== detectedIp;
  put($("ipHint"), moved
    ? "The saved public IP " + state.publicIp + " is not the one found now (" + detectedIp + "): another place? Save the new one."
    : "The address of this site on the internet. It is offered in the Links window of every signal.");
}
$("publicIp").addEventListener("input", () => { $("publicIp").dataset.edited = "1"; });

$("fwBtn").addEventListener("click", () => setFirewall("netNote", "netErr"));
$("fwBtn2").addEventListener("click", () => setFirewall("setNote", "setErr"));

$("ipDetect").addEventListener("click", async () => {
  const btn = $("ipDetect");
  btn.disabled = true;
  try {
    detectedIp = (await api("POST", "/api/public-ip/detect", {})).ip;
    $("publicIp").value = detectedIp;
    $("publicIp").dataset.edited = "1";
    note("netNote", "found " + detectedIp + ", not saved yet");
  } catch (e) { note("netErr", e.message); }
  btn.disabled = false;
});

$("ipForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const ok = await change("PUT", "/api/public-ip", { ip: $("publicIp").value.trim() }, ["netNote", "saved"], "netErr");
  if (ok) { delete $("publicIp").dataset.edited; $("publicIp").value = state.publicIp; }
});

$("rejClear").addEventListener("click", () => change("DELETE", "/api/rejected", {}, null, "mainErr"));

// ---- links ----
let linkSlot = 0;
function openLinks(slot) {
  linkSlot = slot;
  const sel = $("linkAddr");
  const keep = sel.value;
  const local = state.addresses.length ? state.addresses : ["127.0.0.1"];
  sel.replaceChildren(...local.map((a) => el("option", { value: a }, a + " · local network")));
  if (state.publicIp) sel.append(el("option", { value: state.publicIp }, state.publicIp + " · internet (public IP)"));
  if ([...sel.options].some((o) => o.value === keep)) sel.value = keep;
  drawLinks();
  if (!$("linkDlg").open) $("linkDlg").showModal();
}

function drawLinks() {
  if (whipOn() && linkSlot === state.whip.slot) return drawWhipLinks();
  const p = state.places[linkSlot - 1];
  if (!p || !inUse(p)) return;
  $("linkTitle").textContent = "Links for #" + p.slot + " " + label(p);
  const host = $("linkAddr").value + ":" + p.port;
  const play = (u) => "https://play.haivision.com/play?url=" + encodeURIComponent(u.replace("streamid=", "StreamID=").replace("latency=", "Latency="));
  let rows;
  if (perSignal()) {
    // The port is the address of the signal: one link for sending and for receiving, no stream ID.
    const u = "srt://" + host + "?latency=" + state.latencyMs + (p.passIn ? "&passphrase=" + encodeURIComponent(p.passIn) : "");
    const src = srcOf(p.source);
    rows = [
      ["Send to this signal (caller, no stream ID)", u],
      ["Receive on this port (caller, same address): what the matrix assigns, now " + (src ? "#" + src.slot + " " + label(src) : "nothing"), u],
      ["Haivision Play: receive", play(u)],
    ];
  } else {
    const url = (streamId, pass) => "srt://" + host + "?streamid=" + encodeURIComponent(streamId) + "&latency=" + state.latencyMs + (pass ? "&passphrase=" + encodeURIComponent(pass) : "");
    const bare = url(p.id, p.passIn), fwd = url(recvName(p), p.passFwd);
    const fwdWhat = "Receive the forward output (caller" + (p.recvId ? ", receiver key" : "") + "): what the matrix assigns";
    rows = state.noOriginal ? [
      ["Send to this signal (caller, sender key; receiving the original is off)", bare],
      [fwdWhat, fwd],
      ["Haivision Play: forward output", play(fwd)],
    ] : [
      ["Send to this signal, or receive its original (caller, same address for both)", bare],
      [fwdWhat, fwd],
      ["Haivision Play: original", play(bare)],
      ["Haivision Play: forward output", play(fwd)],
    ];
  }
  showLinkRows(rows);
}

function showLinkRows(rows) {
  $("linkList").replaceChildren(...rows.map(([what, link]) => {
    const btn = el("button", { type: "button", class: "sm" }, "Copy");
    btn.addEventListener("click", () => copy(link, btn));
    const qr = el("button", { type: "button", class: "sm", onclick: () => openQr(what, link) }, "QR code");
    return el("div", { class: "link" }, el("div", { class: "what" }, what), el("code", null, link), el("div", { class: "linkBtns" }, btn, qr));
  }));
}

// The QR code is drawn by the bridge itself (no internet needed) and shown in a small window.
let qrUrl = "";
let qrActiveWhat = "", qrActiveLink = "", qrActiveMode = "raw";

function parseSrtUrlParams(u) {
  try {
    const q = u.indexOf("?");
    const base = q >= 0 ? u.slice(0, q) : u;
    const search = q >= 0 ? new URLSearchParams(u.slice(q + 1)) : new URLSearchParams();
    return { base, streamid: search.get("streamid") || "", latency: search.get("latency") || "120", pass: search.get("passphrase") || "" };
  } catch { return { base: u, streamid: "", latency: "120", pass: "" }; }
}

function buildQrTarget(mode, what, link) {
  if (!link.startsWith("srt://")) {
    return { text: link, hint: "" };
  }
  const parsed = parseSrtUrlParams(link);
  if (mode === "larix") {
    let l = "larix://set/v1?conn[][url]=" + encodeURIComponent(parsed.base)
      + "&conn[][name]=" + encodeURIComponent("SRT " + what)
      + "&conn[][srtmode]=c"
      + "&conn[][srtlatency]=" + (parsed.latency || state.latencyMs);
    if (parsed.streamid) l += "&conn[][srtstreamid]=" + encodeURIComponent(parsed.streamid);
    if (parsed.pass) l += "&conn[][srtpass]=" + encodeURIComponent(parsed.pass);
    return { text: l, hint: t("qr_hint_larix", "Open Larix Broadcaster and scan this QR code to auto-import connection settings.") };
  }
  if (mode === "haivision") {
    const hv = "https://play.haivision.com/play?url=" + encodeURIComponent(link.replace("streamid=", "StreamID=").replace("latency=", "Latency="));
    return { text: hv, hint: t("qr_hint_haivision", "Standard web URL for mobile camera and Haivision Play Pro.") };
  }
  return { text: link, hint: t("qr_hint_raw", "Direct SRT URI. For phone camera scanner, select 'Larix App' or 'Haivision Web' above.") };
}

async function updateQrImage() {
  const { text, hint } = buildQrTarget(qrActiveMode, qrActiveWhat, qrActiveLink);
  const res = await fetch("/api/qr", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ text }) });
  if (!res.ok) throw new Error((await res.text()).trim() || "no QR code");
  if (qrUrl) URL.revokeObjectURL(qrUrl);
  qrUrl = URL.createObjectURL(await res.blob());
  $("qrImg").src = qrUrl;
  $("qrTitle").textContent = "QR code · " + qrActiveWhat;
  $("qrText").textContent = text;
  $("qrText").title = qrActiveWhat;
  if ($("qrHint")) $("qrHint").textContent = hint;
}

async function openQr(what, link) {
  try {
    qrActiveWhat = what;
    qrActiveLink = link;
    qrActiveMode = "raw";
    if ($("qrTabs")) {
      $("qrTabs").style.display = link.startsWith("srt://") ? "flex" : "none";
      $("qrTabRaw").className = "sm on";
      $("qrTabLarix").className = "sm";
      $("qrTabHaivision").className = "sm";
    }
    await updateQrImage();
    $("qrDlg").showModal();
  } catch (e) { alert("QR code: " + e.message); }
}

if ($("qrTabRaw")) {
  $("qrTabRaw").addEventListener("click", () => { qrActiveMode = "raw"; $("qrTabRaw").className = "sm on"; $("qrTabLarix").className = "sm"; $("qrTabHaivision").className = "sm"; updateQrImage().catch(() => {}); });
  $("qrTabLarix").addEventListener("click", () => { qrActiveMode = "larix"; $("qrTabRaw").className = "sm"; $("qrTabLarix").className = "sm on"; $("qrTabHaivision").className = "sm"; updateQrImage().catch(() => {}); });
  $("qrTabHaivision").addEventListener("click", () => { qrActiveMode = "haivision"; $("qrTabRaw").className = "sm"; $("qrTabLarix").className = "sm"; $("qrTabHaivision").className = "sm on"; updateQrImage().catch(() => {}); });
}
$("linkAddr").addEventListener("change", drawLinks);

// ---- remote access: this interface over HTTPS on TCP <SRT port> ----
const viewerRemote = () => !!state.remote && state.remote.viewer === "remote";
function remoteUrl() {
  const host = state.publicIp || (viewerRemote() ? location.hostname : state.addresses[0]) || "address-of-this-pc";
  return "https://" + host + ":" + state.remote.port + "/";
}
function renderRemote() {
  const r = state.remote;
  if (!r) return;
  const remote = viewerRemote();
  $("logoutBtn").hidden = !remote;
  const lamp = $("rmLamp");
  if (r.error) { lamp.className = "lamp bad"; put($("rmText"), "ERROR: " + r.error); }
  else if (r.listening) { lamp.className = "lamp live"; put($("rmText"), "ON: HTTPS on TCP " + r.port); }
  else { lamp.className = "lamp"; put($("rmText"), "OFF"); }
  put($("rmToggle"), r.enabled ? "Switch off" : "Switch on");
  $("rmToggle").disabled = remote || (!r.enabled && !r.hasPassword);
  put($("rmHelp"), remote
    ? "You are connected over remote access. Remote access itself, the SRT port and the firewall can only be changed on the bridge PC."
    : !r.hasPassword ? "Set a password first, then switch remote access on."
    : r.enabled ? "Also needed: the firewall rule for TCP " + r.port + " (Internet access, set the rule again) and, in the router, TCP " + r.port + " forwarded to this PC next to UDP. Whoever has the password can change everything except remote access, the SRT port and the firewall."
    : "Switched on, the bridge answers on TCP " + r.port + " over HTTPS, the same number as the SRT port. Off, nothing listens there.");
  put($("rmPassHelp"), remote ? "The password can only be changed on the bridge PC." : r.hasPassword ? "A password is set. A new one logs out everybody who is logged in." : "No password set yet.");
  $("rmPass").disabled = $("rmPass2").disabled = $("rmPassForm").querySelector("button").disabled = remote;
  put($("rmUrl"), remoteUrl());
  put($("rmFp"), r.fingerprint || "–");
  put($("rmToken"), remote ? "only shown on the bridge PC" : r.apiToken || "–");
  $("rmTokenNew").disabled = $("rmTokenCopy").disabled = remote;
  // Over remote access the port stays as it is (it is the viewer's port too), and the firewall belongs to the bridge PC.
  $("setPort").disabled = $("setMode").disabled = $("fwBtn").disabled = remote;
}

async function remoteChange(body, okText) {
  try {
    render(await api("PUT", "/api/remote", body));
    note("rmNote", okText);
    checkFirewall();
    return true;
  } catch (e) { note("rmErr", e.message); return false; }
}
$("rmToggle").addEventListener("click", () => remoteChange({ enabled: !state.remote.enabled },
  state.remote.enabled ? "switched off" : "switched on · now set the firewall rule again"));
$("rmPassForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if ($("rmPass").value !== $("rmPass2").value) { note("rmErr", "the two passwords are not the same"); return; }
  if (await remoteChange({ password: $("rmPass").value }, "password set")) $("rmPass").value = $("rmPass2").value = "";
});
$("rmTokenNew").addEventListener("click", () => remoteChange({ newToken: true }, "new token, the old one no longer works"));
$("rmTokenCopy").addEventListener("click", () => copy(state.remote.apiToken, $("rmTokenCopy")));
$("rmUrlCopy").addEventListener("click", () => copy(remoteUrl(), $("rmUrlCopy")));
$("rmUrlQr").addEventListener("click", () => openQr("Remote access", remoteUrl()));

// ---- start with Windows ----
function renderAutostart() {
  const a = state.autostart;
  if (!a) return;
  if (document.activeElement !== $("asOn")) $("asOn").checked = a.on;
  put($("asHelp"), a.other
    ? "The entry starts another copy: " + a.other + ". Tick it again to start this one."
    : a.on ? "SRT Bridge starts when this Windows user logs in." : "SRT Bridge starts only by hand.");
}
$("asOn").addEventListener("change", () => change("PUT", "/api/autostart", { on: $("asOn").checked },
  ["asNote", "saved"], "asErr"));

// Shut down this computer: two clicks on the same button (no dialog), then a
// countdown of Windows that every open interface shows and can cancel.
function renderShutdown() {
  const btn = $("sdBtn"), at = state.shutdownAt || 0;
  const left = at ? Math.max(0, at - Math.floor(Date.now() / 1000)) : 0;
  if (at) {
    $("sdLamp").className = "lamp bad";
    put($("sdText"), left ? "SHUTTING DOWN in " + left + " s" : "SHUTTING DOWN");
    delete btn.dataset.armed;
    put(btn, "Cancel shutdown", "pending");
  } else {
    $("sdLamp").className = "lamp live";
    put($("sdText"), "running");
    if (!btn.dataset.armed) put(btn, "Shut down this computer", "");
  }
}
$("sdBtn").addEventListener("click", async () => {
  const btn = $("sdBtn");
  if (state.shutdownAt) { await change("POST", "/api/shutdown/cancel", {}, ["asNote", "shutdown cancelled"], "asErr"); return; }
  if (!btn.dataset.armed) {
    btn.dataset.armed = "1";
    put(btn, "Sure? Shut down", "armed");
    setTimeout(() => { if (btn.dataset.armed) { delete btn.dataset.armed; renderShutdown(); } }, 3000);
    return;
  }
  delete btn.dataset.armed;
  await change("POST", "/api/shutdown", {}, ["asNote", "shutting down in 20 s"], "asErr");
});

// ---- Bridge Output: start, stop, an output per signal ----
const OUT_KINDS = [["off", "Off"], ["omt", "OMT"], ["ndi", "NDI"]];
const opRows = [];

// The choice: Off/OMT/NDI, then the SDI ports. A port another signal uses cannot be chosen;
// a port that is set but not found stays in the list, so the setting stays visible.
function fillOutChoice(sel, ports, current, signals, slot) {
  const used = {};
  for (const s of signals) if (s.slot !== slot && s.out.startsWith("sdi:")) used[s.out] = s.slot;
  const options = OUT_KINDS.map(([value, label]) => ({ value, label, disabled: false }));
  for (const p of ports) {
    const value = "sdi:" + p.key;
    let text = "SDI " + p.name;
    if (used[value]) text += " (signal " + used[value] + ")";
    else if (p.busy) text += " (in use)";
    options.push({ value, label: text, disabled: !!used[value] });
  }
  if (!ports.length) options.push({ value: "", label: "SDI: no Blackmagic card found", disabled: true });
  if (current.startsWith("sdi:") && !options.some((o) => o.value === current))
    options.push({ value: current, label: "SDI port not found", disabled: false });
  const sig = JSON.stringify(options);
  if (sel.dataset.sig === sig) return;
  sel.dataset.sig = sig;
  sel.replaceChildren(...options.map((o) => el("option", { value: o.value, disabled: o.disabled }, o.label)));
}

function decoderText(s) {
  const e = s.engine;
  if (s.out === "off") return ["", false];
  if (!e) return ["starting…", false];
  if (e.state === "checking") return ["checking that the name is free on the network…", false];
  if (e.problem && !e.pictures) return [e.problem, true];
  const sdi = s.out.startsWith("sdi:");
  if (e.state === "waiting") return [sdi ? "waiting for the signal" : "on air, waiting for the signal", false];
  if (e.state === "starting") return ["signal arrives, waiting for the first picture", false];
  const parts = [e.video, e.fps + " fps", e.audio ? e.audio : "no sound"];
  if (sdi) parts.push(e.sdi ? "SDI " + e.sdi : "SDI starting…");
  else parts.push(count(e.receivers, "receiver", "receivers"));
  if (e.gaps) parts.push(e.gaps + " gaps");
  if (e.errors) parts.push(e.errors + " errors");
  if (e.problem) return [e.problem + " · " + parts.join(" · "), true];
  return [parts.join(" · "), false];
}

async function saveOut(slot, r) {
  try {
    render(await api("PUT", "/api/output/out", { slot, out: r.sel.value, name: r.name.value.trim() }));
    r.error = "";
    note("ooNote", "saved");
  } catch (e) {
    r.error = e.message;
    put(r.dec, e.message, "dec bad");
    note("ooErr", e.message);
  }
}

function makeOutRow(slot) {
  const r = { error: "" };
  r.lamp = el("span", { class: "lamp" }, el("i"), r.word = el("span", null, "no signal"));
  r.bridge = el("td");
  r.sel = el("select", { class: "opSel" });
  r.name = el("input", { class: "opName", maxlength: 60, spellcheck: "false", autocomplete: "off" });
  r.dec = el("td", { class: "dec" });
  r.sel.addEventListener("change", () => saveOut(slot, r));
  r.name.addEventListener("change", () => saveOut(slot, r));
  r.name.addEventListener("keydown", (ev) => { if (ev.key === "Enter") r.name.blur(); });
  r.tr = el("tr", null, el("td", { class: "sigLamp" }, r.lamp), el("td", { class: "num" }, String(slot)), r.bridge, el("td", null, r.sel), el("td", null, r.name), r.dec);
  return r;
}

function renderOutputs() {
  const o = state.output;
  if (!o) return;
  const st = o.running ? o.state : null;
  const lamp = $("opLamp");
  if (o.running) { lamp.className = "lamp live"; put($("opText"), "RUNNING" + (st ? " · version " + st.version : "") + (o.ours ? " · started by the bridge" : " · started by hand")); }
  else if (o.want && o.found) { lamp.className = "lamp wait"; put($("opText"), "STARTING"); }
  else if (!o.found) { lamp.className = "lamp bad"; put($("opText"), "NOT FOUND: " + (o.path || "srt-bridge-output.exe is not next to the bridge")); }
  else { lamp.className = "lamp"; put($("opText"), "STOPPED"); }
  if (o.error && !o.running) { lamp.className = "lamp bad"; put($("opText"), "ERROR: " + o.error); }
  $("opStart").disabled = o.running || !o.found;
  $("opStop").disabled = !o.running;
  const cards = st && st.cards || [];
  put($("opInfo"), !st ? (o.found ? "Program: " + o.path : " ")
    : (cards.length ? cards.map((c) => c.model + " found, " + count(c.ports, "SDI port", "SDI ports")).join(" · ") : "no SDI card")
      + (st.bridgeOk ? "" : " · it does not reach the bridge at " + st.bridgeUrl));
  if (document.activeElement !== $("opAuto")) $("opAuto").checked = o.auto;

  // The table: one row per signal of the bridge, also while the program is stopped (then switched off).
  const tb = $("opTable");
  const signals = st ? st.signals.filter((s) => s.slot <= state.slots) : state.places.map((p) => ({ slot: p.slot, name: label(p), live: !!p.sender, out: "off", outName: "", engine: null }));
  if (changed("opRows", signals.length)) {
    opRows.length = 0;
    const body = el("tbody");
    for (const s of signals) { const r = makeOutRow(s.slot); opRows.push(r); body.append(r.tr); }
    tb.replaceChildren(el("thead", null, el("tr", null, ...["Signal", "#", "Bridge name", "Output", "Output name", "Decoder"].map((h) => el("th", null, h)))), body);
  }
  for (const s of signals) {
    const r = opRows[s.slot - 1];
    if (!r) continue;
    const p = state.places[s.slot - 1];
    const live = p ? !!p.sender : s.live;
    put(r.word, live ? "SIGNAL" : "no signal", "");
    r.lamp.className = "lamp" + (live ? " live" : "");
    put(r.bridge, p ? label(p) : s.name || "");
    r.sel.disabled = r.name.disabled = !st;
    if (st && document.activeElement !== r.sel) {
      fillOutChoice(r.sel, st.ports || [], s.out, st.signals, s.slot);
      r.sel.value = s.out;
    }
    const sdi = s.out.startsWith("sdi:");
    if (st) r.name.disabled = sdi;
    r.name.title = sdi ? "not used for SDI" : "";
    if (document.activeElement !== r.name && !r.error) r.name.value = s.outName;
    if (r.error) continue; // the reason why it was not saved stays until it is corrected
    const [text, bad] = st ? decoderText(s) : ["", false];
    put(r.dec, text, "dec" + (bad ? " bad" : ""));
    r.dec.title = text;
  }

  // Which program the bridge starts: only on the bridge PC.
  const remote = viewerRemote();
  const path = $("opPath");
  if (!path.dataset.edited && document.activeElement !== path) path.value = o.custom;
  path.disabled = $("opPathForm").querySelector("button").disabled = remote;
  put($("opPathHelp"), remote ? "The program path can only be changed on the bridge PC."
    : o.custom ? (o.found ? "Started from this path." : "Not found at this path.")
    : o.found ? "Found next to the bridge: " + o.path : "Not found next to the bridge (folder output). Install it with the bridge setup, or enter the path of srt-bridge-output.exe.");
}

$("opStart").addEventListener("click", () => change("POST", "/api/output/start", {}, ["opNote", "starting"], "opErr"));
$("opStop").addEventListener("click", () => change("POST", "/api/output/stop", {}, ["opNote", "stopped"], "opErr"));
$("opAuto").addEventListener("change", () => change("PUT", "/api/output/settings", { auto: $("opAuto").checked, path: state.output.custom },
  ["opNote", "saved"], "opErr"));
$("opPath").addEventListener("input", () => { $("opPath").dataset.edited = "1"; });
$("opPathForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if (await change("PUT", "/api/output/settings", { auto: state.output.auto, path: $("opPath").value.trim() }, ["opPathNote", "saved"], "opPathErr"))
    delete $("opPath").dataset.edited;
});

// ---- edit a signal ----
let editSlot = 0;
function openEdit(slot) {
  const p = state.places[slot - 1];
  editSlot = slot;
  $("editTitle").textContent = "Signal #" + slot;
  $("editName").value = p.name;
  $("editName").placeholder = "Line " + slot;
  $("editId").value = p.id;
  $("editPassIn").value = p.passIn;
  $("editPassFwd").value = p.passFwd;
  $("editRecv").value = p.recvId;
  // With a port per signal there is no stream ID and one passphrase for the port.
  const ps = perSignal();
  $("editId").disabled = $("editPassFwd").disabled = $("editGen").disabled = $("editRecv").disabled = $("editRecvGen").disabled = ps;
  put($("editIdLbl"), ps ? "Sender key · not used, the port is UDP " + p.port : "Sender key (stream ID) · empty = the signal is free");
  put($("editRecvLbl"), ps ? "Receiver key · not used" : "Receiver key · empty = sender key*" + state.suffixes[0]);
  put($("editPassInLbl"), ps ? "Passphrase for this port · empty = none" : "Passphrase: input and original · empty = none");
  put($("editPassFwdLbl"), ps ? "Passphrase: forward output · not used" : "Passphrase: forward output · empty = none");
  put($("editHelp"), ps
    ? "A passphrase has 10 to 79 characters and applies to the sender and to the receivers on this port. A new passphrase reconnects the devices of this signal. The key length (AES 128, 192 or 256) is set by the caller: each device chooses its own."
    : "A passphrase has 10 to 79 characters. A new key or passphrase reconnects the devices that use it. The key length (AES 128, 192 or 256) is set by the caller: each encoder and decoder chooses its own, the bridge takes it.");
  $("editErr").textContent = "";
  $("editDlg").showModal();
}
$("editForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const p = state.places[editSlot - 1];
  const id = $("editId").value.trim();
  const n = (p.sender ? 1 : 0) + p.receivers.length + p.forward.length;
  if (perSignal()) {
    try {
      render(await api("PUT", "/api/places", { slot: editSlot, name: $("editName").value.trim(), passIn: $("editPassIn").value }));
      $("editDlg").close();
      note("mainNote", "saved");
    } catch (e) { $("editErr").textContent = e.message; }
    return;
  }
  if (id !== p.id && n && !confirm(count(n, "device is", "devices are") + " connected to this signal.\n" + (id ? "A new stream ID" : "Clearing the signal") + " disconnects " + (n === 1 ? "it" : "them") + ". Save now?")) return;
  try {
    render(await api("PUT", "/api/places", {
      slot: editSlot, name: $("editName").value.trim(), id,
      passIn: id ? $("editPassIn").value : "", passFwd: id ? $("editPassFwd").value : "",
      recvId: id ? $("editRecv").value.trim() : "",
    }));
    $("editDlg").close();
    note("mainNote", "saved");
  } catch (e) { $("editErr").textContent = e.message; }
});

for (const b of document.querySelectorAll("[data-close]")) b.addEventListener("click", () => b.closest("dialog").close());

// ---- WHIP input: the tile after the last signal, its links, and its card in Setup ----
let whipTile = null;
function buildWhipTile() {
  const t = {};
  const dd = (key, cls) => (t[key] = el("dd", { class: cls || "" }, "–"));
  t.no = el("span", { class: "no" }, "#");
  t.name = el("span", { class: "name" });
  t.lamp = el("span", { class: "lamp" }, el("i"));
  t.lampText = el("span");
  t.lamp.append(t.lampText);
  t.sid = el("span", { class: "sid" });
  t.copyBtn = el("button", { type: "button", class: "sm mini", title: "Copy the stream ID for receivers", onclick: () => copy(state.whip.id, t.copyBtn) }, "Copy");
  t.links = el("button", { type: "button", class: "sm narrow", onclick: () => openLinks(state.whip.slot) }, "Links");
  t.edit = el("button", { type: "button", class: "sm narrow", title: "WHIP settings in Setup", onclick: () => document.querySelector('#nav [data-go="setup"]').click() }, "Setup");
  t.disc = el("button", { type: "button", class: "sm wide", title: "closes the WHIP connection; the sender may connect again at once", onclick: () => change("POST", "/api/whip/disconnect", {}, ["mainNote", "WHIP sender disconnected"], "mainErr") }, "Disconnect");
  t.power = el("button", { type: "button", class: "sm wide powerBtn", onclick: () => change("PUT", "/api/whip", { off: !state.whip.off }, ["mainNote", state.whip.off ? "WHIP signal on" : "WHIP signal off"], "mainErr") }, "✕ Off");
  t.root = el("div", { class: "tile whip" },
    el("div", { class: "tileHead" }, t.no, t.name, t.lamp),
    el("div", { class: "idRow" }, t.sid, t.copyBtn),
    el("dl", { class: "stats" },
      el("dt", null, "bitrate"), dd("rate", "big"),
      el("dt", null, "video"), dd("video"),
      el("dt", null, "audio"), dd("audio"),
      el("dt", null, "lost · held"), dd("loss"),
      el("dt", null, "sound delay"), dd("delay"),
      el("dt", null, "on air"), dd("air"),
      el("dt", null, "from"), dd("from"),
      el("dt", null, "error"), dd("err"),
      el("div", { class: "sep" }),
      el("div", { class: "out" }, el("span", null, "original out "), (t.orig = el("b"))),
      el("div", { class: "out" }, el("span", null, "in the matrix "), (t.fwd = el("b")))),
    el("div", { class: "tileActs" }, t.links, t.edit, t.disc, t.power));
  return t;
}

// The program's own state (srt-bridge-whip); {} while it does not answer.
const whipProg = () => (state.whip.program && state.whip.program.state) || {};

function whipError() {
  const w = state.whip, pr = w.program || {}, ps = whipProg();
  if (!pr.found) return "srt-bridge-whip.exe not found";
  if (pr.error) return pr.error;
  if (ps.error) return ps.error;
  if (ps.soundError) return "no sound: " + ps.soundError;
  if (!w.key) return "no WHIP key set: every sender is turned away";
  const e = signalError(w.slot);
  return e ? e.short : "";
}

function fillWhipTile() {
  const t = whipTile, w = state.whip, ps = whipProg(), live = w.live;
  const sender = !!ps.live;
  t.root.className = "tile whip" + (w.off ? " off" : live ? " live" : "");
  put(t.no, "# " + w.slot);
  put(t.name, whipPlace().name);
  t.name.title = "WHIP input · " + whipPlace().name;
  put(t.lampText, w.off ? "OFF" : live ? "LIVE" : sender ? "STARTING" : "WAITING");
  t.lamp.className = "lamp" + (w.off ? " bad" : live ? " live" : " wait");
  showPower(t.power, w.off, true, "WHIP signal");
  put(t.sid, w.id || "no stream ID", "sid" + (w.id ? "" : " none"));
  t.sid.title = "stream ID for receivers (SRT); the sender uses WHIP on port " + w.port;
  t.disc.disabled = !sender;
  put(t.rate, live ? Math.round(w.kbps) + " kbit/s" : "–");
  const m = live && w.media;
  put(t.video, m ? [m.video, m.videoCodec].filter(Boolean).join(" · ") || "not known" : sender && ps.video ? ps.video : "–");
  put(t.audio, m ? audioText(m) + " (from Opus)" : sender && ps.audio ? ps.audio : "–");
  // Lost: video packets not repaired in time. Held: pictures not passed on (broken, or waiting for a keyframe).
  put(t.loss, sender ? (ps.lost || 0) + " · " + (ps.dropped || 0) : "–", sender && ps.lost ? "warn" : "");
  t.loss.title = sender ? "video packets lost for good: " + (ps.lost || 0) + " · pictures held back: " + (ps.dropped || 0) + " · keyframes asked for: " + (ps.keyRequests || 0) + " · sound packets lost: " + (ps.audioLost || 0) : "";
  put(t.delay, sender && ps.delayMs ? ps.delayMs + " ms" : "–");
  t.delay.title = "time between the decoder clock and the pictures: the AAC sound needs it (Opus decode, AAC frame, AAC encoder delay)";
  put(t.air, sender && ps.since ? since(ps.since) + (ps.sessions > 1 ? " · " + count(ps.sessions - 1, "restart", "restarts") : "") : "–");
  put(t.from, sender ? ps.client : "–");
  const e = whipError();
  put(t.err, e || "–", e ? "bad" : "");
  t.err.title = e;
  let o = 0;
  for (const c of w.receivers) o += c.mbps;
  put(t.orig, w.receivers.length ? count(w.receivers.length, "device fetches", "devices fetch") + " it · " + mbps(o) : perSignal() ? "only over the matrix (port per signal)" : "nobody fetches it");
  put(t.fwd, w.forward.length ? "out on " + w.forward.map((n) => "#" + n).join(", ") : "not assigned");
  t.fwd.parentNode.title = "forward outputs that get the WHIP input (Matrix)";
}

// The WHIP address for a sender, with the host chosen in the Links window.
const whipUrl = (host) => "http://" + host + ":" + state.whip.port + "/whip";

function drawWhipLinks() {
  const w = state.whip;
  const name = whipPlace().name;
  $("linkTitle").textContent = "Links for #" + w.slot + " " + name + (name === "WHIP" ? "" : " (WHIP)");
  const addr = $("linkAddr").value;
  const rows = [
    ["WHIP sender (OBS: Service «WHIP», Server = this address, Bearer Token = the key below)", whipUrl(addr)],
    ["WHIP key (bearer token)", w.key || "(no key set)"],
  ];
  if (!perSignal() && w.id) {
    const u = "srt://" + addr + ":" + state.srtPort + "?streamid=" + encodeURIComponent(w.id) + "&latency=" + state.latencyMs + (w.pass ? "&passphrase=" + encodeURIComponent(w.pass) : "");
    rows.push(["Receive this signal (SRT caller)", u]);
    rows.push(["Haivision Play: receive", "https://play.haivision.com/play?url=" + encodeURIComponent(u.replace("streamid=", "StreamID=").replace("latency=", "Latency="))]);
  }
  showLinkRows(rows);
}

// Setup card. The form is filled once, and again after a save, so typing is never overwritten.
let whipFilled = false;
function fillWhipForm() {
  const w = state.whip;
  $("whOn").checked = w.enabled;
  $("whPort").value = w.port;
  $("whBitrate").value = String(w.aacBitrate);
  $("whKey").value = w.key;
  $("whId").value = w.id;
  $("whPass").value = w.pass;
  $("whName").value = w.name;
  whipFilled = true;
}

function renderWhip() {
  if (!state.whip) return;
  if (!whipFilled) fillWhipForm();
  const w = state.whip, pr = w.program || {}, ps = whipProg();
  const lamp = $("whLamp"), text = $("whText");
  if (!w.enabled) { lamp.className = "lamp"; put(text, "OFF"); }
  else if (!pr.found) { lamp.className = "lamp bad"; put(text, "srt-bridge-whip.exe not found next to the bridge"); }
  else if (pr.error && !pr.running) { lamp.className = "lamp bad"; put(text, pr.error); }
  else if (!pr.running) { lamp.className = "lamp wait"; put(text, "STARTING"); }
  else if (w.live) { lamp.className = "lamp live"; put(text, "LIVE: " + (ps.client || "sender") + " sends · " + Math.round(w.kbps) + " kbit/s"); }
  else if (ps.live) { lamp.className = "lamp wait"; put(text, "CONNECTED: " + ps.client + ", waiting for the first keyframe"); }
  else { lamp.className = "lamp wait"; put(text, "READY: waiting for a WHIP sender on port " + w.port); }
  const host = state.publicIp || state.addresses[0] || "address-of-this-pc";
  put($("whOnLbl"), "WHIP input on (signal #" + w.slot + " on Main, after the last signal)");
  put($("whHelp"), "Senders use " + whipUrl(host) + " with the WHIP key as bearer token. "
    + "Open TCP and UDP " + w.port + " in the firewall (Set firewall rule in SRT) and forward both in the router. "
    + "The picture is passed on unchanged (H.264 or H.265); the sound becomes AAC, which needs about 90 ms: the stream is timed for it. "
    + (perSignal() ? "With a port per signal, receivers get the WHIP input over the matrix only." : "Receivers fetch it with the stream ID, forward outputs through the Matrix."));
}

$("whKeyNew").addEventListener("click", () => { try { $("whKey").value = randomKey({ ...keySettings(), len: Math.max(24, keySettings().len) }); } catch (e) { note("whErr", e.message); } });
$("whIdNew").addEventListener("click", () => { try { $("whId").value = randomKey(keySettings()); } catch (e) { note("whErr", e.message); } });
$("whKeyCopy").addEventListener("click", () => copy($("whKey").value, $("whKeyCopy")));
$("whIdCopy").addEventListener("click", () => copy($("whId").value, $("whIdCopy")));
$("whForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const on = $("whOn").checked;
  // Switching on for the first time: a stream ID and a key are made, so it works at once.
  if (on && !$("whId").value.trim()) $("whId").value = randomKey(keySettings());
  if (on && !$("whKey").value.trim()) $("whKey").value = randomKey({ ...keySettings(), len: Math.max(24, keySettings().len) });
  const ok = await change("PUT", "/api/whip", {
    enabled: on, port: Number($("whPort").value), aacBitrate: Number($("whBitrate").value),
    key: $("whKey").value.trim(), id: $("whId").value.trim(), pass: $("whPass").value, name: $("whName").value.trim(),
  }, ["whNote", "saved"], "whErr");
  if (ok) { fillWhipForm(); checkFirewall(); }
});

// ---- pages, clock, polling ----
$("nav").addEventListener("click", (ev) => {
  const go = ev.target.dataset.go;
  if (!go) return;
  for (const b of $("nav").children) b.classList.toggle("on", b === ev.target);
  for (const s of document.querySelectorAll("section[data-page]")) s.hidden = s.dataset.page !== go;
  checkFirewall();
});

setInterval(() => { $("clock").textContent = new Date().toLocaleTimeString("de-CH"); }, 500);

async function poll() {
  if (!loginOpen) {
    try { render(await api("GET", "/api/state")); }
    catch { if (!loginOpen) put($("portBadge"), "No answer from SRT Bridge", "chBadge bad"); }
  }
  setTimeout(poll, 1000);
}

if ($("langEn")) $("langEn").addEventListener("click", () => setLanguage("en"));
if ($("langEs")) $("langEs").addEventListener("click", () => setLanguage("es"));
applyLanguageToDOM();

poll();
