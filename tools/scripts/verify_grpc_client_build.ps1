param(
    [ValidateSet('Debug', 'Release')][string]$Configuration = 'Debug',
    [ValidateSet('x64', 'Win32')][string]$Platform = 'x64'
)

$ErrorActionPreference = 'Stop'

$pf86 = [Environment]::GetFolderPath("ProgramFilesX86")
$vswhere = Join-Path $pf86 "Microsoft Visual Studio/Installer/vswhere.exe"
if (!(Test-Path $vswhere)) {
    throw "vswhere.exe not found: $vswhere"
}

$msbuild = & $vswhere -latest -products * -requires Microsoft.Component.MSBuild -find "MSBuild/**/Bin/MSBuild.exe" | Select-Object -First 1
if (-not $msbuild) {
    throw "MSBuild.exe not found via vswhere"
}

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
Push-Location -LiteralPath $repoRoot
try {
    & $msbuild "cpp/generated/grpc_client/grpc_client.vcxproj" /m:1 /nr:false /p:Configuration=$Configuration /p:Platform=$Platform /nologo /clp:ErrorsOnly
    exit $LASTEXITCODE
} finally {
    Pop-Location
}
