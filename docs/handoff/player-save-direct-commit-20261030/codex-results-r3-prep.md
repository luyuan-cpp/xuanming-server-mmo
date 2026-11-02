# 第三轮准备与验证回执（2026-11-02）

本轮执行已结束；失败、跳过与依赖编译失败而未运行的项目均按实际证据列明，不表示全部通过。RPC 按用户追加确认完成纯符号适配并验证，新构建仍失败，未运行旧 EXE。

- 执行目录：`D:\luyuan\xuanming-server-mmo-cpp`，原 main 工作区；未开 worktree、未切分支。
- 范围：本次用户的 6 项准备/验证，加 handoff「11-02 这批改动的验证」；原第二轮 §1–§18 全套没有在本轮重新跑一遍，§13 本轮只读分类。
- 开始 HEAD：`ff8d37d9f73eab7762e6195da93bc9420848294e`。`2b8184428` ancestor 门禁退出 0，三份必备源码存在。
- 初始 `git status --short` 共 7 条：progress.md、family_test.go、sql_boot_catalog_local.ps1 的既有修改及 4 个 dirty 子模块。本轮没有修改业务逻辑或正式配置。除用户单独确认的两条 ASSERT 类型名同步外，没有改变任何断言比较条件或期望值；没有创建 main 提交或 stash/reset/checkout，也没有向 main 合入或从旧分支恢复内容。清理仅按授权创建 refs/archive 留底对象。
- Go `go1.27.2 windows/amd64`；PowerShell `7.6.5`；MySQL `26.7.0`；TiDB 镜像 `v8.5.2`；Redis `7.2.16` / `8.10.2`。
- 本轮日志根：[build/player-store-verify-20261102-r3-prep](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep)。逐条参数、UTC 起止、退出码和完整输出见 [commands.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/commands.jsonl)。不沿用前两轮通过结果。
- 第二轮 `codex-results.md` 保持原件；核对 SHA256 为 `AE20C93D7711FF271CC1F9B0CFB930F361FB2E53151D8F716FE8456DC013A422`。

## 1 清理：失败（残留目录权限错误）

按 handoff 先执行无参数试跑，清单恰为 11 个旧 worktree、6 个非 main 分支；核对路径、24 小时保护、archive 和自动化：automation-2 为 PAUSED，活跃聊天没有使用待删 worktree。之后才执行同一脚本 `-Apply`，未重试或手工补删。

```powershell
pwsh -NoProfile -File docs/handoff/main-only-cleanup-20261101.ps1
pwsh -NoProfile -File docs/handoff/main-only-cleanup-20261101.ps1 -Apply
```

试跑退出 0；Apply 于 09:06:02–09:10:39Z 执行，退出 1。main HEAD/status 前后保护均通过；分支和 worktree 登记仅剩 main，26 条 archive refs 保留；目录删除并未全部成功。

用户指定的 [build/main-only-cleanup/result.txt](D:/luyuan/xuanming-server-mmo-cpp/build/main-only-cleanup/result.txt) 原文：

```text
HEAD: ff8d37d9f73eab7762e6195da93bc9420848294e
Branches:
* main
Worktrees:
D:/luyuan/xuanming-server-mmo-cpp ff8d37d9f [main]
Archived refs: 26
Problems: 1
Could not remove worktree: C:/Users/luyua/.codex/worktrees/f452/xuanming-server-mmo-cpp
```

完整错误原文：`error: failed to delete 'C:/Users/luyua/.codex/worktrees/f452/xuanming-server-mmo-cpp': Permission denied`。清理脚本虽已移除其登记，不能将目录权限失败写成清理全成功。

日志：[试跑](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/01-cleanup-dry-run.log)、[Apply 全文](D:/luyuan/xuanming-server-mmo-cpp/build/main-only-cleanup/apply-r3.log)。没有删除 `ga-boot3-20261030-r1`、`stage4-resume-20261101-r1`、`all-five-resume-20261102-r1` 或前两轮 build 证据。

## 2 C++：失败（全解构建通过，存在运行失败和 RPC 编译失败）

