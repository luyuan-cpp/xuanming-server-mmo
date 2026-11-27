# 数据覆写审计 r2·换视角查找第 4 轮：复核与修复交接（2026-11-26，交 Gemini）

审计文档：[data-overwrite-rollback-audit-r2-20261019.md](../design/data-overwrite-rollback-audit-r2-20261019.md)（§1「仍开着」、§3.12、§5.0）。r2 前 3 轮与批 S、T、U 的验证另见 [data-overwrite-audit-r2-handoff-20261126.md](data-overwrite-audit-r2-handoff-20261126.md)。

## 0. 为什么交 Gemini、做到哪了

- 第 4 轮由 Claude 会话（Fable 5.2 Max）跑：三路查找已返回，主会话逐行补了证据，定了修复方案（§3）。9 条 high / medium 的对抗复核刚开跑，Claude 的 Fable 周额度就用完了（12-03 20:00 UTC 才重置），**复核没跑完，代码一行没改**。
- 负责人 11-26 的口径：额度不够就交 Gemini。AGENTS §10.3 要求改代码和漏洞分析用最高档模型，所以本单的复核与改码用 **Gemini 4 Neon Extra**；协议生成、编译、测试、冒烟照常由 Gemini 跑。
- **更正（同晚）**：负责人把第 4 轮会话切到 Opus 5.5 后让它「开始做」，会话在收到本单之前已派出 3 个 **Opus 5.5 Max** 子代理落批 V（帮战）、批 W（锦标赛领奖与 match 发件接线）、批 X（生死状银两 scene 兜底），各自同批写设计稿。停在半截比做完更难收拾，所以让它们做完。它们不是 §10.3 指定的最高档，**Gemini 先用 Neon Extra 逐批复核这三批的 diff**：对的保留，错的改，不该改的回退；然后才生成、编译。三批的改动文件、结论与验证命令补在 §7（三批回来后补）。批 V、W、X 之外的各条仍按本单从复核开始。
- **再更正（同晚，负责人原话「改由 Claude 写」，在会话「交接单里的代码改动清零与方案入档」里定）**：本单原来交给 Gemini 写的代码改由 Claude 写。批 V、W、X 仍归第 4 轮会话；其余 9 条（r2-jh#3、r2-craft#1～#4、r2-mall#1、r2-ops#1、r2-ops#2、r2-oa#1）与 V、W、X 回来之后的逐批复核，由「交接单里的代码改动清零与方案入档」会话做，分工以两个会话确认的为准。**Gemini 不要改本单的代码**，只做生成、编译、测试、冒烟。
- 审计基线 HEAD `bc6e70850`，审计期间别的会话推进到 `2d20f6b68`，交接时 HEAD 是 `8a6697af0`。行号可能已漂移，按函数名定位。
- 三路查找原文在本目录旁的 [data-overwrite-audit-r2-round4-20261126/](data-overwrite-audit-r2-round4-20261126/)（`asset-crosszone-idseg.json` 跨区与号段、`fault-partition-lease.json` 网络分区与租约、`entry-ops-cli.json` 运维工具）。每条的触发步骤、证据和 `fix` 建议以原文为准。

## 0a. 换电脑后继续：本单全部剩余工作（负责人 11-27 定）

> **11-27 起入口改为 [r2 剩余单](data-overwrite-audit-r2-remaining-handoff-20261127.md)**：那份把本节和各节散落的待做合成了一张清单，也写了做完后怎么删单。本节保留作对照，两边不一致时以那份为准。

> 全部交接单的总入口是 `docs/handoff/new-computer-start-here-handoff-20261127.md`（11-27 补）：先过它 §1 的开工核对；r2 这条线在它 §3 第 1 行，入口是上面那份 r2 剩余单。

负责人在另一台电脑上说「继续 handoff 里面所有东西」时，那边的 Claude Code（AGENTS §10.3 最高档 Fable 5.2 Max）按本节从头做到尾。**本单里凡写「交 Gemini 写」的代码，一律改由 Claude Code 写**，Gemini 只跑生成、编译、测试、冒烟。只在 main 上改，不开分支，提交由负责人定；Java 独立仓只在 `PARITY.md` 登记待做。

**旧电脑停在哪、新电脑怎么开工（2026-11-27）**：
- 旧电脑上的第 4 轮会话停在额度线上：Fable 周额度用完，全部模型周额度 84%，过了 §6 的 80% 停止线。原账号要到北京时间 12-04 04:00（12-03 20:00 UTC）才重置。
- 停下时还没做的：复核已落码的几批，写悬赏求书剩下的批 C、批 B，修其余各点，也就是下面第 1～4 步。第 5 轮查找等第 4 轮修完再开，即第 5 步。
- 负责人 11-27 定：换电脑、换账号后说「开始」或「继续 handoff」，就从第 0 步做起，不用等原账号重置。新账号照样守 80% 停止线：过线就停在一批的边界上，把没做的写回本节。
- 旧电脑要回收，那边的会话、笔记和 Claude 记忆都带不过来，接手只认本仓文档。上面的进度来自第 4 轮会话的报告，没有逐个文件核对；开工以第 0 步的 git 核对和各节结论为准。

0. **开工核对**：`git log` 里要有 `6b4ae7852`（11-27 旧电脑的 main 与 origin/main 都停在这里）；三批与 §7.4 的代码 `3eb57a09a`、`d77a458ee`、`76e2b59bb`、`24c13a311` 都在它之前。§6.1、§7 各节标「未提交」的测试与设计稿，11-27 核对已全部随后来的快照提交进了 main（最晚 `bcea63be6`），按已提交读。缺了就停下问负责人。`git status` 看清别的会话的在途改动，只动本单范围的行。各节末尾已写了复核结论或「已完成」的，不重做。
1. **复核**（写的人与复核的人分开；新电脑上的会话没写过这些代码，可以复核）：按 §2「先复核，再改」逐批复核 §7.1 批 V、§7.2 批 W、§7.3 批 X、§7.4 五条、§6.1 九条里还没有复核结论的。批 X 已复核通过（§7.3 末条），不重做。diff 基线：批 V 用 `git diff 8a6697af0 a0b3d195e -- <§7.1 文件>`，批 W 用 `git diff 8a6697af0 HEAD -- <§7.2 文件与测试>`，§7.4 用 `git diff a0b3d195e HEAD -- <§7.4 文件>`。终点必须到 HEAD：当时没提交的测试与设计稿，是后来的快照提交带进来的；只看 `76e2b59bb`，或止于 `24c13a311`，都会漏。区间里混着别的会话对同一文件的改动，按 §6.1「还没做的」里复核那条的说明分开看。复核出问题当场改，在对应小节末尾写复核结论。
2. **写代码**（按顺序）：
   1. r2-craft#1 批 C（部署开通）、批 B（scene 与客户端接入）：**入口改为单独的交接文件 `docs/handoff/skill-book-wanted-order-remaining-handoff-20261127.md`**（自成一体，含批 A 的 Gemini 验证、两批的逐文件改法与完成判据，做完删掉它）；做法与前提在 `docs/design/skill-book-wanted-order-trade-escrow.md` §11.2；批 C 前提②（K8s 各档位是否打开 trade `AssetOp.Enabled`）开工前先问负责人，不自己定。
   2. guild 人工终结见证的条件写补比 `attempts`：逐行步骤在 §6「还没做的」第 2 条。
   3. 各批「剩余与取舍」「顺带发现」里记下的点，按「查出的问题现在修」逐条判断：是缺陷就修；确是有意取舍的，在所属设计稿写明不改的理由。至少包括：
      - 删角收尾不删 `guild_war_player_state` 与 `guild_war_garrison`（后者含玩家名与快照），补进账号注销清单；`docs/design/account-deletion.md` 登记新挂起项 `guild_war_credit_owed`（§7.4）。
      - 客户端可见的 `guild.proto` 里 EXPIRED 状态注释与实际含义不符（§7.4）；客户端 `BountyStatusLabel` 没有状态 6 的文案（§7.1）。改客户端可见协议时两版共享的契约同批改。
      - 待入账积压（`guild_war_credit_owed`）没有指标与告警，生死状待退银两超期（`metric=jianghu_duel_fee_overdue`）没有告警规则（§7.3、§7.4）。
      - `CheckJianghuDuelEligibility` 不判「旧局待退银两未结清」，要到备战才拒（§7.3）。
      - 锦标赛领奖遇邮箱满时玩家看到的是内部错误，mail 的满箱提示没透传（§7.2）。
      - 寄售交付腿部分发放后，trade 侧没有出口（mall#1 残余，补法在 `jubaozhai-market.md` §16.1）。当时要改的 `manual_plan.go` 与双人复核会话的改动冲突，所以没做；11-27 核对，该文件已没有别的会话的未提交改动（§6.1「还没做的」）。
