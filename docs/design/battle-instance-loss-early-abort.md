# 战斗进程失效的提前作废与作废通知（方案）

> 状态：方案，2026-11-11 起草；第 10 节四项负责人同日按推荐定案。没有落码，落码顺序见第 11 节。
> 关联：场景服冻结到期与续冻见 [turn-battle-gap-closure.md](./turn-battle-gap-closure.md) §11；截止作废先取证（B5）见 [cross-zone-matchmaking.md](./cross-zone-matchmaking.md) §13.3；缺口 e2e-3 见 [turn-based-battle-server.md](./turn-based-battle-server.md) §22.6；作废时释放道具见 [battle-item-reservation.md](./battle-item-reservation.md)；近期战斗结果见 [battle-result-recovery.md](./battle-result-recovery.md)。
> 两版：Java 版有同样的缺口（Java 仓 `docs/porting/battle-node-spec.md` §4.7：节点崩溃只靠场景服 reaper 到期解冻，也没有通知），落码时两版与客户端同批（AGENTS §12）。

## 1. 现状

战斗服进程在打的过程中崩溃，玩家不会被永远卡住，但要等很久，而且全程没有任何提示。

| 环节 | 现在的规则 | 代码 |
|---|---|---|
| 截止时刻 | 成局时刻 + 120 秒备战预算 + 模板战斗时长（缺省 180 秒，上限 3600 秒） | `go/match/internal/authority/match_rules.go` `battleDeadlineMs` |
| 场景服冻结 | 截止 + 60 秒终局宽限 + 60 秒投递余量后到期；之后每 30 秒向匹配服查 SQL 终局，ABORT 才解冻，查不到就续冻，最晚截止 + 17 分钟强制解冻 | `player_battle.cpp` `ResolveExpiredFightingFreeze`；`player_battle.h` `kFreezeHoldCapMs` |
| 匹配票 | 截止 + 60 秒后才向原实例取证，有证据写 ABORT；没证据等到截止 + 11 分钟无证据作废。之前票停在 state 3，排队被拒 | `go/match/internal/authority/terminal_abort.go` `judgeDeadlineAbort` |
| 玩家看到的 | 只有直连断开；补签在 ABORT 落库前一直失败；场景服解冻时什么也不推 | `global_match.go` `globalRequestBattleTicket`；`player_battle.cpp` `ClearBattleFreeze` |

缺省模板在开打第 1 分钟崩溃，玩家要到成局后约 7 分钟（有证据）或 17~20 分钟（没证据）才能再排；长模板最长一个多小时。

现有机制提前不了，原因有五条：

1. B5 的取证只在截止 + 宽限之后才做。
2. 注册中心摘除很慢：节点 etcd 租约是 180 秒（`bin/etc/base_deploy_config.yaml` 的 `Etcd.NodeTTLSeconds`，压测时从 60 秒调上来的）。
3. 生产推荐的 Agones 形态下，GameServer Pod 是 `restartPolicy: Never`（`tools/scripts/k8s_deploy.ps1` 的说明）。进程崩溃后 Agones 很快删掉 Pod，原 IP 连上去只会超时。B5 认的两种证据（原地址上是另一个实例、拒连且已摘除）这时都拿不到，只能等 10 分钟硬上限。
4. ABORT 不给场景服投递任何东西，场景服只能等冻结到期后自己来问。
5. 参战者没有「本局作废」的客户端契约：`eBattleOutcome` 没有作废值，只有观众有 `SPECTATE_END_BATTLE_ABORTED`。

## 2. 目标与边界

目标：

- **G1**：原执行进程「确定已不在」时，崩溃后约 1 分钟内写下这一局唯一的 ABORT。
- **G2**：任何来源的 ABORT（提前作废、截止作废、帮会取消、人工处置）落库后，参战者几秒内解冻、删锁、放回道具预留。
- **G3**：客户端收到明确的「本局作废」，重登后在近期战斗结果里也看得到。

不做的：

- 不改截止作废的硬上限和场景服的续冻上限，它们继续兜底。
- 不做「租约过期就判死」，即 battle 与 etcd 或 match 失联一段时间就作废它的局。这样 etcd 或 TiDB 抖一次，全服在打的局会一起作废。B5 已经定过不按节点缺席判死，本方案沿用。
- 整机失联、Pod 卡在 Terminating 的情形拿不到确定证据，仍走截止兜底（第 8 节）。
- 观战侧不变，仍是 SOURCE_LOST / ABORTED。

