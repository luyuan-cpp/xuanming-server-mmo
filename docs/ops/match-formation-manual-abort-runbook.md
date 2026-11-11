# 全球成局人工 ABORT 处置手册

> **状态**：v0.2，2026-11-05 随 [跨区匹配设计](../design/cross-zone-matchmaking.md) §13.3「2026-11-05 修复：永久 UNKNOWN 成局的人工处置工具」一起写，同日按评审修正（目标核对改用环境名、缺票行与成局行损坏各有原因码、写明工具处置不了的非 v1 成局与处置后 battle 侧的表现）。**代码未编译、未测试，本手册未在任何环境演练**，待 Codex 验证。以代码为准：代码行为变了，回来同步这里。
> **依据**：`go/match/internal/authority/manual_abort.go`（放行判据、锁内复核、ABORT 与审计同一事务）、`go/match/internal/logic/stranded_formation_ops.go`（调用方准入、目标核对、结论映射）、`go/match/cmd/formationfix`（工具）。ABORT 写入与截止裁决共用 `authority.commitAbortTx`。
> **Java 版**：待做（见 §8）。

---

## 1. 什么时候看这份手册

工具只是事故出口，替代不了自动路径。绝大多数到期的局由到期恢复（`RecoverExpiredOutcomes`）或帮会取消自动写 ABORT，不需要人。只有下面这些信号持续出现时才看本手册：

| 信号 | 含义 |
|---|---|
| `sum(increase(match_global_flow_total{stage="recover",outcome="original_mismatch"}[10m])) > 0`，ERROR `global formation can never reach a terminal outcome battle_id=… (持久创建原件与终局身份冲突(<原因码>))` | 到期恢复候选形状合格，但持久 Create 正文与成局字段不符。COMPLETE 与截止 ABORT 都裁决不了它 |
| `sum(increase(match_global_flow_total{stage="recover",outcome="expired_unrecoverable"}[10m])) > 0`，ERROR `global formations past deadline cannot reach expired-outcome recovery: count=…` | 形状不合格（能力证据缺失、预检实例与目标不符、battle 明确采用旧协议等），进不了到期恢复候选。这个巡检只数 state 2/3，state 4 的同类局只能用 `formationfix list` 看到 |
| 玩家长期停在「已成局 / 战斗中」，帮会历练大厅不释放，guild 反复打 `[GuildTrial] launch original … recovery failed` | 可能是上面两类之一，用 `formationfix list` 确认 |

2026-11-05 起 `SaveSupportedIntent` 落库前在锁内跑同一判据，这类原件不再能新产生（`match_gather_total{outcome="original_unterminable"}` 计被拒的次数）。还会出现的只有数据损坏、手改，或修复前已落库的行。

## 2. 工具做什么、不做什么

**做什么**：对同时满足下面五条的成局，在与截止裁决同一个事务入口里写 ABORT——终局行 `match_battle_completion`（`decision=ABORT`、`source_kind=MANUAL_ABORT`、没有 Battle 原字节）、`match_formation.state` 置 5、全员原票 2/3 置 4，同一事务写一行审计 `match_manual_resolution`：

1. `match_battle_completion` 没有这一局的任何行；
2. state 为 2/3/4，且有持久 Create 原件；
3. 不归自动路径：原件过不了终局核对（`supportedOutcomeOriginal` 失败），或 operation_id 为空、到期恢复不选它（`operation_missing`，见 §3）；
4. 工作租约已到期；
5. TiDB 时刻已过「有效截止」加 60 秒终局宽限。有效截止取 max(成局截止, 原件截止, 原件准入期限)。

放行条件全部在服务端、在锁内按 SQL 可核的事实判定。工具端的「另查」提示（输出里的 `Also check` 段，§5 第 3 步）不作放行条件。

**处置的后果**：这一局全员白打。不发结算、不计评分，原票结束，玩家可以重新匹配。帮会历练局在 guild 恢复器的下一轮（每 10 秒）读到这份 ABORT，与截止 ABORT 走完全同一条路径：同一事务释放 launch、全员角色占用与大厅，`guild_trial_battle` 按 LOSS 收尾。match 不读写 guild 的库（AGENTS.md §7.6），帮会侧不需要任何人工操作。

**不做什么**（遇到就拒绝，见 §3 原因码）：

