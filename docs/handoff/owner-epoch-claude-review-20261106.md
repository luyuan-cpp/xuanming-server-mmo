# owner_epoch 铸造持久化：交 Claude Code 独立审核

最新状态（2026-11-10）：Claude Code 已完成静态独立审核（未编译、未运行），结论见文末「Claude Code 审核结论（2026-11-10）」。没有发现会造成覆盖或同值重铸的缺陷。第 1 条（not_durable 回滚值被源端采纳）负责人选了方案 A（两段回执），同日又定推路由失败的回滚同样两段；第 2–4 条（真实 Redis 用例、探针、过时引用）也已补齐。服务端改动全部落码，未编译、未测试，验证步骤见「方案 A 落码」「第二轮独立审核结论」与文末「Claude Code 收尾」三节。第 2 条（Go 铸造通道缺真实 Redis 证据）：同日第二轮独立审核已写好真 Redis 用例，待 Codex 运行；第二轮还修了铸造通道把「pipeline 没发出去」当成「EVAL 有回复」的缺陷，并审过合并后的两段回执与确认预算口径（不需要调），见文末「第二轮独立审核结论」。最近一次运行证据仍是 11-08 的结果，对应 HEAD `c17837c9c`：主仓整解 exit 0（0 warning / 0 error），归属定向 10/10，真实 Redis 7.2.16 与 8.10.2 各 3/3。仍未压测。

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

2026-11-10 Claude Code 已接手并完成静态审核，结论见下节。11-06、11-08 记的「额度用尽、审核未发送」已经过时。

同日另一个 Claude Code 会话做了第二轮独立审核（不沿用第一轮结论，连同方案 A 的最终实现一起审），结论、已改的代码和交 Codex 的命令见文末「第二轮独立审核结论」。

## Claude Code 审核结论（2026-11-10）

**方式与范围**：只做静态审阅，没有编译或运行任何东西（AGENTS §10.1）。审阅时 HEAD 为 `e0790ad40`，工作区里有他方未提交的改动（见第 6 条）。

- 逐段读过的 Go 代码：
  - `logic/owner_epoch.go` 全文；
  - `svc/owner_epoch_mint.go` 全文；
  - `shared/playerredis/{pinned,playerredis,noreplay}.go`；
  - `changesceneutil.go` 的落点函数；
  - `location_revision.go` 的修订检查；
  - `enterscenelogic.go` 的两处铸造调用点和 not_durable 分支。
- 逐段读过的 C++ 代码：`travel_outcome::ClassifyOwnership`、B5 采纳分支、`ResumeTravelAfterRevocation`、`kResumeFenceLua` 及其十进制辅助函数、`kPublishMarkDirectLua`。
- WAITAOF 与重试语义按源码核对：go-redis v9.17.3（模块缓存），以及 `third_party/redis`（8.10.2）。

**总体结论**：G3a 与 G3b 按设计落码，没有发现会造成旧盖新或同值重铸的缺陷。下面第 1、2 条要处理，第 3、4 条是低级问题，第 5、6 条只是提示。

1. **【中，设计缺口】回滚值没有确认落盘，就被源端采纳了。所以「副本落后之后主节点宕机」这种情况下 G3b 不起作用。**
   - 现状：
     - 铸造没确认落盘时（`enterscenelogic.go:881-889`、`:1352-1358`），回滚经共享连接池执行，不等 WAITAOF（`owner_epoch.go:726-731`）；
     - C++ 源端取证读到回执，走 B5 采纳回滚之后的 epoch（`player_lifecycle.cpp:6710-6746`）；
     - 采纳之后立即强制存盘（`:7395`），把 TiDB 写权行抬到这个值。
   - 后果：铸造确认不足，多半正是因为刷盘或副本落后。随后如果主节点宕机、落后的副本被提升，铸造与回滚会一起丢。此时源端拿着一个 Redis 已经不认识的代次继续持有玩家。这正是 G3b 要防的状态，只是受害者从目标节点换成了源端。
   - 静态推演出的表现：
     - 同节点换图照常；
     - 跨节点与跨 zone 交接全部被拒：`kPublishMarkDirectLua` 要求 owner_epoch 等于缓存值，见 `handoff_sql_barrier.h:326`，这里回 -1；
     - 另一端登录时，如果旧标记还在 TTL 内，会凭它过门并铸出更大的代次。新持有者认领之后，源端下一次存盘被写权行拒为 STALE_OWNER，丢掉上次存盘以来的进度。
   - 与设计不符的两处：
     - 设计 §3 表格「持有者不会拿到会丢的 epoch」、§4.3.3 拓扑表「不可用，但不回退」，在这个分支上不成立；
     - §4.3.3 把它列为「确认不足与切主叠加」的偶发残余，但这两件事正相关，不是独立事件。
   - 可选方案，请负责人定：
     - A：两段回执。not_durable 分支的回滚先写「待确认」回执；SM 在铸造通道上对回滚补一次 WAITAOF，确认之后再按 CAS 改成正式回执。C++ B5 只认正式回执；见到待确认回执，按 B6 收口。源端冻结时的状态已经落盘，不丢数据；代价是确认不了的时候多踢一次线。要同时改 Go、C++、跨语言金样和设计。
     - B：接受这条残余，但按现在的真实后果改写 §3、§4.3.3（含第 4 条），并在 runbook 写清处置办法。
2. **【中，证据缺口】Go 铸造通道从没在真实 Redis 上跑过。**
   - svc 层测试全部连 miniredis，而 miniredis 不认 WAITAOF（`svc/owner_epoch_mint_test.go:23`、`:108-110`）。所以只走得到「WAITAOF 被拒」这一支，「确认落盘」从来没出现过。
   - logic 层用的是替身 `fakeOwnerEpochMinter`（`logic/owner_epoch_mint_test.go:31-37`）。
   - 真实 Redis 的 3 个 C++ 用例（`currency_test.cpp:745`、`:1083`、`:1140`）测的是交接资格与标记的 Lua，不经过铸造通道。
   - 所以下面这些结论目前只有替身证据，单实例也没有验证过：
     - 同一条 pipeline 里的 WAITAOF 能覆盖铸造；
     - 重放分支「写回原值」能让新连接的 WAITAOF 覆盖首发那次写；
     - 预算与读超时能配合服务端阻塞的 WAITAOF；
     - 启动探针在正确配置下能通过。
   - 补测要求见下面的「交 Codex」。
3. **【低】启动探针查不出「副本没开 AOF」。**
   - 探针在新连接上发 WAITAOF，这时连接的写偏移是 0（`networking.c:222`）。副本没开 AOF 时不发 FACK（`replication.c:4616-4630`），主节点记的副本刷盘偏移停在初值 0（`networking.c:208`），而 0 ≥ 0 照样计数（`replication.c:4878-4891`、`:4951-4952`）。
   - 结果是 `numreplicas ≥ 1` 时探针照样通过，到运行期每一次铸造都判 not_durable，所有首登与换节点都回 7。`owner_epoch_mint.go:343-376` 的注释说探针就是要在启动时拦住这一类配置错误。
   - 现有清单所有 Pod 都写了 `appendonly yes`，所以这只是纵深防御上的缺口。建议探针先在同一条连接上写一个探针键（Cluster 下每个主节点各写一个落在本节点 slot 的键），再发 WAITAOF。
4. **【低，注释与文档过时】**
   - `owner_epoch.go:449-450`、`config/config.go:720-722` 写的后果是「持有者下一次存盘被等值 CAS 拒掉、按废黜销毁（丢 ≤300s）」。这条已经不成立：
     - Redis 存档路径和它的 owner CAS 已经删除（`cpp/libs/services/scene/core/system/redis.cpp:227`）；
     - TiDB 写权行对同一持有者 E ≥ Er 照常放行（`player-save-direct-commit.md` §4）；
     - 真实后果见第 1 条。
   - `owner_epoch_mint.go:57`、`:118`、`:383-384` 与 `config.go:724`、`:730-731`、`:738` 引用的东西都已不存在：go/db 的 `durable_enqueue.go`、data_service 的 `route_publish.go`、`Get-RouteStoreWaitAOFNumReplicas`。设计 §4.3.2、§4.3.3 也引用了它们。
   - 现状是：WAITAOF 只剩 scene_manager 在用；K8s 的副本数取 `Get-OwnerEpochMintWaitAOFNumReplicas`，建了玩家域集群时固定取 1（`k8s_deploy.ps1`，另有会话正在改这个文件，行号在变）。「几处要改一起改」指向的文件已经没了。
5. **【提示，R-b】`WaitAOFNumReplicas=1` 时的可用性。**
   - 铸造截止 = 请求截止 −（Kafka 5s + 300ms），所以按现值铸造必须在请求开始后约 2.7s 内完成（`owner_epoch.go:469-475`，yaml Timeout 8000）。
   - 进场前面的 RPC（角色目录强读、归属 zone 查询，后者上限 1500ms）如果用掉约 1.5s，WAITAOF 只剩约 1.1s。
   - 按 §4.3.3 自己的低负载模型（等待 = P，若 P < F 则为 P + 1s，P 与 F 独立且在 [0, 1s) 上均匀分布），超过 1.1s 的概率 = P(0.1s < P < F) = ∫_{0.1}^{1}(1−p)dp ≈ 0.405。
   - 结论：正确性不受影响（会回滚并让客户端重试），但可用性会掉。R-b 要加一个「前置 RPC 变慢」的场景。