## 3. 总览

方案分三部分，B、C 不依赖 A，可以先做：

- **A 匹配服提前作废**：参战者补签失败时把这一局记为可疑 → 定期向原实例取证 → 两次确定证据相隔不少于 30 秒 → 锁内写 ABORT。
- **B 作废送到场景服**：写 ABORT 的同一事务里，给每名参战者写一份「作废结算」来源 → 走现有个人发件箱 → player_inbox → 场景服短路应用 → 存档确认后解冻、删锁、推 `BattleEndS2C`。
- **C 客户端契约**：`eBattleOutcome` 追加 `BATTLE_OUTCOME_VOID = 4`。

## 4. A：匹配服提前作废

### 4.1 什么时候开始怀疑

- **补签**：`globalRequestBattleTicket` 里，成局还没终局，向原实例的 `QueryBattleCreation` 传输失败，或者回了空包（原地址上是另一个实例），就把这一局记进本副本的可疑表。直连一断，参战者的客户端会立刻补签，在线玩家几秒内就会触发。
- **建房收尾**：`globalAbortPrepared` 里 `abortProvablyNoRoom` 连不上原实例时同样记入。

可疑表放在内存里，按 battle_id 去重，设上限（如 4096）。补签入口已经由 `AuthorizeBattleTicket` 核过身份，只有这一局的参战者或观众能触发。副本重启后可疑表会丢：玩家下次补签或重登会再触发，最差回到截止兜底。

不拿注册中心摘除当触发，因为 180 秒太慢；也不做全量巡检，因为截止作废已经兜底。

### 4.2 什么算「原进程确定已不在」

沿用 B5 的同一次交互（`askOriginalInstance`）和同一个分类函数，只认三种证据：

- **E1**：原地址上应答的是另一个实例，即查询成功但回了空包。这是 B5 现有的判据。battle 的实例 UUID 每个进程生成一次。
- **E2**：原地址的 TCP 端口当场拒连（RST），说明主机在、端口上没有进程在听。和 B5 不同，这里不再要求注册中心已经摘除。B5 加这个要求是为了避开停机排空窗口，现在改由 4.3 的复核间隔来覆盖。
- **E3**（新增，只在 K8s 下）：battle 所在命名空间里已经没有任何 Pod 持有原地址的 IP。C++ 节点在 K8s 里登记的内部地址就是 `POD_IP`（`node.cpp` 读 `POD_IP`；`tools/scripts/lib/k8s_client_entry.ps1` 用 `status.podIP` 注入）。正常删除时，Pod 对象要等 kubelet 确认容器已停才会消失，所以这个 IP 上没有 Pod 了，就说明原进程已死。例外见 4.6。

原实例亲口回 PRESENT，立刻撤销怀疑。超时、黑洞、gRPC 报错、只是注册中心查不到，都算「判不了」：不算证据，也不撤销怀疑。

### 4.3 复核规则（唯一的时间判据）

- 第一次拿到确定证据时，记下那一刻的 SQL 时刻 `first_seen`。之后每 5 秒取证一次，没进展就退避到 30 秒。
- 写 ABORT 要同时满足两条：这一次又拿到了确定证据（三种任一，不必和第一次同种）；这一次距 `first_seen` 不少于 **S = 30 秒**。期间只要出现一次 PRESENT，就清零重来。
- S 必须大于 battle 停机排空上限（`node.cpp` 的 `kShutdownDrainBudget`，15 秒，排空期间会补提交终局）加一次 `CommitBattleOutcome` 调用的期限。理由：E1、E2 第一次出现时，原进程至少已经停止监听，最多再排空 15 秒就装上停止闸，不再提交。过了 S，它发出的最后一次提交要么已经落库，这时 ABORT 事务会读到终局行而什么都不做；要么已经失败。E3 也一样：S 覆盖了「死前刚发出、还在路上的提交」。
- 判据写成纯函数，和 `judgeDeadlineAbort` 放在一起。Go 测试读 C++ 常量，核对 S 大于排空预算加调用期限，做法同 `TestDeadlineAbortSceneConstantsMatchCpp`。

### 4.4 提交

