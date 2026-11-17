#requires -Version 7
<#
.SYNOPSIS
    用白名单参数调用 Java 仓库现有 Maven Wrapper，保留真实退出码。
.DESCRIPTION
    只接受已登记的检查 ID，不接受任意命令文本。仅设置当前进程 JAVA_HOME。
    离线模式同时要求 wrapper 发行包已缓存，避免 mvnw 在 Maven 启动前联网下载。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidateSet('java-gameday')][string]$CheckId,
    [Parameter(Mandatory = $true)][string]$Repository,
    [Parameter(Mandatory = $true)][string]$RunDir,
    [Parameter(Mandatory = $true)][string]$JdkRoot
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

function Get-SafeFullPath {
    param([Parameter(Mandatory = $true)][string]$Value)
    # mvnw.cmd 会进入 cmd.exe，wrapper 还会将自身路径放进单引号 PowerShell 字符串。
    # 参数数组不能消除这两层已有脚本的特殊字符解释，故路径显式拒绝这些字符。
    if ($Value -match '[\r\n"''%!?&|<>^()]') {
        throw 'A Maven adapter path contains unsupported shell metacharacters.'
    }
    return [IO.Path]::GetFullPath($Value)
}

$previousJavaHome = [Environment]::GetEnvironmentVariable('JAVA_HOME', 'Process')
$pushedLocation = $false
$exitCode = 2
try {
    $repoPath = Get-SafeFullPath $Repository
    $runPath = Get-SafeFullPath $RunDir
    $jdkPath = Get-SafeFullPath $JdkRoot
    $wrapperPath = Join-Path $repoPath 'mvnw.cmd'
    $propertiesPath = Join-Path $repoPath '.mvn/wrapper/maven-wrapper.properties'
    $javaPath = Join-Path $jdkPath 'bin/java.exe'
    foreach ($requiredFile in @($wrapperPath, $propertiesPath, $javaPath, (Join-Path $repoPath 'pom.xml'))) {
        if (-not (Test-Path -LiteralPath $requiredFile -PathType Leaf)) {
            throw "Required Maven adapter input does not exist: $requiredFile"
        }
    }
    if (-not (Test-Path -LiteralPath $runPath -PathType Container)) {
        throw 'The runner must create a fresh run directory before invoking this adapter.'
    }
    $reportsPath = Join-Path $runPath 'java-tests'
    if (Test-Path -LiteralPath $reportsPath) {
        throw 'The Java report directory already exists; refusing to reuse previous test evidence.'
    }
    $reportSuffix = 'ai-' + [Guid]::NewGuid().ToString('N')
    $reportName = "TEST-com.game.common.time.GameDayTest-$reportSuffix.xml"
    $sourceReport = Join-Path $repoPath "game-common/target/surefire-reports/$reportName"
    if (Test-Path -LiteralPath $sourceReport) { throw 'The unique test report already exists.' }

    $wrapperProperties = Get-Content -LiteralPath $propertiesPath -Raw | ConvertFrom-StringData
    $distributionUrl = [string]$wrapperProperties.distributionUrl
    if ($wrapperProperties.distributionType -ne 'only-script' -or $distributionUrl -notmatch '/apache-maven-[0-9.]+-bin\.zip$') {
        throw 'This adapter requires the reviewed only-script Apache Maven wrapper layout.'
    }
    if ($env:MVNW_REPOURL) {
        throw 'MVNW_REPOURL overrides are not supported by the offline wrapper cache check.'
    }
    $distributionName = ($distributionUrl -replace '^.*/', '') -replace '-bin\.zip$', ''
    $cacheBase = Join-Path ([Environment]::GetFolderPath('UserProfile')) '.m2'
    if ($env:MAVEN_USER_HOME) { $cacheBase = Get-SafeFullPath $env:MAVEN_USER_HOME }
    $md5 = [Security.Cryptography.MD5]::Create()
    try {
        $urlHash = ($md5.ComputeHash([byte[]][char[]]$distributionUrl) | ForEach-Object { $_.ToString('x2') }) -join ''
    }
    finally { $md5.Dispose() }
    $cachedMaven = Join-Path $cacheBase "wrapper/dists/$distributionName/$urlHash/bin/mvn.cmd"
    if (-not (Test-Path -LiteralPath $cachedMaven -PathType Leaf)) {
        throw 'The Maven Wrapper distribution is not cached; offline verification cannot download it.'
    }

    [Environment]::SetEnvironmentVariable('JAVA_HOME', $jdkPath, 'Process')
    Push-Location -LiteralPath $repoPath
    $pushedLocation = $true
    $mavenArguments = @(
        '-B', '-ntp', '-o', '-pl', 'game-common', '-am',
        '-Dtest=GameDayTest', '-Dsurefire.failIfNoSpecifiedTests=false',
        "-Dsurefire.reportNameSuffix=$reportSuffix", 'clean', 'test'
    )
    # reportsDirectory 没有命令行 user property；用官方支持的唯一文件后缀绑定本轮。
    # clean 限定于所选 reactor，防止删除/改名后的旧测试 class 冒充本次执行。
    $startedUtc = [DateTime]::UtcNow
    Write-Output 'Running the registered Java GameDay tests with offline Maven and fresh reports.'
    & $wrapperPath @mavenArguments
    $exitCode = $LASTEXITCODE
    if ($null -eq $exitCode) { $exitCode = 2 }
    if (Test-Path -LiteralPath $sourceReport -PathType Leaf) {
        $sourceInfo = Get-Item -LiteralPath $sourceReport
        if ($sourceInfo.LastWriteTimeUtc -lt $startedUtc) { throw 'The unique Java report is stale.' }
        $null = New-Item -ItemType Directory -Path $reportsPath -ErrorAction Stop
        $destination = Join-Path $reportsPath 'TEST-com.game.common.time.GameDayTest.xml'
        Copy-Item -LiteralPath $sourceReport -Destination $destination -ErrorAction Stop
        [ordered]@{
            reportSuffix = $reportSuffix; source = $sourceReport; startedUtc = $startedUtc.ToString('o')
            sourceModifiedUtc = $sourceInfo.LastWriteTimeUtc.ToString('o')
            sha256 = (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant()
            mavenExitCode = $exitCode
        } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $reportsPath 'receipt.json') -Encoding utf8
    } elseif ($exitCode -eq 0) {
        throw 'Maven returned success without the current GameDay test report.'
    }
}
catch {
    Write-Error -Message $_.Exception.Message -ErrorAction Continue
    $exitCode = 2
}
finally {
    if ($pushedLocation) { Pop-Location }
    [Environment]::SetEnvironmentVariable('JAVA_HOME', $previousJavaHome, 'Process')
}
exit $exitCode