主构建命令：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64 /nr:false`，另加 errors-only 文件日志。未关闭裸指针门禁。

首次命令 `06-msbuild-game` 于 09:11:46–09:42:45Z 执行，退出 1，耗时约 30 分 54 秒，53 警告、2 错误均来自 §9 的 `Hex` 夹具编译问题。原 [完整构建日志](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-msbuild-game.log) 和 [errors-only 原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/msbuild-errors.log) 不覆盖。

获准纯编译修复后，另以 `06-msbuild-game-after-build-fix` 于 09:44:41–10:01:49Z 验证：**全解退出 0，20 警告、0 错误**，裸指针门禁通过。正式缓存包含全仓源码输入，因此单文件修复触发重新扫描；没有关闭或跳过门禁。[修复后完整日志](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-msbuild-game-after-build-fix.log)、[修复后 errors-only](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/msbuild-after-build-fix-errors.log)、[单独修复 diff](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-build-only-fix.diff)。全量 29 工程测试已按下述证据完成调度。

主构建通过后，正式入口 29 工程逐个串行构建，再运行成功工程。全量命令 `06-cpp-tests-full` 于 10:01:51–10:12:22Z 执行，退出 1：28 个工程有实际运行证据，2713 PASS / 35 FAIL / 5 SKIP；`rpc_controller_test` 因编译失败没有运行旧 EXE。

`rpc_controller_test` 不在 `game.sln` 内，整解退出 0 不代表它已编过；本轮统一测试入口的独立构建才暴露其退役接口引用。

| 运行失败工程 | PASS | FAIL |
|---|---:|---:|
| bag_test | 767 | 16 |
| cross_zone_test | 326 | 18 |
| kafka_command_test | 51 | 1 |

35 个失败的名字和全部断言见 [C++ 汇总](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-summary.md)；完整逐案 RUN→FAIL 原文另在 [06-cpp-failed-frames.md](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-failed-frames.md)。没有重试这些失败，没有修改业务逻辑、运行期配置或期望值。

`currency_test` 299/299 PASS，其中 `AssetOpRecallTest` 15/15 PASS。`PlayerStoreClientTest` 45 PASS、`PlayerStoreProjectionTest` 6 PASS、`GuidSegmentClientTest` 15 PASS（这里只列实际终态，不由编译推断）。真 Redis：7.2 的 currency 3 条 + bag 2 条全部 PASS；8.x 仅过滤这 5 条首次执行，5/5 PASS，原生退出码与 XML 均核对，无 SKIP。8.x 日志：[currency](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/currency_test.stdout.log)、[bag](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/bag_test.stdout.log)。

5 个 SKIP 均是 muduo_grpc 的外部 Go 互通用例：4 个缺运行中的 Go client_rpc_router 地址，1 个缺外部 grpc-go client 配合启动端口；原文逐案列于 C++ 汇总。没有将它们算通过，也没有启动完整双区栈来代替本轮限定范围。

`rpc_controller_test` 的原编译错误均来自 `grpc_client_local_reject_test.cpp` 引用已退役 `LoadPlayerData` 类型/Send：clang 默认 20 错上限后停止，原门禁退出 2，不能声称后续潜在错误已见尽。[原完整 build log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/rpc_controller_test.build.log)、[全部已报告错误](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-original-build-errors.txt)。用户随后明确确认了拟议 diff，已将该文件类型/调用名同步为现有内部 RPC `GetPlayerHomeZone`，含两条 ASSERT 的类型名，比较条件和期望值保持原样。

独立验证 `06-rpc-build-after-symbol-fix`：11:31:52–11:32:12Z，`msbuild cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`，退出 1，0 警告、21 条错误。门禁已越过修改的 local_reject 文件，随后在另一个未改的 `grpc_client_loopback_test.cpp` 遇到旧 Request/Response、GrpcPath、Send/Call 类型引用；该回环夹具还使用旧响应的 `set_version`。这些不在本次已确认的单文件纯符号 diff 内，本轮没有继续修改或重试，也没有执行旧 RPC EXE。

因此 RPC 下的 `battle_observer_contract_test`、`RedisClusterClientTest`、`HiredisCommandLifecycle` 等仍属**没跑**；没有将新的编译调用计成测试运行。新诊断的 20 错上限提示一并保留，不能把 21 条诊断当成 21 个独立缺陷。[新构建完整日志](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-build-after-symbol-fix.log)、[新 errors-only 全文](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-symbol-fix-errors.log)、[剩余退役引用静态索引](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-remaining-retired-references.txt)。静态索引不是额外实际编译诊断；后续改码与复验须另留新轮证据，本轮原有 35 个运行失败不覆盖、不重跑。

Linux 镜像按 §6 以 `BUILD_JOBS=1` 单独构建，09:27:18–10:23:55Z 执行，退出 0；场景、gate、battle 及依赖库实际完成。产物 `local/player-store-verify-r3-prep:latest`，linux/amd64，image ID `sha256:94803a373cf0ad3589fdbb160f819a0e66cf1960e7323a7ee34d1b97fd9cdbe7`，没有推送。日志：[完整构建输出](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-linux-docker-build.log)、[实时进度原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-linux-progress-live.log)、[产物检查](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-linux-image-result.log)。

## 3 Go placement：通过

工作目录 `go/shared`：`go test -count=1 ./placement/...`，退出 0，原文 `ok shared/placement 9.651s`。没有使用缓存通过结果。

日志：[03-go-placement.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/03-go-placement.log)。用户随后明确允许安装 MSYS2/UCRT64 GCC 并追加 `-race`。GCC 16.2.0 安装完成后，本轮首次 `go test -race -count=1 ./placement/...` 退出 0，原文 `ok shared/placement 1.062s`；没有 race 报告。只在该子进程设置 `CC`、`CGO_ENABLED=1`、`GOMAXPROCS=2`、`GOFLAGS=-p=2` 和编译器 PATH。

日志：[03-go-placement-race.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/03-go-placement-race.log)、[GCC 版本](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/gcc-version.log)。未重复任何失败测试。

## 4 第二轮 §13 失败时间分组：只读分析完成

分界采用用户指定 `02:53:16Z`；Docker 原件精确 FinishedAt 为 `2026-11-02T02:53:16.959251727Z`。逐条比较原始 Go JSONL 的 RUN、断言输出、FAIL UTC，保留原日志行号，不用命令开始时间代替失败时间。

| 第二轮 §13 新失败 | 退出之前 | 退出之后 |
|---|---:|---:|
| Go FAIL 事件 | 0 | 175 |
| 构成 | 无 | data_service store 168、match 6、独立 MySQL 1 |

因此 **§13 的“之前”组没有用例名或断言原文可列**。不是把未跑/被中止项算通过；175 含父层继承，不能写成 175 个独立顶层用例。后组包括 69 个结局未知、1 个 schema 过期、93 个认证拒绝、1 个 MySQL 目标授权错误和 11 个继承失败。

93 个认证错误与独立 MySQL 的缺授权不能仅凭发生在 TiKV 退出后就归因 TiKV。全部失败名、断言全文、UTC 和原件链接见 [section13-time-analysis.md](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/section13-time-analysis.md)。附录另外列了较早 §4 在退出前的 228 个失败，明确与 §13 的新执行区分。

## 5 PowerShell 合同：失败（全部可执行项已完成）

本轮按当前 42 份文件清单独立重跑 `tools/scripts/tests/*.tests.ps1`；单项失败继续其余，不修改断言。实际执行 41 份合同，加 stress-summarize/docs-index 共 43 条命令：7 条非零，其中 6 份合同共有 48 个具名 FAIL，另 1 条为已知基线本来即红的文档索引检查。调度器退出 0 不代表这些合同通过。

`no_raw_pointer_project` 在 29 工程构建完成、进入仅 EXE 阶段后首次执行，使用系统 TEMP EvidenceDir；退出 0，9 例全部 PASS。36 份 TEMP 证据复制归档后逐文件 SHA256 相等，原件未删。`no_raw_pointer_check.tests.ps1` 明确没跑：独立 Checker 五个候选路径均不存在，未擅自安装 LLVM；VS clang-query 可用且主解构建门禁实际使用它，不将两种检查器混为一项。

失败脚本及具名失败数：`k8s_client_entry_contract` 3、`k8s_deploy_contract` 25、`logical_db_deploy_contract` 8、`publish_images` 6、`redis_player_cluster_contract` 5、`sql_boot_catalog_local` 1；合计 48。`k8s_redis_split_contract`、`go_services_ports`、`go_services_local_mysql`、`docker_debug_sql_boot` 本次退出 0。全部原文与名字列于附录 A。

完整最新失败清单：[latest-script-failures.md](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/latest-script-failures.md)、[JSON 全证据](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/latest-script-failures.json)。这是最终脚本汇总，pending 为空；standalone 缺工具项仍按没跑。另有 [project 证据索引](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-project-evidence.json) 和 [未跑清单](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-not-run.txt)。

## 6 本地环境：已完成（保留首次启动失败）

- TiKV 容器内存从 2 GiB 调至 6 GiB，内存加 swap 总额上限 12 GiB（Docker MemorySwap）；重新启动后观察为 running、OOMKilled=false，未更改服务端正式配置。
- Kafka 本轮重建本轮验证所属的 `player-store-r2-infra` Kafka 容器：`-Xms512m -Xmx1g`，容器上限 2 GiB；实际 Java 命令核对一致，broker API 检查退出 0。未操作其他聊天的 codex-global 容器。
- MySQL 新建专用 `codex_r3` 测试账号，允许实际 Docker 子网 `192.168.80.0/255.255.240.0`；从另一个容器经 TCP 登录成功，SQL 退出 0。随机凭据仅在用户 ACL 保护的忽略目录，不写入本回执、Git 或日志。
- TiDB 首次启动退出 1（非 OOM）：启动时我没有等 TiKV 完成恢复，遇到 region unavailable。原失败已完整保留。随后用户明确允许修正启动顺序、完成环境准备；先核 PD stores=Up、down/pending region 均为 0，再执行一次授权重启。TiDB 正式 MySQL 协议已就绪，专用测试账号从容器经 TCP 登录成功。
- TiDB 使用受限到同一实际子网的 `192.168.80.%` 至 `192.168.95.%` 主机模式，覆盖该 `/20` 容器子网；用户/凭据与 MySQL 专用账号对应。已开启本地 `tidb_enable_check_constraint`，没有重跑任何失败测试。

TiDB 原文：

```text
[FATAL] [terror.go:309] ["unexpected error"] [error="[tikv:9005]Region is unavailable"]
```

日志：[TiDB 本轮完整日志](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-tidb-full-log.log)、[TiKV 本轮完整日志](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-tikv-full-log.log)、[环境调度](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/environment-run.log)、[账号 SQL 结果](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/private-sql-results.jsonl)、[账号公开配置](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/account-public.json)。

授权修正后的证据：[PD stores](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-pd-ready-stores.log)、[down peers=0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-pd-ready-down-peers.log)、[pending peers=0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-pd-ready-pending-peers.log)、[授权启动日志](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-tidb-authorized-startup.log)、[TiDB 账号/版本](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-tidb-test-account.log)、[实际跨容器登录](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-tidb-test-login-from-container.log)、[TiDB 公开配置](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/tidb-account-public.json)。

## 7 新增 Refresh token 验证：失败

在 `go/login`，Redis 7.2 和 8.x 各跑一次：`go test -count=1 -v -run TestTokenFamily_RealRedis ./internal/logic/pkg/token/`。每次显式设置 `LOGIN_TOKEN_REQUIRE_REAL_REDIS=1`、相应 loopback 地址、`LOGIN_TOKEN_TEST_REDIS_DB=9`。两次均退出 1，失败父/子用例一致：

```text
TestTokenFamily_RealRedis
TestTokenFamily_RealRedis/persistent-head-refresh
family_test.go:1566: 真实拒绝不得改写本例任意原件: family err=<nil> 长度 177 -> 177
```

日志：[Redis 7.2](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/16-refresh-redis72.log)、[Redis 8.x](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/16-refresh-redis8.log)。没有打印 token 或含 token 的键名，没有重试。

只读追调用没有找到可达的 Go 写入步骤。静态候选：本例账号长度超过 HASH listpack 默认阈值，读 HGET 可能推进 hashtable rehash，DUMP 的字段序列可能改变而长度仍相同。**目前运行只证明 DUMP 字节不同，未证明字段值改变；保留 FAIL，不把静态推断写成确诊。** 精确代码行、调用顺序及 Redis 官方源码依据见 [只读定位](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/token-family-readonly-analysis.md)。没有据此修改逻辑或断言。

## 8 §18 缺白名单负例：通过（预期退出 1）

移除本次子进程的两种白名单环境变量后，执行与第二轮同一参数：

```powershell
pwsh -NoProfile -NonInteractive -Command "& 'D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\dev_tools.ps1' -Command sql-boot-catalog-local -Zones @(990017,990018) -ZonePortOffsets @('990017=0,990018=1000')"
```

本次退出 1；拒绝位置 `sql_boot_catalog_local.ps1:239`：

```text
Multi-zone mode requires an explicit player database allow list: set DB_ALLOWED_DATABASES or
MMORPG_DB_ALLOWED_DATABASES. The zone_1_db of the source go/db config is not carried over.
```

`expectedReject=true`、`wrongRoot=false`、`catalogCreated=false`，没有再报 `tools/go/data_service` 找不到配置文件。没有由此宣称双区启动或 28 进程 ready；这些完整 §18 正例不在本批四条补验范围，未重跑。

日志：[18-negative-no-allowlist.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/18-negative-no-allowlist.log)、[负例摘要](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/18-negative-summary.json)。

## 9 纯构建修复记录

本轮修改两份测试源码，生产代码未改：

1. [cross_zone_test.cpp:6740](D:/luyuan/xuanming-server-mmo-cpp/cpp/tests/cross_zone_test/cross_zone_test.cpp:6740)：测试夹具使用不存在的无参 `Hex` 构造，且 const 成员不能用于 `emplace_or_replace` 赋值；改为现有 AOI 做法 `remove<Hex>` 后 `emplace<Hex>(player,0,0,0)`。未改断言，修后完整 `game.sln` 退出 0，现有 `EnterFenceEcs.NewSessionInSameSceneRebuildsBaseline` 实际 PASS。
2. [grpc_client_local_reject_test.cpp:35](D:/luyuan/xuanming-server-mmo-cpp/cpp/tests/rpc_controller_test/grpc_client_local_reject_test.cpp:35)：用户单独确认后，仅将退役 `LoadPlayerData` Request/Response/Send 符号同步为 `GetPlayerHomeZone`。应用后核对全文件与确认稿一致；51 条 ASSERT/EXPECT 归一化类型名后逐字相同，仅两条 ASSERT 类型名变化。[实际 diff](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-applied-symbol-fix.diff)。独立编译已执行，但工程被另一个 loopback 文件挡住，运行时没跑。

期间其他会话新增的 bag 持久化、kafka 白名单、Java Mall POM 和既有工作区修改均保留，没有归为本轮修改，也没有用本轮历史结果为后续改过的源码背书。

首次构建中的错误原文为 `registry.hpp(622,94): error G3E2855C1: no matching constructor for initialization of 'Hex'`，并引发该测试工程裸指针门禁 MSB3073/退出 2。初次构建拿全错误结束后，使用独立日志验证修复，原失败保留。未增加新测试或修改断言，直接使用原全解构建和现有回归测试作为编译/运行验证；Java 版本不适用本次 C++ 测试夹具构造修复。

MSYS2/GCC 安装属于用户单独授权的本机工具准备。MSYS2 安装退出 0；初次更新因镜像签名下载超时退出 1，用户又明确允许换官方源继续安装；官方源核心更新 0，后续全量更新曾因 10 秒低速超时退出 1，原件保留。按这项安装授权调整下载超时后全量更新退出 0，GCC 安装退出 0，签名校验保留，版本 `16.2.0`，placement 首次 race 退出 0。没有修改系统 PATH 或 go env 持久设置。来源：[MSYS2 更新流程](https://www.msys2.org/docs/updating/)、[官方镜像说明](https://www.msys2.org/docs/mirrors/)。

安装目录 `C:/Users/luyua/AppData/Local/Programs/msys64`。本套新工具的两份 mirrorlist 改为 `repo.msys2.org`，原件备份在 r3 日志目录；没有修改仓库正式配置。安装期间独立网络传输诊断曾因 180 秒上限退出 28，留下的 17,973,248 字节副本没有用于安装；正式安装仍由 pacman 的包/签名验证完成。所有原始失败在 [命令流水](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/commands.jsonl) 和对应 `gcc-*.log` 中保留。

## 附录 A：本轮脚本失败清单和断言原文


快照 UTC：2026-11-02T10:11:48.959022+00:00

仅07已完成命令；PASS标记是脚本合同用例，不是业务/真实环境验收。
phase尚未完成或仍有pending/缺工具时，不得声明07全部跑完。

完成命令 43；非零 7；PASS标记 1088；具名FAIL 48；仅末尾名单 0；未命名异常 0；汇总缺失 0；缺日志 0。

退出0的调度器/无骨架计数脚本仅记录命令退出，不计业务用例PASS。

完整非零日志、失败正文、全部终端FAILED名单和异常观察：[latest-script-failures.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/latest-script-failures.json)

## 明确未跑及阶段状态

phaseState.finished=True。
- 没跑/缺工具：no_raw_pointer_check.tests.ps1：独立CheckerPath不存在；未安装/升级工具，实际没跑。

## 全部独立错误与失败名

### 错误组 1（1 个记录，原文变体 1）

```text
断言失败: agones + external 的 DryRun 必须成功(DryRun 跳过 CRD 探测)。输出: Kafka topology: brokers=1 controllers=1 replication_factor=1 min_insync_replicas=1
Redis topology: replicas=1 manifest=redis.yaml high_availability=false waitaof_replicas=0
Release: profile=dev image=registry.invalid/test/mmorpg-node:0123456789ab pullPolicy=IfNotPresent go_tag=0123456789ab java_tag=0123456789ab cluster_id=0
WARNING: Working tree is dirty (git status is nonempty); image tag has the -dirty suffix. Production releases (-ReleaseProfile prod) reject this tag.
[dry-run] Skipping current cluster-state preflight (gate / battle modes and address sources, gateway Ingress); actual execution checks before any writes.
Client entry: mode=external battle_orchestrator=agones gate_service_type=NodePort gate_etp=Local require_client_endpoint=true(auto) ingress_host=<none> login_dev_password_auth=False login_wechat=False login_qq=False allow_disruptive_switch=False
PeakShaving: gateway_rate_limit=off(-GatewayRateLimit auto) login_queue=off(-LoginQueue auto) profile=dev initial values (pending stress-test validation) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=45000/zone
AssetOps: off NetworkPolicies: off
GrpcClient deadline budget OK (C++ deadline >= Go zrpc Timeout + 2000): SceneManagerNodeService, DataServiceNodeService, ClientRpcRouterNodeService, MatchNodeService, LoginNodeService, PlayerInboxNodeService, PlayerStoreNodeService; MethodTimeouts exempt (never called from C++): DataServiceNodeService /data_service.DataService/BatchRecallItems=60s, DataServiceNodeService /data_service.DataService/GetRecallBatch=10s, DataServiceNodeService /data_service.DataService/CancelRecallBatch=30s
player-inbox consumer gate: consumer_enabled=true (bin/etc/game_config.yaml) enqueue_types=1 (go/player_inbox/etc/player_inbox.yaml) head=ff8d37d9f73e worktree=clean live=not-needed OK
Router forward budget OK (Go zrpc Timeout + 1000 <= router ForwardTimeoutMs 5000): LoginNodeService, MatchNodeService, TeamNodeService, ChatNodeService, FriendNodeService, GuildNodeService, TradeNodeService, MailNodeService, RankNodeService
MySQL connection ledger: zones=2 steady=896 peak=1508 max_connections=2600 headroom=1092; plan zones=4 peak=2436 headroom=164
MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest data_service_per_instance=79 directory_shard=288 ledger=90 player_store=304 mmorpg=30 mmorpg_friend=80 mmorpg_trade=80 mmorpg_player_inbox=80 mmorpg_guild=80 mmorpg_mail=64 mmorpg_rank=64 mmorpg_chat=64 mmorpg_match=64
Redis connection ledger: zones=2 steady=1413 peak=2447 maxclients=40000 warn_at=32000 headroom=37553; plan zones=4 peak=3303 headroom=36697
Kafka disk ledger: brokers=1 rf=1 audit_partitions=9 command_partitions=512 segment_bytes=33554432 broker_retention_bytes=536870912 other_topics_reserve_gib=4 pvc_gib=40 pvc_source=computed required_gib=40 live_sts_gib=none live_pvc_min_gib=none
DEPLOY-FAILED: 工作负载必须有唯一普通containers段；init不参与runtime密钥注入
 (期望 '0',实际 '1')
```

完整失败名与原始定位：

- `07-k8s_client_entry_contract` / 端到端 infra-up agones + external:battle 是 Fleet(client 端口 Dynamic、health 抬到 105、agones 来源、不写 PORT),RBAC 先于 Fleet,等 Fleet 就绪 / named_case：[07-k8s_client_entry_contract.log:79](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_client_entry_contract.log:79)
  断言/异常条件：断言失败: agones + external 的 DryRun 必须成功(DryRun 跳过 CRD 探测)。 (期望 '0',实际 '1')

### 错误组 2（1 个记录，原文变体 1）

```text
断言失败: agones + podip 的 DryRun 必须成功。输出: Kafka topology: brokers=1 controllers=1 replication_factor=1 min_insync_replicas=1
Redis topology: replicas=1 manifest=redis.yaml high_availability=false waitaof_replicas=0
WARNING: GateServiceType=NodePort only exposes the Service: with -ClientEntryMode podip, login returns the in-cluster POD_IP registered by gate in etcd, so external clients still cannot connect. For external access, use -ClientEntryMode external (gate StatefulSet + per-ordinal Service; see docs/design/k8s-client-entry.md). A single gate-entry is generated only when gate replicas=1 (D90).
Release: profile=dev image=registry.invalid/test/mmorpg-node:0123456789ab pullPolicy=IfNotPresent go_tag=0123456789ab java_tag=0123456789ab cluster_id=0
WARNING: Working tree is dirty (git status is nonempty); image tag has the -dirty suffix. Production releases (-ReleaseProfile prod) reject this tag.
[dry-run] Skipping current cluster-state preflight (gate / battle modes and address sources, gateway Ingress); actual execution checks before any writes.
Client entry: mode=podip battle_orchestrator=agones gate_service_type=NodePort gate_etp=Local require_client_endpoint=false(auto) ingress_host=<none> login_dev_password_auth=False login_wechat=False login_qq=False allow_disruptive_switch=False
PeakShaving: gateway_rate_limit=off(-GatewayRateLimit auto) login_queue=off(-LoginQueue auto) profile=dev initial values (pending stress-test validation) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=45000/zone
AssetOps: off NetworkPolicies: off
GrpcClient deadline budget OK (C++ deadline >= Go zrpc Timeout + 2000): SceneManagerNodeService, DataServiceNodeService, ClientRpcRouterNodeService, MatchNodeService, LoginNodeService, PlayerInboxNodeService, PlayerStoreNodeService; MethodTimeouts exempt (never called from C++): DataServiceNodeService /data_service.DataService/BatchRecallItems=60s, DataServiceNodeService /data_service.DataService/GetRecallBatch=10s, DataServiceNodeService /data_service.DataService/CancelRecallBatch=30s
player-inbox consumer gate: consumer_enabled=true (bin/etc/game_config.yaml) enqueue_types=1 (go/player_inbox/etc/player_inbox.yaml) head=ff8d37d9f73e worktree=clean live=not-needed OK
Router forward budget OK (Go zrpc Timeout + 1000 <= router ForwardTimeoutMs 5000): LoginNodeService, MatchNodeService, TeamNodeService, ChatNodeService, FriendNodeService, GuildNodeService, TradeNodeService, MailNodeService, RankNodeService
MySQL connection ledger: zones=2 steady=896 peak=1508 max_connections=2600 headroom=1092; plan zones=4 peak=2436 headroom=164
MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest data_service_per_instance=79 directory_shard=288 ledger=90 player_store=304 mmorpg=30 mmorpg_friend=80 mmorpg_trade=80 mmorpg_player_inbox=80 mmorpg_guild=80 mmorpg_mail=64 mmorpg_rank=64 mmorpg_chat=64 mmorpg_match=64
Redis connection ledger: zones=2 steady=1413 peak=2447 maxclients=40000 warn_at=32000 headroom=37553; plan zones=4 peak=3303 headroom=36697
Kafka disk ledger: brokers=1 rf=1 audit_partitions=9 command_partitions=512 segment_bytes=33554432 broker_retention_bytes=536870912 other_topics_reserve_gib=4 pvc_gib=40 pvc_source=computed required_gib=40 live_sts_gib=none live_pvc_min_gib=none
DEPLOY-FAILED: 工作负载必须有唯一普通containers段；init不参与runtime密钥注入
 (期望 '0',实际 '1')
```

完整失败名与原始定位：

- `07-k8s_client_entry_contract` / 端到端 infra-up agones + podip:Fleet 只写 CLIENT_ENDPOINT_SOURCE=none / named_case：[07-k8s_client_entry_contract.log:97](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_client_entry_contract.log:97)
  断言/异常条件：断言失败: agones + podip 的 DryRun 必须成功。 (期望 '0',实际 '1')

### 错误组 3（1 个记录，原文变体 1）

```text
断言失败: agones + external 的 DryRun 必须成功,否则下面的扫描是空转 (期望 '0',实际 '1')
```

完整失败名与原始定位：

- `07-k8s_client_entry_contract` / 全部 DryRun 输出:gRPC 口从不进 hostPort / nodePort / Agones ports,不开 hostNetwork;新生成器的工作负载不出现 D92 身份 env / named_case：[07-k8s_client_entry_contract.log:117](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_client_entry_contract.log:117)
  断言/异常条件：断言失败: agones + external 的 DryRun 必须成功,否则下面的扫描是空转 (期望 '0',实际 '1')

### 错误组 4（1 个记录，原文变体 1）

```text
断言失败: 夹具前提:data_service.yaml 现在没有 MethodTimeouts(回档 RPC 已删),下面几条都在文件末尾追加 (不该匹配 /(?mi)^MethodTimeouts[ \t]*:/,但匹配到了 'MethodTimeouts:')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / 负向:给 C++ 会调用的 AllocateIdSegment 配一条超预算的 MethodTimeouts,或把这张表写坏,部署门禁都必须 throw(夹具是临时目录里的配置副本,不碰仓库) / named_case：[07-k8s_deploy_contract.log:30](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:30)
  断言/异常条件：断言失败: 夹具前提:data_service.yaml 现在没有 MethodTimeouts(回档 RPC 已删),下面几条都在文件末尾追加 (不该匹配 /(?mi)^MethodTimeouts[ \t]*:/,但匹配到了 'MethodTimeouts:')

### 错误组 5（2 个记录，原文变体 1）

```text
The term 'Get-FlatSubtreeKeys' is not recognized as a name of a cmdlet, function, script file, or executable program.
Check the spelling of the name, or if a path was included, verify that the path is correct and try again.
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / data-service 的 NameCacheRedis 指向共享 Redis、不写 DB 键;go/db 的 ConfigMap 没有 Redis 句柄 / named_case：[07-k8s_deploy_contract.log:39](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:39)
  断言/异常条件：The term 'Get-FlatSubtreeKeys' is not recognized as a name of a cmdlet, function, script file, or executable program.
- `07-k8s_deploy_contract` / data-service ConfigMap 的 RouteStore 段只写连接池与合服映射刷新周期,取值镜像服务 yaml,池按目录片数摊开 / named_case：[07-k8s_deploy_contract.log:48](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:48)
  断言/异常条件：The term 'Get-FlatSubtreeKeys' is not recognized as a name of a cmdlet, function, script file, or executable program.

### 错误组 6（1 个记录，原文变体 1）

```text
断言失败: staging 档 auto = 开,写路径入口要打出开关解析结果与起步值 (未匹配 /PeakShaving: gateway_rate_limit=on\(-GatewayRateLimit auto\) login_queue=on\(-LoginQueue auto\) profile=staging initial values \(pending stress-test validation\) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=5000/zone/。文本前 600 字符: Kafka topology: brokers=3 controllers=3 replication_factor=3 min_insync_replicas=2
Redis topology: replicas=3 manifest=redis-ha.yaml high_availability=true waitaof_replicas=1
WARNING: GateServiceType=NodePort only exposes the Service: with -ClientEntryMode podip, login returns the in-cluster POD_IP registered by gate in etcd, so external clients still cannot connect. For external access, use -ClientEntryMode external (gate StatefulSet + per-ordinal Service; see docs/design/k8s-client-entry.md). A single gate-entry is generated only when gate replicas=1 (D90).
Release: profile=staging image=)
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / 削峰 staging / prod 档开:gateway 阈值 == 开服档起步值,rate-limit: 只有一个 / named_case：[07-k8s_deploy_contract.log:64](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:64)
  断言/异常条件：断言失败: staging 档 auto = 开,写路径入口要打出开关解析结果与起步值 (未匹配 /PeakShaving: gateway_rate_limit=on\(-GatewayRateLimit auto\) login_queue=on\(-LoginQueue auto\) profile=staging initial values \(pending stress-test validation\) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=5000/zone/。文本前 600 字符: Kafka topology: brokers=3 controllers=3 replication_factor=3 min_insync_replicas=2

### 错误组 7（1 个记录，原文变体 1）

```text
断言失败: staging 档本 zone 的排队容量是开服档起步值 5000(同时在线上限,不是速率) (期望 '5000',实际 '45000')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / 削峰 staging / prod 档开:login Queue 段除三项外 == go/login/etc/login.yaml,锁键带本 zone,ZoneCapacityOverride 只有本 zone / named_case：[07-k8s_deploy_contract.log:69](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:69)
  断言/异常条件：断言失败: staging 档本 zone 的排队容量是开服档起步值 5000(同时在线上限,不是速率) (期望 '5000',实际 '45000')

### 错误组 8（1 个记录，原文变体 1）

```text
断言失败: battle Agones Fleet:带口令的 infra-up DryRun 应当成功(battle 工作负载渲染排在密钥解析之后)。输出: Kafka topology: brokers=1 controllers=1 replication_factor=1 min_insync_replicas=1
Redis topology: replicas=1 manifest=redis.yaml high_availability=false waitaof_replicas=0
Release: profile=dev image=registry.invalid/test/mmorpg-node:0123456789ab pullPolicy=IfNotPresent go_tag=0123456789ab java_tag=0123456789ab cluster_id=0
WARNING: Working tree is dirty (git status is nonempty); image tag has the -dirty suffix. Production releases (-ReleaseProfile prod) reject this tag.
[dry-run] Skipping current cluster-state preflight (gate / battle modes and address sources, gateway Ingress); actual execution checks before any writes.
Client entry: mode=external battle_orchestrator=agones gate_service_type=NodePort gate_etp=Local require_client_endpoint=true(auto) ingress_host=<none> login_dev_password_auth=False login_wechat=False login_qq=False allow_disruptive_switch=False
PeakShaving: gateway_rate_limit=off(-GatewayRateLimit auto) login_queue=off(-LoginQueue auto) profile=dev initial values (pending stress-test validation) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=45000/zone
AssetOps: off NetworkPolicies: off
GrpcClient deadline budget OK (C++ deadline >= Go zrpc Timeout + 2000): SceneManagerNodeService, DataServiceNodeService, ClientRpcRouterNodeService, MatchNodeService, LoginNodeService, PlayerInboxNodeService, PlayerStoreNodeService; MethodTimeouts exempt (never called from C++): DataServiceNodeService /data_service.DataService/BatchRecallItems=60s, DataServiceNodeService /data_service.DataService/GetRecallBatch=10s, DataServiceNodeService /data_service.DataService/CancelRecallBatch=30s
player-inbox consumer gate: consumer_enabled=true (bin/etc/game_config.yaml) enqueue_types=1 (go/player_inbox/etc/player_inbox.yaml) head=ff8d37d9f73e worktree=clean live=not-needed OK
Router forward budget OK (Go zrpc Timeout + 1000 <= router ForwardTimeoutMs 5000): LoginNodeService, MatchNodeService, TeamNodeService, ChatNodeService, FriendNodeService, GuildNodeService, TradeNodeService, MailNodeService, RankNodeService
MySQL connection ledger: zones=2 steady=896 peak=1508 max_connections=2600 headroom=1092; plan zones=4 peak=2436 headroom=164
MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest data_service_per_instance=79 directory_shard=288 ledger=90 player_store=304 mmorpg=30 mmorpg_friend=80 mmorpg_trade=80 mmorpg_player_inbox=80 mmorpg_guild=80 mmorpg_mail=64 mmorpg_rank=64 mmorpg_chat=64 mmorpg_match=64
Redis connection ledger: zones=2 steady=1413 peak=2447 maxclients=40000 warn_at=32000 headroom=37553; plan zones=4 peak=3303 headroom=36697
Kafka disk ledger: brokers=1 rf=1 audit_partitions=9 command_partitions=512 segment_bytes=33554432 broker_retention_bytes=536870912 other_topics_reserve_gib=4 pvc_gib=40 pvc_source=computed required_gib=40 live_sts_gib=none live_pvc_min_gib=none
工作负载必须有唯一普通containers段；init不参与runtime密钥注入
at Get-SqlRuntimeManifestRange, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 11599
at Add-GoSvcEnvEntries, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 11229
at New-BattleWorkloadManifest, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13180
at Apply-Infra, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13269
at <ScriptBlock>, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13662
at <ScriptBlock>, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\lib\captured_script_driver.ps1: line 8
 (期望 '0',实际 '1')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / battle:带口令的 infra-up 里 battle-node-config 的 zoneredis.password == 注入值,battle(Deployment / Agones Fleet)带口令指纹;不注入时空串且无指纹 / named_case：[07-k8s_deploy_contract.log:103](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:103)
  断言/异常条件：断言失败: battle Agones Fleet:带口令的 infra-up DryRun 应当成功(battle 工作负载渲染排在密钥解析之后)。 (期望 '0',实际 '1')

### 错误组 9（1 个记录，原文变体 1）

```text
断言失败: battle Agones 必须生成 battle-node-config
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / node-config 与 battle-node-config 原样镜像 battle_rewards.json，Deployment 与 Agones 均不依赖被遮蔽的镜像配置 / named_case：[07-k8s_deploy_contract.log:143](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:143)
  断言/异常条件：断言失败: battle Agones 必须生成 battle-node-config

### 错误组 10（1 个记录，原文变体 1）

```text
断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / 连 MySQL 的 ConfigMap 只有总账认识的那些(zone-up 三个、infra-up 五个:friend / trade / player-inbox / guild / mail) / named_case：[07-k8s_deploy_contract.log:163](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:163)
  断言/异常条件：断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')

### 错误组 11（1 个记录，原文变体 1）

```text
断言失败: data-service 没有 PoolMirror,ConfiguredPoolConns 只能是 0(显式池上限必须有服务 yaml 出处) (期望 '0',实际 '16')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / Redis 总账输入:客户端表的副本数就是各 manifest 的 replicas;显式 PoolSize 的行等于服务 yaml 的值;有缺省池的 Go 服务 manifest 的 CPU limit ≤ 表里的 GOMAXPROCS(按 2 记的前提) / named_case：[07-k8s_deploy_contract.log:3649](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:3649)
  断言/异常条件：断言失败: data-service 没有 PoolMirror,ConfiguredPoolConns 只能是 0(显式池上限必须有服务 yaml 出处) (期望 '0',实际 '16')

### 错误组 12（1 个记录，原文变体 1）

```text
断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / ConfigMap 镜像 go-redis 池上限 PoolSize == 服务 yaml(zone-up:login / player-locator;infra-up:trade / player-inbox),Redis 连接总账读同一个键 / named_case：[07-k8s_deploy_contract.log:3652](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:3652)
  断言/异常条件：断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')

### 错误组 13（3 个记录，原文变体 1）

```text
断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / guild 部署形态:全局池 2 副本滚动 + PDB + 反亲和 + POD_IP;资产密钥默认不注入;迁移 Job 先于 Deployment / named_case：[07-k8s_deploy_contract.log:7574](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:7574)
  断言/异常条件：断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')
- `07-k8s_deploy_contract` / match 不再发评分快照:ConfigMap 没有 RatingSnapshot* 键,Pod 模板没有 rating-snapshot-publish 注解,参数 -MatchRatingSnapshotPublish 已删 / named_case：[07-k8s_deploy_contract.log:10183](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:10183)
  断言/异常条件：断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')
- `07-k8s_deploy_contract` / rank 部署形态:全局池 2 副本滚动 + PDB + 反亲和 + POD_IP;正规SQL迁移且gRPC / metrics端口同源 / named_case：[07-k8s_deploy_contract.log:12794](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:12794)
  断言/异常条件：断言失败: 带全局 Go 服务的 infra-up DryRun 必须成功。 (期望 '0',实际 '1')

### 错误组 14（1 个记录，原文变体 1）

```text
断言失败: infra-up 产物里应当有 go-svc-guild-config
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / guild ConfigMap 镜像 go/guild/etc/guild.yaml;AssetOp / Activity 整块原样(只 AssetOp.Enabled 改成 false);不写 go-zero Etcd 段 / named_case：[07-k8s_deploy_contract.log:10171](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:10171)
  断言/异常条件：断言失败: infra-up 产物里应当有 go-svc-guild-config

### 错误组 15（1 个记录，原文变体 1）

```text
manifest 块为空 —— DryRun 输出里没找到目标 ConfigMap/Deployment(视为契约破坏)
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / -DatabaseBackend 一个参数切全部服务的 ConfigMap;不传就是 tidb;非法值被参数校验拒绝(方案 §9) / named_case：[07-k8s_deploy_contract.log:10177](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:10177)
  断言/异常条件：manifest 块为空 —— DryRun 输出里没找到目标 ConfigMap/Deployment(视为契约破坏)

### 错误组 16（2 个记录，原文变体 1）

```text
Cannot bind argument to parameter 'Because' because it is an empty string.
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / rank SQL指标与告警同源:确认提交与回放分开，奖励真实延迟及失败保旧，无Redis消费者真相 / named_case：[07-k8s_deploy_contract.log:15395](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:15395)
  断言/异常条件：Cannot bind argument to parameter 'Because' because it is an empty string.
- `07-logical_db_deploy_contract` / MMORPG_DB_PLACEMENT 进写者的 registrar init,只进 init;login 和 gateway 写区域 MySQL 上的 mmorpg,不进 / named_case：[07-logical_db_deploy_contract.log:3720](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:3720)
  断言/异常条件：Cannot bind argument to parameter 'Because' because it is an empty string.

### 错误组 17（1 个记录，原文变体 1）

```text
断言失败: AssetOps off 只禁用 Guild/Trade 两条可选资产通道；必需的 MAIL、目录和SQL签名不受此开关控制 (不该匹配 /MMORPG_ASSET_OP_SECRET_(?:GUILD|TRADE)\b|asset-op-secret-(?:guild|trade)\b/,但匹配到了 'MMORPG_ASSET_OP_SECRET_GUILD')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / 资产通道默认关:scene / guild / trade 不带签名密钥与指纹,不生成 NetworkPolicy,只删以前留下的同名 Secret 与策略;trade 的 AssetOp 仍是两行 / named_case：[07-k8s_deploy_contract.log:15397](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:15397)
  断言/异常条件：断言失败: AssetOps off 只禁用 Guild/Trade 两条可选资产通道；必需的 MAIL、目录和SQL签名不受此开关控制 (不该匹配 /MMORPG_ASSET_OP_SECRET_(?:GUILD|TRADE)\b|asset-op-secret-(?:guild|trade)\b/,但匹配到了 'MMORPG_ASSET_OP_SECRET_GUILD')

### 错误组 18（1 个记录，原文变体 1）

```text
断言失败: infra-up 打开两个开关必须成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / infra-up -AssetOps on:guild / trade 各自只拿自己那把密钥、带指纹、ConfigMap 打开通道(trade 整块搬运);guild / match 的准入策略名单逐字钉住、开放端口覆盖清单 / named_case：[07-k8s_deploy_contract.log:15405](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:15405)
  断言/异常条件：断言失败: infra-up 打开两个开关必须成功。 (期望 '0',实际 '1')

### 错误组 19（1 个记录，原文变体 1）

```text
断言失败: data-service 只剩 go-zero 的 NameCacheRedis 一个缺省池,路由发布与对账的专用池已删 (期望 '0/1/0',实际 '16/1/0')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / Redis 连接总账:没有 db 行,login 行没有 Pub/Sub,data-service 行只有 NameCacheRedis 的缺省池 / named_case：[07-k8s_deploy_contract.log:19045](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:19045)
  断言/异常条件：断言失败: data-service 只剩 go-zero 的 NameCacheRedis 一个缺省池,路由发布与对账的专用池已删 (期望 '0/1/0',实际 '16/1/0')

### 错误组 20（1 个记录，原文变体 1）

```text
断言失败: infra-up DryRun 应当成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / guild:go/guild 已删合服标记段,ConfigMap 不再写 MergeMarkerRedis / RequireRouteReady / named_case：[07-k8s_deploy_contract.log:19051](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:19051)
  断言/异常条件：断言失败: infra-up DryRun 应当成功。 (期望 '0',实际 '1')

### 错误组 21（1 个记录，原文变体 1）

```text
断言失败: 带 infra 与全局 Go 服务的 all-up DryRun 应当成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / all-up:全局服务的迁移 Job 先于 data-service,进程排在第一个 zone 的 data-service 就绪之后,再到其余 zone / named_case：[07-k8s_deploy_contract.log:22658](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:22658)
  断言/异常条件：断言失败: 带 infra 与全局 Go 服务的 all-up DryRun 应当成功。 (期望 '0',实际 '1')

### 错误组 22（1 个记录，原文变体 1）

```text
断言失败: (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / all-up -SqlSchemaOnly:infra 和全局服务的 Job 照跑,zone 只跑 db、data-service 的 Job,不起写者进程 / named_case：[07-k8s_deploy_contract.log:24912](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:24912)
  断言/异常条件：断言失败: (期望 '0',实际 '1')

### 错误组 23（1 个记录，原文变体 1）

```text
断言失败: infra-up DryRun 应当成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / infra-up:DryRun 按「已有 zone」渲染升级路径,全局服务的进程排在 data-service 就绪之后;zone-up 不碰全局服务 / named_case：[07-k8s_deploy_contract.log:28675](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:28675)
  断言/异常条件：断言失败: infra-up DryRun 应当成功。 (期望 '0',实际 '1')

### 错误组 24（1 个记录，原文变体 1）

```text
断言失败: infra-up battle Agones Fleet:DryRun 应当成功。输出: Kafka topology: brokers=1 controllers=1 replication_factor=1 min_insync_replicas=1
Redis topology: replicas=1 manifest=redis.yaml high_availability=false waitaof_replicas=0
Release: profile=dev image=registry.invalid/test/mmorpg-node:0123456789ab pullPolicy=IfNotPresent go_tag=0123456789ab java_tag=0123456789ab cluster_id=0
WARNING: Working tree is dirty (git status is nonempty); image tag has the -dirty suffix. Production releases (-ReleaseProfile prod) reject this tag.
[dry-run] Skipping current cluster-state preflight (gate / battle modes and address sources, gateway Ingress); actual execution checks before any writes.
Client entry: mode=external battle_orchestrator=agones gate_service_type=NodePort gate_etp=Local require_client_endpoint=true(auto) ingress_host=<none> login_dev_password_auth=False login_wechat=False login_qq=False allow_disruptive_switch=False
PeakShaving: gateway_rate_limit=off(-GatewayRateLimit auto) login_queue=off(-LoginQueue auto) profile=dev initial values (pending stress-test validation) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=45000/zone
AssetOps: off NetworkPolicies: off
GrpcClient deadline budget OK (C++ deadline >= Go zrpc Timeout + 2000): SceneManagerNodeService, DataServiceNodeService, ClientRpcRouterNodeService, MatchNodeService, LoginNodeService, PlayerInboxNodeService, PlayerStoreNodeService; MethodTimeouts exempt (never called from C++): DataServiceNodeService /data_service.DataService/BatchRecallItems=60s, DataServiceNodeService /data_service.DataService/GetRecallBatch=10s, DataServiceNodeService /data_service.DataService/CancelRecallBatch=30s
player-inbox consumer gate: consumer_enabled=true (bin/etc/game_config.yaml) enqueue_types=1 (go/player_inbox/etc/player_inbox.yaml) head=ff8d37d9f73e worktree=clean live=not-needed OK
Router forward budget OK (Go zrpc Timeout + 1000 <= router ForwardTimeoutMs 5000): LoginNodeService, MatchNodeService, TeamNodeService, ChatNodeService, FriendNodeService, GuildNodeService, TradeNodeService, MailNodeService, RankNodeService
MySQL connection ledger: zones=2 steady=896 peak=1508 max_connections=2600 headroom=1092; plan zones=4 peak=2436 headroom=164
MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest data_service_per_instance=79 directory_shard=288 ledger=90 player_store=304 mmorpg=30 mmorpg_friend=80 mmorpg_trade=80 mmorpg_player_inbox=80 mmorpg_guild=80 mmorpg_mail=64 mmorpg_rank=64 mmorpg_chat=64 mmorpg_match=64
Redis connection ledger: zones=2 steady=1413 peak=2447 maxclients=40000 warn_at=32000 headroom=37553; plan zones=4 peak=3303 headroom=36697
Kafka disk ledger: brokers=1 rf=1 audit_partitions=9 command_partitions=512 segment_bytes=33554432 broker_retention_bytes=536870912 other_topics_reserve_gib=4 pvc_gib=40 pvc_source=computed required_gib=40 live_sts_gib=none live_pvc_min_gib=none
工作负载必须有唯一普通containers段；init不参与runtime密钥注入
at Get-SqlRuntimeManifestRange, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 11599
at Add-GoSvcEnvEntries, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 11229
at New-BattleWorkloadManifest, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13180
at Apply-Infra, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13269
at <ScriptBlock>, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13662
at <ScriptBlock>, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\lib\captured_script_driver.ps1: line 8
 (期望 '0',实际 '1')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / B0-4:不传 -NodeOpenFileLimitMode 时五套 C++ 模板都不写 NODE_OPEN_FILE_LIMIT_MODE,也不打豁免告警 / named_case：[07-k8s_deploy_contract.log:31337](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:31337)
  断言/异常条件：断言失败: infra-up battle Agones Fleet:DryRun 应当成功。 (期望 '0',实际 '1')

### 错误组 25（1 个记录，原文变体 1）

```text
断言失败: infra-up battle Agones Fleet enforce:DryRun 应当成功。输出: Kafka topology: brokers=1 controllers=1 replication_factor=1 min_insync_replicas=1
Redis topology: replicas=1 manifest=redis.yaml high_availability=false waitaof_replicas=0
Release: profile=dev image=registry.invalid/test/mmorpg-node:0123456789ab pullPolicy=IfNotPresent go_tag=0123456789ab java_tag=0123456789ab cluster_id=0
WARNING: Working tree is dirty (git status is nonempty); image tag has the -dirty suffix. Production releases (-ReleaseProfile prod) reject this tag.
[dry-run] Skipping current cluster-state preflight (gate / battle modes and address sources, gateway Ingress); actual execution checks before any writes.
Client entry: mode=external battle_orchestrator=agones gate_service_type=NodePort gate_etp=Local require_client_endpoint=true(auto) ingress_host=<none> login_dev_password_auth=False login_wechat=False login_qq=False allow_disruptive_switch=False
PeakShaving: gateway_rate_limit=off(-GatewayRateLimit auto) login_queue=off(-LoginQueue auto) profile=dev initial values (pending stress-test validation) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=45000/zone
AssetOps: off NetworkPolicies: off
GrpcClient deadline budget OK (C++ deadline >= Go zrpc Timeout + 2000): SceneManagerNodeService, DataServiceNodeService, ClientRpcRouterNodeService, MatchNodeService, LoginNodeService, PlayerInboxNodeService, PlayerStoreNodeService; MethodTimeouts exempt (never called from C++): DataServiceNodeService /data_service.DataService/BatchRecallItems=60s, DataServiceNodeService /data_service.DataService/GetRecallBatch=10s, DataServiceNodeService /data_service.DataService/CancelRecallBatch=30s
player-inbox consumer gate: consumer_enabled=true (bin/etc/game_config.yaml) enqueue_types=1 (go/player_inbox/etc/player_inbox.yaml) head=ff8d37d9f73e worktree=clean live=not-needed OK
Router forward budget OK (Go zrpc Timeout + 1000 <= router ForwardTimeoutMs 5000): LoginNodeService, MatchNodeService, TeamNodeService, ChatNodeService, FriendNodeService, GuildNodeService, TradeNodeService, MailNodeService, RankNodeService
MySQL connection ledger: zones=2 steady=896 peak=1508 max_connections=2600 headroom=1092; plan zones=4 peak=2436 headroom=164
MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest data_service_per_instance=79 directory_shard=288 ledger=90 player_store=304 mmorpg=30 mmorpg_friend=80 mmorpg_trade=80 mmorpg_player_inbox=80 mmorpg_guild=80 mmorpg_mail=64 mmorpg_rank=64 mmorpg_chat=64 mmorpg_match=64
Redis connection ledger: zones=2 steady=1413 peak=2447 maxclients=40000 warn_at=32000 headroom=37553; plan zones=4 peak=3303 headroom=36697
Kafka disk ledger: brokers=1 rf=1 audit_partitions=9 command_partitions=512 segment_bytes=33554432 broker_retention_bytes=536870912 other_topics_reserve_gib=4 pvc_gib=40 pvc_source=computed required_gib=40 live_sts_gib=none live_pvc_min_gib=none
工作负载必须有唯一普通containers段；init不参与runtime密钥注入
at Get-SqlRuntimeManifestRange, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 11599
at Add-GoSvcEnvEntries, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 11229
at New-BattleWorkloadManifest, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13180
at Apply-Infra, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13269
at <ScriptBlock>, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\k8s_deploy.ps1: line 13662
at <ScriptBlock>, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\lib\captured_script_driver.ps1: line 8
 (期望 '0',实际 '1')
```

完整失败名与原始定位：

- `07-k8s_deploy_contract` / B0-4:-NodeOpenFileLimitMode warn / enforce 写进本次部署的全部 C++ 工作负载(五套模板),每个恰好一次、取值原样;只有 warn 打告警 / named_case：[07-k8s_deploy_contract.log:31361](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log:31361)
  断言/异常条件：断言失败: infra-up battle Agones Fleet enforce:DryRun 应当成功。 (期望 '0',实际 '1')

### 错误组 26（1 个记录，原文变体 1）

```text
断言失败: (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-logical_db_deploy_contract` / mysql-init:K=1 建基名和流水库,K=64 建全部片库和流水库,基名不再是库 / named_case：[07-logical_db_deploy_contract.log:15](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:15)
  断言/异常条件：断言失败: (期望 '0',实际 '1')

### 错误组 27（2 个记录，原文变体 1）

```text
DryRun 输出里 ConfigMap go-svc-guild-config 应当恰好 1 份,实际 0 份
```

完整失败名与原始定位：

- `07-logical_db_deploy_contract` / DbPlacement 段的位置和 Go 的配置结构一致 / named_case：[07-logical_db_deploy_contract.log:3717](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:3717)
  断言/异常条件：DryRun 输出里 ConfigMap go-svc-guild-config 应当恰好 1 份,实际 0 份
- `07-logical_db_deploy_contract` / chat、mail、db 的内部 RPC 配置:chat 和 db 走 etcd 发现目录,mail 直连 guild Service / named_case：[07-logical_db_deploy_contract.log:3722](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:3722)
  断言/异常条件：DryRun 输出里 ConfigMap go-svc-guild-config 应当恰好 1 份,实际 0 份

### 错误组 28（1 个记录，原文变体 1）

```text
DryRun 输出里 Deployment chat 应当恰好 1 份,实际 0 份
```

完整失败名与原始定位：

- `07-logical_db_deploy_contract` / 写者的控制文件都在 emptyDir 挂载根下的私有子目录(挂载根过不了私有目录校验) / named_case：[07-logical_db_deploy_contract.log:3733](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:3733)
  断言/异常条件：DryRun 输出里 Deployment chat 应当恰好 1 份,实际 0 份

### 错误组 29（1 个记录，原文变体 1）

```text
DryRun 输出里 Deployment guild 应当恰好 1 份,实际 0 份
```

完整失败名与原始定位：

- `07-logical_db_deploy_contract` / registrar 带 MMORPG_SQL_BACKEND:login、gateway 固定 mysql,其余跟 -DatabaseBackend,只进 init / named_case：[07-logical_db_deploy_contract.log:3735](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:3735)
  断言/异常条件：DryRun 输出里 Deployment guild 应当恰好 1 份,实际 0 份

### 错误组 30（1 个记录，原文变体 1）

```text
断言失败: infra-up 不部署 db、data-service,不要这份清单。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-logical_db_deploy_contract` / MMORPG_DB_ALLOWED_DATABASES 在写路径入口核:没给、不合规都在任何清单之前拒绝,dev 也一样 / named_case：[07-logical_db_deploy_contract.log:3738](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:3738)
  断言/异常条件：断言失败: infra-up 不部署 db、data-service,不要这份清单。 (期望 '0',实际 '1')

### 错误组 31（1 个记录，原文变体 1）

```text
断言失败: logical-infra 一份 (期望 '1',实际 '0')
```

完整失败名与原始定位：

- `07-logical_db_deploy_contract` / 回收 CronJob:zone 和 infra 各一份,先于写者,只有 pods 的三个动词,非 root,只认主容器的退出码 78 / named_case：[07-logical_db_deploy_contract.log:6758](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log:6758)
  断言/异常条件：断言失败: logical-infra 一份 (期望 '1',实际 '0')

### 错误组 32（4 个记录，原文变体 1）

```text
Argument types do not match at <ScriptBlock>, D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\publish_images.ps1: line 636
```

完整失败名与原始定位：

- `07-publish_images` / 成功:快照轨全量发布,布局 / 清单 / build-info / 符号 / sha256sums / latest 指针正确 / named_case：[07-publish_images.log:11](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-publish_images.log:11)
  断言/异常条件：断言失败: 合法输入应当发布成功。 (期望 '0',实际 '1')
- `07-publish_images` / -SkipIfExists:同一提交已发布时 exit 0 且不再调用镜像脚本 / named_case：[07-publish_images.log:15](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-publish_images.log:15)
  断言/异常条件：断言失败: CI 幂等重跑必须静默成功。 (期望 '0',实际 '1')
- `07-publish_images` / 成功:release 轨发布,版本号注入镜像脚本,目录与 tag 按版本号命名 / named_case：[07-publish_images.log:17](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-publish_images.log:17)
  断言/异常条件：断言失败: (期望 '0',实际 '1')
- `07-publish_images` / 构建路径:按 family 顺序先 preflight/构建,再 list-refs,且都带本次 tag / named_case：[07-publish_images.log:21](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-publish_images.log:21)
  断言/异常条件：断言失败: (期望 '0',实际 '1')

### 错误组 33（1 个记录，原文变体 1）

```text
Could not find a part of the path 'C:\Users\luyua\AppData\Local\Temp\publish-images-tests-421048440d784ccf905e81f1566b6eca\case-09\artifacts\snapshots\images\gd726366b8ba5\sha256sums.txt'.
```

完整失败名与原始定位：

- `07-publish_images` / 负向:同一提交再次发布必须被不可变规则拒绝,已发布内容不变 / named_case：[07-publish_images.log:13](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-publish_images.log:13)
  断言/异常条件：Could not find a part of the path 'C:\Users\luyua\AppData\Local\Temp\publish-images-tests-421048440d784ccf905e81f1566b6eca\case-09\artifacts\snapshots\images\gd726366b8ba5\sha256sums.txt'.

### 错误组 34（1 个记录，原文变体 1）

```text
断言失败: 错误要说清是版本号被另一提交占用 (未匹配 /已由其他提交发布/。文本前 600 字符: [INFO] channel=release version=v1.2.3 tag=v1.2.3-ae108b1e3da0 commit=ae108b1e3da0 families=go [INFO] -SkipBuild: skip builds and collect existing images only (revision label must equal ae108b1e3da0) ===== Collect image manifest (list-refs) ===== [INFO] go registry.invalid/test/mmorpg-login:v1.2.3-ae108b1e3da0 [INFO] go registry.invalid/test/mmorpg-db:v1.2.3-ae108b1e3da0 ===== Check local images (must exist, revision label == ae108b1e3da0, version label == v1.2.3) ===== Error response from daemon: No such image: registry.invalid/test/mmorpg-login:v1.2.3-ae108b1e3da0 Error response from daemon: )
```

完整失败名与原始定位：

- `07-publish_images` / 负向:-SkipIfExists 不能掩盖"同一版本号已由其他提交发布" / named_case：[07-publish_images.log:19](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-publish_images.log:19)
  断言/异常条件：断言失败: 错误要说清是版本号被另一提交占用 (未匹配 /已由其他提交发布/。文本前 600 字符: [INFO] channel=release version=v1.2.3 tag=v1.2.3-ae108b1e3da0 commit=ae108b1e3da0 families=go [INFO] -SkipBuild: skip builds and collect existing images only (revision label must equal ae108b1e3da0) ===== Collect image manifest (list-refs) ===== [INFO] go registry.invalid/test/mmorpg-login:v1.2.3-ae108b1e3da0 [INFO] go registry.invalid/test/mmorpg-db:v1.2.3-ae108b1e3da0 ===== Check local images (must exist, revision label == ae108b1e3da0, version label == v1.2.3) ===== Error response from daemon: No such image: registry.invalid/test/mmorpg-login:v1.2.3-ae108b1e3da0 Error response from daemon: )

### 错误组 35（1 个记录，原文变体 1）

```text
断言失败: dev 缺省 infra-up DryRun 必须成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-redis_player_cluster_contract` / dev 缺省 infra-up:不渲染集群;读登录态的全局服务显式指向 login 的实例与 DB,口令走各自原来的路子 / named_case：[07-redis_player_cluster_contract.log:17](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_player_cluster_contract.log:17)
  断言/异常条件：断言失败: dev 缺省 infra-up DryRun 必须成功。 (期望 '0',实际 '1')

### 错误组 36（1 个记录，原文变体 1）

```text
断言失败: infra-up DryRun 必须成功。 (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-redis_player_cluster_contract` / -PlayerRedisMasters 3 的 infra-up:清单、Secret、建群 Job 都渲染,排在全局 Go 服务之前,占位全部替换,口令不进产物 / named_case：[07-redis_player_cluster_contract.log:4223](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_player_cluster_contract.log:4223)
  断言/异常条件：断言失败: infra-up DryRun 必须成功。 (期望 '0',实际 '1')

### 错误组 37（1 个记录，原文变体 1）

```text
DryRun 输出里 Deployment match 应当恰好 1 份,实际 0 份
```

完整失败名与原始定位：

- `07-redis_player_cluster_contract` / -PlayerRedisMasters 3 的 infra-up:全局服务的 PlayerRedis、LoginRedis 写种子与 PasswordEnv,容器有口令环境变量 / named_case：[07-redis_player_cluster_contract.log:8475](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_player_cluster_contract.log:8475)
  断言/异常条件：DryRun 输出里 Deployment match 应当恰好 1 份,实际 0 份

### 错误组 38（1 个记录，原文变体 1）

```text
断言失败: (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-redis_player_cluster_contract` / 每主 2 副本:节点数 ×1.5,min-replicas-to-write 取 1 / named_case：[07-redis_player_cluster_contract.log:8479](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_player_cluster_contract.log:8479)
  断言/异常条件：断言失败: (期望 '0',实际 '1')

### 错误组 39（1 个记录，原文变体 1）

```text
断言失败: (期望 '0',实际 '1')
正文包含长 DryRun/上下文；完整原文保留在 JSON，不作自动归因。
```

完整失败名与原始定位：

- `07-redis_player_cluster_contract` / 加主节点不增加每节点连接:-PlayerRedisMasters 3 与 6 的 steady 相同 / named_case：[07-redis_player_cluster_contract.log:12028](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_player_cluster_contract.log:12028)
  断言/异常条件：断言失败: (期望 '0',实际 '1')

### 错误组 40（1 个记录，原文变体 1）

```text
断言失败: 没有列级锁写 (不该匹配 /lock_touch/,但匹配到了 'lock_touch')
```

完整失败名与原始定位：

- `07-sql_boot_catalog_local` / TiDB 档:SELECT_ONLY、不带 -LocalMySQL、gateway 保留强核验 / named_case：[07-sql_boot_catalog_local.log:332](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-sql_boot_catalog_local.log:332)
  断言/异常条件：断言失败: 没有列级锁写 (不该匹配 /lock_touch/,但匹配到了 'lock_touch')

### 错误组 41（1 个记录，原文变体 1）

```text
--- STDERR ---
docs/README.md \u7d22\u5f15\u5df2\u8fc7\u671f: \u8bf7\u8fd0\u884c python tools/scripts/gen_docs_index.py
```

完整失败名与原始定位：

- `07-docs-index` / 文档索引 --check 失败 / known_baseline：[07-docs-index.log:1](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-docs-index.log:1)
  断言/异常条件：--- STDERR ---

## 全部完成命令与完整日志索引

| 命令 | exit | PASS标记 | FAIL标记 | 日志/证据 |
|---|---:|---:|---:|---|
| `07-artifacts_fetch_import_retention` | 0 | 29 | 0 | [07-artifacts_fetch_import_retention.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-artifacts_fetch_import_retention.log) |
| `07-artifacts_lib` | 0 | 23 | 0 | [07-artifacts_lib.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-artifacts_lib.log) |
| `07-chat_authority_deploy` | 0 | 3 | 0 | [07-chat_authority_deploy.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-chat_authority_deploy.log) |
| `07-deploy_capture_contract` | 0 | 6 | 0 | [07-deploy_capture_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-deploy_capture_contract.log) |
| `07-dev_tools_db_move_contract` | 0 | 4 | 0 | [07-dev_tools_db_move_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-dev_tools_db_move_contract.log) |
| `07-dev_tools_kafka_offset_reset` | 0 | 6 | 0 | [07-dev_tools_kafka_offset_reset.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-dev_tools_kafka_offset_reset.log) |
| `07-dev_tools_merge_zone_contract` | 0 | 13 | 0 | [07-dev_tools_merge_zone_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-dev_tools_merge_zone_contract.log) |
| `07-dev_tools_proto_gen_exit` | 0 | 35 | 0 | [07-dev_tools_proto_gen_exit.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-dev_tools_proto_gen_exit.log) |
| `07-docker_debug_sql_boot` | 0 | 32 | 0 | [07-docker_debug_sql_boot.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-docker_debug_sql_boot.log) |
| `07-etcd_maintenance_contract` | 0 | 9 | 0 | [07-etcd_maintenance_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-etcd_maintenance_contract.log) |
| `07-global_authority_deploy` | 0 | 7 | 0 | [07-global_authority_deploy.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-global_authority_deploy.log) |
| `07-go_services_local_mysql` | 0 | 40 | 0 | [07-go_services_local_mysql.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-go_services_local_mysql.log) |
| `07-go_services_ports` | 0 | 28 | 0 | [07-go_services_ports.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-go_services_ports.log) |
| `07-go_services_stop` | 0 | 15 | 0 | [07-go_services_stop.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-go_services_stop.log) |
| `07-go_svc_image_authority` | 0 | 4 | 0 | [07-go_svc_image_authority.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-go_svc_image_authority.log) |
| `07-id_issuer_deploy_contract` | 0 | 6 | 0 | [07-id_issuer_deploy_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-id_issuer_deploy_contract.log) |
| `07-k8s_client_entry_contract` | 1 | 108 | 3 | [07-k8s_client_entry_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_client_entry_contract.log) |
| `07-k8s_deploy_contract` | 1 | 255 | 25 | [07-k8s_deploy_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_deploy_contract.log) |
| `07-k8s_gate_drain` | 0 | 55 | 0 | [07-k8s_gate_drain.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_gate_drain.log) |
| `07-k8s_login_auth_provider_guard` | 0 | 41 | 0 | [07-k8s_login_auth_provider_guard.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_login_auth_provider_guard.log) |
| `07-k8s_migrate_gate` | 0 | 68 | 0 | [07-k8s_migrate_gate.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_migrate_gate.log) |
| `07-k8s_redis_split_contract` | 0 | 43 | 0 | [07-k8s_redis_split_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_redis_split_contract.log) |
| `07-k8s_route_tool` | 0 | 16 | 0 | [07-k8s_route_tool.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-k8s_route_tool.log) |
| `07-kafka_offset_reset_output_guard` | 0 | 24 | 0 | [07-kafka_offset_reset_output_guard.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-kafka_offset_reset_output_guard.log) |
| `07-lobby_authority_render` | 0 | 0 | 0 | [07-lobby_authority_render.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-lobby_authority_render.log) |
| `07-logical_db_deploy_contract` | 1 | 26 | 8 | [07-logical_db_deploy_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-logical_db_deploy_contract.log) |
| `07-login_metrics_contract` | 0 | 3 | 0 | [07-login_metrics_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-login_metrics_contract.log) |
| `07-mail_admin_dev_token` | 0 | 9 | 0 | [07-mail_admin_dev_token.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-mail_admin_dev_token.log) |
| `07-maintenance_drain` | 0 | 16 | 0 | [07-maintenance_drain.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-maintenance_drain.log) |
| `07-make_release` | 0 | 11 | 0 | [07-make_release.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-make_release.log) |
| `07-no_raw_pointer_memory` | 0 | 0 | 0 | [07-no_raw_pointer_memory.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-no_raw_pointer_memory.log) |
| `07-no_raw_pointer_setup` | 0 | 0 | 0 | [07-no_raw_pointer_setup.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-no_raw_pointer_setup.log) |
| `07-publish_images` | 1 | 9 | 6 | [07-publish_images.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-publish_images.log) |
| `07-redis_exporter_contract` | 0 | 5 | 0 | [07-redis_exporter_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_exporter_contract.log) |
| `07-redis_maxclients_contract` | 0 | 7 | 0 | [07-redis_maxclients_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_maxclients_contract.log) |
| `07-redis_player_cluster_contract` | 1 | 29 | 5 | [07-redis_player_cluster_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-redis_player_cluster_contract.log) |
| `07-release_common_version` | 0 | 21 | 0 | [07-release_common_version.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-release_common_version.log) |
| `07-sql_boot_catalog_local` | 1 | 48 | 1 | [07-sql_boot_catalog_local.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-sql_boot_catalog_local.log) |
| `07-start_game_command_contract` | 0 | 28 | 0 | [07-start_game_command_contract.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-start_game_command_contract.log) |
| `07-stress_snap_redis` | 0 | 6 | 0 | [07-stress_snap_redis.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-stress_snap_redis.log) |
| `07-stress-summarize` | 0 | 0 | 0 | [07-stress-summarize.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-stress-summarize.log) |
| `07-docs-index` | 1 | 0 | 0 | [07-docs-index.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-docs-index.log) |
| `07-no_raw_pointer_project` | 0 | 0 | 0 | [07-no_raw_pointer_project.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/07-no_raw_pointer_project.log) |

可在新增命令完成后再次运行本聚合器；命令/日志签名未变的记录从本轮JSON缓存复用，不运行任何验证。

## 附录 B：C++ 实际运行、失败和未跑原文


快照UTC：2026-11-02T11:38:40.110516+00:00。只据真实日志，不写最终回执，不覆盖原件。

全量命令：`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -LogDir D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261102-r3-prep\06-cpp-tests`；cwd=`D:/luyuan/xuanming-server-mmo-cpp`；UTC `2026-11-02T10:01:51.5317408+00:00` → `2026-11-02T10:12:22.1235216+00:00`；exit=1；[完整stdout/stderr与总表](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-full.log)。

每工程必须本轮串行Build；构建失败工程未运行旧EXE。每EXE默认120秒，不放宽、不重试。SKIP算没跑。全量gtest实际终态：`{"pass": 2713, "fail": 35, "skip": 5}`。

| 工程 | Build | PASS | FAIL | SKIP | 无终态 | 原件 |
|---|---|---:|---:|---:|---:|---|
| agones_lifecycle_test | OK | 69 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/agones_lifecycle_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/agones_lifecycle_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/agones_lifecycle_test.stderr.log) |
| aoi_delivery_test | OK | 21 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/aoi_delivery_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/aoi_delivery_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/aoi_delivery_test.stderr.log) |
| aoi_test | OK | 61 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/aoi_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/aoi_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/aoi_test.stderr.log) |
| bag_test | OK | 767 | 16 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.stderr.log) |
| buff_test | OK | 35 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/buff_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/buff_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/buff_test.stderr.log) |
| client_endpoint_test | OK | 70 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/client_endpoint_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/client_endpoint_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/client_endpoint_test.stderr.log) |
| configuration_table_test | OK | 50 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/configuration_table_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/configuration_table_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/configuration_table_test.stderr.log) |
| cool_down_time_test | OK | 10 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/cool_down_time_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/cool_down_time_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/cool_down_time_test.stderr.log) |
| cross_zone_test | OK | 326 | 18 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/cross_zone_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/cross_zone_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/cross_zone_test.stderr.log) |
| currency_test | OK | 299 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/currency_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/currency_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/currency_test.stderr.log) |
| ecs_context_test | OK | 8 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/ecs_context_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/ecs_context_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/ecs_context_test.stderr.log) |
| kafka_command_test | OK | 51 | 1 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/kafka_command_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/kafka_command_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/kafka_command_test.stderr.log) |
| message_limiter_test | OK | 3 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/message_limiter_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/message_limiter_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/message_limiter_test.stderr.log) |
| missions_test | OK | 17 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/missions_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/missions_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/missions_test.stderr.log) |
| muduo_grpc_test | OK | 200 | 0 | 5 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/muduo_grpc_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/muduo_grpc_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/muduo_grpc_test.stderr.log) |
| node_sequence_test | OK | 2 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/node_sequence_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/node_sequence_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/node_sequence_test.stderr.log) |
| player_landing_test | OK | 13 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/player_landing_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/player_landing_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/player_landing_test.stderr.log) |
| proto_field_checker_test | OK | 9 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/proto_field_checker_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/proto_field_checker_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/proto_field_checker_test.stderr.log) |
| readfile2string_test | OK | 4 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/readfile2string_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/readfile2string_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/readfile2string_test.stderr.log) |
| reward_test | OK | 5 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/reward_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/reward_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/reward_test.stderr.log) |
| rpc_controller_test | FAIL | 0 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/rpc_controller_test.build.log) |
| routing_identity_test | OK | 222 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/routing_identity_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/routing_identity_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/routing_identity_test.stderr.log) |
| skill_test | OK | 76 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/skill_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/skill_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/skill_test.stderr.log) |
| snow_flake_test | OK | 27 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/snow_flake_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/snow_flake_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/snow_flake_test.stderr.log) |
| time_meter_test | OK | 7 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/time_meter_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/time_meter_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/time_meter_test.stderr.log) |
| time_util_test | OK | 6 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/time_util_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/time_util_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/time_util_test.stderr.log) |
| timer_destroy_test | OK | 1 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/timer_destroy_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/timer_destroy_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/timer_destroy_test.stderr.log) |
| timer_queue_unit_test | OK | 54 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/timer_queue_unit_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/timer_queue_unit_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/timer_queue_unit_test.stderr.log) |
| turn_battle_engine_test | OK | 300 | 0 | 0 | 0 | [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/turn_battle_engine_test.build.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/turn_battle_engine_test.stdout.log) / [stderr](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/turn_battle_engine_test.stderr.log) |

## 全部构建错误与测试失败索引

初次全量原始摘要已保全为 [06-cpp-summary-initial.md](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-summary-initial.md)；35个FAIL完整RUN→FAILED帧保存在 [06-cpp-failed-frames.md](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-failed-frames.md) / [JSONL](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-failed-frames.jsonl)。以下列全部失败名、原stdout行号及首个断言样例；所有剩余断言/运行日志均在上述原件，不重复复制。

### bag_test

官方脚本判定：`{"build": "OK", "result": "767/783 过,挂:PlayerEquipAttributeTest.AccuracyAddsPhysicalAttackOnlyWhileDamageAffixAdd…"}`。

`PlayerEquipAttributeTest.AccuracyAddsPhysicalAttackOnlyWhileDamageAffixAddsBoth`：fail，原stdout行4294–4309。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 500 vs 400
attr 2: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.PrimaryPointAffixesUseThePerPointCoefficientOfTheirOwnDimension`：fail，原stdout行4310–4369。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 7 vs 6
attr 4: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.AllPrimaryPointsAddsToEachPlayerDimensionAndStacksWithSinglePoints`：fail，原stdout行4370–4465。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 8 vs 2
attr 7: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.CombatBonusesAreWrittenByNameAndClearedWhenTheGearComesOff`：fail，原stdout行4472–4597。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 5
attr 12: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys`：fail，原stdout行4665–4678。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(1779): error: Expected equality of these values:
  kSuccess
    Which is: 1000
  PlayerAttributeSystem::AutoAllocate(player, poolId, suggested)
    Which is: 25013
```

`PlayerEquipAttributeTest.PanelValueIncludesEquipmentPointsAndBonusReportsTheExternalPart`：fail，原stdout行4717–4841。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 6
attr 3: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.PanelListsAllFifteenCombatStatsSortedWithFinalValues`：fail，原stdout行4848–4903。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 5
attr 12: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.PanelClampsPercentStatsToOneHundred`：fail，原stdout行4904–4959。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 5
attr 12: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.EquipmentBonusIsNeverWrittenToPersistedComponents`：fail，原stdout行4960–5001。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 5
attr 12: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.AllocationGainDoesNotDependOnEquipment`：fail，原stdout行5002–5021。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 6
attr 3: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.ReloadFromTheDatabaseRecordRecomputesTheEquipmentBonus`：fail，原stdout行5022–5080。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 5
attr 12: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.LegacyTopLevelBagReloadRecomputesTheEquipmentBonus`：fail，原stdout行5081–5139。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 5
attr 12: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.EnhancementScalesBaseAttributesOnlyNotAffixesOrGems`：fail，原stdout行5164–5179。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 500 vs 400
attr 2: the test value exceeds the table cap; pick a smaller value
```

`PlayerEquipAttributeTest.PanelBonusAndCombatLinesStayTheSameAfterEnhanceAndInlay`：fail，原stdout行5330–5512。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_equip_attribute_test.cpp(630): error: Expected: (value) <= (cap->cap()), actual: 10 vs 6
attr 3: the test value exceeds the table cap; pick a smaller value
```

`PetAllocationTest.AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys`：fail，原stdout行6234–6242。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\player_feature_handler_test.cpp(1053): error: Expected equality of these values:
  kSuccess
    Which is: 1000
  PetSystem::AutoAllocate(player, kPetId, suggested)
    Which is: 26014
```

`PersistRoundTripTest.UnknownFieldsOnEveryCarriedNodeSurviveLoadAndSave`：fail，原stdout行7626–7632。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\bag_test\persist_round_trip_test.cpp(580): error: Value of: uncovered.empty()
  Actual: false
Expected: true
夹具没有覆盖这些可携带类型(在 BuildEveryPersistedType 里补一个能撑过加载的节点,或在 proto 上按设计 §3.2 标冻结 / 未携带): AssetRecallLine
```

### cross_zone_test

官方脚本判定：`{"build": "OK", "result": "326/344 过,挂:RelocateConfirmEcs.DrainWithoutSceneManagerIsVerifiedThenKicked, RelocateC…"}`。

`RelocateConfirmEcs.DrainWithoutSceneManagerIsVerifiedThenKicked`：fail，原stdout行833–852。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.OnlyUncorrelatedRepliesAreSwallowedWhileVerifyingANotSentRelocate`：fail，原stdout行853–872。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.TransportFailureForANotSentRelocateIsNotClaimed`：fail，原stdout行873–892。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.ReentryOnSameSessionWaitsForTheLoadThenResolves`：fail，原stdout行893–912。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.AbandonedLandingIsVerifiedThenKicked`：fail，原stdout行913–932。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.KickIsWithheldWhenTheEntityIsBackOnThisNode`：fail，原stdout行933–952。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.ReentryOnAnotherSessionSupersedesWithoutKick`：fail，原stdout行953–972。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.InFlightSceneChangeIsRecordedAsAnEarlierRequest`：fail，原stdout行1025–1039。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5649): error: Value of: entry.has_value()
  Actual: false
Expected: true
```

`RelocateConfirmEcs.NextRelocateSupersedesALandingEntryWithoutKick`：fail，原stdout行1052–1071。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5118): error: Expected equality of these values:
  PlayerLifecycleSystem::RelocateConfirmsPending()
    Which is: 0
  1u
    Which is: 1
```

`RelocateConfirmEcs.UnsettledPreviousRelocateIsCarriedIntoTheNextOne`：fail，原stdout行1072–1145。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(5800): error: Expected: (second->seq) != (first->seq), actual: 2 vs 2
```

`EnterFenceEcs.StaleEnterDoesNotSupersedeRelocateEntry`：fail，原stdout行1446–1460。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(7027): error: Value of: entry.has_value()
  Actual: false
Expected: true
```

`ReleaseFenceEcs.EqualEpochReleases`：fail，原stdout行1617–1633。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(7625): error: Expected equality of these values:
  exit_release_stats::Read().decisions[skipCause]
    Which is: 1
  before.decisions[skipCause] + 1
    Which is: 2
退出原因必须是 released_by_transfer(A1′ 按原因跳过)
```

`ReleaseFenceEcs.UnversionedReleaseKeepsLegacy`：fail，原stdout行1634–1650。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(7625): error: Expected equality of these values:
  exit_release_stats::Read().decisions[skipCause]
    Which is: 1
  before.decisions[skipCause] + 1
    Which is: 2
退出原因必须是 released_by_transfer(A1′ 按原因跳过)
```

`ReleaseFenceEcs.CacheUnknownReleasesAndCounts`：fail，原stdout行1651–1668。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(7625): error: Expected equality of these values:
  exit_release_stats::Read().decisions[skipCause]
    Which is: 1
  before.decisions[skipCause] + 1
    Which is: 2
退出原因必须是 released_by_transfer(A1′ 按原因跳过)
```

`ReleaseFenceEcs.SessionEndedReleaseExitsHolderAsClientDisconnect`：fail，原stdout行1682–1698。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(8184): error: Expected equality of these values:
  exitAfter.decisions[write]
    Which is: 2
  exitBefore.decisions[write] + 1
    Which is: 3
按客户端断线退出:A1′ 判定写标记
```

`ReleaseFenceEcs.SessionEndedReleaseExitsCoveredLogin`：fail，原stdout行1721–1737。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(8034): error: Expected equality of these values:
  exit_release_stats::Read().decisions[write]
    Which is: 2
  before.decisions[write] + 1
    Which is: 3
按客户端断线退出:A1′ 判定写标记
```

`ReleaseFenceEcs.SessionEndedReleaseExitsOlderLoginOnLoginCollision`：fail，原stdout行1738–1754。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(8034): error: Expected equality of these values:
  exit_release_stats::Read().decisions[write]
    Which is: 2
  before.decisions[write] + 1
    Which is: 3
按客户端断线退出:A1′ 判定写标记
```

`ReleaseFenceEcs.SessionEndedReleaseWithoutCoveredIdentityKeepsLegacyBehavior`：fail，原stdout行1758–1774。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp(8034): error: Expected equality of these values:
  exit_release_stats::Read().decisions[write]
    Which is: 2
  before.decisions[write] + 1
    Which is: 3
按客户端断线退出:A1′ 判定写标记
```

### kafka_command_test

官方脚本判定：`{"build": "OK", "result": "51/52 过,挂:KafkaCommandWhitelist.SceneAcceptsOnlyItsPublishedIngressEvents"}`。

`KafkaCommandWhitelist.SceneAcceptsOnlyItsPublishedIngressEvents`：fail，原stdout行51–59。

```text
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\kafka_command_test\kafka_command_whitelist_test.cpp(28): error: Expected equality of these values:
  Contains(kSceneEvents, eventId)
    Which is: true
  IsSceneCommandEvent(eventId)
    Which is: false
42
```

### rpc_controller_test

官方脚本判定：`{"build": "FAIL", "result": "D:\\luyuan\\xuanming-server-mmo-cpp\\cpp\\tests\\rpc_controller_test\\grpc_client_local_reject…"}`。

```text
L8: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(35,21): error G1A4676F8: no member named 'LoadPlayerDataRequest' in namespace 'data_service' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L11: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(36,21): error G1A4676F8: no member named 'LoadPlayerDataResponse' in namespace 'data_service' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L14: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(81,9): error G58EA42A4: unknown type name 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L17: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(83,23): error G1A4676F8: no member named 'SendDataServiceLoadPlayerData' in namespace 'data_service'; did you mean 'SendDataServiceGetPlayerRoute'? [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L24: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(85,62): error G58EA42A4: unknown type name 'LoadPlayerDataResponse' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L27: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(139,13): error G58EA42A4: unknown type name 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L30: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(141,27): error G1A4676F8: no member named 'SendDataServiceLoadPlayerData' in namespace 'data_service'; did you mean 'SendDataServiceGetPlayerRoute'? [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L37: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(142,73): error G58EA42A4: unknown type name 'LoadPlayerDataResponse' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L40: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(184,15): error G58EA42A4: unknown type name 'LoadPlayerDataResponse' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L43: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(185,23): error G1A4676F8: no member named 'SendDataServiceLoadPlayerDataErased' in namespace 'data_service'; did you mean 'SendDataServiceGetPlayerRouteErased'? [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L50: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(236,54): error G30564CF7: use of undeclared identifier 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L53: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(240,52): error G30564CF7: use of undeclared identifier 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L56: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(241,38): error G58EA42A4: unknown type name 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L59: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(243,23): error G1A4676F8: no member named 'SendDataServiceLoadPlayerDataErased' in namespace 'data_service'; did you mean 'SendDataServiceGetPlayerRouteErased'? [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L66: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(232,23): error GF6FBF31F: no viable conversion from '(lambda at D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp:232:23)' to 'Functor' (aka 'std::function<void ()>') [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L113: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(288,15): error G58EA42A4: unknown type name 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L116: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(289,23): error G1A4676F8: no member named 'SendDataServiceLoadPlayerData' in namespace 'data_service'; did you mean 'SendDataServiceGetPlayerRoute'? [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L123: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(291,61): error G58EA42A4: unknown type name 'LoadPlayerDataResponse' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L126: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_local_reject_test.cpp(300,23): error G58EA42A4: unknown type name 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L129: EXEC : fatal error : too many errors emitted, stopping now [-ferror-limit=] [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L131:   [no-raw-pointer-member] FAILED for 'rpc_controller_test' (exit 2).
L132: D:\luyuan\xuanming-server-mmo-cpp\Directory.Build.targets(56,5): error MSB3073: 命令“pwsh -NoProfile -ExecutionPolicy Bypass -File "D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\check_no_raw_pointer_member.ps1" -ProjectDir "D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test" -ProjectName "rpc_controller_test" -ProjectFile "D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj" -Configuration "Debug" -Platform "x64" -MSBuildPath "C:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe"”已退出，代码为 2。 [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
```

## 跳过与没跑

- `muduo_grpc_test / GoRouterInteropTest.MissingSessionMetadataIsUnauthenticated`：

```text
[ RUN      ] GoRouterInteropTest.MissingSessionMetadataIsUnauthenticated
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\muduo_grpc_test\go_interop_test.cpp(299): Skipped
MUDUO_GRPC_INTEROP_ROUTER (ip:port of a running Go client_rpc_router) is not set

[  SKIPPED ] GoRouterInteropTest.MissingSessionMetadataIsUnauthenticated (1 ms)
```

- `muduo_grpc_test / GoRouterInteropTest.UnknownMessageIdReturnsTipAndEchoesSessionHeader`：

```text
[ RUN      ] GoRouterInteropTest.UnknownMessageIdReturnsTipAndEchoesSessionHeader
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\muduo_grpc_test\go_interop_test.cpp(299): Skipped
MUDUO_GRPC_INTEROP_ROUTER (ip:port of a running Go client_rpc_router) is not set

[  SKIPPED ] GoRouterInteropTest.UnknownMessageIdReturnsTipAndEchoesSessionHeader (0 ms)
```

- `muduo_grpc_test / GoRouterInteropTest.TwoHundredConcurrentCallsAllSucceed`：

```text
[ RUN      ] GoRouterInteropTest.TwoHundredConcurrentCallsAllSucceed
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\muduo_grpc_test\go_interop_test.cpp(299): Skipped
MUDUO_GRPC_INTEROP_ROUTER (ip:port of a running Go client_rpc_router) is not set

[  SKIPPED ] GoRouterInteropTest.TwoHundredConcurrentCallsAllSucceed (0 ms)
```

- `muduo_grpc_test / GoRouterInteropTest.CallAfterServerIdleTimeoutStillSucceeds`：

```text
[ RUN      ] GoRouterInteropTest.CallAfterServerIdleTimeoutStillSucceeds
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\muduo_grpc_test\go_interop_test.cpp(299): Skipped
MUDUO_GRPC_INTEROP_ROUTER (ip:port of a running Go client_rpc_router) is not set

[  SKIPPED ] GoRouterInteropTest.CallAfterServerIdleTimeoutStillSucceeds (0 ms)
```

- `muduo_grpc_test / ServerInterop.ServesGrpcGoClientUntilShutdown`：

```text
[ RUN      ] ServerInterop.ServesGrpcGoClientUntilShutdown
D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\muduo_grpc_test\server_interop_test.cpp(155): Skipped
set MUDUO_GRPC_SERVER_INTEROP_PORT=<port> to run the grpc-go server interop test

[  SKIPPED ] ServerInterop.ServesGrpcGoClientUntilShutdown (0 ms)
```

- `rpc_controller_test`：没有本轮stdout运行证据，没跑。

## 重点suite与两版真实Redis

`AssetOpRecallTest`真实终态：`{"pass": 15}`；已随currency全集运行，不再定向重跑Recall。所有suite逐名终态见JSON。

redis72五个真实后端用例：`{"pass": 5}`。

- `CurrencyTest.TravelPublishMarkDirectAgainstRealRedis`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/currency_test.stdout.log)。
- `CurrencyTest.HandoffQualificationCASRevokesLateWritersAndReplaysLostRepliesAgainstRealRedis`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/currency_test.stdout.log)。
- `CurrencyTest.HandoffMarkFramesDelayedOnReplacedConnectionCannotReviveCredentialAgainstRealRedis`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/currency_test.stdout.log)。
- `BattleLockContract.RenewalOnlyExtendsKeepsCtxInStepAndNeverCreates`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.stdout.log)。
- `BattleLockContract.WriteLandsLockAndCtxTogetherAndResendIsIdempotent`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.stdout.log)。

redis8五个真实后端用例：`{"pass": 5}`。

- `CurrencyTest.TravelPublishMarkDirectAgainstRealRedis`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/currency_test.stdout.log)。
- `CurrencyTest.HandoffQualificationCASRevokesLateWritersAndReplaysLostRepliesAgainstRealRedis`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/currency_test.stdout.log)。
- `CurrencyTest.HandoffMarkFramesDelayedOnReplacedConnectionCannotReviveCredentialAgainstRealRedis`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/currency_test.stdout.log)。
- `BattleLockContract.RenewalOnlyExtendsKeepsCtxInStepAndNeverCreates`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/bag_test.stdout.log)。
- `BattleLockContract.WriteLandsLockAndCtxTogetherAndResendIsIdempotent`：pass，[原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/bag_test.stdout.log)。

定向命令 `06-currency_test-redis8`：UTC `2026-11-02T10:12:22.1639759+00:00` → `2026-11-02T10:12:22.8146122+00:00`，exit=0；[完整记录](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-currency_test-redis8.log)。

定向命令 `06-bag_test-redis8`：UTC `2026-11-02T10:12:22.8169249+00:00` → `2026-11-02T10:12:23.2740348+00:00`，exit=0；[完整记录](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-bag_test-redis8.log)。

Redis8 `currency_test`原生进程exit=0，UTC `2026-11-02T10:12:22.3936041+00:00` → `2026-11-02T10:12:22.7209751+00:00`，timeout=False；[XML](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/currency_test.xml) / [native结果JSON](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/currency_test.result.json)。

Redis8 `bag_test`原生进程exit=0，UTC `2026-11-02T10:12:23.0377459+00:00` → `2026-11-02T10:12:23.2027086+00:00`，timeout=False；[XML](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/bag_test.xml) / [native结果JSON](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-cpp-tests-redis8/bag_test.result.json)。

## 获批RPC符号适配与独立构建的最终结果

初次29工程中RPC构建失败仍保留；本节仅追加独立修复后的完成命令，不覆盖原35FAIL或初始总数。

获用户明确批准的local_reject符号替换静态核对：整文件只做4类符号替换，51条ASSERT/EXPECT归一化类型名后逐字相同；240/241两条ASSERT仅类型名变化，比较条件/期望不变。[逐字核对证据](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-symbol-assert-audit.json)。这不是运行通过证据。

批准的local_reject纯符号适配已经执行；独立构建exit1（MSBuild报告0警告、21错误），停在该次构建时尚未适配的grpc_client_loopback_test.cpp退役LoadPlayerData类型/GrpcPath/Send引用及派生错误，并达到Clang错误上限。该loopback不在本次已确认的diff内；后续源码审阅还发现旧response.set_version调用，不能只沿用local_reject替换结论。set_version属于源码审阅事实，不冒充错误上限之前已报出的诊断。

同组RPC源码已由另一个全球同服验收聊天接手，本轮执行代理及辅助代理没有并发修改该loopback，也没有重试这次失败构建。独立构建未达到成功门禁，首次全量RPC EXE仍没跑；没有运行旧EXE，也没有重跑原35个失败case。构建错误不能计成实际测试运行，剩余编译问题交后续处理。

新错误全文索引：[06-rpc-symbol-fix-build-errors.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-symbol-fix-build-errors.txt)（22条索引包含额外门禁FAILED行，不等于22个MSBuild错误）；完整上下文与重复汇总保留在下面命令原件。

`06-rpc-build-after-symbol-fix`：UTC `2026-11-02T11:31:52.0174911+00:00` → `2026-11-02T11:32:12.8575801+00:00`，exit=1；[命令原件](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/06-rpc-build-after-symbol-fix.log)；实际gtest终态：`{}`。

该次全部已报告编译错误（MSBuild重复汇总去重，完整上下文保留原件；错误上限后隐藏部分不伪称已覆盖）：

```text
L19: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(59,21): error G1A4676F8: no member named 'LoadPlayerDataRequest' in namespace 'data_service' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L22: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(60,21): error G1A4676F8: no member named 'LoadPlayerDataResponse' in namespace 'data_service' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L25: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(61,42): error G30564CF7: use of undeclared identifier 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L28: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(123,33): error G30564CF7: use of undeclared identifier 'LoadPlayerDataCall' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L31: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(115,50): error G1A4676F8: no member named 'kDataServiceLoadPlayerDataGrpcPath' in namespace 'data_service'; did you mean 'kDataServiceGetPlayerRouteGrpcPath'? [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L38: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(130,27): error G30564CF7: use of undeclared identifier 'LoadPlayerDataCall' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L41: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(132,26): error G58EA42A4: unknown type name 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L44: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(132,58): error G58EA42A4: unknown type name 'LoadPlayerDataResponse' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L47: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(237,13): error G58EA42A4: unknown type name 'LoadPlayerDataRequest' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L50: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(241,27): error G1A4676F8: no member named 'SendDataServiceLoadPlayerData' in namespace 'data_service'; did you mean 'SendDataServiceGetPlayerRoute'? [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L57: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(243,66): error G58EA42A4: unknown type name 'LoadPlayerDataResponse' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L60: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(272,5): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_PerCallDoneSucceedsExactlyOnce_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L63: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(273,17): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_PerCallDoneSucceedsExactlyOnce_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L66: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(275,5): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_PerCallDoneSucceedsExactlyOnce_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L69: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(294,5): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_ServerDeferringTheCallYieldsDeadlineExceededExactlyOnce_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L72: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(295,17): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_ServerDeferringTheCallYieldsDeadlineExceededExactlyOnce_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L75: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(299,17): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_ServerDeferringTheCallYieldsDeadlineExceededExactlyOnce_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L78: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(301,5): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_ServerDeferringTheCallYieldsDeadlineExceededExactlyOnce_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L81: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_loopback_test.cpp(316,5): error G3E3E7BDF: cannot initialize object parameter of type '(anonymous namespace)::GrpcClientLoopbackTest' with an expression of type '(anonymous namespace)::GrpcClientLoopbackTest_ServerShutdownCompletesStartedCallWithUnavailable_Test' [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L84: EXEC : fatal error : too many errors emitted, stopping now [-ferror-limit=] [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
L86:   [no-raw-pointer-member] FAILED for 'rpc_controller_test' (exit 2).
L87: D:\luyuan\xuanming-server-mmo-cpp\Directory.Build.targets(56,5): error MSB3073: 命令“pwsh -NoProfile -ExecutionPolicy Bypass -File "D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\check_no_raw_pointer_member.ps1" -ProjectDir "D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test" -ProjectName "rpc_controller_test" -ProjectFile "D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj" -Configuration "Debug" -Platform "x64" -MSBuildPath "C:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe"”已退出，代码为 2。 [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj]
```

SKIP/disabled/无RUN均按没跑；原程序退出码以完成命令或定向result.json为准。
统一脚本未输出每个子EXE的结构化exit/UTC，保留其整体完成记录、逐工程stdout/stderr及表格判定，不补造子退出码。

## 本轮结束核对

- main HEAD 仍为 ff8d37d9f73eab7762e6195da93bc9420848294e；没有提交、切分支、stash/reset/checkout。两份本轮测试源码修改各自有独立 diff，其他会话的既有及新增修改均保留。
- 第二轮 codex-results.md SHA256 仍为 AE20C93D7711FF271CC1F9B0CFB930F361FB2E53151D8F716FE8456DC013A422，没有覆盖第二轮回执。
- 仍没跑：独立 NoRaw checker 合同（缺工具）；RPC 全部运行时用例（修后工程仍编译失败，包括 battle observer/Hiredis/RedisClusterClient）；5 个需要外部 Go 互通环境的 muduo_grpc 用例。完整双区正例、§11 压测及原第二轮其它整套项目本轮未重跑。
- 本轮实际执行完毕，不表示全绿。清理 Problems=1、35 个 C++ 运行失败、RPC 新旧编译失败、两版 Refresh 失败、48 个脚本合同 FAIL 和 docs-index 基线错误全部保留，不重试或改期望掩盖。
- 已确认的 RPC 符号适配及单独编译验证完成，没有待用户批准的本轮修改；loopback 同族旧接口和其返回字段适配留给接手改码会话另轮验证。本轮没有继续并发修改这些文件。
- 11:37Z 只读核对：本轮所属 MySQL、TiKV、TiDB、Redis 7.2/8.x、Kafka 均 running、OOMKilled=false，TiKV内存6GiB、Kafka2GiB；环境按准备目标保留运行。原件见 [env-final-r3-state.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261102-r3-prep/env-final-r3-state.log)。

状态:本轮跑完
