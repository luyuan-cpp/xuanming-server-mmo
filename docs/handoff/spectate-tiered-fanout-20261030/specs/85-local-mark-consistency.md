# 任务 LOCAL-MARK：本地模式观战的原操作与标记一致性（已结案）

**结论（2026-11-05）**：任务已结案，不用再做，也不用验证这里的旧方案。Go 的本地 Redis 观战路径已于 2026-10-30 随「go/match 只剩 SQL 权威路径」整段删除，开发与生产走同一条 SQL 路径。本任务为本地路径写的代码与用例（`spectate:watching:*` 标记、`stopWatching`、`EntryOfInstance` 等）随之删除，历史见 `docs/PROGRESS.md` 2026-10-24、10-30 的条目与 git 记录。

## 规则在 SQL 路径上的落点

权威说明在 [battle-spectate-tiered-fanout.md](../../../design/battle-spectate-tiered-fanout.md) §10.1，这里只列落点：

- Add 与 Remove 都沿 `match_spectator` 原件带同一个 op、准入截止与实例。只有带回原 op 与实例的 `cancelled` 才算取消证明（`observerCancelProven`），没有证明就不关闭占用。
- 换场与进匹配都要先拿到取消证明（`cancelGlobalSpectator`、`closeCapturedSpectatorExit`）。AddObserver 结果未知时保留原件，同 request_id 重试或后台恢复沿同一个 op 续推。
- 跨会话复核在 `locateGlobalWatch`；观众名只取角色公开名（`challengePublicName`）。

**为什么不再要本地路径**：Redis 标记与 SQL 占用是两份真相，标记的 TTL 和尽力清退给不出事务互斥。SQL 原件在提交点与匹配票互斥，并有后台恢复。

## 本任务最后一处修复

- **准入窗口**：`go/match/internal/authority/spectator.go` 的 `spectatorAdmitWindowMs`。准入截止从 SQL now + 30s 改成 + 25s。battle 按自己的墙钟只接受至多未来 30s，SQL 时钟只要比它快过落原件到 Add 送达这段耗时，Add 和 Remove 就都被拒；留 5s 给两边的时钟偏差。
- **用例**：`authority/spectator_native_test.go` 的 `TestSpectatorTiDBAdmitWindowLeavesClockSkewMargin`，要真 TiDB。
- **验证**：按 [CODEX.md](../CODEX.md) 第 4 步，必须 0 SKIP。

## Java

本地路径还在，原操作已由 `add334cb` 落码。两条待做项登记在 Java 仓 `PARITY.md`「观战 P0 缺陷」行：robot 走 dev 口的观战步骤缺原操作字段，本地 163 缺跨会话栅栏。准入窗口 Java 没有这个问题：全局路径取 min(30s, 请求剩余预算)，本地路径固定 25s。
