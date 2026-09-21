#requires -Version 7
param([Parameter(Mandatory)][string]$MSBuildPath, [Parameter(Mandatory)][string]$EvidenceDir)
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$fixture = Join-Path $EvidenceDir ('checker fixture ' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $fixture | Out-Null
$project = Join-Path $fixture 'fixture.vcxproj'
@'
<Project>
  <ItemGroup>
    <ClCompile Include="main.cpp">
      <PreprocessorDefinitions>REQUIRED_VALUE=7</PreprocessorDefinitions>
      <AdditionalIncludeDirectories>include</AdditionalIncludeDirectories>
    </ClCompile>
    <ClCompile Include="excluded.cpp"><ExcludedFromBuild>true</ExcludedFromBuild></ClCompile>
  </ItemGroup>
</Project>
'@ | Set-Content -LiteralPath $project -Encoding utf8
New-Item -ItemType Directory -Path (Join-Path $fixture 'include') | Out-Null
$header = Join-Path $fixture 'include/business.h'
'#include "business.h"' | Set-Content -LiteralPath (Join-Path $fixture 'main.cpp')
'#include "not_part_of_build.h"' | Set-Content -LiteralPath (Join-Path $fixture 'excluded.cpp')
$hook = Join-Path $PSScriptRoot '../check_no_raw_pointer_member.ps1'
$marker = Join-Path $fixture '.no_raw_ptr_check_ok'
$cases = @(
    @{Name='工程宏、包含目录、排除项与业务头'; Source="#if REQUIRED_VALUE != 7`n#error missing_project_definition`n#endif`nstruct Good { int value; };"; Exit=0},
    @{Name='业务头裸指针与缓存失效'; Source='struct Bad { int* value; };'; Exit=1},
    @{Name='业务头解析失败必须拒绝'; Source='#include "missing_business_header.h"'; Exit=2}
)
foreach ($case in $cases) {
    [IO.File]::WriteAllText($header, $case.Source)
    $output = & pwsh -NoProfile -File $hook -ProjectDir $fixture -ProjectName fixture -ProjectFile $project -MSBuildPath $MSBuildPath 2>&1
    $result = $LASTEXITCODE
    $output | Set-Content -LiteralPath (Join-Path $fixture "case-$($case.Exit).log")
    if ($result -ne $case.Exit) { throw "$($case.Name): expected $($case.Exit), actual $result; $output" }
    if (($case.Exit -eq 0) -ne (Test-Path -LiteralPath $marker)) { throw '成功缓存与检查结果不一致。' }
    Write-Host "PASS: $($case.Name)"
}
Write-Host "PASS: 3 个 MSBuild 实际输入检查用例；证据 $fixture"
exit 0