3. **文档**：每改一条，按 §4 在所属功能的设计稿写五项（问题、方案、为什么这样选、被否决的做法、代价与边界），不容易想到的好方案一定写进去；r2 审计文档 `docs/design/data-overwrite-rollback-audit-r2-20261019.md` 的 §5.0 补全第 4 轮各编号的最终状态、§1a 补入口、§1「仍开着」划掉第 4 轮；`docs/PROGRESS.md` 纯追加；新建文档后按 AGENTS §5 更新文档索引（先 `--check`，diff 只许加行）。
4. **交 Gemini 只跑**：生成、编译、测试、冒烟，命令在 §6.1、§7.1～§7.4 与设计稿 §11.1、§11.2；红了保留首错原文交回 Claude Code，不由 Gemini 改代码。
5. **第 5 轮审计**：以上都落完后开，做法见 §6（`D_ROUNDS[4]` 自由视角三路，并进旧核心机制的网络分区推演，去重清单 133 条）；审计脚本与旧去重清单用 `git show ccf2840b2:docs/handoff/data-overwrite-audit-r2-20261019/<文件>` 读。§6.1「还没做的」最后一条记了两处线索，这一轮顺带看。
6. **别漏另一处**（11-27 补）：选角页回选上限的用例（原 review-suggestions 交接单第 8 节）已移到总入口 `new-computer-start-here-handoff-20261127.md` §3.1，不依赖本单代码，在第 5 步之前做完即可。review-suggestions 交接单第 4 节第 1 条剩下的 guild 条件写 SQL 就是上面第 2.2 步，别做两遍；那份其余各节代码都已写完，只交 Gemini 验证。
7. **r2 前几轮的验证交接**（11-27 补）：`docs/handoff/data-overwrite-audit-r2-handoff-20261126.md` 排了 r2 前 3 轮各批（含批 S、T、U）的验证顺序与通过标准。它没有要写的代码，只交 Gemini 跑。最好等本单第 1～4 步落完、审计 §1「仍开着」清空之后，再整体跑一遍。

## 1. 发现（17 条：3 high、6 medium、8 low，几乎都在 11-20 以后的新玩法代码）

「已核」= Claude 主会话逐行读过代码路径属实；可达性与上游守卫还要对抗复核。

| 编号 | 级 | 问题 | 位置 | 状态 |
|---|---|---|---|---|
| r2-gw#1 | high | 帮战追杀令结单不核对被击败方：`bounty_id`、`leyline_id` 取自客户端。任意帮战带别人的追杀令号打赢即领托管赏金；带任意灵脉号可削第三方灵脉的旗、拿破旗赏金 | `go/guild/internal/logic/war_logic.go`（开战入口、结算第 7 步、灵脉结算）；`go/guild/internal/data/war_repo.go`；`go/guild/internal/war/rules.go` | 已核 |
| r2-gw#2 | high | 追杀令托管银两只进不出：到期不退、撕单另一半不退；赏金指令在结算事务之外，失败被吞、重投不补；领赏人非成员或当日满上限时不发也不退 | 全仓对 `guild_war_bounty` 的写只有 `war_repo.go` 两处与 `asset_store.go` 一处；`war_logic.go` 的 `EnqueueBountyPayout` | 已核 |
| r2-gw#3 | medium | 灵脉旗势事务外读、事务内按绝对值盲写；两副本并发结算时丢扣减、破旗赏金发两次、一帮可占两座 | `war_logic.go` 灵脉结算；`war_repo.go` 灵脉写 | 写法已核，并发模型待复核 |
| r2-gw#4 | low | 帮战发号失败时用墙钟自造持久 ID（追杀令、约战、开战请求号），违反 AGENTS §7 第 1 条 | `war_logic.go` 三处发号 | 一处已核 |
| r2-tn#1 | high | 群仙问道赛领奖：发件未装配时跳过发件，照常把奖励置 CLAIMED；部署没给 match 注入 `MAIL_PRODUCER_SECRET_MATCH`，mail 的 Producers 没有 match | `go/match/internal/logic/tournamentlogic.go`；`go/match/internal/authority/tournament.go`；`go/match/internal/svc/servicecontext.go`；`go/mail/etc/mail.yaml`；`go/mail/internal/data/repo.go` | 已核 |
| r2-tn#2 | medium | 领奖邮件 ExpireMs 取当前时刻、标题正文取当前配表；邮件已写而标记没落定后重领，摘要不同，mail 回冲突，奖励永远停在可领取并挡住同批后面的奖励 | `tournamentlogic.go`；`go/mail/internal/data/repo.go` | 已核 |
| r2-jh#1 | medium | 生死状普通战书预扣的 500 银两没有终局兜底（金帖走道具预留有复核）：取消没送到在线实体就不退；下一局预扣直接覆盖旧标记 | `cpp/libs/services/scene/battle/system/player_battle.cpp`（预扣、取消、复核）；`cpp/libs/services/scene/player/system/player_jianghu.cpp` | 已核 |
| r2-jh#2 | medium（发件接线后可达，现缺省不可达） | 退费有 match 发邮件与 scene 在线退两条通道，邮件结果不明时删标记再让 scene 退，双退；有原件分支固定 skip=false | `go/match/internal/logic/jianghu_duel_logic.go`；`gather.go`；`global_match.go` | 已核 |
| r2-jh#3 | low | 帮派恩怨墙与个人宿敌录用会被裁到 20 条的战报行当去重锚点，旧结果重投重复计数 | `go/guild/internal/data/jianghu_grudge_repo.go`；`go/match/internal/authority/jianghu_grudge.go` | 未核 |
| r2-craft#1 | medium | 悬赏求书揭榜同一帧改两名玩家、各自存盘；只落一边时银两或技能书多一份 | `cpp/libs/services/scene/player/system/player_craft.cpp` 揭榜 | 已核 |
| r2-craft#2 | low | 书架 skill_books 无条数上限（与「玩家存档里的集合要有上界」冲突） | `player_craft.cpp` | 未核 |
| r2-craft#3 | low | 名匠代打分成邮件收件人是别的玩家，邮件删除模式下 37 天后判重行被清、重寄成复制（`Sweep.Mode` 缺省 report_only，不可达） | `player_mail_outbox.cpp`；`go/mail/internal/data/sweep.go` | 未核 |
| r2-craft#4 | low（存疑） | 名匠口碑离线回账不幂等（只影响口碑展示） | `player_craft.cpp` | 未核 |
| r2-mall#1 | medium（门控） | 商城人工处置：寄售交付转人工而付款仍 PAID 时只有 RETURN_TO_SELLER 能过，不建退款就把订单终结，trade 又拒绝退回 | `java/mall_game/.../ManualAdminStore.java`；`go/trade/internal/commerce/rmb.go` | 未核 |
| r2-ops#1 | low | `table-bundle rollback` 的永久表丢行检查缺省不执行 | `go/table_sync/cmd/table-bundle/main.go` | 未核 |
| r2-ops#2 | low | 热关停受保护名单不含 go/db 存盘与认领 | `go/db/db.go`；`docs/design/rpc-kill-switch.md` §8 | 未核 |
| r2-oa#1 | low | 线性一致归属权威 enforce 模式准入屏障不生效（没人续实例租约、实例身份是常量；缺省不启用） | `go/scene_manager/internal/logic/owner_authority_bridge.go` | 未核 |

覆盖缺口：按故障一路把力气花在新代码上，旧核心机制（player_store 写权、handoff_fence、发号器、写者名册、shardowner）的分区推演与 muduo 半开连接这轮没重做。

## 2. 先复核，再改

1. 每条 high / medium 先按查找原文的触发步骤逐行走一遍：入口能不能被客户端或别的服务触发、上游有没有已经挡住的守卫、缺省配置下是否可达。成立就按 §3 改；不成立的写明理由，在 §3.12 标「不成立」。
2. low 各条看成立与否；成立且改法小的同批改，改法见原文 `fix` 字段；不改的写明理由。
3. 一批一批改，改完一批再复核下一批，别同时动多批共用的文件。

## 3. 修复设计（Claude 主会话定，按「最标准」）

### 批 V：帮战（go/guild），含 r2-gw#1～#4

1. 开战入口：`leyline_id` 只有打守旗阵（含一键开战回落到守旗阵）时用客户端给的值并照常校验；野外直打按目标所在场景推，其余一律 0。`bounty_id` 只有 BOUNTY 分支用客户端值；直打活人只由服务端按目标自动挂钩；其余 0。挂钩与 BOUNTY 分支都核接单资格（公开榜或发单帮）与区服。
2. 结算第 7 步在追杀令行锁内再核：本局防守方真人含 `TargetPlayerId`、区服一致、攻方有接单资格、status=2 且未过期；不满足就不动追杀令。
3. 赏金与退款指令并进结算事务（指令号事务前发好，`SettleWarBattleInput` 里已留 `BountyPayoutOpID` / `BountyPayoutBundle`），删掉事务外的 `EnqueueBountyPayout`。撕单：一半给目标、一半退发布者，同一事务。领赏人当日满上限或已不满足入账条件：不结单（追杀令保持 ACTIVE），不销毁托管。
4. 到期收尾：status=2 且过期 → 置过期并给发布者建退款指令，同一事务，行锁 + 状态 CAS 保证多副本幂等；挂 guild 现有周期任务。新循环要登记循环活性契约测试。公费档退回帮会资金。
5. 灵脉：事务外读照旧；事务里按 `leyline_id` 升序锁本区灵脉行，与事务外读到的快照（主人、旗势、护盾、是否已占他脉）逐项比对，不一致回写冲突、整笔重算重试（有界）。不改锁序。
6. 发号失败一律回 `ErrIDGenUnavailable`，不用墙钟。

