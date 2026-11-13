# 战斗进程失效的提前作废与作废通知

> 状态：2026-11-11 起草并定案，11-12 服务端与客户端代码写完，**未编译、未测试，待 Codex 验证**（第 12 节）。Java 版待做。
> 关联：场景服冻结到期与续冻见 [turn-battle-gap-closure.md](./turn-battle-gap-closure.md) §11；截止作废先取证（B5）见 [cross-zone-matchmaking.md](./cross-zone-matchmaking.md) §13.3；缺口 e2e-3 见 [turn-based-battle-server.md](./turn-based-battle-server.md) §22.6；作废时释放道具见 [battle-item-reservation.md](./battle-item-reservation.md)；近期战斗结果见 [battle-result-recovery.md](./battle-result-recovery.md)。

## 1. 改之前的问题

战斗服进程在打的过程中崩溃，玩家不会被永远卡住，但要等很久，而且全程没有任何提示。

| 环节 | 改之前的规则 |
|---|---|
| 截止时刻 | 成局时刻 + 120 秒备战预算 + 模板战斗时长（缺省 180 秒，上限 3600 秒） |
| 场景服冻结 | 截止 + 2 分钟后到期；之后每 30 秒向匹配服查终局，作废才解冻，最晚截止 + 17 分钟强制解冻 |
| 匹配票 | 截止 + 60 秒后才向原实例取证，有证据写作废；没证据等到截止 + 11 分钟 |
| 玩家看到的 | 只有直连断开；补签一直失败；场景服解冻时什么也不推 |

缺省模板在开打第 1 分钟崩溃，玩家要到成局后约 7~20 分钟才能再排；长模板最长一个多小时。

提前不了的原因：

1. 取证只在截止 + 宽限之后才做。
2. 注册中心摘除很慢：节点 etcd 租约是 180 秒。
3. 生产推荐的 Agones 形态下，GameServer Pod 是 `restartPolicy: Never`。进程崩溃后 Pod 很快被删，原 IP 连上去只会超时，原有的两种证据都拿不到。
4. 作废不给场景服投递任何东西，场景服只能等冻结到期后自己来问。
5. 参战者没有「本局作废」的客户端契约：`eBattleOutcome` 没有作废值。

## 2. 目标与边界

- **G1**：原执行进程确认已不在时，约 1 分钟内写下这一局唯一的作废决定（ABORT）。
- **G2**：任何来源的 ABORT 落库后，参战者几秒内解冻、删锁、放回道具预留。
- **G3**：客户端收到明确的「本局作废」。

不做的：

- 不改截止作废的硬上限和场景服的续冻上限，它们继续兜底。
- 不做「租约过期就判死」：etcd 或 TiDB 抖一次，全服在打的局会一起作废。
- 整机失联、Pod 卡在 Terminating 时拿不到确定证据，仍走截止兜底（第 8 节）。
- 观战侧不变。

## 3. 总览

- **A 匹配服提前作废**（第 4 节）：补签问不到原实例时把这一局记为可疑 → 向原实例取证 → 同一个实例两次「已不在」的观察相隔不少于 30 秒 → 逐局在锁内写 ABORT。
- **B 作废送到场景服**（第 5 节）：写 ABORT 的同一事务里给每名成员写一份「作废结算」 → 走现有个人发件箱和收件箱 → 场景服短路应用 → 存档确认后解冻、删锁、推 `BattleEndS2C`。
- **C 客户端契约**（第 6 节）：`eBattleOutcome` 追加 `BATTLE_OUTCOME_VOID = 4`。

## 4. A：匹配服提前作废

代码：`go/match/internal/logic/global_instance_loss.go`（可疑表、取证循环、按实例的复核记录器）、`global_deadline_evidence.go`（一次观察）、`go/match/internal/authority/instance_gone.go`（锁内提交）、`go/match/internal/battlepods/`（查 Pod）。

### 4.1 什么时候开始怀疑

- **补签**：`globalRequestBattleTicket` 向原实例的查询失败，或回了空包，就把这一局记进本副本的可疑表。直连一断，参战者的客户端会立刻补签，在线玩家几秒内触发。
- **发房收尾**：`globalAbortPrepared` 没能证明原实例不会建房时同样记入。

