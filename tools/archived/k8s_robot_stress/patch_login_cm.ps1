#!/usr/bin/env pwsh
# Patch login ConfigMaps to disable client-side circuit breaker
# for PlayerLocatorRpc and SceneManagerRpc.

param(
    [int]$Zones = 10
)

$ErrorActionPreference = 'Stop'

for ($z = 1; $z -le $Zones; $z++) {
    $ns = "mmorpg-zone-zone-$z"
    Write-Host "Patching $ns ..." -NoNewline

    $raw = kubectl get configmap go-svc-login-config -n $ns -o json 2>$null
    if (-not $raw) {
        Write-Host " SKIP (no configmap)"
        continue
    }
    $json = $raw | ConvertFrom-Json
    $yaml = $json.data.'login.yaml'

    # Check if correctly patched (exactly one Middlewares block per RPC)
    $breakerCount = ([regex]::Matches($yaml, 'Breaker: false')).Count
    if ($breakerCount -eq 2) {
        Write-Host " already patched correctly"
        continue
    }

    # Remove any existing (possibly duplicated) Middlewares blocks first
    $yaml = $yaml -replace '(?m)\n  Middlewares:\n    Breaker: false', ''

    # Insert Middlewares block after the Timeout line in each RPC block.
    # The YAML uses 2-space indent under the RPC key.
    $yaml = $yaml -replace '(Key: playerlocator\.rpc\n  Timeout: 5000)', "`$1`n  Middlewares:`n    Breaker: false"
    $yaml = $yaml -replace '(Key: scenemanagerservice\.rpc\n  Timeout: 5000)', "`$1`n  Middlewares:`n    Breaker: false"

    $tmpFile = [System.IO.Path]::Combine($env:TEMP, "login_cm_z$z.yaml")
    [System.IO.File]::WriteAllText($tmpFile, $yaml)

    $out = kubectl create configmap go-svc-login-config --from-file="login.yaml=$tmpFile" -n $ns --dry-run=client -o yaml 2>&1 | kubectl apply -f - -n $ns 2>&1
    Write-Host " $out"

    Remove-Item $tmpFile -ErrorAction SilentlyContinue
}

Write-Host "`nDone. Restart login pods to pick up changes:"
Write-Host "  1..10 | % { kubectl rollout restart deployment login -n mmorpg-zone-zone-`$_ }"
