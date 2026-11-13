# 开服线 Codex 验证单(2026-10-20 按当前树重写)

> **用途**:交接 [handoff-launch-line-remaining-20261019.md](../handoff-launch-line-remaining-20261019.md) §3 第 1 项。本单**取代**各规格文末的 `verify_commands`,以及 PROGRESS 各条目里给 Codex 的命令;两边冲突时以本单为准。
> **状态**:未执行。Claude 只读核对了 HEAD `c63b22dca` 加 2026-10-20 的工作区,没有跑任何编译、测试或生成(AGENTS §10.1)。
>
> 核对之后,用户提交了 `c35e1133d`,又合并成 `9a9390035`。这两次提交里涉及本线文件的只有两类,都是把核对时已在工作区的内容入库,不是新改动:
> - `redis_client.h`、`currency_test.cpp`:他人的改动;
> - `load_refresh.go`、`load_reporter.go`:EnterScene 陈旧位置会话的改动。
>
> 两个存盘脚本常量与本线用例都没变。
> **覆盖范围**:B0-2、B0-5、B0-4、B0-3 两批、B0-1,外加 S5a 投递账本的 C++ 用例(交接 §3 第 6 项最后一条)。**D1 1b-1 不在本单里**:D1 的文件现归落点线,验证单由 [placement-line-handoff-20261019.md](../placement-line-handoff-20261019.md) §4 第 2 步重写。

## 0′. 2026-11-11 现状(先读这节)

本单写于 10-20。到 11-11,大部分对象已被后来的改造删掉或重写,**只有下表标「仍要跑」的部分还有效**,其余各节只作历史记录保留:

| 节 | 对象 | 11-11 现状 | 还要不要跑本节 |
|---|---|---|---|
| §8 B0-2 | 存盘 Lua 的 `dirty_keys_set` 写入 | 玩家存档 10-31 起只存 TiDB,`kSaveLuaScript` / `kSaveIfGuardLuaScript`、`MessageSyncRedisClient` 与死工程 `redis_test` 都已不在树里 | 不用,对象已删 |
| §6 B0-3 | task:result 读回执分频道 | `go/shared/taskresult`、login 的 dispatcher、`DBTask.reply_instance` 都已随读回执链路删除 | 不用,对象已删 |
| §9 S5a | 投递账本 `DbTaskDispatch*` | `db_task_dispatch_test.cpp` 与 `DbTaskDispatchSystem` 已随存盘改直写删除 | 不用,对象已删 |
| §4 B0-5 | scene_manager 按 zone 批量刷新 | 已被「控制面按 zone 化」(缺口清单第 4、5 条)重写,`zone_registry.go` 已删;按 zone 选主、只 watch 本 zone、换 zone 分支走领导者收尾都已落地。验证以 `docs/design/control-plane-zone-scoping-20261105.md` §8 为准(Go 11-06、C++ 11-08 已跑通,部署契约与 kind 冒烟未跑) | 不用,改跑那边的 §8 |
| §7 B0-1 | player_locator 按 gate 建索引 | 已改成按 slot 分片的死 gate 清扫(`gate_sweep.go`),存活集合理性闸门(清单 B1-1)也已落码。验证以 PROGRESS「2026-10-31 玩家态 Redis 集群化第 7 批」「2026-11-10 去单点批 C2」两条为准 | 不用,改跑那两条 |
| §5 B0-4 | nofile 启动门槛 | `open_file_limit.h` 与 `open_file_limit_test.cpp` 字节未变(`node.cpp`、vcxproj 有他人改动)。Windows 侧:Codex 11-04 的整套运行里 `OpenFileLimit.*` 21/21 通过(`build/verification/loglevel-no-popup-20261105T021421360Z/client_endpoint_test/client_endpoint_test.stdout.log`)。**Linux 分支从未编译、运行过** | **仍要跑**:§5 第 4~8 步,都要先取得授权(Docker 构建、容器、kind) |
| 部署接线 | `-NodeOpenFileLimitMode`、`maxUnavailable: 0`、`-WaitReady` 捞拒启行 | 已落码,命令在 PROGRESS「2026-10-22 B0-4 部署侧后续项」「B0-4 部署侧:包装入口透传 -NodeOpenFileLimitMode」两条;回滚脚本那部分已随 `k8s_zone_rollback.ps1` 退役 | **仍要跑**:按那两条执行 |
| 新增 | login 注释与 CRITICAL 日志文案(11-11) | 见 PROGRESS「2026-11-11 开服线收尾核对」 | **仍要跑**:按那一条执行 |
| 新增 | B0-4 当时登记的后续项(11-12):accept 资源耗尽不再崩溃、fd 用量指标与告警、致命日志写 stderr、余量定案 | 已落码,未编译。`open_file_limit.h` 只改了注释,上一行说的「字节未变」从这次起不再成立,`OpenFileLimit.*` 21 条要重跑。命令见 PROGRESS「2026-11-12 开服线:B0-4 后续项现在做完」 | **仍要跑**:按那一条执行;其中容器步骤并入 §5 第 4~8 步 |

