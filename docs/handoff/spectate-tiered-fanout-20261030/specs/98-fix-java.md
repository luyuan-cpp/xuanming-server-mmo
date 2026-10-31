# 任务 FIX-JAVA：按评审修正 Java 版观战

在 Java 仓 `D:\luyuan\xuanming-server-mmo` 落码（`D:\luyuan\xuanming-server-mmo-cpp` 只读参考）。先读 `00-common-rules.md` 与 Java 仓 `AGENTS.md`；评审原文 `reports/review-java.md`（含位置、触发序列、修法），定稿决策见设计文档 `docs/design/battle-spectate-tiered-fanout.md` §15.9。动手前先核实每条仍成立，不成立的不改并说明。不跑 mvn / mvnw / ContractSync；只做外科式 Edit；工作区可能混有其他会话的改动。

## 要修（主会话已拍板，全部按评审的最小修法实现）
1. **严重-1 按场串行发布**：房间持有 `CompletionStage` 尾巴（只在逻辑线程读写，随房间回收），上一帧完成（失败也算完成）才发下一帧，保证 TURN 不被 END 越过。
2. **严重-2 单 gate 约 250 个观战频道上限**：`RedisProperties` 加 `subscriptionsPerConnection`，gate 配到与目标容量匹配的值（评审建议 500），写进配置。
3. **严重-3 实例换掉后观战原件关不掉**：`settled` 拿不到证明时，在同一笔 P 锁事务里满足任一条件就 closed 并置 5：成局已终态；已过成局期限 + 宽限；目录里同 node_id 已是别的实例（查不到不算）。口径同 mmorpg `specs/97-fix-go.md` 第 4 条。
4. **一般-1 L0 满员转 LOBBY 一笔事务 + battle 同 op 重放回首次拒绝码**：battle `ObserverOperationLedger` 记首次拒绝码，同 op、同请求重放回同一结论（语义同 C++ `battle_room_manager.cpp` 的 `SpectatorOperation.rejectTip`）；match 一侧按评审修。
5. **一般-2 签票前核对实例**：查 battle 目录，同 node_id 已是别的实例就按房间不存在处理，查不到不判；目录读加短缓存。
6. **一般-3 空闲收尾前先看最新帧键**：`sweep` 判定源站失联前先 `readState` 一次，读到同实例的 END 就按真实终局收尾，读不到再发 `SOURCE_LOST`。丢最后一回合这一半不补，写进注释。
7. **一般-4 请求历史保留期**：低频任务分批删除 `created_ms` 超过 7 天、不是 head、已有回执或已取消的 `match_global_watch_request` 行，每轮有上限，幂等；跳过评审指出的仍需要的行。
8. **一般-5**：本地路径每名大厅档观众的 INFO 降为 DEBUG 或采样。
9. **一般-6 测试与未单列的小项**：按评审补（退订后立刻重订、连发 TURN + END 的真 Redis 顺序用例等）。
10. **`GAME_MATCH_SPECTATE_RELAY_ENABLED` 默认打开**：按评审「默认打开的改动点」表逐处改。Java 全栈共用一套 Redis，没有跨 Kafka 集群的顾虑。
11. 观战键 TTL、结束释放如评审确认已有等价或不适用，不改，在交付说明写明。

PARITY.md：只改本批自己追加的那几行备注。完整交付说明写到 `D:\luyuan\xuanming-server-mmo-cpp\docs\handoff\spectate-tiered-fanout-20261030\reports\fix-java.md`（含 Codex 命令与每条的处理结果），最终回复三五行摘要。
