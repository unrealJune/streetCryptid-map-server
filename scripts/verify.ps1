# Verify a local PMTiles + manifest + signature triple before publishing.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$PMTiles,
    [Parameter(Mandatory)][string]$Manifest,
    [string]$PubKeyPem,
    [string]$Sig = "manifest.sig"
)
$ErrorActionPreference = "Stop"

Write-Host "== Signature =="
if ($PubKeyPem) {
    openssl pkeyutl -verify -pubin -inkey $PubKeyPem -rawin -in $Manifest -sigfile $Sig
} else {
    Write-Host "(skipping openssl verify; pass -PubKeyPem to check the signature)"
}

$json = Get-Content -Raw $Manifest | ConvertFrom-Json
$gotSha = (Get-FileHash -Algorithm SHA256 $PMTiles).Hash.ToLower()
$gotSize = (Get-Item $PMTiles).Length
if ($json.sha256.ToLower() -ne $gotSha) { throw "SHA mismatch: $gotSha != $($json.sha256)" }
if ([int64]$json.size -ne $gotSize) { throw "size mismatch: $gotSize != $($json.size)" }
Write-Host "sha256 OK ($gotSha)"
Write-Host "size OK ($gotSize)"

$magic = [System.Text.Encoding]::ASCII.GetString([System.IO.File]::ReadAllBytes($PMTiles)[0..6])
if ($magic -ne "PMTiles") { throw "not a PMTiles file (magic=$magic)" }
Write-Host "PMTiles magic OK"
Write-Host "All checks passed."
