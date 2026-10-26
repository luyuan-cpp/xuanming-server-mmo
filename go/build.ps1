<#
.SYNOPSIS
    Regenerate go-zero wrappers (goctl) for db/login and fix imports for
    the shared proto module (go/proto/).

.DESCRIPTION
    The shared proto module (go/proto/) holds all .pb.go outputs.
    goctl generates server/client wrappers with per-service import paths;
    this script patches those imports to use the shared module.

    For .pb.go regeneration, run the protogen tool:
        pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run
#>
param(
    [switch]$SkipGoctl
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$goRoot = $PSScriptRoot

Push-Location $goRoot
try {
    # ----------------------------------------------------------------
    # 1. goctl: db  (server + client wrappers)
    # ----------------------------------------------------------------
    if (-not $SkipGoctl) {
        Write-Host '>>> [goctl] db' -ForegroundColor Cyan
        Push-Location db
        goctl rpc protoc `
            ../../generated/proto/db/proto/db/db.proto `
            --proto_path=../../generated/proto/db `
            --proto_path=../../third_party/protobuf/src/ `
            --go_out=./proto/ --go-grpc_out=./proto/ `
            --zrpc_out=./ -m
        Pop-Location

        Write-Host '>>> [goctl] login' -ForegroundColor Cyan
        Push-Location login
        goctl rpc protoc `
            ../../generated/proto/login/proto/login/login.proto `
            --proto_path=../../generated/proto/login `
            --proto_path=../../third_party/protobuf/src/ `
            --go_out=./proto/ --go-grpc_out=./proto/ `
            --zrpc_out=./ -m
        Pop-Location
    }

    # ----------------------------------------------------------------
    # 2. Fix imports: per-service → shared proto module
    # ----------------------------------------------------------------
    $importFixes = @{
        'login' = @{
            OldList = @(
                '"login/proto/login/proto/login"',
                '"login/proto/login"'
            )
            New = '"proto/login"'
        }
        'db' = @{
            OldList = @(
                '"db/proto/db/proto/db"',
                '"db/proto/db"'
            )
            New = '"proto/db"'
        }
        'scene_manager' = @{
            OldList = @(
                '"scene_manager/scene_manager"'
            )
            New = '"proto/scene_manager"'
        }
        'data_service' = @{
            OldList = @(
                '"data_service/data_service"'
            )
            New = '"proto/data_service"'
        }
    }

    foreach ($svc in $importFixes.Keys) {
        $oldList = $importFixes[$svc].OldList
        $new = $importFixes[$svc].New
        $svcDir = Join-Path $goRoot $svc

        # Process all .go files OUTSIDE the per-service proto/ dir
        Get-ChildItem $svcDir -Filter '*.go' -Recurse |
            Where-Object { $_.FullName -notmatch '\\proto\\' } |
            ForEach-Object {
                $content = Get-Content $_.FullName -Raw
                $changed = $false
                foreach ($old in $oldList) {
                    if ($content -match [regex]::Escape($old)) {
                        $content = $content.Replace($old, $new)
                        $changed = $true
                    }
                }
                if ($changed) {
                    Set-Content $_.FullName $content -NoNewline
                    Write-Host "  fixed: $($_.FullName.Replace($goRoot + '\', ''))" -ForegroundColor Green
                }
            }
    }

    # ----------------------------------------------------------------
    # 3. Delete per-service .pb.go duplicates (shared module has them)
    #    goctl creates login/proto & db/proto; keep list defensive for
    #    any stale copies left by older pipeline runs.
    # ----------------------------------------------------------------
    Write-Host '>>> Cleaning per-service proto duplicates' -ForegroundColor Cyan
    $dirsToClean = @(
        'login/proto'
        'db/proto'
        'scene_manager/proto'
        'scene_manager/scene_manager'
        'data_service/proto'
        'data_service/data_service'
        'player_locator/proto'
        'player_locator/contracts'
        'login/contracts'
        'friend/proto'
        'guild/proto'
    )

    foreach ($rel in $dirsToClean) {
        $dir = Join-Path $goRoot $rel
        if (Test-Path $dir) {
            Remove-Item $dir -Recurse -Force
            Write-Host "  deleted: $rel/" -ForegroundColor Yellow
        }
    }

    Write-Host '>>> Done.' -ForegroundColor Green
}
finally {
    Pop-Location
}
