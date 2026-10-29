# 全局单点 P0 五项:实测步骤原文(2026-10-14)

> 配套 [Codex 任务单](global-singletons-p0-codex-tasksheet-20261014.md)。本文是五个只读代理把各项实测步骤锚定到 HEAD `2f897bd07`(完整哈希 `2f897bd0780c1cc78284548284f20d01e61d7ea8`)之后的原文,主会话没有逐字改写。
>
> - **任务单 §2「公共约定」与 §3「覆盖条款」优先于本文。** 例如:工作树统一用任务单的 `$W`;P0-A 的 A2 要补 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`;docker 命令一律带 `--context desktop-linux` 与 `--pull never`。
> - 标「受阻」、需要负责人同意、或要求先写测试文件的步骤,本轮不做(任务单 §5)。
> - 「出处 file:line」按 `2f897bd07`;标「外部常识」「需 Codex 先确认」的以实际输出为准。

## P0-A C++ 节点连 Redis 不发 AUTH


### 现在能跑 / 受阻(摘要)

**现在能跑**:现在就能跑:步骤 0(0.1、0.2)、A2、A3、C0~C7,以及可选的 A1(已知必红一条,与本项无关)。这些都不编译 C++、不需要子模块、不连集群;只有 C 段在本机没有任何 redis 镜像时,要先经负责人同意下载。

**受阻**:

受阻:B0~B6(运行期 Q-B),以及修复后的全部 C++ 验证(fix_design 第 10 条第 1、2、4、5 步)。

阻塞原因:
1. Boost 是模块布局,而 vcxproj 只把 third_party/boost/ 根加进 include。muduo 的 EventLoop.h:18(boost/any.hpp)和 Timestamp.h:12(boost/operators.hpp)因此报 C1083(PROGRESS:7637)。归档恢复脚本要的分卷已随历史重写删除,本机也没有 Python。
2. third_party/grpc/install_vs2026_dbg 不存在。三个节点和 rpc_controller_test / configuration_table_test 链接的 hiredisd.lib、yaml-cppd.lib、gRPC / absl / protobuf 库全部缺失,运行期需要的 zlibd.dll 也在这里。
3. bin\ 下没有 scene.exe。
4. 5 个未初始化子模块(cppcodec、fmt、muduo-linux、recastnavigation、xxhash)是否被 Windows 构建需要,待确认。
5. 上述解除后主干其余部分能否编过,未核实。
6. 编译树未定:新工作树没有子模块(初始化即下载);主工作区当前有 71 处他人在途改动,编出来的不是纯 HEAD 或纯修复版。

解除条件:B0 前三项都为 True,且 B1 全量构建 `/m:1` 退出码为 0、产出 bin\scene.exe。

由谁解除:
- 负责人拍板授权。装 / 升 cmake、ninja,在 third_party/boost 生成扁平头或改 vcxproj include,build_grpc.ps1 加 yaml-cpp / hiredis 的全量构建,初始化子模块,都属 AGENTS.md §10.2 的停止条件。
- 授权后由 Codex 修环境并构建,具体命令需 Codex 先确认。Claude 不改构建环境。

建议:B 段推迟到第一批落码之后,环境修好后做一次构建,同时取红态(黑盒第 0 组)和绿态。

### 步骤原文

【P0-A 实测任务单(锚定 2f897bd07;所有出处按 git show 2f897bd07 核过)】

要回答的问题
- Q-A(部署侧):在注入了 Redis 口令的部署产物里,是否服务端开了 requirepass、Go 服务拿到了口令,而 C++ 的 node-config / battle-node-config 和 C++ 工作负载都没有任何口令来源?
- Q-C(外部前提,B 段判据与修复设计依赖它):带 requirepass 的 Redis 对未认证命令回 NOAUTH 并计入 errorstats;错口令回 WRONGPASS 且不回显;向无口令 Redis 发 AUTH 报错;docker restart 后口令仍在。
- Q-B(运行侧):C++ scene 连带 requirepass 的 Redis 时,是否不发 AUTH、命令被拒?

分组
- 【现在就能跑】步骤 0、A1~A3、C0~C7。不编 C++,不需要子模块,不连集群。
- 【受阻】B0~B6。需要可运行的 scene.exe。

判据
- A 段:
  - 成立:node-config 与 battle-node-config 的 zoneredis 段是 `password: ""`;C++ 工作负载块无 secretKeyRef、无口令串;go-svc-db-config 的 RedisClient.Password 等于注入值;infra-up 输出里有带该口令的 Secret。
  - 不成立:任一 C++ 配置含注入值,或任一 C++ 工作负载块有 secretKeyRef 或口令。
  - 没跑起来:退出码非 0、C++ 块数为 0、或取不到目标块。把输出文件交回。
- C 段:
  - 前提成立:C2~C6 全部符合。
  - 前提不成立:任一不符。照实回报,B 段对应判据作废或改写。
  - 没跑起来:docker 不可用、没有镜像、或端口被占。
- B 段:
  - 成立:先出现 `Redis connected to 127.0.0.1:6390`,随后两条 `SCRIPT LOAD failed for PlayerAllData (reply`,探针 errorstats 出现 `errorstat_NOAUTH`。
  - 不成立:两条 `SCRIPT LOAD ok for PlayerAllData sha1=`,且没有 NOAUTH。
  - 没跑起来:缺 `GAME_CONFIG_PATH env override applied:` 或 `Zone Redis address: 127.0.0.1:6390`。
  - 对照组(无口令探针)必须得到「不成立」的表现,证明判据能区分。

【步骤 0:环境与工作树】(现在就能跑;只读检查加新建一个工作树;约 5 分钟;无破坏性;无下载)
0.1 确认 PowerShell 7:
```
pwsh --version
```
- 期望 `PowerShell 7.x`。
- 找不到就停下报负责人。安装工具需授权(AGENTS.md §10.2),不要用 Windows PowerShell 5.1 硬跑。
- 出处:tools/scripts/tests/lib/deploy_capture.ps1:1 `#requires -Version 7`;:78 子进程用 pwsh。
- 旁证:Codex 10-14 的 battle MSBuild 日志里 pwsh 跑过 check_no_raw_pointer_member.ps1(C:/Users/luyua/AppData/Local/Temp/codex-battle-handler-20261014/battle-msbuild.log),版本未记录。

0.2 新建干净工作树:
```
git -C D:/luyuan/xuanming-server-mmo-cpp rev-parse 2f897bd07
git -C D:/luyuan/xuanming-server-mmo-cpp worktree add --detach C:/Users/luyua/.codex/worktrees/p0a-2f897bd07 2f897bd07
git -C C:/Users/luyua/.codex/worktrees/p0a-2f897bd07 rev-parse HEAD
git -C C:/Users/luyua/.codex/worktrees/p0a-2f897bd07 status --short
```
- 期望:第 1、3 条都输出 `2f897bd0780c1cc78284548284f20d01e61d7ea8`;第 4 条为空。保留四条输出。
- 下面三处不要跑 A 段,也不得 stash / reset / checkout --:
  - 主工作区:有他人在途改动,其中 k8s_deploy.ps1 和 k8s_deploy_contract.tests.ps1 与 HEAD 不同;
  - Codex f452;
  - Codex 793e。
- 工作树路径是本任务单新取的,可以改。A 段不需要子模块和构建产物。

【A 段:部署渲染】(现在就能跑;pwsh 7;不连集群、不用 docker、不编译;A2+A3 约 10 分钟,A1 耗时未核实;无破坏性;无下载)

A1(可选,只记录,不作为 A2/A3 的门):
```powershell
Set-Location C:/Users/luyua/.codex/worktrees/p0a-2f897bd07
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```
- 期望:退出码 1,属预期。
- 已知必红一条:`负向:zones.10zones.yaml 时打 MySQL连接总账超限 警告且不阻断`。用例在 :1649,:1656 断言旧中文 `MySQL连接总账超限`,而 k8s_deploy.ps1:1238 现在打 `[MySQLConnectionLedger]OverLimit:`。
- 其它红照原文附上交负责人,不在本项归因。
- 出处:该文件 :20-21。

A2 zone-up 渲染(prod 档):
```powershell
Set-Location C:/Users/luyua/.codex/worktrees/p0a-2f897bd07
. ./tools/scripts/tests/lib/deploy_capture.ps1
Remove-Item Env:MMORPG_REDIS_PASSWORD -ErrorAction SilentlyContinue
$pw = 'contract-test-redis-pw-01'
$envs = @{
  MMORPG_GATE_TOKEN_SECRET     = 'contract-test-secret-0123456789abcdef0123'
  MMORPG_MYSQL_USER            = 'mmorpg_app'
  MMORPG_MYSQL_PASSWORD        = 'contract-test-mysql-pw-01'
  MMORPG_REDIS_PASSWORD        = $pw
  MMORPG_GATEWAY_DB_USER       = 'gateway_app'
  MMORPG_GATEWAY_DB_PASSWORD   = 'contract-test-gwdb-pw-01'
  MMORPG_INTERNAL_AUTH_SECRET  = 'contract-test-internal-auth-0123456789abcdef'
  MMORPG_DB_ALLOWED_DATABASES  = 'zone_1_db,zone_2_db'
  MMORPG_GATEWAY_ADMIN_API_KEY = 'contract-test-gw-admin-key-0123456789abcdef'
}
$zargs = @('-Command','zone-up','-ZoneName','contract-test','-ZoneId','101','-DryRun','-GoSvcRegistry','registry.invalid/test','-JavaSvcRegistry','registry.invalid/test','-ReleaseProfile','prod','-SkipPreflight','-NodeImage','ghcr.io/luyuancpp/mmorpg-node:0123456789ab')
$run = Invoke-DeployDryRun -Arguments $zargs -Env $envs
$run.Output | Set-Content -Encoding utf8 (Join-Path $env:TEMP 'p0a_A2_zone_up.txt')
"1 exit=$($run.ExitCode)"
$node = Select-ManifestByName -Output $run.Output -Name 'node-config'
"2 node-config zoneredis 段:"; [regex]::Match($node,'(?ms)^\s*zoneredis:.*?retry_interval:[^\r\n]*').Value
"3 node-config 含口令=$($node -match [regex]::Escape($pw))"
$db = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $run.Output -Name 'go-svc-db-config')
"4 db RedisClient.Password=$(Get-FlatValue -Flat $db -KeyPath 'data.db.yaml.ServerConfig.RedisClient.Password')"
$cpp = @(Get-ManifestBlocks -Output $run.Output | Where-Object { $_ -match 'mountPath:\s*/app/bin/etc' })
"5 C++ 工作负载块数=$($cpp.Count)"
"6 其中带 secretKeyRef 或口令的块数=$(@($cpp | Where-Object { $_ -match 'secretKeyRef' -or $_ -match [regex]::Escape($pw) }).Count)"
```
- 期望(缺陷成立):
  - 第 1 行 `exit=0`;
  - 第 2 行含 `host: "redis.mmorpg-infra"` 和 `password: ""`;
  - 第 3 行 False;
  - 第 4 行等于 `contract-test-redis-pw-01`;
  - 第 5 行 ≥ 1;
  - 第 6 行 0。
- 缺陷不成立:第 3 行 True,或第 6 行 ≥ 1。
- 没跑起来:第 5 行为 0,或第 2 / 4 行取不到。把输出文件交回。
- 第 1 行不是 0 时:保留输出末尾 40 行。把 `$zargs` 里 `'-ReleaseProfile'` 起到末尾的参数去掉,`-Env` 换成 `@{ MMORPG_REDIS_PASSWORD = $pw }`,重跑第 1~6 行;判据相同。dev 档变量非空即采用(release_common.ps1:690-692)。
- 保留:六行输出和 `%TEMP%\p0a_A2_zone_up.txt`。口令是测试假值,可以留。
- 出处:
  - 参数与环境变量:k8s_deploy_contract.tests.ps1:33-40、:975-995;
  - 函数:deploy_capture.ps1:95、:122、:147、:166、:180;
  - 口令解析:k8s_deploy.ps1:502-503;
  - zoneredis 模板 :2298-2305(New-NodeConfigMapYaml :2109),zone 的 node-config 在 :6043 生成;
  - db 的 RedisClient.Password :3998-4001;
  - C++ 工作负载挂载 :2496、:3036、lib/k8s_client_entry.ps1:701;
  - 默认 InfraNamespace :24。

A3 infra-up 渲染(同一 pwsh 会话接着跑):
```powershell
$iargs = @('-Command','infra-up','-DryRun','-SkipPreflight','-SkipGoSvc','-SkipJavaSvc','-WaitReady','-InfraNamespace','contract-battle-infra','-NodeImage','registry.invalid/test/mmorpg-node:0123456789ab')
$benv = @{ MMORPG_BATTLE_TOKEN_SECRET='contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET='contract-gate-ticket-0123456789abcdef012345' }
$with    = Invoke-DeployDryRun -Arguments $iargs -Env ($benv + @{ MMORPG_REDIS_PASSWORD = $pw })
$without = Invoke-DeployDryRun -Arguments $iargs -Env $benv
$with.Output | Set-Content -Encoding utf8 (Join-Path $env:TEMP 'p0a_A3_infra_with.txt')
$secretPattern = '(?m)^stringData:\s*\r?\n\s+password:\s*"' + [regex]::Escape($pw) + '"'
"7 exit(with)=$($with.ExitCode) exit(without)=$($without.ExitCode)"
"8 with: 建 Secret=$($with.Output -match $secretPattern)"
"9 without: 建 Secret=$($without.Output -match $secretPattern) 删 Secret=$($without.Output -match 'delete secret redis-auth -n contract-battle-infra --ignore-not-found')"
$bcfg = Select-ManifestByName -Output $with.Output -Name 'battle-node-config'
"10 battle-node-config zoneredis 段:"; [regex]::Match($bcfg,'(?ms)^\s*zoneredis:.*?retry_interval:[^\r\n]*').Value
"11 battle-node-config 含口令=$($bcfg -match [regex]::Escape($pw))"
```
- 期望(缺陷成立):
  - 第 7 行两个都是 0;
  - 第 8 行 True;
  - 第 9 行 `建 Secret=False 删 Secret=True`;
  - 第 10 行含 `password: ""`;
  - 第 11 行 False。
- 缺陷不成立:第 11 行 True。
- 没跑起来:第 7 行非 0,或第 10 行为空。
- 不能用 `name: redis-auth` 当判据:redis.yaml:102 恒有这一行,DryRun 又会打印整份清单(k8s_deploy.ps1:2059-2068)。
- 保留:第 7~11 行输出和 `%TEMP%\p0a_A3_infra_with.txt`。
- 出处:
  - k8s_deploy_contract.tests.ps1:1149-1151;
  - Secret 段 k8s_deploy.ps1:6497-6514(模板 :6504-6512,删除 :6498);
  - battle-node-config :6436-6437(BattleReplicas>0 才生成);
  - DryRun 下 kubectl 命令行的打印格式 :2035-2037。

A 段结束后,若 B 段不在这棵树编译,可删工作树:
```
git -C D:/luyuan/xuanming-server-mmo-cpp worktree remove C:/Users/luyua/.codex/worktrees/p0a-2f897bd07
```

【C 段:Redis 外部行为探针】(现在就能跑;只起 docker 一次性容器,不编 C++,不需要工作树;约 5 分钟;无破坏性:不动 compose 的 `redis` 容器和 6379,不清任何数据)
C0 选镜像:
```
docker image ls redis
```
- 用本机已有的:compose 用 `redis:latest`(deploy/docker-compose.yml:217),K8s 用 `redis:7.2`(redis.yaml:83)。
- 一个都没有就停。**下载先征得负责人同意**。

C1 起两个探针(容器名、端口、口令是本任务单新取的):
```
docker run -d --name p0a-redis-auth -p 127.0.0.1:6390:6379 <镜像> redis-server --requirepass p0a-probe-pass-01
docker run -d --name p0a-redis-open -p 127.0.0.1:6391:6379 <镜像> redis-server
```

C2 未认证命令:
```
docker exec p0a-redis-auth redis-cli PING
```
- 期望输出含 `NOAUTH Authentication required`。

C3 errorstats:
```
docker exec p0a-redis-auth redis-cli -a p0a-probe-pass-01 --no-auth-warning INFO errorstats
```
- 期望含 `errorstat_NOAUTH:count=`,计数 ≥ 1。
- 段为空说明镜像低于 6.2(errorstats 从 6.2 起才有,外部常识)。此时 B 段只用日志判据。

C4 错口令:
```
docker exec p0a-redis-auth redis-cli AUTH p0a-wrong-pass-02
```
- 期望以 `WRONGPASS` 开头,且输出不含 `p0a-wrong-pass-02`。

C5 向无口令 Redis 发 AUTH:
```
docker exec p0a-redis-open redis-cli AUTH p0a-probe-pass-01
```
- 期望以 `ERR` 开头。新版文本是 `AUTH <password> called without any password configured …`;仓内注释 k8s_deploy.ps1:4634 记的是旧版 `ERR Client sent AUTH, but no password is set`。两种都算。

C6 重启后口令仍在:
```
docker restart p0a-redis-auth
docker exec p0a-redis-auth redis-cli PING
```
- 期望仍含 NOAUTH。

C7 清理(B 段届时重新起):
```
docker rm -f p0a-redis-auth p0a-redis-open
```

- redis-cli 收到错误应答时的退出码不作判据,只看输出文本。保留 C0~C6 的原始输出。
- 出处:
  - docker exec 调 redis-cli 的写法:tools/scripts/currency_crash_window.ps1:191;
  - `-a … --no-auth-warning`:tools/scripts/k8s_zone_rollback.ps1:847;
  - NOAUTH 判法:redis.yaml:117;
  - 源码佐证(仓内 third_party/redis 是 8.4.0,运行镜像版本可能不同):src/server.c:2077(NOAUTH)、:6555(errorstats 行格式)、src/acl.c:1479(WRONGPASS)、:3255-3257(无口令时 AUTH)。
- C 段不证明缺陷本身(那由 A 段加静态证据给出),只核对外部前提。

【B 段:运行期】受阻。先做 B0,任一项不满足就停下报负责人,不要自行解决。

B0 前置检查(pwsh;只读):
```powershell
Set-Location <准备用来编译的树根>
"boost 扁平头=$(Test-Path third_party/boost/boost/any.hpp)"
"hiredisd.lib=$(Test-Path third_party/grpc/install_vs2026_dbg/lib/hiredisd.lib)"
"yaml-cppd.lib=$(Test-Path third_party/grpc/install_vs2026_dbg/lib/yaml-cppd.lib)"
"scene.exe=$(Test-Path bin/scene.exe)"
"submodules 未初始化数=$(@(git submodule status | Where-Object { $_ -match '^-' }).Count)"
```
- 期望(可以继续):前三个为 True。
- 现状(10-14 主工作区只读核对):False / False / False / False / 5。
- 阻塞 1:Boost 是模块布局,头文件在 third_party/boost/libs/<库>/include,而工程只把 third_party/boost/ 根加进 include(cpp/libs/engine/muduo_windows/muduo.vcxproj:194 等)。
  - muduo 的 EventLoop.h:18(boost/any.hpp)与 Timestamp.h:12(boost/operators.hpp)因此找不到。Codex 编 battle.vcxproj 就停在这里的 C1083(docs/PROGRESS.md:7637、:7645)。
  - 归档恢复脚本 tools/patches/boost/restore_local_source.py 所需的 zip 分卷已随历史重写删除(PROGRESS:7618),本机也没有 Python,这条路不可用。
- 阻塞 2:install_vs2026_dbg 不存在。
  - gate.vcxproj:158、scene.vcxproj:123、battle.vcxproj:141 用 /WHOLEARCHIVE 链接其中的 hiredisd.lib / yaml-cppd.lib;rpc_controller_test 与 configuration_table_test 也链接该目录;zlibd.dll 也从这里取(run_cpp_tests.ps1:120)。
  - 构建入口是 tools/scripts/third_party/build_grpc.ps1,其说明写着缺 cmake / ninja 时会经 winget 安装或升级。yaml-cpp 与 hiredis 要按 PROGRESS:749-750 另行构建。具体命令需 Codex 先确认。
- 5 个未初始化子模块是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash。Windows 源码里 grep 不到对它们的 include,但 xxhash / recastnavigation 出现在几个 vcxproj 的 include 目录里。是否影响构建,需 Codex 先确认。
- 两项解除后主干其余部分能否编过:未核实。
- 不满足时停下,不要自行做:`git submodule update`、第三方全量构建、在 third_party/boost 里生成头文件、改 vcxproj。这些都属 §10.2 的停止条件。
- 编译用哪棵树由负责人定:新工作树没有任何子模块,初始化就要下载;主工作区当前有他人在途改动。

以下 B1~B6 只在 B0 通过后执行。

B1 构建(pwsh,工作树根;必须 /m:1 串行;耗时未核实):
```powershell
$msbuild = & "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\MSBuild.exe' | Select-Object -First 1
& $msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
```
- 期望:退出码 0,产出 bin\scene.exe(scene.vcxproj:128-130 在构建后复制过去)。
- 失败时保留前 20 行 `error`。
- 出处:命令形式见 docs/PROGRESS.md:5569;MSBuild 定位见 tools/scripts/run_cpp_tests.ps1:86-89。

B2 起探针:跑 C1 的两条 `docker run`,再跑一次 C3。
- 期望此时没有 `errorstat_NOAUTH` 行(新容器)。

B3 探针配置(写在仓库外):
```powershell
$cfg = Join-Path $env:TEMP 'p0a_game_config.yaml'
(Get-Content bin/etc/game_config.yaml -Raw) -replace '(?m)^(\s*port:\s*)6379', '${1}6390' | Set-Content -Encoding utf8 $cfg
Select-String -Path $cfg -Pattern 'port:'
$env:GAME_CONFIG_PATH = $cfg
```
- 期望打印 `port: 6390`。
- 出处:node.cpp:1247-1258(覆盖生效时 :1255 打日志);bin/etc/game_config.yaml:22-25。
- 绝对路径可读:yaml-cpp 的 LoadFile 用 std::ifstream 打开文件(third_party/yaml-cpp/src/parse.cpp:32-38)。运行期以 B5 的日志行为准。

B4 启动 scene(pwsh,工作树根):
```powershell
$t0 = Get-Date
pwsh -File tools/scripts/cpp_nodes.ps1 -Command status
pwsh -File tools/scripts/cpp_nodes.ps1 -Command start -Nodes scene -SceneCount 1 -Zone 99 -NodeIp loopback
```
- 先看 status:PID 文件里已有 scene 实例时先问负责人。后面的 `stop -Nodes scene` 按基名过滤、不分 zone(cpp_nodes.ps1:455-459),会把它们一起停掉。
- `-Zone 99` 让节点以 zone 99 注册(:364)。
- 不启动 Go 服务。etcd / kafka 用平时的本机开发基础设施,不清数据。
- 启动后等约 15 秒。
- 出处:参数 :58-104;Node::Initialize 的顺序见 node.cpp:495-498。
- 未核实:etcd / kafka 不可达时 scene 会不会在 loop 开始前退出。

B5 观察:
```powershell
Get-ChildItem run/logs/cpp_nodes/z99_scene.*.log, bin/logs/cpp_nodes/* | Where-Object LastWriteTime -gt $t0 |
  Select-String -Pattern 'GAME_CONFIG_PATH env override applied|Zone Redis address|Redis connected to|Redis reconnected, retrying|SCRIPT LOAD (ok|failed) for PlayerAllData'
docker exec p0a-redis-auth redis-cli -a p0a-probe-pass-01 --no-auth-warning INFO errorstats
```
- 期望(缺陷成立)依次出现:
  - `GAME_CONFIG_PATH env override applied: <$cfg>`(node.cpp:1255);
  - `Zone Redis address: 127.0.0.1:6390`(node.cpp:556);
  - `Redis connected to 127.0.0.1:6390`(redis_manager.cpp:99);
  - `Redis reconnected, retrying pending player loads`(scene redis.cpp:30);
  - 两条 `SCRIPT LOAD failed for PlayerAllData (reply 6); will retry on next save`(redis_client.h:570-571)。两条来自 save / guard 两个脚本槽(:297)。
- errorstats 期望 `errorstat_NOAUTH:count=`,计数 ≥ 1(预期 2)。
- 类型号 6 即 REDIS_REPLY_ERROR(third_party/redis/deps/hiredis/read.h:56;链接的 hiredisd.lib 是否同一版本未核实)。判据只认到 `(reply` 为止,数字照实记录。
- 缺陷不成立:两条 `SCRIPT LOAD ok for PlayerAllData sha1=`(redis_client.h:575),且没有 NOAUTH。
- 没跑起来:缺前两行。交回 stdout / stderr。
- 日志位置:stdout 在 run\logs\cpp_nodes\z99_scene.stdout.log(cpp_nodes.ps1:351);滚动日志在 bin\logs\cpp_nodes\(:318),具体文件名需 Codex 先确认。
- 本机 LogLevel 2 按 muduo 枚举是 INFO(bin/etc/base_deploy_config.yaml:25 的注释写成 WARN,是错的),所以上面的 INFO 行都会输出。
- 保留:命中的日志行和 errorstats 输出。
- 对照组:
  1. `pwsh -File tools/scripts/cpp_nodes.ps1 -Command stop -Nodes scene`;
  2. 把 `$cfg` 里的端口改成 6391,重复 B4、B5,errorstats 查 p0a-redis-open,不带 -a;
  3. 期望两条 `SCRIPT LOAD ok …`,errorstats 里没有 NOAUTH。

B6 清理:
```powershell
pwsh -File tools/scripts/cpp_nodes.ps1 -Command stop -Nodes scene
docker rm -f p0a-redis-auth p0a-redis-open
Remove-Item Env:GAME_CONFIG_PATH; Remove-Item (Join-Path $env:TEMP 'p0a_game_config.yaml')
```
- 副作用:本机 etcd 里 zone 99 的 scene 注册最多残留一个租约周期(180s,bin/etc/base_deploy_config.yaml:6)。

【标注】
- 需下载:
  - Redis 镜像,仅当本机没有任何 redis 镜像时(C0 / B2),先征得同意;
  - B0 的解除(子模块、第三方构建、可能装 cmake / ninja)全部需负责人授权。
- 破坏性步骤:无。不清库,不重启现有基础设施。B6 的 stop 会停掉该树里所有 zone 的 scene,B4 已加前置检查。
- 耗时:
  - 步骤 0 加 A2、A3 约 15 分钟;A1 未核实;
  - C 段约 5 分钟;
  - B 段探测约 20 分钟,另加环境修复与一次全量构建(未核实)。
- 不涉及压测口径(AGENTS.md §6)。
- 建议顺序:步骤 0 → A2 → A3 → C →(A1 可选)。
- B 段等负责人决定环境修复。修复前的运行期红态可以在修复后的同一次构建里用黑盒第 0 组(不设口令连带口令探针)取得:空口令路径与修复前行为相同。

## P0-B K8s 上 battle 的 Kafka 生产者拿不到 broker


### 现在能跑 / 受阻(摘要)

**现在能跑**:P(pwsh --version)、1-0~1-4、0 现在就能跑:1-0 新建 2f897bd07 worktree,1-1/1-2 做 DryRun 渲染回答判据 A,1-3 跑契约测试基线,1-4 收尾,0 是只读环境清单。这几步都不编译、不连集群、不下载。另外,pre_measure_code 第 2 项那条可选的批 2 契约测试红用例,一旦负责人批准、写好,也能马上用 pwsh 跑。

**受阻**:

步骤 2(2-1~2-3,回答判据 B)受阻
- 阻塞原因:
  1. Boost 是模块化布局。third_party/boost 在 c89e626,根目录没有 boost/ 头文件树。infra.vcxproj:159 和测试工程 vcxproj:116-133 只挂了 third_party/boost/ 根,而 kafka_manager.cpp:4-5 要 muduo EventLoop.h(:18 需要 boost/any.hpp)和 boost/algorithm。结果必然 C1083,Codex 10-14 在 battle.vcxproj 上已实证(PROGRESS:7637)。原来的本地分叉 87db1d0d 和恢复分卷已随 10-13 的历史重写移除。
  2. third_party/grpc/install_vs2026_dbg 不存在。protobuf / gtest / abseil 等库和 zlibd.dll 都在这里;rdkafka.lib / rdkafka.dll 的来源仓库内找不到,需 Codex 先确认。
  3. lib\ 下没有任何 .lib。
  4. 红态代码还没写。
- 解除条件:步骤 0 全部 True,并且按 2f897bd07 生成的红补丁已就位。
- 由谁解除:
  - 1~3 需负责人先拍板授权,再由 Codex 修环境。这属于修改构建环境、长时间构建,可能下载,见 AGENTS §10.2。可用的入口有 dev_tools.ps1 -Command third-party-grpc-build(:1417 → tools/scripts/third_party/build_grpc.ps1);Boost 头文件树怎么补,仓库里没有现成脚本,需 Codex 先确认做法。
  - 第 4 项由 Claude 写,何时写由负责人定。
- 独立 GTest 路线绕不开:本项用例必须链接 KafkaManager / KafkaProducer 及其依赖,不是零依赖逻辑。

步骤 3(3-1~3-5,回答判据 C,可选旁证)受阻
- 阻塞原因:没有 battle.exe(bin\ 只有 README.md、etc、logs);编 battle 节点卡在同一个 Boost C1083;运行期 DLL 应在 install_vs2026_dbg\bin(start_game.ps1:551),但这个目录不存在。3-5 另外需要 gate / scene exe、robot.exe、Go 服务和 Java 网关 jar。
- 解除条件:battle 节点能编出,且步骤 0 里 battle.exe 和两个 DLL 都为 True。
- 由谁解除:负责人授权后由 Codex 修环境。建议负责人同意无限期延后,不要单独为它修环境。

### 步骤原文

【P0-B 实测任务单】

锚点:仓库 D:\luyuan\xuanming-server-mmo-cpp,提交 2f897bd07(完整哈希 2f897bd0780c1cc78284548284f20d01e61d7ea8)。下文的 file:line 都是这个提交的行号。

主工作区此刻有他人未提交改动(45 个文件,还在变)。所以:
- 渲染步骤一律在新建的 2f897bd07 worktree 里跑;
- 不在主工作区跑,也不在 Codex 的 f452(73b852f32)和 793e(0aa1027bc)worktree 里跑。

【要回答的问题】
A. K8s 渲染出的 battle-node-config 和 zone 的 node-config 里,Kafka.GroupID 是否非空?
B. GroupID 非空、进程又不注册任何消费回调(也就是 battle 的形态)时,KafkaManager::Init 之后本线程的生产者是否没有 broker?
C.(可选旁证)真实的 battle 进程按 K8s 同款 Kafka 段启动,日志能否印证 B?

【判据(三态;「没跑起来」既不算成立,也不算不成立)】
- A
  - 成立:两份 ConfigMap 的 Kafka.GroupID 都打印 game-consumer-group。
  - 不成立:为空。
  - 没跑起来:pwsh 不可用,或 DryRun 退出码非 0,或取不到 ConfigMap 块。
  - DisableLegacyPerNodeTopic 只记录,不进判据。
- B
  - 成立:KafkaManagerInit.ProducerBrokersConfiguredWhenGroupIdIsSet FAILED,同时对照用例 …WhenGroupIdIsEmpty OK。
  - 不成立:两条都 OK。
  - 没跑起来:编译或链接报 error;或汇总行是「退出码 …,无 gtest 输出」(-1073741515 = 0xC0000135,缺 DLL)。
- C
  - 成立:本次启动写出的 battle 日志里有 `KafkaManager: No message callback provided, skipping subscribe.`,而且没有 `KafkaProducer: brokers stored for lazy init:`。
  - 不成立:出现后一行。
  - 没跑起来:进程没起来,或本次启动没有新日志。

【综合判定】
- A 成立 + B 成立:缺陷成立,有运行证据,可以落修复。
- A 成立 + B 没跑起来(当前预期):只有「触发条件的运行证据 + C++ 行为的静态证据链」。是否据此落码由负责人拍板。Codex 不要为了跑 B 自行修构建环境、初始化子模块或下载依赖。
- A 不成立:当前模板不触发缺陷,停下报告。
- C 只是旁证。
- 全程不上 kind、不压测(AGENTS §6)、不清 Redis / MySQL / etcd / Kafka 数据。

【分组】
- 现在就能跑:P、1-0~1-4、0。
- 受阻:2(回答 B)、3(回答 C)。
- 本项没有可用的 docker 一次性容器探针或 Go 开发栈步骤:缺陷在 C++ 进程内部的生产者登记,Go 和 docker 侧都没有能观测它的地方。

════════ 现在就能跑 ════════

【P 前置检查】无破坏,不下载,不到 1 分钟。
```powershell
pwsh --version
```
- 期望:7.x。
- 没有 pwsh:步骤 1 记为「没跑起来」。原因:deploy_capture.ps1:1 写了 `#requires -Version 7`,而且子进程用 `& pwsh -NoProfile -NonInteractive -File` 启动(deploy_capture.ps1:78)。

【步骤 1 渲染模板(回答 A)】在 PowerShell 7 会话里执行。不编译、不连集群、不下载、无破坏。
- 预计耗时(未实测):1-0~1-2 约 1~3 分钟;1-3 约 5~15 分钟或更久(契约测试已从 1596 行涨到 1897 行,DryRun 从 25 处增到 27 处)。

1-0 新开检出(只在主仓库的 .git/worktrees 下写一条元数据):
```powershell
$repo = 'D:\luyuan\xuanming-server-mmo-cpp'
$wt   = Join-Path $env:TEMP 'p0b-render-2f897bd07'
git -C $repo worktree add --detach $wt 2f897bd07
git -C $wt rev-parse HEAD
git -C $wt status --short
```
- 期望:rev-parse 输出 `2f897bd0780c1cc78284548284f20d01e61d7ea8`;status 无输出。
- 如果 $wt 已存在:停下报告,不要覆盖。
- 新 worktree 不带子模块内容,步骤 1 也用不到。CI(.github/workflows/deploy-config-tests.yml:86)同样是不带子模块的 actions/checkout@v4。

1-1 battle-node-config:
```powershell
Set-Location $wt
. ./tools/scripts/tests/lib/test_harness.ps1
. ./tools/scripts/tests/lib/deploy_capture.ps1
# 一次性夹具值,不是任何真实密钥。生成器要求 battle 密钥去空白后 >= 32 字节,且与 gate 密钥不同。
$fixture = @{
  MMORPG_BATTLE_TOKEN_SECRET = 'p0b-fixture-battle-' + [guid]::NewGuid().ToString('N')
  MMORPG_GATE_TOKEN_SECRET   = 'p0b-fixture-gate-'   + [guid]::NewGuid().ToString('N')
}
$run = Invoke-DeployDryRun -Env $fixture -Arguments @('-Command','infra-up','-DryRun','-SkipPreflight','-SkipGoSvc','-SkipJavaSvc','-WaitReady','-InfraNamespace','contract-battle-infra','-NodeImage','registry.invalid/test/mmorpg-node:0123456789ab')
"infra-up exit=$($run.ExitCode)"
$cm = Select-ManifestByName -Output $run.Output -Name 'battle-node-config'
if ($null -eq $cm) { throw '没取到 battle-node-config 块:属于没跑起来,保留 $run.Output 的前 100 行上报' }
$flat = ConvertTo-FlatManifest -Block $cm
"battle-node-config Kafka.GroupID=[$(Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.Kafka.GroupID')]"
"battle-node-config LogLevel=[$(Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.LogLevel')]"
"battle-node-config has DisableLegacyPerNodeTopic=$($flat.Scalars.Contains('data.base_deploy_config.yaml.Kafka.DisableLegacyPerNodeTopic'))"
```
- 期望(缺陷成立):`infra-up exit=0`;`Kafka.GroupID=[game-consumer-group]`;`LogLevel=[1]`;最后一行 `False`(只记录)。
  - (2026-10-22 注)这是 10-14 的修前期望。现在 battle-node-config 的 `LogLevel` 从 bin/etc 镜像,重跑应为 `[2]`;GroupID 也早已改为镜像(契约测试有对应用例)。
- 出处:
  - 参数同契约测试 k8s_deploy_contract.tests.ps1:1149;HEAD 上用同样参数跑的 battle DryRun 在 :1151。
  - 四个取值函数在 tests/lib/deploy_capture.ps1:95-102、:147、:166、:180;键路径写法同契约测试 :1146、:1282。
  - ReleaseProfile 默认 dev(k8s_deploy.ps1:38);battle 密钥约束在 :2149-2160。
  - 模板常数:GroupID 在 :2262,LogLevel 在 :2222,Kafka 段 :2257-2266。battle-node-config 由 Apply-Infra 调用同一个生成函数(:6436-6437)。
  - DryRun 下只打印、不执行 kubectl::2035-2038、:2059-2069、:2087-2089。本区间新增的两处直接调 kubectl(:1509、:2856)在 DryRun 下先返回(:1503、:2849)。

1-2 zone 的 node-config,外加修复设计要用的一个事实:
```powershell
$zone = Invoke-DeployDryRun -Arguments @('-Command','zone-up','-ZoneName','contract-test','-ZoneId','101','-DryRun','-GoSvcRegistry','registry.invalid/test','-JavaSvcRegistry','registry.invalid/test')
"zone-up exit=$($zone.ExitCode)"
$zflat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $zone.Output -Name 'node-config')
"node-config Kafka.GroupID=[$(Get-FlatValue -Flat $zflat -KeyPath 'data.base_deploy_config.yaml.Kafka.GroupID')]"
"bin/etc Kafka.GroupID length=$((Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.GroupID').Length)"
```
- 期望:`zone-up exit=0`;`node-config Kafka.GroupID=[game-consumer-group]`;最后一行 `length=0`(证明空串能被权威取值函数读出来而不抛错,批 2 的镜像写法依赖这一点)。
- 出处:
  - 参数同契约测试 :33-40 的 $BaseArgs(基线调用在 :42);node-config 的取法同 :1144。
  - Get-EtcValue 在 deploy_capture.ps1:196-206。
  - 空串处理:release_common.ps1:53-63(去引号)、:124-130、:143-158;bin/etc/base_deploy_config.yaml:75。

1-3(可选)契约测试基线,给批 2 用:
```powershell
Set-Location $wt
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1 *> (Join-Path $env:TEMP 'p0b-contract-baseline.txt')
"contract exit=$LASTEXITCODE"
```
- 期望:不要求 exit=0。
  - 按静态阅读,2f897bd07 上至少「负向:zones.10zones.yaml 时打 MySQL连接总账超限 警告且不阻断」(:1649)会红:它断言中文「MySQL连接总账超限」(:1656),而脚本现在打的是 `[MySQLConnectionLedger]OverLimit:`(k8s_deploy.ps1:1238)。
  - prod 档用例也可能红(推断:主工作区有人正在给 $ProdEnv 补 MMORPG_LOGIN_QUEUE_TOKEN_SECRET)。
  - 这些都是 2f897bd07 已提交状态的既有问题,和本项无关:记下红用例名,继续,不在本项修。
- 判 A 不依赖 1-3。
- 出处:契约测试 :20-21 的 .EXAMPLE。

1-4 收尾:
```powershell
Set-Location $repo
git -C $repo worktree remove $wt
```
- 保留:1-1、1-2 打印的各行,以及 1-3 的输出文件。
- $run.Output 全文含一次性夹具值,不必留存。
- DryRun 过程中如果出现真实的 kubectl 报错或连集群的行为,立即停下报告。

【步骤 0 C++ 环境清单】只读,现在就能跑,决定步骤 2、3 能否解锁。不到 1 分钟。
```powershell
$b = '<可编译检出的根目录>'   # 需 Codex 先确认,见下面的说明
git -C $b rev-parse --short HEAD
git -C $b status --short
"boost_any_root=" + (Test-Path "$b\third_party\boost\boost\any.hpp")
"rdkafkacpp.h=" + (Test-Path "$b\third_party\librdkafka\src-cpp\rdkafkacpp.h")
"grpc_dbg_lib=" + (Test-Path "$b\third_party\grpc\install_vs2026_dbg\lib")
'infra','core','config','modules','scene','battle','gate','proto','proto_helpers','rpc','table','session','thread_context','muduo','grpc_client','third_party' | ForEach-Object { "$_.lib=" + (Test-Path "$b\lib\$_.lib") }
'rdkafka.dll','zlibd.dll' | ForEach-Object { "$_ bin=" + (Test-Path "$b\bin\$_") + " grpc_bin=" + (Test-Path "$b\third_party\grpc\install_vs2026_dbg\bin\$_") }
"battle.exe=" + (Test-Path "$b\bin\battle.exe")
```
- 关于 $b:10-14 的 battle 构建是在主工作区做的(lib\battle.pdb;PROGRESS:7637)。但主工作区此刻有他人的 C++ 半成品(scene.vcxproj、gate handler 等),拿它当构建检出会把别人的未完成代码一起编进去。构建检出放哪里由负责人定。
- 写本单时对主工作区的只读观察(以 Codex 的实际输出为准):
  - boost_any_root=False:Boost 在记录版本 c89e626,是模块化布局,头文件在 libs\any\include\boost\any.hpp;
  - rdkafkacpp.h=True;
  - grpc_dbg_lib=False;
  - 16 个 .lib 全是 False(lib\ 下只有 battle.pdb);
  - 两个 DLL 在两处都是 False;
  - battle.exe=False。
- 期望:全部 True 才能做步骤 2;battle.exe 和两个 DLL 为 True 才能做步骤 3。任何一项 False:停下,把输出交给负责人,不要自行补环境。
- 出处:库清单见 kafka_command_test.vcxproj:137-138;DLL 来源见 run_cpp_tests.ps1:120-121;Boost 判据见 third_party/muduo/muduo/net/EventLoop.h:18 和 infra.vcxproj:159(只挂了 third_party/boost/ 根)。

════════ 受阻 ════════

【步骤 2 C++ 红用例(回答 B)】受阻
阻塞原因:
1. Boost 布局。kafka_manager.cpp:4-5 包含 muduo/net/EventLoop.h 和 boost/algorithm/string/join.hpp;EventLoop.h:18 需要 boost/any.hpp;而 infra.vcxproj:159 和测试工程 kafka_command_test.vcxproj:116-133 只挂了 third_party/boost/ 根。结果必然是 C1083,Codex 10-14 已在 battle.vcxproj 上实证(PROGRESS:7637)。
2. install_vs2026_dbg 不存在。测试工程的库目录(:137)、protobuf / gtest / abseil 等库、以及 zlibd.dll 都在这里。rdkafka.lib / rdkafka.dll 的来源在仓库里找不到构建脚本,需 Codex 先确认。
3. lib\ 下没有任何依赖库。
4. 红态代码还没写(见 pre_measure_code 第 1 项)。

解除条件:步骤 0 全部 True,并且按 2f897bd07 生成的红补丁已就位。
- 1~3 属于修改构建环境、长时间构建,可能还要下载,Codex 须经负责人授权后才能做。
- 第 4 项由 Claude 写。

不能用独立 GTest 绕开:Codex 10-14 的 BattleAdmissionGate 7/7 测的是零依赖逻辑;本项的用例必须编译并链接 KafkaManager / KafkaProducer,一样会卡在第 1、2 项。

2-1 套红补丁($s 是放补丁的草稿目录):
```powershell
git -C $b apply --check "$s\p0b-red.patch"
git -C $b apply "$s\p0b-red.patch"
Copy-Item "$s\kafka_manager_init_test.cpp" "$b\cpp\tests\kafka_command_test\"
git -C $b status --short -- cpp tools/scripts/run_cpp_tests.ps1
```
- 期望:--check 无输出;最后一条命令在原有输出之外多出 3 个 ` M`(kafka_producer.h、kafka_command_test.vcxproj、run_cpp_tests.ps1)和 1 个 `??`。

2-2 编译(工作目录 $b;MSBuild 必须串行 /m:1,并发会报假的 C1041 / LNK1104):
```powershell
Set-Location $b
msbuild cpp\libs\engine\infra\infra.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\tests\kafka_command_test\kafka_command_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
```
- 期望:两次都 0 error,产物是 build\cpp\tests\kafka_command_test.exe。
- 出处:命令形式见 docs/PROGRESS.md:5341、:5686;产物目录见 vcxproj:80;MSBuild 候选位置见 run_cpp_tests.ps1:78-92。
- 测试工程没有 ProjectReference,靠 lib\ 下现成的 .lib 链接。
- 未核实:新用例第一次把 RdKafka C++ 包装层带进这个 exe,链接能否通过只能靠这一步确认。报 LNK 错误记为「没跑起来」,保留第一处 error,不要连续重试。

2-3 运行:
```powershell
pwsh tools/scripts/run_cpp_tests.ps1 -Filter kafka_command
.\build\cpp\tests\kafka_command_test.exe --gtest_filter=KafkaManagerInit.* --gtest_output=xml:"$s\p0b-red.xml"
dumpbin /dependents build\cpp\tests\kafka_command_test.exe | Select-String rdkafka
```
- 期望(缺陷成立):
  - 第 1 条的汇总行形如 `kafka_command_test  OK  N/M 过,挂:KafkaManagerInit.ProducerBrokersConfiguredWhenGroupIdIsSet, KafkaManagerInit.EmptyBrokersRejectedWhenGroupIdIsEmpty`,脚本退出码 1;
  - 第 2 条:…WhenGroupIdIsEmpty 和 EmptyBrokersRejectedWhenGroupIdIsSet OK,另外两条 FAILED;
  - 第 3 条:只出现 rdkafka.dll。
- 出处:汇总格式见 run_cpp_tests.ps1:184、:188;DLL 同步见 :120-129;缺 DLL 的表现见 :17-18、:166。
- 两条红用例的原因:
  - kafka_manager.cpp:57-63 走到 :76-79,在 :90 登记 broker 之前就返回 false;
  - :50-54:Brokers 为空时,空组分支仍然返回 true。
- 这几条用例只走 setBrokers,不建 librdkafka 句柄、不连 broker。析构时的 flush 在没有句柄时直接返回 true(kafka_producer.cpp:341-344)。
- 保留:msbuild 的完整输出、汇总表、gtest 控制台输出与 XML、dumpbin 那一行。跑完不要还原补丁。

【步骤 3 真实进程(回答 C,可选)】受阻
阻塞原因:
- 没有 battle.exe;
- 编 battle 卡在同一个 Boost C1083;
- 运行期 DLL 在 install_vs2026_dbg\bin(start_game.ps1:551),这个目录不存在;
- 3-5 另外还需要 gate / scene 的 exe、robot.exe、Go 服务和 Java 网关 jar。

解除条件:battle 节点能编出来,且步骤 0 里 battle.exe 和两个 DLL 都为 True。由 Codex 在负责人授权修环境之后做。Init 这段逻辑自 2026-06-01 起没变,手头有旧的 battle.exe 也可以用。

3-1 临时改配置(只在 $b 里改):
- 先把 bin\etc\base_deploy_config.yaml 复制到 $s,命名为 base_deploy_config.yaml.bak。
- 再改两行:
  - 第 75 行 `GroupID: ""` 改成 `GroupID: "game-consumer-group"`;
  - 第 25 行 `LogLevel: 2` 改成 `LogLevel: 1`(Windows 的枚举里 1 是 DEBUG)。
- 出处:k8s_deploy.ps1:2222、:2262;Logging.h:19-29(cpp/libs/engine/muduo_windows/src/muduo/base);node.cpp:1232。

3-2 只起 battle:
```powershell
$env:PATH = (Join-Path $b 'third_party\grpc\install_vs2026_dbg\bin') + ';' + $env:PATH
$env:RPC_PORT = '20010'
pwsh tools/scripts/dev_tools.ps1 -Command cpp-node-start -CppNodes battle -GateCount 0 -SceneCount 0 -BattleCount 1 -Zone 1 -NodeIp loopback
```
- 出处:start_game.ps1:551、:587-588;dev_tools.ps1:1449。
- node.cpp:497 早于 :498。推断:LoadAllConfigData 是否需要外部服务未核实,稳妥起见让本机的 docker 依赖保持运行。

3-3 只看本次启动写出的日志:
```powershell
$p = Get-Process battle | Sort-Object StartTime -Descending | Select-Object -First 1
Get-ChildItem bin\logs\cpp_nodes -Filter 'battle*.log' | Where-Object { $_.LastWriteTime -ge $p.StartTime } |
  Select-String -SimpleMatch -Pattern 'No message callback provided, skipping subscribe','brokers stored for lazy init','Init skipped: empty default group_id'
```
- 期望(缺陷成立):命中 `KafkaManager: No message callback provided, skipping subscribe.`,不命中 `KafkaProducer: brokers stored for lazy init:`。
- 对照(恢复配置后重启):命中 `KafkaManager::Init skipped: empty default group_id.` 和 `KafkaProducer: brokers stored for lazy init: localhost:9092`。
- 出处:kafka_manager.cpp:77、:46;kafka_producer.cpp:34;start_game.ps1:282-283。stderr 在 run\logs\cpp_nodes\z1_battle.stderr.log(start_game.ps1:260)。

3-4 停止并恢复:
```powershell
pwsh tools/scripts/dev_tools.ps1 -Command cpp-node-stop -CppNodes battle
Copy-Item "$s\base_deploy_config.yaml.bak" bin\etc\base_deploy_config.yaml -Force
git diff --quiet -- bin/etc/base_deploy_config.yaml; "restore_exit=$LASTEXITCODE"
```
- 期望:`restore_exit=0`。出处:dev_tools.ps1:1450。

3-5(更可选)端到端冒烟:
- 做法:保持 3-1 的改动起整栈,在 robot\ 目录下执行 `.\robot.exe -c etc/battle_smoke.yaml`。
- 期望(缺陷成立):
  - 输出 `BATTLE_SMOKE_FAIL step=… reason=…`;
  - battle 日志出现 `[Kafka] Produce rejected (message NOT queued): producer unavailable` 和 `battle outbound GateCommand send failed`(2f897bd07 上已英文化);
  - 同一次运行里 gate / scene 的登录进场景正常。
- 出处:robot/etc/battle_smoke.yaml:3-6;battle_smoke_scenario.go:124;kafka_producer.cpp:135;battle_room_manager.cpp:97。
- 需 Codex 先确认:start_game.ps1 的起栈参数;robot.exe、Go 服务、Java 网关 jar 是否都已构建。缺任何一个就跳过 3-5。
- 副作用:本机 Kafka 会留下消费组 game-consumer-group 和两个空 topic gate-<id> / scene-<id>(node_kafka_command_handler.h:124-136;kafka_consumer.cpp:126)。
- 收尾:跑完先 `pwsh tools/scripts/dev_tools.ps1 -Command dev-stop`(dev_tools.ps1:1484),再做 3-4。
- 保留:本次启动的 battle 日志全文、z1_battle.stderr.log、冒烟的控制台输出。

【下载 / 破坏性 / 耗时汇总】
- 下载:P、1、0 都不需要。解锁步骤 2、3 可能涉及下载和长时间构建,须负责人同意。
- 破坏性:无。
  - 1-0 在主仓库 .git/worktrees 写一条元数据,1-4 删掉;
  - 1-3 在 %TEMP% 写一个输出文件;
  - 步骤 3 临时改一份配置并恢复。
- 不做:kind、压测、清数据;不动主工作区、f452、793e。
- 耗时(都未实测):P 不到 1 分钟;1-0~1-2 约 1~3 分钟;1-3 约 5~15 分钟或更久;0 不到 1 分钟;2 在库齐全时约 5~15 分钟;3 约 10~20 分钟。

## P0-C etcd 没有 MVCC 历史压缩


### 现在能跑 / 受阻(摘要)

**现在能跑**:现在就能跑的是 A 组的 M0、M2、M3(探针 B、B2、A)、M4、清理和 S1;另有两步是条件执行:M1 要求开发容器 etcd 在跑,S2 要求 kind-mmorpg 集群已存在。全组不需要编译任何东西;前提是 pwsh 7.3+ 与 desktop-linux 可用,本机没有镜像时须先获同意 1。

**受阻**:

1. M5a(Go 开发栈采样):不需要 C++,卡在授权与前置确认。
   - 阻塞原因:要在新 worktree 里 go build login / scene_manager,可能下载 Go 模块;compose 会复用或新建开发依赖容器;开发 etcd 会被写入。
   - 解除条件:负责人给「同意 2a」;Codex 先确认这两个服务在没有 C++、Java、player_locator、data_service 时能起来,并完成槽位申领。
   - 由谁解除:负责人拍板,Codex 确认。
2. M5(整栈采样):受阻。
   - 阻塞原因:bin/ 下没有任何 exe,start_game.ps1 会在 :371、:379、:383 抛错;battle.vcxproj 编译报 C1083 找不到 boost/any.hpp(Boost 是模块化 include 布局,工程只包含根目录,PROGRESS.md:7637);third_party/grpc/install_vs2026_dbg 不存在(依赖库、DLL、protoc 35.1 都在这里);5 个子模块未初始化(是否必需未核实);缺 Java 网关 jar。
   - 解除条件:修好构建环境,并另开构建任务单,串行 /m:1 编 C++,再构建 Go 与 Java。
   - 由谁解除:负责人给「同意 2b」(修 Boost include 路径、重建 install_vs2026_dbg、初始化子模块,都涉及下载或改环境,AGENTS §10.2),由 Codex 修环境。本项结论不依赖 M5。
3. M6(开发 etcd 手工压缩并重启):C++ 部分受阻,原因同 M5;并且它不可逆。
   - 解除条件:负责人给「同意 3」,并与 P0-D 的实测合并成一次。Go 部分可以在 M5a 的栈上做,但同样需要同意 3。
   - 由谁解除:负责人拍板,Codex 执行。

### 步骤原文

# P0-C etcd 没有 MVCC 历史压缩 —— Codex 实测任务单(锚定 2f897bd07)

【谁执行】Codex 执行,结果原样交负责人贴回。Claude 不运行任何命令。

【锚点】
- 仓库 D:\luyuan\xuanming-server-mmo-cpp,提交 2f897bd07,完整哈希 2f897bd0780c1cc78284548284f20d01e61d7ea8,用 `git -C D:\luyuan\xuanming-server-mmo-cpp rev-parse 2f897bd07` 核对。
- A 组除 S1 外都不读仓库文件;S1 用 `git show 2f897bd07:<路径>` 读。
- 需要工作树的步骤只有 B 组 M5a,一律新建 worktree 到 2f897bd07。
- C:/Users/luyua/.codex/worktrees/f452 与 793e 两处不许动。主工作区不写任何文件。

【要回答的问题】
- Q1:bitnamilegacy/etcd:latest 不带任何压缩 env 时,是否自己做 MVCC 压缩?默认配额是多少?进程是否用配置文件启动?(M2)
- Q2:按真实键形写入时,每个 revision 占多少字节(b)?(M2)
- Q3:ETCD_AUTO_COMPACTION_* / ETCD_QUOTA_BACKEND_BYTES 在该镜像上是否生效?已有数据目录时重启是否仍生效?(M3 探针 B / B2)
- Q4:NOSPACE 下 put、含 put 的 txn、lease grant、keep-alive、del、get 各自成功还是失败?报错原文是什么?解除顺序里每一步之后 put 的表现?(M3 探针 A)
- Q5:压缩之后,带 prev_kv 的 watch 回放 DELETE 事件时,前值还在不在?(M4)
- Q6:真实开发栈的 revision/s 与写入键族,是否与静态盘点一致?(B 组 M5a / M5,延后)

【判据】
- 缺陷成立(四条同时满足):
  - 默认容器的进程参数、配置文件、启动日志里都没有非零的 auto-compaction;
  - 写入 1 万笔之后,旧 revision 仍可读;
  - 压缩 revision 指标不推进;
  - 配额指标约为 2.147e9。
- 缺陷不成立(任一条满足):启动日志或配置里有非零的 auto-compaction-retention;读旧 revision 报「已压缩」;压缩 revision 指标自行推进。
- 镜像忽略 env(修复方案要改):探针 B 的配额指标不是 3221225472,或者 7 分钟后旧 revision 仍可读。
- 只在空目录生效(阶段 2 与 compose 方案要改):探针 B 通过而探针 B2 不通过。
- 没跑起来:记「未执行」,既不算成立也不算不成立,保留输出后停下。包括以下情形:
  - pwsh 低于 7.3 或找不到;
  - docker 不可用,或没有 desktop-linux context;
  - 本机没有镜像且未获「同意 1」;
  - 临时容器起来 10 秒后 endpoint health 仍失败。

【0. 规则】
- 先确认 PowerShell 版本。在启动 shell 里跑 `pwsh --version`,再跑 `pwsh -NoProfile -Command '$PSVersionTable.PSVersion.ToString()'`(写法同 .github/workflows/deploy-config-tests.yml:98)。低于 7.3 或找不到就停下报告,不自行安装(装工具需授权,AGENTS §10.2)。理由(外部常识):Windows PowerShell 5.1 会丢掉空字符串参数,M1 的 `etcdctl get "" --prefix` 会变形;M5 用到的 `??` 只有 7 才有。
- 其余命令在同一个 pwsh 会话里从上往下执行。不用 Git Bash:以 `/` 开头的 etcd 键会被 MSYS 改写(出处 docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md:229)。
- docker:
  - 每条命令都显式带 `--context desktop-linux`(出处 tools/scripts/start_game.ps1:110-112);
  - 每条 `docker run` 都带 `--pull never`,不允许静默下载;
  - PATH 上没有 docker 时,可以用 start_game.ps1:364 列出的两处候选路径里实际存在的那个完整路径代替,并记录用了哪个;两处都没有就停下。
- A 组不需要编译,不需要工作树。输出一律写到仓库之外的 `$out`。
- 对名为 `etcd` 的开发容器只做只读操作。例外是 B 组的 M5a 和 M6,这两步须先获同意。
- 临时容器:名字一律以 `etcd-p0c-` 开头;宿主端口用 42379–42382,只绑 127.0.0.1,这是为了避开宿主 C++ 节点的 20000..35535(出处 deploy/k8s/kind-config.yaml:48-49)。端口绑定失败就换一个空闲端口并记录。
- 任何一步与期望不符:保留输出后停下,不重试,不自行换法。
- 需要负责人事先同意的事:
  - 同意 1:本机没有 bitnamilegacy/etcd:latest 时,是否从 Docker Hub 拉取(大小未核实);
  - 同意 2a:M5a(Go 构建可能下载模块;compose 会复用或新建开发依赖容器;开发 etcd 会被写入);
  - 同意 2b:M5 / M6 需要的 C++ 构建环境修复;
  - 同意 3:M6 在开发 etcd 上做不可逆压缩并重启。
- 破坏性:
  - A 组只创建并删除本单自己的临时容器与卷,对开发容器只读;
  - M5a 会让 Go 服务往开发 etcd 写入注册与水位,属于常规开发使用;
  - M6 不可逆。
- 预计耗时:M0 2 分钟;M1 3 分钟;M2 约 15 分钟;M3 约 30 分钟(含一次 7 分钟等待);M4 5 分钟;S1 1 分钟;S2 5 分钟;M5a 至少 40 分钟(含 30 分钟采样间隔,不含构建)。
- 标「凭记忆」的字段名、指标名、路径、报错文本,一律以实际输出为准;对不上就记录真实值。

====== A 组:现在就能跑(不需要编译任何东西) ======

【M0 环境与镜像】只读。[下载:只有在镜像不存在且获同意 1 时才会下载]
```powershell
$out = Join-Path $env:TEMP ("etcd-p0c-" + (Get-Date -Format 'yyyyMMdd-HHmmss'))
New-Item -ItemType Directory -Path $out | Out-Null; $out
git -C D:\luyuan\xuanming-server-mmo-cpp rev-parse 2f897bd07 | Tee-Object "$out\m0-anchor.txt"
docker context ls
docker --context desktop-linux info --format '{{.OSType}}|{{.ServerVersion}}'
docker --context desktop-linux image inspect bitnamilegacy/etcd:latest --format '{{.Id}}|{{.Created}}|{{json .RepoDigests}}' *>&1 | Tee-Object "$out\m0-image.txt"
docker --context desktop-linux image inspect bitnamilegacy/etcd:latest --format '{{json .Config}}' *>&1 | Tee-Object "$out\m0-image-config.json"
docker --context desktop-linux run --rm --pull never --entrypoint etcd bitnamilegacy/etcd:latest --version *>&1 | Tee-Object "$out\m0-etcd-version.txt"
docker --context desktop-linux run --rm --pull never --entrypoint etcdctl bitnamilegacy/etcd:latest version *>&1 | Tee-Object "$out\m0-etcdctl-version.txt"
docker --context desktop-linux run --rm --pull never --entrypoint sh bitnamilegacy/etcd:latest -c 'command -v etcd etcdctl sed grep tr head timeout; id' *>&1 | Tee-Object "$out\m0-tools.txt"
docker --context desktop-linux ps -a --filter 'name=^etcd$' --format '{{.Names}}|{{.Image}}|{{.Status}}|{{.Ports}}|{{.CreatedAt}}' *>&1 | Tee-Object "$out\m0-dev-etcd.txt"
```
- 期望:
  - rev-parse 输出 2f897bd0780c1cc78284548284f20d01e61d7ea8;
  - context 列表里有 desktop-linux;OSType 为 linux;
  - 镜像存在,并打印出 etcd 版本;
  - sh 里能找到 etcd、etcdctl、sed、grep、tr、head、timeout。
- 镜像不存在:停下,等「同意 1」。
- 从 m0-image.txt 取 RepoDigests 里的摘要,再做一次只读确认(把 DIGEST 换成实际值):
```powershell
docker --context desktop-linux image inspect "bitnamilegacy/etcd@sha256:DIGEST" --format '{{.Id}}'
```
  - 期望 Id 与上面相同,说明按摘要引用不需要下载。
  - RepoDigests 为空,或 Id 不同:记录下来,钉版本的写法要另议。
- 从 m0-image-config.json 记下三件事:PATH 是否含 etcdctl 所在目录;镜像自带的版本标签或环境变量(定 tag 用);默认用户。
- 出处:镜像名与容器名 deploy/docker-compose.yml:450-451;端口 :453;`info --format '{{.OSType}}'` 的写法 start_game.ps1:401。

【M1 开发 etcd 的即时判据】只读。只在 M0 最后一条显示容器 `etcd` 为 Up 时做;容器不存在或没在跑就跳过,不算失败。
```powershell
docker --context desktop-linux inspect etcd --format '{{.Created}}|{{.Image}}|{{json .Mounts}}' *>&1 | Tee-Object "$out\m1-inspect.txt"
docker --context desktop-linux inspect etcd --format '{{json .Config.Env}}' *>&1 | Tee-Object "$out\m1-env.json"
docker --context desktop-linux exec etcd etcdctl endpoint status -w json *>&1 | Tee-Object "$out\m1-status.json"
docker --context desktop-linux exec etcd etcdctl alarm list *>&1 | Tee-Object "$out\m1-alarm.txt"
docker --context desktop-linux exec etcd etcdctl get "" --prefix --keys-only *>&1 | Set-Content "$out\m1-keys.txt"
docker --context desktop-linux exec etcd etcdctl get p0c_probe_key --rev=1 *>&1 | Tee-Object "$out\m1-rev1.txt"; "exit=$LASTEXITCODE" | Add-Content "$out\m1-rev1.txt"
docker --context desktop-linux logs etcd 2>&1 | Select-String -Pattern 'auto-compaction|quota|compact' | Select-Object -First 60 | Out-File "$out\m1-log.txt"
(Invoke-WebRequest -UseBasicParsing 'http://127.0.0.1:2379/metrics').Content | Set-Content "$out\m1-metrics.txt"
Select-String -Path "$out\m1-metrics.txt" -Pattern '^etcd_(debugging_)?mvcc_(compact_revision|current_revision|keys_total|db_total_size)|^etcd_server_quota_backend_bytes'
```
- 先核对挂载里有 compose 的 etcd-data 卷(出处 docker-compose.yml:475、:491),确认采到的是开发栈那个容器。
- 历史在累积的迹象:
  - revision 远大于键数;
  - `--rev=1` 退出码 0,且没有「compacted」字样;
  - 压缩 revision 指标不大于 0;
  - dbSize 明显大于按键数估算的大小。
- revision 很小(容器刚建)时,这一步没有结论,以 M2、M3 为准。
- 出处:
  - `etcdctl get "" --prefix` 的写法:tools/scripts/stress_round19.ps1:218、incident 文档 :226;
  - JSON 输出开关:start_game.ps1:270 用的是 `--write-out=json`,与 `-w json` 是同一开关(外部常识);
  - 「2379 提供 /metrics」是外部常识,返回 404 就跳过指标。

【M2 默认容器:进程参数、默认值、每 revision 字节数】回答 Q1、Q2。
```powershell
docker --context desktop-linux run -d --pull never --name etcd-p0c-c -p 127.0.0.1:42381:2379 -e ALLOW_NONE_AUTHENTICATION=yes bitnamilegacy/etcd:latest
Start-Sleep 10
docker --context desktop-linux exec etcd-p0c-c etcdctl endpoint health

$sh = @'
echo "## cmdline"; for p in /proc/[0-9]*/cmdline; do tr '\0' ' ' < "$p"; echo; done
echo "## env"; env | grep -E '^(ETCD|ETCDCTL|ALLOW)_' | sort
echo "## conf"; ls -la /opt/bitnami/etcd/conf 2>&1; cat /opt/bitnami/etcd/conf/* 2>&1
'@ -replace "`r", ""
[IO.File]::WriteAllText("$out\inspect.sh", $sh)
docker --context desktop-linux cp "$out\inspect.sh" etcd-p0c-c:/tmp/inspect.sh
docker --context desktop-linux exec etcd-p0c-c sh /tmp/inspect.sh *>&1 | Tee-Object "$out\m2-inspect.txt"
docker --context desktop-linux logs etcd-p0c-c 2>&1 | Select-String -Pattern 'auto-compaction|quota|compact' | Out-File "$out\m2-log.txt"

$sh = @'
N=${N:-10000}
i=0
while [ "$i" -lt "$N" ]; do
  v=$((1791000000 + i))
  printf '\nput /snowflake/scene-manager/c0/watermark/1 %s\nput /scene_manager/snowflake_guard/1 %s\n\n\n' "$v" "$v" | etcdctl txn >/dev/null 2>/tmp/synth.err || { echo "txn failed at i=$i"; cat /tmp/synth.err; exit 1; }
  i=$((i+1))
done
echo "txns=$i"
'@ -replace "`r", ""
[IO.File]::WriteAllText("$out\synth.sh", $sh)
docker --context desktop-linux cp "$out\synth.sh" etcd-p0c-c:/tmp/synth.sh
docker --context desktop-linux exec etcd-p0c-c etcdctl endpoint status -w json | Set-Content "$out\m2-s0.json"
docker --context desktop-linux exec -e N=20 etcd-p0c-c sh /tmp/synth.sh
docker --context desktop-linux exec etcd-p0c-c etcdctl get /snowflake/scene-manager/c0/watermark/1 -w json
docker --context desktop-linux exec etcd-p0c-c etcdctl endpoint status -w json | Set-Content "$out\m2-s1.json"
docker --context desktop-linux exec -e N=10000 etcd-p0c-c sh /tmp/synth.sh
docker --context desktop-linux exec etcd-p0c-c etcdctl endpoint status -w json | Set-Content "$out\m2-s2.json"

$a=(Get-Content "$out\m2-s1.json" -Raw|ConvertFrom-Json)[0].Status; $b=(Get-Content "$out\m2-s2.json" -Raw|ConvertFrom-Json)[0].Status
$dr=$b.header.revision-$a.header.revision
"dRev=$dr dDbSize=$($b.dbSize-$a.dbSize) dInUse=$($b.dbSizeInUse-$a.dbSizeInUse) bytesPerRev=$([math]::Round(($b.dbSizeInUse-$a.dbSizeInUse)/$dr,1))" | Tee-Object "$out\m2-summary.txt"
$r1=$a.header.revision
docker --context desktop-linux exec etcd-p0c-c etcdctl get /snowflake/scene-manager/c0/watermark/1 --rev=$r1 *>&1 | Tee-Object "$out\m2-oldrev.txt"; "exit=$LASTEXITCODE" | Add-Content "$out\m2-oldrev.txt"
(Invoke-WebRequest -UseBasicParsing 'http://127.0.0.1:42381/metrics').Content | Set-Content "$out\m2-metrics.txt"
Select-String -Path "$out\m2-metrics.txt" -Pattern '^etcd_(debugging_)?mvcc_(compact_revision|current_revision|db_total_size)|^etcd_server_quota_backend_bytes'
```
- 先跑 20 笔,是为了确认 txn 的 stdin 格式:期望 revision 恰好增加 20,那把键的 version 为 20。
- revision 增加 40 或 txn 报错:说明格式不对,停下报告。备选写法是每轮两次 put,此时 b 按「每个键版本」解释。
- 期望(缺陷成立):
  - m2-inspect.txt 的进程参数与配置文件里没有非零 auto-compaction;关注是否带 `--config-file`;
  - m2-oldrev.txt 读到旧值,退出码 0;
  - 压缩 revision 指标不大于 0,配额指标约 2.147e9;
  - bytesPerRev 给出 b(每个 revision 含新、旧两把键各一个版本)。
- 保留:$out 下 m2-* 全部文件。
- 出处:
  - 键形:go/shared/snowflakealloc/allocator.go 的 rootKey :186、watermarkPrefix :206、watermarkKey :213、legacyGuardPrefix :224、legacyGuardKey :240;
  - kind 与旧前缀:go/scene_manager/internal/svc/servicecontext.go:90-93;
  - 值为十进制秒:allocator.go:1375;一笔 Txn 写两把键:allocator.go:1379-1390。
- 凭记忆的项:配置目录 /opt/bitnami/etcd/conf;JSON 字段名(Status.header.revision、dbSize、dbSizeInUse);指标名。
- 局限:这一步测不出「有没有盘点之外的写入方」,那要靠 M5a / M5。

【外推公式】推算,不是实测结论。
- revision/s(Z) = Z·(2·R_login + R_sm) + 0.1·N_gate总 + R_match(若部署 guild,再加 R_guild)。K8s 默认档约 6.2·Z + 2。
- 字节/天 ≈ 86400 × revision/s(Z) × b。gate 的 Put 带整份 NodeInfo JSON,比水位大,但只占 revision 的约 3%,误差按不超过 +20% 计。
- 到配额的天数 = (Q − 当前 dbSize) ÷ 字节/天,Q 取实测的配额指标。
- 开启压缩后的常态占用 ≈ KEEP_REVISIONS × b + 现存键。
- 速率出处:
  - 副本数:deploy/k8s/manifests/go-svc 下 login.yaml:24、scene-manager.yaml:24、match.yaml:36 的 `replicas: 2`;
  - gate 副本:tools/scripts/k8s_deploy.ps1:76 `GateReplicas = 2`;
  - 全局池标记:k8s_deploy.ps1:1351 / :1353(login / scene-manager 不带 Global)、:1356(match 带 Global);
  - 写入节拍:allocator.go:926、:972、:1005;login.go:263-272;cpp/nodes/gate/main.cpp:408-411。

【M3 探针 B / B2 / A】回答 Q3、Q4。顺序固定:B 与 B2 不通过,就不做 A。
```powershell
$sh = @'
i=0; while [ "$i" -lt 500 ]; do etcdctl put p0c/k$i v >/dev/null || exit 1; i=$((i+1)); done; echo "wrote=$i"
'@ -replace "`r", ""
[IO.File]::WriteAllText("$out\fill500.sh", $sh)

# 探针 B:空目录 + env
docker --context desktop-linux run -d --pull never --name etcd-p0c-b -p 127.0.0.1:42379:2379 -e ALLOW_NONE_AUTHENTICATION=yes -e ETCD_AUTO_COMPACTION_MODE=revision -e ETCD_AUTO_COMPACTION_RETENTION=100 -e ETCD_QUOTA_BACKEND_BYTES=3221225472 bitnamilegacy/etcd:latest
# 探针 B2 第一段:不带 env,数据写进卷
docker --context desktop-linux volume create etcd-p0c-vol
docker --context desktop-linux run -d --pull never --name etcd-p0c-b2 -p 127.0.0.1:42380:2379 -v etcd-p0c-vol:/bitnami/etcd/data -e ALLOW_NONE_AUTHENTICATION=yes bitnamilegacy/etcd:latest
Start-Sleep 10
foreach ($c in 'etcd-p0c-b','etcd-p0c-b2') {
  docker --context desktop-linux cp "$out\fill500.sh" "${c}:/tmp/fill500.sh"
  docker --context desktop-linux cp "$out\inspect.sh" "${c}:/tmp/inspect.sh"
  docker --context desktop-linux exec $c sh /tmp/fill500.sh
}
# 探针 B2 第二段:同一个卷,带 env 重起
docker --context desktop-linux stop etcd-p0c-b2; docker --context desktop-linux rm etcd-p0c-b2
docker --context desktop-linux run -d --pull never --name etcd-p0c-b2 -p 127.0.0.1:42380:2379 -v etcd-p0c-vol:/bitnami/etcd/data -e ALLOW_NONE_AUTHENTICATION=yes -e ETCD_AUTO_COMPACTION_MODE=revision -e ETCD_AUTO_COMPACTION_RETENTION=100 -e ETCD_QUOTA_BACKEND_BYTES=3221225472 bitnamilegacy/etcd:latest
Start-Sleep 10
docker --context desktop-linux cp "$out\inspect.sh" etcd-p0c-b2:/tmp/inspect.sh
foreach ($p in @{c='etcd-p0c-b';port=42379}, @{c='etcd-p0c-b2';port=42380}) {
  docker --context desktop-linux exec $p.c sh /tmp/inspect.sh *>&1 | Set-Content "$out\m3-$($p.c)-inspect.txt"
  docker --context desktop-linux exec $p.c etcdctl get p0c/k0 *>&1 | Tee-Object "$out\m3-$($p.c)-k0.txt"
  (Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$($p.port)/metrics").Content | Select-String -Pattern '^etcd_server_quota_backend_bytes' | Tee-Object "$out\m3-$($p.c)-quota.txt"
}
Start-Sleep 420
foreach ($p in @{c='etcd-p0c-b';port=42379}, @{c='etcd-p0c-b2';port=42380}) {
  docker --context desktop-linux exec $p.c etcdctl get p0c/k0 --rev=2 *>&1 | Tee-Object "$out\m3-$($p.c)-rev2.txt"; "exit=$LASTEXITCODE" | Add-Content "$out\m3-$($p.c)-rev2.txt"
  (Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$($p.port)/metrics").Content | Select-String -Pattern 'compact_revision' | Tee-Object "$out\m3-$($p.c)-compact.txt"
  docker --context desktop-linux logs $p.c 2>&1 | Select-String -Pattern 'compact' | Select-Object -Last 10 | Out-File "$out\m3-$($p.c)-log.txt"
}
```
- B、B2 的通过标准相同:
  - 配额指标为 3.221225472e+09;
  - k0 仍可读(对 B2 来说,这证明旧数据还在);
  - 约 7 分钟后 `--rev=2` 报「已压缩」,且退出码非 0;
  - 压缩 revision 指标大于 0。
- B 不通过:该镜像忽略这些 env。B 通过而 B2 不通过:只在空目录生效。两种情况都停下报告,阶段 2 与 compose 的注入方式另议。
- 保留「已压缩」报错的原文,CronJob 脚本要按它做匹配。
- 出处:
  - 卷挂载点:docker-compose.yml:475;
  - 「已有数据时 initial-* 启动参数被忽略」的先例:deploy/k8s/manifests/infra/etcd.yaml:143-145;
  - 外部常识:「revision 模式约每 5 分钟压一次」「单节点只给 ALLOW_NONE_AUTHENTICATION 就能起」。起不来就照 docker-compose.yml:456-473 补齐 env,并记录。

探针 A(NOSPACE 与解除):
```powershell
docker --context desktop-linux run -d --pull never --name etcd-p0c-a -p 127.0.0.1:42382:2379 -e ALLOW_NONE_AUTHENTICATION=yes -e ETCD_QUOTA_BACKEND_BYTES=16777216 bitnamilegacy/etcd:latest
Start-Sleep 10
(Invoke-WebRequest -UseBasicParsing 'http://127.0.0.1:42382/metrics').Content | Select-String -Pattern '^etcd_server_quota_backend_bytes'
```
配额指标不是 1.6777216e+07 就停下,不要往下灌。
```powershell
$sh = @'
etcdctl lease grant 600 | tee /tmp/lease_grant.txt
sed -n 's/^lease \([0-9a-f]*\) granted.*/\1/p' /tmp/lease_grant.txt > /tmp/lease_id
v=$(head -c 3000 /dev/zero | tr '\0' x)
i=0
while [ "$i" -lt 20000 ]; do
  etcdctl put p0c/big "$v$i" >/dev/null 2>/tmp/err || break
  i=$((i+1))
done
echo "wrote=$i"; cat /tmp/err
[ "$i" -lt 20000 ] || echo "CAP_REACHED_NO_ERROR"
'@ -replace "`r", ""
[IO.File]::WriteAllText("$out\fill.sh", $sh)
$sh = @'
run() { echo "### $*"; "$@"; echo "exit=$?"; }
LID=$(cat /tmp/lease_id)
run etcdctl alarm list
run etcdctl endpoint status -w json
run etcdctl put p0c/nospace-put x
echo "### txn(put)"; printf '\nput p0c/nospace-txn x\n\n\n' | etcdctl txn; echo "exit=$?"
run etcdctl lease grant 60
run etcdctl lease keep-alive --once "$LID"
run etcdctl get p0c/big --keys-only
run etcdctl del p0c/none
REV=$(etcdctl get p0c/probe -w json | sed -n 's/.*"revision":\([0-9][0-9]*\).*/\1/p'); echo "REV=$REV"
run etcdctl compact "$REV"
run etcdctl compact "$REV"
run etcdctl put p0c/after-compact x
run etcdctl endpoint status -w json
run etcdctl defrag
run etcdctl endpoint status -w json
run etcdctl put p0c/before-disarm x
run etcdctl alarm disarm
run etcdctl alarm list
run etcdctl put p0c/after-disarm x
run etcdctl endpoint status -w json
'@ -replace "`r", ""
[IO.File]::WriteAllText("$out\nospace.sh", $sh)
docker --context desktop-linux cp "$out\fill.sh" etcd-p0c-a:/tmp/fill.sh
docker --context desktop-linux cp "$out\nospace.sh" etcd-p0c-a:/tmp/nospace.sh
docker --context desktop-linux exec etcd-p0c-a sh /tmp/fill.sh *>&1 | Tee-Object "$out\m3-a-fill.txt"
docker --context desktop-linux exec etcd-p0c-a sh /tmp/nospace.sh *>&1 | Tee-Object "$out\m3-a-nospace.txt"
```
- 出现 `CAP_REACHED_NO_ERROR`:2 万次写入仍没报错,停下报告。
- 要回答,并原样保留(写 runbook 用):
  - NOSPACE 下 put、含 put 的 txn、lease grant、keep-alive、get、del 各自的输出与退出码;
  - alarm list 的原文;
  - 第二次 compact 同一 revision 的报错原文;
  - 压缩后、defrag 后、disarm 前、disarm 后四个时刻 put 的表现;
  - defrag 前后的 dbSize 与 dbSizeInUse。
- 出处:Go 水位走 Txn(allocator.go:1383);C++ 注册走 Txn(cpp/libs/engine/core/node/system/etcd/etcd_helper.cpp:94、:108)。「16MiB 配额可被接受」与 txn 的 stdin 格式是外部常识。

【M4 探针 D:压缩后 prev_kv 是否丢失】回答 Q5。可选,用 M2 留下的容器 etcd-p0c-c。
```powershell
$sh = @'
etcdctl put p0c/d/node '{"node_uuid":"u1"}' >/dev/null
etcdctl put p0c/d/pad x >/dev/null
etcdctl del p0c/d/node >/dev/null
REV=$(etcdctl get p0c/d/none -w json | sed -n 's/.*"revision":\([0-9][0-9]*\).*/\1/p'); echo "delete_rev=$REV"
echo "## before compact"; timeout 5 etcdctl watch p0c/d/node --rev="$REV" --prev-kv -w json; echo "exit=$?"
etcdctl compact "$REV"
echo "## after compact, from delete_rev"; timeout 5 etcdctl watch p0c/d/node --rev="$REV" --prev-kv -w json; echo "exit=$?"
echo "## after compact, from delete_rev-1"; timeout 5 etcdctl watch p0c/d/node --rev="$((REV-1))" --prev-kv -w json; echo "exit=$?"
'@ -replace "`r", ""
[IO.File]::WriteAllText("$out\prevkv.sh", $sh)
docker --context desktop-linux cp "$out\prevkv.sh" etcd-p0c-c:/tmp/prevkv.sh
docker --context desktop-linux exec etcd-p0c-c sh /tmp/prevkv.sh *>&1 | Tee-Object "$out\m4-prevkv.txt"
```
- 要回答三件事:
  - 压缩前,DELETE 事件带不带前值;
  - 压缩到删除所在 revision 之后,同一个 watch 的 DELETE 事件还带不带前值;
  - 从更早的 revision 建 watch 时返回什么(是否带压缩 revision 的取消应答)。
- watch 被 timeout 结束时退出码为 124,属正常。
- 出处:
  - C++ 建 watch 时要求前值:etcd_helper.cpp:50;
  - 摘除节点靠前值:etcd_service.cpp:388、node.cpp:1339-1345(解析失败的日志在 :1342);
  - 契约:proto/etcd/etcd.proto:400-401、:444-452。

【清理】只删本单创建的临时容器与卷。
```powershell
docker --context desktop-linux rm -f -v etcd-p0c-a etcd-p0c-b etcd-p0c-b2 etcd-p0c-c
docker --context desktop-linux volume rm etcd-p0c-vol
docker --context desktop-linux ps -a --filter 'name=etcd-p0c-'
```
最后一条应无输出。`$out` 目录整个保留,并回报路径。

【S1 K8s 清单与 compose 静态核对】替代部署渲染,只读。
为什么不运行 k8s_deploy.ps1:Apply-Infra 对 etcd.yaml 只做一次占位替换,不注入 env(k8s_deploy.ps1:6537;Apply-Infra 在 :6432-6594);而 DryRun 会把 Secret 明文打印出来(:6503 注释)。所以读静态文件就够。
```powershell
git -C D:\luyuan\xuanming-server-mmo-cpp show 2f897bd07:deploy/k8s/manifests/infra/etcd.yaml | Select-String -Pattern 'bitnamilegacy/etcd|ETCD_AUTO_COMPACTION|ETCD_QUOTA|kind: CronJob' | Tee-Object "$out\s1-etcd-yaml.txt"
git -C D:\luyuan\xuanming-server-mmo-cpp show 2f897bd07:deploy/docker-compose.yml | Select-String -Pattern 'bitnamilegacy/etcd|ETCD_AUTO_COMPACTION|ETCD_QUOTA' | Tee-Object "$out\s1-compose.txt"
git -C D:\luyuan\xuanming-server-mmo-cpp diff --quiet 2f897bd07 -- deploy/k8s/manifests/infra/etcd.yaml deploy/docker-compose.yml; "exit=$LASTEXITCODE" | Tee-Object "$out\s1-worktree-same.txt"
```
- 期望:
  - etcd.yaml 恰好命中 2 行::111(注释)和 :114(`image: bitnamilegacy/etcd:latest`);
  - compose 恰好命中 1 行::450;
  - diff 退出码 0,即主工作区这两份文件与 2f897bd07 相同(M5a 会直接用主工作区的 compose 文件路径)。

【S2 kind 只读核对】可选。只在名为 kind-mmorpg 的集群已经存在时做;不为此建集群,不装工具。每条命令都带 context 与 kubeconfig(出处 deploy/k8s/kind-config.yaml:12-21)。
```powershell
$kc = "$env:TEMP\kind-mmorpg.kubeconfig"
kubectl --context kind-mmorpg --kubeconfig $kc get node
kubectl --context kind-mmorpg --kubeconfig $kc get ns
kubectl --context kind-mmorpg --kubeconfig $kc -n mmorpg-infra get pod -l app=etcd -o wide
kubectl --context kind-mmorpg --kubeconfig $kc -n mmorpg-infra get pod -l app=etcd -o jsonpath='{.items[*].status.containerStatuses[0].imageID}'
kubectl --context kind-mmorpg --kubeconfig $kc -n mmorpg-infra exec etcd-0 -- etcdctl endpoint status --cluster -w table
kubectl --context kind-mmorpg --kubeconfig $kc -n mmorpg-infra exec etcd-0 -- etcd --version
```
- 第一条不通就停,不要去掉 `--context` 再试(本机默认 context 是别的项目的集群,kind-config.yaml:15)。
- namespace 以 `get ns` 的实际结果为准(`mmorpg-infra` 出自 deploy/k8s/README.md:461-462)。
- 目的:看三个成员的 imageID 是否相同,是否与本机 compose 的镜像是同一摘要。

====== B 组:受阻或需授权 ======

【M5a Go 开发栈采样】回答 Q6 的 Go 部分。
- 状态:技术上不需要编译 C++,但需要「同意 2a」,而且以下两点需 Codex 先确认。
- 需 Codex 先确认:
  - (1) 没有 C++ 节点、Java 网关、player_locator、data_service 时,login 与 scene_manager 能否起来,并完成 snowflake 槽位申领。看 run\logs\go_services 下各自的 stdout / stderr(go_services.ps1:87、:662-663);申领成功日志的原文由 Codex 从日志里确认。
  - (2) 依赖容器够不够。start_game.ps1:446 另外还起了 redis-cluster-* 与 redis-cluster-init;Kafka 主题预建在 start_game.ps1:182 的 Initialize-LocalKafkaTopics 里,本单不调用。
  - 起不来就停下回报,不自行补服务。
- [下载:Go 模块可能下载] [破坏性:开发 etcd 被写入;compose 可能新建依赖容器与卷] [耗时:不少于 40 分钟,不含构建]
```powershell
$wt = Join-Path $env:TEMP 'etcd-p0c-go'
git -C D:\luyuan\xuanming-server-mmo-cpp worktree add --detach $wt 2f897bd07
git -C $wt rev-parse HEAD      # 期望 2f897bd0780c1cc78284548284f20d01e61d7ea8
docker --context desktop-linux compose -f D:\luyuan\xuanming-server-mmo-cpp\deploy\docker-compose.yml --profile redis-cluster up -d --no-recreate --pull never etcd redis mysql kafka
pwsh -File (Join-Path $wt 'tools\scripts\go_services.ps1') -Command start -Services login,scene_manager
# 两个服务都申领成功后:定义 M5 的 Save-EtcdSnap,执行 Save-EtcdSnap $out 't0';不少于 30 分钟后执行 Save-EtcdSnap $out 't1',再跑 M5 的统计段
pwsh -File (Join-Path $wt 'tools\scripts\go_services.ps1') -Command stop
git -C D:\luyuan\xuanming-server-mmo-cpp worktree remove --force $wt
```
- 期望:
  - revision/s ≈ 2·N_login + N_scene_manager,各起 1 个时约 3/s(±15%);
  - Delta 不为 0 的键只落在水位族:`/snowflake/<kind>/c<cluster>/watermark/…` 与 `…/watermark_ms/…`;ClusterId 为 0 时还有旧前缀镜像 `/login/snowflake_guard/…`、`/login/guard_ms/…`、`/scene_manager/snowflake_guard/…`(allocator.go:182、:206-246);
  - 清单外的键、负的 Delta,都原样上报。
- 出处:compose 的写法 start_game.ps1:445-446;go_services.ps1 的用法 :20-31;worktree 的写法同 godb 交接 :124。
- 局限:没有 gate,测不到 gate 每秒 0.1 笔的写入(约占 3%)。

【M5 整栈两次采样】回答完整的 Q6。受阻。
- 阻塞原因:
  1. bin/ 下没有任何 exe。主工作区与两个 Codex worktree 都只有 README.md、etc、logs。start_game.ps1 会在 :371(缺 Java 网关 jar)、:379(缺非可选 Go 服务 exe)、:383(缺 C++ 节点 exe)直接抛错。
  2. C++ 节点编不出来。2026-10-14 串行 `/m:1` 编译 cpp/nodes/battle/battle.vcxproj Debug|x64 时,在 third_party/muduo/muduo/net/EventLoop.h:18 报 C1083,找不到 boost/any.hpp(docs/PROGRESS.md:7637)。原因是 Boost 为模块化 include 布局,而工程只包含根目录。gate / scene 同样经 muduo 引入它,推断同样受阻,未核实。
  3. third_party/grpc 源码已检出,但 third_party/grpc/install_vs2026_dbg 不存在。C++ Debug 的依赖库、运行期 DLL 与 protoc 35.1 都装在这里(PROGRESS.md:750、:4186、:4206、:5997)。
  4. 5 个子模块未初始化(cppcodec、fmt、muduo-linux、recastnavigation、xxhash);是否为 Windows 节点构建所必需,未核实。
- 解除条件:
  - 负责人给「同意 2b」,即修 Boost include 布局、重建 install_vs2026_dbg、初始化子模块(都涉及下载或改构建环境,AGENTS §10.2);
  - 再另开构建任务单,定 C++ MSBuild 串行 `/m:1`、Go、Java jar 的顺序与依赖下载;本单不发明构建步骤;
  - 本项结论不依赖 M5。
- 栈起来之后的采样:t0 与 t1 间隔不少于 30 分钟,各调用一次。
```powershell
function Save-EtcdSnap([string]$Dir,[string]$Tag) {
  (Get-Date).ToUniversalTime().ToString('o') | Set-Content "$Dir\$Tag.time.txt"
  docker --context desktop-linux exec etcd etcdctl endpoint status -w json | Set-Content "$Dir\$Tag.status.json"
  docker --context desktop-linux exec etcd etcdctl get "" --prefix -w json | Set-Content "$Dir\$Tag.kv.json"
  (Invoke-WebRequest -UseBasicParsing 'http://127.0.0.1:2379/metrics').Content | Set-Content "$Dir\$Tag.metrics.txt"
}
Save-EtcdSnap $out 't0'    # 30 分钟后:Save-EtcdSnap $out 't1'
function Load($f){ (Get-Content $f -Raw|ConvertFrom-Json).kvs | % { [pscustomobject]@{Key=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($_.key));Version=[int64]$_.version} } }
$s0=(Get-Content "$out\t0.status.json" -Raw|ConvertFrom-Json)[0].Status; $s1=(Get-Content "$out\t1.status.json" -Raw|ConvertFrom-Json)[0].Status
$dt=([datetime](Get-Content "$out\t1.time.txt")-[datetime](Get-Content "$out\t0.time.txt")).TotalSeconds; $dr=$s1.header.revision-$s0.header.revision
"dt=$dt dRev=$dr revPerSec=$([math]::Round($dr/$dt,2)) dInUse=$($s1.dbSizeInUse-$s0.dbSizeInUse)"
$a=@{}; Load "$out\t0.kv.json" | % { $a[$_.Key]=$_.Version }
Load "$out\t1.kv.json" | % { [pscustomobject]@{Delta=$_.Version-($a[$_.Key] ?? 0);Key=$_.Key} } | Sort-Object Delta -Descending | Format-Table -AutoSize | Out-String -Width 300 | Set-Content "$out\per-key-delta.txt"
```
- 期望:
  - revision/s ≈ 2·N_login + N_scene_manager + N_match + N_guild + 0.1·N_gate(±15%);
  - Delta 不为 0 的键只落在三族里:水位键、旧前缀镜像键、`GateNodeService.rpc/zone/…` 的 gate 键;
  - 清单外的键、负的 Delta,都原样上报。

【M6 开发 etcd 上手工压缩并重启】
- 状态:C++ 部分受阻,原因同 M5;另外需要「同意 3」。与 P0-D 的实测合并成一次做。不可逆:只删历史版本,不动当前键值。
- Go 部分(只看重启后的 re-watch)可以在 M5a 的栈上做,但同样需要同意 3。
- 压缩前先记下每个日志文件的行数,压缩后只看新增行。Go 服务同时写 stdout 与 stderr(go_services.ps1:662-663),两个都要搜。
- 压缩后停掉一个 C++ 或 Go 节点,在其它 C++ 节点日志的新增行里找:
  - 正常:`Service node stop, key:` 后面带完整 JSON(node.cpp:1320),随后出现 `Service node stopped`(:1497);
  - 前值丢失:`Parse node JSON failed`(:1342)或 `Ignore service node stop with empty node_uuid`(:1356)。
- 再执行 `docker --context desktop-linux restart etcd`,观察:
  - Go:
    - 每个 Handle 出现一次 `ownership watch ended … will re-verify and re-watch`(allocator.go:1016),之后不再出现 `slot ownership lost`(:1253);
    - killswitch 出现 `[killswitch] Watch interrupted; repeating full synchronization`(go/shared/killswitch/watch.go:60)。日志已英文化,旧的中文文案已不存在;
    - scenenode 与 match 的 NodeWatcher 出现 `Watch interrupted; synchronizing again`(go/shared/scenenode/watcher.go:125、go/match/internal/discovery/node_watcher.go:90)。
  - C++:预期出现 `RPC failed`(cpp/generated/grpc_client/grpc_init_client.cpp:409)与 `Lease deadline exceeded`(node.cpp:1541),这就是 P0-D。
- 日志路径出处:start_game.ps1:260、:282、:347;go_services.ps1:87。

【回报】
- `$out` 路径;M0–M4、S1、S2 每步的关键输出;与期望不符之处;未执行的步骤及原因(未获同意或受阻)。
- 每份结果附上以下两条命令的输出,只记录现场,不做任何改动:
  - `git -C D:\luyuan\xuanming-server-mmo-cpp rev-parse HEAD`
  - `git -C D:\luyuan\xuanming-server-mmo-cpp status --short`
- Claude 收到后写回两处:
  - docs/PROGRESS.md 追加一条;
  - docs/handoff/handoff-global-singletons-10m-ccu-20261013.md 文末追加订正小节,把本行状态从「静态推断」改为实测结论。

## P0-D C++ etcd 双向流 ok=false 后不重建


### 现在能跑 / 受阻(摘要)

**现在能跑**:现在就能跑的是 M0、M1、M2、M3、M4,各有前提:M0 只读盘点;M1 存量日志排查;M2 Go 模板红测,要等 Claude 先写好 P1 的测试文件,Codex 用 Go 1.27.2 在新建的 2f897bd07 worktree 里跑;M3 部署渲染契约测试,要 pwsh 7;M4 可选的 etcd 租约重启探针,环境上能跑,但要负责人同意重启共享 etcd,而且 Codex 要先确认两条 etcdctl 子命令的写法。

**受阻**:

1. A1(路线 A,进程内复现 T1)
   - 阻塞原因:
     ① gRPC Debug 安装缺失(third_party/grpc/install_vs2026_dbg 不存在;嵌套子模块 boringssl、cares、re2、zlib、googletest 等未检出,build_grpc.ps1:89 会直接报错)。
     ② Boost 只有模块化布局。core、grpc_client、rpc_controller_test 的 include 路径只有 third_party/boost/ 根,所以 muduo 的 EventLoop.h:18(boost/any.hpp)、etcd_grpc_client.h:5(circular_buffer)、etcd_grpc_client.cpp:8(pool)都会 C1083,PROGRESS:7637 已实测。
     ③ rpc_controller_test 还链 scene.lib、gate.lib 等,scene 在 HEAD 能否编过未核实。
     ④ 五个未初始化子模块的影响未核实。
   - 解除条件:递归初始化子模块(下载)→ 跑 build_grpc.ps1(可能经 winget 装或升级 cmake、ninja)→ 修 Boost 头文件布局(做法由 Codex 先确认)→ 在主工作区串行 /m:1 编译,再跑 run_cpp_tests.ps1 -Filter rpc_controller_test。
   - 由谁解除:负责人拍板授权;Codex 修环境并执行。这属于 §10.2 的下载、安装工具、改构建环境。
2. B0~B7(路线 B,docker 重启实验)
   - 阻塞原因:A1 的全部阻塞,加上没有 gate.exe(bin\ 下没有任何 exe),而 gate 链 scene.lib。
   - 解除条件:A1 解除后编出 gate.exe。或者 M0 发现 2026-10-07 之后构建的节点镜像,且负责人同意由 Codex 先提「容器起 gate」的方案,仓库里没有现成命令。
   - 由谁解除:同 A1,由负责人拍板、Codex 执行。
   - 另外 B4、B6 会重启或停掉共享 etcd,需要负责人同意;B1 前要不要预建 Kafka 正式 topic,需 Codex 先确认。
3. K1(三成员 kind 验收)
   - 阻塞原因:需要 kind、镜像和 Linux 构建,属于下载加构建;按设计也只在修复之后做。
   - 解除条件:修复合入后,负责人单独授权。

### 步骤原文

P0-D 实测任务单(锚定 2f897bd07)

基线:2f897bd0780c1cc78284548284f20d01e61d7ea8,用 `git rev-parse 2f897bd07` 取得。

通用约束:
- Shell 用 PowerShell;脚本标了 `#requires -Version 7` 的用 pwsh 7。不要用 Git Bash,它会改写以 `/` 开头的参数。
- 不动 Codex 的两棵 worktree(C:/Users/luyua/.codex/worktrees/f452、793e)。
- 每步结论只写三种:成立 / 不成立 / 没跑起来。
- 失败不连续重试;保留原始输出。

判据依据(HEAD 行号):
- 丢弃 ok=false 并 return:grpc_init_client.cpp:407-411;模板 grpc_init_total.cpp.tmpl:84-88。
- 写标志只在成功的 WRITE 里复位:KeepAlive 在 etcd_grpc_client.cpp:615(置位)、:629(复位);Watch 在 :389、:403。
- 读只在成功分支续发::415-423、:641-649。
- 流的唯一创建点:etcd_grpc_client.cpp:974-1010 ← grpc_init_client.cpp:504-505 ← etcd_service.cpp:31。
- 唯一的完成队列轮询点:etcd_service.cpp:33-37。
- 本地失租判定:etcd_service.cpp:622-624;触发:node.cpp:1537-1546。
- gRPC 约定(1.83.0,third_party/grpc c876f4d):include/grpcpp/completion_queue.h:145-169。

要回答的问题:
- Q0(机械):生成器模板是否丢弃 ok=false、没有重建入口。
- Q1:断连后 LeaseKeepAlive 流会不会自行恢复。
- Q2:Watch 流会不会自行恢复。
- Q3:不恢复时,是否约每 181s 重注册一轮;从第 2 轮起键是否先被删再写回。
- Q4(可选):先起节点、后起 etcd,节点是否永远不注册。

========== 一、现在就能跑 ==========

M0 只读盘点 [无下载|非破坏|约 5 分钟]
工作目录 D:\luyuan\xuanming-server-mmo-cpp。
```powershell
git rev-parse HEAD
git status --short
pwsh --version
$PSVersionTable.PSVersion
Get-Command pwsh,go,cmake,ninja,protoc,grpc_cpp_plugin,docker -ErrorAction SilentlyContinue | Format-Table Name,Source
go version; protoc --version
git submodule status
Test-Path third_party\grpc\install_vs2026_dbg
Test-Path third_party\boost\boost\any.hpp
& "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\MSBuild.exe'
docker --context desktop-linux images --format "{{.Repository}}:{{.Tag}} {{.CreatedAt}}"
docker --context desktop-linux ps -a --format "{{.Names}} {{.Status}}"
git log -1 --format="%h %ad" --date=short -- cpp/generated/grpc_client/etcd/ cpp/libs/engine/core/node/system/etcd/
git log -1 --format="%h %ad" --date=short -- cpp/generated/grpc_client/grpc_init_client.cpp
```
期望:
- rev-parse 为 2f897bd0780c1cc78284548284f20d01e61d7ea8。
- status 只有 ` m third_party/librdkafka`、` m third_party/ue5navmesh`。不一致就停下回报:说明有人在动主工作区。
- submodule 首列为 `-` 的恰好是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash。
- 两个 Test-Path 都是 False:gRPC Debug 安装缺失;Boost 只有模块化布局(PROGRESS:7637 那次 C1083 的根因)。
- 倒数第二条为 `a8e9ffb1f 2026-10-07`;最后一条为 `898c803ea 2026-10-10`(那次提交只加了 guild_internal / match_internal 的分发,!ok 分支未动)。

另外回报:
- pwsh 7 是否可用(M3 和路线 B 都要)。
- Go 版本:protogen 要求 go 1.27.2(go.mod:3)。
- protoc 版本:regen 要求 `libprotoc 35.1`(PROGRESS:5997)。
- 本机有没有 `ghcr.io/luyuancpp/mmorpg-node` 或 `mmorpg-node:*` 镜像,以及创建时间。晚于 2026-10-07 的才与 HEAD 的 etcd 客户端一致。
- 10-14 独立 GTest 用的 GTest 装在哪。
- 10-14 构建的生成器二进制在哪。

出处:vswhere 用法 run_cpp_tests.ps1:86-88;`--context desktop-linux` 见 start_game.ps1:112;镜像名 dev_tools.ps1:37、deploy/k8s/README.md:89。

M1 存量排查 [只读|无下载]
主仓 bin\logs\cpp_nodes\ 现在是空的。别的机器或环境如果留着 C++ 节点文件日志,执行:
```powershell
Select-String -Path <节点日志> -Pattern 'Lease deadline exceeded: no keepalive ACK from etcd within TTL'
```
同一进程相邻两条间隔约 181s 的,就是受害节点,只能重启。出处 node.cpp:1541。

M2 Go 模板红测(回答 Q0;机械证据,不是运行证据)[无下载|非破坏|耗时未实测]
前提:Claude 已按 pre_measure 的 P1,在主工作区写好 tools/proto_generator/protogen/internal/generator/cpp/grpc_stream_lifecycle_test.go(未提交)。
GOTOOLCHAIN=local 下如果还要下载工具链或模块,判为「没跑起来」。
```powershell
git -C D:\luyuan\xuanming-server-mmo-cpp worktree add --detach $env:TEMP\p0d-etcd 2f897bd07
git -C $env:TEMP\p0d-etcd rev-parse HEAD
Copy-Item -LiteralPath D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\generator\cpp\grpc_stream_lifecycle_test.go -Destination $env:TEMP\p0d-etcd\tools\proto_generator\protogen\internal\generator\cpp\
Set-Location $env:TEMP\p0d-etcd\tools\proto_generator\protogen
$env:GOTOOLCHAIN = 'local'
go test -mod=readonly ./internal/generator/cpp/ -run "TestGrpcInitTemplateSupportsMultipleServicesForSameNodeType|TestGrpcInitTemplateDoesNotDropFailedStreamCompletions|TestGrpcAsyncClientTemplateStreamLifecycle" -count=1 -v
```
期望 rev-parse 为 2f897bd0780c1cc78284548284f20d01e61d7ea8。

判据:
- 成立:两条新用例都是 `--- FAIL`,失败信息是断言文本。可能的文本:`if (!ok)` 块里仍有 `return;`;流式分发没收到 ok;缺 Restart / Cancel / StreamClosedHandler;仍有 `cq.Shutdown()`。同时,同一次运行里的既有用例 TestGrpcInitTemplateSupportsMultipleServicesForSameNodeType 是 PASS(证明夹具和模板解析正常)。
- 不成立:新用例在 2f897bd07 上 PASS。停下回报。
- 没跑起来:编译错误;模板解析或渲染报错(失败信息以 parse / render 开头);需要下载;既有用例也挂了。

保留完整 go test 输出。worktree 留给 M3 用。
出处:模板测试先例 grpc_process_test.go:40-136;Go 参数 PROGRESS:7636;worktree 用法 docs/design/cross-zone-scene-travel.md:1423。

M3 部署渲染核对(辅助证据:只核部署侧前提,不判缺陷)[无下载|非破坏|耗时未知|需 pwsh 7]
DryRun 不调 kubectl,只打印清单(k8s_deploy.ps1:2086-2089、:2059-2068)。
```powershell
Set-Location $env:TEMP\p0d-etcd
pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```
判据:下面三条都是 `[PASS]`,即部署前提成立:C++ 节点的 TTL 取权威值 180;gate、scene 没有会杀容器的探针,所以受害节点不会被 K8s 自动拉起。
- `node ConfigMap 的 NodeTTLSeconds 必须与 bin/etc 权威值一致`
- `gate 就绪探针只探唯一的监听口 18000,不探不存在的 gRPC 口,也不加会杀容器的探针`
- `scene(Deployment)就绪探针探 gRPC 口 50000 并声明该端口;Agones Fleet 不加任何 K8s 探针`

任一条 `[FAIL]`:回报原文,本项「受害节点只能人工重启」的口径需要改。其余用例的失败与本项无关,照实列出,不在本项处理。
「Hosts 只有一项」是静态事实(k8s_deploy.ps1:2207-2209),本测试不断言它。
收尾(先例 placement-reverify-checklist-20261012.md:573):
```powershell
Set-Location D:\luyuan\xuanming-server-mmo-cpp; git worktree remove --force $env:TEMP\p0d-etcd; git worktree prune
```

M4(可选)etcd 单成员重启后租约是否保留(外部语义 X3,只影响推断 I2)[无下载|破坏性:重启本机共享的 etcd 容器,执行前需负责人同意|约 2 分钟]
不需要 C++ 节点。做法:
1. 用 TTL 180 的租约挂一把探针键 `BattleNodeService.rpc/allocated/node_type/999/node_id/424242`。
2. `docker --context desktop-linux restart etcd`。
3. 等 `docker --context desktop-linux exec etcd etcdctl endpoint health` 退出码为 0(start_game.ps1:450)。
4. 依次执行 `etcdctl lease list`、`etcdctl lease timetolive <id> --keys`。

判据:
- X3 成立:租约仍在列表里,剩余 TTL 回到约 180。
- X3 不成立:租约或探针键消失。

需 Codex 先确认:`etcdctl lease grant`、`put --lease`、`lease revoke` 的参数形态,仓库里没有先例。跑完删掉探针键,撤销租约。

========== 二、受阻 ==========

A1 路线 A 进程内复现(回答 Q1 的机制;首选的运行证据)
内容:编译并运行 pre_measure 的 P2 用例 EtcdStreamBreakTest.KeepAliveWriteStateIsNotStuckAfterStreamBreak。它在进程内起假 etcd 服务,客户端走真实的 InitGrpcNode / HandleCompletedQueueMessage。

阻塞:
1. gRPC Debug 安装缺失。third_party/grpc/install_vs2026_dbg 不存在,而 rpc_controller_test.vcxproj:126 要链 grpc++.lib 等,gate.vcxproj:160 也指向这个目录。gRPC 源码已检出(c876f4d),但嵌套子模块只检出了 abseil-cpp 和 protobuf;boringssl-with-bazel、cares、re2、zlib、googletest 等都没有,build_grpc.ps1:89 遇到 BoringSSL 缺失会直接报错。
2. Boost 头文件布局。third_party/boost 只有模块化的 libs/*/include,没有 boost/ 头目录;core、grpc_client、rpc_controller_test 的 include 路径只有 third_party/boost/ 根。凡包含下面任一头文件的编译单元都会报 C1083(Codex 10-14 在 battle 上已实测,PROGRESS:7637):
   - third_party/muduo/muduo/net/EventLoop.h:18 的 boost/any.hpp;
   - etcd_grpc_client.h:5 的 boost/circular_buffer.hpp;
   - etcd_grpc_client.cpp:8 的 boost/pool/object_pool.hpp。
3. rpc_controller_test 还要链 scene.lib、gate.lib、session.lib、table.lib、rdkafka++.lib、hiredisd.lib(vcxproj:126)。scene 在 HEAD 上能否编过未核实。
4. 未初始化的子模块(cppcodec、fmt、muduo-linux、recastnavigation、xxhash)对这条链有没有影响,未核实。

「不链整个节点的独立 GTest」在这里不适用。BattleAdmissionGate 能独立编,是因为它刻意不依赖 muduo、protobuf 和引擎(battle_admission_gate_test.cpp:5-7);本用例必须链真实 gRPC 运行库和生成的 etcd 客户端,绕不开阻塞 1、2。

解除条件:负责人授权 Codex 做三件事:
1. 递归初始化子模块。要联网下载;不能用 junction 代替(PROGRESS:4983)。
2. 跑 `pwsh -File tools/scripts/third_party/build_grpc.ps1`。它会经 winget 安装或升级 cmake、ninja(tools/scripts/README.md:125-135),属于 AGENTS.md §10.2 的「安装工具」。
3. 修 Boost 头文件布局。仓库里没有先例,具体做法需 Codex 先确认;这属于修改构建环境。

之后在主工作区执行(新 worktree 没有子模块,初始化又要下载;rpc_controller_test 不在 game.sln 里,由脚本的 -Build 编):
```powershell
Set-Location D:\luyuan\xuanming-server-mmo-cpp
$msbuild = & "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\MSBuild.exe' | Select-Object -First 1
& $msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test
```
判据:
- 成立:汇总表里 rpc_controller_test 那一行的结果列是「N/M 过,挂:EtcdStreamBreakTest.KeepAliveWriteStateIsNotStuckAfterStreamBreak」(run_cpp_tests.ps1:184),末行是 `1 test projects failed`(:196);gtest 输出里断言处 isInProgress 为 true;其余用例与基线一致。
- 不成立:T1 在 HEAD 上 PASS,说明 gRPC 实际行为与 completion_queue.h 的约定不符。停下回报,不要继续落码。
- 没跑起来:出现任何 `error C` 或 `LNK`;汇总表报「退出码 …,无 gtest 输出」或「疑似崩在 …」。

保留:第一条 error C / LNK 及其前后 20 行;gtest 原始输出。
负责人:授权由负责人拍板,执行由 Codex。

B0~B7 路线 B docker 重启实验(回答 Q2、Q3、Q4)
阻塞:A1 的 1~4 全部;此外没有 gate.exe(bin\ 下没有任何 exe,lib\ 只有 battle.pdb),而 gate 链 scene.lib(gate.vcxproj:159)。
解除条件:
- 首选:A1 解除后编 game.sln 出 gate.exe。
- 备选:M0 发现 2026-10-07 之后构建的节点镜像,且负责人同意改用容器起 gate。仓库里没有「本机 docker 单起 gate 容器」的现成命令,需要 Codex 先提方案,再由负责人拍板。

标注:
- 下载:无。compose 一律带 `--pull never`;缺镜像就停下。
- 破坏性:B4 重启本机共享的 etcd(不清数据);此时若有别的 C++ 节点在跑,它们会进入同一个永久重注册循环,只能重启。B6 停 etcd 约 20s。
- 耗时:主实验约 10 分钟(要等满两个 181s 周期),B6 约 3 分钟。
- 局限:单成员重启时所有 C++ 节点同时断流,看不到「watch 正常的 gate 摘掉受害 scene」(推断 I3)。

判据。A、B、C 三条都按「成立」出现才算成立;任一条按「不成立」出现就停下,把日志交回:

| 观察点 | 缺陷成立 | 缺陷不成立 |
| --- | --- | --- |
| A 续租 | `lease timetolive` 的剩余 TTL 单调下降,不回到约 180 | 始终约 179~180 |
| B 失租日志 | T0+180~185s 出现 `Lease deadline exceeded: no keepalive ACK from etcd within TTL.`,随后 `Lease granted: id=` 给出新 id | 400s 内没有这一行,租约 id 不变 |
| C watch 探针 | 两次删除探针键后 30s 内,文件日志里都没有 `Service node stop, key: <探针键>` | 5s 内出现 |
| D 断流瞬间 | T0 之后出现 2~4 行 `RPC failed`,之后不再增加 | 也可能出现;单看这一条无法区分 |

注意两点:
- `Keeping node alive, lease_id:`(etcd_manager.cpp:198,DEBUG 级)在入队时就打印,流死了照样每秒一行,不能当续租成功的证据。
- 判据一律查 bin\logs\cpp_nodes\ 下的文件日志(1 秒一刷,node.cpp:374、:405);不查 stdout 重定向的日志(console_log.cpp:49-53 没有刷新,可能滞后很久)。

B0 前置检查。
```powershell
$repo = 'D:\luyuan\xuanming-server-mmo-cpp'; Set-Location $repo
Test-Path bin\gate.exe
pwsh -File tools/scripts/cpp_nodes.ps1 -Command status
Get-Process gate,scene,battle,match -ErrorAction SilentlyContinue
```
期望:True;`No tracked nodes.`;最后一条无输出。有别的节点在跑就停下问负责人。
出处:cpp_nodes.ps1:60、:325-328、:481-483。

B1 起依赖。
```powershell
docker --context desktop-linux ps -a --filter "name=^redis$" --format "{{.Names}} {{.Status}}"
docker --context desktop-linux compose -f D:\luyuan\xuanming-server-mmo-cpp\deploy\docker-compose.yml up -d --no-recreate --pull never etcd redis kafka
docker --context desktop-linux exec etcd etcdctl endpoint health
docker --context desktop-linux inspect etcd --format "{{json .Mounts}}"
```
期望:health 退出码为 0;Mounts 里有 etcd-data → /bitnami/etcd/data(docker-compose.yml:447-476、:491)。
说明:
- kafka 必须真在监听:gate 装不上 Kafka 处理器会 LOG_FATAL(node.cpp:963)。
- 需 Codex 先确认:要不要先按 start_game.ps1:182-205 的做法预建正式 topic。docker-compose.yml:66-81 写明 broker 会自动建 1 分区的 topic,建出来之后契约核对会失败。
出处:start_game.ps1:446、:450。

B2 起一个 gate。
```powershell
$env:PATH = (Join-Path $repo 'third_party\grpc\install_vs2026_dbg\bin') + ';' + $env:PATH
pwsh -File tools/scripts/cpp_nodes.ps1 -Command start -Nodes gate -NodeIp loopback
pwsh -File tools/scripts/cpp_nodes.ps1 -Command status
Get-Content run\logs\cpp_nodes\gate.stderr.log -Tail 20
$log = Get-ChildItem bin\logs\cpp_nodes\gate.*.log | Sort-Object LastWriteTime | Select-Object -Last 1
Select-String -Path $log.FullName -Pattern 'NODE STARTED SUCCESSFULLY','Lease granted: id='
```
期望:status 显示 RUNNING;stderr 日志为空;文件日志里有 banner 和 `Lease granted: id=<十进制>, ttl=180s`。
- 记下租约 id 并转十六进制:`'{0:x}' -f [int64]<id>`。以 `lease list` 的输出形态为准。
- 进程不在或 stderr 有内容:本步无效,停下回报。脚本 30s 后打的 `[timeout]`(cpp_nodes.ps1:277)不是结论。
- 顺带回报:同一行日志在 stdout 日志和文件日志里出现的时间差。
出处:PATH 见 start_game.ps1:551,不带会缺 zlibd.dll(PROGRESS:4206);cpp_nodes.ps1:44、:50、:351-352;banner 在 node.cpp:1012;`Lease granted` 在 etcd_service.cpp:496;「单起 gate 能到 banner」的依据只有 start_game.ps1:266-267 的注释,未实跑。

B3 对照(etcd 未重启)。
```powershell
$lease = '<十六进制租约号>'
1..3 | ForEach-Object { docker --context desktop-linux exec etcd etcdctl lease timetolive $lease --keys; Start-Sleep 5 }
docker --context desktop-linux exec etcd etcdctl get '<键>' --write-out=json   # 上面列出的三把键各执行一次
$probe = 'BattleNodeService.rpc/allocated/node_type/999/node_id/424242'
docker --context desktop-linux exec etcd etcdctl put $probe 'p0d-probe-0'
docker --context desktop-linux exec etcd etcdctl del $probe
Start-Sleep 5
Select-String -Path $log.FullName -Pattern 'Service node stop, key: BattleNodeService.rpc/allocated/node_type/999/node_id/424242'
```
期望:
- TTL 约 179~180。
- keys 有三把:`/service/127.0.0.1/port/<p>`、`GateNodeService.rpc/allocated/node_type/<t>/node_id/<n>`、`GateNodeService.rpc/zone/<z>/node_type/<t>/node_id/<n>`。三把键的 create_revision、mod_revision、version、lease 存档。
- 探针:日志里出现一行,结尾是 `value: p0d-probe-0`。不出现说明探针无效,先停下排查。

探针为什么安全:
- 带 `/allocated/` 的键,新增时被跳过(service_discovery_manager.cpp:158-162);删除时只打一行 INFO 就跳过(node.cpp:1320、:1328-1332),不改任何状态。
- watch 带 prev_kv(etcd_helper.cpp:50);前缀在 base_deploy_config.yaml:46。
- node_type 999 不与真实的分配前缀重叠(etcd_manager.cpp:60-70)。
- Go 的 match 也跳过这类键(go/match/internal/discovery/node_watcher.go:207-208)。
出处:键的形态 etcd_manager.cpp:47-85;`--write-out=json` 见 start_game.ps1:270;本机默认日志级别是 INFO(base_deploy_config.yaml:25 加 Logging.h:20-29)。

B4 重启 etcd。
```powershell
$T0 = Get-Date; docker --context desktop-linux restart etcd
do { Start-Sleep 1; docker --context desktop-linux exec etcd etcdctl endpoint health } until ($LASTEXITCODE -eq 0)
$T1 = Get-Date
docker --context desktop-linux exec etcd etcdctl lease list
```
期望:原租约 id 仍在列表里。不在就停下回报,说明数据目录不是预期位置;不要改判据。

B5 观察(时间点相对 T0 / T1)。
- 5.1 在 T1+10s、T1+60s 各执行一次 `Select-String -Path $log.FullName -Pattern 'RPC failed'`,记行数与时间戳,对照判据 D。etcd 停机期间会有 `gRPC KV.Range failed: code=`(格式见 gate/main.cpp:329),两种结论下都会有,T1 之后应停止。
- 5.2 在 T1+15s 和 T0+70s 各做一次 B3 的探针(值改为 p0d-probe-1、p0d-probe-2),每次等 30s 后搜日志,对照判据 C。第二次晚于断链感知上限约 40s(grpc_channel_cache.h:44-60)。
- 5.3 从 T1+15s 起,每 30s 执行一次 `lease timetolive $lease --keys`,共 6 次,对照判据 A。
- 5.4 在 T0+175s~T0+200s 盯文件日志,对照判据 B。成立时还应依次出现:
  - `Registration mode: InitialBoot -> ReRegisterExisting, reason=lease deadline exceeded locally`(etcd_service.cpp:147)
  - `Re-registering existing node with node_id=`(node_allocator.cpp:102)
  - `Re-claiming global node-id allocation (absent-or-owned): `(etcd_manager.cpp:110)
  - `Node re-registration successful, node_id=`(etcd_service.cpp:215)
- 5.5 在 T0+200s、T0+400s 各执行一次 `lease list`,并对三把键重复 B3 的 `get --write-out=json`。这是推断 I2,出不出现都要记:
  - T0+200s:create_revision 不变,lease 换成新租约。
  - T0+400s:日志里有第二次 `Lease deadline exceeded`;create_revision 变大,说明键被删过再写回。
  - 键元数据的语义见 proto/etcd/kv.proto:9-16。

B6(可选,回答 Q4)。
```powershell
pwsh -File tools/scripts/cpp_nodes.ps1 -Command stop -Nodes gate
docker --context desktop-linux stop etcd
pwsh -File tools/scripts/cpp_nodes.ps1 -Command start -Nodes gate -NodeIp loopback
pwsh -File tools/scripts/cpp_nodes.ps1 -Command status   # 必须是 RUNNING,否则本步无效
Start-Sleep 20; docker --context desktop-linux start etcd; Start-Sleep 120
```
判据:
- 成立:最新一份文件日志里有 `RPC failed` 和 `Watching prefix: `,但一直没有 `Requesting etcd lease. mode=InitialBoot`,也没有 banner。
- 不成立:etcd 起来后几秒内出现 banner。
- 脚本 30s 后打的 `[timeout]` 属预期;进程不在则不算成立。
出处:etcd_service.cpp:283、:355-359、:412;completion_queue.h:145-150(fail-fast 下 StartCall 返回 ok=false)。

B7 收尾:`pwsh -File tools/scripts/cpp_nodes.ps1 -Command stop -Nodes gate`。不清 etcd、Redis、Kafka 数据。

要保留并交回:M0 的全部输出;gate 的文件日志和 stdout / stderr 日志;每次 timetolive、lease list、get --write-out=json 的输出;T0 / T1;gate.exe 的构建提交;B1 的 Mounts 输出。

K1 三成员 kind 验收(只在修复之后做)
阻塞:需要 kind、镜像和 Linux 构建(deploy/k8s/Dockerfile.cpp),属于下载加构建。确认缺陷本身不需要它:单成员重启走的是同一条代码路径,C++ 只连一个 ClusterIP 地址(k8s_deploy.ps1:2209)。
解除:修复合入后,由负责人单独授权。
验收项沿用原报告:
1. 逐个删除 etcd-0、etcd-1、etcd-2(等前一个 Ready 再删下一个)。
2. 只杀 leader。
3. 只让部分 C++ 节点断流时,watch 正常的 gate 日志里不得出现 `Service node stop, key: SceneNodeService.rpc/zone/`。
4. 成员失去法定人数的场景只观察并记录。

## P0-E 共享 Redis 没设 maxclients


### 现在能跑 / 受阻(摘要)

**现在能跑**:现在就能跑:S0、S1、S2(S2.1~S2.4,前提是本机已有 redis:7.2,否则拉镜像要负责人同意)、S2b(不联网;可选的 login 构建要求模块已在缓存)。满足条件即可跑:S3a(本机已有在跑的开发栈时只读采样)、S6(已有 K8s 环境时)、R0(Claude 写好回归测试文件后跑红)。

**受阻**:

【S3b 自起整栈空闲采样】
四个阻塞原因:
1. 没有 gate / scene / battle 的 exe。
   - Codex 2026-10-14 串行 /m:1 编译 cpp/nodes/battle/battle.vcxproj(Debug|x64)时,third_party/muduo/muduo/net/EventLoop.h:18 报 C1083,找不到 boost/any.hpp。原因是 Boost 采用模块化 include 布局,而工程只包含根目录(docs/PROGRESS.md:7637)。
   - gate、scene 同样经 muduo 引入这个头文件,推断同样受阻。
2. third_party/grpc/install_vs2026_dbg 不存在。
   - start_game.ps1:551 把它的 bin 放进 PATH。
   - 它由 `dev_tools.ps1 -Command third-party-grpc-build` 产出(dev_tools.ps1:1417 → tools/scripts/third_party/build_grpc.ps1:486)。
3. 没有网关 jar。
   - start_game.ps1:370-371 要求 java/gateway_node/target/gateway-node-*.jar,:366-369 要求 Java ≥ 23。
4. cppcodec、fmt、muduo-linux、recastnavigation、xxhash 五个子模块未初始化,是否被节点编译用到,需 Codex 先确认。

解除条件:三个 exe、install_vs2026_dbg、网关 jar 都齐备,并且 `start_game.ps1 -CheckOnly` 通过。

由谁解除:
- 1、2、4 属于修构建环境(改 Boost include 布局或工程属性;编译 gRPC 很耗时;初始化子模块可能要下载)。须负责人授权,Codex 执行。
- 3 是 Codex 常规的 mvn 打包,不改环境,但耗时。是否为本项去做,由负责人定。

【S4 轻负载与不回落】
- 阻塞原因:依赖 S3b。机器人要走网关 8081 → gate → scene(robot_smoke.yaml:3)。另外,机器人的认证方式是否匹配当前栈,需 Codex 先确认。
- 解除条件:S3b 解除;或者本机已有带网关和 gate 的现成栈,这时转为 S3a 条件下执行。
- 由谁解除:同 S3b。

【S5 到线时 Go 客户端的表现】
- 阻塞原因:需要一个在跑的本机开发栈(S3a 或 S3b);并且有破坏性,期间本机登录与存盘失败。
- 解除条件:有栈,并且负责人同意。
- 由谁解除:负责人拍板。

【S6 K8s 实机只读核对】(只在没有 K8s 环境时算受阻)
- 解除条件:已有环境。新建 kind 或拉镜像要负责人同意;而且 kind 回答不了生产运行时的情况。
- 生产或预发集群上 redis-0 的 nofile 硬限和各 Go 服务的 GOMAXPROCS,只能由负责人或运维在目标集群上只读取一次。

【对判定的影响】
S3b、S4、S5 只用来校准总账系数,看到线时的表现,不参与「成立与否」。建议不为本项单独解除构建阻塞:这几步延后,等其他项修好 C++ 构建环境后顺带补测。

### 步骤原文

【P0-E 实测任务单 · 锚定 2f897bd07(完整哈希 2f897bd0780c1cc78284548284f20d01e61d7ea8)】

要回答的问题
- Q1 生效的 maxclients:
  - redis:7.2 的缺省值;
  - nofile 不够时是否自动下调,日志原文是什么;
  - Redis 是否自己抬软限;
  - K8s 实机上的生效值,以及 redis-0 的 nofile 硬限。
- Q2 总账系数:
  - K8s 上各 Go 服务的 GOMAXPROCS(静态预期 2,见 S6);
  - go-zero 封装的池参数;
  - v9.16.0 / v9.17.3 的池封顶、无后台回收、握手是否与 v9.18.0 相同;
  - 负载撤掉后连接数是否回落。
- Q3 到线时,服务端、就绪探针句子、管理命令各自怎么表现。
- Q4(可选)到线时 Go 客户端日志里出现什么文本。

判据
- 成立(需要显式设值并建总账):S1 第一条无输出,第二条恰好两行且都不含 --maxclients;并且 S2.1 的缺省 maxclients 是 10000。
  - Q2 只用来校正总账系数,不参与成立与否;
  - 少量机器人测到的连接数,不能用来降级。
- 比交接说的更糟(不等窗口,立即报负责人),满足任一条即算:
  - S6 实机 `CONFIG GET maxclients` 小于 10000;
  - redis-0 的 nofile 硬限小于 10032;
  - S2b 或 S6 显示某个 Go 服务的 GOMAXPROCS 明显大于 2。
- 不成立:S2.1 的缺省值远大于 10000;或者 S6 的实机值来自某处显式配置。静态上已排除,出现即说明漏看了入口。
- 没跑起来:前置条件不满足(镜像不在、模块缓存不在、没有在跑的栈、没有 K8s 环境),或者命令报错。处理方式:
  - 该步记「未取得」,附完整输出;
  - 不重试,不绕路,不改判定;
  - 只要 S1 和 S2.1 跑出来,就能给出判定。
- 任何一步结果与「期望」不同:不重试,原样上报。

通用约定
- Shell 一律 PowerShell 7,命令里不出现嵌套的双引号。
- 不动 Codex 的两个 worktree(f452、793e),不在主工作区做 checkout。干净检出一律用 S0 新建的 W。
- 输出存到 `W/run/logs/p0e-maxclients/`。run/ 是既有的运行产物目录,出处 tools/scripts/go_services.ps1:85-87。
- docker 一律带 `--context desktop-linux`,出处 start_game.ps1:110-113。
- 全程不清数据,不是压测,不出性能结论(AGENTS §6)。
- 以下动作都要负责人先同意:下载(拉镜像、go 模块、kind);S5 的破坏性操作。
- 标注含义:
  - 【可跑】现在就能跑;
  - 【条件】满足所写条件才跑,不满足记「未取得」;
  - 【受阻】阻塞原因与解除条件见末尾。

══════ 第一组:现在就能跑 ══════

【S0 前置】【可跑】约 5 分钟,无下载
```
$PSVersionTable.PSVersion
pwsh --version
go version
Set-Location D:/luyuan/xuanming-server-mmo-cpp
git rev-parse 2f897bd07
$W = Join-Path $env:TEMP 'p0e-maxclients-2f897bd07'
git worktree add --detach $W 2f897bd07
Set-Location $W
git rev-parse HEAD
git status --short
New-Item -ItemType Directory -Force run/logs/p0e-maxclients | Out-Null
docker --context desktop-linux ps --format '{{.Names}} {{.Image}}'
docker --context desktop-linux image inspect redis:7.2 --format '{{.Id}}'
```
- 期望:
  - PowerShell 不低于 7.3,更低就停下;
  - `go version` 是 go1.27.2。所有 go/*/go.mod 的 go 行和 deploy/k8s/Dockerfile.go-svc:64 都是 1.27.2;不是就停下,避免触发工具链下载;
  - 两次 rev-parse 都输出 2f897bd0780c1cc78284548284f20d01e61d7ea8;
  - `git status --short` 为空。新 W 里子模块不初始化,本项也用不到。
- redis:7.2 不在本机:S2 停下,问负责人(属下载)。出处:deploy/docker-compose.yml:258;start_game.ps1:445-446 用 `--pull never` 起它。
- 记下 docker ps 里有没有名为 redis 的容器在跑,S3a 要用。
- 已知事实,不用再跑 -CheckOnly:
  - bin/ 下只有 README.md、etc、logs,没有 gate / scene / battle 的 exe;
  - third_party/grpc/install_vs2026_dbg 不存在;
  - java/gateway_node/target 只有 classes、test-classes,没有 gateway-node-*.jar。
  - 所以 S3b、S4 受阻。

【S1 静态基线】【可跑】约 1 分钟
```
git grep -n -i maxclients 2f897bd07 -- deploy tools/scripts go cpp java bin proto
git show 2f897bd07:deploy/k8s/manifests/infra/redis.yaml | Select-String -Pattern '^\s*exec redis-server'
```
- 期望:
  - 第一条无输出;
  - 第二条恰好两行(redis.yaml:95、:97),都以 `exec redis-server --appendonly yes` 开头,都不含 `--maxclients`。
- 不跑 DryRun:
  - Apply-Infra 对 redis.yaml 只替换 `__INFRA_NAMESPACE__`(k8s_deploy.ps1:6537),之后是通用占位检查(:6558-6561)和 apply(:6563),渲染结果就是文件本身;
  - DryRun 还会把注入的 Redis 口令明文打印出来(:6503 的注释)。

【S2 镜像缺省值、nofile 行为、到线语义】【可跑,前提是 redis:7.2 已在本机】约 10 分钟
- 一次性容器,不映射宿主端口,不影响在跑的 redis 容器。四段都保留完整输出。

S2.1 缺省值:
```
docker --context desktop-linux run -d --rm --name p0e-redis-default redis:7.2 redis-server --appendonly no
Start-Sleep 2
docker --context desktop-linux exec p0e-redis-default redis-cli config get maxclients
docker --context desktop-linux exec p0e-redis-default redis-cli config get timeout
docker --context desktop-linux exec p0e-redis-default redis-cli config get tcp-keepalive
docker --context desktop-linux exec p0e-redis-default redis-cli config get tcp-backlog
docker --context desktop-linux exec p0e-redis-default cat /proc/1/limits
docker --context desktop-linux exec p0e-redis-default cat /proc/sys/net/core/somaxconn
docker --context desktop-linux logs p0e-redis-default
docker --context desktop-linux stop p0e-redis-default
```
- 期望(外部常识,以实际为准):
  - maxclients 10000、timeout 0、tcp-keepalive 300、tcp-backlog 511;
  - 记下 /proc/1/limits 里 Max open files 的软限和硬限;
  - 日志里有 backlog 相关告警就照实记。

S2.2 硬限不足(maxclients 用缺省):
```
docker --context desktop-linux run -d --rm --name p0e-redis-lowfd --ulimit nofile=1024:1024 redis:7.2 redis-server --appendonly no
Start-Sleep 2
docker --context desktop-linux exec p0e-redis-lowfd redis-cli config get maxclients
docker --context desktop-linux logs p0e-redis-lowfd
docker --context desktop-linux stop p0e-redis-lowfd
```
- 期望(外部常识):maxclients 小于 10000,约 992;日志含 `maxclients has been reduced`。
- 日志原文与此不同时照实记。修复设计里的日志判据以实测文本为准。

S2.3 软限低、硬限够(显式 40000),再加一组硬限不够:
```
docker --context desktop-linux run -d --rm --name p0e-redis-raise --ulimit nofile=1024:65536 redis:7.2 redis-server --appendonly no --maxclients 40000
Start-Sleep 2
docker --context desktop-linux exec p0e-redis-raise redis-cli config get maxclients
docker --context desktop-linux exec p0e-redis-raise cat /proc/1/limits
docker --context desktop-linux logs p0e-redis-raise
docker --context desktop-linux stop p0e-redis-raise
docker --context desktop-linux run -d --rm --name p0e-redis-short --ulimit nofile=1024:20000 redis:7.2 redis-server --appendonly no --maxclients 40000
Start-Sleep 2
docker --context desktop-linux exec p0e-redis-short redis-cli config get maxclients
docker --context desktop-linux logs p0e-redis-short
docker --context desktop-linux stop p0e-redis-short
```
- 期望(外部常识):
  - 第一组返回 40000,软限被 Redis 自己抬到 40032,日志含 `Increased maximum number of open files`;
  - 第二组小于 40000(约 19968),并有下调日志。
- 用途:决定启动薄壳要不要自己执行 `ulimit -n`。

S2.4 到线语义(上限设成 6):
```
docker --context desktop-linux run -d --rm --name p0e-redis-full redis:7.2 redis-server --appendonly no --maxclients 6
Start-Sleep 2
1..5 | ForEach-Object { docker --context desktop-linux exec -d p0e-redis-full sh -c 'sleep 90 | redis-cli' }
Start-Sleep 3
docker --context desktop-linux exec p0e-redis-full redis-cli info clients
docker --context desktop-linux exec -d p0e-redis-full sh -c 'sleep 90 | redis-cli'
Start-Sleep 2
docker --context desktop-linux exec p0e-redis-full redis-cli ping
docker --context desktop-linux exec p0e-redis-full redis-cli config get maxclients
docker --context desktop-linux exec p0e-redis-full sh -c 'r=$(redis-cli -h 127.0.0.1 -p 6379 ping 2>&1); echo probe_reply=$r; case $r in PONG|NOAUTH*) echo verdict=ready ;; *) echo verdict=notready ;; esac'
Start-Sleep 95
docker --context desktop-linux exec p0e-redis-full redis-cli info stats
docker --context desktop-linux exec p0e-redis-full redis-cli info clients
docker --context desktop-linux stop p0e-redis-full
```
- 期望:
  - 第一次 `info clients` 的 connected_clients 是 6(5 条占位加本次查询)。不是 6 就停下上报;
  - 占满后 ping 与 config get 都被拒,记原文(预期含 `max number of clients reached`,也可能是连接重置)。这同时证实到线后管理命令也进不去;
  - 探针那一句输出 verdict=notready。探针原句出处 redis.yaml:117、:126;
  - 占位退出后 rejected_connections 不小于 3。

【S2b 读模块缓存源码】【可跑】约 5 分钟,不联网
```
Set-Location (Join-Path $W 'go/scene_manager')
$env:GOPROXY = 'off'
$env:GOTOOLCHAIN = 'local'
$gz = go list -m -f '{{.Dir}}' github.com/zeromicro/go-zero
$gr = go list -m -f '{{.Dir}}' github.com/redis/go-redis/v9
$gz; $gr
Select-String -Path (Join-Path $gz 'core/stores/redis/*.go') -Pattern 'MinIdleConns|PoolSize|DisableIdentity|DisableIndentity|redis_client_pool|breaker' | Select-Object -First 40
Get-ChildItem $gz -Recurse -Filter *.go | Select-String -Pattern 'automaxprocs' -List | Select-Object -First 5 Path, Line
Select-String -Path (Join-Path $gr 'options.go') -Pattern 'PoolSize == 0' -Context 0,2
Select-String -Path (Join-Path $gr 'internal/pool/pool.go') -Pattern 'NewTicker|reaper|IdleCheckFrequency|semaphore|queue\s+chan|PoolFIFO' | Select-Object -First 20
Select-String -Path (Join-Path $gr 'redis.go') -Pattern 'conn\.Hello\(|failed to authenticate' -Context 0,3
```
- 再到 `go/login` 目录重复一遍:那里是 go-zero v1.9.2 和 go-redis v9.16.0(go/login/go.mod:13、:15)。scene_manager 是 go-zero v1.10.0 和 go-redis v9.17.3(go.mod:15、:64)。
- GOTOOLCHAIN=local 沿用 Codex 已经用过的写法(docs/PROGRESS.md:7636)。
- 期望(待证实):
  - go-zero 固定设置 MinIdleConns,不设 PoolSize;
  - 有没有 go-zero 包 import automaxprocs。各 go.mod 里 automaxprocs v1.6.0 都是 indirect,仓库内没有直接引用;
  - v9.17.3、v9.16.0 与仓库内 v9.18.0 相同:缺省池 = 10 × GOMAXPROCS,有信号量封顶,没有定时回收,握手先发 HELLO。v9.18.0 的对照位置:robot/vendor/github.com/redis/go-redis/v9 下 options.go:341-342、internal/pool/pool.go:343、:746、:991、:1091,redis.go:504-525。
- `go list` 报错或输出为空,就记「未取得」。不要 `go mod download`,下载须负责人同意。
- 可选,需 Codex 先确认模块已在缓存:仍保持 GOPROXY=off,在 W 根目录构建 login,再读链接进去的版本。
```
Set-Location $W
pwsh -NoProfile -File tools/scripts/go_services.ps1 -Command build -Services login
go version -m bin/go_services/login.exe | Select-String 'automaxprocs|go-redis|go-zero'
Remove-Item Env:GOPROXY
Remove-Item Env:GOTOOLCHAIN
```
- 出处:go_services.ps1:987-1027(Invoke-Build);产物路径 :1007-1008。构建失败记「未取得」。

【S3a 已有开发栈的只读采样】【条件】约 10 分钟
- 条件:S0 看到 redis 容器在跑,并且下面「按进程计数」的结果里有某个本仓库检出下 bin\ 的进程。条件不满足就记「未取得」。
- 不起栈,不重启任何东西。记下该栈所在目录(取进程的 Path)和它的提交:`git -C <该目录> rev-parse HEAD`。

采服务端:
```
Set-Location $W
docker --context desktop-linux exec redis redis-cli info server | Select-String redis_version
docker --context desktop-linux exec redis redis-cli config get maxclients
docker --context desktop-linux exec redis redis-cli info clients
docker --context desktop-linux exec redis redis-cli info stats | Select-String 'rejected_connections|total_connections_received'
docker --context desktop-linux exec redis cat /proc/1/limits
docker --context desktop-linux logs redis 2>&1 | Select-String 'maxclients|open files|backlog'
docker --context desktop-linux exec redis redis-cli client list > run/logs/p0e-maxclients/s3-client-list-idle.txt
```
- 容器名出处 docker-compose.yml:219。
- 本机共享 Redis 是 redis:latest(:218),与 K8s 的 7.2 不是同一版本。要记下 redis_version;本机的服务端文本不能直接当 K8s 的结论。

按进程数连接:
```
Get-NetTCPConnection -RemotePort 6379 -State Established -ErrorAction SilentlyContinue | Group-Object OwningProcess | ForEach-Object { $p = Get-Process -Id ([int]$_.Name) -ErrorAction SilentlyContinue; [pscustomobject]@{ Pid = $_.Name; Process = $p.ProcessName; Conns = $_.Count; Path = $p.Path } } | Sort-Object Conns -Descending | Format-Table -AutoSize
```
- Get-NetTCPConnection 不可用时,改用 `netstat -ano | Select-String ':6379 '`。
- 按进程路径排除无关进程。Java 版仓库的 compose 也默认占宿主 6379(Java 仓库 deploy/compose/infra.yaml:64)。

聚合 CLIENT LIST:
```
$rows = Get-Content run/logs/p0e-maxclients/s3-client-list-idle.txt | ForEach-Object { $kv=@{}; $_ -split ' ' | ForEach-Object { $i=$_.IndexOf('='); if($i -gt 0){ $kv[$_.Substring(0,$i)]=$_.Substring($i+1) } }; [pscustomobject]@{ Key = 'lib={0}|flags={1}|sub={2}|name={3}' -f $kv['lib-name'],$kv['flags'],$kv['sub'],$kv['name']; Mem = [long]$kv['tot-mem'] } }
$rows | Group-Object Key | Sort-Object Count -Descending | Format-Table Count,Name -AutoSize
$rows | Measure-Object Mem -Sum -Average
```
- lib-name 为空的一组 = C++ 加 login 加 db。login、db 关了 CLIENT SETINFO(go/login/internal/svc/servicecontext.go:115;go/db/internal/svc/servicecontext.go:28、:41)。一律以进程计数为准。

读各 Go 服务的指标口:
```
9101,9190,9150,9160,9170,9180,9210,9220,9230,9260 | ForEach-Object { "== $_"; curl.exe -s "http://127.0.0.1:$_/metrics" | Select-String 'gomaxprocs|redis_client_pool' }
```
- 端口出处:各服务 etc/*.yaml 的 Prometheus 段,例如 login.yaml:348、player_locator.yaml:69、guild.yaml:216。
- 指标名是外部常识:有就记数值,没有就记「无此指标」。

期望:
- maxclients 10000;name 全空。
- 每个 C++ 进程恰好 1 条:node.cpp:557 是唯一建连点。
- 网关 JVM 1 条:本机限流缺省关(application.yaml:129);限流开时 2 条(RateLimitConfig.java:50、:61)。
- go-zero 服务(scene_manager、match、chat、friend、data_service)各约 8 条常驻,待证实。
- login 至少 2 条,其中 1 条 sub=1(task_result_dispatcher.go:227)。
- db、player_locator、trade 各至少 1 条。
- guild 至少 3 条(servicecontext.go:57、:72、:180)。

上限核对:
- 每个 Go 进程的连接数 ≤ 客户端对象数 × 10 × G_local。login 再加 1;data_service 按 2 个池算;guild 按 3 个客户端算。
- G_local 取值:该栈起栈时设了 GOMAXPROCS 就用它;没设就是本机逻辑核数(Windows 没有 cgroup,容器感知不生效,外部常识)。结果里写明取的是哪个。
- 超过上限即说明总账的每副本上限算错了,原样上报。

交付:一张「进程 × 连接数」表,外加 CLIENT LIST 聚合结果和每连接内存均值。

【S6 K8s 实机只读核对】【条件:已有在跑的 K8s 环境】约 5 分钟
- 不为此新建集群。新建 kind 或拉镜像都须负责人同意。
- kind 的约定:context 是 kind-mmorpg,kubeconfig 是单独文件(deploy/k8s/AGENTS.md:48-50)。
```
$kc = @('--kubeconfig', "$env:TEMP\kind-mmorpg.kubeconfig", '--context', 'kind-mmorpg')
kubectl @kc get pods -A -o wide
kubectl @kc -n mmorpg-infra exec redis-0 -c redis -- cat /proc/1/limits
kubectl @kc -n mmorpg-infra exec redis-0 -c redis -- cat /proc/sys/net/core/somaxconn
kubectl @kc -n mmorpg-infra logs redis-0 -c redis | Select-String 'maxclients|open files|backlog'
kubectl @kc -n mmorpg-infra exec redis-0 -c redis -- sh -c 'if [ ${REDIS_PASSWORD:+x} ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; redis-cli config get maxclients; redis-cli info clients; redis-cli info stats | grep rejected_connections; redis-cli client list'
```
- 出处:
  - namespace 缺省 mmorpg-infra(k8s_deploy.ps1:24);
  - Pod 名 redis-0、容器名 redis(redis.yaml:65、:82);
  - 口令经 REDISCLI_AUTH 传入,同 k8s_gate_drain.ps1:640 的取法。

各 Go 服务的 GOMAXPROCS:
- 每个服务取一个 Pod。zone 命名空间以第一条命令列出的 `mmorpg-zone*` 为准;端口以该 Pod 的 ConfigMap 为准,需 Codex 先确认。
```
kubectl @kc -n <zone 命名空间> logs deploy/login | Select-String 'maxprocs|GOMAXPROCS'
kubectl @kc -n <zone 命名空间> exec deploy/login -- wget -qO- http://127.0.0.1:9101/metrics | Select-String 'gomaxprocs|redis_client_pool'
```
- 对 db、data-service、player-locator、scene-manager,以及 infra 命名空间里的 match、chat、friend、trade,各做一次。

期望:
- redis-0 的 nofile 硬限不低于 10032;`CONFIG GET maxclients` 为 10000;日志没有下调告警。
- 各 Go 服务 G=2。依据:Go 1.27.2 的容器感知,500m 和 "1" 的 CPU limit 向上取整、下限 2(外部常识)。若 S2b 证实 automaxprocs 被链入并生效,可能是 1。明显大于 2 就按「更糟」上报。
- 集群内 CLIENT LIST 的 addr 是 Pod IP,可以按第一条命令的输出归到各 Pod。
- 本机 kind 回答不了生产运行时的情况。生产或预发的这两个量,由负责人在目标集群上取。

【R0 回归测试跑红】【条件:Claude 已在主工作区写好 tools/scripts/tests/redis_maxclients_contract.tests.ps1,见 pre_measure_code】约 1 分钟
```
Copy-Item D:/luyuan/xuanming-server-mmo-cpp/tools/scripts/tests/redis_maxclients_contract.tests.ps1 (Join-Path $W 'tools/scripts/tests/')
pwsh -NoProfile -File (Join-Path $W 'tools/scripts/tests/redis_maxclients_contract.tests.ps1')
```
- 期望:退出码非 0;T1~T3 以「查不到 MAXCLIENTS」一类文本失败。T4 只在负责人决定 compose 同改时才存在。
- 退出码为 0 说明测试写错了,原样上报。

══════ 第二组:受阻(阻塞原因与解除条件见 blocked 字段)══════

【S3b 自起整栈空闲采样】【受阻】解除后约 25 分钟
- 阻塞:没有 C++ 节点 exe(C1083 boost/any.hpp);没有 install_vs2026_dbg;没有网关 jar。
- 解除后,在构建产物所在目录执行:
```
pwsh -NoProfile -File tools/scripts/start_game.ps1 -CheckOnly
$env:GOMAXPROCS = '1'
pwsh -NoProfile -File tools/scripts/start_game.ps1
Remove-Item Env:GOMAXPROCS
```
- 设 GOMAXPROCS=1,让每个池的上限与 K8s 的 A 档相同(10 条)。整栈因此起不来时,去掉这一行重起,并在结果里注明。
- 不要用 dev-start-exe:它不起网关(dev_tools.ps1:1459-1464)。
- 起稳后等 2 分钟,采样命令与期望同 S3a,G_local 取 1。
- 出处:start_game.ps1:22(-CheckOnly)、:366-371(Java ≥ 23 与 jar)、:388-389(三个 exe)、:445-446(基础设施)、:551(gRPC 运行库)、:580-606(gate、scene、battle、Go、网关的启动顺序)。

【S4 轻负载与不回落】【受阻:依赖 S3b;或者 S3a 的现成栈带网关和 gate】约 20 分钟
- 用冒烟配置让连接池涨起来:
```
Set-Location robot; go run . -c etc/robot_smoke.yaml
```
- 机器人数 3(robot/etc/robot_smoke.yaml:5);网关地址 http://127.0.0.1:8081(:3);运行方式见 robot/README.md:13。
- 认证方式是否匹配当前栈,需 Codex 先确认(robot_smoke.yaml 用口令认证)。
- 不用压测配置,不走 stress_round19.ps1(AGENTS §6)。
- 在三个时刻各重复一遍 S3a 的「服务端 info clients」与「按进程计数」,文件名用 s4-*:机器人在线时;机器人退出后立刻;退出 5 分钟后。
- 期望:
  - 任何时刻每个 Go 进程都不超过 S3 写的上限;
  - 退出 5 分钟后的连接数等于在线时的峰值,即不回落。若回落,与静态读码(pool.go:746、:991、:1091)矛盾,原样上报。
- 这一步不产出「P0 还是加固项」的结论。

【S5 到线时 Go 客户端的表现】【条件且有破坏性:需在跑的本机开发栈(S3a 或 S3b)+ 负责人同意;没有栈即受阻】约 15 分钟,可选
- 影响:期间本机登录与存盘会失败。不清数据,不重启基础设施,不重启 C++ 节点。裸重启会丢一键启动设的运行模式环境变量,出处 start_game.ps1:564、:580;cpp/nodes/gate/main.cpp:80。
- 在该栈自己的目录 D_stack 执行:pid 和日志按目录存放(go_services.ps1:85-87、:662-663)。ORIG、P 换成实际读到的数字。
```
Set-Location <D_stack>
docker --context desktop-linux exec redis redis-cli config get maxclients
docker --context desktop-linux exec -d redis sh -c '(sleep 300; echo config set maxclients ORIG) | redis-cli'
docker --context desktop-linux exec redis redis-cli info clients
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command go-svc-stop -GoServices player_locator,chat
docker --context desktop-linux exec redis redis-cli info clients
docker --context desktop-linux exec redis redis-cli config set maxclients P
docker --context desktop-linux exec redis redis-cli ping
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe -GoServices player_locator,chat -TierReadySeconds 30
Get-Content run/logs/go_services/player_locator.stderr.log -Tail 40
Get-Content run/logs/go_services/chat.stderr.log -Tail 40
Select-String -Path run/logs/go_services/*.log -Pattern 'max number of clients|EOF|connection reset|broken pipe|failed to authenticate|failed to connect Redis|circuit breaker|redis:' | Select-Object -Last 60
```
- 第 2 条是自动恢复:这条连接现在就建好,300 秒后在同一条连接上把上限改回 ORIG。
- 第 3 条核对 connected_clients 比挂之前多 1;没多就停下,不要往下做。
- P = 第 5 条读到的 connected_clients 减 1(减掉查询自己那条)。设完后名额恰好占满,第 7 条的 ping 应被拒。
- 出处:
  - 启停写法与一键启动相同:start_game.ps1:341;dev_tools.ps1:1440-1441;
  - player_locator 是 go-redis v9.16.0 直连,Ping 失败即 panic,文本是 `failed to connect Redis`(go/player_locator/internal/svc/servicecontext.go:34-35);
  - chat 是 go-zero(go/chat/internal/svc/servicecontext.go:74)。
- 收尾:从第 2 条起算满 300 秒后执行:
```
docker --context desktop-linux exec redis redis-cli config get maxclients
docker --context desktop-linux exec redis redis-cli info stats | Select-String rejected_connections
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe -GoServices player_locator,chat -TierReadySeconds 30
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command dev-status
```
- 期望:
  - 两个服务在到线时启动失败;
  - 日志里多半是 EOF、连接重置或 failed to connect Redis,不一定出现 max number of clients;
  - rejected_connections 有增量;
  - 300 秒后上限回到 ORIG,两个服务能起来。
- 兜底:
  - 上限没恢复时,再停一个 Go 服务腾出名额,手动 `config set maxclients ORIG`;
  - 最后手段是 `docker --context desktop-linux restart redis`(CONFIG SET 不持久,重启即复原),须负责人同意。

══════ 收尾与失败保留 ══════
- 结果连同 W/run/logs/p0e-maxclients/ 的内容交付、负责人确认之后,才删除 W:
```
Set-Location D:/luyuan/xuanming-server-mmo-cpp
git worktree remove --force $W
```
- S2 的一次性容器都带 --rm,stop 即删。
- 失败时保留:每一步的完整标准输出与错误输出;S5 的日志匹配行前后各 20 行。