### 批 W：锦标赛领奖与 match 发件接线，含 r2-tn#1 / #2

1. 领奖：有附件而发件未装配 → 回可重试错误、保持 CLAIMABLE；无附件的荣誉奖励可直接落已领取。
2. 邮件请求确定化：ExpireMs 传 0（mail 首次落库时按 30 天算）；标题正文在结算时冻结进奖励行（要加列与 proto，按 AGENTS §4 生成），或先用固定文案。单条失败不中断循环。
3. 部署接线：`tools/scripts/k8s_deploy.ps1` 给 match 注入 `MAIL_PRODUCER_SECRET_MATCH` 与 MailRpc，mail 的 Producers 加 match（yaml、manifest、密钥清单、契约测试一起改）。非 dev 档锦标赛或江湖恩怨开着而发件没装配时启动即拒。

### 批 X：生死状银两，含 r2-jh#1 / #2

1. 退费只留 scene 一条通道：删 match 的邮件退费（`refundJianghuSilverOnce`、退费标记表的读写），取消一律 skip=false。
2. `PlayerJianghuComp` 加 `pending_silver_fee_review_after_ms`（= 战斗期冻结到期时刻）。reaper 的预留复核把「冻结已不在、复核时刻已过的待退银两」一并送 `SendTrackedTerminalQuery`，回答 ABORT / NO_ORIGINAL 才退（与道具预留同一判据 `IsBattleHoldReleasable`）。同一（玩家，局）只发一次查询，回调同时处理道具预留与银两。
3. `CommitPrepareSilverFee` 遇到别的局的未结清标记 → 拒绝备战（可重试），不覆盖。`OnBattlePrepareCancelled` 先入账成功再清标记，入账失败保留标记。
4. 新持久字段走字段保护台账流程（[混版本持久字段保护](../design/mixed-version-persist-field-protection.md)）。

### 其余

- r2-craft#1：没有小改法。两名玩家各存各的盘，原子交换只能靠托管方。**负责人 11-26 定：改走 go/trade 托管合同**，顺带解决悬赏榜只看得到同场景节点的玩家。
  - 做法：发榜时把悬赏银两托管进 trade，揭榜时托管技能书，两条发放腿由 trade 交付；沿用 go/trade 现有托管合同的写法，不另造一套。
  - 顺序：负责人 11-26 定由 Claude（Opus 5.5）写，第 4 轮会话在批 V/W/X 之后接着做。先出技术设计，写进 `docs/design/` 下的设计稿（没有就新建；玩法口径有变时同步 `docs/planning/equipment-quality-and-skill-crafting-v2-v5.md`），再落码。
  - 客户端可见的协议要改的话，两版共享的契约同批改，Java 版只登记待做。
- r2-mall#1：RETURN_TO_SELLER 只在付款已撤销时放行；付款仍 PAID 时同事务建退款待办，或拒绝；补寄售「已交付」的收敛动作。Java 商城代码属本仓 `java/`，照改。

## 4. 文档要求（负责人 11-26，强制）

- 每修一条，在**对应功能的设计稿**里写五项：问题、方案、为什么这样选、被否决的做法、代价与边界。重要流程和设计步骤也写，尤其是不容易想到的好方案。不能只写在 PROGRESS 或代码注释里。帮战追杀令、生死状、锦标赛领奖、悬赏求书各自的设计稿在 `docs/planning/` 或 `docs/design/`，先 grep 找到再写。
- 改完代码同批写那一节，不留到最后补。
- r2 审计文档：§3.12 写第 4 轮的发现与复核结论；§5.0 加各编号的最终状态；修完后在 §1a「设计要点速览」各补一行入口，并在 §1「仍开着」里划掉第 4 轮那项。
- `docs/PROGRESS.md` 纯追加一条，写清改了什么、验证命令与结果。

## 5. 约束

