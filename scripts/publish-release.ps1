# Publish a baked PMTiles release: generate + sign the manifest and upload in the
# durability-safe order (artifact, then signature, then manifest.json last).
#
# The Ed25519 signing PRIVATE key stays offline and never enters the cluster.
# Generate one once with:
#   openssl genpkey -algorithm ed25519 -out tiles-manifest.key
#   scripts/publish-release.ps1 -PubKey tiles-manifest.key   # prints raw hex pubkey
[CmdletBinding()]
param(
    [string]$Version,
    [string]$PMTiles,
    [string]$BaseUrl,
    [string]$SigningKey = "tiles-manifest.key",
    [int]$MinZoom = 0,
    [int]$MaxZoom = 14,
    [string]$PubKey,
    # Script block invoked as & $UploadCmd <local> <remoteName>
    [scriptblock]$UploadCmd
)
$ErrorActionPreference = "Stop"

if ($PubKey) {
    $der = openssl pkey -in $PubKey -pubout -outform DER
    $bytes = [byte[]]$der
    ($bytes[-32..-1] | ForEach-Object { $_.ToString("x2") }) -join ""
    exit 0
}

if (-not $Version -or -not $PMTiles -or -not $BaseUrl) {
    throw "Usage: publish-release.ps1 -Version <v> -PMTiles <file> -BaseUrl <https://...> [-UploadCmd { param(`$l,`$r) ... }]"
}

$safe = ($Version -replace '[^A-Za-z0-9._-]', '-')
$remoteName = "planet-$safe.pmtiles"
$size = (Get-Item $PMTiles).Length
$sha = (Get-FileHash -Algorithm SHA256 $PMTiles).Hash.ToLower()
$created = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$manifest = @"
{
  "schema_version": 1,
  "version": "$Version",
  "url": "$BaseUrl/$remoteName",
  "sha256": "$sha",
  "size": $size,
  "min_zoom": $MinZoom,
  "max_zoom": $MaxZoom,
  "tile_schema": "openmaptiles",
  "created_at": "$created"
}
"@

# Write with no trailing newline so the signed bytes are exact.
[System.IO.File]::WriteAllText("manifest.json", $manifest)
openssl pkeyutl -sign -inkey $SigningKey -rawin -in manifest.json -out manifest.sig

Write-Host "== Upload order: artifact -> signature -> manifest.json (last) =="
if ($UploadCmd) {
    & $UploadCmd $PMTiles $remoteName
    & $UploadCmd "manifest.sig" "manifest.sig"
    & $UploadCmd "manifest.json" "manifest.json"
    Write-Host "Published $Version."
} else {
    Write-Host "No -UploadCmd given. Upload manually in this order:"
    Write-Host "  1. $PMTiles  -> $remoteName"
    Write-Host "  2. manifest.sig -> manifest.sig"
    Write-Host "  3. manifest.json -> manifest.json (LAST)"
}
