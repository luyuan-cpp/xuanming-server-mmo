#requires -Version 7
# 全球SQL事务域和本机最小权限证明注入的行为回归，不启动服务。
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/account_directory_dev_secret.ps1"
$taskTokens = $null
$taskErrors = $null
$taskAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../k8s_deploy.ps1'),[ref]$taskTokens,[ref]$taskErrors)
if ($taskErrors.Count) { throw '部署脚本解析失败' }
foreach ($taskName in @('Get-GlobalAuthoritySqlSettings','Get-FriendSqlSettings','Get-RoleProofStorageIds')) {
    $taskDefinition = $taskAst.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $taskName },$true)
    if ($null -eq $taskDefinition) { throw "缺少部署权威函数: $taskName" }
    Set-Item -Path "Function:script:$taskName" -Value $taskDefinition.Body.GetScriptBlock()
}
Test-Case '指定单一TiDB入口时全部旧别名归一，开发缺省只用于本地' {
    $value = Get-GlobalAuthoritySqlSettings -Profile prod -CanonicalHost 'tidb.internal:4000'
    Assert-Equal -Expected 'tidb.internal:4000' -Actual $value.Host -Because '正式域必须有唯一SQL入口'
    Assert-Equal -Expected 'true' -Actual $value.RequireTiDB -Because '显式入口必须要求后端TiDB探测'
    $alias = Get-GlobalAuthoritySqlSettings -Profile prod -FriendHost 'tidb.internal:4000' -MatchHost 'tidb.internal:4000'
    Assert-Equal -Expected $value.Host -Actual $alias.Host -Because '既有一致别名可以迁移到唯一入口'
    $dev = Get-GlobalAuthoritySqlSettings -Profile dev -DevelopmentHost 'mysql.local:3306'
    Assert-Equal -Expected 'false' -Actual $dev.RequireTiDB -Because '开发环境显式保留本机MySQL夹具'
}
Test-Case '独立SQL入口、生产缺省和带注入内容的地址全部拒绝' {
    foreach ($arguments in @(
        @{ Profile='prod' },
        @{ Profile='dev'; CanonicalHost='tidb.internal:4000'; TradeHost='other.internal:4000' },
        @{ Profile='prod'; CanonicalHost="tidb.internal:4000`nInjected: true" },
        @{ Profile='prod'; CanonicalHost='user:password@tidb.internal:4000' }
    )) {
        $rejected = $false
        try { $null = Get-GlobalAuthoritySqlSettings @arguments } catch { $rejected = $true }
        Assert-True -Condition $rejected -Because '事务目录与资产不能配置成不同SQL集群或注入清单内容'
    }
}
Test-Case '本机证明跨进程复用，match只取得scene验签能力' {
    $taskRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-authority-secret-tests-'+[Guid]::NewGuid().ToString('N'))
    $taskSaved = Backup-AccountDirectoryDevSecrets -Service 'data-service'
    try {
        foreach ($key in $taskSaved.Keys) { [Environment]::SetEnvironmentVariable($key,$null,'Process') }
        Initialize-AccountDirectoryDevSecrets -RepoRoot $taskRoot -Service match
        $scene = [Environment]::GetEnvironmentVariable('MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE','Process')
        Assert-True -Condition ($scene.Length -ge 32) -Because 'scene证明需要随机充分长度'
        Assert-True -Condition ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable('MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE','Process'))) -Because 'match不得取得trade签名能力'
        Initialize-AccountDirectoryDevSecrets -RepoRoot $taskRoot -Service 'data-service'
        Assert-True -Condition ($scene -ceq [Environment]::GetEnvironmentVariable('MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE','Process')) -Because '同一本机证明文件不能在后起进程轮换scene密钥'
        [Environment]::SetEnvironmentVariable('MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE',("x"*32)+"`nextra",'Process')
        $rejected = $false
        try { Initialize-AccountDirectoryDevSecrets -RepoRoot $taskRoot -Service match } catch { $rejected = $true }
        Assert-True -Condition $rejected -Because '显式环境值的换行不能写入证明文件'
    } finally {
        Restore-AccountDirectoryDevSecrets $taskSaved
        $resolved = [IO.Path]::GetFullPath($taskRoot)
        $allowed = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/')+[IO.Path]::DirectorySeparatorChar
        if (-not $resolved.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolved) -notlike 'mmorpg-authority-secret-tests-*') { throw '临时证明目录超出范围' }
        if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
    }
}
Test-Case 'scene-manager只继承独立Lookup密钥，不继承目录写与角色准入能力' {
    $saved = Backup-AccountDirectoryDevSecrets -Service 'scene-manager'
    try {
        [Environment]::SetEnvironmentVariable('MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER',('r'*32),'Process')
        foreach ($caller in @('LOGIN','TRADE','SCENE')) { [Environment]::SetEnvironmentVariable("MMORPG_ACCOUNT_DIRECTORY_SECRET_$caller",('x'*32),'Process') }
        Initialize-AccountDirectoryDevSecrets -RepoRoot ([IO.Path]::GetTempPath()) -Service 'scene-manager'
        Assert-Equal -Expected 'MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER' -Actual ((Get-AccountDirectoryDevSecretNames -Service 'scene-manager') -join ',') -Because 'SM只取得Lookup独立能力'
        foreach ($caller in @('LOGIN','TRADE','SCENE')) {
            Assert-True -Condition ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable("MMORPG_ACCOUNT_DIRECTORY_SECRET_$caller",'Process'))) -Because 'SM子进程不得继承其它caller密钥'
        }
    } finally { Restore-AccountDirectoryDevSecrets $saved }
}
Test-Case 'SQL角色证明从独立正规玩家库清单反解，拒绝畸形/重复/超界' {
    Assert-Equal -Expected '101,102,1000000' -Actual ((Get-RoleProofStorageIds -DatabaseNames 'zone_102_db,player_store_1000000_db,zone_101_db') -join ',') -Because '反解正规storage并按unsigned排序'
    foreach ($value in @('', 'zone_0_db', 'zone_01_db', 'mmorpg_global', 'player_store_999999_db', 'player_store_4294967296_db', 'zone_1_db,zone_1_db', (('zone_1_db,'*1024)+'zone_2_db'))) {
        $denied=$false;try{$null=Get-RoleProofStorageIds -DatabaseNames $value}catch{$denied=$true}
        Assert-True -Condition $denied -Because '不能从畸形/重复/超过1024的玩家存储清单授权跨schema强读'
    }
}
Test-Case 'runtime唯一权限叶不再由initdb共享账号ALL旁路' {
    $root=Join-Path $PSScriptRoot '../../..'
    $init=Get-Content -LiteralPath (Join-Path $root 'deploy/mysql-init/00_init_zone_dbs.sql') -Raw
    $statements=($init -split "\r?\n" | Where-Object {$_ -notmatch '^\s*--'}) -join "\n"
    Assert-True -Condition ($statements -notmatch '(?i)GRANT\s+ALL|GRANT\s+.+\.\*.+appuser') -Because '只建库，不能从第二权限叶给runtime全schema'
    $runtime=Get-Content -LiteralPath (Join-Path $root 'deploy/sql/authority_runtime_role.sql') -Raw
    Assert-Match -Text $runtime -Pattern 'writer_lifecycle_receipt' -Because 'runtime按永久原REGISTER核boot部署/锁资格'
    Assert-Match -Text $runtime -Pattern 'LOCK_TOUCH_GRANT' -Because '受限锁写只由正式已资格REGISTER选择，不自行升级表权限'
}