可疑表在内存里，按 battle_id 去重，上限 16384。副本重启会丢：下一次补签或重登会再触发，最差回到截止兜底。两条发布轨道的副本都跑取证循环，因为全员金丝雀的对局只会向金丝雀副本补签。

不拿注册中心摘除当触发（180 秒太慢），也不做全量巡检（截止作废已经兜底）。

### 4.2 一次观察认什么

一次观察的结论是**实例级**的：战斗服的实例号每个进程生成一次，同一个实例上的各局看到的是同一个事实。

「这一次观察说原进程已不在」只认三种：

- **E1**：原地址上应答的是另一个实例（查询成功，但回了空包）。
- **E2**：原地址的 TCP 端口当场拒连，**并且**注册中心已同步、原实例已不在册。
- **E3**：K8s 里战斗服所在命名空间已经没有**存活**的 Pod 持有原 IP：按 IP 列出的 Pod 为空，或都处于 `Succeeded` / `Failed`（kubelet 已确认容器全部终止）。`Running`、`Pending`、`Unknown`、正在 Terminating 的都算存活。

原实例亲口应答（查询回显了它自己的实例号），不论这一局是什么状态，都说明它还活着，这时另外三样说什么都不作数。超时、黑洞、gRPC 报错、只是注册中心查不到，都算判不了。

和最初方案的两处不同，都是落码时为了「确定」收紧的：

- **E2 保留「已不在册」的要求**。只凭拒连不够：Windows 监听队列满、防火墙按 REJECT 配置时，活着的进程所在地址也会回 RST，而它还持有租约，玩家还连着它在打。代价是进程崩溃后不重启、Pod 又还在的情形（本机开发最常见）要等租约过期，约 3.5 分钟。重启过的进程走 E1，Agones 走 E3，都不受影响。
- **截止之前只做只读查询**（`queryOriginalInstance`）。截止作废用的那一次交互会先发 `DestroyBattle`，它给还没有房间的原操作立拒收墓碑；截止之前建房可能还在途，补签又是客户端随时能触发的，照搬会把本来建得成的战斗作废掉。

### 4.3 复核规则（唯一的时间判据）

- 同一个实例要有两次相隔不少于 **30 秒**的「已不在」观察（不必同种），期间它没有亲口应答，才算确认。期间只是没问到不清零。
- 复核按实例记（`instanceLossTracker`），同实例的各局共用：一台战斗服崩溃，它上面的几千局只需要两次取证。实例号不会复用，确认之后不再取证，其余各局直接作废。
- 30 秒要盖住原进程停止应答之后还可能提交终局的最长时间：战斗服停机先停听、再排空并补提交（`kShutdownDrainBudget`，15 秒），排空结束前发出的最后一次提交还有 10 秒调用期限（`kDefaultGrpcCallDeadlineMs`），余量 5 秒。常量是 `authority.InstanceGoneSettleMs`，`TestInstanceGoneSettleCoversBattleShutdownDrain` 读 C++ 源码核对。
- 取证节拍：复核排在第一次观察之后满 30 秒；到点没问到按 5 秒重试；一直判不了从 5 秒翻倍退避到 30 秒。

### 4.4 提交

- 取证在事务外，事务里只复核。`AbortInstanceGone` 沿用截止裁决的锁序（全员原 P → formation → 原票），锁内先复读终局，再核对原操作与原实例和证据一致、原件归自动路径裁决。
- 写入只走 `commitAbortTx`，终局来源是 `MATCH_BATTLE_COMPLETION_SOURCE_MATCH_INSTANCE_GONE_ABORT = 7`，计 `match_global_flow_total{stage="terminal",outcome="abort_instance_gone"}`。
- 截止之前之后都可以写。不要求工作租约：建房目标在提交点写定，战斗服只接发给自己实例号的建房请求，原进程不在就不可能再建成。

### 4.5 截止作废共用同一条复核

截止作废（到期恢复、帮会取消）的取证经同一个复核记录器：

- 实例已确认不在：不再取证，直接给出证据。
- 第一次观察到不在：回新的证据取值 `DeadlineEvidenceInstanceGonePending`。裁决上和判不了一样，只是下一次取证排在 32 秒之后（复核间隔 + 2 秒钟差余量），保证那一次一定过得了复核。
- 原实例亲口应答：清掉这个实例之前的观察。

