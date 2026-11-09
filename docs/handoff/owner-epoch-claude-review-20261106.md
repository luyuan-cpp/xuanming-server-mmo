# owner_epoch 铸造持久化：交 Claude Code 独立审核

最新状态（2026-11-08）：当前主仓 Debug/x64 串行增量整解通过，exit 0，日志汇总 0 warning / 0 error。随后用主仓产物重跑归属定向 10/10、真实 Redis 7.2.16 与 8.10.2 各 3/3，全部零失败、零跳过。本次沿用已验证的任务专属 TEMP，未新增生产代码修改；仍未压测，Claude 独立审核待进行。

## 用户要求与范围

负责人要求 Codex 修复验证阻塞，再交 Claude Code「按他的想法去审核」。请独立选择审核方法和重点，可以质疑已有设计、实现和 Codex 的判断；本文提供事实与证据入口，不预设审核结论。

本线负责 scene_manager 的 owner_epoch 铸造持久化：G3a 墙钟地板、G3b 同节点同连接 EVAL/WAITAOF，以及 C++ 对墙钟跳跃后回滚凭据的采纳与恢复。玩家存盘已迁到 TiDB，owner_epoch 铸造仍在玩家域 Redis。Java 的 owner_epoch 在 SQL 事务内递增，不属于本线。服务器缺口审计的其他条目仍由各自会话负责。

负责人明确「现在先别压测」：本轮不跑 R-b，不调整专用连接池，不给吞吐或延迟结论。审核遵守 AGENTS.md，Claude 不运行编译、测试或压测；若需要新运行证据，列出待 Codex 执行的命令和通过标准。不要提交、推送或修改独立客户端仓库。

## 源码与设计入口

- 设计：`docs/design/zone-redis-regression-guard-20261022.md` §4.3，及 `docs/design/redis-player-store-cluster.md`。
- 铸造目标、Lua 与调用方：`go/scene_manager/internal/logic/owner_epoch.go`、`changesceneutil.go`、`enterscenelogic.go`。
- 专用通道：`go/scene_manager/internal/svc/owner_epoch_mint.go`，及 `go/shared/playerredis/pinned.go`。玩家域配置的 `PlayerFixed=32` 是按节点池口径。
- Go 证据入口：`owner_epoch_mint_test.go`（logic 与 svc 各一份）、`owner_epoch_crosslang_test.go`，以及同目录 `owner_epoch_test.go`。
- C++：`cpp/libs/services/scene/player/system/player_lifecycle.h`、`.cpp`、`handoff_sql_barrier.h`；`cpp/tests/cross_zone_test/cross_zone_test.cpp` 的 `TravelOwnership.*`；`cpp/tests/currency_test/currency_test.cpp` 的三个 `*AgainstRealRedis` 用例。

审核开始时请重新检查工作区和源码；本仓有多个会话并行修改，不能将某个时点的验证泛化为任意后续 HEAD。2026-11-06 取样 HEAD 为 `bc24d18027d3478ebbc5c9381a0a5709ff7c58e7`，文件级 SHA-256 在 `build/owner-epoch-review-20261106/source-before.json`。11 月 8 日续验入口与结果另见下节。

## 上轮阻塞与本轮核对

上轮正式执行了生成器构建及 no-unity 协议生成，两者均退出 0。27 份自定义守护段逐字节核对无变化，未手改生成产物。证据在 `build/owner-epoch-verify-20261105/` 的 `proto-gen-build.result.json`、`proto-gen-run.result.json`、`guard-blocks-check.json`。

随后整解退出 1（`game-debug-after-generation.result.json`），不能写成通过。本轮开始时，以下阻塞已被其他会话修齐，Codex 没有重复覆盖：

- `scene_control_redis` 已进入 C++/Go 正式生成绑定，`mutable_scene_control_redis()` 已存在。
- 观战发布函数的声明/定义均按值接收可修改帧，单 gate 挂接处使用可变局部帧。
- cross_zone 旧 HOT 夹具已使用 CHARACTER；data_service 的业务代码已改用现存分行枚举。
- scene_manager 的未闭合字符串已修，旧全区查询调用已改为当前 zone 编排入口。

