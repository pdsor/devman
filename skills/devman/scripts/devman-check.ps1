# Reports DevMan availability and this project's state as a single JSON object.
#
# The script never starts anything: if the daemon is not running it says so,
# rather than launching a background process as a side effect of a check.
#
#   pwsh -File devman-check.ps1 [-Path <project dir>]

[CmdletBinding()]
param(
    [string]$Path = (Get-Location).Path
)

$ErrorActionPreference = 'Continue'

$resolved = Resolve-Path -LiteralPath $Path -ErrorAction SilentlyContinue
$projectDir = if ($resolved) { $resolved.Path } else { $Path }

function Invoke-DevmanJson {
    # Runs devman and returns the parsed object, or $null when the output is not
    # a single JSON document (a failing `validate` prints the result *and* the
    # error, so callers rely on the exit code as well).
    param([string[]]$Arguments)

    $text = (& devman @Arguments 2>&1) -join "`n"
    $script:LastDevmanExit = $LASTEXITCODE
    $script:LastDevmanText = $text
    try { return $text | ConvertFrom-Json } catch { return $null }
}

$result = [ordered]@{
    devman_installed = $false
    devman_version   = $null
    daemon_running   = $false
    project_dir      = $projectDir
    config_path      = $null
    config_present   = $false
    config_valid     = $null
    config_output    = $null
    registered       = $null
    trusted          = $null
    services         = @()
    next_action      = 'install_devman'
}

function Write-Result {
    $result | ConvertTo-Json -Depth 6
    exit 0
}

if (-not (Get-Command devman -ErrorAction SilentlyContinue)) {
    Write-Result
}
$result.devman_installed = $true

$version = Invoke-DevmanJson @('--json', 'version')
if ($version) { $result.devman_version = $version.version }

$configPath = Join-Path $projectDir 'devman.yaml'
$result.config_path = $configPath
if (-not (Test-Path -LiteralPath $configPath)) {
    $result.next_action = 'init'
    Write-Result
}
$result.config_present = $true

# validate runs locally, so config validity is known even with no daemon.
$null = Invoke-DevmanJson @('--json', 'validate', $projectDir)
$result.config_valid = ($script:LastDevmanExit -eq 0)
if (-not $result.config_valid) {
    $result.config_output = $script:LastDevmanText
    $result.next_action = 'fix_config'
    Write-Result
}

$daemon = Invoke-DevmanJson @('--json', 'daemon', 'status')
if ($daemon -and $daemon.PSObject.Properties.Name -contains 'info') {
    $result.daemon_running = $true
} else {
    # Registration lives in the daemon's database, so it cannot be answered here
    # without starting one. `devman status` will start it when the agent is ready.
    $result.next_action = 'query_status'
    Write-Result
}

$status = Invoke-DevmanJson @('--json', 'status', '--project', $projectDir)
if (-not $status) {
    $result.config_output = $script:LastDevmanText
    $result.next_action = 'query_status'
    Write-Result
}
if ($status.PSObject.Properties.Name -contains 'error') {
    if ($status.error.code -eq 'PROJECT_NOT_FOUND') {
        $result.registered = $false
        $result.next_action = 'register'
    } else {
        $result.config_output = $script:LastDevmanText
        $result.next_action = 'fix_config'
    }
    Write-Result
}

$result.registered = $true
$result.trusted = [bool]$status.trusted
$result.services = @(
    foreach ($service in @($status.services)) {
        [ordered]@{
            name    = $service.name
            status  = $service.status
            health  = $service.health.status
            ports   = @(foreach ($port in @($service.ports)) { $port.port })
            url     = $service.url
            message = $service.message
        }
    }
)

$running = @($result.services | Where-Object { $_.status -eq 'RUNNING' }).Count
$result.next_action = if (-not $result.trusted) { 'trust' }
    elseif ($running -eq 0) { 'start' }
    else { 'nothing' }

Write-Result
