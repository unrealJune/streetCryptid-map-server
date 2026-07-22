# Bake the world-wide OpenMapTiles planet to a versioned PMTiles file with
# Planetiler. Run OUTSIDE Kubernetes on a workstation/CI box: ~80 GB download,
# ~60-100 GB output, ~64 GB RAM recommended, potentially a day of processing.
#
# Do NOT run this as a Helm hook, init container, sidecar, or normal K8s Job.
[CmdletBinding()]
param(
    [string]$Version = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ"),
    [string]$OutDir = "./data",
    [string]$PlanetilerJar = "planetiler.jar",
    [int]$MaxZoom = 14,
    # Restrict baked name:<lang> label variants. OpenMapTiles otherwise emits
    # ~40 languages per label feature, bloating tiles and bundles. "en" keeps
    # name:en; the base name (local), name:latin, name:nonlatin, and name_int
    # are always emitted regardless. Pass "default" to restore the full OMT set.
    [string]$Languages = "en"
)
$ErrorActionPreference = "Stop"

New-Item -ItemType Directory -Force $OutDir | Out-Null
$safe = ($Version -replace '[^A-Za-z0-9._-]', '-')
$out = Join-Path $OutDir "planet-$safe.pmtiles"

Write-Host "Baking planet version=$Version -> $out"
java -Xmx56g -jar $PlanetilerJar `
    --download `
    --area=planet `
    --maxzoom=$MaxZoom `
    --languages=$Languages `
    --nodemap-type=array `
    --storage=mmap `
    --output=$out

Write-Host "Done: $out"
Write-Host "Next: scripts/publish-release.ps1 -Version `"$Version`" -PMTiles `"$out`""
