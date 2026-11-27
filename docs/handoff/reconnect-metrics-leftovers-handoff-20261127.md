# 断线重连与 C++ 指标导出：换电脑后要做完的事（2026-11-27）

> 一次性交接单。新电脑上负责人说「继续做完 handoff」，就按本文从上到下做。全部做完后**直接删除本文件**，再按 §6 收尾。
>
> 范围只有一条线：会话「接着做完服务器剩下的代码（缺口清单续）」，以及它开出的两个子会话（「go/db 删除存档库运行期冷却」「C++ 只打日志的业务计数接进节点指标出口」）。别的线的待办看 `docs/handoff/README.md` 与 `docs/handoff/new-computer-start-here-handoff-20261127.md`，这里不重复。

## 0. 现状

- 代码和文档全部写完，已提交并推送到 origin/main（截至 `03ce3b652`）。**全部没编译、没跑测试。**
- 剩下三件事：§2 子模块里一个只在旧电脑上的提交，§3 交 Gemini 验证，§4 验证红了由 Claude 修。

## 1. 规矩（新电脑没有旧电脑的 Claude 记忆，照这几条做）

- 只在 main 上改，不开分支。项目没上线，玩家数据可以清空。
- Claude 只写代码和文档；编译、测试、协议生成、冒烟都交 Gemini（AGENTS §10.1）。
- Gemini 跑红了：保留第一条编译错误或失败用例的原文，不改断言、不连续重试，交回 Claude 修；修完再让 Gemini 复跑那一项。
- C++ 测试工程链接预编好的库：改了库要先 `msbuild` 整个解决方案，`run_cpp_tests -Build` 不会重编库。
- Linux 的 CMakeLists 由 `.vcxproj` 生成（`build_linux.sh`）；新增 .cpp 只登记 vcxproj 和 .filters。
- `tools/scripts/tests/*.tests.ps1` 这类文本契约测试钉着指标名、告警名和 C++ 成员名，改名要两边一起改。
- 交付 C++ 前跑两项只读预检：`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`、`python tools/scripts/tests/test_cpp_async_callback_capture.py`（11-27 在旧电脑上都通过）。
- Java 只在 Java 仓 `PARITY.md` 登记待做。这条线没有新增 Java 待办。

## 2. 先做：子模块 ue5navmesh 的本机提交

旧电脑上 `third_party/ue5navmesh` 停在一个只在本机的提交 `4c6b821`（2026-11-11「保存导航网格构建兼容性改动」）。它没推到子模块远端 `https://github.com/lhl-cpp/ue5navmesh.git`，主仓记的子模块指针仍是 `aacf2f8`。

1. 新电脑上先查：`git -C third_party/ue5navmesh fetch` 后跑 `git -C third_party/ue5navmesh branch -r --contains 4c6b821`。
   - 有输出：旧电脑回收前已经推上去了。再看主仓的子模块指针是否已指向它；指着就跳过本节。
2. 没有输出，就照下面重做那个提交的全部内容：
   - `Navmesh.vcxproj`：四个 `ItemDefinitionGroup`（Debug|Win32、Release|Win32、Debug|x64、Release|x64）的 `<ClCompile>` 里，第一行加 `<AdditionalOptions>/utf-8 %(AdditionalOptions)</AdditionalOptions>`。作用是让 MSVC 按 UTF-8 读源码，是 AGENTS 的构建规定。
   - `Private/Detour/DetourNavMeshBuilder.cpp`：在 `#include "Detour/DetourCommon.h"` 之后加 `#include <cassert>`。
3. 在子模块里提交、推送，再在主仓提交新的子模块指针。推送前先问负责人。
4. 让 Gemini 单独编一次 Navmesh 工程。

## 3. Gemini 验证

按顺序跑。每项的完整用例名单和预期，在括号里的 `docs/PROGRESS.md` 条目「验证」段（按标题搜）。

### 3.1 先核对代码在

`git pull` 后，`git log --oneline -1` 应是 `03ce3b652` 或更新。再抽查：
- `go/shared/authoritysql/tx_outcome.go` 有 `func IsRetryableTxError`；
- `go/shared/noderegistry/registry.go` 有 `func WaitForListening`；
- `cpp/libs/engine/core/network/rpc_channel_stats.cpp` 有 `WriteRpcChannelMetrics`；
- `cpp/libs/engine/core/node/system/node/node.cpp` 有 `WriteKafkaProducerMetrics` 和 `Register("dependency_link"`；
- `cpp/libs/engine/config/config.cpp` 的 `readGameConfig` 开头有 `gameConfig.Clear();`。

### 3.2 C++（串行）

1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`，0 个错误。
2. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test`，全绿。新用例：
   - `DependencyLinkMetrics.*` 三条（「断线重连 R17」）；
   - `RedisClusterTopologyTest.RefreshPolicy*` 三条（「断线重连 R19」）；
   - `RpcLinkHeartbeatStats.PrometheusExportCarriesEveryOutcomeAndCounter`（2026-11-27「断线重连 R17 续」）。
