# 交接：观战分级扩容（B / C / P0 剩余已落码；评审修正与 P1 / P2 待做，2026-10-30）

设计与契约：[battle-spectate-tiered-fanout.md](../../design/battle-spectate-tiered-fanout.md)（§10.1 A 批进展、§12 / §13 决策、**§15 P1 / P2 落码契约**）。
负责人 2026-10-30 授权：项目未上线、玩家数据可清空；服务端按标准做法全部做完，客户端代码、协议生成、编译、冒烟交给 Codex。
本目录 `specs/` 是派给落码代理的任务说明（`00-common-rules.md` 是共用硬约束），`reports/` 是各代理的交付说明（含给 Codex 的命令）。
specs 里引用的 `..\wf\scout_*.md` 勘查报告已随临时目录丢失，代理都是直接按代码落码的，不影响使用。

**给 Codex 的执行清单见 [CODEX.md](CODEX.md)**（取代下面第 3 节）。本地模式观战标记的规则见 `specs/85-local-mark-consistency.md`。

## 1. 已落码（都没编译、没跑测试）

| 批 | 内容 | 交付说明 |
|---|---|---|
| A（10-24） | 满员专用码 16021、掉线占座回收、全局观战错误码与指标；两版 | PROGRESS「观战缺陷 A 批」条目；Codex 已跑 Go match 100 项闭合 |
| B | 可见性闸：`eSpectateVisibility` 建房冻结；match 四处执法；battle 第二道闸。策略：只有非活动局的 PVE_SOLO / PVE_TEAM 公开，1V1 / 5V5 计分局（匹配设计 D7）、切磋、帮会历练、PVP_TEAM、未知模式都私密 | `reports/go-match-b.md`、`reports/cpp-battle.md` |
| P0 剩余 | 观众帧编码一次；观众席节点配额（各房间名单和 ≥ max(20, 连接上限/4) 回 16021） | `reports/cpp-battle.md` |
| C | gate 推送身份栅栏（`PushToPlayerEvent.player_id` / `BroadcastToPlayersEvent.player_list`）、共享投递模块 `gate_push`、Kafka 场景 / 全服广播 handler 补全、广播编码一次、生成器对必须带 payload 的事件拒绝空 payload；Go 全部生产者填目标玩家；跨语言对拍向量 | `reports/cpp-gate.md`、`reports/go-push.md` |
| Java | B + 本地模式原操作 + P0 剩余（代理在 5 小时额度见底时仍在跑；先看 Java 仓 `PARITY.md` 末尾与 `git -C D:\luyuan\xuanming-server-mmo status` 确认做到哪） | `reports/java.md`（若存在） |
| proto | P1 / P2 全部协议已由主会话改好（§15.2）：battle_node 两条新 RPC、`SpectateFrameEvent`（观战帧 topic 的消息体本身，不套 GateCommand）、`eSpectateTier`、`eSpectateDelivery`、凭证 `gate_spectate_ticket.proto`、gate 本地客户端服务 `gate_client_spectate.proto`、`spectate_token_secret`（config 字段 24）、`MatchSpectatorIntentRecord.delivery = 10`、`MatchInternal.SetSpectateFeatured`、`SPECTATE_END_SOURCE_LOST`；Tip.xlsx 新增 `MatchSpectateTicketInvalid`（第 269 行）；topic 命名工具 `node_command_topic.h`（`kSpectateRelayTopicBase` 等） | 本文件 |

## 2. 下一步（按顺序；额度允许时每波 ≤ 4 个代理）

1. **B / C / P0 评审修正**（主会话已定的修正点，评审代理再找其它问题）：
   - gate `gate_push`：单播、场景、全服广播缺 `message_content` 一律拒绝并计数（现在只有指名广播检查）。
   - 生成器 `PayloadRequired` 判据改为「事件里有任何信封装不下的字段」（含 string / bytes，如 `RedirectToGateEvent`），不只看 message / repeated。
   - battle：「连接上限 0 = 硬上限」的规则在 `battle_client_edge` 与 `BattleRoomManager` 各写了一份，改为管理器从直连面取有效上限（DRY）。
   - 文档：`docs/design/broadcast-message-size-analysis.md:92` 的 `EncodeBitmapFields` 已删，改写为 `EncodeBroadcastTargets`；`routing-identity-audit-20260908.md` 的 R13 补记 Kafka 推送栅栏已落；`client_session_handler.cpp:856` 引用的 `gate_service_handler.cpp:143` 行号过时。
2. **P1 / P2 落码**：派 `specs/60-p1-battle.md`、`70-p1-gate.md`、`80-p1-match.md`、`90-p1-deploy.md` 四路（文件互不重叠；先 ListAgents，并通知「全服匹配方案讨论」会话：P1 会改 match 的 `config.go`、`servicecontext.go`（只加装配）、`matchinternalserver.go`）。
3. **P1 / P2 评审修正** → **Java P1 / P2**（照 §15.7：内部通道走 Redis pub/sub，客户端契约同 §15.2）→ PARITY 登记。
4. 每批收尾：`docs/PROGRESS.md` 只追加；设计文档 §10.1 / §15.8 更新状态。

