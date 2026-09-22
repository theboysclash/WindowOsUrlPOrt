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