6. **【提示，证据新鲜度】11-08 的运行证据不覆盖当前的工作树。**
   - 11-08 的证据对应 `c17837c9c`。之后 `74b097888` 在 `player_lifecycle.{h,cpp}`、`cross_zone_test.cpp`、`currency_test.cpp`、`enterscenelogic.go` 做了改名（如 `IsCrossZoneFrozen` → `IsPlayerFrozen`）。
   - 工作区里还有他方未提交的 `player_lifecycle.{h,cpp}` 改动（`RunIdentityAtRiskSaves` 提成成员函数）和 `cross_zone_test.cpp` 改动。
   - 静态看都不碰本线逻辑，但宣称通过之前要在当前树上重跑。

**核对过、没有发现问题的部分**：

- G3a 的目标值：Go 一次算好；Lua 按十进制串逐字节比较，上限 2^53−2（`9007199254740990`）。重放识别比的是 ARGV[9]，回滚 INCR 之后的值仍在 double 的精确范围内。
- 修订检查对重发是兼容的：location 与修订标签都已经是本次的值时放行（`location_revision.go:157-165`），所以重放分支走得到。
- 节点客户端不会自动重发：Cluster 层传 −1，`ClusterOptions.init` 保留 −1；节点 `Options.init` 把 −1 换成 0（`osscluster.go:201-202`、`:376`，`options.go:354-358`）。所以 WAITAOF 读失败时不会把已生效的 EVAL 再发一遍。
- 错误分类正确：
  - go-redis 9.17 的类型化错误保留原文，`ClassifyNotExecuted` 能按首词判断；
  - `noReplayHook` 把网络错误包成非 `redis.Error`，`isRedisErrorReply` 不会把它误判成确定性错误。
- WAITAOF 的语义与设计一致：
  - `call()` 在传播之后才更新本连接的写偏移（`server.c:4257-4260`）；
  - 没有副本、没有 backlog 时，复制偏移照样递增（`replication.c:672-679`）；
  - 对副本发 WAITAOF 会被拒（`:4941-4944`）；
  - 副本只在每秒一次的 cron 或收到 GETACK 时上报刷盘偏移（`replication.c:193-197`、`:1576-1578`）。
- C++ 三处「≥ +2」口径一致，都有溢出防护：`ClassifyOwnership` 用 `UINT64_MAX − 2`；`ResumeTravelAfterRevocation` 在 `:7366`；`kResumeFenceLua` 用 `plus2` / `cmp`，进位与 20 位上限都对。
- 毫秒级 epoch 没有被窄化：在 scene_manager、`shared/ownerepoch`、`shared/handofffence`、data_service、go/db 与 C++ 玩家路径里，grep 不到对 epoch 的 uint32 / int 转换。
- K8s 侧没问题：建了玩家域集群时副本数取 1；各清单都开着 AOF。

**未覆盖**：

- `enterscenelogic.go` 其余分支；
- LeaveScene、无资格撤销、改派路径（第 1 条后果的细节要按这些路径再推一遍）；
- 真实 Cluster 切主与迁槽；
- R-b 与连接池定容；
- Java 版（按本线范围不适用）。

**交 Codex**：第 1 条在负责人定方案之前不动代码。第 3、4 条可以与第 2 条同批改。第 2 条按下面补测（测试文件尚未写），7.2 与 8.x 各跑一轮：

- 环境：各起一个一次性的单实例 Redis，参数 `--appendonly yes --appendfsync everysec`，用唯一容器名、随机 loopback 端口、无共享卷，口径同 11-08。设置 `MMORPG_TEST_REDIS_ADDR=127.0.0.1:<端口>`。
- 用例：放在 `go/scene_manager/internal/svc` 新建的真实 Redis 测试里，没设环境变量时 Skip。测试入口要构造 `RedisOwnerEpochMinter`，经 `EvalAndConfirm` / `Probe` 执行，不能只直接发命令：
  - (a) `1 0` 时铸造回 Confirmed，回复等于目标值；
  - (b) 单实例配 `1 1` 时，在预算内回 NotDurable，且耗时不超过 WAITAOF 的等待上限加 200ms；
  - (c) 探针：`1 0` 通过，`1 1` 失败；
  - (d) 重放：首发生效之后，在新连接上原样重发，回复为 −目标值；前后读 `INFO replication` 的 `master_repl_offset`，必须增加（证明写回原值确实传播了）；
  - (e) 回 0 / −1 的分支前后，`master_repl_offset` 不变。
- 命令：在仓库根下运行 `cd go/scene_manager && go test ./internal/svc -run RealRedis -count=1 -v`。
- 通过标准：设了环境变量时 0 FAIL、0 SKIP；结束后容器按任务标签清理干净。
- 失败时保留：失败用例名、断言输出、`redis-server --version`、`go version`。
- 可选：在 `deploy/compose.redis-cluster.yml` 上补一例 MOVED 之后改投 slot 主节点的验证。

### 方案 A（两段回执）落码（2026-11-10，未编译、未测试，待 Codex 验证）

负责人对第 1 条选了方案 A。设计见 `docs/design/zone-redis-regression-guard-20261022.md` §4.3.4；新判定 B5′ 见 `cross-zone-scene-travel.md` §12.8 判定表。

- **比审核时的方案多了一道闸**：审核时只写了三点 —— not_durable 回滚先写待确认回执，确认落盘后改成正式回执，C++ 只认正式回执。落码时补了一条：location 带待确认回执时，凡是不铸造就下发 epoch 的落点（同落点重连、同物理节点换图、dev 旁路），先在铸造通道上确认落盘、把回执改成正式回执，再下发；确认不了回 7。
- **为什么要补这道闸**：源端按 B5′ 踢线之后，玩家重登多半落回原场景。不拦这一道，同落点重连会原样下发那个没确认的值，方案 A 就等于没做。
- **改动文件**：
  - Go 生产代码：`go/shared/ownerepoch/ownerepoch.go`、`go/scene_manager/internal/logic/owner_epoch.go`、`enterscenelogic.go`、`go/scene_manager/internal/metrics/metrics.go`、`go/scene_manager/internal/config/config.go`（只改注释）。
  - Go 测试：`go/shared/ownerepoch/ownerepoch_test.go`、`logic/owner_epoch_mint_test.go`、`owner_epoch_crosslang_test.go`。
  - C++ 生产代码：`cpp/libs/services/scene/player/comp/player_ownership_comp.h`、`player/system/player_lifecycle.h`、`player_lifecycle.cpp`、`travel_freeze_cap.h`。
  - C++ 测试：`cpp/tests/cross_zone_test/cross_zone_test.cpp`、`player_ownership_metrics_test.cpp`。
  - 告警：`deploy/k8s/owner-epoch-alerts.yaml` 新增 `SceneManagerOwnerEpochRollbackReceiptPending`。
  - 文档：上面两份设计、`docs/ops/cross-zone-failure-test-runbook.md`（新计数一行、新日志一行）、本文件、PROGRESS。
- **没做的**：
  - （已做）推路由失败的回滚：负责人同日定为同样两段，已合并成同一个回滚函数 `rollbackPlacementAfterPushFailure`，不再是残余；
  - 审核第 2–4 条只顺手改了与本条同一处的过时后果描述：`owner_epoch.go` 的 `errOwnerEpochMintNotDurable` 注释、`config.go` 的 `OwnerEpochMintConfig` 注释。其余（Go 真实 Redis 测试、探针、其余过时引用）没动；
  - Java：owner_epoch 在 SQL 事务内递增，不适用，不登记 PARITY。

**交 Codex 验证**（按顺序执行；C++ 必须串行 `/m:1`）：

1. **Go 构建与测试**：
   - 在 `go/scene_manager` 下运行：
     ```
     go build ./...
     go vet ./...
     go test ./internal/logic ./internal/svc ./internal/metrics -count=1
     ```
   - 在 `go/shared` 下运行：
     ```
     go test ./ownerepoch -count=1
     ```
   - 通过标准：下面这些新增或改动的用例全部 PASS。其余失败按文件归属区分（工作区里有他方在制改动），失败的用例名逐个列出。
     - `TestEnterScene_MintNotDurableRollsBackHandoffWithoutRouting`
     - `TestEnterScene_MintNotDurableRollbackConfirmedUpgradesReceipt`
     - `TestEnterScene_MintDurabilityErrorRollsBackCrossZoneAdmission`
     - `TestRollbackLuaReplayBranchWritesNothing`
     - `TestConfirmPendingRollbackLuaWritesBackOnlyWhenUnchanged`
     - `TestUpgradeRollbackReceiptRewritesRevisionTag`
     - `TestEnterScene_SamePlacementConfirmsPendingRollbackBeforeRouting`
     - `TestEnterScene_SamePlacementRefusesToRouteUnconfirmedPendingRollback`
     - `TestEnterScene_MintingPlacementSkipsPendingRollbackConfirmation`
     - `TestEnterScene_RouteFailureRollbackWithoutConfirmationLeavesPendingReceipt`
     - 推路由失败的既有用例改走确认路径，替身缺省确认，仍应得到正式回执：`TestEnterScene_RouteFailureAfterHandoffBumpsEpochAndLeavesReceipt`、`TestEnterScene_CrossZoneRedirectKafkaFailureBumpsEpochAndLeavesReceipt`、`TestEnterScene_WallClockFloorRouteFailureBumpsOneAndForwardsMarker`
     - `TestPendingRollbackReceiptPrefixMatchesCpp`
     - `TestPendingRollbackReceiptRoundTrip`
     - `owner_epoch_test.go` 里回滚子测试 `marker_forwarded_then_source_judged`
