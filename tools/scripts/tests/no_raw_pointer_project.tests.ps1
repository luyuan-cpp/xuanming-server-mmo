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
# 每项使用独立工程；被排除的翻译单元故意缺头文件，防止只隐藏诊断却仍解析它们。
# 夹具放调用方指定的证据目录，调用时应传临时目录，不能放进会被排除的 build 树。
$boundaryCases = @(
    @{
        Id = 'third-party-source'; Name = '第三方源码翻译单元不参与检查'; Exit = 0
        Sources = @('third_party/sdk/ignored.cpp'); Options = ''
        Files = @{
            'main.cpp' = 'struct Good { int value; };'
            'third_party/sdk/ignored.cpp' = '#include "missing_third_party_header.h"'
        }
    },
    @{
        Id = 'muduo-source'; Name = '嵌套 muduo 源码翻译单元不参与检查'; Exit = 0
        Sources = @('cpp/libs/engine/muduo_windows/ignored.cpp'); Options = ''
        Files = @{
            'main.cpp' = 'struct Good { int value; };'
            'cpp/libs/engine/muduo_windows/ignored.cpp' = '#include "missing_muduo_header.h"'
        }
    },
    @{
        Id = 'included-library-headers'; Name = '业务包含第三方与嵌套 muduo 头文件不误报'; Exit = 0
        Sources = @(); Options = ''
        Files = @{
            'main.cpp' = "#include `"third_party/vendor.h`"`n#include `"cpp/libs/engine/muduo_windows/vendor.h`"`nstruct Good { int value; };"
            'third_party/vendor.h' = 'struct ThirdPartyRecord { int* library_member; };'
            'cpp/libs/engine/muduo_windows/vendor.h' = 'struct MuduoRecord { int* library_member; };'
        }
    },
    @{
        Id = 'similar-project-directory'; Name = '自有 third_party_adapter 目录仍检查'; Exit = 1
        Sources = @('third_party_adapter/local.cpp'); Options = ''; Member = 'project_member'
        Files = @{
            'main.cpp' = 'struct Good { int value; };'
            'third_party_adapter/local.cpp' = 'struct ProjectRecord { int* project_member; };'
        }
    },
    @{
        Id = 'project-pointer-to-library'; Name = '业务持有第三方类型的裸指针仍拒绝'; Exit = 1
        Sources = @(); Options = ''; Member = 'project_member'
        Files = @{
            'main.cpp' = "#include `"third_party/vendor.h`"`nstruct ProjectRecord { ThirdPartyRecord* project_member; };"
            'third_party/vendor.h' = 'struct ThirdPartyRecord { int* library_member; };'
        }
    },
    @{
        Id = 'external-include-with-spaces'; Name = 'CMake 外部包含选项保留含空格路径与系统头语义'; Exit = 0
        Sources = @(); Options = '/external:I "$(MSBuildProjectDirectory)\external headers"'
        Files = @{
            'main.cpp' = '#include <external_vendor.h>'
            'external headers/external_vendor.h' = 'struct ExternalLibrary { int* library_member; };'
        }
    }
)
foreach ($case in $boundaryCases) {
    $caseRoot = Join-Path $fixture $case.Id
    New-Item -ItemType Directory -Path $caseRoot | Out-Null
    foreach ($entry in $case.Files.GetEnumerator()) {
        $path = Join-Path $caseRoot $entry.Key
        New-Item -ItemType Directory -Path (Split-Path -Parent $path) -Force | Out-Null
        [IO.File]::WriteAllText($path, $entry.Value)
    }
    $additionalSources = @($case.Sources | ForEach-Object {
        '<ClCompile Include="' + [Security.SecurityElement]::Escape($_) + '" />'
    }) -join "`n"
    $options = [Security.SecurityElement]::Escape($case.Options)
    $caseProject = Join-Path $caseRoot 'fixture.vcxproj'
    @"
<Project>
  <ItemGroup>
    <ClCompile Include="main.cpp">
      <AdditionalOptions>$options</AdditionalOptions>
    </ClCompile>
    $additionalSources
  </ItemGroup>
</Project>
"@ | Set-Content -LiteralPath $caseProject -Encoding utf8
    $output = & pwsh -NoProfile -File $hook -ProjectDir $caseRoot -ProjectName $case.Id -ProjectFile $caseProject -MSBuildPath $MSBuildPath 2>&1
    $result = $LASTEXITCODE
    $output | Set-Content -LiteralPath (Join-Path $caseRoot 'check.log')
    if ($result -ne $case.Exit) { throw "$($case.Name): 期望退出码 $($case.Exit)，实际 $result；$output" }
    $caseMarker = Join-Path $caseRoot '.no_raw_ptr_check_ok'
    if (($case.Exit -eq 0) -ne (Test-Path -LiteralPath $caseMarker)) { throw "$($case.Name): 成功缓存与检查结果不一致。" }
    $outputText = $output -join "`n"
    if ($case.ContainsKey('Member') -and $outputText -notmatch [regex]::Escape($case.Member)) {
        throw "$($case.Name): 未报告预期的业务成员 $($case.Member)；$output"
    }
    if ($outputText -match "raw pointer member 'library_member'") { throw "$($case.Name): 第三方内部成员被误报；$output" }
    Write-Host "PASS: $($case.Name)"
}
Write-Host "PASS: $($cases.Count + $boundaryCases.Count) 个 MSBuild 实际输入检查用例；证据 $fixture"
exit 0
