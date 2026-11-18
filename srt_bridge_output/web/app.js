"use strict";
const $ = (id) => document.getElementById(id);
const OUTS = [["off", "Off"], ["omt", "OMT"], ["ndi", "NDI"]];
const rows = [];

// The choice: Off/OMT/NDI, then the Blackmagic ports of this PC. A port another signal uses cannot be chosen;
// a port that is configured but not found stays in the list so the setting is visible.
function fillChoice(sel, ports, current, signals, slot) {
  const used = {};
  for (const s of signals) if (s.slot !== slot && s.out.startsWith("sdi:")) used[s.out] = s.slot;
  const options = OUTS.map(([value, label]) => ({ value, label, disabled: false }));
  for (const p of ports) {
    const value = "sdi:" + p.key;
    let label = "SDI " + p.name;
    if (used[value]) label += " (signal " + used[value] + ")";
    else if (p.busy) label += " (in use)";
    options.push({ value, label, disabled: !!used[value] });
  }
  if (!ports.length) options.push({ value: "", label: "SDI: no Blackmagic card found", disabled: true });
  if (current.startsWith("sdi:") && !options.some((o) => o.value === current))
    options.push({ value: current, label: "SDI port not found", disabled: false });
  const sig = JSON.stringify(options);
  if (sel.dataset.sig === sig) return;
  sel.dataset.sig = sig;
  sel.replaceChildren(...options.map((o) => {
    const e = el("option", "", o.label);
    e.value = o.value;
    e.disabled = o.disabled;
    return e;
  }));
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function makeRow(slot) {
  const row = el("div", "row");
  const sig = el("div", "sig off");
  const lamp = el("span", "lamp");
  const word = el("span", "", "no signal");
  sig.append(lamp, word);
  const num = el("div", "", String(slot));
  const name = el("div", "", "");
  const cell = el("div");
  const sel = el("select");
  const nameCell = el("div");
  const outName = el("input");
  outName.type = "text";
  outName.maxLength = 60;
  outName.spellcheck = false;
  nameCell.append(outName);
  const dec = el("div", "dec", "");
  const r = { sig, lamp, word, name, sel, outName, dec, error: "", saved: "" };
  const save = async () => {
    const res = await fetch("/api/out", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ slot, out: sel.value, name: outName.value }) });
    const body = await res.json().catch(() => ({}));
    if (res.ok) { r.error = ""; render(body); }
    else { r.error = body.error || "not saved"; showDecoder(r, r.error, true); }
  };
  sel.addEventListener("change", save);
  outName.addEventListener("change", save);
  outName.addEventListener("keydown", (e) => { if (e.key === "Enter") outName.blur(); });
  cell.append(sel);
  row.append(sig, num, name, cell, nameCell, dec);
  $("rows").append(row);
  return r;
}

function showDecoder(r, text, bad) {
  r.dec.textContent = text;
  r.dec.className = "dec" + (bad ? " bad" : "");
  r.dec.title = text;
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
  const parts = [e.video, e.fps + " fps"];
  parts.push(e.audio ? e.audio : "no sound");
  if (sdi) parts.push(e.sdi ? "SDI " + e.sdi : "SDI starting…");
  else parts.push(e.receivers + (e.receivers === 1 ? " receiver" : " receivers"));
  if (e.gaps) parts.push(e.gaps + " gaps");
  if (e.errors) parts.push(e.errors + " errors");
  if (e.problem) return [e.problem + " · " + parts.join(" · "), true];
  return [parts.join(" · "), false];
}

function render(st) {
  $("ver").textContent = "v" + st.version;
  $("bridgeLamp").className = "lamp " + (st.bridgeOk ? "on" : "bad");
  $("bridgeText").textContent = st.bridgeOk ? "SRT Bridge " + st.bridgeVersion + " connected" : "SRT Bridge not reachable at " + st.bridgeUrl + " (is it running?)";
  const cards = st.cards || [];
  $("sdiLamp").className = "lamp" + (cards.length ? " on" : "");
  $("sdiText").textContent = cards.length
    ? cards.map((c) => c.model + " found, " + c.ports + (c.ports === 1 ? " SDI port" : " SDI ports")).join(" · ")
    : "no SDI card";
  for (const s of st.signals) {
    const r = rows[s.slot - 1] || (rows[s.slot - 1] = makeRow(s.slot));
    r.lamp.className = "lamp" + (s.live ? " on" : "");
    r.sig.className = "sig" + (s.live ? "" : " off");
    r.word.textContent = s.live ? "SIGNAL" : "no signal";
    r.name.textContent = s.name || "";
    if (document.activeElement !== r.sel) {
      fillChoice(r.sel, st.ports || [], s.out, st.signals, s.slot);
      r.sel.value = s.out;
    }
    // SDI has no name on the network.
    const sdi = s.out.startsWith("sdi:");
    r.outName.disabled = sdi;
    r.outName.title = sdi ? "not used for SDI" : "";
    if (document.activeElement !== r.outName && !r.error) r.outName.value = s.outName;
    if (r.error) continue; // the reason why the name was not saved stays until it is corrected
    const [text, bad] = decoderText(s);
    showDecoder(r, text, bad);
  }
}

async function tick() {
  try {
    const r = await fetch("/api/state");
    if (r.ok) render(await r.json());
  } catch (e) { /* the program is ending */ }
}
tick();
setInterval(tick, 1000);