改之前「拒连 + 已摘除」会立即作废，在战斗服重注册失败、排空补提交的 15 秒里可能抢先。

### 4.6 E3 的部署

- 清单 `deploy/k8s/manifests/go-svc/match-battle-pod-reader-rbac.yaml`：专用 ServiceAccount `match`，只限本命名空间的 pods `list`。`match.yaml` 的 Pod 模板写死 `serviceAccountName: match`，`k8s_deploy.ps1` 在 StatefulSet 之前 apply，所有档位都 apply。
- 配置 `GlobalMatch.BattlePodNamespace`：部署脚本按它部署战斗服的命名空间写进 ConfigMap 模板，服务自己的 yaml 不许写。留空就不查 Pod（本机开发）。写错成别的命名空间会让 E3 恒成立，所以只由脚本生成，有契约测试。
- **运维要把这个键抄进 match 的 `runtimeConfigSecret`**（常驻进程读的是它，ConfigMap 只是模板）。漏抄不会拒绝启动，进程会打一行 ERROR `battle pod lookup disabled`，E3 失效。
- 运维约束：不要强删战斗服 Pod，不要手工删 Node 对象。见 [人工 ABORT 手册](../ops/match-formation-manual-abort-runbook.md) §9。

### 4.7 各种情形的结论

| 情形 | 证据 | 结果 |
|---|---|---|
| 容器原地重启、或开发时重启了进程 | E1 | 首次观察后约 30 秒作废 |
| Agones 下崩溃 | E3（Pod 进 Failed 或被删） | 同上 |
| 崩溃后不重启、Pod 或主机还在 | E2，要等租约过期 | 约 3.5 分钟 |
| 正常停机（排空不超过 15 秒） | 排空中判不了，之后 E2 或 E3 | 复核时终局已落库或补提交已失败；有终局就不动 |
| 匹配服与战斗服网络分区，战斗服还活着 | 判不了 | 不作废，等截止兜底 |
| 整机失联，Pod 卡在 Terminating | 判不了 | 不作废，等截止兜底 |
| 有人强删 Pod，而进程还活着 | E3 | 作废（手册禁止强删） |
| 客户端自己网络差，误触发了怀疑 | 原实例应答 | 撤出可疑表，不作废 |

## 5. B：作废送到场景服

### 5.1 匹配服：所有 ABORT 都给成员写作废结算

- `commitAbortTx` 在同一事务里按成员写 `match_battle_settlement_source`（`void_settlement.go`），所以六个 ABORT 入口都生效：截止、帮会取消、没建成房、原件不合格、实例失效、人工处置。
- 载荷是作废结算：`BattleSettlementData` 只带 `battle_id`、`player_id`、`outcome = BATTLE_OUTCOME_VOID`、`player_team_index`、`finished_at_ms`、`match_mode`、`battle_config_id`。所在方优先取建房原件里的分边。
- 归属取同一事务里的原票。原票缺失（坏数据）的成员不写，打 ERROR，他的冻结由场景服到期查询兜底。
- 原票仍在 ABORT 事务里直接放回，不等作废入账。
- 现有个人发件箱只按行状态取件，作废行原样投递，没有另写投递逻辑。

### 5.2 收件箱服务与场景服

- **收件箱服务**（`go/player_inbox/internal/inbox/registry.go`）原先只收胜、负、平三种结局，作废结算会被永久拒收。现在收 VOID，条件是正文只出现上面七个字段。
- **场景服**（`PlayerBattleSystem::ApplyInboxSettlement`）在任何效果路径之前认出 VOID，走 `ApplyVoidSettlement`：
  - 正文出现七个字段之外的任何字段，或身份不符：整笔延后并打 ERROR `battle_void_settlement_malformed`，不当作废也不当结算。
  - 否则释放这一局的道具预留，写一条 VOID 近期战斗结果，返回已应用。
  - **绝不进 `ApplySettlementToEntity`**：那里把 `health` 当战后终值回写，作废正文里的 0 会把血量写成 0。