- 只在 main 工作区改，不开分支。提交由负责人定。
- 工作区有很多别的会话的未提交改动，只动本单范围的行，改前重读。
- 日志文案英文，注释与文档中文；tip 码只能经 `data/tip/Tip.xlsx` 导表；改了 proto 按 AGENTS §4 生成并同步客户端仓。
- C++ 改动先跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`；不新起线程（AGENTS §9）。
- 新加 match 后台循环要同批登记 `loop_liveness_alerts_contract.tests.ps1` 的点位名单与节拍表。
- Java 独立仓（`luyuan-java/xuanming-server-mmo`）不写代码，只在它的 `PARITY.md` 登记待做；不适用或已有等价时不加行。

## 7. 批 V、W、X 的落码结果（2026-11-26 晚，Opus 5.5 Max 子代理；未生成、未编译、未测试）

三批都写完了。逐批复核由「交接单里的代码改动清零与方案入档」会话做（写的人与复核的人分开），复核通过后才交 Gemini 生成、编译、测试。

**提交状态（重要）**：三批的大部分代码不是本会话提交的，是别的会话的快照提交在三批写到一半时一并带进了 main：`3eb57a09a`（22:13，只带进批 X 的一半，单独编不过）、`d77a458ee`（22:16）、`76e2b59bb`（22:30，带进批 V 与批 W 当时已改的文件，并顺带重新生成了 match_authority 的产物）。`git diff HEAD` 看不到这些改动，复核按批看：批 V、X 用 `git diff 8a6697af0 -- <文件>`，批 W 用 `git show 76e2b59bb -- <文件>` 加工作区未提交部分。三个代理都核过 HEAD 与自己的最终内容一致。**HEAD 现在必须先生成协议才编得过**。

### 7.1 批 V：帮战追杀令与灵脉（r2-gw#1～#4 都成立）

- **结论要点**：r2-gw#3 的并发模型核实成立——结果消息 key 是 battle_id（`go/match/internal/svc/servicecontext.go`），同一灵脉的几局落不同分区；guild 各副本同一消费组 `guild-trial`、默认 2 副本，只在单进程内串行。顺手修了同类：FIELD_PLAYER 在非灵脉图上不清零客户端灵脉号，两个外帮在别人灵脉图上打架会改第三方的旗。
- **改法**（与 §3 批 V 一致，细节见设计稿）：
  - 开战入口 `clientWarTargetHints`：灵脉号只在打守旗阵时采信，追杀令号只在追杀榜直打时采信；自动挂钩与追踪都过 `BountyHuntable`（区服、接单资格）。
  - `SettleWarBattle` 改成「普通读定计划 → 按锁序转账 → 锁内复核」：第 4 步按升序点锁本区三座灵脉比对快照，第 7 步点锁追杀令重算，不符回 `ErrWarSettleStale`，逻辑层整笔重读重算最多 3 次；旗势只在防守帮就是灵脉主人时才变。
  - 领赏与撕单退款指令进同一事务（撕单另一半：银两档退发布者，公费档按实扣资金退回帮会），领赏次数锁内占用，满上限不结单；删掉 `EnqueueBountyPayout`。
  - 到期收尾 `ExpireBounty` / `ListDueBounties`，挂在待入队物品循环上（约每分钟一轮，15 分钟宽限给晚到的结算先落定），没新起循环。
  - 扣款失败改置新状态 6（ESCROW_FAILED），状态 5 只表示「到期且已退回」。
  - 三处发号失败回 `ErrIDGenUnavailable`，不用墙钟。
  - 交易类型复用 `TX_GUILD_BOUNTY_PAYOUT`（C++ 白名单已放行），C++ 不用改；没新增 tip 码。
- **改动文件**：`go/guild/internal/war/{rules.go,rules_test.go}`、`go/guild/internal/data/{war_repo.go,war_repo_test.go(新,真库),asset_store.go}`、`go/guild/internal/logic/{war_logic.go,war_logic_test.go,trial_background.go}`、`proto/guild/guild_db.proto`（`GuildWarBountyRecord.escrow_guild_funds = 18`、索引 `status,expires_at_ms`）、`proto/guild/guild.proto`（`GUILD_WAR_BOUNTY_STATUS_ESCROW_FAILED = 6`，**客户端可见**）。
- **设计稿**：`docs/planning/guild-play-v2-implementation-plan.md` 末尾定案一节（四个问题各五项、两条事务的完整步骤）；`docs/planning/guild-play-v2.md` 正文同步。两份文档未提交。
- **验证（Gemini，仓库根，串行，保留首错）**：
  1. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`（生成之前 Go 编不过）。
  2. guild.proto 客户端可见：在 `D:\luyuan\mmorpg-client` 依次跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`，0 错误。
  3. `cd go/guild; go build ./...; go vet ./...`；仓库根 `python tools/scripts/check_log_text_english.py`。
  4. `cd go/guild; go test ./internal/war/... ./internal/logic/... -count=1` 全 PASS。
  5. 真库：设 `GUILD_TEST_MYSQL_DSN`（库名 guild_test），`go test ./internal/data -run "GuildWarBounty|SettleWarBattle_|ExpireBounty_" -count=1 -v` 全 PASS、不得 SKIP；再跑 `go test ./internal/data -count=1` 与 `go test ./... -count=1`。
  6. schemamigrate 不给已存在的表补新索引：本地已有的 `guild_war_bounty` 要重建（未上线，可清）。
  7. 复核 diff 时排除两份别人的未提交改动：`go/guild/internal/kafka/trial_result_consumer.go`、`go/guild/cmd/assetopfix/main.go`。
- **剩余与取舍**：开战入口没有完整请求路径的集成用例（只靠纯函数）；客户端 `BountyStatusLabel` 没有状态 6 的文案（列表只显示 1、2，影响小）；灵脉主人帮的成员主动点外帮，打赢打输都不动旗势（旧代码会削自己的旗）；到期退款最晚到期后约 16 分钟才发。
- **复核（2026-11-27，会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」，Opus 5.5，只读代码，未编译）：通过，没有要改的。**
  - 读的是 `git diff 8a6697af0 HEAD` 下 §7.1 的源文件与两份 proto；HEAD 上的 `SettleWarBattle`、`ExpireBounty`、`PurgeTerminalBounties` 整段按锁序逐步读。§7.4 叠在同一批函数上的改动一并读，结论见 §7.4 末条。
  - r2-gw#1：`clientWarTargetHints` 只在守旗阵采信灵脉号、只在追杀榜直打采信追杀令号；野外直打按目标场景推灵脉，直打活人只由服务端挂钩。`BountyFacts` 全部取自开战冻结的上下文与结算消息（攻方帮、区服、双方首名真人、对局结束时刻）。`ResolveBounty` 要求追杀局、同区、有接单资格、ACTIVE、按对局结束时刻未过期、防守方首人就是目标。旗势只在防守帮就是灵脉此刻主人时才变。
  - r2-gw#2：领赏、撕单退款与结单在同一事务里；指令号事务前预发，发号失败整局交给重投。领赏人当日满上限或已删除时整笔不转移，追杀令保持 ACTIVE，托管不销毁。到期收尾在行锁内复核 ACTIVE、到期、发布者与退款额，多副本之间、与结算第 7 步之间都是先提交者算数，只退一次。扣款作废改置 6，状态 5 只表示到期已退。发单时已挡「同一目标同时只有一张未结追杀令」。
  - r2-gw#3：第 4 步按 leyline_id 升序点锁本区全部灵脉，比对主人、旗势、护盾与「攻方是否已占别脉」四项快照，不符回 `ErrWarSettleStale`，逻辑层整笔重读最多 3 次再交给重投。灵脉的另外两个写者不会被覆盖：解散释放按 `owner_guild_id` 条件写，分红只写分红三列。
  - r2-gw#4：开战请求号、追杀令号、约战号三处发号失败都回 `ErrIDGenUnavailable`，不落墙钟。
  - 重投与重跑：对局行 INSERT 是幂等闸门，两个副本并发结算同一局时后者撞主键、整笔回滚；`inTx` 遇 1213 重跑闭包时，输出与 `rec` 的两列都从原值开始。
  - 协议：`escrow_guild_funds = 18`、状态 6、两个新索引都是新号，没有复用；新表都不进客户端。
  - 剩余与取舍照上一条，不另加。

### 7.2 批 W：锦标赛领奖与 match 发件接线（r2-tn#1、r2-tn#2、r2-jh#2 match 侧都成立）

- **结论要点**：另核到两处缺口——K8s 的 match ConfigMap 模板根本没有 `MailRpc`；mail 的 gRPC 准入名单没有 match（只接密钥的话，开了 `-NetworkPolicies` 照样连不上）。`jianghu_duel_logic_test.go` 原来把「超时就让 scene 再退」写成了断言。
- **改法**：
  - 领奖拆成 `claimTournamentRewards` / `claimOneTournamentReward` / `tournamentRewardMailRequest`：带附件而发件没装配回 `kMatchInternal`、行不动；纯荣誉奖励直接落已领取；拿到 `mail_id` 才落已领取；一条失败不挡其余；请求只由奖励行构造，`ExpireMs=0`。
  - 结算时把邮件标题正文冻结进奖励行（`match_authority.proto` 奖励行 `mail_title = 16`、`mail_body = 17`，三处 INSERT 与扫描加两列）；`tournament/catalog.go` 的 `Build` 拒绝超过 128 字的标题或 4096 字的正文。
  - `svc/servicecontext.go` 新增 `requireTournamentMail`：dev / test 以外开着锦标赛却没装配发件，启动即拒。
  - 生死状退费只留 scene 一条通道：删 `refundJianghuSilverOnce`、`maybeRefundJianghuSilverForFormation`、`cancelPreparedWithOptions` 与退费标记表（`MatchJianghuRefundRecord`、`authority/jianghu_duel.go` 三个读写方法、`schema.go` 表清单）；`globalAbortPrepared` 去掉 skip 参数；新增 AST 守卫测试：match 只有 tournamentlogic.go 发信，任何地方都不置 `SkipJianghuSilverRefund`。
  - 发件接线：`go/mail/etc/mail.yaml` Producers 加 `match: MAIL_PRODUCER_SECRET_MATCH`；mail 指标加 `sender="match"`；`deploy/k8s/manifests/go-svc/mail.yaml` 注入该密钥（secretKeyRef，optional:false）；`k8s_deploy.ps1` 的 `Add-MatchAuthoritySecretEnv` 给 match 注入密钥，match ConfigMap 补 `MailRpc`（`mail.<infra>:50900`）与 `MailSecretEnv`（只写变量名），mail 准入名单加 match；本机开发 `tools/scripts/lib/producer_dev_secret.ps1`、`tools/scripts/go_services.ps1` 同步。
- **改动文件**（已随 `76e2b59bb` 进 main）：`proto/match/match_authority.proto`、`go/match/internal/logic/{tournamentlogic.go,jianghu_duel_logic.go,gather.go,global_match.go}`、`go/match/internal/authority/{tournament.go,jianghu_duel.go,schema.go}`、`go/match/internal/tournament/catalog.go`、`go/match/internal/svc/servicecontext.go`、`go/mail/etc/mail.yaml`、`go/mail/internal/{metrics,server}`、`deploy/k8s/manifests/go-svc/mail.yaml`、`tools/scripts/k8s_deploy.ps1`。**未提交**：`go/match/etc/match_service.yaml` 与 `go/match/internal/config/config.go`（只改注释）、`go/match` 新测试 `logic/tournament_reward_claim_test.go`、`logic/jianghu_refund_single_channel_test.go` 及改动的 `jianghu_duel_logic_test.go`、`tournament/rules_test.go`、`svc/servicecontext_test.go`、`go/mail/internal/data/match_sender_identity_test.go`（新）、`tools/scripts/lib/producer_dev_secret.ps1`、`tools/scripts/go_services.ps1`、契约测试 `k8s_deploy_contract`、`k8s_migrate_gate`、`lobby_authority_render`、`logical_db_deploy_contract`、`go_services_local_mysql`。
- **设计稿**：`docs/planning/season-team-tournament-v1.md` 新增第七节（每条五项）并改正第 75、92、93 行与漏洞表那一行；`docs/design/mail-system.md` 文末一节；`docs/design/workload-secrets-via-k8s-secret.md` 盘点表一行；`docs/design/gate-online-identity.md` 准入名单一行；`docs/ops/k8s-open-server-runbook.md`「运维仍要预置的键」加 `mail-producer-match`。
- **验证（Gemini，仓库根）**：
  1. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。另一会话（r2-jh#3）在同一份 match_authority.proto 与 `schema.go` 里加 `MatchGrudgeAppliedRecord`，等它落完再生成一次。这份 proto 不进客户端；`robot/vendor` 的 match 副本没有自动同步钩子（robot 不用这两个消息）。
  2. `go/match`：`gofmt -l internal/` 无输出；`go build ./...`；`go vet ./internal/logic/ ./internal/authority/ ./internal/tournament/ ./internal/svc/`；`go test ./internal/logic/ -count=1 -run 'TestClaimTournamentRewards|TestTournamentRewardMailRequest|TestJianghuSilverRefundHasOnlyTheSceneChannel|TestJianghuDuel_' -v`；`go test ./internal/logic/ ./internal/tournament/ ./internal/svc/ ./internal/authority/ ./internal/config/ -count=1`。新增与改名用例全 PASS，整包不新增失败。
  3. `go/mail`：`go build ./...`；`go test ./internal/data/ -count=1 -run 'TestMatchSenderIdentityMatchesTournamentClaim|TestBattleSenderIdentityMatchesSceneOutbox' -v`；`go test ./internal/metrics/ ./internal/server/ -count=1`（预建序列数改成 3×8×5）。
  4. 部署契约（PowerShell 7，先清掉 `MMORPG_*` 环境变量）：`pwsh -NoProfile -File tools/scripts/tests/<名字>.tests.ps1`，逐个跑 `k8s_deploy_contract`、`k8s_migrate_gate`、`lobby_authority_render`、`logical_db_deploy_contract`、`go_services_local_mysql`、`go_services_ports`，全部 `[PASS]`。
  5. 已有的库先跑 match 迁移（奖励表 ADD COLUMN），否则 schema 核对不过、match 拒绝接流。
- **剩余与取舍**：
  - `mail-producer-match` 由运维预置，不由部署自动生成（照 rank → mail 的先例，理由在设计稿第七节）。**代价：infra 必须先建好这把键，否则 mail 与 match 都起不来，mail 停摆会影响全部邮件**；已按旧模板抄过 `runtimeConfigSecret` 的环境要重抄。
  - 启动校验只看启动那一刻的锦标赛开关，运行期热更打开锦标赛只靠领取时 fail-closed 兜底。
  - 邮箱满时玩家看到的是内部错误，mail 的满箱提示没透传；响应同时带错误与已领取部分，客户端怎么显示没核。
  - mail 的 Sweep 开删除模式（缺省不删）后，「邮件已建、已领取没落定」而玩家 37 天以上没重领的，再领会补发第二封。
  - 退费标记表在旧库里会留成孤表（未上线，可清库）。
- **复核（2026-11-27，会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」，Opus 5.5，只读代码，未编译）：通过；顺手补了「邮箱满提示透传」一处（上一条第 3 小点）。**
  - 读的是 `git diff 8a6697af0 HEAD` 下 §7.2 的源文件。同一区间里 `gather.go`、`global_match.go`、`servicecontext.go` 与 mail `server.go` 还有别的会话的改动（万象残局、防沉迷截止、榜名查询、双人复核），不算本批。`gather.go` 现已没有未提交改动。
  - r2-tn#1：带附件而发件没装配回内部错误、行保持可领取；只有拿到 `mail_id` 才落已领取。`MarkTournamentRewardClaimed` 行锁内只把 CLAIMABLE 改成 CLAIMED，双击并发领取只落一次，两次邮件请求被 mail 按 (sender_kind, request_id) 判成同一封。非 dev / test 档开着锦标赛而没装配发件，启动即拒。
  - r2-tn#2：请求只由奖励行构造（标题正文结算时冻结进行里、`ExpireMs` 不填、收件人取行上的 player_id），重领逐字节相同。`catalog.Build` 按 mail 的长度上限拒绝超长文案，冻结进行里的原件不会被 mail 永远拒收。
  - r2-jh#2 match 侧：`refundJianghuSilverOnce`、退费标记表与 skip 参数都删干净，grep 不到残留；只剩 scene 一条退费通道，与 §7.3 批 X 对上。
  - 协议：`mail_title = 16`、`mail_body = 17` 是新号；删掉的 `MatchJianghuRefundRecord` 是整条内部表消息，不涉及字段号复用。
  - 发件接线：mail `Producers` 加 `match`，sender_kind 6 与 match 的 `tournamentMailSenderKind` 一致；mail 清单经 `secretKeyRef` 注入 `mail-producer-match`。
  - 上一条「剩余与取舍」第 4 小点（Sweep 删除模式下补发第二封）已被 r2-craft#3 的 `mail_request_receipt` 关掉：收据按 (sender_kind, request_id) 存、清理不删，对 match 同样生效。
  - 改了什么：`claimOneTournamentReward` 遇到 mail 回 `kMailRecipientBoxFull` 时原样回这个 tip，其余失败照旧回内部错误；`tournament_reward_claim_test.go` 的邮件替身加 `rejectNext`，新增 `TestClaimTournamentRewardsPassesThroughRecipientBoxFull`。五项写在 `docs/planning/season-team-tournament-v1.md` §7.7。验证：`cd go/match; go test ./internal/logic/ -count=1 -run 'TestClaimTournamentRewards' -v` 全 PASS。

### 7.3 批 X：生死状普通战书银两（r2-jh#1、r2-jh#2 scene 侧都成立）

- **改法**：
  - `PlayerJianghuComp` 加 `pending_silver_fee_review_after_ms = 13`；备战时预留与银两写同一个复核时刻。
  - 别的局有未结清待退时，`ValidateAndCollectPrepareFee` 与 `CommitPrepareSilverFee` 都拒绝（`kFeatureUnavailable`），不覆盖标记。
  - `RefundPendingSilverFee` 取代 `OnBattlePrepareCancelled` / `OnBattleVoid`：先入账成功再清标记，玩家不可改时不动；`CountsAsGain` 豁免 `TX_JIANGHU_DUEL_REFUND`，获取封禁不挡退费。
  - 复核改名 `ReviewBattleHoldsAndFees` / `ReviewBattleHoldAndFee` / `OnBattleHoldAndFeeTerminalAnswer`：同一 (玩家, 局) 的道具预留与待退银两合并成一项，只查一次 SQL 终局；ABORT / 没有可裁决原件才退；FIGHTING 冻结到期查到同样结论时一并退。
  - scene 不再读 `skip_jianghu_silver_refund`（proto 注释改「已废弃，恒为 false」，字段号保留）。
  - 两处偏离定案（已写进设计稿）：拒绝码用 `kFeatureUnavailable` 而不是 `kServiceUnavailable`（后者在 Tip 表是 fault，match 会误判成 scene 内部故障）；复核对待退银两不按「本地已应用」跳过（作废结算可能已应用而退款没入账），另加缺陷兜底 `ConsumePendingSilverFee`（COMPLETE 且结算已在本实体应用、标记却还在时清掉不退并打 ERROR，免得玩家永远开不了普通生死状）。
- **改动文件**（代码已随 `3eb57a09a`、`d77a458ee` 进 main）：`proto/common/component/player_jianghu_comp.proto`、`proto/scene/scene.proto`（注释）、`cpp/libs/modules/gain_block/gain_block_service.h`、`cpp/libs/services/scene/player/system/player_jianghu.{h,cpp}`、`cpp/libs/services/scene/battle/system/player_battle.{h,cpp}`。**未提交**：`cpp/tests/bag_test/player_jianghu_test.cpp`（改 2、新增 3）、`cpp/tests/bag_test/battle_freeze_hold_test.cpp`（新夹具 `BattleSilverFeeReviewTest` 6 个）。
- **设计稿**（未提交）：`docs/planning/battle-jianghu-grudge-duel-phase1-implementation-plan.md` 末尾定案一节（流程、三个问题五项、持久字段、日志、验证），正文 6 处「经 mail 退还」改正；`docs/planning/battle-jianghu-grudge-duel-bounty-v1.md` 5 处改正；`docs/design/battle-item-reservation.md` 补「与道具预留共用同一次终局复核」一段；`docs/design/turn-battle-gap-closure.md` 函数名更新。
- **验证（Gemini，仓库根，串行，保留首错）**：
  1. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`，再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`（两个 proto 都不进客户端）。通过：`player_jianghu_comp.pb.h` 有新字段；`proto/persist_schema_gen.txt` 本批只多一行 `1=PlayerJianghuComp#13 optional uint64 pending_silver_fee_review_after_ms`；`state=bootstrap` 不变；`kPersistSchemaGen` 仍为 1（台账不手改，由生成器追加）。
  2. `cd tools/proto_generator/protogen && go vet ./internal/persistschema/... && go test -count=1 ./internal/persistschema/...`。
  3. 静态门禁：`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`（代理已跑 13/13 OK）、`python tools/scripts/tests/test_cpp_thread_sources.py`、`python tools/scripts/check_log_text_english.py`。
  4. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`，0 个 `error C` / `LNK`；局部编按 `cpp/generated/proto/proto.vcxproj` → `cpp/libs/modules/modules.vcxproj` → `cpp/libs/services/scene/scene.vcxproj` → `cpp/tests/bag_test/bag_test.vcxproj`（先编库，测试链接预编好的 lib）。
  5. `build\cpp\tests\bag_test.exe --gtest_filter=PlayerJianghuTest.*:BattleSilverFeeReviewTest.*:BattleItemHoldTest.*:BattleItemHoldRules.*:BattleFreezeHold*:PlayerItemGainBlockTest.*:PersistRoundTripTest.*:PlayerFeaturePersistence*` 全绿（新增 `PlayerJianghuTest.SilverWritPendingForAnotherBattleRejectsTheNewPreparationWithoutTouchingTheMarker`、`PlayerJianghuTest.SilverRefundKeepsTheMarkerWhenTheCreditFailsOrThePlayerIsNotMutable`、`PlayerJianghuTest.GainBlockDoesNotStopTheSilverRefund`、`BattleSilverFeeReviewTest.*` 六个；改过 `PlayerJianghuTest.SilverWritDebitAndCancelRefund`、`PlayerJianghuTest.VoidAndZeroRoundRefundPendingSilverFeeOnce`）；再跑完整 `bag_test.exe`，`currency_test.exe` 与 `cross_zone_test.exe` 不回归。
  6. 批 W 与本批都过之后跑 robot 冒烟：普通战书成局、成局失败取消、VOID 三种，核对每局 500 银两恰好扣一次，并恰好退一次或被结算消耗一次。
- **剩余与取舍**：旧局取消丢失时，玩家要等旧局复核退款（约旧局截止 + 120 秒，match 不可用更久）才能再开普通生死状，金帖不受影响；超期每 10 分钟一行 ERROR `metric=jianghu_duel_fee_overdue`，没加告警规则；资格预检 `CheckJianghuDuelEligibility` 不判旧局未结清，要到备战时才拒；过户仍清待退标记、不退（沿用原策略）；合并复核今天不会真的合并（普通战书局没有道具预留），是防以后两条路互顶；`PrepareBattle` 把复核时刻传给 `CommitPrepareSilverFee` 这一处没有直接单测；`ConsumePendingSilverFee` 若判反方向会少退 500，只有 ERROR 日志。
- **复核（2026-11-26 晚，会话「交接单里的代码改动清零与方案入档」，Opus 5.5，只读 diff，未编译）：通过，没有要改的。**
  - 读的是 `git diff 8a6697af0` 的五个源文件与两个未提交测试文件。
  - 核过的点：退款先入账、成功才清标记，玩家不可改时不动；备战与提交两处都在别的局未结清时拒绝、不覆盖标记；同一 (玩家, 局) 的预留与待退银两只发一次终局查询，ABORT / 没有原件才退，冻结到期那条路（`ResolveExpiredFightingFreeze` 的回答）同样退；作废结算在收件箱同一次存档里退，入账失败标记留着、由复核按 ABORT 重试；`CountsAsGain` 豁免退款；scene 里已经没有读 `skip_jianghu_silver_refund` 的地方。
  - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 13/13 OK（本会话跑的）。
  - 剩余与取舍照上一条，不另加。match 侧删邮件退费那一半在批 W 的文件里，随批 W 一起复核。

### 7.4 修复代理顺带发现的 5 条（2026-11-27，第 4 轮会话的 Opus 5.5 Max 子代理落码；未生成、未编译、未测试）

5 条都成立。复核原定由会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」做（与批 V、W 一起）；那个会话开工时 Claude 全部模型周额度 81%，按 §6 口径改交 Gemini 4 Neon Extra，做法见 §6.1「还没做的」。

**提交状态**：代码改动被别的会话的快照提交 `24c13a311`（23:48）一并带进 main，不是本会话提交的；那次提交已顺带生成了 `go/proto/guild/guild_db.pb.go`。复核看 `git diff a0b3d195e 24c13a311 -- <文件>` 加工作区 `git diff HEAD`。**本批没有改 `go/guild/internal/data/asset_store.go`**。

| 编号 | 结论与证据（行号按改动前的 `a0b3d195e`） | 改法 |
|---|---|---|
| r2-gw#5 | 成立：结算第 3 步拿到 `AllocateSeq` 的错误原样返回（`war_repo.go:1163-1166`），到期收尾同样（:1427-1430）；上限是 16 条或跨度 512（`go/shared/assetop/seq.go:216-221`）；帮战结果走 `guild-trial` 消费组，重投 20 次约 4～7.5 分钟后整局进死信，没有巡检兜底 | 照历练的「先记待入账、由循环补插」做：新表 `guild_war_credit_owed`（主键 `(player_id, op_id)`，锁序位置 24），`creditOrOweTx`「能插指令就插，否则记待入账」；`ConvertWarCreditOwed` 用预发的同一个 op_id，先判行还在不在再插，同一笔至多一条指令 |
| r2-gw#6 | 成立：结算与到期收尾事务都没带 `withRoleTargets`（`war_repo.go:1038`、:1403），历练带了（`activity_repo.go:1318`）；帮战没有 launch 原件行，挂起全集拦不住「终局提交」与「结算消费」之间冻结的角色；删角收尾删 seq 行后，迟到的结算会用 `ensureSeqRowTx` 重建并给墓碑角色插指令 | 按人分流、不整笔拒绝（整笔拒绝会重现 #5 的「一人挡全局」）：`classifyCreditFence` / `lockCreditFences` 锁全部入账目标的护栏副本；冻结中的人记待入账（冻结解除后补给这个角色），已删除的人什么都不写；领赏人已删除时追杀令不转移，等到期退回发布者；发布者已删除时银两退款放弃。挂起全集 `role_fence_pending.go` 加 `guild_war_credit_owed` |
| r2-gw#7 | 成立：扣款终结 `UPDATE … WHERE escrow_op_id = ? AND status = 1`（`asset_store.go:1070`），而 `guild_db.proto` 的四个索引都不以 `escrow_op_id` 开头；全仓没有删 `guild_war_bounty` 的语句。逐个核过引用，终态行不再被任何路径需要（扣款终结只认状态 1，结算读到终态行与读不到行都是「不动追杀令」，战报只读对局行自己的列，`bounty_id` 是永久 ID 不复用） | `guild_war_bounty` 加第 5 个索引 `escrow_op_id`；`PurgeTerminalBounties`：终态行保留 30 天后删（常量），每轮至多 200 行，一行一个短事务，先主键点锁再点删，挂在到期收尾的节拍上 |
| r2-gw#8 | 成立：`InspectFieldWarTargets` 对任何 ACTIVE 单都回单号、赏金与档位（`war_logic.go:1759-1763`），追杀榜（`war_repo.go:786`）是有过滤的 | 新增 `fieldTargetBounty`，按 `BountyHuntable` 过滤 |
| r2-tn#3 | 成立，**但不是本批改的**：另一个会话（改名与内容安全第三轮）已修完并在 main（`k8s_deploy.ps1` 的准入名单与 match ConfigMap，契约测试在 `k8s_deploy_contract.tests.ps1`）；子代理核过端口 50400 / 50700 / 50300 与 `$GoSvcCatalogue` 一致，集群只有入站准入、没有出站策略，没有重复改 | — |

- **改动文件**（已随 `24c13a311` 进 main）：`proto/guild/guild_db.proto`、`go/guild/internal/data/{war_credit_owed.go(新),war_repo.go,role_fence_pending.go,tables.go}`、`go/guild/internal/logic/{war_logic.go,trial_background.go}`、测试 `war_credit_owed_test.go`（新）、`war_credit_owed_tidb_test.go`（新，3 个 TiDB 护栏用例）、`war_repo_test.go`（加 4 个真库用例）、`role_fence_tidb_test.go`、`war_logic_test.go`、`guild_repo_test.go` 与 `economy_flow_integration_test.go` 的删表清单、`docs/design/horizontal-scale-logical-db.md` 的 guild 挂起全集一行。**未提交**：`docs/planning/guild-play-v2-implementation-plan.md` 的 §7～§10（r2-gw#5～#8 各五项、转换流程表）与 §11（验证命令）、`docs/planning/battle-jianghu-grudge-duel-phase1-implementation-plan.md` 的 r2-tn#3 五项、`go/guild/internal/data/asset_tables_shape_test.go`（钉住新表与索引形状）、`war_logic.go` 一行注释。
- **验证（Gemini，仓库根，串行，保留首错）**：
  1. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`（`guild_db.proto` 不进客户端；批 V 的 `guild.proto` 若还没同步客户端，按 §7.1 用缺省配置生成并同步）。通过：`go/proto/guild/guild_db.pb.go` 不再有差异。
  2. `cd go/guild; gofmt -l internal/` 无输出；`go build ./...`、`go vet ./...`；仓库根 `python tools/scripts/check_log_text_english.py`。
  3. 纯单测：`go test ./internal/data -run 'TestWarCreditOwedColumnsCoverEveryProtoField|TestClassifyCreditFence|TestLockCreditFencesWithoutGuardAndVerdictLists|TestWarCreditRecordCarriesTheOwedCreditVerbatim|TestDropListCoversTables|TestGuildWarBountyColumnsCoverEveryProtoField' -count=1 -v`；`go test ./internal/logic -run 'TestFieldTargetBountyHidesGuildOnlyBountiesFromOtherGuilds|TestWarBountyExpiryEveryRounds' -count=1 -v`；`go test ./internal/war/... ./internal/logic/... -count=1`。
  4. MySQL 真库：设 `GUILD_TEST_MYSQL_DSN`（库名 `guild_test`），`go test ./internal/data -run 'SettleWarBattle_|ExpireBounty_|PurgeTerminalBounties_|TestAssetTablesShape' -count=1 -v` 全 PASS、不得 SKIP，批 V 原有用例不回归。
  5. TiDB 真库：设 `GUILD_ROLE_TEST_DSN`，`go test ./internal/data -run 'TestTiDBWarSettle|TestTiDBExpireBountyOwes|TestTiDBPendingGuildBlocksFreezeAndRemoval' -count=1 -v` 全 PASS、不得 SKIP。
  6. 全包 `go test ./... -count=1` 与 `go test -tags integration ./internal/logic -count=1`。
  7. r2-tn#3 部署契约（PowerShell 7，先清 `MMORPG_*`）：`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1` 与 `chat_authority_deploy.tests.ps1` 全部 `[PASS]`，含「match 的江湖生死状接线」与准入名单那条。
  8. 本地已有的 `guild_war_bounty` 要重建（schemamigrate 不给已存在的表补索引）；新表会自动建。