其他会话登记的 scene_manager 成员清理五项失败不等于 owner_epoch 失败；其中 `read_unknown_fail_closed_test.go` 在本轮开始时已有他方未提交修改，未覆盖。

本轮新增修复仅为 `go/data_service/internal/playerbundle/bundle_test.go`：`TestDecodeRowKeepsBundleUsableWhenPayloadIsBad` 仍按旧文本 `unknown kind` 匹配，当前分行契约的诊断已包含未知与退役两类。修正预期，并验证未知编号 99、退役编号 1/2/5 不按现有布局解码、留下明确诊断且原始 payload 不变；坏 payload 同样检查原字节不变。未修改生产逻辑。红例与修后绿例均保留。

## 运行证据

### 已核实的历史证据

- `build/player-store-verify-20261101-r2/commands.jsonl`：scene_manager build、vet、全量 test 均退出 0，发生于 2026-11-02 UTC。它覆盖当时的 G3a/G3b，不代表当前整个服务全绿。
- `build/player-store-verify-20261102-r3-prep/06-cpp-tests/cross_zone_test.stdout.log`：`TravelOwnership.*` 8/8 通过，包括 `WallClockFloorRollbackIsAdopted`。同次其他 suite 有失败，不能称 cross_zone 全量通过。
- 同目录 `currency_test.stdout.log`：当时全量 299/299 通过，含真 Redis 三例；`06-cpp-tests-redis8/currency_test.stdout.log` 及 `currency_test.result.json` 记录 Redis 8 三例通过。

### 2026-11-06 定向复验

本轮证据集中于 `build/owner-epoch-review-20261106/`。

| 检查 | 实际结果 | 原始证据 |
|---|---|---|
| 当前 scene_manager build / vet | 均 exit 0 | `go/01-scene-manager-build.result.json`、`go/02-scene-manager-vet.result.json` |
| 当前 owner_epoch 定向 | 40/40 PASS、0 FAIL、0 SKIP | `go/03-scene-manager-owner-epoch-tests.log`、`go/scene-manager-test-scope.json` |
| 当前 shared/playerredis 全包 | 20/20 PASS、0 FAIL、0 SKIP | `go/06-shared-playerredis-tests.log` |
| 当前 data_service store/playerbundle build | exit 0 | `go/04-data-service-packages-build.result.json` |
| data_service 定向红例 | 14 PASS、1 FAIL | `go/05-data-service-tests.failures.txt` |
| 修后原 15 项复验 | 15/15 PASS、0 FAIL、0 SKIP | `go/08-data-service-targeted-tests-after-fix.log` |
| C++ 定向 | 10/10 PASS、0 FAIL、0 SKIP | `cross-zone.xml`、`cross-zone.result.json` |
| Redis 7.2.16，currency 交接围栏 | 3/3 PASS、0 FAIL、0 SKIP | `currency-redis72.xml`、`currency-redis72.result.json` |
| Redis 8.10.2，currency 交接围栏 | 3/3 PASS、0 FAIL、0 SKIP | `currency-redis8.xml`、`currency-redis8.result.json` |

Go 40 项的范围为 logic mint 18/18、svc mint 16/16、crosslang 2/10（C++ Lua 副本逐字节与回滚地板）、原 owner_epoch 4/74；没有声称当前 scene_manager 全量测试通过。源码比较见 `go/source-comparison.json`：owner_epoch 六文件、store 生产代码、bundle.go、shared/playerredis 在记录范围内未变；修后复验期间所记录的输入保持一致。格式化和本次测试文件的 `git diff --check` 均 exit 0。