2. **C++ 整解**：
   - 改的是 scene 库，必须整解，`run_cpp_tests -Build` 不会重编库。
   - 在仓库根运行下面的命令。TEMP/TMP 只在构建进程内指向任务专属目录，口径同 11-08：
     ```
     msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /p:CL_MPCount=1
     ```
   - 通过标准：exit 0，0 error。
3. **C++ 定向测试**：
   - 先运行定向用例：
     ```
     build/cpp/tests/cross_zone_test.exe --gtest_filter=TravelOwnership.*:TravelOwnershipEcs.*:TravelFreezeCap*:PlayerOwnershipMetrics.*:PlayerStoreDirectEcs.ClaimFillsTheMetadataBackIntoTheLoad
     ```
   - 新增的两个用例：`TravelOwnership.PendingRollbackReceiptIsNeverAdopted`、`TravelOwnershipEcs.PendingRollbackReceiptConcludesWithoutAdopting`。
   - 再跑一次不带过滤的全量 `cross_zone_test`，看有没有回归；全量里以前有他方 suite 失败，按 suite 区分。
4. **真实 Redis 回归**：currency_test 的三个真实 Redis 用例，7.2 与 8.x 各跑一轮，口径同 11-08。本次没改它们用到的 Lua，只作回归。
5. **失败时保留**：失败用例名、断言输出、`go version`，以及 MSBuild 首个错误前后 30 行。

审核第 2 条要求的 Go 真实 Redis 测试已由第二轮审核写好（`go/scene_manager/internal/svc/owner_epoch_mint_realredis_test.go`，运行方法见文末「第二轮独立审核结论」）。「确认脚本写回原值之后 `master_repl_offset` 必须增加」由其中 (d) 覆盖。

**落码后的调整（同日，第二轮审核会话的建议）**：

- **回滚改走共享连接池**：回滚先经共享连接池执行，待确认回执立即生效；之后再在铸造通道上用确认脚本写回原值、等 WAITAOF，确认了才改成正式回执。最初的写法是回滚本身经铸造通道发：副本落后时，那条通道的连接都被等满的 WAITAOF 占着，回滚最容易拿不到连接，而 not_durable 恰好在这时成批出现。
- **回滚脚本恢复原样**：重放分支照旧一个字节都不写，`owner_epoch_test.go` 也回到原样。
- **铸造通道缺陷由那边修**：对方发现，取不到连接时铸造通道会回「无错误、无回复」。已改 `svc/owner_epoch_mint.go`（未编译）：这种情形改为返回错误，并且不在通道内重试；此前没有结果未知的发送时包上 `ErrOwnerEpochMintNotSent`，否则仍按结果未知返回。本线的确认路径已按 not_sent 处理，回执留在待确认形态。
- **`sameNodeZeroMint` 不改**：它没确认之后的 keep 回滚记为低级残余，见 §4.3.4。
- **推路由失败的回滚也改成两段**（负责人选「最标准」的做法）：铸造确认过，回滚写下的却是另一个值，同样要确认落盘才写正式回执。两个入口合并成同一个函数 `rollbackPlacementAfterPushFailure`，四个调用点都走它，`mintUnconfirmed` 分支删掉。代价：Kafka 写满超时、前置 RPC 与铸造又都慢时，留给确认的预算可能不够（`2.7s −（前置 RPC + 铸造等待）`），回执留在待确认形态、源端踢一次线，指标 `site="rollback",result="not_sent"` 看得到。

## 第二轮独立审核结论（2026-11-10，Claude Code）

**方式与范围**：

- 只做静态审阅，没有编译或运行（AGENTS §10.1）。HEAD 为 `e0790ad40`，工作区有多个会话的未提交改动。方案 A 由「主仓串行增量整解验证」会话同日落码；本轮审的是它落完、并按本轮建议调整回滚顺序之后的最终形态。
- 方法：不沿用第一轮结论，按两条不变量逐个核对写者：
  - 交给持有者的 owner_epoch（路由、重定向、源端凭回执采纳）必须已确认落盘；
  - epoch 只升不降，不重铸同值。
- owner_epoch 键的写者全集：
  - Go 只有五处：铸造 Lua、回滚 INCR、确认脚本写回原值、同节点补种 SETNX、无资格撤销 INCR；
  - C++ 只读不写；player_inbox 只读，用于诊断。
- 语义按源码核对：go-redis v9.17.3（模块缓存）、`third_party/redis`（8.10.2）。

**结论**：

- G3a、G3b 与方案 A 的设计成立。没有发现旧盖新、同值重铸，也没有新的「采纳或下发没确认落盘的值」的路径。推路由失败的回滚负责人同日已定为同样两段、已落码，合并后的实现与预算口径见第 3 条。
- 发现 1 处实现缺陷（第 1 条），已在 main 上改并补测试。
- 第一轮第 2 条的证据缺口，本轮已把真 Redis 用例写好（第 2 条），等 Codex 运行。

**发现**：

1. **【低，已修】铸造通道把「pipeline 根本没发出去」当成「EVAL 有回复」。**
   - 原因：
     - 拿不到连接（池等待 `PoolTimeout`、拨号失败、新连接握手失败）时，go-redis 9.17.3 的 `generalProcessPipeline` 因错误「可重试」而退出循环，不调 `setCmdsErr`（模块缓存 `redis.go:866-898`）。这里 `MaxRetries` 配的是 -1，被换成 0。
     - 错误只从 `Pipelined` 返回（`pipeline.go:104-113`），旧的 `sendPipeline` 丢掉了这个返回值。
     - 结果 `EvalAndConfirm` 回 `err=nil、Reply=nil`，违反它自己的契约「error 为 nil 时 Reply 是脚本回复」。
   - 后果：
     - 铸造这一侧的结局是对的：回 3，一个字节都没写。
     - 但日志打的是 `owner_epoch CAS 返回值非法: <nil>`（`changesceneutil.go:260-262`），看上去像 Lua 与 Go 的约定漂移了。池耗尽恰好发生在登录高峰、副本落后（WAITAOF 占满连接）的时候，会把排障引错方向。
     - 方案 A 初稿让回滚也走这条通道，池满时回滚会被判成 redis_error，既不执行也不兜底。按本轮建议改成「回滚先走共享池」之后，剩下的影响只是确认与 3d 路径把它记成 error 而不是 not_sent，两者都是 fail-closed。
   - 改法（`go/scene_manager/internal/svc/owner_epoch_mint.go`，`sendPipeline` 与 `EvalAndConfirm`）：
     - EVAL 既没有错误也没有值时，返回包装了 `errMintPipelineNotSent` 的错误；找不到持有 slot 的主节点同样算没发出去。
     - 没发出去的那次不在通道里重试：池满时再排一次队，只会把调用方的预算耗在等连接上，最后被 ctx 截止打成「结果未知」。
     - 此前没有过结果未知的发送时，回包装了 `ErrOwnerEpochMintNotSent` 的错误，原因留在包装链里；否则仍按结果未知返回。
     - ctx 在等连接时到期，go-redis 设的是 ctx 错误，与发出去之后的读写失败分不开，所以仍按结果未知处理（保守的一侧）。
     - `ErrOwnerEpochMintNotSent` 的含义扩成「整条 pipeline 一次都没到达 Redis」。调用方（3d 回 3、确认路径记 not_sent、方案 A 的共享池兜底）都用 `errors.Is` 判，不用改。
     - 顺手改掉同一文件里四处已不存在的引用：go/db 的 `durable_enqueue.go`、data_service 的 `route_publish.go`，以及与它们「几处要改一起改」的约定。这是第一轮第 4 条里属于本文件的部分。
   - 新增测试（未运行），都在 `svc/owner_epoch_mint_test.go`：
     - `TestEvalAndConfirmUnsentPipelineIsNotSentAndNotResent`：钩子按 go-redis 的真实行为只返回 `ErrPoolTimeout`，不碰命令；
     - `TestEvalAndConfirmDialFailureIsNotSent`：真实的拨号失败；
     - `TestEvalAndConfirmUnsentResendAfterUnknownStaysUnknown`：先结果未知、再没发出去，仍报结果未知。
   - 真 Redis 用例 (f) 用真实的池耗尽再核一次（见第 2 条）。
