<#
.SYNOPSIS
  Enables the Windows Hypervisor Platform so QEMU can run the guest with
  hardware acceleration (WHPX). Run as Administrator, then reboot.
#>
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Error "Run this script from an elevated (Administrator) PowerShell."
  exit 1
}

$features = @("HypervisorPlatform", "VirtualMachinePlatform")
foreach ($f in $features) {
  $state = (Get-WindowsOptionalFeature -Online -FeatureName $f).State
  if ($state -eq "Enabled") {
    Write-Host "$f already enabled"
  } else {
    Write-Host "Enabling $f ..."
    Enable-WindowsOptionalFeature -Online -FeatureName $f -NoRestart | Out-Null
  }
}

bcdedit /set hypervisorlaunchtype auto | Out-Null
Write-Host ""
Write-Host "Done. Reboot the PC, then start vmserver.exe again."
Write-Host "If the console still says 'Software emulation', enable virtualization (VT-x / AMD-V / SVM) in the BIOS/UEFI."
