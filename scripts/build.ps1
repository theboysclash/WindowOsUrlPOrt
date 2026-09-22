<#
.SYNOPSIS
  Builds vmserver.exe and assembles a ready-to-run folder in dist\.

.DESCRIPTION
  1. Compiles the Go program for Windows x64 as a GUI-less console app.
  2. Copies the QEMU for Windows installation into dist\third_party\qemu
     (pass -QemuDir, defaults to C:\Program Files\qemu). QEMU is not in this
     repository; install it from https://qemu.weilnetz.de/w64/ first.
  3. Optionally bundles cloudflared.exe so sharing works offline from GitHub
     (-BundleCloudflared). Otherwise the server downloads it on first use.
  4. Zips everything into dist\vmserver-windows-amd64.zip.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -BundleCloudflared
#>
param(
  [string]$QemuDir = "C:\Program Files\qemu",
  [switch]$BundleCloudflared,
  [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$dist = Join-Path $root "dist"
if (Test-Path $dist) { Remove-Item -Recurse -Force $dist }
New-Item -ItemType Directory -Path $dist | Out-Null

Write-Host ">> go build"
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $dist "vmserver.exe") ./cmd/vmserver
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

if (Test-Path $QemuDir) {
  Write-Host ">> bundling QEMU from $QemuDir"
  $q = Join-Path $dist "third_party\qemu"
  New-Item -ItemType Directory -Path $q | Out-Null
  # Only the x86_64 system emulator, qemu-img, DLLs and firmware are needed.
  Copy-Item (Join-Path $QemuDir "qemu-system-x86_64.exe") $q
  Copy-Item (Join-Path $QemuDir "qemu-img.exe") $q
  Copy-Item (Join-Path $QemuDir "*.dll") $q
  Copy-Item (Join-Path $QemuDir "share") (Join-Path $q "share") -Recurse
  foreach ($f in @("bios-256k.bin","efi-e1000.rom","kvmvapic.bin","vgabios-stdvga.bin","linuxboot_dma.bin","COPYING")) {
    $p = Join-Path $QemuDir $f
    if (Test-Path $p) { Copy-Item $p $q }
  }
} else {
  Write-Warning "QEMU not found at $QemuDir; dist will need vm.qemu_dir set or QEMU on PATH."
}

if ($BundleCloudflared) {
  Write-Host ">> downloading cloudflared"
  $c = Join-Path $dist "third_party\cloudflared"
  New-Item -ItemType Directory -Path $c | Out-Null
  Invoke-WebRequest -Uri "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-amd64.exe" -OutFile (Join-Path $c "cloudflared.exe")
}

Copy-Item (Join-Path $root "scripts\enable-whpx.ps1") $dist
Copy-Item (Join-Path $root "scripts\install-service.ps1") $dist
Copy-Item (Join-Path $root "config.example.yaml") $dist
Copy-Item (Join-Path $root "README.md") $dist
Copy-Item (Join-Path $root "LICENSE") $dist

Write-Host ">> zipping"
Compress-Archive -Path (Join-Path $dist "*") -DestinationPath (Join-Path $dist "vmserver-windows-amd64.zip") -Force
Write-Host "Done: $dist"
