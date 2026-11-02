param(
    [Parameter(Mandatory=$true)][string]$RunDir,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-f0-9]{32}$')][string]$RunId,
    [Parameter(Mandatory=$true)][string]$SourceReceipt,
    [Parameter(Mandatory=$true)][datetimeoffset]$RunStartedUtc,
    [string]$RobotConfig,
    [string]$DelayAccount,
    [string]$ZombieAccount,
    [ValidatePattern('^[A-Fa-f0-9]{64}$')][string]$ExpectedSourceReceiptSha256,
    [switch]$Watchdog,
    [int]$ParentPid=0,
    [datetimeoffset]$ParentStartedUtc=[datetimeoffset]::MinValue
)
# 仅本机独立功能演练，不做压测、不启动/停止基础设施，不改变系统配置。
# 控制器(go/data_service/internal/faultdrill)和机器人测试由本脚本从当前源码编进 RunDir，不接受外部旧二进制。
# 启动前由负责人提供本轮新建 scene 回执与真实 scene_manager 配置。
# 所有进程/锁都有截止；失败保留日志与资产，停止本脚本自己的 robot/控制器。
$ErrorActionPreference='Stop'
$taskRepo=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$taskRoot=[IO.Path]::GetFullPath($RunDir)
$taskBuild=[IO.Path]::GetFullPath((Join-Path $taskRepo 'build'))+[IO.Path]::DirectorySeparatorChar
if(-not $taskRoot.StartsWith($taskBuild,[StringComparison]::OrdinalIgnoreCase)){throw '演练目录必须在本仓 build 内'}
if(-not (Test-Path -LiteralPath $taskRoot -PathType Container)){throw '须先创建本轮独立目录并启动带 fault_run 的自有 source scene'}
if(-not $Watchdog -and (Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory -lt 2MB){throw '不足2GiB可用内存；不初始化或启动演练进程'}
# 只读一次字节，身份与SHA来自同一快照；watchdog不得重新解释被替换的收据。
$taskReceiptBytes=[IO.File]::ReadAllBytes([IO.Path]::GetFullPath($SourceReceipt))
$taskReceiptHasher=[Security.Cryptography.SHA256]::Create()
try{$taskReceiptSHA256=[BitConverter]::ToString($taskReceiptHasher.ComputeHash($taskReceiptBytes)).Replace('-','')}finally{$taskReceiptHasher.Dispose()}
if($Watchdog -and -not $ExpectedSourceReceiptSha256){throw '看门狗必须绑定主编排的收据SHA'}
if($ExpectedSourceReceiptSha256 -and $taskReceiptSHA256 -ne $ExpectedSourceReceiptSha256){throw 'scene收据字节已变，不操作任何进程'}
$taskReceipt=[Text.Encoding]::UTF8.GetString($taskReceiptBytes).TrimStart([char]0xFEFF)|ConvertFrom-Json
if($taskReceipt.service -ne 'scene' -or $taskReceipt.zone -ne 990017 -or $taskReceipt.fault_run -ne $RunId -or ([datetimeoffset]$taskReceipt.started_utc) -lt $RunStartedUtc){throw '拒绝暂停非本轮新建自有 scene'}
# 周期只读回执，不写死、不补缺省；控制器要等一次真实周期存盘，上限与控制器 ownershipFaultMaxIntervalSeconds 相同。
$taskIntervalValue=$taskReceipt.PSObject.Properties['source_save_interval_seconds']
if(-not $taskIntervalValue -or -not($taskIntervalValue.Value -is [int] -or $taskIntervalValue.Value -is [long]) -or $taskIntervalValue.Value -lt 1 -or $taskIntervalValue.Value -gt 120){throw 'scene回执必须记录1到120之间的真实source_save_interval_seconds'}
$taskIntervalSeconds=[int]$taskIntervalValue.Value
# 与控制器 ownershipFaultBaseBudgetSeconds+2*周期 同公式，改动两边一起改。
$taskBudgetSeconds=360+2*$taskIntervalSeconds
$taskNodeUuidValue=$taskReceipt.PSObject.Properties['node_uuid']
if(-not $taskNodeUuidValue -or $taskNodeUuidValue.Value -isnot [string] -or -not $taskNodeUuidValue.Value){throw 'scene回执必须记录本轮实际node_uuid，不补假值'}
$taskCreationValue=$taskReceipt.PSObject.Properties['process_creation_filetime_utc']
if(-not $taskCreationValue -or $taskCreationValue.Value -isnot [string] -or $taskCreationValue.Value -notmatch '^[1-9][0-9]{0,18}$'){throw 'scene回执必须记录实际进程创建FILETIME的十进制字符串，不接受缺省或近似时间'}
[uint64]$taskCreationFileTime=0
if(-not [uint64]::TryParse($taskCreationValue.Value,[ref]$taskCreationFileTime) -or $taskCreationFileTime -gt [long]::MaxValue){throw 'scene创建FILETIME非法'}
if(([datetimeoffset]$taskReceipt.started_utc).UtcDateTime.Ticks -ne [datetime]::FromFileTimeUtc([long]$taskCreationFileTime).Ticks){throw 'scene回执的UTC开始时间与精确创建FILETIME不一致'}
$taskImage=[IO.Path]::GetFullPath($taskReceipt.binary)
if(-not $taskImage.StartsWith($taskBuild,[StringComparison]::OrdinalIgnoreCase)){throw 'scene 必须是本轮 build 下的显式副本'}
function Get-OwnedSource {
    $taskProcess=Get-CimInstance Win32_Process -Filter "ProcessId=$($taskReceipt.pid)"
    if(-not $taskProcess){return $null}
    if($taskProcess.ExecutablePath -ne $taskImage -or [math]::Abs(($taskProcess.CreationDate.ToUniversalTime()-([datetimeoffset]$taskReceipt.started_utc).UtcDateTime).TotalSeconds) -gt 5){throw 'source PID/image/starttime 变化，拒绝操作'}
    if((Get-FileHash -LiteralPath $taskImage -Algorithm SHA256).Hash -ne $taskReceipt.binary_sha256){throw 'source image SHA 与创建回执不符'}
    # CIM只作预检；精确身份和SHA必须在同一已打开句柄存活期间再核验。
    [OwnershipFaultNative]::Verify([int]$taskReceipt.pid,$taskImage,$taskCreationFileTime,$taskReceipt.binary_sha256)
    return $taskProcess
}
if(-not ('OwnershipFaultNative' -as [type])){
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;
public static class OwnershipFaultNative {
    [DllImport("kernel32.dll", SetLastError=true)] private static extern IntPtr OpenProcess(uint access, bool inherit, int pid);
    [DllImport("kernel32.dll")] private static extern bool CloseHandle(IntPtr process);
    [DllImport("kernel32.dll", EntryPoint="QueryFullProcessImageNameW", CharSet=CharSet.Unicode, SetLastError=true)]
    private static extern bool QueryFullProcessImageName(IntPtr process, uint flags, StringBuilder image, ref uint length);
    [DllImport("kernel32.dll", SetLastError=true)]
    private static extern bool GetProcessTimes(IntPtr process, out long creation, out long exit, out long kernel, out long user);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern bool TerminateProcess(IntPtr process, uint exitCode);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern uint WaitForSingleObject(IntPtr process, uint milliseconds);
    [DllImport("ntdll.dll")] private static extern int NtSuspendProcess(IntPtr process);
    [DllImport("ntdll.dll")] private static extern int NtResumeProcess(IntPtr process);
    private static void VerifyHandle(IntPtr process, string expectedImage, ulong expectedCreation, string expectedSha) {
        if(expectedCreation==0 || String.IsNullOrEmpty(expectedSha) || expectedSha.Length!=64)
            throw new ArgumentException("缺少精确的自有进程身份");
        StringBuilder image=new StringBuilder(32768);
        uint length=(uint)image.Capacity;
        if(!QueryFullProcessImageName(process, 0, image, ref length))
            throw new Win32Exception(Marshal.GetLastWin32Error(), "读取自有进程路径失败");
        long creation, exit, kernel, user;
        if(!GetProcessTimes(process, out creation, out exit, out kernel, out user))
            throw new Win32Exception(Marshal.GetLastWin32Error(), "读取自有进程创建时间失败");
        if(!String.Equals(image.ToString(), Path.GetFullPath(expectedImage), StringComparison.OrdinalIgnoreCase) || (ulong)creation!=expectedCreation)
            throw new InvalidOperationException("自有进程句柄身份不符，拒绝操作");
        using(SHA256 sha=SHA256.Create())
        using(FileStream file=File.OpenRead(image.ToString())) {
            string actualSha=BitConverter.ToString(sha.ComputeHash(file)).Replace("-", "");
            if(!String.Equals(actualSha, expectedSha, StringComparison.OrdinalIgnoreCase))
                throw new InvalidOperationException("自有进程句柄对应镜像SHA不符，拒绝操作");
        }
    }
    private static IntPtr OpenOwnedProcess(int pid, string expectedImage, ulong expectedCreation, string expectedSha, uint access) {
        if(pid<=0) throw new ArgumentException("非法自有进程PID");
        IntPtr process=OpenProcess(access, false, pid);
        if(process==IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error(), "无法打开自有进程，未修改系统策略");
        try {
            VerifyHandle(process, expectedImage, expectedCreation, expectedSha);
            return process;
        } catch { CloseHandle(process); throw; }
    }
    public static void Verify(int pid, string expectedImage, ulong expectedCreation, string expectedSha) {
        IntPtr process=OpenOwnedProcess(pid, expectedImage, expectedCreation, expectedSha, 0x1000);
        CloseHandle(process);
    }
    public static void Change(int pid, string expectedImage, ulong expectedCreation, string expectedSha, bool suspend) {
        // 句柄在核验期间与实际操作期间始终指向同一对象，PID退出/复用不能切到另一个进程。
        IntPtr process=OpenOwnedProcess(pid, expectedImage, expectedCreation, expectedSha, 0x0800|0x1000);
        try { int status=suspend?NtSuspendProcess(process):NtResumeProcess(process); if(status!=0) throw new InvalidOperationException("暂停或恢复失败 status="+status); }
        finally { CloseHandle(process); }
    }
    public static void TerminateOwnedChild(Microsoft.Win32.SafeHandles.SafeProcessHandle handle, string expectedImage, ulong expectedCreation, string expectedSha) {
        bool referenced=false;
        try {
            handle.DangerousAddRef(ref referenced);
            IntPtr process=handle.DangerousGetHandle();
            uint state=WaitForSingleObject(process, 0);
            if(state==0) return;
            if(state!=0x102) throw new Win32Exception(Marshal.GetLastWin32Error(), "查询自有子进程退出状态失败");
            VerifyHandle(process, expectedImage, expectedCreation, expectedSha);
            if(!TerminateProcess(process, 1)) {
                int error=Marshal.GetLastWin32Error();
                if(WaitForSingleObject(process, 0)!=0)
                    throw new Win32Exception(error, "清理自有子进程失败");
            }
        } finally { if(referenced) handle.DangerousRelease(); }
    }
}
'@
}
function Write-Phase([string]$Name,[hashtable]$Data){
    $Data.run=$RunId
    $taskPath=Join-Path $taskRoot ($Name+'.json')
    if(Test-Path -LiteralPath $taskPath){throw '阶段证据已有，禁止覆盖'}
    $taskTemporary=$taskPath+'.ps.tmp'
    [IO.File]::WriteAllText($taskTemporary,($Data|ConvertTo-Json -Depth 6),[Text.UTF8Encoding]::new($false))
    [IO.File]::Move($taskTemporary,$taskPath)
}
if($Watchdog){
    # 独立进程：主编排被杀/退出或达到240s暂停上限时，仍能恢复同一 image/starttime。
    if(-not (Get-OwnedSource)){throw '看门狗启动时自有source已经退出'}
    Write-Phase 'watchdog-ready' @{pid=$taskReceipt.pid;image=$taskImage;process_creation_filetime_utc=$taskCreationValue.Value;binary_sha256=$taskReceipt.binary_sha256;source_receipt_sha256=$taskReceiptSHA256;utc=[datetime]::UtcNow.ToString('o')}
    $taskWatch=[Diagnostics.Stopwatch]::StartNew()
    while($taskWatch.Elapsed.TotalSeconds -lt 240){
        $taskDisarmed=Join-Path $taskRoot 'watchdog-disarmed.json'
        if(Test-Path -LiteralPath $taskDisarmed){$taskDisarm=Get-Content -LiteralPath $taskDisarmed -Raw|ConvertFrom-Json;if($taskDisarm.run -eq $RunId){return}}
        $taskParent=Get-CimInstance Win32_Process -Filter "ProcessId=$ParentPid"
        if(-not $taskParent -or [math]::Abs(($taskParent.CreationDate.ToUniversalTime()-$ParentStartedUtc.UtcDateTime).TotalSeconds) -gt 5){break}
        Start-Sleep -Milliseconds 200
    }
    $taskOwned=Get-OwnedSource
    if($taskOwned){[OwnershipFaultNative]::Change([int]$taskReceipt.pid,$taskImage,$taskCreationFileTime,$taskReceipt.binary_sha256,$false)}
    Write-Phase 'watchdog-resumed' @{pid=$taskReceipt.pid;image=$taskImage;utc=[datetime]::UtcNow.ToString('o');source_exists=($null-ne $taskOwned)}
    return
}
if((Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory -lt 2MB){throw '不足2GiB可用内存；不启动演练进程'}
if(-not $RobotConfig -or -not (Test-Path -LiteralPath $RobotConfig -PathType Leaf)){throw '缺少无凭据机器人配置'}
if($DelayAccount -eq $ZombieAccount -or $DelayAccount -notmatch '^robot_assetarrival_fault_[a-z0-9_]+$' -or $ZombieAccount -notmatch '^robot_assetarrival_fault_[a-z0-9_]+$'){throw '须提供两个本轮独立白名单账号'}
if(-not $env:OWNERSHIP_FAULT_PASSWORD -or -not $env:OWNERSHIP_FAULT_PLAYER_DSN){throw '凭据及专用端点须从受控环境注入，不打印或写计划'}
if(-not $env:OWNERSHIP_FAULT_SM_CONFIG -or -not [IO.Path]::IsPathRooted($env:OWNERSHIP_FAULT_SM_CONFIG) -or -not (Test-Path -LiteralPath $env:OWNERSHIP_FAULT_SM_CONFIG -PathType Leaf)){throw 'OWNERSHIP_FAULT_SM_CONFIG 必须是本轮真实 scene_manager 配置的绝对路径'}
if(Test-Path -LiteralPath (Join-Path $taskRoot 'runner-result.json')){throw '已有运行证据，换新目录，不自动重试'}
if(-not (Get-OwnedSource)){throw '自有source当前不存活，拒绝演练'}
$taskLogRoot=[IO.Path]::GetFullPath((Join-Path $taskReceipt.cwd 'logs/cpp_nodes'))
if(-not $taskLogRoot.StartsWith($taskBuild,[StringComparison]::OrdinalIgnoreCase)){throw '拒绝读取非本轮隔离C++日志'}
$taskOffsets=@{}
$taskLogTails=@{}
foreach($taskFile in @(Get-ChildItem -LiteralPath $taskLogRoot -File -Recurse -ErrorAction SilentlyContinue)){$taskOffsets[$taskFile.FullName]=$taskFile.Length}
# 从当前源码编译进本轮目录。旧二进制不能代替当前源码；目标测试不在二进制里时 go test 会零用例退出0，先用 -test.list 拦住。
function Build-OwnedTest([string]$Name,[string]$ModuleDir,[string]$Package,[string]$Test){
    $taskImagePath=Join-Path $taskRoot ($Name+'.test.exe')
    if(Test-Path -LiteralPath $taskImagePath){throw '保留原始产物，禁止重用目录'}
    $taskCompiler=Start-Process -FilePath 'go' -ArgumentList @('test','-c','-o',$taskImagePath,$Package) -WorkingDirectory $ModuleDir -WindowStyle Hidden -Wait -PassThru -RedirectStandardOutput (Join-Path $taskRoot ($Name+'.build.stdout.log')) -RedirectStandardError (Join-Path $taskRoot ($Name+'.build.stderr.log'))
    if($taskCompiler.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $taskImagePath -PathType Leaf)){throw ($Name+' 测试编译失败，见 build 日志')}
    $taskListed=@(& $taskImagePath ('-test.list=^'+$Test+'$'))
    if($taskListed -notcontains $Test){throw ($Name+' 二进制里没有目标测试 '+$Test)}
    return $taskImagePath
}
$env:GOTOOLCHAIN='local'
$env:GOPROXY='off'
$ControllerTestBinary=Build-OwnedTest 'controller' (Join-Path $taskRepo 'go/data_service') './internal/faultdrill' 'TestFullstackOwnershipFaults'
$RobotTestBinary=Build-OwnedTest 'robot' (Join-Path $taskRepo 'robot') '.' 'TestFullstackOwnershipFaultRobot'
$env:OWNERSHIP_FAULT_SOURCE_SAVE_INTERVAL_SECONDS="$taskIntervalSeconds"
$env:OWNERSHIP_FAULT_SOURCE_NODE_UUID=$taskNodeUuidValue.Value
$env:OWNERSHIP_FAULT_ROBOT_BUDGET_SECONDS="$($taskBudgetSeconds-30)"
$env:OWNERSHIP_FAULT_REQUIRE='1'
$env:OWNERSHIP_FAULT_MODE='dev'
$env:OWNERSHIP_FAULT_DIR=$taskRoot
$env:OWNERSHIP_FAULT_RUN=$RunId
$env:OWNERSHIP_FAULT_CONFIG=[IO.Path]::GetFullPath($RobotConfig)
$env:GOMAXPROCS='2'
$env:GOMEMLIMIT='128MiB'
$taskChildren=[Collections.Generic.List[object]]::new()
$taskSuspended=$false
$taskResumeAttempted=$false
$taskSourceInput=$null
$taskWatchdog=$null
$taskSucceeded=$false
$taskPrimaryError=$null
function Start-OwnedTest([string]$Name,[string]$Image,[string]$Test,[string]$Cwd){
    if((Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory -lt 2MB){throw '新增测试进程前内存门禁失败'}
    $taskOut=Join-Path $taskRoot ($Name+'.stdout.log')
    $taskErr=Join-Path $taskRoot ($Name+'.stderr.log')
    if((Test-Path -LiteralPath $taskOut)-or(Test-Path -LiteralPath $taskErr)){throw '保留原始日志，禁止重用角色'}
    $taskExpectedImage=[IO.Path]::GetFullPath($Image)
    $taskExpectedSHA=(Get-FileHash -LiteralPath $Image -Algorithm SHA256).Hash
    $taskChild=Start-Process -FilePath $Image -ArgumentList @('-test.v=true','-test.count=1',('-test.run=^'+$Test+'$'),('-test.timeout='+($taskBudgetSeconds+20)+'s')) -WorkingDirectory $Cwd -WindowStyle Hidden -PassThru -RedirectStandardOutput $taskOut -RedirectStandardError $taskErr
    # 创建器的句柄一直保留；清理时不再重新按PID解析对象。
    $taskEntry=@{name=$Name;process=$taskChild;pid=$taskChild.Id;image=$taskExpectedImage;sha256=$taskExpectedSHA;handle=$null;creation_filetime=[uint64]0}
    $taskChildren.Add($taskEntry)
    $taskEntry.handle=$taskChild.SafeHandle
    $taskEntry.creation_filetime=[uint64]$taskChild.StartTime.ToUniversalTime().ToFileTimeUtc()
    $taskNative=Get-CimInstance Win32_Process -Filter "ProcessId=$($taskChild.Id)"
    if(-not $taskNative){throw '测试进程启动即退出，可能受到Application Control限制；保留失败，不绕过系统策略'}
    Write-Phase ('process-'+$Name) @{pid=$taskEntry.pid;image=$taskEntry.image;binary_sha256=$taskEntry.sha256;created_utc=[datetime]::FromFileTimeUtc([long]$taskEntry.creation_filetime).ToString('o');process_creation_filetime_utc=$taskEntry.creation_filetime.ToString()}
    return $taskChild
}
function Resume-OwnedSource {
    $taskOwned=Get-OwnedSource
    if($taskOwned){[OwnershipFaultNative]::Change([int]$taskReceipt.pid,$taskImage,$taskCreationFileTime,$taskReceipt.binary_sha256,$false)}
    return ($null-ne $taskOwned)
}
# 目标测试必须实际进入并通过；退出码0不够（零用例也是0），SKIP 算失败。
function Assert-TestPassed([string]$Name,[string]$Test,[string[]]$Subtests){
    $taskOutput=[IO.File]::ReadAllText((Join-Path $taskRoot ($Name+'.stdout.log')))
    if($taskOutput -match '(?m)^\s*--- SKIP:'){throw ($Name+' 出现 SKIP，算失败')}
    if($taskOutput -notmatch ('(?m)^=== RUN\s+'+[regex]::Escape($Test)+'\s*$')){throw ($Name+' 未实际进入目标测试')}
    if($taskOutput -notmatch ('(?m)^--- PASS: '+[regex]::Escape($Test)+' ')){throw ($Name+' 目标测试未通过')}
    foreach($taskSubtest in $Subtests){if($taskOutput -notmatch ('(?m)^\s+--- PASS: '+[regex]::Escape($Test+'/'+$taskSubtest)+' ')){throw ($Name+' 场景未完成: '+$taskSubtest)}}
}
try{
    Write-Phase 'runner-inputs' @{source_receipt_sha256=$taskReceiptSHA256;source_process_creation_filetime_utc=$taskCreationValue.Value;controller_test_sha256=(Get-FileHash -LiteralPath $ControllerTestBinary -Algorithm SHA256).Hash;source_save_interval_seconds=$taskIntervalSeconds;budget_seconds=$taskBudgetSeconds;robot_test_sha256=(Get-FileHash -LiteralPath $RobotTestBinary -Algorithm SHA256).Hash;robot_config_sha256=(Get-FileHash -LiteralPath $RobotConfig -Algorithm SHA256).Hash;source_scene_sha256=$taskReceipt.binary_sha256;secret_values_recorded=$false}
    $taskController=Start-OwnedTest 'controller' $ControllerTestBinary 'TestFullstackOwnershipFaults' (Join-Path $taskRepo 'go/data_service')
    $taskDeadline=[Diagnostics.Stopwatch]::StartNew()
    $taskSeen=''
    $taskCommands=@('start-delay','start-source','suspend','start-target','resume','done')
    $taskNextCommand=0
    while($taskDeadline.Elapsed.TotalSeconds -lt ($taskBudgetSeconds+10)){
        $taskController.Refresh()
        if(Test-Path -LiteralPath (Join-Path $taskRoot 'command-abort.json')){throw '控制器失败，执行恢复和本轮子进程清理'}
        if($taskController.HasExited){break}
        $taskCommandPath=if($taskNextCommand -lt $taskCommands.Count){Join-Path $taskRoot ('command-'+$taskCommands[$taskNextCommand]+'.json')}else{''}
        if($taskCommandPath -and (Test-Path -LiteralPath $taskCommandPath)){
            $taskCommand=Get-Content -LiteralPath $taskCommandPath -Raw|ConvertFrom-Json
            if($taskCommand.run -ne $RunId){throw '控制命令不属于本轮'}
            if($taskCommand.command -ne $taskSeen){
                if($taskCommand.command -ne $taskCommands[$taskNextCommand]){throw '控制命令顺序错误'}
                $taskSeen=$taskCommand.command
                $taskNextCommand++
                switch($taskSeen){
                    'start-delay' {$env:OWNERSHIP_FAULT_ROLE='delay';$env:OWNERSHIP_FAULT_ACCOUNT=$DelayAccount;$null=Start-OwnedTest 'delay' $RobotTestBinary 'TestFullstackOwnershipFaultRobot' (Join-Path $taskRepo 'robot')}
                    'start-source' {$env:OWNERSHIP_FAULT_ROLE='zombie-source';$env:OWNERSHIP_FAULT_ACCOUNT=$ZombieAccount;$null=Start-OwnedTest 'zombie-source' $RobotTestBinary 'TestFullstackOwnershipFaultRobot' (Join-Path $taskRepo 'robot')}
                    'suspend' {
                        $taskSourceInput=Get-Content -LiteralPath (Join-Path $taskRoot 'suspend-input.json') -Raw|ConvertFrom-Json
                        if($taskSourceInput.run -ne $RunId -or $taskSourceInput.player_id -eq 0 -or $taskSourceInput.epoch -eq 0){throw '缺少真实源实体归属'}
                        if(-not (Get-OwnedSource)){throw '暂停前source已退出'}
                        $taskParent=Get-CimInstance Win32_Process -Filter "ProcessId=$PID"
                        $taskWatchArgs=@('-NoProfile','-File',$PSCommandPath,'-Watchdog','-RunDir',$taskRoot,'-RunId',$RunId,'-SourceReceipt',$SourceReceipt,'-ExpectedSourceReceiptSha256',$taskReceiptSHA256,'-RunStartedUtc',$RunStartedUtc.ToString('o'),'-ParentPid',"$PID",'-ParentStartedUtc',$taskParent.CreationDate.ToUniversalTime().ToString('o'))
                        $taskWatchdog=Start-Process -FilePath (Get-Process -Id $PID).Path -ArgumentList $taskWatchArgs -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $taskRoot 'watchdog.stdout.log') -RedirectStandardError (Join-Path $taskRoot 'watchdog.stderr.log')
                        $taskWatchReady=[Diagnostics.Stopwatch]::StartNew()
                        while(-not(Test-Path -LiteralPath (Join-Path $taskRoot 'watchdog-ready.json')) -and $taskWatchReady.Elapsed.TotalSeconds -lt 10){$taskWatchdog.Refresh();if($taskWatchdog.HasExited){throw '独立恢复看门狗启动失败，不暂停scene'};Start-Sleep -Milliseconds 50}
                        if(-not(Test-Path -LiteralPath (Join-Path $taskRoot 'watchdog-ready.json'))){throw '恢复看门狗就绪截止，不暂停scene'}
                        $taskReady=Get-Content -LiteralPath (Join-Path $taskRoot 'watchdog-ready.json') -Raw|ConvertFrom-Json
                        if($taskReady.run -ne $RunId -or $taskReady.pid -ne $taskReceipt.pid -or $taskReady.image -ne $taskImage -or $taskReady.process_creation_filetime_utc -ne $taskCreationValue.Value -or $taskReady.binary_sha256 -ne $taskReceipt.binary_sha256 -or $taskReady.source_receipt_sha256 -ne $taskReceiptSHA256){throw '恢复看门狗完整身份或收据SHA不符'}
                        # 先登记“可能已暂停”，中途异常同样执行finally恢复；看门狗独立兜底。
                        $taskSuspended=$true
                        [OwnershipFaultNative]::Change([int]$taskReceipt.pid,$taskImage,$taskCreationFileTime,$taskReceipt.binary_sha256,$true)
                        Write-Phase 'control-suspend' @{pid=$taskReceipt.pid;image=$taskImage;utc=[datetime]::UtcNow.ToString('o')}
                    }
                    'start-target' {$env:OWNERSHIP_FAULT_ROLE='zombie-target';$env:OWNERSHIP_FAULT_ACCOUNT=$ZombieAccount;$null=Start-OwnedTest 'zombie-target' $RobotTestBinary 'TestFullstackOwnershipFaultRobot' (Join-Path $taskRepo 'robot')}
                    'resume' {$taskPresent=Resume-OwnedSource;$taskSuspended=$false;$taskResumeAttempted=$true;Write-Phase 'control-resume' @{source_exists=$taskPresent;utc=[datetime]::UtcNow.ToString('o')}}
                    'abort' {throw '控制器失败，执行恢复和本轮子进程清理'}
                    'done' {}
                    default {throw '未知控制命令'}
                }
            }
        }
        if($taskResumeAttempted -and -not(Test-Path -LiteralPath (Join-Path $taskRoot 'old-write-evidence.json'))){
            $taskPattern='HandlePlayerSaveRejected: owner_epoch CAS rejected save for player '+[regex]::Escape([string]$taskSourceInput.player_id)+' key=\S+ epoch='+[regex]::Escape([string]$taskSourceInput.epoch)+'(?:\s|$)'
            foreach($taskFile in @(Get-ChildItem -LiteralPath $taskLogRoot -File -Recurse -ErrorAction SilentlyContinue)){
                $taskOffset=if($taskOffsets.ContainsKey($taskFile.FullName)){$taskOffsets[$taskFile.FullName]}else{0L}
                if($taskFile.Length -lt $taskOffset){throw '本轮C++日志截断，不能可靠判定旧写证据'}
                $taskStream=[IO.File]::Open($taskFile.FullName,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)
                $taskReader=$null
                try{$null=$taskStream.Seek($taskOffset,[IO.SeekOrigin]::Begin);$taskReader=[IO.StreamReader]::new($taskStream);$taskText=$taskReader.ReadToEnd();$taskOffsets[$taskFile.FullName]=$taskStream.Position}finally{if($taskReader){$taskReader.Dispose()}else{$taskStream.Dispose()}}
                if($taskLogTails.ContainsKey($taskFile.FullName)){$taskText=$taskLogTails[$taskFile.FullName]+$taskText}
                $taskLogTails[$taskFile.FullName]=$taskText.Substring([math]::Max(0,$taskText.Length-1024))
                if($taskText -match $taskPattern){Write-Phase 'old-write-evidence' @{old_write_rejected=$true;player_id=$taskSourceInput.player_id;epoch=$taskSourceInput.epoch;source_pid=$taskReceipt.pid;source_image_sha256=$taskReceipt.binary_sha256;log_file=$taskFile.Name;utc=[datetime]::UtcNow.ToString('o')};break}
            }
        }
        Start-Sleep -Milliseconds 50
    }
    $taskController.Refresh()
    if(Test-Path -LiteralPath (Join-Path $taskRoot 'command-done.json')){$taskLast=Get-Content -LiteralPath (Join-Path $taskRoot 'command-done.json') -Raw|ConvertFrom-Json;if($taskLast.run -eq $RunId -and $taskLast.command -eq 'done'){$taskSeen='done'}}
    if(-not $taskController.HasExited -or $taskController.ExitCode -ne 0 -or $taskSeen -ne 'done' -or -not(Test-Path -LiteralPath (Join-Path $taskRoot 'old-write-evidence.json'))){throw '真实故障控制器失败、超时或无实际旧写拒绝证据'}
    $taskDrain=[Diagnostics.Stopwatch]::StartNew()
    do{$taskAlive=@();foreach($taskEntry in $taskChildren){$taskEntry.process.Refresh();if(-not $taskEntry.process.HasExited){$taskAlive+=$taskEntry}};if($taskAlive.Count -eq 0){break};Start-Sleep -Milliseconds 100}while($taskDrain.Elapsed.TotalSeconds -lt 10)
    foreach($taskEntry in $taskChildren){$taskEntry.process.Refresh();if(-not $taskEntry.process.HasExited -or $taskEntry.process.ExitCode -ne 0){throw '真实机器人未在截止前正常完成，不能只根据控制器宣称通过'}}
    Assert-TestPassed 'controller' 'TestFullstackOwnershipFaults' @('commit_blocked_handoff_waits','stale_scene_resume_write_rejected')
    foreach($taskRole in @('delay','zombie-source','zombie-target')){Assert-TestPassed $taskRole 'TestFullstackOwnershipFaultRobot' @()}
    foreach($taskProofName in @('save-blocked-evidence','delay-proof','zombie-proof')){
        $taskProofPath=Join-Path $taskRoot ($taskProofName+'.json')
        if(-not(Test-Path -LiteralPath $taskProofPath)){throw ('缺少场景证据 '+$taskProofName)}
        if((Get-Content -LiteralPath $taskProofPath -Raw|ConvertFrom-Json).run -ne $RunId){throw ('场景证据不属于本轮 '+$taskProofName)}
    }
    $taskSucceeded=$true
}catch{
    $taskPrimaryError=$_
}finally{
    # 首先恢复scene，绝不因为控制器/机器人失败而遗留挂起进程。
    $taskRestoreError=$null
    $taskCleanupErrors=[Collections.Generic.List[string]]::new()
    if($taskSuspended){try{$null=Resume-OwnedSource;$taskSuspended=$false}catch{$taskRestoreError='主编排恢复失败，独立看门狗仍保持武装'}}
    if(-not $taskSuspended -and $taskWatchdog -and -not(Test-Path -LiteralPath (Join-Path $taskRoot 'watchdog-disarmed.json'))){
        try{Write-Phase 'watchdog-disarmed' @{utc=[datetime]::UtcNow.ToString('o')}}catch{$taskCleanupErrors.Add('watchdog-disarmed: '+$_.Exception.Message)}
    }
    foreach($taskEntry in $taskChildren){
        try{
            $taskEntry.process.Refresh()
            if(-not $taskEntry.process.HasExited){
                if(-not $taskEntry.handle -or $taskEntry.creation_filetime -eq 0){throw '缺少创建器持有的精确子进程身份，拒绝按PID清理'}
                [OwnershipFaultNative]::TerminateOwnedChild($taskEntry.handle,$taskEntry.image,$taskEntry.creation_filetime,$taskEntry.sha256)
                if(-not $taskEntry.process.WaitForExit(5000)){throw '自有子进程清理后仍未退出'}
            }
        }catch{$taskCleanupErrors.Add($taskEntry.name+': '+$_.Exception.Message)}
    }
    try{Write-Phase 'runner-result' @{actual_pass=($taskSucceeded -and -not $taskRestoreError -and $taskCleanupErrors.Count -eq 0);scene_still_suspended=$taskSuspended;restore_error=$taskRestoreError;primary_error=$(if($taskPrimaryError){$taskPrimaryError.Exception.Message}else{$null});cleanup_errors=@($taskCleanupErrors.ToArray());secret_values_recorded=$false;other_processes_touched=$false;infrastructure_touched=$false;ended_utc=[datetime]::UtcNow.ToString('o')}}catch{$taskCleanupErrors.Add('runner-result: '+$_.Exception.Message)}
}
if($taskPrimaryError){throw $taskPrimaryError}
if($taskRestoreError){throw $taskRestoreError}
if($taskCleanupErrors.Count){throw ($taskCleanupErrors -join '; ')}