- 原件合格的局（`class=auto`）：归到期恢复 / 帮会取消，人工不得抢。2026-11-06 起自动作废之前先向原 battle 实例取证，原实例仍持有房间或连不上时最多等到截止加约 13 分钟（硬上限 + 持有宽限，见设计 `cross-zone-matchmaking.md` §13.3「2026-11-06 修复：截止作废先取证」），这段时间里 `abort_wait_*` 计数在涨是正常的。超过这个时间仍没被自动处理，查到期恢复循环本身（match 副本是否在跑、TiDB 是否可写），不要用本工具；
- 已有任何终局行：COMPLETE 已落、只有 Kafka 归档（decision=0），或终局身份与成局冲突。工具只写新终局，不覆盖已有行。这几种升级给研发；
- state 1（还没有 Create 原件，由 `Finish(false)` 收尾）、state 5 却没有终局行（旧代码的安全中止）；
- 成员原票已不在 2/3、换了轮次或换了主（`tickets_not_original`），或某名成员的票行不存在（`ticket_missing`）；
- COMPLETE 之后个人结算还没 APPLIED、票停在 state 3：这是收件箱侧的问题，见 [收件箱手册](player-inbox-runbook.md)，释放票会让那份奖励孤立。

**不要做的事**：

- 不要手改 `match_formation` / `match_authority_ticket` / `match_battle_completion` / `match_manual_resolution` 去伪造终局；
- 不要绕过工具直连 match 库写，不要跨库改 guild 的表；
- 不要在维护排空期间运行。发布准入会关闭新工作入口，调用回 `Unavailable`。

## 3. 原因码

`show` / `list` 打印两类码：`failed_check` 是原件第一项不通过的核对，说明这一局为什么永久 UNKNOWN；`refusal`（`actionable: no (…)`）说明工具为什么不处置。

| failed_check | 含义 | 分类 |
|---|---|---|
| `outcome_protocol` | 成局行上的终局协议不是 1。任何自动路径都不裁决它；2026-11-06 起过了截止就不再续租（原先 state 2/3 的会被反复续 120 秒、工具处置不了），可以处置 | shape_invalid |
| `preflight_version` | 发房前能力版本不是 1 | shape_invalid |
| `target_instance_missing` / `preflight_instance_mismatch` | 目标实例为空，或预检实例与目标不符 | shape_invalid |
| `adopted_legacy` | battle 明确回执采用了旧协议。另查这一局是否已按旧路径发过结算（§5 第 3 步） | shape_invalid |
| `adopted_invalid` | 采用回执非法 | shape_invalid |
| `operation_missing` | operation_id 为空。COMPLETE 在入口就被拒、到期恢复不选它，2026-11-06 起帮会取消也不再给它写截止 ABORT（与到期恢复同一判据），本工具是唯一出口 | shape_invalid |
| `create_undecodable` / `create_protocol` | 原件不能解码 / 原件协议不是 1 | original_mismatch |
| `battle_id_mismatch` / `operation_mismatch` / `target_instance_mismatch` / `config_mismatch` / `mode_mismatch` | 原件身份与成局不符 | original_mismatch |
| `deadline_missing` / `deadline_mismatch` | 原件没有截止 / 截止与成局不符 | original_mismatch |
| `created_before_formation` | 原件创建时刻早于成局时刻。2026-11-05 修复前 match 本机时钟慢于 TiDB 留下的形状 | original_mismatch |
| `created_after_deadline` / `players_count_mismatch` | 原件创建时刻不早于截止 / 参战人数与成局不符 | original_mismatch |

| refusal | 含义 | 怎么办 |
|---|---|---|
| `auto_recoverable` | 原件合格 | 等自动恢复；长时间不动查到期恢复循环 |
| `deadline_not_due` | 还没过有效截止加 60 秒 | 等到 `effective_deadline_ms + 60000` 之后再来 |
| `work_lease_active` | 工作租约未到期，持租副本可能还在推进 | 等 `retry_at_ms` 过去（最多 120 秒）。过了截止、有原件的局不会再续租，一直这样就升级研发 |
| `deadline_missing` | 成局与原件都没有截止 | 升级研发 |
| `has_terminal_row` | 已有终局行（只有归档，或身份冲突） | 升级研发，附 `show` 输出 |
| `tickets_not_original` | 原票已换轮、换主或不在 2/3 | 升级研发 |
| `ticket_missing` | 成局还在，某名成员的票行却不存在（match 从不删票行，只会是数据损坏或手改）。`show` 打出的 `ticket:` 行会少于 `members` | 升级研发，附 `show` 输出 |
| `not_started` / `state_released` / `state_invalid` / `create_missing` | 不是本工具处理的状态 | 升级研发 |
| `target_mismatch` | `-expect-target` 与本部署的环境名（match 配置 `GlobalMatch.ManualResolutionEnvironment`）不符 | 确认 `-endpoint` 连的就是工单上的环境；`-expect-target` 按工单填，不要照抄 `show` 打印的 `target=` |
| `formation_unreadable` | 成局行本身解不开（成员、备战端点等列损坏）。`list` 只把这一局标出来，其余照常列；`show` / `abort` 回同一个码。连接断开、超时这类读库失败不会报成它，那种回「稍后重试」（退出码 1） | 升级研发 |

