import RFB from "./novnc/core/rfb.js";
import KeyTable from "./novnc/core/input/keysym.js";

const boot = JSON.parse(document.getElementById("boot").textContent);

const $ = (id) => document.getElementById(id);
const screen = $("screen");
const statusDot = $("statusDot");
const statusText = $("statusText");
const accelChip = $("accelChip");
const banner = $("banner");
const stopped = $("stopped");
const btnControl = $("btnControl");

let rfb = null;
let mode = "control";
let reconnectTimer = null;
let lastStatus = boot.status;
let control = { mine: false, holder: "" };
let scale = true;

function setStatus(kind, text) {
  statusDot.className = "dot " + kind;
  statusText.textContent = text;
}

function showBanner(text, kind = "") {
  banner.className = "banner " + kind;
  banner.innerHTML = "";
  const span = document.createElement("span");
  span.textContent = text;
  banner.appendChild(span);
  banner.hidden = false;
}
function hideBanner() { banner.hidden = true; }

async function api(path, opts = {}) {
  const res = await fetch(path, {
    method: opts.method || "GET",
    headers: { "Content-Type": "application/json" },
    body: opts.body ? JSON.stringify(opts.body) : undefined,
    credentials: "same-origin",
  });
  if (res.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname); return null; }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function wsURL() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  return `${proto}://${location.host}/ws/vnc?mode=${mode}`;
}

function connect() {
  if (rfb) return;
  clearTimeout(reconnectTimer);
  setStatus("warn", "Connecting…");
  rfb = new RFB(screen, wsURL(), { wsProtocols: [] });
  rfb.scaleViewport = scale;
  rfb.resizeSession = false;
  rfb.showDotCursor = true;
  rfb.background = "#000";
  rfb.viewOnly = mode === "view";

  rfb.addEventListener("connect", async () => {
    setStatus("ok", mode === "view" ? "Connected (view only)" : "Connected");
    stopped.hidden = true;
    rfb.focus();
    await refreshStatus();
  });
  rfb.addEventListener("disconnect", (e) => {
    rfb = null;
    setStatus("bad", e.detail.clean ? "Disconnected" : "Connection lost");
    scheduleReconnect(1500);
  });
  rfb.addEventListener("clipboard", (e) => {
    $("clipText").value = e.detail.text;
  });
  rfb.addEventListener("securityfailure", (e) => {
    showBanner("VNC security failure: " + e.detail.reason, "error");
  });
}

function disconnect() {
  if (rfb) { const r = rfb; rfb = null; r.disconnect(); }
}

function scheduleReconnect(ms) {
  clearTimeout(reconnectTimer);
  reconnectTimer = setTimeout(async () => {
    await refreshStatus();
    if (lastStatus.state === "running") connect();
    else scheduleReconnect(3000);
  }, ms);
}

function applyControl(c) {
  control = c;
  if (lastStatus.state !== "running") { btnControl.hidden = true; return; }
  if (c.mine) {
    btnControl.hidden = true;
    if (mode !== "control") { mode = "control"; disconnect(); connect(); }
    if (rfb) rfb.viewOnly = false;
    if (banner.textContent.startsWith("Viewing")) hideBanner();
  } else if (c.holder) {
    btnControl.hidden = false;
    btnControl.textContent = `Take control from ${c.holder}`;
    if (rfb) rfb.viewOnly = true;
    mode = "view";
    showBanner(`Viewing only — ${c.holder} has the keyboard and mouse.`);
  } else {
    // Seat is free; grab it.
    btnControl.hidden = true;
    if (mode !== "control") { mode = "control"; disconnect(); connect(); }
  }
}