## 0. 为什么重写

只读核对(各项一名核查员,B0-5 另配一名复核)的结论:五项落码在当前树里**都完整生效**,fc3ff045e 合并没有带回旧逻辑,`k8s_deploy.ps1` 也没有重复函数定义。但原命令照着跑会误报:

- **红灯基线**:规格写的是「在 HEAD 上取红」,可代码早已入库,在 HEAD 上取到的是假绿。B0-5、B0-2 要用 `a105c3efd`,B0-4、B0-3、B0-1 要用 `360d56ae5`。两个基线都已用 `git cat-file -t` 确认是 commit。
- **用例名过时**:
  - B0-5 规格的正则漏了 2 条红灯用例、9 个新用例,还引用了已删除的 `TestBootstrapZoneRegistryFromScan`。
  - B0-1 规格的正则漏了 62 个用例里的 19 个,点名的两个用例也不存在。
  - B0-4 规格指向不存在的 `open_file_limit_test` 工程,期望行里漏了 `mode=enforce`。
- **工具或命令不存在**:`dev_tools.ps1 -Command etcd-up` 不存在;PROGRESS 里提到的 B0-1、B0-5「交付说明 / 验证清单」不在仓库里。
- **SKIP 会冒充通过**:merge_zone 集成测试和 player_locator integration 用例在依赖缺失时 SKIP、退出码为 0。
- **范围核对必然误报**:工作区里有大量他人改动,`git diff` 全量对比会把别人的改动算成本线越界。

## 1. 通则(每一步都适用)

- **环境**:同一个 PowerShell 父会话里先设:

  ```
  $env:GOTOOLCHAIN='local'; $env:GOPROXY='off'
  foreach ($n in 'ASSET_FENCE_TEST_DSN','ASSET_FENCE_REQUIRE_TIDB','GODB_REDIS_ITEST_ADDR','KAFKA_INTEGRATION_BROKER','KAFKA_DURABILITY_BROKERS','DBTASK_AUDIT_DUMP','DBTASK_AUDIT_PARTITIONS') { Remove-Item "Env:$n" -ErrorAction SilentlyContinue }
  Get-ChildItem Env:OWNERSHIP_FAULT_* -ErrorAction SilentlyContinue | Remove-Item
  Get-ChildItem Env:MMORPG_* -ErrorAction SilentlyContinue | Remove-Item
  ```

  其他约束:
  - 不改 go.mod / go.sum,不 `gofmt -w`,不改断言,不跳过用例。
  - go/test.ps1 不会自己设这些变量,子进程继承父会话的环境。
- **快照**:开跑前和收尾时各记一次 `git rev-parse HEAD` 与 `git status --short`。工作区同时有多个会话在写,其中 `k8s_deploy.ps1`、`k8s_deploy_contract.tests.ps1` 在核对期间仍在变。
- **归因**:红了先看失败落在哪个文件。
  - 落在本线文件上(各节「本线文件」所列)→ 本线问题,交回 Claude 修。
  - 落在别的文件上 → 如实写明归属的会话或文件,不顺手改。
  - 已知会混进他人改动的位置:
    - `go/login` 的 dataloader / clientplayerlogin / config(login 持久 schema C4);
    - `go/db/internal/kafka`(persist_schema、53361d055);
    - `go/scene_manager` 的 enterscenelogic / leave_release* / load_reporter 里的 `isZoneGoneFromRegistry`;
    - `tools/merge_zone`(D1);
    - `cpp/libs/engine/infra/storage/redis_client/redis_client.h` 与 `cpp/tests/currency_test/currency_test.cpp`(两者都有他人数百行未提交改动)。
- **红灯 worktree**:建在 `$env:TEMP` 下,只拷贝下面点名的测试文件进去;跑完必须 `git worktree remove --force`,并用 `git worktree list` 确认已删除。不在主工作区 stash 或 reset。
- **失败处理**:前一步红就停下归因,不连续重试。保留首个失败的完整 `-v` 输出。
- **需要先问负责人的**:标「需授权」的步骤(拉镜像、联网构建、起容器、动 kind 集群),没有授权就如实写「未运行」。

## 2. 已有的运行证据(只作归因参考,不能代替本单)