- 「不带任何效果」三处都按「只许出现哪些字段」判，不逐个列效果字段：以后给结算加新效果时，带着它的作废正文默认被拒。
- `CompleteInboxSettlement` 不改：存档确认后删锁、解冻、推 `BattleEndS2C`（outcome = VOID）、公开结果。
- 场景服冻结到期后的查询保留作兜底。

### 5.3 为什么复用结算链

- 至少一次投递、按（玩家, 战斗）去重、离线补投、与存档同一次提交，都是现成的。
- 战斗结算域每人最多一单在途，作废和结算排在同一条有序邮箱里。
- 另开一种消息要多一套生产、去重和告警。作废在业务上就是「这一局的个人结果 = 没有任何效果」。

## 6. C：客户端契约

- `proto/battle/battle_data.proto`：`BATTLE_OUTCOME_VOID = 4`。只由匹配服的 ABORT 产生，战斗引擎不产出；战斗服冻结终局、匹配服收终局时都拒收它。
- 客户端（`../mmorpg-client`，已改）：
  - 结算面板认出 VOID：标题「本局作废」，说明行代替奖励行。改之前会落进「失败」分支，还列出「HP 0 / MP 0」。
  - 没有战斗上下文时收到作废（重登后才送达）：不弹面板，提示一句。
  - 「无法连接战斗服务器」的三处文案改成中性，不再断言「本局由系统自动出手」。
- 客户端还没有近期战斗结果的界面，所以「列表里显示作废」没有可改的地方。
- robot：两个战斗冒烟把 VOID 判为失败（步骤 `a-battle-voided`）。改之前作废会被当成「收到了终局」而通过。

## 7. 改后的时间线

| 情形 | 改之前 | 改后 |
|---|---|---|
| 崩溃，参战者在线，E1 或 E3 | 崩溃后约 6~20 分钟（缺省模板） | 约 40~50 秒 |
| 崩溃后不重启、Pod 还在（E2） | 同上 | 约 3.5~4 分钟 |
| 崩溃，参战者都离线 | 同上 | 有人重登补签时触发；没人回来就和以前一样 |
| 截止作废、帮会取消、人工处置 | 作废落库后还要等冻结到期再来查 | 落库后几秒 |
| 整机失联 | 截止 + 11~17 分钟 | 不变 |

40~50 秒 = 补签触发几秒 + 复核 30 秒 + 取证与作废事务几秒 + 发件箱节拍不超过 2 秒 + 收件箱与存档确认几秒。

## 8. 剩余风险

- 整机失联、Pod 卡在 Terminating：仍等截止兜底。
- 强删 Pod 或手工删 Node：靠手册约束。
- 原 IP 被另一个存活的 Pod 占用时 E3 判不了，交给 E1、E2。
- 战斗结算域每人一单在途：上一局的结算还在延后时，本局作废要排在它后面入箱。
- 可疑表与复核记录在内存里：副本重启就丢，只会晚，不会早。
- 误判时（两次观察都错）：原实例之后提交的结果会被拒收，这一局白打，数据仍然一致；复核记录器会打 ERROR `answered after it was confirmed gone`。

## 9. 改动清单

- **proto**：`battle/battle_data.proto`、`match/match_authority.proto`。
- **go/match**：
  - 新增 `authority/void_settlement.go`、`authority/instance_gone.go`、`logic/global_instance_loss.go`、`battlepods/`、`svc/battle_instance_loss.go`；
  - 修改 `authority/terminal_abort.go`、`logic/global_deadline_evidence.go`、`logic/global_match.go`、`config/config.go`、`svc/servicecontext.go`、`match_service.go`；
  - 测试新增四个文件，修改九个。
- **go/player_inbox**：`inbox/registry.go` 与测试。
- **C++**：`scene/battle/system/player_battle.{h,cpp}`、`battle/system/result_commit.h`、`tests/bag_test/player_battle_settlement_test.cpp`。
- **robot**：两个战斗冒烟。
- **部署**：新增 RBAC 清单，修改 `match.yaml`、`k8s_deploy.ps1`、两个契约测试。
- **客户端**：`BattleClient.cs`、`BattleResultPanel.cs`、`BattleScreen.cs`、`BattleUiRoot.cs`、`BattleClientStateMachineTests.cs`。