- 取证在事务外做，事务里只复核。锁序沿用截止裁决：全员原 P 升序 → formation `FOR UPDATE` → 原票。锁内先复读终局行，再核对 operation 和 target_instance 与取证时一致。proof 带 `first_seen` 和 `latest_seen` 两个 SQL 时刻，锁内要求 `stamp - first_seen ≥ S`，且 `stamp - latest_seen ≤ 15 秒`（证据要新鲜）。
- 写入只走 `commitAbortTx`。终局来源新增 `MATCH_BATTLE_COMPLETION_SOURCE_MATCH_INSTANCE_GONE_ABORT = 6`。计数用 `match_global_flow_total{stage="terminal",outcome="abort_instance_gone"}`，日志带 battle_id 和证据种类。
- 不要求工作租约，和 COMPLETE 一样只靠锁序与终局主键互斥。理由：建房目标在提交点就固化进 formation，原实例已经不在，建房不可能再成功，也没有换节点的路径。落码时要对照 `AbortWithoutRoom` 的 `checkWork` 再核一遍，确认没有漏掉在途建房的副作用。

### 4.5 截止作废同步收紧（规则收一处）

B5 拿到「拒连 + 已摘除」会立即作废。有一种情形它可能抢在补提交之前写下 ABORT：battle 分区恢复后重注册失败 → 停听 → 排空并补提交（最多 15 秒）。这段时间里它已经被摘除，端口也拒连。

截止作废改用同一个分类函数和 4.3 的复核规则，即多一次相隔至少 S 的复核；硬上限和持有宽限不变。代价是有证据的截止作废晚大约 30 秒，仍远早于场景服的续冻上限。

### 4.6 E3 的部署前提

- 在 battle 所在命名空间给 match 的 ServiceAccount 加一条只读 Role：pods `list`，按 `status.podIP` 字段选择。battle 现在在 `mmorpg-infra`，和 match 同一个命名空间。先例：`deploy/k8s/manifests/go-svc/scene-manager-agones-rbac.yaml`。
- 新增配置 `GlobalMatch.BattlePodNamespace`，留空就关闭 E3（本机开发和非 K8s 部署）。API 调用失败按判不了处理。
- 运维约束写进手册：不要用 `--force --grace-period=0` 强删 battle Pod，也不要手工删 Node 对象。这两种操作会让 Pod 对象先消失，而进程可能还活在失联的节点上。match 会按 E3 作废这一局，进程之后提交的结果会被拒收：终局只落一份，数据一致，但这一局白打。

### 4.7 各种情形的结论

| 情形 | 拿到的证据 | 结果 |
|---|---|---|
| 进程崩溃，主机或 Pod 还在（本机，或 Deployment 还没重启） | E2 | 首次证据后约 30 秒作废 |
| Deployment 容器原地重启 | 先 E2，后 E1 | 同上 |
| Agones 下崩溃（Pod 随后被删） | 先 E2，或直接 E3 | Pod 删掉后约 30 秒作废 |
| 正常停机（排空不超过 15 秒） | 排空中出现 E2 | 复核时终局已落库或补提交已失败；有终局就不动 |
| match 与 battle 网络分区，battle 还活着 | 超时（判不了） | 不作废，等截止兜底 |
| 整机失联，Pod 卡在 Terminating | 判不了 | 不作废，等截止兜底 |
| 有人强删 Pod，而进程还活着 | E3 | 作废（手册禁止强删） |
| 客户端自己网络差，误触发了怀疑 | PRESENT | 撤销，不作废 |

## 5. B：作废送到场景服

### 5.1 匹配服：所有 ABORT 都给参战者写作废来源

- 在 `commitAbortTx` 的同一事务里，按成员写 `match_battle_settlement_source`，列和 COMPLETE 相同（`completion.go` 里 COMPLETE 写个人来源那段）。
- 载荷是作废结算：`BattleSettlementData{battle_id, player_id, outcome = BATTLE_OUTCOME_VOID, player_team_index, match_mode, battle_config_id}`，其余效果字段全空。
- 归属列取锁内读到的原票（截止裁决等入口已经持有这些行锁）。
- 现有个人发件箱只按行状态取件（`outcome_outbox.go`），不看终局种类；player_inbox 也不解析载荷。所以作废行会被原样投递，不用另写投递逻辑。
- 票仍在 ABORT 事务里直接放回（state 4），不等作废 APPLIED。
- 对所有 ABORT 来源都生效：截止、帮会取消、提前作废（no_room 与 instance_gone）、人工处置。`cancelPrepared` 这类按 ABORT 解冻备战的现有调用保留，它们是幂等的。

