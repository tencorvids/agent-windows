[CmdletBinding()]
param(
  [Parameter(Mandatory)] [string] $DeviceID,
  [string] $Broker = "mqtts://harmonyconnected.com:8883",
  [string] $Username = "",
  [string] $Password = "",
  [int] $PollIntervalSeconds = 30,
  [string] $BinaryUrl = ""
)

$ErrorActionPreference = "Stop"

$ServiceName = "HarmonyAgent"
$ReleaseBaseUrl = "https://github.com/HarmonyConnected/HarmonyAgent/releases/latest/download"
$InstallDir = "C:\Program Files\HarmonyAgent"
$ConfigDir = "C:\ProgramData\HarmonyAgent"
$BinaryPath = Join-Path $InstallDir "HarmonyAgent.exe"
$ConfigPath = Join-Path $ConfigDir "windows.yaml"

if ([string]::IsNullOrWhiteSpace($BinaryUrl)) {
  $BinaryUrl = "$ReleaseBaseUrl/HarmonyAgent.exe"
}

function Assert-Administrator {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "HarmonyAgent installer must be run from an elevated PowerShell session."
  }
}

function ConvertTo-YamlSingleQuotedString([string] $Value) {
  if ($null -eq $Value) {
    $Value = ""
  }
  return "'" + $Value.Replace("'", "''") + "'"
}

function Stop-HarmonyAgentService {
  $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($null -eq $service) {
    return $false
  }

  if ($service.Status -ne "Stopped") {
    Write-Host "Stopping $ServiceName..."
    Stop-Service -Name $ServiceName -Force
    $service.WaitForStatus("Stopped", "00:00:30")
  }

  return $true
}

Assert-Administrator

Write-Host "Creating HarmonyAgent directories..."
New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null

$serviceExists = Stop-HarmonyAgentService

Write-Host "Downloading HarmonyAgent from $BinaryUrl..."
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
Invoke-WebRequest -UseBasicParsing -Uri $BinaryUrl -OutFile $BinaryPath

if (Get-Command Unblock-File -ErrorAction SilentlyContinue) {
  Unblock-File -Path $BinaryPath
}

Write-Host "Writing config to $ConfigPath..."
$configContent = @"
Broker: $(ConvertTo-YamlSingleQuotedString $Broker)
Username: $(ConvertTo-YamlSingleQuotedString $Username)
Password: $(ConvertTo-YamlSingleQuotedString $Password)
DeviceID: $(ConvertTo-YamlSingleQuotedString $DeviceID)
PollIntervalSeconds: $PollIntervalSeconds
"@
Set-Content -Path $ConfigPath -Value $configContent -Encoding UTF8

if ($serviceExists) {
  Write-Host "Uninstalling existing $ServiceName service..."
  & $BinaryPath -service uninstall -config $ConfigPath
  Start-Sleep -Seconds 2
}

Write-Host "Installing $ServiceName service..."
& $BinaryPath -service install -config $ConfigPath

Write-Host "Starting $ServiceName service..."
& $BinaryPath -service start -config $ConfigPath

$service = Get-Service -Name $ServiceName
$service.WaitForStatus("Running", "00:00:30")
Write-Host "HarmonyAgent service status: $($service.Status)"