function applyVMStatus(st) {
  lastStatus = st;
  if (st.accel_note) { accelChip.hidden = false; accelChip.textContent = "Software emulation"; accelChip.title = st.accel_note; }
  else { accelChip.hidden = true; }

  if (st.state === "running") {
    stopped.hidden = true;
    if (!rfb) connect();
  } else {
    disconnect();
    stopped.hidden = false;
    const titles = { stopped: "VM is stopped", starting: "VM is starting…", stopping: "VM is shutting down…", crashed: "VM crashed" };
    $("stoppedTitle").textContent = titles[st.state] || st.state;
    let text = st.last_error || "";
    if (!st.qemu_found) text = "QEMU was not found on the server. " + (boot.admin ? "Open setup for instructions." : "Ask an administrator.");
    else if (!st.disk_exists) text = "No disk image exists yet. " + (boot.admin ? "Open setup to create one and start the Windows installer." : "Ask an administrator to run setup.");
    $("stoppedText").textContent = text;
    $("btnStartBig").disabled = st.state === "starting" || st.state === "stopping" || !st.disk_exists || !st.qemu_found;
    setStatus(st.state === "crashed" ? "bad" : "warn", $("stoppedTitle").textContent);
  }
  $("logText").textContent = (st.log || []).join("\n");
}

let tunnelStatus = { state: "disabled" };
let tsStatus = { state: "disabled" };

function linkBlock(container, label, url, note) {
  container.innerHTML = "";
  const l = document.createElement("div");
  l.textContent = label;
  const a = document.createElement("a");
  a.href = url; a.textContent = url; a.target = "_blank"; a.rel = "noopener";
  container.append(l, a);
  if (note) {
    const n = document.createElement("div");
    n.className = "muted small";
    n.textContent = note;
    container.appendChild(n);
  }
}

function updateShareSummary() {
  const live = (tsStatus.state === "running") || (tunnelStatus.state === "connected");
  const busy = ["starting", "downloading", "needs_login"].includes(tsStatus.state) || ["starting", "downloading"].includes(tunnelStatus.state);
  const err = tsStatus.state === "error" || tunnelStatus.state === "error";
  $("shareSummary").textContent = live ? "Share · live" : err ? "Share · error" : busy ? "Share · …" : "Share";
}

function applyTunnel(t) {
  tunnelStatus = t || { state: "disabled" };
  const body = $("shareBody");
  const copy = $("shareCopy");
  const toggle = $("shareToggle");
  const texts = {
    disabled: "Off. Gives a public https://….trycloudflare.com link (no account). Some school/work networks block it.",
    downloading: "Downloading cloudflared… (first time only)",
    starting: "Connecting to Cloudflare…",
    stopped: "Off.",
  };
  if (tunnelStatus.state === "connected" && tunnelStatus.url) {
    linkBlock(body, "Anyone with this link can reach the sign-in page:", tunnelStatus.url,
      tunnelStatus.mode === "quick" ? "This link changes when the server restarts." : "");
    copy.hidden = false;
  } else if (tunnelStatus.state === "error") {
    body.textContent = "Error: " + (tunnelStatus.error || "unknown");
    copy.hidden = true;
  } else {
    body.textContent = texts[tunnelStatus.state] || tunnelStatus.state;
    copy.hidden = true;
  }
  if (toggle) {
    const on = !(tunnelStatus.state === "disabled" || tunnelStatus.state === "stopped");
    toggle.textContent = on ? "Turn off" : "Turn on";
    toggle.dataset.on = on ? "1" : "0";
  }
  updateShareSummary();
}