2. **【中，证据缺口；用例已写，待运行】Go 铸造通道的真 Redis 用例。**
   - 对应第一轮第 2 条。方案 A 又多了一个依赖点：确认脚本在铸造通道上写回原值之后，WAITAOF 要能覆盖另一条连接上更早执行的回滚。
   - 新文件 `go/scene_manager/internal/svc/owner_epoch_mint_realredis_test.go`，设了 `MMORPG_TEST_REDIS_ADDR` 才运行，共 6 条，全部经 `EvalAndConfirm` / `Probe` 执行：
     - (a) `1 0` 确认落盘，回复等于脚本回复，复制偏移前进；
     - (b) 单实例配 `1 1` 回 NotDurable，耗时在 WAITAOF 超时附近、不会挂住，写已执行；
     - (c) 探针：`1 0` 通过，`1 1` 失败；
     - (d) 另一条连接先写，通道上写回原值：偏移前进、WAITAOF 确认；只读的识别分支不推进偏移（证明写回是必需的）；
     - (e) CAS 不过的分支一个字节都不写；新连接上的 WAITAOF 立即「满足」，这份确认什么都没证明；
     - (f) 池耗尽时回 `ErrOwnerEpochMintNotSent`，一个字节都没写。
   - 偏移断言只放在 WAITAOF 不阻塞的位置。原因：单实例上每次阻塞的 WAITAOF 都会「发」一次 GETACK，没有副本时 Redis 也给 `master_repl_offset` 加 1（`server.c:1923-1929` → `replication.c:672-679`）。
   - 不覆盖：Cluster 下 MOVED / ASK 改投、切主演练（设计 §8.2 D2）、副本上报刷盘偏移。
3. **【已由负责人定并落码，本轮审过】推路由失败的回滚也走两段回执。**
   - 我审的时候它还是 §4.3.4 的残余，我的意见也是改成两段。理由：§4.3.4 说「Kafka 失败」与「随后约 1s 内切主」互相独立，但同一台宿主机故障、同一次网络分区可以同时带走 Kafka broker 与 Redis 主节点，本仓也没有跨服务的反亲和约束。静态推演的后果：源端拿着 Redis 不认识的 N+2，跨节点换图被 `kPublishMarkDirectLua` 拒（`handoff_sql_barrier.h:326`）；另一端要等无资格撤销宽限（缺省 120s）才能铸新代次；撤销与新认领如果赶在源端还有未存盘进度时，源端之后的存盘被拒为 STALE_OWNER。
   - 负责人同日已定为同样两段，「主仓串行增量整解验证」会话已合并成一个 `rollbackPlacementAfterPushFailure(ctx, …)`（`owner_epoch.go:887-919`），四个调用点都走它（`enterscenelogic.go:932`、`:956`、`:1365`、`:1468`）。核过：
     - 不凭标记的 bump 与 keep 仍一段式，恢复出的 location 没有回执，不会被采纳；
     - 凭标记的 bump 先在共享池写待确认回执，回滚确实生效（restored 且没有 drift）才去确认；
     - 「Kafka 报错但其实已投递」时，目标节点的登记 / A2′ 与回滚照旧靠 owner_epoch 互斥，和回执是哪种形态无关。
   - 确认预算口径成立，不建议调：
     - 铸造截止 = 请求截止 D −（Kafka 5s + 300ms），确认截止 = D − 300ms。所以推路由用满 5s 时，确认还剩「铸造截止 − 铸造完成时刻」，也就是 2.7s −（前置 RPC + 铸造等待），WAITAOF 再扣 100ms 回包余量。
     - Kafka 快速失败（broker 不可用、连接被拒）时，确认差不多还有整段 Kafka 预算；只有写超时用满、前面又慢时才确认不了，这时源端按 B5′ 踢线，冻结时的状态已落盘，不丢数据。
     - 两个替代方案都更差：把确认放到请求截止之后，失败应答回不到源端，会退化成 30s 看门狗冻结；把 300ms 余量压小，只能多出约 0.2s。
     - 需要留意：`site="rollback"` 现在同时包括 not_durable 与推路由失败两种来源，回执 pending 告警在 Kafka 写超时时也会响。
4. **【低，文档与注释；已改正】方案 A 调整之后的遗留表述。** 「主仓串行增量整解验证」会话落完之后已空闲、收不到消息，本轮直接改了正文（下面行号是改之前的）；同时在设计 §4.3.3 补了「连连接都没拿到」的失败语义与新测试。原来的问题是：
   - `docs/design/zone-redis-regression-guard-20261022.md:285` 的表格还写着「回滚经铸造通道发并等 WAITAOF」，而且只写了 not_durable 一支。这与 §4.3.4 和代码不一致：现在回滚走共享池，确认才走铸造通道，推路由失败的回滚也一样。
   - 同文件 `:318` 仍写 `Get-RouteStoreWaitAOFNumReplicas`、「与 go/db、data_service 同源」。现在用的是 `Get-OwnerEpochMintWaitAOFNumReplicas`，建了玩家域集群时固定取 1。
   - 同文件 `:249`、`:251`、`:307` 引用的 `publish_redis.go`、`durable_enqueue.go` 已经不存在。
   - 同文件 `:371` 写「直到 LeaveScene 清掉 location」，不准确：取证脚本已删掉本族标记，LeaveScene 拿不到释放证据；实际是宽限之后由无资格撤销清掉（缺省 120s，可配 (85, 600]）。建议写明时长。
   - C++ `player_ownership_comp.h:23-25` 的键契约仍写「只由 scene_manager 用 INCR 推进……回滚也是再 INCR 一格(N+1 → N+2)」。G3a 之后铸造取的是 max(当前值 + 1, 墙钟)。
5. **【低，沿用第一轮】**
   - 启动探针查不出副本没开 AOF（第一轮第 3 条），结论不变。真 Redis 用例 (c) 只覆盖单实例。现有清单 `redis-ha.yaml`、`redis-player-cluster.yaml:49-50` 都开着 AOF。
     - 「主仓串行增量整解验证」会话已认领修复：`probeNode` 在同一条 pipeline 里先 SET 一个落在本节点 slot 上的探针键，再发 WAITAOF。本轮把 `svc/owner_epoch_mint.go` 交给它改，不再动这个文件。
     - 它另写 logic 层真 Redis 用例，测真实的铸造重放与两段回执全链；svc 层这 6 条已覆盖的不再重复。
   - R-b 可用性提示（第一轮第 5 条）仍成立。方案 A 在 not_durable 这一支再加一次 WAITAOF（最多约 2s），只拉长失败请求的耗时，成功路径不变。

**核对过、没有发现问题的部分**：

- 方案 A 的安全性：
  - 回滚（共享池）在先，确认脚本（铸造通道）写回原值在后，两者在 Redis 里按先后执行，写回的偏移晚于回滚。AOF 与复制流都是单一有序日志，WAITAOF 确认了写回，也就覆盖了回滚和更早的铸造。
  - 确认与改写回执都按 epoch、location、修订侧车原样 CAS；中间被并发改写就回 superseded，不改回执（`owner_epoch.go:860-887`、`:946-984`）。
- 待确认回执不会被采纳，也不会被原样下发：
  - 只有 C++ `ResolveTravelOutcome` 读回执。B5′ 排在 B5 之前，不看其余交叉校验（`player_lifecycle.h:1033-1055`、`player_lifecycle.cpp:6934`）。
  - Go 侧不铸造就下发观察值的落点都排在第 3d 步之后（`enterscenelogic.go:795-811`）。HostedPlacement 的同落点重发、`resumeSceneOf` 回原场景，也都落在 samePlacement 分支里。
  - 跨区第 2 步只写等待落点，不下发 epoch；第二条腿一定铸造。
- 竞态：
  - 源端的取证要早于 scene_manager 回包，只能靠 30s 看门狗；而 scene_manager 的处理受 zrpc 8s 截止约束。
  - 别的登录凭转写标记 "N+2:t" 过换手门时会铸造新值，新值确认落盘；源端随后按 B8 销毁。
  - 那次铸造如果也没确认、再被回滚，写的是 "pending:N+2:t"，与源端本次的标记对不上，落到 B7，不会被采纳。
- 写者全集：无资格撤销的 INCR、补种的 SETNX 都不把新值交给任何节点。
- go-redis 语义：
  - EVAL 已读到回复、WAITAOF 读失败时，网络错误都走「可重试」分支，循环因 `MaxRetries=0` 退出，EVAL 保留成功结果。第一轮这条结论成立。
  - 例外：ctx 取消与协议解析错误会被 `setCmdsErr` 盖到 EVAL 上，退化成结果未知，方向是安全的。
- Redis 语义：
  - 没有副本时，只要开着 AOF，写照样推进 `master_repl_offset`（`replication.c:672-679`）；
  - 开 AOF 时 `fsynced_reploff` 的初值是 0（`server.c:3015`），新连接 woff=0 时 WAITAOF 立即满足（`replication.c:4952`）。所以确认脚本必须写回原值，这条理由成立。
- 告警：`deploy/k8s/owner-epoch-alerts.yaml` 有 not_durable、clock_behind、回执 pending 三条。

**未覆盖**：

- Redis Cluster 的 MOVED / ASK 改投与切主演练；
- R-b 与连接池定容；
- C++ 疏散、排空改派对回执的处理；
- Java（本线不适用）。

**交 Codex**（都没运行过；Go 命令都在 `go/scene_manager` 下执行）：

1. 构建与单测：
   - 运行 `go build ./...`、`go vet ./...`、`go test ./internal/svc -count=1`。
   - 通过标准：没设 `MMORPG_TEST_REDIS_ADDR` 时，6 条 `TestRealRedis*` 显示 SKIP，其余全部 PASS，其中包括第 1 条的三条新用例。
   - 方案 A 的 logic 等用例按上一节「交 Codex 验证」第 1 步执行。
