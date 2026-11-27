# 数据覆写审计 r2·换视角查找第 4 轮：复核与修复交接（2026-11-26，交 Gemini）

审计文档：[data-overwrite-rollback-audit-r2-20261019.md](../design/data-overwrite-rollback-audit-r2-20261019.md)（§1「仍开着」、§3.12、§5.0）。r2 前 3 轮与批 S、T、U 的验证另见 [data-overwrite-audit-r2-handoff-20261126.md](data-overwrite-audit-r2-handoff-20261126.md)。

## 0. 为什么交 Gemini、做到哪了

- 第 4 轮由 Claude 会话（Fable 5.2 Max）跑：三路查找已返回，主会话逐行补了证据，定了修复方案（§3）。9 条 high / medium 的对抗复核刚开跑，Claude 的 Fable 周额度就用完了（12-03 20:00 UTC 才重置），**复核没跑完，代码一行没改**。
- 负责人 11-26 的口径：额度不够就交 Gemini。AGENTS §10.3 要求改代码和漏洞分析用最高档模型，所以本单的复核与改码用 **Gemini 4 Neon Extra**；协议生成、编译、测试、冒烟照常由 Gemini 跑。
- **更正（同晚）**：负责人把第 4 轮会话切到 Opus 5.5 后让它「开始做」，会话在收到本单之前已派出 3 个 **Opus 5.5 Max** 子代理落批 V（帮战）、批 W（锦标赛领奖与 match 发件接线）、批 X（生死状银两 scene 兜底），各自同批写设计稿。停在半截比做完更难收拾，所以让它们做完。它们不是 §10.3 指定的最高档，**Gemini 先用 Neon Extra 逐批复核这三批的 diff**：对的保留，错的改，不该改的回退；然后才生成、编译。三批的改动文件、结论与验证命令补在 §7（三批回来后补）。批 V、W、X 之外的各条仍按本单从复核开始。
- **再更正（同晚，负责人原话「改由 Claude 写」，在会话「交接单里的代码改动清零与方案入档」里定）**：本单原来交给 Gemini 写的代码改由 Claude 写。批 V、W、X 仍归第 4 轮会话；其余 9 条（r2-jh#3、r2-craft#1～#4、r2-mall#1、r2-ops#1、r2-ops#2、r2-oa#1）与 V、W、X 回来之后的逐批复核，由「交接单里的代码改动清零与方案入档」会话做，分工以两个会话确认的为准。**Gemini 不要改本单的代码**，只做生成、编译、测试、冒烟。
- 审计基线 HEAD `bc6e70850`，审计期间别的会话推进到 `2d20f6b68`，交接时 HEAD 是 `8a6697af0`。行号可能已漂移，按函数名定位。
- 三路查找原文在本目录旁的 [data-overwrite-audit-r2-round4-20261126/](data-overwrite-audit-r2-round4-20261126/)（`asset-crosszone-idseg.json` 跨区与号段、`fault-partition-lease.json` 网络分区与租约、`entry-ops-cli.json` 运维工具）。每条的触发步骤、证据和 `fix` 建议以原文为准。

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
  - 顺序：由 Gemini 4 Neon Extra 先出技术设计，写进 `docs/design/` 下的设计稿（没有就新建；玩法口径有变时同步 `docs/planning/equipment-quality-and-skill-crafting-v2-v5.md`），再落码。
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

## 7. 批 V、W、X 的落码结果（Opus 5.5 Max 子代理，未编译；待 Neon Extra 复核）

三批完成后在这里补：每条问题的结论（成立 / 推翻）、改动文件清单、给 Gemini 的验证命令、有意没做的与剩余风险。补齐之前不要动这三批涉及的文件：`go/guild/internal/{logic/war_logic.go,data/war_repo.go,data/asset_store.go}`、`go/match/internal/logic/{tournamentlogic.go,jianghu_duel_logic.go,gather.go,global_match.go}`、`go/match/internal/authority/tournament.go`、`go/mail/etc/mail.yaml`、`tools/scripts/k8s_deploy.ps1`、`cpp/libs/services/scene/player/system/player_jianghu.*`、`cpp/libs/services/scene/battle/system/player_battle.*`、`proto/common/component/player_jianghu_comp.proto`。

## 6. 负责人 11-26 已定

- r2-craft#1 悬赏求书**改走 go/trade 托管**，做法见 §3「其余」。
- **跑第 5 轮**（`D_ROUNDS[4]` 自由视角三路）：第 4 轮还有新发现，按「连续两轮没有新发现才停」继续。
  - 时机与模型：等第 4 轮的修复（批 V、W、X 与其余各条）落完再开，用最高档模型（12-03 Fable 额度重置后用 Fable 5.2 Max，或现在用 Gemini 4 Neon Extra）。
  - 旧核心机制的网络分区推演并进这一轮一起跑：按「最标准的定」的口径定，负责人没有单独表态。
  - 去重清单：第 3 轮 104 条、11-19 / 11-20 评审修正 7 条、第 4 轮 17 条，共 128 条。
  - 审计脚本和旧去重清单在旧交接目录，11-20 瘦身时已删，用 `git show ccf2840b2:docs/handoff/data-overwrite-audit-r2-20261019/<文件>` 读。
