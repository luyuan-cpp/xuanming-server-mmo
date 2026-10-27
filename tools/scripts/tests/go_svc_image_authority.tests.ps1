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
    foreach($name in @('guild','mail','rank','writer-authority')){
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
exit (Complete-TestRun -SuiteName 'registrar真实镜像工件')