## 4. 开启入口

入口缺省关闭。开启步骤：

1. 生成一把至少 32 字节的随机密钥，放进 K8s Secret。它与观战运营的 `MMORPG_MATCH_OPS_CALLER_SECRET` 分开，不得共用；
2. match 配置写 `GlobalMatch.ManualResolutionSecretEnv: MMORPG_MATCH_FORMATION_FIX_SECRET` 与 `GlobalMatch.ManualResolutionEnvironment: <本部署的环境名>`（1~64 个 ASCII 字母、数字或 `.` `_` `-`，例如 `prod-global`、`staging-global`，每个环境不同），Pod 注入同名环境变量，滚动重启 match。环境名就是工具 `-expect-target` 要填的值，也写进工单；
3. 启动日志里没有 `Stranded formation manual resolution disabled` 即已开启。变量缺失、密钥不足 32 字节、或没写有效的环境名时会打这行 ERROR，入口保持关闭，不影响服务。

处置完成后撤掉环境变量（或配置项）再滚动重启，入口回到关闭。工具只认 serviceauth 调用方 `ops-formation`，带客户端会话的调用一律拒绝；签名时间戳窗口为 5 分钟，运行工具的机器时钟要准。

## 5. 处置步骤

工具不连数据库，只调 match 的 gRPC（`-endpoint`，即 match 的 `ListenOn`）。在网络策略允许访问 match gRPC 的运维 Pod 里运行，密钥只从环境变量读，不进命令行，也不进输出：

```bash
export MMORPG_MATCH_FORMATION_FIX_SECRET=<与 match 同值>
EP=<match Pod IP>:50500
```

1. **列出**：`formationfix -endpoint $EP list`。每页最多 100 条，按 `(retry_at_ms, battle_id)` 翻页，末尾打印下一页命令。列表里含原件合格、等自动恢复的局（`class=auto`），`manual` 一栏是 `eligible` 才可处置。
2. **核对**：`formationfix -endpoint $EP show -battle <id>`。确认 `target=` 就是工单上写明的环境，记下 `digest=`，看 `class`、`failed_check`、`effective_deadline_ms`、`terminal: none`，以及每名成员的票（`state` 2/3、`formation_id` 等于本局，`ticket:` 行数等于 `members`）。`show` 是锁外预检，不看票是否仍是原票，最终以第 4 步的锁内演练为准。
3. **另查**（只作提示，不作放行条件）：
   - battle 日志里这个 `battle_id` 的 `ErrBattleOutcomeOriginal` / `battle_sql_outcome_unflushed_at_shutdown`：确认 battle 端也没能提交终局；
   - `failed_check=adopted_legacy`：另查这一局是否已按旧路径发过结算；
   - `failed_check` 是 `operation_mismatch`、`target_instance_mismatch`、`target_instance_missing` 或 `operation_missing`：battle 提交终局时带的是原件里的身份，与处置写下的 ABORT（记的是成局行的身份）对不上，处置之后 battle 的提交仍被拒（match 日志 `match_terminal_outcome_rejected reason=original_conflict`，battle 收到 `FailedPrecondition`），battle 房间不会因这份 ABORT 收尾，按 battle 自己的重放上限（快速 6 小时、再慢速 24 小时，见 `cpp/libs/services/battle/system/result_commit.h`）本地删房，期间的滞留房间提醒是预期的。玩家的票与帮会占用照常在 match / guild 侧释放。其余类别身份一致，battle 的迟到提交会读到 ABORT 并据此收尾；
   - 帮会历练局（`activity=true`）：记下帮会侧报错的 launch 号，第 6 步核对。
4. **演练**（不写库）：

   ```bash
   formationfix -endpoint $EP abort -battle <id> -expect-digest <digest> -expect-target <工单上写明的环境名> \
     -operator <名字> -reason "<工单号>: <核对结论>" -confirm <id>
   ```

   `-expect-target` 按工单填，不要从 `show` 的输出照抄：照抄就核不出 `-endpoint` 连错了环境。

   与真正执行同一锁序、同一判据，最后整段回滚。输出一行 `[FormationManual] … executed=false` 和 `Plan only`。