2. 真 Redis，7.2 与 8.x 各跑一轮：
   - 每轮起一个一次性单实例，例如：`docker run -d --name <唯一名> --label task=owner-epoch-realredis -p 127.0.0.1::6379 redis:<版本> redis-server --appendonly yes --appendfsync everysec`。不设口令、不挂共享卷，其余口径同 11-08。
   - 用 `docker port <唯一名> 6379` 查到映射端口，然后执行 `$env:MMORPG_TEST_REDIS_ADDR='127.0.0.1:<端口>'` 与 `go test ./internal/svc -run RealRedis -count=1 -v`。
   - 通过标准：6 条全部 PASS、0 SKIP；结束后按标签删掉容器。
   - 失败时保留：失败用例名、断言输出、`redis-server --version`、`go version`。
3. C++ 整解、cross_zone 定向与 currency 真 Redis 回归，按上一节第 2–4 步执行。

## Claude Code 收尾（2026-11-10，未编译、未测试）

两轮审核提出的服务端改动到这里全部落码。剩下的只有运行验证（交 Codex）和不在本线范围的项（见末尾）。

- **第一轮第 3 条（启动探针）已改**：`svc/owner_epoch_mint.go` 的 `Probe` / `probeNode`，由第二轮审核会话交给本会话改。
  - 每个主节点在同一条 pipeline 里，先写探针键 `scene_manager:owner_epoch_mint_probe:{n}`（TTL 60s），再发 WAITAOF。Cluster 下逐个试 tag，用 `playerredis.MasterForKey` 按地址挑出落在本节点 slot 上的那个。
  - 等待取 max(配置值, 5s)：先写后等，等的是真实刷盘与副本上报，铸造的预算对启动期太紧。
  - 副本没开 AOF 时，WAITAOF 等满超时回 [1,0]，服务拒启。
  - miniredis 单测 `TestProbe` 改为断言：一条 pipeline 依次是 set、waitaof，没有单发，探针键带 TTL。
- **第一轮第 2 条的 logic 层真实 Redis 用例已写**：`go/scene_manager/internal/logic/owner_epoch_realredis_test.go`，3 条，环境变量与 svc 那份相同。
  - `TestRealRedisLogicScriptsWriteBackPropagatesAndMissesDoNot`：铸造脚本的重放识别写回原值会推进复制偏移，CAS 落败不推进；确认脚本原样时推进，location 变了不推进。偏移断言都用不发 WAITAOF 的通道，原因是阻塞的 WAITAOF 会发 GETACK，也会推进偏移。
  - `TestRealRedisRollbackConfirmsDurabilityBeforeFinalReceipt`：用生产函数 `placePlayerLocationCtx` 在真实 Redis 上凭标记铸造，再调 `rollbackPlacementAfterPushFailure`。回滚走共享连接，然后经铸造通道确认、改成正式回执；改写后修订侧车校验通过，转写标记为 `3:t`。
  - `TestRealRedisRollbackWithoutDurabilityLeavesPendingReceipt`：同一条链配 `1 1`。铸造回 not_durable，回滚照样执行，回执停在待确认形态。
  - svc 层 6 条已覆盖的（确认落盘、份数不足、探针、池耗尽等），这里不重复。
- **第一轮第 4 条（过时引用）已清完**：`config.go` 的 `OwnerEpochMintConfig` 注释原来写着「口径与 go/db 一致」「与 go/db、data_service 同一个函数」，现在改成「全仓只剩这一处用 WAITAOF」，K8s 渲染按 `Get-OwnerEpochMintWaitAOFNumReplicas` 加玩家域集群固定取 1 来写。`owner_epoch_mint.go` 与设计里的同类引用已由第二轮审核会话改掉。
- **告警说明**：按第二轮审核意见补上「site=rollback 的 not_sent 与 pending 在 Kafka 写满超时时也会响」。

**交 Codex**：在上面两节的步骤之外，再加下面两项。都在 `go/scene_manager` 下执行。

1. **真实 Redis 一轮里同时跑 logic 层用例**：
   - 容器与环境变量沿用上节第 2 步：每轮一个一次性单实例，`--appendonly yes --appendfsync everysec`，没有副本，测试期间不做别的写入。
   - 运行：
     ```
     go test ./internal/svc ./internal/logic -run RealRedis -count=1 -v
     ```
   - 通过标准：7.2 与 8.x 两轮都是 svc 6 条加 logic 3 条全部 PASS、0 SKIP。
   - 失败时保留：失败用例名、断言输出、`redis-server --version`、`go version`。
2. **不设环境变量的常规单测**（`go test ./internal/svc ./internal/logic -count=1`）：
   - 这 9 条应显示 SKIP，`TestProbe` 按新断言应 PASS，其余失败按文件归属区分。

**不在本线范围、未做**：

- Redis Cluster 的 MOVED / ASK 改投与切主演练；
- R-b 压测与连接池定容（负责人说先不压测）；
- 同节点 epoch 0 铸造没确认之后的 keep 回滚：只有存量 epoch 0 玩家走得到，数据清空后走不到，记为低级残余；
- Java：owner_epoch 在 SQL 事务内递增，不适用，不登记 PARITY。

## 运行证据核对（2026-11-11，Claude Code，只读）

负责人问上面交 Codex 的几步做了没有。按 Codex 留下的原件逐项核对，结论如下。服务端代码这边没有要修的，剩下的都是运行验证。

| 步骤 | 状态 | 证据 |
|---|---|---|
| Go 单测（不设环境变量） | 本线用例都过了。Codex 11-11 00:45 在 scene_manager 跑 `go test ./... -count=1 -v`，本线 15 条新增或改写的用例和 `TestProbe` 全部 PASS，9 条真实 Redis 用例按门控 SKIP。整套退出 1，3 条失败都是 `TestDeadNodeReregister_*`，属于缺口第 32 条，与本线无关；那三条测试已由别的会话按 R15 新行为改写（PROGRESS 11-11「缺口第 32 条的 3 条回归用例」），还没运行 | `artifacts/validation/gate-stale-20261110-232423/finish-20261111-004436/scene-synced-full.log`；PROGRESS 11-11「跨 zone 停写标记验证结果」 |
| Go vet | 只跑了 `go vet ./internal/logic/`，`./internal/svc` 没跑 | 同上 |
| `go/shared/ownerepoch` 单测 | 没跑 | — |
| 真实 Redis 7.2 / 8.x，9 条 | 没跑 | — |
| C++ 串行整解 | 11-11 00:58 的 `game.sln` 整解退出 0，已经包含本线的 C++ 改动：输入审计显示本线改的三个文件在 scene 库和 cross_zone_test 开编之前就改完了，日志里两者都编过。之后别的会话在 01:44~01:50 又改了 `player_lifecycle.{h,cpp}` 和 `cross_zone_test.cpp`；01:32 那次增量构建失败在别线的 `mail_outbox_rules.h:19`（C1083，找不到生成头 `proto/common/component/player_mail_outbox_comp.pb.h`，要先跑 proto 生成）。所以当前工作区还没有一次通过的整解 | `build/verification/release-track-20261110/round2/cpp-solution.log`、`cpp-input-audit-summary.json`、`cpp-incremental-final.log` |
| cross_zone 定向、全量 | 没跑。最近一次是 11-08，在本线改动之前 | `build/verification/naming-audit-20261108/cpp-tests/cross_zone_test/` |
| currency 真实 Redis 回归 | 没跑 | — |

**交 Codex 的顺序**（命令口径与上面「2026-11-08 主仓整解续验」「方案 A 落码」「Claude Code 收尾」三节相同）：

1. 先按 AGENTS.md §4 跑 proto 生成，补出别线新增的生成头，再在仓库根目录执行 `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /p:CL_MPCount=1`，要求 0 error。还卡在别线文件上时，保留首个错误和文件归属，不在本线改。
2. cross_zone 先跑定向，再跑全量：
   - 定向：`build/cpp/tests/cross_zone_test.exe --gtest_filter=TravelOwnership.*:TravelOwnershipEcs.*:PlayerStoreDirectEcs.ClaimFillsTheMetadataBackIntoTheLoad`
   - 必须包含本线两条新用例 `TravelOwnership.PendingRollbackReceiptIsNeverAdopted` 和 `TravelOwnershipEcs.PendingRollbackReceiptConcludesWithoutAdopting`，以及 `player_ownership_metrics_test.cpp` 里多了 `rollback_unconfirmed` 的那条。
3. currency 真实 Redis：Redis 7.2 和 8.x 各跑一次 `build/cpp/tests/currency_test.exe --gtest_filter=CurrencyTest.*AgainstRealRedis`，要求 3/3。
4. Go 的格式、vet 和单测：
   - `go/scene_manager` 下执行 `gofmt -l ./internal`、`go vet ./...`、`go test ./... -count=1`；
   - `go/shared` 下执行 `go test ./ownerepoch/ -count=1`；
   - 失败的用例按文件归属分开记。
5. 真实 Redis 7.2 和 8.x 各起一个一次性单实例，在 `go/scene_manager` 下执行 `go test ./internal/svc ./internal/logic -run RealRedis -count=1 -v`，要求两轮都是 9 条 PASS、0 SKIP。容器参数见上面「Claude Code 收尾」第 1 项。

## 第二轮审核续修（2026-11-12，Claude Code，未编译、未测试）

