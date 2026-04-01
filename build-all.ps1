# Build script for p2p-file-sync
# Usage: .\build-all.ps1

$ErrorActionPreference = "Stop"

$version = git describe --tags --always 2>$null
if (-not $version) { $version = "dev" }

Write-Host "Building p2p-file-sync $version for all platforms..." -ForegroundColor Cyan

New-Item -ItemType Directory -Path build -Force | Out-Null

$targets = @(
    @{ GOOS="windows"; GOARCH="amd64"; Ext=".exe" },
    @{ GOOS="linux";   GOARCH="amd64"; Ext="" },
    @{ GOOS="linux";   GOARCH="arm64"; Ext="" },
    @{ GOOS="darwin";  GOARCH="amd64"; Ext="" },
    @{ GOOS="darwin";  GOARCH="arm64"; Ext="" }
)

foreach ($t in $targets) {
    $output = "build/p2p-file-sync-$($t.GOOS)-$($t.GOARCH)$($t.Ext)"
    Write-Host "  Building $output..."
    $env:GOOS = $t.GOOS
    $env:GOARCH = $t.GOARCH
    go build -ldflags "-X main.version=$version" -o $output ./cmd/p2p-file-sync
}

Write-Host "`nBuild complete!" -ForegroundColor Green
Get-ChildItem build | Format-Table Name, @{N="Size (MB)";E={[math]::Round($_.Length/1MB, 1)}}