C++ 使用其他会话完成构建的 APS **隔离快照**，固定复制 EXE/DLL 后由本线重新运行定向测试；不冒充当前主仓整解产物。来源、二进制 SHA-256、17 份文件和 5 个目标片段的比较分别见 `cpp-binaries.json`、`cpp-snapshot-equivalence.json`。完整 `travel_outcome`、`ResolveTravelOutcome`、`ResumeTravelAfterRevocation`、恢复 Lua 与目标用例相同；player_lifecycle 全文件及 cross_zone 工程存在任务外差异，所以这份证据仅覆盖列出的目标行为。C++ 10 项由 `TravelOwnership.*` 8 项（含 `WallClockFloorRollbackIsAdopted`）、`TravelOwnershipEcs.*` 1 项和 `PlayerStoreDirectEcs.ClaimFillsTheMetadataBackIntoTheLoad` 1 项组成。

Redis 两轮均为本地现有镜像、唯一容器名、任务标签、随机 loopback 端口、无共享卷；设置 `MMORPG_TEST_REDIS_ADDR` 后运行 `CurrencyTest.*AgainstRealRedis`。两轮清理均 exit 0，任务标签下无残留容器。常驻数据栈未清空。本轮没有真实 Redis Cluster 切主/迁槽演练，也没有 R-b 性能证据。

当时另一会话的主仓串行整解仍在途，本线没有抢占共享输出。2026-11-08 已确认其最终结果为失败，具体见下一节，不能继续将它描述为仍在运行。旧 TEMP 日志和 APS 源码目录现已不存在；13 号构建日志已迁到 `build/verification/battle-handoff-20261106-r4/`，11-06 本线固定二进制、哈希、XML 和回执仍保留。

### 2026-11-08 主仓整解续验

负责人要求检查主仓整解、有错误就修复复验、更新本交接，仍不压测。开始时 HEAD 为 `c17837c9c`，没有在途 MSBuild；工作区已有他方 `docs/PROGRESS.md` 增量及四个脏子模块，全部保留。

旧构建的实际终态如下，原件均在 `build/verification/battle-handoff-20261106-r4/`：

| 运行 | 最终状态 | 原件 |
|---|---|---|
| 13 号主仓整解 | exit 1，5 项进程终止 / DLL 初始化错误，与宿主重启记录一致 | `13-cpp-msbuild.log`、`13-cpp-msbuild.exit.txt` |
| 18 号主仓串行整解 | exit 1，121 warning、1 error | `18-cpp-build-start.json`、`18-cpp-msbuild.log`、`18-cpp-msbuild.exit.txt` |
| 19 号 configuration_table_test 补编 | exit 0，1 warning、0 error | `19-cpp-configuration-table-msbuild.log`、`19-cpp-configuration-table.exit.txt` |

18 号在 configuration_table_test 的 AST 门禁打印 PASSED 后，系统 TEMP 内的 MSBuild `*.exec.cmd` 消失，产生 MSB5018 / MSB3073。19 号用任务专属 TEMP/TMP 补编通过。这是临时文件执行失败，不能归因为裸指针违规，也不能把单目标补编写成整解成功。

本次沿用这个已验证的环境处置，仅在构建进程内设置 TEMP/TMP 到 `build/owner-epoch-review-20261108/temp/`；执行当前主仓 `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /p:CL_MPCount=1`，未修改系统环境、未绕过门禁。09:36:43Z—09:56:49Z（纽约时间 04:36—04:56），本次完整 Build 命令 exit 0，日志汇总 0 warning / 0 error；configuration_table_test 的原失败点已通过。此处为增量整解 Build，未执行 `/t:Rebuild`。本次没有复现新的源码错误，也没有新增生产代码或测试修改。

本轮实际结果如下，所有路径以 `build/owner-epoch-review-20261108/` 为根：