负责人要求审出来的问题不留到以后、现在修，要等压测才定的事先按最标准的做法定。本节是第二轮审核会话这一轮的改动。同一条线的组提交、连接池定容、告警阈值、铸造时间预算（服务端超时不等式）由「主仓串行增量整解验证」会话负责，见它自己的小节。本节不改它的文件。

**改了什么**：

1. **铸造结果未知也回滚**（原来是已登记的残余）。
   - 原来的问题：首发其实生效、回复丢了之后，有三种情形原来都不回滚：
     - 不凭标记的重发回 0，原来回 19；
     - 凭标记时，源端在两次发送之间删了标记，重发回 0 / -1；
     - 重发耗尽，原来回 3。
     结果 Redis 里留下没确认落盘、指向从未载入玩家的目标的落点，之后的同落点重连会原样下发这个值。
   - `svc/owner_epoch_mint.go`：新增 `ErrOwnerEpochMintResultUnknown`。此前有过结果未知的发送时，之后的终止（重定向耗尽、预算不够、没发出去、被拒、重发耗尽）都包上它（`unknownIfEarlier`）。
   - `logic/changesceneutil.go`：`placePlayerLocationCtx` 遇到它，或重发之后回 0 / -1，交回计划写下的落点，并返回 `errOwnerEpochMintResultUnknown`（定义在 `owner_epoch.go`）。
   - `logic/enterscenelogic.go`：
     - 第 6 步新增 `rollbackUnknownMintResult`：不推路由，先按原字节回滚（凭标记的走两段回执），再按回滚之后的 location 退人数（`removeSceneMember` 按 location 保留重叠身份，所以顺序不能反），回 7；
     - 跨 zone 第一条腿同法。
2. **同节点 epoch 0 的 keep 残留不再下发**（原来记为低级残余、不改）。
   - 同节点换图的判据改成只看 location 记的 epoch：记 0 就再铸一次，不要求键也读到 0。
   - 同落点重连在 location 记 0 时，下发 location 记的 0，不下发键里没确认的铸出值。
3. **铸造通道读超时盖住探针等待**（11-11 02:52 已改，随 `4edb25640` 入库，在 Codex 11-11 00:45 那次运行之后）。
   - `ReadTimeout = max(WaitAOFTimeoutMs, 探针等待 5s) + 1s`。
   - 原因：go-redis 读 pipeline 回复时按客户端 ReadTimeout 设读截止。原来探针等 5s、读超时只有 3s，副本配错时探针会把「份数不足」报成连接问题。
4. **设计**：`zone-redis-regression-guard-20261022.md` 更新了：
   - §4.3.3：失败语义表「EVAL 结果未知」一行、「结果未知也回滚」一条、下发观察值一条、ReadTimeout 一条；
   - §4.3.4：同节点 epoch 0 一条改为已修，测试清单补上。

**新增 / 改动的测试**（都没运行）：

- `svc/owner_epoch_mint_test.go`：
  - 新增 `TestEvalAndConfirmRejectAfterUnknownStaysUnknown`；
  - `TestEvalAndConfirmGivesUpAfterBoundedAttempts`、`TestEvalAndConfirmUnsentResendAfterUnknownStaysUnknown` 加了 `ErrOwnerEpochMintResultUnknown` 断言；
  - `TestEvalAndConfirmScriptErrorIsNotResent` 断言首发就被拒不算结果未知；
  - `TestOwnerEpochMintClientConfig` 按新读超时断言。
- `logic/owner_epoch_mint_test.go`：
  - 替身新增两种模式：`setUnknownAfterEval`、`setResendAfterEval`；
  - 新增 6 条：
    - `TestEnterScene_FirstPlacementMintResultUnknownRollsBack`
    - `TestEnterScene_UnmarkedMintResendReturningZeroRollsBack`
    - `TestEnterScene_HandoffMintResultUnknownRollsBackWithReceipt`
    - `TestEnterScene_CrossZoneMintResultUnknownRollsBack`
    - `TestEnterScene_SameNodeLeftoverOfUnconfirmedZeroMintIsReminted`
    - `TestEnterScene_SamePlacementDoesNotRouteLeftoverOfZeroMint`
  - 其中最后两条在改动前会红：第二次请求原来下发的是没确认的 1。

**当时没覆盖、同日已修**（见文末「结果未知的作废与已确认位点」一节）：

- 首发卡在网络里、晚于这次回滚才执行时，回滚 CAS 不过，首发随后照样生效。
- dev 旁路（`AllowUnsafeCrossNodeHandoff`）的跨节点不铸造落点照样下发观察值。

C++ 本轮没改。Java 不适用，不登记 PARITY。

**交 Codex**（Go 命令都在 `go/scene_manager` 下执行；本节的改动在 Codex 11-11 00:45 那次运行之后）：

1. 运行 `gofmt -l ./internal`、`go vet ./internal/svc ./internal/logic`、`go test ./internal/svc ./internal/logic -count=1`。
   - 通过标准：上面列出的新增、改动用例全部 PASS，9 条 `RealRedis` 用例按门控 SKIP。
   - 其余失败按文件归属分开记：工作区里另有 Redis Cluster 第 2a 批等会话在改 `enterscenelogic.go` 的控制域键。
2. 真实 Redis 一轮的口径不变，见上一节第 5 步，两轮都要 9 条 PASS、0 SKIP。本节改了 `ownerEpochMintClientConfig` 的读超时，svc 的 (f) 池耗尽用例仍覆盖它。
3. 失败时保留：失败用例名、断言输出、`go version`；真实 Redis 那一轮还要 `redis-server --version`。

## 时间预算与铸造通道定容（2026-11-12，Claude Code「主仓串行增量整解验证」会话，未编译、未测试）

负责人 11-12 要求：以前留到以后的现在就修，等压测再定的先按标准做法定。和上一节的会话分了工：logic 层的两条残余（结果未知回滚、同节点 epoch 0）归那边；本节只做时间预算、铸造通道的池子与组提交，以及两处没覆盖路径的核对。enterscenelogic.go、changesceneutil.go、logic/owner_epoch.go 本节一个字没改。

### 1. 时间预算：zrpc Timeout 8000 → 11500

- **问题**（上一节的会话查到，交给本会话）：契约测试的不等式只算了「归属查询 + Kafka 写 + 1500」，漏了两段：
  - 铸造前的角色目录强读（`svc/role_directory.go` 的 Lookup，写死 1500ms）；
  - 铸造的 WAITAOF（`OwnerEpochMint.WaitAOFTimeoutMs` 2000）。
- **后果**：成功路径最坏要 1500 + 1500 + 2000 + 5000 + 1500 = 11500，Timeout 只有 8000。前置两个 RPC 都在各自上限之内、只是慢一点，铸造的 WAITAOF 就被截短；两者都用满时铸造预算 2700 − 3000 < 0，一条都不发、回 3。各段都没超时，整体却失败，这是可用性缺陷。
- **改法**：Timeout 取成功路径上逐段串行的预算之和 11500，上游同批放宽，守住「上游比下游宽 2000」：
  - `go/scene_manager/etc/scene_manager_service.yaml`：`Timeout: 11500`，文件头写明不等式与每一段的出处；OwnerEpochMint 段的「约 1100ms」注释改掉。
  - `go/scene_manager/internal/svc/role_directory.go`：1500ms 提成常量 `roleDirectoryLookupTimeout`，Lookup 与 ReadPlayerStoreHolder 共用，契约测试从这一行读数。
  - `tools/scripts/tests/k8s_deploy_contract.tests.ps1`：`Get-SceneManagerTimeoutBudgetViolations` 加 `-RoleDirectoryLookupMs` 参数，并多核 `OwnerEpochMint.WaitAOFTimeoutMs`；新函数 `Get-SceneManagerRoleDirectoryLookupMs` 读 Go 源码，读不到就报错，不会按 0 放行。自检夹具改成 11500 通过；8000、11499、缺 WAITAOF 键、WAITAOF 调到 2500 而 Timeout 没跟、角色目录 1501 或 0 都报违例。
  - `go/login/etc/login.yaml`：`SceneManagerRpc.Timeout` 10000 → 13500。这一跳在 EnterGame 的后台链里，后台链总预算 5 分钟，不受 login 自己的 zrpc Timeout 约束。
  - `bin/etc/base_deploy_config.yaml`：`SceneManagerNodeService` 10000 → 13500。由它派生的换图在途 TTL 变成 14.5s。
  - `cpp/libs/services/scene/player/system/relocate_confirm.h`：settle 下限 `kSmSettleWindow` 15s → 19s，即 13.5 + 5 = 18.5 取整。这个下限按 scene_manager 允许的最小 deadline 取，宿主没 Apply 配置、落到内置缺省 10000 时也不缩。
  - 新增 Go 跨语言测试 `TestRelocateSettleFloorCoversSceneManagerTimeout`（`logic/relocate_settle_crosslang_test.go`）：核对 `kSmSettleWindow` ≥ yaml 的 Timeout + 2000 + Kafka 写超时。
  - `cpp/tests/cross_zone_test/cross_zone_test.cpp`：
    - `RelocateConfirmBudgets` 两条改成 19s；
    - 表机制用例的 `RelocateBudgets()` 改成固定的 `{15s, 30s}`。表只按给定预算排时刻，那些用例验的是相对时刻，不随配置重算；
    - 接真实粘合层的用例（墙钟三段）把「16s 超窗」改成 20s，注释改成 19s。
  - 只改注释的：`grpc_client_call.h`（内置缺省 10000 不再承诺盖住 Go 服务的 Timeout，八个 Go 目标由部署门禁要求显式列出）、`travel_freeze_cap.h`。
  - 文档：
    - `zone-redis-regression-guard-20261022.md` §4.3.3「时间预算」与 §4.3.4 代价里的确认余量：推路由失败之后确认剩 6.2s 减前两段，最坏约 1.2s；
    - `cross-zone-scene-travel.md` §12.2 / §13.0 / §13.5 / §13.8；
    - `grpc-muduo-transport.md` §16.3 表；
    - `docs/ops/cross-zone-failure-test-runbook.md` 的 V0、RC-1、RC-5 与 settle 时长。