function applyTailscale(t) {
  tsStatus = t || { state: "disabled" };
  const body = $("tsBody");
  const copy = $("tsCopy");
  const toggle = $("tsToggle");
  if (tsStatus.state === "running" && tsStatus.url) {
    const who = tsStatus.funnel && !tsStatus.note ? "Works from the public internet and from your Tailscale devices:" : "Works from any device signed in to your Tailscale network:";
    linkBlock(body, who, tsStatus.url, tsStatus.note || "");
    copy.hidden = false;
  } else if (tsStatus.state === "needs_login") {
    body.innerHTML = "";
    const l = document.createElement("div");
    l.textContent = "Connect this PC to your Tailscale account (one time):";
    const a = document.createElement("a");
    a.href = tsStatus.auth_url; a.textContent = "Open Tailscale sign-in"; a.target = "_blank"; a.rel = "noopener";
    a.className = "btn small";
    body.append(l, a);
    copy.hidden = true;
  } else if (tsStatus.state === "error") {
    body.textContent = "Error: " + (tsStatus.error || "unknown");
    copy.hidden = true;
  } else if (tsStatus.state === "starting") {
    body.textContent = "Connecting to Tailscale…";
    copy.hidden = true;
  } else {
    body.textContent = "Off. Private link over Tailscale (install Tailscale on the other PC and sign in to the same account), or turn on Funnel in settings for a public https://….ts.net link.";
    copy.hidden = true;
  }
  if (toggle) {
    const on = !(tsStatus.state === "disabled" || tsStatus.state === "stopped");
    toggle.textContent = on ? "Turn off" : "Turn on";
    toggle.dataset.on = on ? "1" : "0";
  }
  updateShareSummary();
}

$("tsCopy").onclick = async () => {
  try { await navigator.clipboard.writeText(tsStatus.url); $("tsCopy").textContent = "Copied"; setTimeout(() => ($("tsCopy").textContent = "Copy link"), 1500); }
  catch { prompt("Copy this link:", tsStatus.url); }
};
const tsToggle = $("tsToggle");
if (tsToggle) {
  tsToggle.onclick = async () => {
    const turnOn = tsToggle.dataset.on !== "1";
    try {
      await api("/api/tailscale", { method: "POST", body: { enabled: turnOn } });
      applyTailscale({ state: turnOn ? "starting" : "stopped" });
      setTimeout(refreshStatus, 1500);
    } catch (err) { showBanner(err.message, "error"); }
  };
}

$("shareCopy").onclick = async () => {
  try { await navigator.clipboard.writeText(tunnelStatus.url); $("shareCopy").textContent = "Copied"; setTimeout(() => ($("shareCopy").textContent = "Copy link"), 1500); }
  catch { prompt("Copy this link:", tunnelStatus.url); }
};
const shareToggle = $("shareToggle");
if (shareToggle) {
  shareToggle.onclick = async () => {
    const turnOn = shareToggle.dataset.on !== "1";
    try {
      const st = await api("/api/tunnel", { method: "POST", body: { enabled: turnOn } });
      applyTunnel(st);
    } catch (err) { showBanner(err.message, "error"); }
  };
}

async function refreshStatus() {
  try {
    const data = await api("/api/status");
    if (!data) return;
    applyVMStatus(data.vm);
    applyControl(data.control);
    applyTunnel(data.tunnel);
    applyTailscale(data.tailscale);
  } catch (err) {
    setStatus("bad", "Server unreachable");
  }
}

// --- toolbar wiring ---

$("btnCAD").onclick = () => rfb && rfb.sendCtrlAltDel();
$("btnWin").onclick = () => {
  if (!rfb) return;
  rfb.sendKey(KeyTable.XK_Super_L, "MetaLeft", true);
  rfb.sendKey(KeyTable.XK_Super_L, "MetaLeft", false);
};
$("btnScale").onclick = (e) => {
  scale = !scale;
  e.currentTarget.setAttribute("aria-pressed", String(scale));
  if (rfb) rfb.scaleViewport = scale;
};
$("btnFullscreen").onclick = () => {
  if (document.fullscreenElement) document.exitFullscreen();
  else document.documentElement.requestFullscreen().catch(() => {});
};
btnControl.onclick = async () => {
  try {
    const c = await api("/api/control/take", { method: "POST" });
    mode = "control";
    disconnect();
    connect();
    applyControl(c);
  } catch (err) { showBanner(err.message, "error"); }
};