| 项 | 已有证据 | 为什么不能直接沿用 |
|---|---|---|
| B0-5 | 10-19 07:46Z,「会话释放覆盖身份」验收顺带跑了 go/scene_manager 的 `go vet ./internal/...` 和 `go test ./...`,原生 0;B0-5 的 35 个用例全部 PASS(`build/verification/session-release-cover-acceptance-20261019T074117806Z/`) | 之后 load_refresh.go、load_reporter.go 有他人改动,fc3ff045e 又新增了 metrics / config;红灯和 merge_zone 部分都没跑 |
| B0-4 | 10-18 06:43Z 增量 Build 原生 0,OpenFileLimit 21/21;07:52Z 完整 Rebuild 原生 0。4 个本线文件的哈希与当前一致(见 §5 第 1 步) | 只覆盖 Windows;真正起作用的 `#ifdef __linux__` 分支从未被任何编译器编过 |
| B0-3 | 10-19 约 04:56Z,六个 Go 服务私有构建,原生 0;Java ContractSync 的 fullsync / fullcheck 原生 0 | 早于 53361d055;红灯、定向用例、L1 都没跑 |
| B0-1 | 10-19 07:56Z,location-release 线跑了 player_locator 的 vet 与全量测试,原生 0,117 个顶层用例 PASS;07:53Z 契约测试里 player-locator 两条 PASS | 之后有 37 个依赖或生成文件变了;红灯、gofmt、integration 没跑 |
| B0-2 | 无 | — |

## 3. 总顺序

