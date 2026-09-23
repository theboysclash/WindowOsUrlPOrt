const $ = (id) => document.getElementById(id);

async function post(path, body) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    credentials: "same-origin",
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function showError(msg) {
  const el = $("setupError");
  el.textContent = msg;
  el.hidden = !msg;
}

let uploadsInFlight = 0;

$("setupForm").onsubmit = async (e) => {
  e.preventDefault();
  showError("");
  if (uploadsInFlight > 0) {
    showError("Wait for the ISO upload to finish before saving.");
    return;
  }
  const f = new FormData(e.target);
  const body = {
    iso_path: f.get("iso_path").trim(),
    virtio_iso_path: f.get("virtio_iso_path").trim(),
    ram_mb: Number(f.get("ram_mb")),
    cpus: Number(f.get("cpus")),
    disk_gb: Number(f.get("disk_gb") || 0),
    disk_interface: f.get("disk_interface"),
    accel: f.get("accel"),
    clipboard_sync: f.get("clipboard_sync") === "on",
    start: f.get("start") === "on",
  };
  const btn = $("setupSubmit");
  btn.disabled = true;
  btn.textContent = "Working…";
  try {
    await post("/api/setup", body);
    location.href = "/console";
  } catch (err) {
    showError(err.message);
    btn.disabled = false;
    btn.textContent = "Save";
  }
};

const eject = $("btnEject");
if (eject) {
  eject.onclick = async () => {
    if (!confirm("Detach the installation ISO? Do this only after Windows setup has finished.")) return;
    try { await post("/api/vm/eject", {}); location.reload(); }
    catch (err) { showError(err.message); }
  };
}

$("userForm").onsubmit = async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  try {
    await post("/api/users", { username: f.get("username"), password: f.get("password"), admin: f.get("admin") === "on" });
    alert("User saved.");
    e.target.reset();
  } catch (err) { alert(err.message); }
};

// --- sharing / Cloudflare Tunnel ---

const tunnelForm = $("tunnelForm");
const modeSel = $("tunnelMode");
modeSel.onchange = () => { $("namedFields").hidden = modeSel.value !== "named"; };

function describeTunnel(st) {
  const el = $("tunnelStatus");
  el.innerHTML = "";
  if (st.state === "connected" && st.url) {
    el.append("Connected: ");
    const a = document.createElement("a");
    a.href = st.url; a.textContent = st.url; a.target = "_blank"; a.rel = "noopener";
    el.appendChild(a);
  } else if (st.state === "error") {
    el.textContent = "Error: " + (st.error || "unknown");
  } else {
    el.textContent = "State: " + st.state;
  }
}

tunnelForm.onsubmit = async (e) => {
  e.preventDefault();
  $("tunnelError").hidden = true;
  const f = new FormData(tunnelForm);
  try {
    const st = await post("/api/tunnel", {
      enabled: f.get("enabled") === "on",
      mode: f.get("mode"),
      token: f.get("token") || "",
      hostname: f.get("hostname") || "",
      auto_download: f.get("auto_download") === "on",
    });
    describeTunnel(st);
    pollTunnel(20);
  } catch (err) {
    $("tunnelError").textContent = err.message;
    $("tunnelError").hidden = false;
  }
};

async function pollTunnel(times) {
  for (let i = 0; i < times; i++) {
    await new Promise((r) => setTimeout(r, 1500));
    try {
      const res = await fetch("/api/status", { credentials: "same-origin" });
      const data = await res.json();
      describeTunnel(data.tunnel);
      if (data.tunnel.state === "connected" || data.tunnel.state === "disabled") return;
    } catch { /* keep polling */ }
  }
}

$("tunnelLogBtn").onclick = async () => {
  const pre = $("tunnelLog");
  pre.hidden = !pre.hidden;
  if (pre.hidden) return;
  try {
    const res = await fetch("/api/tunnel/log", { credentials: "same-origin" });
    const data = await res.json();
    pre.textContent = (data.log || []).join("\n") || "(no output yet)";
  } catch (err) { pre.textContent = err.message; }
};

if (location.hash === "#sharing") document.getElementById("sharing").scrollIntoView();

// --- Tailscale ---

$("tsForm").onsubmit = async (e) => {
  e.preventDefault();
  $("tsError").hidden = true;
  const f = new FormData(e.target);
  try {
    await post("/api/tailscale", {
      enabled: f.get("enabled") === "on",
      funnel: f.get("funnel") === "on",
      hostname: f.get("hostname") || "",
      auth_key: f.get("auth_key") || "",
    });
    $("tsStatusBox").textContent = "State: starting";
    pollTailscale(40);
  } catch (err) {
    $("tsError").textContent = err.message;
    $("tsError").hidden = false;
  }
};

async function pollTailscale(times) {
  const box = $("tsStatusBox");
  for (let i = 0; i < times; i++) {
    await new Promise((r) => setTimeout(r, 1500));
    try {
      const res = await fetch("/api/status", { credentials: "same-origin" });
      const st = (await res.json()).tailscale;
      box.innerHTML = "";
      if (st.state === "running" && st.url) {
        box.append("Connected: ");
        const a = document.createElement("a");
        a.href = st.url; a.textContent = st.url; a.target = "_blank"; a.rel = "noopener";
        box.appendChild(a);
        if (st.note) box.append(" — " + st.note);
        return;
      } else if (st.state === "needs_login" && st.auth_url) {
        box.append("Waiting for sign-in: ");
        const a = document.createElement("a");
        a.href = st.auth_url; a.textContent = "open the Tailscale login link"; a.target = "_blank"; a.rel = "noopener";
        box.appendChild(a);
      } else if (st.state === "error") {
        box.textContent = "Error: " + (st.error || "unknown");
        return;
      } else {
        box.textContent = "State: " + st.state;
        if (st.state === "disabled" || st.state === "stopped") return;
      }
    } catch { /* keep polling */ }
  }
}