## 10. 定案（负责人 2026-11-11）

1. 给匹配服加战斗服所在命名空间的只读 Pod 权限，启用 E3。
2. `eBattleOutcome` 追加 `BATTLE_OUTCOME_VOID = 4`，客户端同批改。
3. 截止作废同步改用 4.3 的复核规则。
4. 复核间隔 30 秒，取证节拍 5 秒。

## 11. Java 版

待做，已登记在 Java 仓 `PARITY.md`：客户端契约（VOID）必须同批；作废结算、收件箱校验、场景侧短路、按实例复核的提前作废按 Java 惯用方式实现。

## 12. 验证（全部未执行，交 Codex）

先生成协议，再按顺序跑。任何一步失败保留完整输出，不要跳过后面的步骤之外的无关项。

1. **协议生成**（仓库根）：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。客户端要一起生成（`eBattleOutcome.BattleOutcomeVoid`），用默认配置；只验服务端时可加 `-ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
2. **go/match**（工作目录 `go/match`）：
   - `gofmt -l ./internal/authority ./internal/logic ./internal/battlepods ./internal/svc .`（本批文件不得列出）；`go vet ./...`；`go build ./...`。
   - 纯单测：`go test ./internal/battlepods/ ./internal/logic/ . -count=1 -v -run "TestGone|TestApiFailures|TestLookupWithout|TestInstanceLoss|TestCachedPodGone|TestClassify|TestInstanceSightingProbe|TestDeadlineEvidenceProbe|TestOriginalAddressRefuses|TestAskOriginalInstance|TestBattleRegistryOf|TestMain"`。
   - `go test ./internal/authority/ -count=1 -v -run "TestJudgeDeadlineAbortTable|TestInstanceGoneSettleCoversBattleShutdownDrain|TestDeadlineAbort|TestVoidSettlementFor|TestMemberSides"`。
   - 真 TiDB（设 `MATCH_IT_TIDB`、`MATCH_IT_REQUIRE_TIDB=1`）：`go test ./internal/authority/ -count=1 -v -run "TestInstanceGoneTiDB|TestAbortTiDB|TestTerminalTiDBAbortAndCompleteHaveOneWinner|TestAbortWithoutRoomTiDB|TestCommitBattleOutcomeTiDB|TestManualAbort|TestRejectedOriginal|TestReadyTiDB"`，要求 0 跳过。
   - 最后 `go test ./... -count=1`，并加一轮 `-race` 跑 `./internal/logic/`。
3. **go/player_inbox**（工作目录 `go/player_inbox`）：`go vet ./...`；`go test ./internal/inbox/ -count=1 -v -run "TestVoidBattleSettlement|TestBattleOriginal|TestBattleSettlementRegistry"`；再 `go test ./... -count=1`。
4. **robot**（工作目录 `robot`）：`go build ./...`；`go test ./... -count=1`。
5. **C++**（Debug x64，串行 `/m:1`）：先 `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`（改了库，必须先重编库），再 `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build`；重点看 `bag_test.exe --gtest_filter=PlayerBattleSettlement*:BattleFreezeHold*`，其中新用例名含 `VoidSettlement`。
6. **部署契约**：`pwsh -File tools/scripts/tests/k8s_migrate_gate.tests.ps1`、`pwsh -File tools/scripts/tests/lobby_authority_render.tests.ps1`、`pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`。
7. **客户端**：Unity EditMode 跑 `BattleClientStateMachineTests`（新用例名含 `Void`）与战斗相关用例；作废态的结算面板没有效果图，沿用现有面板布局，视觉验收另算。
8. **演练**（本机）：
   - 起一局战斗后 `kill -9` 战斗服并立即重启：约 1 分钟内两个参战者都收到 `BattleEndS2C{VOID}`，之后能再排队；匹配服日志有 `confirmed gone` 与 `aborted early`。
   - 杀掉后不重启：约 3.5~4 分钟（等租约过期）。
   - 正常停机（Ctrl+C）时在打的局：不得在排空的 15 秒内被作废。
   - K8s：Agones 下删一个在打的 GameServer，核对 E3 生效；另核对 match 日志没有 `battle pod lookup disabled`。