## 3. 给 Codex 的总顺序（各批细节见 `reports/*.md`）

1. 导表（Tip.xlsx 新增 `MatchSpectateTicketInvalid`；`MatchSpectateFull` 已在 10-24 发号 16021）：复制出 `exporter_config.local.yaml` 并把 `csharp.deploy` 设为 `[]`，`py -3 tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.local.yaml`，跑完删掉 local 配置。
2. 生成器源码变了（`gate_kafka_command_router.go` / `.tmpl`）：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`，再 `... -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。与其他会话的未提交 proto 协调后统一跑一次。
3. 生成器 `go test`（`tools/proto_generator/protogen`）；Go：`go/shared` 的 `go test ./kafkautil/...`，`go/match`、`go/friend`、`go/guild`、`go/login`、`go/mail` 各 `go build ./... && go vet ./... && go test ./... -count=1`；match 的 TiDB native 测试与 `match -migrate`（`close_tip` / `delivery` 列）。
4. C++（MSBuild 一律 `/m:1`）：proto → table → gate → battle → `kafka_command_test`、`rpc_controller_test` 等测试工程，运行 `BattleObserverContract.*`、`BroadcastTargetVectors.*`、`BroadcastTargetCodec*` 等。
5. Java：`java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp --check`（不一致先协调再跑写入模式）→ `./mvnw -B clean install -DskipTests` → `-pl game-battle -am test`、`-pl game-match -am test`。

## 4. 与其他会话的约定

- 「全服匹配方案讨论」：`gather.go` 归它；B 批两处可见性改动已与其第二批共存（第 355、404 行）。它不再写 Tip.xlsx。P1 开工前通知它。
- 「全球经济系统代码审核」：只改了 `battle_room_manager.cpp` 的 `PumpSqlOutcomes`；P1 改该文件前重读。
- 「Turn battle gap closure handoff」：不碰 battle 节点观战文件。

## 5. 进度更新（2026-10-30 第二波，主会话随时可能压缩上下文，以本节为准）

**已落码（未编译）**：C++ / Go 的 B、C、P0 剩余、P1 / P2（`reports/p1-battle.md`、`p1-gate.md`、`p1-match.md`、`p1-deploy.md`）；Java 的 B、P1 / P2（`reports/java.md`、`java-p1.md`）；本地模式标记一致性（`specs/85-local-mark-consistency.md`，子会话已收尾）。Codex 10-30 03:18 跑过一次导表与 proto-gen，之后 proto 又改过（`SPECTATE_FEATURED_REJECT_DENIED`、注释），要再跑一次。
主会话已直接改：`config.cpp` 读 `SpectateTokenSecret`；`k8s_client_entry_contract.tests.ps1` 补新密钥；`gate-spectate` 保留期降到 10 分钟（compose、K8s、脚本、契约测试）；gate hub 拆两条水位（`lastTurnRound` / `cachedStateRound`）并补 3 条用例；`match_service.yaml` 日志忽略表加 `SetSpectateFeatured`；runbook 与 topic 保留策略文档；§6.1 / §9 / §15.4 / §15.5 / §15.7。

**评审**：`reports/review-cpp-relay.md`、`review-c-batch.md`、`review-match.md` 已回；修正任务见 `specs/96-fix-cpp.md`、`97-fix-go.md`。

**在跑（交回后读对应 reports）**：FIX-CPP（`reports/fix-cpp.md`）、FIX-GO（`reports/fix-go.md`）、Java 语义评审（`reports/review-java.md`）。

**这三路交回后主会话要做**：
1. battle「同 op 重放回首次拒绝码」（review-match G2 battle 侧）：`battle_room_manager.{h,cpp}` 的 `SpectatorOperation` 记首次拒绝码，同 op、同请求重放时回它（没有记录时维持 `kInvalidParameter`）；补用例；Java 同语义（放进 Java 修正）。等 FIX-CPP 交回再改，它在动这个文件。
2. 凭证跨语言固定向量：Java 在 `SpectateTicketsTest` 有一组，Go（FIX-GO，见 `reports/fix-go.md`）与 C++ `gate_spectate_ticket_test.cpp` 用同一组 secret + payload + 期望 hex（三边统一）。
3. Java 修正：按 `review-java.md` 派修正代理；并入：`GAME_MATCH_SPECTATE_RELAY_ENABLED` 默认打开、battle 同 op 重放回首次拒绝码、凭证向量。
4. 收尾：`reports/cpp-gate.md` 顶部注明 §22 / §29 / §33 已被 `p1-gate.md` 取代（生成器 9 个事件都拒空 payload）；设计文档 §15.8 状态、S4 放宽与 G7 保留期的决策写进 §15；`docs/PROGRESS.md` 追加收尾条目；更新本文件第 3 节 Codex 总顺序（再跑 proto-gen-build + proto-gen-run，因为生成器与 proto 都改了；另加 FIX 批的测试）；告诉用户可以关窗口。