$("btnClipboard").onclick = () => { $("clipPanel").hidden = !$("clipPanel").hidden; };
$("clipClose").onclick = () => { $("clipPanel").hidden = true; };
$("clipSend").onclick = () => {
  if (rfb) rfb.clipboardPasteFrom($("clipText").value);
  $("clipPanel").hidden = true;
  rfb && rfb.focus();
};

$("btnLog").onclick = () => { $("logPanel").hidden = !$("logPanel").hidden; refreshStatus(); };
$("logClose").onclick = () => { $("logPanel").hidden = true; };

$("btnPassword").onclick = async () => {
  const pw = prompt("New password (at least 8 characters):");
  if (!pw) return;
  try { await api("/api/password", { method: "POST", body: { password: pw } }); alert("Password changed."); }
  catch (err) { alert(err.message); }
};

async function vmAction(action) {
  try {
    const dangerous = { forcestop: "Force stop is like pulling the power cord. Continue?", reset: "Hard reset the VM?" };
    if (dangerous[action] && !confirm(dangerous[action])) return;
    setStatus("warn", "Working…");
    const st = await api("/api/vm/" + action, { method: "POST" });
    if (st) applyVMStatus(st);
  } catch (err) { showBanner(err.message, "error"); }
  document.querySelectorAll("details.menu[open]").forEach((d) => (d.open = false));
}
document.querySelectorAll("[data-vm]").forEach((b) => (b.onclick = () => vmAction(b.dataset.vm)));
$("btnStartBig").onclick = () => vmAction("start");

// --- snapshots ---

async function loadSnapshots() {
  const list = $("snapList");
  list.innerHTML = "";
  try {
    const snaps = await api("/api/snapshots");
    if (!snaps || snaps.length === 0) { list.innerHTML = '<div class="muted small">No snapshots</div>'; return; }
    for (const s of snaps) {
      const item = document.createElement("div");
      item.className = "item";
      const name = document.createElement("span");
      name.textContent = `${s.tag} · ${s.date || ""}`;
      const actions = document.createElement("span");
      actions.className = "actions";
      const restore = document.createElement("button");
      restore.textContent = "Restore";
      restore.onclick = async () => {
        if (!confirm(`Restore snapshot "${s.tag}"? Current VM state will be lost.`)) return;
        try { await api(`/api/snapshots/${encodeURIComponent(s.tag)}/restore`, { method: "POST" }); showBanner(`Restored ${s.tag}`, "info"); }
        catch (err) { showBanner(err.message, "error"); }
      };
      const del = document.createElement("button");
      del.textContent = "Delete";
      del.className = "danger";
      del.onclick = async () => {
        if (!confirm(`Delete snapshot "${s.tag}"?`)) return;
        try { await api(`/api/snapshots/${encodeURIComponent(s.tag)}`, { method: "DELETE" }); loadSnapshots(); }
        catch (err) { showBanner(err.message, "error"); }
      };
      actions.append(restore, del);
      item.append(name, actions);
      list.appendChild(item);
    }
  } catch (err) {
    list.innerHTML = `<div class="muted small">${err.message}</div>`;
  }
}
$("snapshotMenu").parentElement.addEventListener("toggle", (e) => { if (e.target.open) loadSnapshots(); });
$("snapForm").onsubmit = async (e) => {
  e.preventDefault();
  const name = $("snapName").value.trim();
  try {
    showBanner("Saving snapshot… the VM is paused briefly.", "info");
    await api("/api/snapshots", { method: "POST", body: { name } });
    hideBanner();
    $("snapName").value = "";
    loadSnapshots();
  } catch (err) { showBanner(err.message, "error"); }
};

// Close open menus when clicking elsewhere.
document.addEventListener("click", (e) => {
  document.querySelectorAll("details.menu[open]").forEach((d) => { if (!d.contains(e.target)) d.open = false; });
});

// --- boot ---
applyVMStatus(boot.status);
refreshStatus();
setInterval(refreshStatus, 5000);