- **没改、核对过的**：
  - 源端 30s 应答看门狗、70s 冻结上限都远大于 13.5s；
  - 客户端换图请求先应答、结果另推，等入场 60s，跨区传送受理后 75s，不受影响；
  - robot 的等入场预算 20~25s 也盖得住；
  - 部署门禁 `Assert-GrpcClientDeadlineBudget` 与 login → scene_manager 的契约用例都按 yaml 动态读，不用改；
  - 冲突疏散串两道 15s 看门狗，第一轮改派的条目 19s 到点，仍在第二道之内，RC-5 的性质不变。

### 2. 铸造通道：池按 Little 定律定为 130，不上组提交

- **池**：`ownerEpochMintPoolSize` 32 → 130（每节点）= ⌈194/s × 2s ÷ 3⌉。
  - 194/s 取自容量模型 `capacity-model-hok-scale.md` 每个 SM 实例 EnterScene 的上限，全部按铸造算，偏保守；
  - 2s 是 `config.MaxOwnerEpochMintWaitAOFTimeoutMs`；
  - 3 是玩家域集群主节点数的下限。
  - 新单测 `TestOwnerEpochMintPoolSizeFollowsLittlesLaw` 钉住这个算式。
  - `k8s_deploy.ps1` 连接总账两份 scene-manager 行的 `PlayerFixed` 同步改为 130，两条契约测试从源码读字面量；`deploy/k8s/README.md` 总账说明里的池数同步改。
  - 按规划档估算，每节点多记约 1200 条，离 maxclients 40000 的 80% 水位很远。基线 DryRun 不应出现 OverWarnRatio，这一点交 Codex 核对。
- **不上组提交**（采纳上一节会话的分析）：everysec 下 WAITAOF 不触发 fsync，各连接的 WAITAOF 本来就在等同一次后台刷盘，GETACK 也共享，组提交省不下刷盘和副本上报，只省连接数。凑批的铸造却最坏多等一个刷盘周期，高可用下更容易超出 2000ms。容量靠 SM 副本数 × 主节点数水平扩展，入口还有排队与限流。
- 设计 §4.3.3「吞吐与连接」改写；§8.3 R-b 改成只核对推算，不再是定池子的前提；§14 决策行补记。
- **告警阈值**：本会话没改。11-12 另一个会话已把 `SceneManagerOwnerEpochMintNotDurable` 定为「10 分钟内出现即报」（健康时应为 0）。本会话只把设计 §7 表里那行「阈值待压测」同步成这个口径。

### 3. 没覆盖路径的核对（只读，没有要改的）

- **C++ 疏散、排空改派**：只有源端 `ResolveTravelOutcome` 读回执。改派确认的核实只看 zone / node / scene（`player_lifecycle.cpp` 的 `kLuaReadPlacement` 裁决处有注释），不采纳任何 epoch。
  - 回滚之后 location 指回源场景，核实读到「没变」，踢线让玩家重登。
  - 重登走 EnterScene：同落点由 3d 那道闸先确认待确认回执；换节点就铸新值。
  - 回执是哪种形态都不影响这条链。
  - 踢线前按读到的 owner_epoch 补写的凭证标记，只当过门凭证，下一次落点会铸新值，不会把待确认的值交出去。
- **LeaveScene 与无资格撤销**：源端按 B5′ 收口时，取证脚本已删掉本族标记，LeaveScene 拿不到释放证据，要等无资格撤销宽限（缺省 120s）。§4.3.4 代价已写明。
  - 撤销的 INCR 不把新值交给任何节点，不需要落盘确认。
  - 换节点重登在宽限内回 18，落回原节点不用等。
  - 这是设计内的活性代价，不是遗留缺陷。

### 4. 交 Codex（都没运行过）

前置：C++ 改了 `relocate_confirm.h`（scene 库）与 `grpc_client_call.h`（core 头，只改注释），必须整解，`run_cpp_tests -Build` 不重编库。

1. **Go**，在 `go/scene_manager` 下执行，口径同 11-11：`GOTOOLCHAIN=local`、`GOPROXY=off`。
   - `gofmt -l internal/svc internal/config internal/logic/relocate_settle_crosslang_test.go`，应无输出；
   - `go vet ./internal/svc/ ./internal/config/ ./internal/logic/`；
   - `go test ./internal/svc -run 'TestOwnerEpochMintPoolSizeFollowsLittlesLaw|TestOwnerEpochMintClientConfig|TestProbe' -count=1 -v`，3 条 PASS；
   - `go test ./internal/logic -run 'TestRelocateSettle' -count=1 -v`：`TestRelocateSettleWindowCoversKafkaWriteTimeout` 与新的 `TestRelocateSettleFloorCoversSceneManagerTimeout` 都 PASS，后者不得 SKIP，SKIP 说明没找到 C++ 源码；
   - `go test ./... -count=1`：失败按文件归属分开记。
2. **PowerShell 契约测试**，在仓库根执行，用 pwsh 7：
   - 命令：`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1`；
   - 要求：两个都退出 0、`fail=0`；
   - 重点用例：
     - 「scene-manager 的 zrpc Timeout 必须为正、盖住角色目录强读 + …」与它的自检；
     - login → scene-manager 超时；
     - C++ deadline 部署门禁；
     - 「G3b Redis 连接总账：scene-manager 行 Fixed == …ownerEpochMintPoolSize」；
     - 基线 DryRun 不出现 `[RedisConnectionLedger]OverWarnRatio` / `[RedisPlayerConnectionLedger]OverWarnRatio`。
3. **C++**：
   - 在仓库根执行 `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /p:CL_MPCount=1`，要求 0 error；
   - 若还卡在别线缺的生成头上（11-11 的 `player_mail_outbox_comp.pb.h`），先按 AGENTS.md §4 跑 proto 生成；
   - 再跑 `build/cpp/tests/cross_zone_test.exe --gtest_filter=RelocateConfirm*:TravelOwnership.*:TravelOwnershipEcs.*`，然后不带过滤跑全量 cross_zone_test，失败按 suite 归属分开记。
4. **失败时保留**：失败用例名、断言输出、`go version`、MSBuild 首个错误前后 30 行。

**Java**：本节全是服务端内部的超时与连接池，不涉及客户端契约。Java 版 owner_epoch 在 SQL 事务内递增，不适用，不登记 PARITY。

## 结果未知的作废与已确认位点（2026-11-12，Claude Code 第二轮审核会话，未编译、未测试）

负责人再次要求：查出来的问题现在修，不留到以后。上面「第二轮审核续修」一节末尾登记了两条没覆盖的；修的时候顺着同一类问题又查出两条没登记的。四条一并收口，设计见 `zone-redis-regression-guard-20261022.md` §4.3.5。本节不改「时间预算与铸造通道定容」一节的文件内容（`owner_epoch_mint.go`、契约测试、yaml 都没碰）。

### 1. 四条问题

G3b 要守的是「一个 owner_epoch 值确认落盘之后才许交给节点」。以前只在请求流程里守，请求没走完时守不住：

| 情形 | 以前的结果 |
|---|---|
| 铸造结果未知，首发晚于收尾的回滚才执行（已登记） | 回滚 CAS 不过，首发随后生效，留下指向从未载入玩家的目标、epoch 没确认的落点 |
| 铸造没确认落盘，回滚又没做成（新查出） | 同样的落点留在 Redis，同落点重连原样下发 |
| 铸造写进 Redis 之后 scene_manager 退出（新查出） | 同上 |
| dev 旁路下发键里没确认的铸出值（已登记） | 原样下发 |

第一条比登记时估计的更容易出现：让首发结果未知的原因多半是 Redis 停顿，停顿期间首发与收尾的回滚各自排在自己的连接上，恢复之后谁先执行不一定。

### 2. 改了什么

1. **作废**（`logic/owner_epoch.go` 的 `luaRollbackPlayerPlacement`、`rollbackOrVoidUnknownMint`）。铸造结果未知时，同一段回滚脚本原子地二选一：
   - 状态已是本次落点：照常回滚；
   - 状态还是本次铸造观察到的那一份：把迟到的首发作废；
   - 都不是：什么都不改。
   作废的写法：
   - 不凭标记（首次落点、第二条腿、死节点接管）：直接写成回滚之后的状态，epoch 越过本该铸出的值，脚本回 6；
   - 凭标记（跨节点、跨 zone 交接）：只删掉所凭的标记，epoch 与 location 不动，脚本回 7，源端收到 7 之后照常解冻；
   - 同节点 epoch 0 的铸造不作废。
   脚本多收 ARGV[11..14]，只有结果未知的回滚才传；推路由失败、铸造没确认落盘的回滚不传，行为不变。
