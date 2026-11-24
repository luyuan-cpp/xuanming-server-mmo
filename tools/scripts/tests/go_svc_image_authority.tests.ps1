#requires -Version 7
# 只解析正式源码与调用镜像命令桩；不调用Go/Docker/native，不冒构建通过。
$ErrorActionPreference='Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
$source=Join-Path $PSScriptRoot '../go_svc_image.ps1'
$tokens=$null;$parseErrors=$null
$ast=[Management.Automation.Language.Parser]::ParseFile($source,[ref]$tokens,[ref]$parseErrors)
if($parseErrors.Count){throw ($parseErrors | Out-String)}
$assignment=$ast.Find({param($n)$n -is [Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -ceq '$Catalogue'},$false)
if($null -eq $assignment){throw '缺正式镜像Catalogue'}
$Catalogue=& ([scriptblock]::Create($assignment.Right.Extent.Text))
Test-Case '原guild/mail/rank重复键最小修正且每服务只出现一次' {
    foreach($name in @('guild','mail','rank','writer-authority','db-migrate')){
        Assert-Equal -Expected 1 -Actual @($Catalogue.Keys | Where-Object {$_ -ceq $name}).Count -Because '有序目录必须唯一，重复键原脚本应解析失败'
    }
}
Test-Case 'registrar是真实可构建cmd工件，不借业务服务命令分支' {
    $entry=$Catalogue['writer-authority']
    Assert-Equal -Expected 'data_service' -Actual $entry.Dir -Because '复用现data_service正规module'
    Assert-Equal -Expected './cmd/writer_authority' -Actual $entry.Entry -Because '编译完整cmd包，而非main单文件漏源'
    Assert-Equal -Expected 'mmorpg-writer-authority' -Actual $entry.ImageName -Because '部署init有独立镜像工件'
    $root=Join-Path $PSScriptRoot '../../..'
    Assert-True -Condition (Test-Path -LiteralPath (Join-Path $root 'go/data_service/cmd/writer_authority/main.go')) -Because '主命令必须真实存在，目录项不能只作说明'
}
Test-Case 'go/db 的迁移入口是真实可构建cmd工件，名字和部署目录配对' {
    $entry=$Catalogue['db-migrate']
    Assert-Equal -Expected 'db' -Actual $entry.Dir -Because '复用db模块，/app/data 才带表清单'
    Assert-Equal -Expected './cmd/migrate' -Actual $entry.Entry -Because '编译完整cmd包'
    Assert-Equal -Expected 'mmorpg-db-migrate' -Actual $entry.ImageName -Because 'db-migrate Job用的独立镜像'
    $root=Join-Path $PSScriptRoot '../../..'
    Assert-True -Condition (Test-Path -LiteralPath (Join-Path $root 'go/db/cmd/migrate/main.go')) -Because '主命令必须真实存在'
    Assert-Match -Text (Get-Content -LiteralPath (Join-Path $root 'tools/scripts/k8s_deploy.ps1') -Raw) -Pattern 'MigrateImageName = "mmorpg-db-migrate"' -Because '部署目录要配对，否则Job拉不到镜像'
}
Test-Case '镜像构建Interface确实传入registrar module和完整ENTRY' {
    foreach($name in @('Get-ImageFullName','Invoke-BuildAll')){
        $definition=$ast.Find({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true)
        Set-Item -Path "Function:script:$name" -Value $definition.Body.GetScriptBlock()
    }
    $taskRoot=Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-registrar-image-'+[Guid]::NewGuid().ToString('N'))
    $null=[IO.Directory]::CreateDirectory($taskRoot);[IO.File]::WriteAllText((Join-Path $taskRoot 'fixture.json'),'{}')
    try{
        $TablesDir=$taskRoot;$Dockerfile=$source;$GoRoot=Join-Path $PSScriptRoot '../../../go';$Registry='fixture.invalid';$Tag='fixture';$BuildVersion='fixture';$BuildCommit='fixture';$BuildTime='2000-01-01T00:00:00Z';$DockerCommand='Invoke-AuthorityDockerFixture';$DryRun=$false
        $Catalogue=[ordered]@{'writer-authority'=$Catalogue['writer-authority']}
        function Get-ExternalBuildContexts {param([string]$ServiceDir) return @()}
        function Invoke-AuthorityDockerFixture { $script:CapturedDockerArgs=@($args);$global:LASTEXITCODE=0 }
        Invoke-BuildAll
        Assert-True -Condition ($script:CapturedDockerArgs -contains 'SERVICE=data_service') -Because '实际构建参数选择真实module'
        Assert-True -Condition ($script:CapturedDockerArgs -contains 'ENTRY=./cmd/writer_authority') -Because 'ENTRY从正式Catalogue到Docker未丢失'
        Assert-True -Condition ($script:CapturedDockerArgs -contains 'fixture.invalid/mmorpg-writer-authority:fixture') -Because '独立registrar image确实进入命令'
    } finally {
        $full=[IO.Path]::GetFullPath($taskRoot);$allowed=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/')+[IO.Path]::DirectorySeparatorChar
        if(-not $full.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $full) -notlike 'mmorpg-registrar-image-*'){throw '夹具清理路径非法'}
        Remove-Item -LiteralPath $full -Recurse -Force
    }
}
Test-Case 'host预编译模式复用相同alpine运行基镜像并通过prebuilt命名上下文打包' {
    $root=Join-Path $PSScriptRoot '../../..'
    $mainDf=Get-Content -LiteralPath (Join-Path $root 'deploy/k8s/Dockerfile.go-svc') -Raw
    $preDf=Get-Content -LiteralPath (Join-Path $root 'deploy/k8s/Dockerfile.go-svc.prebuilt') -Raw
    $alpineRe=[regex]'FROM\s+(alpine:[^\s]+)'
    $m1=$alpineRe.Match($mainDf); $m2=$alpineRe.Match($preDf)
    Assert-True -Condition ($m1.Success -and $m2.Success -and $m1.Groups[1].Value -ceq $m2.Groups[1].Value) -Because 'prebuilt 运行基镜像 digest 必须与 Dockerfile.go-svc 完全一致'
    Assert-Match -Text $preDf -Pattern 'USER 10001:10001' -Because 'prebuilt 必须保持非 root 10001 运行'
    Assert-Match -Text $preDf -Pattern 'COPY --from=prebuilt /service /app/service' -Because 'prebuilt 必须从命名上下文拷入宿主编译二进制'

    foreach($name in @('Get-ImageFullName','Invoke-BuildAll')){
        $definition=$ast.Find({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true)
        Set-Item -Path "Function:script:$name" -Value $definition.Body.GetScriptBlock()
    }
    $taskRoot=Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-prebuilt-image-'+[Guid]::NewGuid().ToString('N'))
    $null=[IO.Directory]::CreateDirectory($taskRoot);[IO.File]::WriteAllText((Join-Path $taskRoot 'fixture.json'),'{}')
    try{
        $TablesDir=$taskRoot;$Dockerfile=$source;$PrebuiltDockerfile=Join-Path $root 'deploy/k8s/Dockerfile.go-svc.prebuilt';$BuildMode='host';$GoRoot=Join-Path $PSScriptRoot '../../../go';$Registry='fixture.invalid';$Tag='fixture';$BuildVersion='fixture';$BuildCommit='fixture';$BuildTime='2000-01-01T00:00:00Z';$DockerCommand='Invoke-HostModeDockerFixture';$DryRun=$false
        $Catalogue=[ordered]@{'login'=$Catalogue['login']}
        $script:HostCompileCalled=$false
        function Invoke-HostGoCrossCompile { param($ServiceDir,$Entry,$OutputPath) $script:HostCompileCalled=$true; [IO.File]::WriteAllText($OutputPath,'stub') }
        function Invoke-HostModeDockerFixture { $script:CapturedHostArgs=@($args);$global:LASTEXITCODE=0 }
        Invoke-BuildAll
        Assert-True -Condition $script:HostCompileCalled -Because 'host 模式必须调用宿主交叉编译'
        Assert-True -Condition ($script:CapturedHostArgs -contains $PrebuiltDockerfile) -Because 'host 模式必须使用 Dockerfile.go-svc.prebuilt'
        Assert-True -Condition (@($script:CapturedHostArgs | Where-Object { $_ -like 'prebuilt=*' }).Count -eq 1) -Because 'host 模式必须传入 prebuilt 命名上下文'
    } finally {
        Remove-Item -LiteralPath $taskRoot -Recurse -Force
    }
}
exit (Complete-TestRun -SuiteName 'registrar真实镜像工件')