- **剩余与取舍**：待入账积压没有 gauge 与告警（只有每轮汇总日志与每笔 Info 日志）；冻结中的玩家每轮多一次 `GetRoleFenceState`（寄售可能持续几天）；过户完成后冻结期间记下的这一笔照常补给这名角色（与历练按「目标」的口径一致）；保留期是常量 30 天，不跟 `AssetOp.TerminalRetentionDays`；客户端可见的 EXPIRED 状态注释仍写「已原路退回」，实际还含「退款排在待入账里」与「发布者已删除、退款放弃」，只在设计稿写明。
- **顺带发现、没改**：删角收尾不删 `guild_war_player_state` 与 `guild_war_garrison`（后者含玩家名与快照），是账号注销清单原有的缺口；`docs/design/account-deletion.md` 正被别的会话改，新增挂起项 `guild_war_credit_owed` 没登记进去（客户端按通用文案显示）。
- **复核（2026-11-27，会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」，Opus 5.5，只读代码，未编译）：通过，没有要改的。**
  - 读的是 HEAD 上 `war_credit_owed.go` 全文，`SettleWarBattle` 的第 F、3、3b、10 步，`ExpireBounty`、`PurgeTerminalBounties`，logic 层三个循环与 `fieldTargetBounty`，以及 `role_fence_pending.go`、`tables.go` 的登记。
  - r2-gw#5：本流未决已满只让这一笔记进待入账表，seq 不前进，seq 行锁照持（它是领赏计数的守卫）。转换用行里预发的 op_id，先判行还在再插；点删影响 0 行就整笔回滚，`guild_asset_op` 主键再兜一层。同一笔至多变成一条指令。
  - r2-gw#6：护栏副本是事务第一把锁，按 player_id 升序。副本缺行或冻结先自愈一次（`inTx` 经 `fence.RunTx` 自愈后重跑），仍冻结才记待入账。已删除的人不建 seq 行、不写帮贡与个人态；领赏人已删除时整笔不转移，等到期退回发布者。
  - r2-gw#7：`escrow_op_id` 索引补上。保留期清理一行一个短事务，主键点锁后复核仍是终态才删，状态 1、2 永远不删。
  - r2-gw#8：野外点选面板与追杀榜、开战挂钩共用 `BountyHuntable`。同一目标同时只有一张未结追杀令，`LIMIT 1` 不会挑错单。
  - 锁序：结算是护栏副本 → G → M → Q → O → C → 灵脉 → … → 追杀令 → 对局 → 宿敌 → 待入账，到期收尾是护栏副本 → G → M → Q → O → 追杀令 → 待入账。两边同序，只会排队，不会成环。
  - 剩余与取舍、顺带发现照上两条，归 r2 剩余单 §3.3。