3. `-Filter configuration_table_test`，全绿。含 `GameConfigControlRedis.ReadGameConfigTwiceDoesNotAccumulateSeeds` 与 `ReadBaseDeployConfigTwiceDoesNotAccumulateRepeatedFields`（2026-11-26「断线重连 R3 / R6 / R17 / R19 复核收口」）。
4. 子会话三条各自列的工程：`bag_test`、`currency_test`、`cross_zone_test`、`routing_identity_test`、`muduo_grpc`（2026-11-27「C++ 节点只打日志的业务计数接进指标出口」「C++ 指标导出第二轮」「C++ 指标导出第三批」）。
5. Linux：`build_linux.sh` 重新生成 CMakeLists 后编过。

### 3.3 Go（各模块之间可以并行）

逐条跑下面这些 PROGRESS 条目的「验证」段：
- 2026-11-19「断线重连 R3 收尾」：go/guild、go/player_locator、go/login 的 node 包；go/shared 的 noderegistry，带 etcd 再跑 `-tags=integration`。
- 2026-11-19「断线重连 R6 残余」：go/shared（authoritysql、assetop）、go/trade、go/player_inbox、go/data_service、go/guild、go/mail、go/rank。
- 2026-11-19「断线重连 R19」：go/shared/noderegistry（`TestRegisterAfterListening_WaitsForThePort`）、go/scene_manager、go/client_rpc_router。
- 2026-11-19「go/db 删除存档库运行期冷却」：go/db 的 proto_sql 与 metrics，外加 `tools/scripts/test_stress_summarize.ps1`。
- 2026-11-27「C++ 指标导出第三批」：`cd go/player_inbox; go test ./internal/metrics/ -run TestAlertRulesReferenceRegisteredMetrics -count=1`。

真库用例（TiDB / MySQL）没有 DSN 会 SKIP。SKIP 不算通过，要按平时的夹具另跑。

### 3.4 契约测试与告警语法（pwsh）

- `tools/scripts/tests/noderegistry_alerts_contract.tests.ps1`：`total=4 pass=4`。
- `tools/scripts/tests/cpp_node_dependency_alerts_contract.tests.ps1`：`total=4 pass=4`。它核对 5 条规则：依赖连接 2 条，`cpp-node.calls` 组 3 条。
- 子会话三条里列的契约测试：`battle_settlement_alerts_contract`、`persist_version_skew_alerts_contract`、`player_inbox_alerts_contract`、`k8s_deploy_contract`（「LogQL 契约」）、`assetop_work_shard_alerts_contract`、`loop_liveness_alerts_contract`。
- 改过的告警文件，把 `spec` 抽成普通规则文件后跑 `promtool check rules`。

### 3.5 冒烟（调试栈，设 `MMORPG_METRICS_PORT`）

- gate、scene、battle 各起一个，`curl -s http://127.0.0.1:<端口>/metrics`：
  - `cpp_node_dependency_*`：scene 的 redis_player、redis_control、etcd、grpc、node_link 连接数都 ≥ 1；
  - `cpp_node_rpc_client_calls_total` 九个 outcome 都有序列；玩家登录走一遍后，`outcome="ok"` 在涨；
  - `cpp_node_kafka_producer_failures_total` 两个 reason 都以 0 出现；
  - 子会话三条各自列的 grep（`scene_battle_*`、`scene_player_inbox_*` 等）。
- etcd 里 guild、player_locator、login 的注册键都在，login 的 node_id 是 1（「断线重连 R3 收尾」第 6 步）。
- 断开与恢复的计数照「断线重连 R17」第 4 步核对（停 Redis 30 秒再起）。

## 4. 验证红了怎么办

交回 Claude，给它失败那项的 PROGRESS 条目标题和第一条错误原文。Claude 修完在 PROGRESS 追加一条，再交 Gemini 复跑那一项。

全部通过，才算这份交接做完。剩下的红项负责人同意另开任务，也算做完。

## 5. 已经定案，不要再做或改回

- 两项不导出指标：Kafka 自身的连接状态、节点链路的 TCP 建连失败。理由在 `docs/design/infra-reconnect-overview.md` §1.8。
- 节点间 RPC 失败比例告警：5% / 10 分钟是起步值，有压测数据再调。分子不算 remote_rejected（含热关停的人为拒绝）与 channel_closed（本端关机）。
- C++ 告警里剩下的两条 LogQL 是定案保留：
  - `battle_sql_outcome_unflushed_at_shutdown`；
  - `data-service-audit-alerts.yaml` 里快照发送失败的定位查询。
- go/db 的存档库运行期冷却已删，不要接回；打开失败时的冷却保留。
- C++ 的 `Etcd.Hosts` 必须恰好一个负载均衡地址，客户端不轮换多个成员地址。
- 以后新加依赖指标、etcd 注册、事务重试、启动期核对、告警，照 `infra-reconnect-overview.md` §1.11 做，不要另抄一份。

## 6. 做完以后

1. 删除本文件。
2. 跑 `python tools/scripts/gen_docs_index.py --check`；红了再跑 `python tools/scripts/gen_docs_index.py`，diff 只应删掉本文件那一行。
3. `docs/PROGRESS.md` 末尾追加一条验证结论：哪些通过，证据在哪。
