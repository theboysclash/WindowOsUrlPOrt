# VM Web Server

One `vmserver.exe` that turns the PC it runs on into a host for a Windows 10 virtual machine you can use from any browser. Other PCs open a URL, sign in, and get the VM's screen with full keyboard, mouse and clipboard control, plus power, snapshot and sharing controls: a VM console in a tab.

```
other PC's browser ──HTTPS/WSS──> vmserver.exe ──VNC (loopback)──> QEMU running Windows 10
                                      │
                    (optional) Cloudflare Tunnel ──> public https://….trycloudflare.com link
```

## What is in the box

| Piece | Implementation |
|---|---|
| Server | Go, single static executable, all web assets embedded |
| Hypervisor | QEMU with WHPX (Windows Hypervisor Platform) acceleration, software fallback |
| Display | QEMU VNC → authenticated WebSocket bridge → [noVNC](https://novnc.com) canvas |
| Login | bcrypt passwords, HttpOnly/SameSite cookies, per-IP lockout, idle and absolute timeouts |
| Sharing | `cloudflared` quick tunnel (no account) or named tunnel (your own hostname) |
| Controls | start / ACPI shutdown / reset / pause / force stop, live snapshots, Ctrl+Alt+Del, Win key, clipboard, fullscreen, one controller with take-over, view-only spectators |

## Windows image and licensing

The executable does **not** contain Windows. On first run the admin opens **VM setup**, points it at a Windows 10 ISO that is already on the host PC, and the installer boots inside the console. Use official media from Microsoft's Media Creation Tool and your own licence key.

"Tiny10"-style ISOs are third-party modified Windows images. They will boot here like any other ISO (Tiny10 works fine with the default SATA disk), but redistributing them or shipping one inside this program violates Microsoft's licence terms, so this project never bundles one. If you want a small guest, install stock Windows 10 and debloat it inside the VM.

## Quick start (host PC)

1. Install [QEMU for Windows](https://qemu.weilnetz.de/w64/) (default location is fine) **or** unzip a release that already contains `third_party\qemu`.
2. Enable hardware virtualization: run `scripts\enable-whpx.ps1` as Administrator once and reboot. Without it the VM still runs, but slowly, and the console shows a "Software emulation" chip.
3. Double-click `vmserver.exe`. The window prints the LAN URLs and a one-time admin password (also saved to `data\initial-credentials.txt`).
4. From any PC on the same network open `https://<host-ip>:8443`, accept the self-signed certificate warning, sign in.
5. Open **admin ▸ VM setup**, enter the ISO path, memory, CPUs and disk size, click **Create and start**. The Windows installer appears in the console; install as usual. Afterwards click **Detach installation media**.
6. Change the admin password (**admin ▸ Change password**) and add users if needed.

Command-line flags: `-dir <folder>` (config/data location), `-listen host:port`, `-share` (public link for this run), `-no-vm`, `-add-user user:pass[:admin]`, `-reset-admin-password`, `-print-urls`.

## Sharing a link with people outside your network

Router port forwarding is not needed. The server can run a Cloudflare Tunnel connector next to it:

* **Quick tunnel** – free, no account. Click **Share ▸ Turn on sharing** in the console (or start with `-share`). Within a few seconds you get an `https://<random-words>.trycloudflare.com` link that anyone can open; they still have to sign in. The link changes whenever the server restarts.
* **Named tunnel** – a fixed hostname on your own domain. In the Cloudflare dashboard go to *Zero Trust ▸ Networks ▸ Tunnels ▸ Create a tunnel*, copy the connector token, and set the public hostname's service to `https://localhost:8443` with **No TLS Verify** enabled. Paste the token in **VM setup ▸ Sharing**, choose *Named tunnel*, save.

`cloudflared.exe` is downloaded from GitHub automatically the first time sharing is switched on (or bundled by `scripts\build.ps1 -BundleCloudflared`). Login rate limiting uses the real visitor address that Cloudflare supplies.

Because a shared link is reachable from the whole internet: use long passwords, keep the number of accounts small, and turn sharing off when you do not need it.

## Running at boot

`scripts\install-service.ps1` (Administrator) registers a scheduled task that starts the server as SYSTEM at boot and opens the firewall port. `-Uninstall` removes it.

## Clipboard, snapshots and guest tools

* Clipboard sync between browser and guest uses the QEMU vdagent channel; install [spice-guest-tools](https://www.spice-space.org/download.html) inside Windows to activate it. QEMU cannot take live snapshots while that channel is attached, so **VM setup** has a *Clipboard sync* switch: turn it off if you prefer snapshots.
* For the fastest disk, choose the *virtio* interface and attach [virtio-win.iso](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/) during installation (load the driver when the installer cannot see a disk). SATA works with stock media and no extra drivers.
* Audio is not transmitted (VNC has no audio channel).

## Building from source

Requires Go 1.22+.

```powershell
go test ./...
powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -BundleCloudflared   # dist\vmserver-windows-amd64.zip
```

or on any OS: `go build -o vmserver ./cmd/vmserver`. The server also runs on Linux (KVM) and macOS (HVF) hosts; only the packaging scripts are Windows-specific.

## Layout

```
cmd/vmserver/        entry point, flags, first-run bootstrap
internal/config/     config.yaml schema and validation
internal/auth/       passwords, sessions, lockout, Cloudflare client IP
internal/vm/         QEMU command line, process supervision, QMP client, snapshots
internal/proxy/      WebSocket <-> VNC bridge with server-side VNC auth and view-only filter
internal/tunnel/     cloudflared supervisor (quick/named), auto-download
internal/httpserver/ routes, middleware, TLS, API handlers
web/                 embedded templates, CSS, console/setup scripts, vendored noVNC (MPL-2.0)
scripts/             build.ps1, enable-whpx.ps1, install-service.ps1
```

## Security notes

* The VNC and QMP ports bind to 127.0.0.1 only and the VNC password is random per boot and never sent to browsers; the bridge authenticates to QEMU itself.
* Cookies are HttpOnly, Secure and SameSite=Strict; state-changing API calls additionally check `Sec-Fetch-Site`/`Origin`.
* A strict Content-Security-Policy is sent; there is no inline script.
* Only one signed-in session controls input at a time. Others are view-only until they click *Take control*; the restriction is enforced server-side by filtering RFB input messages, not just in the browser.