// --- ISO drag and drop upload ---

const CHUNK = 32 * 1024 * 1024;

function fmtSize(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(2) + " GB";
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(0) + " MB";
  return n + " B";
}

function fileName(p) {
  return p ? p.split(/[\\/]/).pop() : "";
}

function sendChunk(url, blob, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", url);
    xhr.withCredentials = true;
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      let data = {};
      try { data = JSON.parse(xhr.responseText); } catch { /* not json */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(data);
      else reject(Object.assign(new Error(data.error || `HTTP ${xhr.status}`), { status: xhr.status, data }));
    };
    xhr.onerror = () => reject(new Error("network error"));
    xhr.send(blob);
  });
}

async function uploadISO(file, zone, input) {
  const bar = zone.querySelector(".dz-bar");
  const progress = zone.querySelector(".dz-progress");
  const status = zone.querySelector(".dz-status");
  status.classList.remove("err");
  if (!file.name.toLowerCase().endsWith(".iso")) {
    status.textContent = "That is not an .iso file.";
    status.classList.add("err");
    return;
  }
  uploadsInFlight++;
  zone.classList.add("busy");
  progress.hidden = false;
  const started = Date.now();
  let offset = 0;
  try {
    while (offset < file.size || file.size === 0) {
      const end = Math.min(offset + CHUNK, file.size);
      const url = `/api/upload/iso?name=${encodeURIComponent(file.name)}&offset=${offset}&total=${file.size}`;
      let res;
      for (let attempt = 1; ; attempt++) {
        try {
          res = await sendChunk(url, file.slice(offset, end), (loaded) => {
            const done = offset + loaded;
            const pct = file.size ? (done / file.size) * 100 : 100;
            const secs = (Date.now() - started) / 1000;
            const rate = secs > 0 ? done / secs : 0;
            const left = rate > 0 ? Math.round((file.size - done) / rate) : 0;
            bar.style.width = pct.toFixed(1) + "%";
            status.textContent = `Uploading ${file.name}: ${fmtSize(done)} of ${fmtSize(file.size)} (${pct.toFixed(0)}%)` +
              (left > 0 ? `, about ${left > 90 ? Math.round(left / 60) + " min" : left + " s"} left` : "");
          });
          break;
        } catch (err) {
          if (attempt >= 4 || (err.status && err.status < 500 && err.status !== 409)) throw err;
          status.textContent = `Connection hiccup, retrying (${attempt}/3)…`;
          await new Promise((r) => setTimeout(r, 1500 * attempt));
        }
      }
      offset = end;
      if (res.done) {
        input.value = res.path;
        zone.querySelector(".dz-current").textContent = `Selected: ${res.name}`;
        bar.style.width = "100%";
        status.textContent = `Uploaded ${fmtSize(file.size)}. Click ${$("setupSubmit").textContent.trim()} below to use it.`;
        loadISOList();
        return;
      }
      if (file.size === 0) break;
    }
  } catch (err) {
    status.textContent = "Upload failed: " + err.message;
    status.classList.add("err");
  } finally {
    uploadsInFlight--;
    zone.classList.remove("busy");
    setTimeout(() => { if (!zone.classList.contains("busy")) progress.hidden = true; }, 1500);
  }
}

document.querySelectorAll(".dropzone").forEach((zone) => {
  const input = document.querySelector(`input[name="${zone.dataset.target}"]`);
  const picker = zone.querySelector("input[type=file]");
  const current = zone.querySelector(".dz-current");
  current.textContent = input.value ? `Selected: ${fileName(input.value)}` : "";
  input.addEventListener("input", () => { current.textContent = input.value ? `Selected: ${fileName(input.value)}` : ""; });

  const choose = () => { if (!zone.classList.contains("busy")) picker.click(); };
  zone.addEventListener("click", choose);
  zone.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); choose(); } });
  picker.addEventListener("change", () => { if (picker.files[0]) uploadISO(picker.files[0], zone, input); picker.value = ""; });

  zone.addEventListener("dragover", (e) => { e.preventDefault(); zone.classList.add("over"); });
  zone.addEventListener("dragleave", () => zone.classList.remove("over"));
  zone.addEventListener("drop", (e) => {
    e.preventDefault();
    zone.classList.remove("over");
    const file = e.dataTransfer.files[0];
    if (file && !zone.classList.contains("busy")) uploadISO(file, zone, input);
  });
});

// A file dropped next to a zone would otherwise make the browser open it.
window.addEventListener("dragover", (e) => e.preventDefault());
window.addEventListener("drop", (e) => e.preventDefault());

window.addEventListener("beforeunload", (e) => {
  if (uploadsInFlight > 0) { e.preventDefault(); e.returnValue = ""; }
});

async function loadISOList() {
  const box = document.querySelector('.iso-list[data-for="iso_path"]');
  if (!box) return;
  try {
    const res = await fetch("/api/isos", { credentials: "same-origin" });
    const list = await res.json();
    box.innerHTML = "";
    if (!Array.isArray(list) || list.length === 0) return;
    const label = document.createElement("div");
    label.className = "muted";
    label.textContent = "Uploaded earlier:";
    box.appendChild(label);
    for (const iso of list) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = `${iso.name} (${fmtSize(iso.size)})`;
      b.onclick = () => {
        const input = document.querySelector('input[name="iso_path"]');
        input.value = iso.path;
        input.dispatchEvent(new Event("input"));
      };
      box.appendChild(b);
    }
  } catch { /* list is optional */ }
}
loadISOList();