2. **已确认位点**（`shared/ownerepoch` 的 `ConfirmedKey`，键 `player:{id}:owner_epoch_confirmed`，EX 7 天，只有 scene_manager 读写）：
   - 写：铸造确认之后（`placePlayerLocationCtx`）、回滚之后的落盘确认之后、下面这道闸自己确认之后；
   - 读：EnterScene 新增第 3e 步。不铸造就要下发观察到的 epoch 时（同落点重连、同物理节点换图、dev 旁路），位点恰好等于它才直接下发，否则先经铸造通道确认落盘（`ensureOwnerEpochConfirmed`），确认不了回 7 / 19 / 3 / ErrRedis；
   - 缺省是「没确认」：键缺失、过期、读失败、值对不上都走确认。
3. **顺带的整理**：
   - 落盘确认的前半段抽成 `confirmOwnerEpochDurable`，两段回执的第二段与 3e 共用；
   - `errPendingRollbackNotDurable` 改名 `errOwnerEpochNotConfirmed`，`rejectUnconfirmedPendingRollback` 改名 `rejectUnconfirmedOwnerEpoch`，3d 与 3e 共用；
   - `placedLocation` 多带这次写入所凭的观察值（`observedEpoch` / `observedRaw` / `observedKnown`）。
4. **指标**（`internal/metrics/metrics.go`）：
   - `scene_manager_enter_scene_rollback_total` 多两个 outcome：`voided`、`marker_voided`；
   - `scene_manager_owner_epoch_rollback_receipt_total` 多一个 site：`handout`。现有告警 `SceneManagerOwnerEpochRollbackReceiptPending` 按 `result!="confirmed"` 算，自动覆盖它；规则没改，只在 `deploy/k8s/owner-epoch-alerts.yaml` 的告警说明里补了 `handout` 的含义。
5. **文档**：设计 §4.3.3 失败语义表与两条说明、§4.3.4 末尾、新增 §4.3.5；`cross-zone-failure-test-runbook.md` 指标表的 outcome 个数。

### 3. 文件

- `go/shared/ownerepoch/ownerepoch{,_test}.go`、`go/shared/redisslot/redisslot{,_test}.go`（新键与保留后缀登记）；
- `go/scene_manager/internal/logic/{owner_epoch,changesceneutil,enterscenelogic}.go`；
- `go/scene_manager/internal/metrics/metrics.go`；
- `deploy/k8s/owner-epoch-alerts.yaml`（只改告警说明）；
- 测试：`logic/{owner_epoch_mint_test,owner_epoch_test,owner_epoch_crosslang_test,owner_epoch_realredis_test,enterscene_identity_test}.go`。

### 4. 新增 / 改动的测试（都没运行）

- `logic/owner_epoch_mint_test.go`，替身新增「首发被扣住、稍后才到」模式（`setWithholdEval` / `deliverWithheld`），新增 11 条：
  - 作废：
    - `TestEnterScene_FirstPlacementUnknownMintNotYetAppliedIsVoided`
    - `TestEnterScene_HandoffUnknownMintNotYetAppliedVoidsMarker`
    - `TestEnterScene_CrossZoneUnknownMintNotYetAppliedVoidsMarker`
    - `TestEnterScene_SameNodeZeroEpochUnknownMintIsNotVoided`
    - `TestRollbackLuaVoidsUnmarkedMintThatHasNotApplied`
    - `TestRollbackLuaVoidOfMarkedMintOnlyRemovesItsMarker`
  - 位点：
    - `TestPlacePlayerLocation_ConfirmedMintRecordsConfirmedMark`
    - `TestEnterScene_SamePlacementConfirmsEpochMissingConfirmedMark`
    - `TestEnterScene_SamePlacementRefusesEpochThatCannotBeConfirmed`
    - `TestEnterScene_SameNodeSceneChangeConfirmsEpochMissingConfirmedMark`
    - `TestEnterScene_LeftoverOfLateMintIsConfirmedBeforeRouting`
- `logic/owner_epoch_test.go`：`TestClassifyRollbackReply` 补 5 行（返回值 6 / 7）。
- `logic/owner_epoch_crosslang_test.go`：`TestSourceJudgeScriptDeletesOnlyItsOwnLineageAndSeesReceipt` 加子用例 `marker_voided_by_scene_manager_reads_as_unchanged`，用 C++ 的取证脚本核对：标记被作废删掉之后，源端读到 epoch 没变、location 原样。
- `logic/owner_epoch_realredis_test.go`：新增 `TestRealRedisRollbackVoidsMintThatHasNotApplied`（两个子用例）；`realRedisPlayerID` 的清理名单加上位点键。
- `shared/ownerepoch`、`shared/redisslot`：新键的形状、同槽、保留后缀表。
- `logic/enterscene_identity_test.go` 的助手 `identSeedLocation`：epoch 非 0 时同时摆位点，代表一条正常铸造并确认过的落点。

### 5. 对现有用例可能的影响（Codex 跑全量时留意）

本次给 EnterScene 加了两类 Redis 命令，现有 868 条用例我没法运行，只能按读代码判断：

- 每次确认过的铸造之后多一条 `SET … EX`（位点）。经 `UpdatePlayerLocation` 或 EnterScene 摆初始状态的用例，库里会多一把 `player:{id}:owner_epoch_confirmed`。
- 带 GateId 的不铸造落点多一次 `GET`（位点）。位点对不上时还会经铸造通道多发一条确认脚本。用例若绕过铸造直接摆 location 与 epoch 键（不经 `identSeedLocation`），就会走到这一步：替身缺省回确认，流程照常成功，只是铸造通道多一次调用、Redis 多一条 EVAL。
- 可能因此变红的是这几类断言，都属本次改动，失败请按用例名回报：
  - 装了 `SetPreHook`、按「第一条带某个键的 EVAL」触发的用例（`identHookOnceBeforeEval`），若它的初始状态不是经 `identSeedLocation` 摆的；
  - 按命令次数或顺序设规则的用例（`injectRedisFaultsForTest` 的 skip / times），若规则打在 owner_epoch 键的 GET 上而流程走到了确认脚本；
  - 整库 Dump 与字面量期望比对的用例。

### 6. 残余

- 收尾的回滚 EVAL 自己一个字节都没到 Redis 时，迟到的首发仍可能生效，留下指向从未载入玩家的目标的落点。它记的 epoch 不会被原样下发（第 3e 步）；玩家换到别的节点仍要等无资格撤销的宽限，这是「推路由失败 + 回滚失败」既有的活性代价。
- 位点与它担保的值都在同一个 Redis 里。铸造值随 Redis 回退消失之后又被铸出同一个数值、而此前那次确认的位点写迟到才落地时，位点会替新值作保。同值重铸由 G3a 墙钟地板排除，只在墙钟不起作用（副本间时钟偏差）时才可能。
- 不凭标记的作废在死节点接管上会删掉死节点留下的 location，并让 epoch 越过它，等同接管已经发生；下一次请求按首次落点走。与「铸造生效之后回滚」的既有终态相同。

### 7. 交 Codex

口径同上一节：`GOTOOLCHAIN=local`、`GOPROXY=off`。本次没改 proto、没改 C++，不用生成、不用整解。

1. **shared**，在 `go/shared` 下：
   - `gofmt -l ownerepoch redisslot`，应无输出；
   - `go vet ./ownerepoch/ ./redisslot/`；
   - `go test ./ownerepoch/ ./redisslot/ -count=1`，全部 PASS。
2. **scene_manager**，在 `go/scene_manager` 下：
   - `gofmt -l ./internal`，应无输出；
   - `go vet ./internal/svc ./internal/logic ./internal/metrics`；
   - 先跑本次的用例：`go test ./internal/logic -run 'UnknownMint|RollbackLuaVoid|ConfirmedMark|CannotBeConfirmed|LeftoverOfLateMint|TestClassifyRollbackReply|TestSourceJudgeScriptDeletesOnlyItsOwnLineageAndSeesReceipt' -count=1 -v`，11 条新增用例、`TestClassifyRollbackReply` 与跨语言那一条（含新子用例，不得 SKIP）全部 PASS；
   - 再跑全量：`go test ./internal/... -count=1`。上一节与「第二轮审核续修」一节的用例仍要 PASS；其余失败按第 5 条分辨是不是本次改动引起的，按文件归属分开记。
3. **真实 Redis**，7.2 与 8.x 各一轮，环境同前（`MMORPG_TEST_REDIS_ADDR`，单实例、`appendonly yes`、无副本）：
   - `go test ./internal/svc ./internal/logic -run RealRedis -count=1 -v`；
   - 现在共 10 条（svc 6 条、logic 4 条），要求 10 条 PASS、0 SKIP。回滚脚本本次改过，logic 的 4 条都要看。
4. **失败时保留**：失败用例名、断言输出、`go version`；真实 Redis 那一轮还要 `redis-server --version`。

**Java**：Java 版 owner_epoch 在 SQL 事务内递增，没有 Redis 铸造，不适用，不登记 PARITY。