| 检查 | 实际结果 | 原件 |
|---|---|---|
| 主仓串行 `game.sln` Debug/x64 Build | exit 0，0 warning、0 error | `01-game-start.json`、`01-game-result.json`、`01-game-msbuild.log` |
| 主仓 `cross_zone_test` 定向 | 10/10 PASS，0 FAIL / SKIP；精确测试名与预期名单一致 | `cross-zone.result.json`、`cross-zone.xml`、`cross-zone.log` |
| 主仓 `currency_test`，Redis 7.2.16 | 3/3 PASS，0 FAIL / SKIP，exit 0 | `currency-redis72.result.json`、`currency-redis72.xml`、`currency-redis72.log` |
| 主仓 `currency_test`，Redis 8.10.2 | 3/3 PASS，0 FAIL / SKIP，exit 0 | `currency-redis8.result.json`、`currency-redis8.xml`、`currency-redis8.log` |
| 构建输入比对 | 2151/2151 份记录文件 SHA-256 未变，HEAD 未变 | `inputs-before.json`、`inputs-after.json`、`source-comparison.json` |

独立只读复核见 `final-audit.json`：重算记录输入与产物哈希、逐名解析三份 XML、核对最终回执与日志，结论一致；`cleanup-final.json` 记录本轮任务标签下无残留容器。2151 份是本轮记录的源码输入，不代表所有外部依赖均被封存。

本次使用的是主仓产物，整解确认它们为最新；因增量检查无需重编，EXE 的修改时间仍可早于本次命令。`cross_zone_test.exe` 的 SHA-256 为 `E4D49E80141AF2C2CC10FD58CA7558272FA4FE1A78A5B57A1ACA1439E63AAE97`，`currency_test.exe` 为 `B8F0D50EACFF680C3185D05FF6BD9B96A521490BE6298414EF935973E288EAE0`，各次测试运行前后均一致。本次证据不再依赖 APS TEMP 快照。

可复验命令（工作目录为仓库根）：

```powershell
build/cpp/tests/cross_zone_test.exe --gtest_filter=TravelOwnership.*:TravelOwnershipEcs.*:PlayerStoreDirectEcs.ClaimFillsTheMetadataBackIntoTheLoad
# 每轮指向专用的 Redis 7.2 / 8.x 测试实例，不能省略该环境变量，否则用例会跳过。
$env:MMORPG_TEST_REDIS_ADDR='127.0.0.1:<本轮专用端口>'
build/cpp/tests/currency_test.exe --gtest_filter=CurrencyTest.*AgainstRealRedis
```

本轮 10 项含 `WallClockFloorRollbackIsAdopted`，与上轮相同的 8 项归属判定、1 项缺组件保持冻结、1 项载入元数据组成；两轮 currency 各为同一份三个真实围栏用例。Redis 使用本地已有镜像、唯一任务标签、随机 loopback 端口、128 MiB 容器上限、开启 AOF 且无共享卷；每轮结束均核标签后按完整 ID 删除，清理 exit 0。常驻数据栈未清空。这些围栏用例不证明 G3b 在真实 Cluster 切主或迁槽时的刷盘保证。

11-06 的 11 个核心文件和 C++ 快照比对中的 17 份主仓输入，在本次开始时哈希均未变；原 10/3/3 份 C++ 与 Redis XML 重新解析均无失败或跳过，详见 `prior-evidence-audit.json`。Go 已通过范围继续按原 75 项记录，本次没有重复运行或扩大为全服务验证。未运行全部 C++ 测试、真实 Cluster 故障演练、R-b 或连接池定容；Java 仍因采用 SQL owner_epoch 而不适用本线。本次只更新本交接及 PROGRESS，无提交、推送或客户端操作。

## 审核交付

请按你自己的推理审核源码、契约和证据适用范围。报告实质问题、理由和可定位的源码行；若未发现问题，也请明确实际审阅范围与未覆盖项。需要修复或补测的内容交回 Codex，不要仅依据本文已有结论判定通过。

2026-11-06 的 Claude Code 桌面观察显示周额度 100%，恢复时间为 11 月 12 日 15:00；当时输入框有其他任务草稿，Codex 未覆盖，也未成功发送本线审核。11 月 8 日仅更新本地交接，未重新查看桌面或发送消息；上述额度信息不是实时状态，仍不能称 Claude 已接手或审核完成。