1. proto 只读核对(§6 第 2 步)。**不重跑生成**:c63b22dca 改了生成器源码,工作区里还有他人在途的 proto 改动,重跑会替他们写生成物。真要重生成,并入统一生成窗口:先 `proto-gen-build`,再 `proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
2. 各项红灯:§4 第 1 步、§6 第 3、4 步、§7 第 2 步。
3. Go 绿灯:§4 第 2~6 步、§6 第 5~8 步、§7 第 3~7 步;然后 `pwsh -File go/test.ps1`(§6 第 10 步),再 `robot` 的 `go build ./...`(§6 第 11 步)。
4. C++:一次 `game.sln` 整解 Build(§5 第 2 步)。之后依次跑 currency_test(§8)、client_endpoint_test(§5 第 3 步)、cross_zone_test(§8、§9)、bag_test(§8)。
5. PowerShell 与文件解析:k8s_deploy_contract(§7 第 8 步)、test_stress_summarize(§6 第 9 步)、告警与看板解析(§4 第 7 步)、文档条目(§7 第 9 步)。
6. 需授权的可选项:§4 第 6 步、§5 第 4~8 步、§7 第 10~11 步、§8 第 4~5 步。

## 4. B0-5 scene_manager 按 zone 批量刷新

本线文件:
- `go/scene_manager/internal/logic/` 下的 `load_refresh*.go`、`zone_registry*.go`、`orphan_cleanup.go`、`world_autoscale.go`、`leader_gate.go`,以及 `load_reporter.go` 里的 updateNodeLoad、refreshLoadScores、fullSync、sweepStaleLoadSetMembers;
- `internal/metrics/metrics.go` 的 loadRefresh*;
- `tools/merge_zone/scene_hot_state*.go`;
- `deploy/k8s/scene-manager-alerts.yaml` 的 4 条 LoadRefresh 告警。

1. **红灯**(基线 `a105c3efd`):

   ```
   $red = Join-Path $env:TEMP 'sm-red-b05-a105c3efd'
   git worktree add --detach $red a105c3efd
   Copy-Item go\scene_manager\internal\logic\load_refresh_scope_test.go (Join-Path $red 'go\scene_manager\internal\logic\')
   Push-Location (Join-Path $red 'go\scene_manager')
   go test ./internal/logic/ -count=1 -v -run '^(TestRefreshLoadScores_(FollowerIssuesNoRedisCommands|FollowerPublishesOnlyOwnZone|LeaderDemotionDropsNodeSeries|LeaderRefreshesEveryZoneAndHealsMembership|LeaderBatchesScoreWritesPerZone|ReadErrorKeepsPreviousScore|EmptyCounterValueKeepsPreviousScore|NonMemberReadFailureJoinsLast|RegistryWriteFailureStillWritesLoadSet)|TestSweepStaleLoadSetMembers_UsesZoneRegistryNotScan|TestCleanupOrphanWorldChannels_DiscoversViaRegistryWithoutScan|TestDeleteOrphanChannel_KeepsDesiredFieldUntilSetDeleted|TestDesiredWorldChannelCount_SeedRegistersZone)$' 2>&1 | Tee-Object $env:TEMP\b05-red.log
   Pop-Location; git worktree remove --force $red
   ```

   期望:恰好 13 行 `--- FAIL:`、0 行 `--- PASS:`,退出 1;包能编译。其中 `LeaderRefreshesEveryZoneAndHealsMembership` 只在登记断言上失败。如果是编译失败,保留首条错误交回 Claude,不改基线。
2. **gofmt / build / vet**:在 `go/scene_manager` 下逐个文件 `gofmt -l`,不按目录扫(internal 下有已知的他人格式差异)。文件依次是:
   - `internal/logic/` 下:`load_refresh.go`、`zone_registry.go`、`load_reporter.go`、`orphan_cleanup.go`、`world_autoscale.go`、`leader_gate.go`、`world_channel_directory.go`、`load_refresh_scope_test.go`、`load_refresh_test.go`、`zone_registry_test.go`、`node_detach_deferred_test.go`、`nodes_by_role_metrics_test.go`、`world_channel_directory_test.go`;
   - `internal/metrics/metrics.go`;
   - `internal/config/config.go`。

   然后 `go build ./...`、`go vet ./...`。期望:gofmt 无输出,build 与 vet 都是 0。
3. **稳定性**:`go test ./internal/logic/ -count=10 -run '^(TestRefreshLoadScores_|TestUpdateNodeLoad_|TestSweepStaleLoadSetMembers_|TestCleanupOrphanWorldChannels_|TestOrphanCleanup_|TestDeleteOrphanChannel_|TestDesiredWorldChannelCount_|TestSceneManagerZonesKey_|TestListRegisteredZones_|TestUnionZones_|TestRegisterSceneManagerZones_|TestRegisterOwnZoneOnStartup|TestClassifyCounterReply|TestPipelineBatchRejected|TestPipelineHitTransportError|TestLoadRefreshReportOutcomePriority|TestLoadRefreshOutcomesArePreseeded|TestResetLeaderGaugesDropsNodeSeries|TestNodeRedisStateIsScopedByZoneAndNodeID|TestParseWorldChannelsKey|TestWorldChannelDirectoryKeyFmt_CrossLanguageContract)'`。期望退出 0。
4. **逐例确认**:用同一个正则加 `-count=1 -v` 跑一遍,再把 `load_refresh_scope_test.go`、`load_refresh_test.go`、`zone_registry_test.go` 里的 `^func (Test\w+)\(` 逐个对日志里的 `--- PASS:`。期望 35 个(13 + 16 + 6)全部 PASS,missing=0。
5. **整包**:`go test ./... -count=1`,期望退出 0。用例数比 07:46Z 那次多,因为 10-19 EnterScene 陈旧位置会话新增了用例。失败先按文件归因。
6. **merge_zone**(该目录现归 D1,有大量未提交改动):
   - 先在 `tools/merge_zone` 下跑 `gofmt -l scene_hot_state.go scene_hot_state_test.go` 和 `go test -count=1 -v -run '^TestSourceZoneRegistryMember$' .`;
   - 再跑 `go vet ./...` 与 `go test -count=1 ./...`。不加 `-race`:本机 CGO_ENABLED=0。
   - **集成测试(需授权)**:设 `MERGE_ZONE_IT_MYSQL_DSN=<一次性 MySQL 基址,不带库名>`、`MERGE_ZONE_IT_REDIS_ADDR=<DB 9/10/11 为空的 Redis>`、`MERGE_ZONE_IT_REQUIRE=1`,然后跑 `go test -tags merge_integration -count=1 -v -run '^TestIT_ClearHotState_RemovesZoneFromSceneManagerRegistry$' .`。
     - 必须是 `--- PASS`,出现 SKIP 不算通过。
     - TestMain 会 `go build` 含 D1 在途代码的 merge_zone,构建失败归 D1。
     - 不借 codex-* 容器。
7. **告警与看板**:
   - `deploy/k8s/scene-manager-dashboard.json` 用 `json.load` 解析;
   - `scene-manager-alerts.yaml` 用 `yaml.safe_load_all` 解析。本机没有 PyYAML 就记「未验证」,不安装。
   - 告警总数现为 **25**:条目里写的 23 已过时,e7bb46070、8b9592de3 各加了 1 条。
   - `SceneManagerLoadRefresh(Stalled|Failing|Slow|NodeErrors)` 四条都在。
   - `git diff --name-only a105c3efd -- go/scene_manager/go.mod go/scene_manager/go.sum tools/merge_zone/go.mod tools/merge_zone/go.sum` 无输出。
   - 入库范围用 `git show --stat e76f6f747` 核对,不用工作区 diff。

## 5. B0-4 启动时抬 RLIMIT_NOFILE

本线文件:
- `cpp/libs/engine/core/node/system/node/open_file_limit.h`;
- `node.cpp` 里 `EnsureOpenFileLimitOrDie` 的调用;
- `cpp/tests/client_endpoint_test/open_file_limit_test.cpp` 及其 vcxproj 登记。

1. **哈希核对**:`Get-FileHash` 这四个文件:`open_file_limit.h`、`node.cpp`、`open_file_limit_test.cpp`、`client_endpoint_test.vcxproj`。
   - 期望依次为(2026-10-20 实测,与 10-18 Codex 证据相同):
     - `31BE1A484834E2E80A257E8C84735C1BA0CE147F7CBD53BA3D55A3B4ACD5A981`
     - `2486EE2842D699F932545E57FBAC29445C0B461E6FEC25717A3559737CCB1683`
     - `495A77A283AC5C4595315A0D60C618311D60724C429F9FAB160066265A2D4789`
     - `CAF7F5B83BD346F661E9C50BA012E9CDEF358122720EE91F8F5217156B1CEB74`
   - 相同,说明 10-18 的 Windows 证据对应当前字节;不同就以新跑结果为准。
2. **整解 Build**(与总顺序第 4 步是同一次,不另起 MSBuild):
   - 命令:`msbuild game.sln /t:Build /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /p:CL_MPCount=1`。
   - 开跑前用 `Get-Process MSBuild,cl,link` 确认没有其他构建在跑。
   - **不要逐个单编 gate / battle 的 vcxproj**:这两个工程没有 ProjectReference,单编会链接旧库,出现假绿或无关的链接错。
   - 首错不在本线文件上时,归他线并停下。
3. **单测**:先 `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter client_endpoint -LogDir build/verification/b0-4-<ts>/cpp-tests`,再 `build\cpp\tests\client_endpoint_test.exe --gtest_filter=OpenFileLimit.*`。
   - 期望:client_endpoint_test 全过(10-18 是 64/64);OpenFileLimit 21/21。
   - 纯函数单测没有修复前红:被测对象在 360d56ae5 里不存在。行为上的红由第 6、7 步的容器对照给出。
4. **(需授权)Linux 预检**:
   - 跑 `docker --context desktop-linux version` / `info` / `image ls`,看引擎是否可达、gcc:13.4.0 与 ubuntu:24.04 是否已在本地、有没有现成的 `mmorpg-node` 旧镜像。
   - 10-19 记录过本机 Linux API 管道不存在、可用内存约 2.5GiB。不可达或要拉镜像就停下来问。
5. **(需授权)新镜像**:`docker --context desktop-linux build --network=none -f deploy/k8s/Dockerfile.cpp --build-arg BUILD_JOBS=2 -t mmorpg-node:nofile-new .`。
   - 期望:三个二进制里 `[open_file_limit]` 的计数都 ≥1。
   - 在 deps 阶段因为没网失败,就停下来问是否允许联网。
   - 这是 `#ifdef __linux__` 分支第一次被编译,首错要保留原文。
6. **(需授权)容器红绿**:
   - 公共参数:`-v <bin\etc>:/app/bin/etc:ro -w /app/bin`;用 `sh -c` 包一层,不让节点当 PID 1。
   - 用例:
     - (a) gate,`--ulimit nofile=1024:1024`:拒启;
     - (b) gate,`1024:524288`:抬到 524288;
     - (c1) scene,`4095:4095`:拒启,文案含 `no client-facing cap`;
     - (c2) scene,`4096:4096`:ok;
     - (d1) battle,`8191:8191`:拒启;
     - (d2) battle,`1024:8192`:抬到 8192;
     - (e) `NODE_OPEN_FILE_LIMIT_MODE=warn`:有 WARN 行、不拒启;
     - (f) `=WARN`:报非法取值,回落 enforce 后拒启。
   - 期望启动行(gate 正常时):`[open_file_limit] role=gate verdict=ok mode=enforce required=24096 client_cap=20000 cap_key=GateMaxConnections reserve=4096 soft_before=1024 soft=524288 hard=524288 raise=ok`。拒启行是 `FATAL open file limit config: role=gate soft=1024 hard=1024 required=24096 ...`。
   - 退出码只记实际值,不断言 134。
7. **(需授权)修复前对照**:
   - 优先用本机现有的旧镜像:`/app/bin/gate` 里 `[open_file_limit]` 的计数必须是 0。
   - 没有旧镜像要从 360d56ae5 构建,这需要 submodule 初始化和联网,先问。
   - 在旧镜像上跑 (a)(b):没有 `[open_file_limit]` 行;(b) 的软限仍是 1024。
8. **(需授权,只在 dev kind)** 按 `docs/ops/gate-kernel-tuning-runbook.md` §3.1 读各类 Pod 的 nofile 硬限(E11),记进 PROGRESS。`bash -n tools/scripts/deploy-staging.sh` 只做语法检查,实跑写「未运行」。

## 6. B0-3 task:result 读回执分频道(两批)

本线文件:
- `go/shared/taskresult/`;
- `go/login/internal/dispatcher/`;
- dataloader 里 reply_instance 盖章那几处;
- `go/db/internal/kafka/task_result_notify*.go`;
- `go/db/internal/metrics/task_result_notify*.go`;
- go/db 里 `publishDBReadResult` / `failRead` 调 `publishTaskResult` 的那两行;
- `deploy/k8s/task-result-notify-alerts.yaml`;
- stress_summarize 的 2g 段。

1. **快照**:记 `git status --short -- proto/db go/proto/db go/shared/taskresult go/login/internal/dispatcher go/login/internal/logic/pkg/dataloader go/login/internal/logic/clientplayerlogin go/login/internal/config go/db/internal/kafka go/db/internal/metrics`。
2. **proto 只读核对**(替代重跑生成):
   - `go/proto/db/db_task.pb.go` 与 `robot/vendor/proto/db/db_task.pb.go` 里 `func (x *DBTask) GetReplyInstance` 各 1 处,两个文件哈希相同;
   - `cpp/generated/proto/db/db_task.pb.h` 含 `reply_instance`;
   - generated 下 4 份 `db_task.proto` 都含 `string reply_instance = 10;`;
   - 这些文件 `git status` 无输出。
3. **第 1 批红灯**(基线 `360d56ae5`):
   - `git worktree add --detach $env:TEMP\b03-red-360d56ae5 360d56ae5`,拷入 `go/login/internal/dispatcher/task_result_dispatcher_subscribe_test.go`;
   - 在 worktree 的 go/login 下跑 `go test -count=1 -v -run 'TestStart_SubscribesLegacyAndInstanceChannels' ./internal/dispatcher/`。
   - 期望 FAIL,信息是 `timed out after 5s waiting for legacy and per-instance subscriptions`。worktree 留给下一步。
4. **第 2 批红灯**(同一个 worktree):
   - 拷入当前的 `go/proto/db/db_task.pb.go` 与 `go/db/internal/kafka/task_result_notify_route_test.go`;
   - 在 worktree 的 go/db 下跑 `go test -count=1 -v -run 'TestPublishDBReadResult_NotifiesReplyInstanceChannel|TestFailReadTask_NotifiesReplyInstanceChannel|TestEncodeRetryPayload_PreservesReplyInstance' ./internal/kafka/`。
   - 期望:前两例 FAIL,期望频道 `task:result:notify:<uuid>`,实际 `task:result:notify`;第三例 PASS,它是兼容守卫。
   - 跑完删除 worktree。
5. **go/shared**:`gofmt -l taskresult; go vet ./taskresult/; go test -count=1 -v ./taskresult/`,全部通过。
6. **go/login**:
   - `gofmt -l internal/dispatcher internal/logic/pkg/dataloader`;
   - `go vet ./internal/dispatcher/ ./internal/logic/pkg/dataloader/ ./internal/logic/clientplayerlogin/`;
   - `go test -count=1 -v -run 'DeliverySeries|NewTaskResultDispatcher_|TestDispatch_|RunDispatchJob|SweepExpired|TestCancel_|TestStart_|NewDBReadTask|SendDBReadRequests|StampsRegistrar|SendFailureCancels' ./internal/dispatcher/ ./internal/logic/pkg/dataloader/`;
   - 两个包整包测试,最后 `go build ./...`。

   本项的绿以定向用例为准。dataloader 与 clientplayerlogin 有 C4 会话的在途改动,非本项用例的失败注明归属。
7. **go/db**:
   - `gofmt -l` 本批新建的 5 个文件:`internal/kafka/task_result_notify.go`、`internal/kafka/task_result_notify_route_test.go`、`internal/kafka/task_result_notify_test.go`、`internal/metrics/task_result_notify.go`、`internal/metrics/task_result_notify_test.go`。
   - 对 `key_ordered_consumer.go`、`placement_route.go`、`metrics.go` 也跑 `gofmt -l`:有差异时只看是否落在本批那三处上,其余只报告。
   - `go vet ./internal/kafka/ ./internal/metrics/`。
   - `go test -count=1 -v -run 'TaskResultNotify|PublishTaskResult|NotifiesReplyInstance|PreservesReplyInstance' ./internal/kafka/ ./internal/metrics/`,然后两个包整包测试,再 `go build ./...`。
8. **L1 稳定性**:`1..3 | % { go test -count=1 ./internal/metrics/... }`,三次都 ok。第 2 批把这个包加进了 L1。
9. **stress_summarize**:`pwsh -NoProfile -File tools/scripts/test_stress_summarize.ps1`,末行 PASS,含三行 `ok: DB task:result notify / ...`。脚本会临时挪走 `run\logs\cpp_nodes\scene*.log`,所以跑之前要确认本机没有 scene 进程。
10. **L1**:`pwsh -File go/test.ps1`,末行 `L1_TESTS_OK`。db 套件先红时,后面的套件不会运行,这时本项以第 5~8 步为准。
11. **robot**:`cd robot; go build ./...`,退出 0。
12. **C++**:整解 Build(§5 第 2 步)覆盖 proto、scene 库、scene 节点,不另起 MSBuild。
13. **Java(只读)**:在 `D:\luyuan\xuanming-server-mmo` 下跑 `java tools/ContractSync.java --mmorpg D:\luyuan\xuanming-server-mmo-cpp --check`。
    - Java 副本从 cea4d88 起已含 `reply_instance = 10`,`SOURCE.properties` 的 `mmorpg.commit` 是 `c63b22dca`。
    - `--check` 退出 1 时,差异里没有 `proto/db/db_task.proto` 就不算本项红。
    - **不要跑不带 `--check` 的同步**:它会写 Java 仓,还会顺带同步本仓他人在途的 proto。

## 7. B0-1 player_locator 会话对账按 gate 建索引

本线文件:`go/player_locator/internal/logic/` 下的 `keys`、`session_cas`、`gate_session_index`、`session_reconciler`、`reconcile_view`、`reconcile_metrics`;`internal/node/node.go`;`internal/config/config.go`;`player_locator.go`;`deploy/k8s/player-locator-alerts.yaml`;`k8s_deploy.ps1` 的 player-locator Prometheus 段。

1. **快照**:记 `git status --short -- go/player_locator deploy/k8s/player-locator-alerts.yaml deploy/k8s/manifests/go-svc/player-locator.yaml`(应为空),以及这些文件和 `k8s_deploy.ps1`、契约测试的 SHA。第 8 步之后再算一次,核对首尾。
2. **红灯**(基线 `360d56ae5`):
   - 建 worktree,只拷入 `go/player_locator/internal/logic/gate_session_index_test.go`;
   - 跑 `go test ./internal/logic/ -count=1 -v -run '^TestGateIndex_'`,设 `GOFLAGS=-mod=readonly`。
   - 期望:恰好 7 个 `--- FAIL`,是 testify 断言失败(索引键不存在):FirstLoginIndexesOnlineSession、ReplaceLoginMovesMemberAcrossGates、ReplaceLoginOnSameGateKeepsMember、ReconnectIndexesNewGate、StaleVersionLeavesIndexUntouched、ReplayIsIdempotent、MarkOfflineRemovesMember。
   - 另 3 个 PASS:SetDisconnectingRemovesMember、EmptyGateInstanceNotIndexed、CleanupPendingDoesNotIndex。
   - 出现 `[build failed]` 或 `undefined:` 都不算有效的红。
3. **gofmt**(Git Bash,在 `go/` 下):`gofmt -l player_locator/internal player_locator/player_locator.go player_locator/player_locator_test.go guild/internal/node/node.go scene_manager/internal/noderegistry/registry.go match/internal/noderegistry/registry.go client_rpc_router/internal/noderegistry/registry.go data_service/internal/noderegistry/registry.go`,无输出。
4. **vet**:在 go/player_locator 下 `go vet ./...`,退出 0。
5. **定向**:
   - `go test ./internal/logic/ -count=1 -v -run '^(TestGateIndex_|TestIndexRound_|TestRepairPass_|TestAuditPass_|TestAudit_|TestReadyMarker_|TestRun_|TestCampaign_|TestNewSessionReconciler_|TestIsKeyTypeError|TestParseRedisRunID|TestClassifyIndexedMember|TestCollectNodeUUIDs|TestCapabilityUUIDs|TestLegacyWriters|TestIndexedGate|TestCollectLiveGateInstances_|TestAlertRulesReferenceRegisteredMetrics|TestReconcileMetricLabelsArePrimed)'`:期望 62 个顶层用例 PASS,其中必须有 `TestIndexRound_DoesNotScanKeyspace`。
   - 再跑 `go test ./internal/config/ ./internal/node/ . -count=1 -v`:7 个顶层用例 PASS。
6. **全量**:`go test -json -count=1 -timeout=15m ./...`,退出 0。带 Test 字段的 fail / skip 都是 0;6 个无测试文件的包出现包级 skip,属正常。
7. **护栏**:在 go/shared 下 `go test ./leader/... ./nodeinfo/... -count=1`,两个包都 ok。
8. **部署契约**:`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`。
   - 「player-locator ConfigMap 的 LeaseTTL」与「player-locator 指标端口 9190」两条必须 `[PASS]`。
   - 整套退出 1 时逐条归因:10-19 已知 D1 1b-3 的粘滞核对有 2 条失败。
9. **文档条目**:
   - 跑 `python -c "import sys; sys.path.insert(0,'tools/scripts'); import gen_docs_index as g; e=g.entry('design','player-locator-gate-session-index.md',{}); print(e in open('docs/README.md',encoding='utf-8').read(), e in g.build())"`,两个都是 True。
   - `gen_docs_index.py --check` 很可能因为无关漂移退出 1,只记录不归因;不要不带 `--check` 跑。
10. **(需授权)integration**:只能用为本次新起的一次性 etcd(127.0.0.1:2379),不借 codex-* 容器。跑 `go test -tags=integration ./internal/node/... -count=1 -v`,7 个 `TestAlloc*` 全部 PASS;出现 SKIP 不算通过。
11. **(需授权)真 Redis**:按设计 §12 末尾的核对,在 Redis 7.2 与 8.x 上各跑一轮,只记录结果,不下性能结论。

## 8. B0-2 删除存盘 Lua 的 `dirty_keys_set` 写入

本线文件:`redis_client.h` 的 `kSaveLuaScript` / `kSaveIfGuardLuaScript`,`currency_test.cpp` 的两个新用例。这两个文件都有他人大量未提交改动,失败先看是否落在这两个脚本常量和两个用例上。代码随 `e76f6f747` 入库。

1. **静态**:`git grep -n "dirty_keys_set" -- cpp` 只命中 `cpp/tests/currency_test/currency_test.cpp`;`git grep -n "kSaveAndMarkLuaScript" -- cpp docs/design` 为空。2026-10-20 的只读核对两条都成立。
2. **单测**:整解 Build 之后,跑 `run_cpp_tests.ps1 -Build -Filter currency`。`CurrencyTest.RedisSaveScriptsTouchOnlyDeclaredKeys` 必须 PASS,这个用例不连 Redis。
3. **回归**:`run_cpp_tests.ps1 -Build -Filter cross_zone` 与 `-Filter bag`,全过;失败按文件归因。
4. **(需授权)真 Redis**:
   - 新起一个专用的 `redis:7.2` 容器,先 `FLUSHALL`;
   - 只对这一次运行设 `MMORPG_TEST_REDIS_ADDR=<IP>:<port>`(host 必须是 IP;§1 的清理会删掉它,所以要在清理之后设);
   - 跑 `currency_test.exe --gtest_filter=CurrencyTest.RedisSaveLeavesNoGlobalDirtySetAgainstRealRedis`,必须是 PASS,不能是 SKIP;
   - 跑完看 `INFO commandstats`:有 `cmdstat_evalsha` 或 `cmdstat_eval`,没有 `cmdstat_sadd`。`EXISTS dirty_keys_set` 不作判据。
   - 8.x 再跑一轮。
5. **(可选)红灯**:C++ 基线 worktree 需要 submodule 与第三方依赖,代价高。替代证据是 `git show a105c3efd:cpp/libs/engine/infra/storage/redis_client/redis_client.h | Select-String "SADD"`,应命中两段脚本,输出到终端,不重定向成文件。真要取红,先问负责人。

## 9. S5a 投递账本(交接 §3 第 6 项「Kafka 存盘失败后 MySQL 永久落后」)

只读核对结论:**代码已落**,未编译、未测试。做法与任务卡片原文不同:不作废脏比较快照,改成按玩家记投递账本,投递失败后强制重存。依据是 `docs/handoff/godb-pipeline-impl-20261015/S5-dirty-snapshot-retention-spec.md` 的 S5a。

- 整解 Build 之后,跑 `build\cpp\tests\cross_zone_test.exe --gtest_filter=DbTaskDispatch*:ExitDbTaskGate*:ExitPersist*`(命令来源:`godb-pipeline-impl-handoff-20261015.md` 约第 264 行),全部 PASS。
- `db_task_dispatch_test.cpp` 有他人未提交改动,失败先归因。

## 10. 回报格式

每一步回报:命令原文、原生退出码、通过 / 失败 / 未运行(附原因)、首个失败的原文、归属判断。最后附一张汇总表:

| 项 | 红灯 | Go | C++ | PowerShell / 解析 | 需授权项 |
|---|---|---|---|---|---|

全部绿之前,任何文档都不得写「已验证」。

## 11. Java 版

- 本单不涉及 Java 代码。
- B0-3 第 1 批的 Java 契约同步已由 Codex 10-19 完成:Java 副本从 cea4d88 起含 `reply_instance`。Java `PARITY.md` 那一行等 Java 线确认 game-proto 构建通过后收口。
- B0-4 在 Java 仍是待做,见 PARITY。
- 其余各项不适用,不在 PARITY 登记。
