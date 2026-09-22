<#
.SYNOPSIS
  Makes vmserver.exe start automatically at boot (as SYSTEM, before anyone
  logs in) and opens the firewall port. Uses a scheduled task, which works with
  any executable. Run as Administrator from the folder that contains
  vmserver.exe. Use -Uninstall to remove.
#>
param(
  [switch]$Uninstall,
  [int]$Port = 8443
)

$ErrorActionPreference = "Stop"
$task = "VMWebServer"
$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$exe = Join-Path $dir "vmserver.exe"

if ($Uninstall) {
  schtasks /End /TN $task 2>$null | Out-Null
  schtasks /Delete /TN $task /F | Out-Null
  netsh advfirewall firewall delete rule name="VM Web Server" | Out-Null
  Write-Host "Autostart removed."
  exit 0
}

if (-not (Test-Path $exe)) { throw "vmserver.exe not found next to this script" }

$action = New-ScheduledTaskAction -Execute $exe -Argument "-dir `"$dir`"" -WorkingDirectory $dir
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Days 3650) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName $task -Action $action -Trigger $trigger -Settings $settings -User "SYSTEM" -RunLevel Highest -Force | Out-Null

netsh advfirewall firewall add rule name="VM Web Server" dir=in action=allow protocol=TCP localport=$Port | Out-Null
Start-ScheduledTask -TaskName $task
Write-Host "Task '$task' installed and started. Console: https://localhost:$Port"
Write-Host "First-run admin password: see data\initial-credentials.txt next to vmserver.exe"