5. **执行**：同一条命令加 `-execute`。退出码 0、输出 `[FormationManual] … executed=true` 即已提交，把这一行贴进工单。服务端同时打一行同样内容的 ERROR 日志（battle_id 只进日志正文，不做指标标签），并计 `match_global_flow_total{stage="terminal",outcome="abort_manual"}`。
6. **确认**：
   - `show -battle <id>` 应显示 `terminal: decision=BATTLE_OUTCOME_DECISION_ABORT source_kind=MATCH_BATTLE_COMPLETION_SOURCE_MANUAL_ABORT`，`actionable: no (resolved)`；
   - 帮会历练局：10 秒内 guild 日志出现 `[GuildTrial] launch original <launch 号> of guild <帮会> released on an authoritative match cancel receipt`，之前的 `recovery failed` 不再出现。

退出码：

| 码 | 含义 | 下一步 |
|---|---|---|
| 0 | 成功（`list` / `show` 读成、演练通过、或 ABORT 已提交） | — |
| 1 | 执行出错：读库失败、传输失败、提交结果未知 | 先 `show` 重查：显示 `source_kind=…MANUAL_ABORT` 即已完成；否则稍后重试。重复执行是安全的（第二次回「已处理」） |
| 2 | 参数不合法、入口未开或签名被拒、前置条件不满足、没有这一局 | 按 stderr 与 §3 原因码处理 |
| 3 | 已被处理，或成局在 `show` 之后变过（例如迟到的采用回执），本次未改动 | 重新 `show`，拿新的 `digest` 再来 |

## 6. 为什么只会释放一次

- 锁序与 COMPLETE、截止 ABORT、帮会取消同一入口 `terminalFormation`：全员原角色 P 升序 → `match_formation FOR UPDATE` → 原票 `FOR UPDATE`。锁内先复读终局，再取 TiDB 时刻跑放行判据，再逐字节比对操作员核对过的成局摘要；
- `match_battle_completion` 与 `match_manual_resolution` 的主键都是 `battle_id`。多个 match 副本、多位操作员同时执行，只有一方写下 ABORT，另一方回「已处理」（退出码 3）；
- 审计行与 ABORT 在同一事务里写：审计行写不进去时整段回滚、回「稍后重试」，不会出现有 ABORT 却没有审计的局；
- 可处置的局都不归自动路径：到期恢复、帮会取消、提前裁决只裁决 `class=auto` 的局，对其余各类只回 UNKNOWN、不写库，COMPLETE 也会被永久拒收（`operation_missing` 过得了原件核对，但 COMPLETE 在入口就因 operation 为空被拒），所以人工 ABORT 不会作废一份合法终局；
- 有效截止取三者最大、再加 60 秒宽限：battle 房间按原件截止强制结束，准入期限之后不会再接这份 Create。

## 7. 审计

`match_manual_resolution` 一局一行，只增不改：操作人、依据、TiDB 时刻、处置前成局行与全员原票的确定性序列化、成局摘要、分类与原因码、有效截止、执行副本主机名。只读查询：

```sql
SELECT battle_id, formation_class, failed_check, operator_name, reason, resolved_ms, formation_digest_before, effective_deadline_ms, replica_instance
FROM match_manual_resolution ORDER BY resolved_ms DESC LIMIT 50;
```

表由迁移任务 `match -f <配置> -migrate` 建出，常驻账号只需 DML。

## 8. 待补

- 工具还没有打进 match 镜像，也没有现成的运维 Pod 模板；首次上线前补部署形态（只装工具、不带 match 写资格）；
- 回包没有签名：连到伪造的端点会看到伪造的结论。网络策略应只允许运维 Pod 访问 match gRPC；
- 巡检计数 `expired_unrecoverable`（`CountStrandedExpiredFormations`）2026-11-06 起与 `list` 同一圈定范围，数的就是 `list` 里 `shape_invalid` 一类（含 state 4、终局协议不是 1 的局）；`original_mismatch` 要解原件才判得出，不在这个计数里，看到期恢复的 `original_mismatch` 计数与 `list`；
- 已有冲突终局行、只有归档行的成局，工具只报告不处置，处置方案另行设计；
- 目标核对只是辅助：`-expect-target` 按工单填才有用，照抄 `show` 的 `target=` 就核不出连错环境。真正防误操作的是按环境分开的密钥（另一个环境的密钥验签不过）与成局摘要（`battle_id` 与成局行逐字节绑定）；
- Java 版待做：同一放行判据、同一锁序与 ABORT 事务入口、审计表、独立调用方密钥、缺省关闭。
