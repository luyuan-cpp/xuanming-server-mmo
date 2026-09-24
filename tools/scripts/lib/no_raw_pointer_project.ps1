# 由 check_no_raw_pointer_member.ps1 装入；不修改源码或编译工程。
function Get-ClangQueryExitCode {
    param([string]$OutputText, [int]$ProcessExitCode)
    # clang-query 在头文件解析失败时也可能退出 0 并打印 0 matches，必须拒绝。
    if ($ProcessExitCode -ne 0 -or $OutputText -match '(?m)(?:^|:\s+)(?:fatal )?error:') { return 2 }
    if ($OutputText -match '"root" binds here') { return 1 }
    return 0
}

function Invoke-ProjectRawPointerCheck {
    if (-not $MSBuildPath -or -not (Test-Path -LiteralPath $MSBuildPath)) {
        throw '工程检查需要当前构建所用的 MSBuildPath。'
    }
    $metadataText = & $MSBuildPath $ProjectFile "/p:Configuration=$Configuration" "/p:Platform=$Platform" '-getItem:ClCompile' '-getProperty:IncludePath'
    if ($LASTEXITCODE -ne 0) { throw '读取 MSBuild 编译参数失败，未执行检查。' }
    $metadata = ($metadataText -join "`n") | ConvertFrom-Json -AsHashtable
    $items = @($metadata.Items.ClCompile | Where-Object {
        $_['ExcludedFromBuild'] -ne 'true' -and $_.FullPath -notmatch $thirdPartyPathPattern -and
        $_.FullPath -notmatch '[\\/](generated|bin|x64|build|\.vs)[\\/]'
    })
    if (-not $items.Count) {
        Write-Host "[no-raw-pointer-member] No compiled translation units in '$ProjectName'."
        return 0
    }

    # 缓存包含工程求值结果、工具与脚本，以及仓内源码和生成头的时间戳。
    # 全仓头依赖参与失效，防止改共享业务头后沿用旧工程通过记录。
    $markerFile = Join-Path $ProjectDir '.no_raw_ptr_check_ok'
    $inputs = @(Get-ChildItem (Join-Path $repoRoot 'cpp') -Recurse -File -Include *.h,*.hpp,*.hh,*.hxx,*.cpp,*.cc,*.cxx,*.vcxproj,*.props |
        Where-Object { $_.FullName -notmatch '\\(build|bin|x64|\.vs)\\' } |
        Sort-Object FullName | ForEach-Object { "$($_.FullName)|$($_.LastWriteTimeUtc.Ticks)|$($_.Length)" })
    $inputs += @(Get-ChildItem -LiteralPath $ProjectDir -Recurse -File -Include *.h,*.hpp,*.hh,*.hxx,*.cpp,*.cc,*.cxx |
        Sort-Object FullName | ForEach-Object { "$($_.FullName)|$($_.LastWriteTimeUtc.Ticks)|$($_.Length)" })
    foreach ($path in @($PSCommandPath, (Join-Path $PSScriptRoot '../check_no_raw_pointer_member.ps1'), (Join-Path $repoRoot 'cpp/plugin/no_raw_ptr_matcher.cq'), $toolExe, $clangQuery)) {
        if ($path -and (Test-Path -LiteralPath $path)) {
            $item = Get-Item -LiteralPath $path
            $inputs += "$($item.FullName)|$($item.LastWriteTimeUtc.Ticks)|$($item.Length)"
        }
    }
    $hashText = ($metadataText -join "`n") + ($inputs -join "`n")
    $hash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($hashText)))
    $cachedHash = $null
    if (Test-Path -LiteralPath $markerFile) {
        try { $cachedHash = (Get-Content -LiteralPath $markerFile -Raw | ConvertFrom-Json).hash }
        catch { $cachedHash = $null } # 旧版只存日期，不代表当前工程输入已检查。
    }
    if ($cachedHash -eq $hash) {
        Write-Host "[no-raw-pointer-member] PASSED (compiled inputs cached) for '$ProjectName'."
        return 0
    }
    if (Test-Path -LiteralPath $markerFile) { Remove-Item -LiteralPath $markerFile }

    $groups = @{}
    foreach ($item in $items) {
        $args = [Collections.Generic.List[string]]::new()
        $args.Add('-std=c++23')
        # 每个翻译单元自己的定义、包含路径和强制头，不能拼成多个项目的并集。
        foreach ($definition in ($item['PreprocessorDefinitions'] -split ';')) {
            if ($definition) { $args.Add("-D$definition") }
        }
        foreach ($definition in ($item['UndefinePreprocessorDefinitions'] -split ';')) {
            if ($definition) { $args.Add("-U$definition") }
        }
        foreach ($include in ($item['AdditionalIncludeDirectories'] -split ';')) {
            if (-not $include) { continue }
            $absolute = [IO.Path]::GetFullPath($include, $ProjectDir)
            if ($absolute -match $thirdPartyPathPattern -or $absolute -match '[\\/]generated([\\/]|$)') { $args.Add("-isystem$absolute") }
            else { $args.Add("-I$absolute") }
        }
        # CMake 的 SYSTEM 目录通过 /external:I 写入 AdditionalOptions，不能丢掉，
        # 否则 LLVM 等第三方头在真正编译前就会被检查器误报为找不到。
        foreach ($external in [regex]::Matches([string]$item['AdditionalOptions'], '(?:^|\s)[/-]external:I\s*(?:"([^"]+)"|(\S+))')) {
            $include = if ($external.Groups[1].Success) { $external.Groups[1].Value } else { $external.Groups[2].Value }
            $args.Add('-isystem' + [IO.Path]::GetFullPath($include, $ProjectDir))
        }
        foreach ($include in ($metadata.Properties.IncludePath -split ';')) {
            if ($include) { $args.Add("-isystem$include") }
        }
        foreach ($include in ($item['ForcedIncludeFiles'] -split ';')) {
            if ($include) { $args.Add('-include'); $args.Add($include) }
        }
        $key = $args -join "`n"
        if (-not $groups.ContainsKey($key)) {
            $groups[$key] = @{ Arguments=$args; Sources=[Collections.Generic.List[string]]::new() }
        }
        $groups[$key].Sources.Add($item.FullPath)
    }
    Write-Host "[no-raw-pointer-member] Checking $($items.Count) compiled translation units / $($groups.Count) configuration groups in '$ProjectName' ..."
    Push-Location -LiteralPath $ProjectDir
    try {
        foreach ($group in $groups.Values) {
            if ($useStandalone) {
                $responsePath = [IO.Path]::GetTempFileName()
                try {
                    $arguments = @($group.Sources) + @('--skip-path=generated', '--skip-path=\build\', '--skip-path=\.vs\')
                    $arguments += @($group.Arguments | ForEach-Object { "--extra-arg=$_" })
                    $quoted = @($arguments | ForEach-Object { '"' + ($_ -replace '(\\*)"', '$1$1\"' -replace '(\\+)$', '$1$1') + '"' })
                    [IO.File]::WriteAllLines($responsePath, $quoted)
                    & $toolExe "@$responsePath" | ForEach-Object { Write-Host $_ }
                    $code = $LASTEXITCODE
                } finally { Remove-Item -LiteralPath $responsePath -Force -ErrorAction SilentlyContinue }
            } else {
                $query = Join-Path $repoRoot 'cpp/plugin/no_raw_ptr_matcher.cq'
                $arguments = @('-f', $query) + @($group.Sources) + @('--', '-xc++', '-fsyntax-only', '-fms-extensions', '-fms-compatibility', '-Wno-everything') + @($group.Arguments)
                $result = & $clangQuery @arguments 2>&1
                $code = Get-ClangQueryExitCode -OutputText ($result -join "`n") -ProcessExitCode $LASTEXITCODE
                $result | ForEach-Object { Write-Host $_ }
            }
            if ($code -ne 0) {
                Write-Host "[no-raw-pointer-member] FAILED for '$ProjectName' (exit $code)."
                return $code
            }
        }
    } finally { Pop-Location }
    @{hash=$hash; project=$ProjectFile; configuration=$Configuration; platform=$Platform; translation_units=$items.Count; checked_at=[DateTime]::UtcNow.ToString('o')} |
        ConvertTo-Json | Set-Content -LiteralPath $markerFile -Encoding utf8
    Write-Host "[no-raw-pointer-member] PASSED for '$ProjectName'."
    return 0
}