### 7.5 Java 版

三批都没改 Java（独立仓 `luyuan-java/xuanming-server-mmo`）。Java 版的生死状同样用 `match_jianghu_refund` 加邮件退费，帮战追杀令、锦标赛领奖是否有同类问题待核对；只在 Java 仓 `PARITY.md` 登记待做。

## 6. 负责人 11-26 已定

- r2-craft#1 悬赏求书**改走 go/trade 托管**，做法见 §3「其余」。
- **批 V/W/X 之外的 9 条由 Claude（Opus 5.5）写**，不交 Gemini：
  - 包括 r2-mall#1、r2-craft#1 两条 medium，以及 r2-jh#3、r2-craft#2~#4、r2-ops#1、r2-ops#2、r2-oa#1 七条 low。
  - **实际分工（2026-11-26 晚，两个会话确认）**：这 9 条由「交接单里的代码改动清零与方案入档」会话做。它在负责人改定之前，已按两个会话当时的约定派出子代理：r2-mall#1 已完成（未编译），其余 8 条在写，craft#1 先出设计再落码。它写完、复核后把结论、文件和 Gemini 命令填进本节对应的行。第 4 轮会话改做 §7.4 修复代理新发现的 5 条（r2-gw#5～#8、r2-tn#3）。负责人若坚持由第 4 轮会话做这 9 条，那个会话会让子代理停在文件边界，把已改的文件清单交过来。
  - 周额度（全部模型）超过 80% 就停在一批的边界上，把没做的写回本节交 Gemini。
  - Gemini 照常只做生成、编译、测试，并按 §0 先复核 Opus 写的批次。
