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

$("setupForm").onsubmit = async (e) => {
  e.preventDefault();
  showError("");
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
