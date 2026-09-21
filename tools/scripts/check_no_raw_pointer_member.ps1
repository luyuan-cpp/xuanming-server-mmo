param(
    [Parameter(Mandatory = $true)]
    [string]$ProjectDir,

    [Parameter(Mandatory = $false)]
    [string]$ProjectName = "unknown-project"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repoRoot = (Resolve-Path "$PSScriptRoot\..\..").Path

# ==================================================================
# Phase 1 – locate a checker engine
#   Priority: standalone no_raw_ptr_check.exe > clang-query
# ==================================================================
$useStandalone = $false
$toolExe = $null

# Try standalone tool (built from cpp/plugin)
foreach ($cfg in 'Release', 'Debug', 'RelWithDebInfo', 'MinSizeRel') {
    $candidate = Join-Path $repoRoot "cpp\plugin\build\$cfg\no_raw_ptr_check.exe"
    if (Test-Path $candidate) { $toolExe = $candidate; $useStandalone = $true; break }
}
if (-not $toolExe) {
    $candidate = Join-Path $repoRoot "cpp\plugin\build\no_raw_ptr_check.exe"
    if (Test-Path $candidate) { $toolExe = $candidate; $useStandalone = $true }
}

# Fall back to clang-query
$clangQuery = $null
if (-not $useStandalone) {
    $cq = Get-Command clang-query -ErrorAction SilentlyContinue
    if ($cq) { $clangQuery = $cq.Source }
    if (-not $clangQuery) {
        $candidate = "C:\Program Files\LLVM\bin\clang-query.exe"
        if (Test-Path $candidate) { $clangQuery = $candidate }
    }
}

if (-not $useStandalone -and -not $clangQuery) {
    Write-Host "[no-raw-pointer-member] SKIP ($ProjectName): neither no_raw_ptr_check.exe nor clang-query found."
    exit 0
}

if (-not (Test-Path -LiteralPath $ProjectDir -PathType Container)) {
    Write-Error "ProjectDir does not exist: $ProjectDir"
    exit 1
}

# ==================================================================
# Phase 2 – collect source files & check cache
# ==================================================================
$sourceFiles = @(
    Get-ChildItem -LiteralPath $ProjectDir -Recurse -File -Include *.h,*.hpp,*.hh,*.hxx,*.cpp,*.cc,*.cxx |
        Where-Object {
            $_.FullName -notmatch "\\(third_party|generated|bin|x64|build|\.vs)\\"
        }
)

if (-not $sourceFiles) {
    Write-Host "[no-raw-pointer-member] No source files in $ProjectDir ($ProjectName)"
    exit 0
}

# Timestamp cache: skip if no file changed since last successful check
$markerFile = Join-Path $ProjectDir ".no_raw_ptr_check_ok"
if (Test-Path $markerFile) {
    $markerTime = (Get-Item $markerFile).LastWriteTime
    $changed = @($sourceFiles | Where-Object { $_.LastWriteTime -gt $markerTime })
    if (-not $changed) {
        Write-Host "[no-raw-pointer-member] PASSED (cached) for '$ProjectName'"
        exit 0
    }
}

# ==================================================================
# Phase 3 – common include-path arguments
#   -I         → node & services logic code (checked for violations)
#   -isystem   → engine infra / third_party / generated (treated as system → skipped)
# ==================================================================
$srcIncludes = @(
    "$repoRoot\cpp\libs\services\scene"
    "$repoRoot\cpp\libs\services\player"
    "$ProjectDir"
)

$sysIncludes = @(
    "$repoRoot\cpp\libs"
    "$repoRoot\cpp\libs\engine"
    "$repoRoot\cpp\libs\engine\core"
    "$repoRoot\cpp\libs\engine\infra"
    "$repoRoot\cpp\libs\engine\modules"
    "$repoRoot\cpp\libs\engine\config"
    "$repoRoot\cpp\generated"
    "$repoRoot\third_party"
    "$repoRoot\third_party\boost"
    "$repoRoot\third_party\grpc\include"
    "$repoRoot\third_party\grpc\third_party\protobuf\src"
    "$repoRoot\third_party\grpc\third_party\abseil-cpp"
    "$repoRoot\third_party\spdlog\include"
    "$repoRoot\third_party\muduo"
    "$repoRoot\third_party\muduo\contrib\windows\include"
    "$repoRoot\third_party\xxhash"
    "$repoRoot\third_party\librdkafka\src-cpp"
    "$repoRoot\third_party\redis"
    "$repoRoot\third_party\redis\deps"
    "$repoRoot\third_party\zlib"
    "$repoRoot\third_party\entt\single_include"
    "$repoRoot\third_party\sol2\include"
    "$repoRoot\third_party\lua"
    "$repoRoot\third_party\yaml-cpp\include"
    "$repoRoot\third_party\fmt\include"
)

# ==================================================================
# Phase 4 – run the check
# ==================================================================
Write-Host "[no-raw-pointer-member] Checking $($sourceFiles.Count) file(s) in '$ProjectName' ..."

function Invoke-StandaloneTool {
    $respFile = [System.IO.Path]::GetTempFileName()
    try {
        $lines = [System.Collections.Generic.List[string]]::new()
        foreach ($f in $sourceFiles) { $lines.Add($f.FullName) }

        $lines.Add('--skip-path=third_party')
        $lines.Add('--skip-path=generated')
        $lines.Add('--skip-path=\build\')
        $lines.Add('--skip-path=\.vs\')

        if ($env:INCLUDE) {
            foreach ($d in $env:INCLUDE.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)) {
                if (Test-Path $d) { $lines.Add("--extra-arg=-isystem$d") }
            }
        }
        foreach ($inc in $srcIncludes) {
            if (Test-Path $inc) { $lines.Add("--extra-arg=-I$inc") }
        }
        foreach ($inc in $sysIncludes) {
            if (Test-Path $inc) { $lines.Add("--extra-arg=-isystem$inc") }
        }

        # LLVM 按 Windows 命令行规则拆响应文件；路径中的空格和末尾反斜杠必须保留。
        $quotedLines = @($lines | ForEach-Object {
            '"' + ($_ -replace '(\\*)"', '$1$1\"' -replace '(\\+)$', '$1$1') + '"'
        })
        [System.IO.File]::WriteAllLines($respFile, $quotedLines)
        # 函数只返回退出码，不能把工具 stdout 混进 $ec 变成数组。
        & $toolExe "@$respFile" | ForEach-Object { Write-Host $_ }
        return $LASTEXITCODE
    }
    finally {
        Remove-Item -LiteralPath $respFile -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-ClangQuery {
    $queryScript = Join-Path $repoRoot "cpp\plugin\no_raw_ptr_matcher.cq"
    if (-not (Test-Path $queryScript)) {
        Write-Host "[no-raw-pointer-member] SKIP: query script not found at $queryScript"
        return 0
    }

    # Pick the entry-point .cpp file (main.cpp) for scanning — it transitively
    # includes the vast majority of project headers, giving good coverage in a
    # single parse. Full-project scanning of every .cpp can be done in CI via
    # the standalone tool (cpp/plugin/no_raw_ptr_check.exe).
    $cppFiles = @($sourceFiles | Where-Object { $_.Extension -in '.cpp','.cc','.cxx' })
    $mainCpp = $cppFiles | Where-Object { $_.Name -eq 'main.cpp' } | Select-Object -First 1
    if (-not $mainCpp) {
        $mainCpp = $cppFiles | Select-Object -First 1
    }
    if (-not $mainCpp) {
        Write-Host "[no-raw-pointer-member] No .cpp files to scan."
        return 0
    }

    $cqArgs = [System.Collections.Generic.List[string]]::new()
    $cqArgs.Add("-f")
    $cqArgs.Add($queryScript)
    $cqArgs.Add($mainCpp.FullName)

    # Separator — everything after is a compiler flag
    $cqArgs.Add("--")
    $cqArgs.Add("-xc++")
    $cqArgs.Add("-std=c++20")
    $cqArgs.Add("-fsyntax-only")
    $cqArgs.Add("-fms-extensions")
    $cqArgs.Add("-fms-compatibility")
    $cqArgs.Add("-w")
    $cqArgs.Add("-Wno-everything")
    $cqArgs.Add("-ferror-limit=0")

    # MSVC system include dirs
    if ($env:INCLUDE) {
        foreach ($d in $env:INCLUDE.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)) {
            if (Test-Path $d) { $cqArgs.Add("-isystem$d") }
        }
    }
    foreach ($inc in $srcIncludes) {
        if (Test-Path $inc) { $cqArgs.Add("-I$inc") }
    }
    foreach ($inc in $sysIncludes) {
        if (Test-Path $inc) { $cqArgs.Add("-isystem$inc") }
    }

    Write-Host "[no-raw-pointer-member] Scanning $($mainCpp.Name) with clang-query ..."
    $argArray = $cqArgs.ToArray()
    $output = & $clangQuery @argArray 2>$null | Out-String

    # Parse diag output: collect unique file:line violations
    $skipPatterns = @('third_party', 'generated', '\build\', '\.vs\')
    $seen = [System.Collections.Generic.HashSet[string]]::new()
    $violations = [System.Collections.Generic.List[string]]::new()

    foreach ($line in $output -split "`n") {
        $trimmed = $line.Trim()
        if ($trimmed -match '^(.+):(\d+):\d+:\s+note:\s+"root"\s+binds here') {
            $filePath = $Matches[1]
            $lineNum  = $Matches[2]

            $skip = $false
            foreach ($pat in $skipPatterns) {
                if ($filePath -match [regex]::Escape($pat)) { $skip = $true; break }
            }
            $key = "${filePath}:${lineNum}"
            if (-not $skip -and $seen.Add($key)) {
                $violations.Add("  $key")
            }
        }
    }

    if ($violations.Count -gt 0) {
        Write-Host "[no-raw-pointer-member] Raw pointer member variables are prohibited:"
        foreach ($v in $violations) { Write-Host $v }
        Write-Host ""
        Write-Host "Found $($violations.Count) unique violation(s)."
        return 1
    }
    return 0
}

if ($useStandalone) {
    $ec = Invoke-StandaloneTool
} else {
    $ec = Invoke-ClangQuery
}

# ==================================================================
# Phase 5 – cache result on success
# ==================================================================
if ($ec -eq 0) {
    [System.IO.File]::WriteAllText($markerFile, (Get-Date -Format o))
    Write-Host "[no-raw-pointer-member] PASSED for '$ProjectName'."
} else {
    # Remove stale marker so next build re-checks
    if (Test-Path $markerFile) { Remove-Item $markerFile -Force -ErrorAction SilentlyContinue }
    Write-Host "[no-raw-pointer-member] FAILED for '$ProjectName'."
}

exit $ec