- **跑第 5 轮**（`D_ROUNDS[4]` 自由视角三路）：第 4 轮还有新发现，按「连续两轮没有新发现才停」继续。
  - 时机与模型：等第 4 轮的修复（批 V、W、X 与其余各条）落完再开，用最高档模型（12-03 Fable 额度重置后用 Fable 5.2 Max，或现在用 Gemini 4 Neon Extra）。
  - 旧核心机制的网络分区推演并进这一轮一起跑：按「最标准的定」的口径定，负责人没有单独表态。
  - 去重清单：第 3 轮 104 条、11-19 / 11-20 评审修正 7 条、第 4 轮 17 条、修复代理新发现的 5 条（r2-gw#5～#8、r2-tn#3，见 §7.4），共 133 条。
  - 审计脚本和旧去重清单在旧交接目录，11-20 瘦身时已删，用 `git show ccf2840b2:docs/handoff/data-overwrite-audit-r2-20261019/<文件>` 读。

### 6.1 九条的结论与验证（会话「交接单里的代码改动清零与方案入档」，2026-11-26 晚，Opus 5.5 子代理写、本会话复核；全部未生成、未编译）

大部分代码已被别的会话的快照提交 `a0b3d195e`（22:48）等带进 HEAD；**proto-gen 与导表之前，HEAD 上的 go/match、go/guild、scene 编不过**（新表类型、两条新 tip）。

| 编号 | 结论 | 改了什么 | 设计记录 |
|---|---|---|---|
| r2-mall#1 | 成立（门控，缺省不可达） | `java/mall_game` `ManualAdminStore`：RETURN_TO_SELLER 只认付款已撤销；新增 RETRY_DELIVERY（订单 7→1、交付待办回 PENDING，由 trade 重新回执）；网关 `mall-admin.html` 下拉；`rmb_cancel_test.go` 补断言（trade 代码不动） | `jubaozhai-market.md` §16.1；隔离手册 §4.2 / §6 |
| r2-craft#1 | 成立 | 负责人 11-26 晚批准分三批,**2026-11-27 三批代码全部写完,未编译、未运行,待 Gemini 验证**。批 A(trade 侧 kind 7 求购单合同,RPC 582~584,已独立复核)见设计稿 §11.1;批 C(K8s 各档位以 `-AssetOps on` 部署,求购单开关跟着打开,trade 读镜像里的表、表包模式挂 sidecar)与批 B(scene 删 560~563 四条 RPC 与悬赏存档字段、`ClientRequestKind` 26~28 退役、限流行删除;客户端 `SkillCraftClient` 改调 trade,书架书先封卷再揭榜,发榜 / 揭榜按同一请求号重发查结论)见 §11.2。scene 里已没有同帧改两名玩家的揭榜代码 | `skill-book-wanted-order-trade-escrow.md` §0～§9 设计、§11.1 批 A、§11.2 批 C / B 落码记录与 Gemini 验证顺序 |
| r2-craft#2 | 成立 | 书架上限 100 本，制书 / 开卷 / 揭榜送书三处副作用前判；Tip.xlsx `//skill_error` 段末加 `SkillBookshelfFull`、`SkillWantedPosterBookshelfFull`（本会话用 `tools/scripts/craft_bookshelf_cap_xlsx_patch.py` 加，幂等，**导表未跑**） | 同上 §10；策划 v2-v5 §2.4 |
| r2-craft#3 | 成立（只在 `Sweep.Mode=delete` 时可达） | mail 新表 `mail_request_receipt`（判重收据，清理不删），发件先查收据、收据先于邮件行写 | `mail-system.md` §2.4.1 |
| r2-craft#4 | 成立（只影响口碑展示） | 口碑回账改 Lua 原子取走（HMGET + HDEL）再入账，宗师离开或存盘排不上原样还回 | `craft-proxy-reputation-digest-and-hot-ranking.md` §2.5 |
| r2-ops#1 | 成立 | `table-bundle rollback` 不加 `-force` 时两个目录必填，并按 `GET /admin/tables` 的生效包与 `-target-digest` 核对 | `config-table-hot-reload.md` §9；手册 §4 |
| r2-ops#2 | 成立（要写通配规则才触发） | go/db 热关停把 `SavePlayer` 设为受保护方法 | `rpc-kill-switch.md` §8.1 |
| r2-oa#1 | 成立（缺省不启用） | `Mode=enforce` 启动即拒（fail-closed），shadow 不变；部署配置里没有开 enforce 的地方 | `linearizable-owner-authority.md` §7 |
| r2-jh#3 | 成立（只影响统计与播报） | 一局一行、不裁剪的入账闸门表：match `match_grudge_applied`、guild `guild_jianghu_grudge_applied`（锁序 23） | `battle-jianghu-grudge-duel-bounty-v1.md` §9 |