### 5.2 场景服：作废短路应用

`PlayerBattleSystem::ApplyInboxSettlement` 在应用任何效果之前先认出 VOID：

- 效果字段（health、mana、经验、金币、货币、物品、宝宝、击杀、回合数等）只要有一个非空，就按版本错位处理（`kVersionSkew`，停这个域，fail-closed）：既不当作废，也不当结算。
- 否则：释放这一局的战斗道具预留（`ReleaseBattleItemHold`，原因记 void）；写一条近期战斗结果 `PlayerBattleResult{outcome = VOID}`（没有明细行，`settled_at_ms` 取本地时刻）；返回 APPLIED。
- **绝不调用 `ApplySettlementToEntity`**：`BattleSettlementData.health` 的注释是「战后终值，由 scene 回写」，作废载荷里的 0 会把血量写成 0。

`CompleteInboxSettlement` 不改：存档确认后按同一局条件删锁、摘 `InBattleComp`、推 `BattleEndS2C`（outcome = VOID），并公开结果。

下面这些情形由收件箱现有机制直接覆盖，不用另写：

- 重复投递：按战斗结局环去重。
- 离线玩家：下次落地时拉取。
- 「按锁重建冻结」与作废交错：结算已在环里时，现有逻辑会撤回重建出来的冻结。

reaper 的到期查询保留作兜底；VOID 已应用时它走 `applied_here` 直接解冻。

### 5.3 为什么复用结算链，而不另开一种消息

- 至少一次投递、按（玩家, 战斗）去重、离线补投、与存档同一次提交，这些都是现成的。
- 战斗结算域 `max_pending = 1`，作废和结算排在同一条有序邮箱里，不会打破 D4「每人最多一单在途」。
- 另开一种收件箱消息，就要多一套生产、去重和告警。在业务上，作废就是「这一局的个人结果 = 没有任何效果」，归同一个域最自然。

## 6. C：客户端契约（两版与客户端同批）

- `proto/battle/battle_data.proto` 的 `eBattleOutcome` 追加 `BATTLE_OUTCOME_VOID = 4;`。它只由 match 的 ABORT 产生，battle 引擎永远不会产出。改完按 AGENTS §4 重生协议。
- 参战者：大厅连接收到 `BattleEndS2C{outcome = VOID}` 时退出战斗界面，提示「战斗服务器异常，本局作废，没有奖励也没有消耗，可以重新排队」；近期战斗结果列表里这一局显示「作废」。
- ABORT 落库之前补签会失败，这段时间客户端的文案改成中性（e2e-3 的候选修法 ①），不再断言「本局由系统自动出手」。
- 观众不变。robot 要认得 VOID，压测统计里单列一项。
- 服务端里消费 `eBattleOutcome` 的地方（rating、帮会历练、榜单、回放）要逐个核对：ABORT 不产生整局结果事件，它们收不到 VOID，但仍要有 switch 默认分支和单测兜住。

## 7. 改后的时间线

| 情形 | 现在 | 改后 |
|---|---|---|
| 崩溃，参战者在线 | 截止 + 2~3 分钟（有证据）或截止 + 11~17 分钟（没证据）；缺省模板即崩溃后约 6~20 分钟 | 约 40~50 秒：补签触发几秒 + 复核 30 秒 + 取证节拍不超过 5 秒 + 发件箱不超过 2 秒 + 收件箱与存档确认几秒 |
| 崩溃，参战者都离线 | 同上 | 有人重登补签时触发；没人回来就和现在一样 |
| 截止作废、帮会取消、人工处置 | ABORT 落库后，要等冻结到期再来查询 | 落库后几秒 |
| 整机失联 | 截止 + 11~17 分钟 | 不变 |

改后所有情形都有作废通知。

## 8. 剩余风险

