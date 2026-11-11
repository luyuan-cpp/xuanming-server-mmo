# owner_epoch 铸造持久化：交 Claude Code 独立审核

最新状态（2026-11-10）：Claude Code 已完成静态独立审核（未编译、未运行），结论见文末「Claude Code 审核结论（2026-11-10）」。没有发现会造成覆盖或同值重铸的缺陷。有两项待处理：not_durable 回滚值被源端采纳的设计缺口，要负责人定方案；Go 铸造通道缺真实 Redis 证据，要 Codex 补测。最近一次运行证据仍是 11-08 的结果，对应 HEAD `c17837c9c`：主仓整解 exit 0（0 warning / 0 error），归属定向 10/10，真实 Redis 7.2.16 与 8.10.2 各 3/3。仍未压测。

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