**Gemini 验证顺序**（只跑，红了保留首错原文交回 Claude）：
1. 导表：`dev.bat gen`（会带进别的会话未提交的表改动，开跑前先核 `git status --short data/`）；`skill_error_tip.pb.h` 要有两条新码。
2. 协议：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`；`go/proto` 里要有 `MatchGrudgeAppliedRecord`、`GuildJianghuGrudgeAppliedRecord`、`MailRequestReceiptRecord`。
3. C++：`msbuild` 串行 `/m:1` 依次 `cpp/generated/table/table.vcxproj`、`cpp/generated/proto/proto.vcxproj`、`cpp/libs/services/scene/scene.vcxproj`、`cpp/tests/bag_test/bag_test.vcxproj`，再整解；`bag_test.exe --gtest_filter=PlayerCraftQualityTest.*:EquipCraftQualityRulesTest.*`。
4. Go（各模块 build / vet / test）：`go/table_sync`（`TestRollbackRequiresBothDirsUnlessForce`、`TestRollbackChecksTheLiveBundleAndTheTarget`、`TestCheckPermanentIDRollbackRejectsDirsThatAreNotTheTwoBundles`）；`go/db`（`TestPlayerStoreKillSwitchKeepsSaveOpenUnderWildcards`）；`go/scene_manager`（`TestInitOwnerAuthority_ConfigValidation`）；`go/match` 真 TiDB（`MATCH_IT_TIDB` + `MATCH_IT_REQUIRE_TIDB=1`，`TestGrudgeLedger_RedeliveryAfterTrimOrTransferStaysDuplicate`）；`go/guild` 真 MySQL（`GUILD_TEST_MYSQL_DSN`，`TestRecordJianghuDuelResultStaysIdempotentAfterRecordTrimmed`、`TestDropListCoversTables`）；`go/mail` 真 TiDB（`MAIL_RANK_TEST_TIDB` + `MAIL_RANK_REQUIRE_TESTS=1`，`TestTiDBResendAfterSweepReturnsOriginalMail`）；`go/trade` 真 TiDB（`COMMERCE_TEST_DSN` + `COMMERCE_REQUIRE_TIDB=1`，`TestTiDBRmbCancelRefusesManualContractAfterPaidDeliveryWentPartial`）。
5. Java：在 `java/` 下 `mvn -B -pl mall_game -am test "-Dtest=ManualAdminStoreTest" -Dsurefire.failIfNoSpecifiedTests=false`，再全量 `mall_game`、`gateway_node`。
6. r2-craft#1 批 A（go/trade 求购单）：第 1 步导表要出 `TradeError_kTradeWantedOrderLimitReached`、`TradeError_kTradeWantedBookMismatch`；之后 `go/trade` 的 build / vet / 单测与真 TiDB 用例（`TestTiDBWantedSkillBook*`）、客户端三步，命令与通过标准见 `skill-book-wanted-order-trade-escrow.md` §11.1 末。

**还没做的**（写回这里，按负责人口径额度过 80% 交 Gemini）：
- r2-craft#1 落码：**2026-11-27 批 A、批 C、批 B 都已写完（未编译）**，验证命令见设计稿 §11.1 末与 §11.2 末；原定「批 C、批 B 负责人 11-27 改定换电脑由 Claude Code（Fable 5.2 Max）写，Gemini 不写，只做写完后的生成、编译、测试」（设计稿 §11.2，写完同节补五项记录，再按设计稿 §9 跑）。Gemini 跑批 A 时把导表放在最前：两条新 tip 不导出来 go/trade 编不过。
- guild 人工终结的条件写补比 `attempts`（r2 早期遗留第 1 条的最后一处）：**2026-11-27 已由会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」写完（负责人在该会话说「开始做吧」），未编译，验证见 r2 剩余单 §3.2。** 以下为当时的交接原文：**2026-11-27 交 Gemini 4 Neon Extra 写**（负责人随后改定：换电脑由 Claude Code 写，见 §0a）。会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」开工时 Claude 全部模型周额度 81%，按 §6 口径没动手；§0 的「Gemini 不要改本单的代码」不适用于本条。
  - 文件已空出：§7.4 没改过 `asset_store.go`，现在没有会话占着。行号按 HEAD `24c13a311` 核过。
  - `go/guild/internal/data/asset_store.go:259`：`sqlResolveAssetOpQuiet` 末尾加 `` AND `attempts` = ? ``。
  - 同文件 :739：`ResolveManuallyIfQuiet` 的实参在 `witness.LastSendMs` 之后加 `witness.Attempts`。
  - 同步 :257-258 与 :727-732 两处注释：只领没发也落空；钟慢副本发过请求、`last_send_ms` 推不动时，靠 attempts 落空。
  - `go/guild/internal/data/economy_repo_test.go` :1901：见证字面量必须补 `Attempts: read.Attempts`。那条先 Reschedule 过，attempts=1，SQL 一改不补就红。
  - 同文件 :1863、:1877：分别补 `read.Attempts`、`fresh.Attempts`，保持口径一致。
  - 加一条真库用例，照 `go/data_service/internal/store/recall_store_integration_test.go:538` 的 `TestRecallStore_ResolveManuallyIfQuiet_MissesWhenOnlyAttemptsMoved`：领取 → 本地 NOT_HERE 重排 → 取见证 → 再领取 → 再本地重排 → 旧见证落空、新见证终结。
  - 口径以 `data-consistency-guarantee-map-20261020.md` §6.2 第 2 条补记为准；补完核对 `docs/design/guild-phase2/05-economy.md` §5.23 的说法与代码一致。
  - 验证：`cd go/guild`，设 `GUILD_TEST_MYSQL_DSN`（库名 `guild_test`），跑 `go test ./internal/data -run 'TestResolveManuallyIfQuiet_' -count=1 -v`，全 PASS、不得 SKIP；再跑 `go test ./cmd/assetopfix/... -count=1`。
  - 补之前，第二次读到条件写之间（毫秒级）少一道兜底。
- 批 V、W 与 §7.4 五条的独立复核：**2026-11-27 已由会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」做完（负责人在该会话说「开始做吧」）：三块都通过，批 W 顺手补了邮箱满提示透传，结论在 §7.1、§7.2、§7.4 末条。** 以下为当时的交接原文：**2026-11-27 交 Gemini 4 Neon Extra**（负责人随后改定：换电脑由 Claude Code 复核，见 §0a 第 1 步）。§7.4 已落完（第 4 轮会话 11-27 通知），复核条件已具备。接手的会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」开工时全部模型周额度 81%，按口径停在第一批之前：没读代码、没下结论、没改代码。
  - 范围：按 §7.1 / §7.2 / §7.4 复核，只改错，不扩范围。
  - 口径：§2（每条按 `data-overwrite-audit-r2-round4-20261126/*.json` 查找原文的触发步骤走一遍）与 §3。
  - 重点：行锁、CAS、事务内外的读写；幂等与重投；发号失败不用墙钟；指令号在事务前发好；tip / 日志规范；协议字段号不复用。
  - 发现问题：明确的缺陷做最小改动，同批在设计稿写五项；拿不准的写进本节问负责人。
  - 结论：照 §7.3 末条的写法，在 §7.1、§7.2、§7.4 末尾各写一条。
  - 批 V 的 diff：`git diff 8a6697af0 a0b3d195e -- <§7.1 改动文件>`，由 `d77a458ee`、`76e2b59bb`、`a0b3d195e` 三次快照带进来；照 §7.1 第 7 条排除别人的两份文件。
  - §7.4 的 diff：`git diff a0b3d195e HEAD -- <§7.4 改动文件>`。当时未提交的 `war_logic.go` 一行注释与 `asset_tables_shape_test.go`，已随 `bcea63be6` 进 main。
  - 批 W 的 diff：`git diff 8a6697af0 HEAD -- <§7.2 改动文件与测试>`。`k8s_deploy.ps1` 在 `24c13a311` 里还带着别的会话修的 r2-tn#3（match 的 FriendRpc / ChatRpc / GuildRpc 与准入名单），要分开看。`go/match/internal/logic/gather.go` 当时工作区那处未提交改动已随 `bcea63be6` 进 main，先核归属再算进批 W。
  - 批 X 已复核通过（§7.3 末条）。
- 寄售交付腿部分发放在 trade 侧没有出口（mall#1 残余，补法在 `jubaozhai-market.md` §16.1），要改 `manual_plan.go`。当时它与双人复核会话的改动冲突，所以没做；11-27 核对，该文件已没有未提交改动，归 §0a 第 2.3 步。
- 账号注销缺口（四条 low 的子代理顺带发现，没深核）：match 的删角清理不删 `match_grudge_relation`、`match_grudge_log`，帮会恩怨墙存了队长名，注销设计 `account-deletion.md` §6.2 没给这几张表归类。**按负责人「现在修、不等以后」**：随本单 §0a 的剩余工作一起先复核再修（删或匿名化，§6.2 归类）。
- **以下两条同样现在修，不等第 5 轮**（先按查找原文的口径复核成立与否）：官方订单的 `CREATE_REFUND` 同样不等 trade kind-1 合同的结局；`applyJianghuDuelResult` 查档案失败按 owner_revision=0 入账、取的是处理时刻的 owner_revision（四条 low 的子代理在禁区文件里看到的，没改）。