- 整机失联、Pod 卡在 Terminating：仍要等截止兜底。
- 强删 Pod 或手工删 Node：靠手册约束，见 4.6。
- 防火墙按 REJECT（回 RST）配置时，分区会看起来像 E2。K8s NetworkPolicy 的默认行为是丢包，不受影响；自建防火墙要配成丢包。
- 战斗结算域 `max_pending = 1`：上一局的结算还在 DEFER（例如两包都满）时，本局作废要排在它后面才能入箱，这期间场景服仍按续冻规则处理。
- 可疑表在内存里：副本重启就丢，回到「下一次补签再触发」或截止兜底。

## 9. 改动清单（估计 25~30 个文件，超过 30 个要先停下来报告）

- **proto**：`battle_data.proto`（VOID）、`match_authority.proto`（来源值 6），改完重生。
- **go/match**：
  - 可疑表与取证循环（新文件，暂定 `logic/global_instance_gone.go`）；
  - 分类函数加 E3、复核纯函数、`AbortInstanceGone` 锁内提交；
  - `commitAbortTx` 写作废来源（覆盖所有 ABORT 入口）；
  - 补签和建房收尾两处触发；
  - K8s Pod 查询（新增 client-go 依赖，scene_manager 已经在用同一个库）；
  - 配置、指标、单测。
- **部署**：`k8s_deploy.ps1` 给 match 加 ServiceAccount、Role 和 RoleBinding（只读 pods list）并注入配置；写运维手册。
- **C++ scene**：`ApplyInboxSettlement` 的 VOID 短路、近期结果写 VOID；单测加在 `bag_test`（含 `battle_freeze_hold_test.cpp`）。
- **客户端**（独立仓，要先授权）：处理 VOID、改文案。robot 统计 VOID。
- **Java 版**：同批实现场景服、匹配服的等价逻辑和客户端契约，在 `PARITY.md` 登记。
- **文档**：本方案、turn-based §22.6 e2e-3、cross-zone §13.3、turn-battle-gap-closure §11、battle-item-reservation（释放来源多一种）、运维手册。

## 10. 定案（负责人 2026-11-11）

1. **给 match 加 battle 所在命名空间的只读 Pod 权限，启用 E3。** 权限只有 pods `list`，只在同一个命名空间，参照 scene_manager 已有的同类权限。没有它，Agones 下的崩溃基本拿不到证据。
2. **客户端可见的 `eBattleOutcome` 追加 `BATTLE_OUTCOME_VOID = 4`**，客户端与 Java 同批改。不另加字段区分作废，免得客户端多一层判断。
3. **截止作废同步改用 4.3 的复核规则**（4.5），截止作废与提前作废只有一份证据判据。
4. **复核间隔 S = 30 秒，取证节拍 5 秒**（没进展时退避到 30 秒）。

## 11. 前置与落码顺序

1. 先等 B5 由 Codex 验证。B5 在 11-11 落码、未编译，本方案要改它的同一批文件（`global_deadline_evidence.go`、`terminal_abort.go` 等）。
2. 战斗道具预留（11-10~11 落码、未编译）也要先验证：作废短路要调它的释放入口。
3. 第一批：B + C（作废送达 + 客户端契约）。对现有的所有 ABORT 立即见效，不依赖 Pod 权限。
4. 第二批：A 的 E1、E2，连同截止作废改用同一复核规则（4.5）。本机和 Deployment 形态见效。
5. 第三批：E3 与部署权限（第 10 节第 1 项）。Agones 形态见效。
6. 每一批的 Codex 命令随落码写进 `docs/PROGRESS.md`。

## 12. 验证要点（给 Codex，落码时细化成命令）

- **Go**：
  - 复核判据表：`first_seen`、PRESENT 清零、判不了不算、两次证据不同种、S 的边界；
  - 分类函数的 E1、E2、E3；
  - 锁内复核：终局已在、operation 不符、证据过期；
  - `commitAbortTx` 写出作废来源，且每个 ABORT 入口都会写；
  - 发件箱能投递作废行；
  - 读 C++ 常量做对照。
- **C++**：
  - VOID 不改血量、释放预留、写 VOID 结果、返回 APPLIED；
  - VOID 带任何效果字段时判版本错位；
  - `CompleteInboxSettlement` 推出 VOID；
  - reaper 对已应用的 VOID 走 `applied_here`。
- **演练**：
  - 本机 `kill -9` battle：参战者约 1 分钟内收到作废，并能再排队；
  - Deployment 形态的容器崩溃；
  - Agones 下 GameServer 被删之前、之后各演一次；
  - 正常停机排空期间不误判。
