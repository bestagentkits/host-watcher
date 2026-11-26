# Pre-command check for Windows PowerShell. Exit 0 for go/throttle/unknown, 3 for clean/hold.
#   pwsh hooks/generic/host-pressure-check.ps1; if ($LASTEXITCODE -eq 0) { npm run build }
$root = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$py = (Get-Command python -ErrorAction SilentlyContinue) ?? (Get-Command py -ErrorAction SilentlyContinue)
if (-not $py) { Write-Warning 'host-pressure-check: python not found; skipping'; exit 0 }
$json = & $py.Source (Join-Path $root 'scripts\host_status.py') --json --samples 1 | ConvertFrom-Json
$verdict = $json.data.verdict
Write-Output "host verdict: $verdict"
if ($verdict -in @('clean', 'hold')) { exit 3 } else { exit 0 }