Test-Case 'Java authority镜像带R根proto与固定Linux生成器输入，缺原件不得启动构建' {
    $tokens=$null;$parseErrors=$null
    $imageAst=[Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../java_svc_image.ps1'),[ref]$tokens,[ref]$parseErrors)
    if($parseErrors.Count){throw 'Java镜像脚本解析失败'}
    foreach($name in @('Get-ImageFullName','Invoke-Build')){
        $definition=$imageAst.Find({param($node)$node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name},$true)
        Set-Item -Path "Function:script:$name" -Value $definition.Body.GetScriptBlock()
    }
    $root=Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-java-image-input-'+[Guid]::NewGuid().ToString('N'))
    $null=[IO.Directory]::CreateDirectory($root)
    try {
        $RepoRoot=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'));$JavaRoot=Join-Path $RepoRoot 'java';$Dockerfile=Join-Path $RepoRoot 'deploy/k8s/Dockerfile.java-svc'
        $Catalogue=[ordered]@{gateway=@{Dir='gateway_node';ImageName='fixture-gateway'}};$Registry='fixture.invalid';$Tag='fixture';$DryRun=$true
        $BuildVersion='fixture';$BuildCommit='fixture';$BuildTime='fixture';$LinuxProtocDir='';$LinuxProtocSha256=''
        $message='';try{Invoke-Build}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '明确已批准LinuxProtocDir' -Because '缺生成器不能静默下载或继续构建错误descriptor'
        # 只有格式校验与DryRun夹具；四字节不是可运行protoc，更不构成生成器资格证据。
        $LinuxProtocDir=$root;$tool=Join-Path $root 'protoc';[IO.File]::WriteAllBytes($tool,[byte[]]@(127,69,76,70))
        $LinuxProtocSha256=(Get-FileHash -LiteralPath $tool -Algorithm SHA256).Hash.ToLowerInvariant()
        $output=(Invoke-Build 6>&1 | Out-String)
        Assert-Match -Text $output -Pattern ([regex]::Escape('--build-context qualified-protoc='+$root)) -Because 'Docker接收独立明确的正规tool输入'
        Assert-Match -Text $output -Pattern ([regex]::Escape('LINUX_PROTOC_SHA256='+$LinuxProtocSha256)) -Because '容器内重新核同一SHA防读写间变化'
        Assert-Match -Text $output -Pattern ([regex]::Escape($RepoRoot)) -Because '根proto与java reactor均可见'
        $LinuxProtocSha256='0'*64;$message='';try{Invoke-Build}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern 'SHA不符' -Because '不同生成器不能借相同路径替换'
    } finally {
        $full=[IO.Path]::GetFullPath($root);$allowed=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/')+[IO.Path]::DirectorySeparatorChar
        if(-not $full.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $full) -notlike 'mmorpg-java-image-input-*'){throw '镜像夹具清理范围非法'}
        Remove-Item -LiteralPath $full -Recurse -Force
    }
}

exit (Complete-TestRun -SuiteName '全球SQL与本机证明注入')
