# 任务 FIX-GO：按评审修正 Go match 与 login 的观战 / 推送问题

先读同目录 `00-common-rules.md`。评审原文（含位置、触发序列、修法）：`reports/review-match.md`（S1–S4、G1–G8）、`reports/review-c-batch.md`（严重-1、严重-2）。动手前先核实每条仍成立，不成立的不改并说明。「修本地模式观战」会话改过 `spectate.go`、`watchbattlelogic.go` 的标记删除语义（只有拿到取消证明或 Add 被明确拒绝才按值删除），保持这条规则，不新增无条件删标记的调用点。

## 允许改的文件
`go/match/internal/logic/` 下观战文件与 `global_match.go`（只在 `CommitBattleOutcome` 加一处尽力清理）、`matcher.go`（只加全局分支的低频清理循环）；`go/match/internal/authority/spectator.go` 及其 native 测试；`go/match/internal/metrics/metrics.go`（只追加）；`go/match/cmd/spectatefeatured/main.go`；`go/login/internal/svc/gate_command.go`、`go/login/internal/logic/clientplayerlogin/entergamelogic.go` 及其测试。

## 要修（主会话已拍板，按此实现）
1. **S1 键 TTL**：全局模式下观战键（档位、中继 gate HASH、赛事局标记与索引）TTL 与过期分界按最长战斗期限算（`catalog.MaxBattleSeconds + WorkLeaseMs/1000 + 60` 或评审确认的等价值），不再是 `BattleMaxDurationSeconds + 60`。和 S2、S3 一起落。
2. **S2 结束释放配额**：`CommitBattleOutcome` 拿到 decision 后尽力调一次 `removeSpectateBattle`（幂等，失败只记日志，不影响终局回执）。确认它同时摘活跃集合、L2 配额、赛事局索引，并清掉档位、中继 HASH 等本场键（有 TTL 兜底，但结束即清更干净）。
3. **S3 全局定时清理**：全局分支加低频循环调 `cleanupExpiredSpectateIndex`（照 `match.global.expired.audit` 的写法），赛事局索引的过期成员也要能回收；`resumeGlobalSpectator` 的 `roomMissing` 分支摘一次索引。
4. **S4 / C 批严重-2 原实例已不存在时关闭观战原件**：负责人授权放宽「只有正证明才关闭」。拿不到取消证明时，满足任一条件就不带证明推进到 state 5：
   (a) 成局 `state == 5`；(b) 已过成局 `deadline_ms` + `terminalOutcomeGraceMs`（房间必已被定时器收尾）；(c) 注册表里同 node_id 已是别的 uuid（`BattleNodes.EntryOfNode`；查不到不算）。
   理由写进注释：观众对战斗状态零写权，Add 带 `target_instance_id`，新实例不接纳旧 op，放宽不碰资产，最坏是僵尸实例多发几帧。排队路径（`closeCapturedSpectatorExit` 一类）与观战路径共用同一判定。补用例：Remove 回空应答且满足 (a) / (b) / (c) 时关闭；都不满足时不关。
5. **G1 配额账**：Attach 回 `broadcast=true` 的同步分支补一次 ZADD（score = 期限，超额也记）；新占者 Promote 失败退还前先复读档位，已是 BROADCAST 就不退。
6. **G2**：Go 侧调换顺序：中继可用时先写 SQL 结论（`RejectSpectatorToLobby`），升档失败只记日志（重放会补升档）。battle 侧「同 op 重放回首次拒绝码」由主会话单独改，夹具 `fakeBattleNode.AddObserver` 按真实墓碑语义改（第二次同 op 回首次拒绝码），覆盖这条。
7. **G3**：`spectatePromote` 把 battle 的拒绝码返回给调用方；房间不存在按现有懒剔除口径处理（`removeSpectateBattle`，拒绝这名观众），不论配额是否本次新占。
8. **G4**：签凭证前查内存注册表：`BattleNodes.EntryOfNode` 成功且 `NodeUuid` 与来源实例不同，按房间不存在处理；查不到不判。不增加 RPC。
9. **G5**：升档 RPC 前加短租约（`SET spectate:promote_try:{id} NX PX 3000`），拿到租约的观众才发 RPC，其余按 RELAY 签票。
10. **G6**：`SetSpectateFeatured` 未开启或验签失败改回 `SPECTATE_FEATURED_REJECT_DENIED`（proto 已加，Codex 跑 proto-gen 后生成），不回 gRPC PermissionDenied；运营工具把它打印成明确错误并返回非零。
11. **G7 意图保留期**：state=5、不是请求头、关闭超过 7 天的 `match_spectator_intent` 行由低频任务分批删除（每轮有上限，幂等，多副本同时跑无害）。写清依据：同 request_id 重放窗口是秒级、战斗最长约 1 小时。若表上没有可用的时间列，用现有字段或加一列（项目未上线，schema 直接改，走 proto 驱动迁移），保持最小。
12. **G8 测试**：补战斗期限大于旧 TTL 的用例（S1）、结束释放配额（S2）、全局清理（S3）、配额账（G1）；加一条凭证跨语言固定向量：固定 secret 与 payload 字节算出的 hex，写成 Go 测试常量并在交付说明里给出，C++ `gate_spectate_ticket_test.cpp` 由主会话补同一条。
13. **C 批严重-1 Bind 之前的进场失败提示**：login 记住本链是否已发 Bind。Bind 之前的失败走单独的 builder（命名体现「未绑定会话」），`PushToPlayerEvent.player_id` 留 0（gate 判兼容放行），信封 `PlayerId` 照填；Bind 之后仍带真实 player_id。这是「生产者拒发 0」的唯一例外，注释写清：内容是固定 tip、不含私有数据，`target_instance_id` 已挡住 node_id 复用。补回归：Bind 前后 player_id 的差别。

交付说明写到 `docs/handoff/spectate-tiered-fanout-20261030/reports/fix-go.md`（含 Codex 命令与每条的处理结果），最终回复三五行摘要。
