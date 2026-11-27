# 数据覆写审计 r2：剩余工作总清单（2026-11-27 换电脑接手，做完即删）

> 负责人在新电脑上说「开始」或「继续做完 handoff」时，Claude Code 先过 [总入口](new-computer-start-here-handoff-20261127.md) §1，再照本单从 §1 做到 §8。**全部做完后按 §9 删掉本单。**
> 本单把第 4 轮交接单 §0a 和各节里散落的待做合成了一张清单。细节仍在下面点名的章节：发现原文、修复设计、改动文件、Gemini 命令。本单与那些章节不一致时，以本单为准（截至 11-27）。
> 文中「第 4 轮单」指 [data-overwrite-audit-r2-round4-handoff-20261126.md](data-overwrite-audit-r2-round4-handoff-20261126.md)，「r2 文档」指 [data-overwrite-rollback-audit-r2-20261019.md](../design/data-overwrite-rollback-audit-r2-20261019.md)，「求书设计稿」指 [skill-book-wanted-order-trade-escrow.md](../design/skill-book-wanted-order-trade-escrow.md)。

## 0. 现在停在哪（11-27，旧电脑回收前）

- **已写完、都没编译**：
  - r2 前 3 轮各批，含批 S、T、U；
  - 第 4 轮批 V（帮战追杀令与灵脉）、批 W（锦标赛领奖与 match 发件）、批 X（生死状银两）；
  - 第 4 轮单 §7.4 的五条，§6.1 的九条（其中悬赏求书只写了批 A）。
  - 全部在 main。本单第一版随 `3d715db97` 推上 origin/main，之后的改动由负责人在旧电脑回收前一起提交推送。
- **已复核**：批 X（第 4 轮单 §7.3 末条）；§6.1 除悬赏求书以外的 8 条（该节表头记着「子代理写、本会话复核」）；批 V、批 W、§7.4 五条（2026-11-27，旧电脑会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」，结论在第 4 轮单 §7.1、§7.2、§7.4 末条：都通过，批 W 顺手补了邮箱满提示透传）；悬赏求书批 A（同一会话，结论在求书设计稿 §11.1 末条：通过）。
- **11-27 旧电脑又做完的**：§3.2 guild 见证 SQL；§3.3 第 1～5 条（删角清帮战三表与解散释放灵脉、EXPIRED 注释与客户端状态 6 文案、两处告警、生死状资格预检判旧局未结清、邮箱满提示透传与客户端先并入已领取）。都未编译，验证命令在各自小节。§3.3 第 6 条（寄售交付腿部分发放的 trade 出口）同日也做了。§3.3 只剩第 7 条的两处补用例（开战入口走完整请求路径的集成用例、`PrepareBattle` 把复核时刻传给 `CommitPrepareSilverFee` 的直接单测）：都只防回归、要搭完整的开战与备战夹具，旧电脑额度不够，没做。
- **还没做**：
  - 写悬赏求书批 C、批 B；
  - 各批记下的顺带发现；
  - 选角页回选上限的用例；
  - 文档收口、Gemini 验证、第 5 轮查找。
- **额度**：旧电脑上的第 4 轮会话停在额度线上。Fable 周额度用完，全部模型周额度 84%，过了 80% 停止线；原账号到北京时间 12-04 04:00 才重置。负责人 11-27 定：换电脑、换账号后说「开始」就做，不用等重置。
- 以上进度来自各会话的报告和 11-27 的 git 核对，没有逐个文件重读代码。以 §1 的核对和各节结论为准。

## 1. 开工核对

1. `git log` 里要有 `6b4ae7852`。它之前要有这些提交，各批代码和后来补进的测试、设计稿都在里面：`3eb57a09a`、`d77a458ee`、`76e2b59bb`、`a0b3d195e`、`24c13a311`、`bcea63be6`。第 4 轮单 §6.1、§7 里标「未提交」的文件，11-27 已全部进 main，按已提交读。缺了就停下问负责人。
2. `git status` 看清别的会话的在途改动。只动本单范围的行，改前重读。
3. 模型要用 AGENTS §10.3 的最高档（Claude Code 是 Fable 5.2 Max）。不是最高档就先问负责人；负责人明说用当前模型，就照做。

## 2. 复核（先做；写的人与复核的人分开）

新电脑上的会话没写过这些代码，可以复核。

| 范围 | 看哪段 diff | 说明在 |
|---|---|---|
| 批 V | `git diff 8a6697af0 a0b3d195e -- <§7.1 改动文件>`。排除别的会话的两个文件：`go/guild/internal/kafka/trial_result_consumer.go`、`go/guild/cmd/assetopfix/main.go` | 第 4 轮单 §3 批 V、§7.1 |
| 批 W | `git diff 8a6697af0 HEAD -- <§7.2 改动文件与测试>`。`tools/scripts/k8s_deploy.ps1` 里还有别的会话修的 r2-tn#3（match 的 FriendRpc / ChatRpc / GuildRpc 与准入名单），要分开看；`go/match/internal/logic/gather.go` 在 `bcea63be6` 改的一处，先核是谁的 | 第 4 轮单 §3 批 W、§7.2 |
| §7.4 五条 | `git diff a0b3d195e HEAD -- <§7.4 改动文件>` | 第 4 轮单 §7.4 |

> 四行 2026-11-27 都已复核完：批 V、批 W、§7.4 的结论在第 4 轮单 §7.1、§7.2、§7.4 末条，悬赏求书批 A 的结论在求书设计稿 §11.1 末条，都通过。
| 悬赏求书批 A | 求书设计稿 §11.1 列的文件（`go/trade/internal/commerce/wanted_skill_book.go` 等），对照该稿 §3 的设计 | 求书设计稿 §3、§11.1 |

diff 的终点一律用 HEAD：当时没提交的测试和设计稿，是后来的快照提交带进来的。

- **做法**：每条按查找原文（第 4 轮单同级目录 `data-overwrite-audit-r2-round4-20261126/*.json`）的触发步骤走一遍，核三件事：入口能不能被客户端或别的服务触发，上游有没有守卫已经挡住，缺省配置下能不能走到。再对照第 4 轮单 §3 的修复设计。
- **重点**：
  - 行锁、CAS、事务内外的读写；
  - 幂等与重投；
  - 发号失败不用墙钟，指令号在事务前发好；
  - tip 与日志规范，proto 字段号不复用；
  - 新持久字段走混版本字段保护台账。
- **有错**：当场做最小改动，同批按 §4 写设计稿。拿不准的写进 §8，问负责人。
- **结论**：照第 4 轮单 §7.3 末条的写法，写在 §7.1、§7.2、§7.4 末尾；批 A 的结论写在求书设计稿 §11.1 末尾。

## 3. 写代码（按顺序）

### 3.1 悬赏求书批 C（部署开通）、批 B（scene 退役旧揭榜，客户端同批切换）

- 做法、文件、前提全在求书设计稿 §11.2，批 B 的文件表在 §8。顺序是先 C 后 B；上线前清档时，三批也可以一起上。
- 批 C 有两个前提：
  - ①已满足；
  - ② K8s 各档位要不要打开 trade 的 `AssetOp.Enabled`，这个不自己定，**开工前先问负责人**。
- 批 B 最容易漏的几处，全文见 §11.2：
  - scene 的 RPC 560~563 删掉后，生成器会把号转成墓碑。一次正好 4 条，同一轮生成里不能再删别的 RPC。`MessageLimiter.xlsx` 里这四行同批删。
  - 存档字段退役走混版本字段保护台账。`ClientRequestKind` 26~28 只退役、不复用。
  - `kProxyTaxBps` 与 `kSealedSkillBookConfigId` 不能删。
- 揭榜书架书的流程是先 `SealSkillBook` 拿到道具号，再交 trade 托管。新加「移出人物背包」的入口，必须先问 `CheckRemovable*`，否则会绕过战斗道具预留（见 `docs/design/battle-item-reservation.md`）。
- 写完在 §11.2 补五项记录，再交 Gemini 按该稿 §9 跑；Java 仓 `PARITY.md` 那一节补一句「客户端已切」。

### 3.2 guild 人工终结见证的条件写补比 `attempts`

> **2026-11-27 已做完（旧电脑会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」，Opus 5.5；未编译，只剩下面「验证」交 Gemini）。** 按下列步骤改了 `asset_store.go` 的 SQL、实参与两处注释，`economy_repo_test.go` 三处见证字面量，并新增真库用例 `TestResolveManuallyIfQuiet_MissesWhenOnlyAttemptsMoved`；顺带删掉 `go/shared/assetop/manual_evidence.go`、`go/guild/cmd/assetopfix/main_test.go` 注释与 `05-economy.md` §5.23、保证图 §6.2、r2 文档 §3.x 第 1 条里的「guild 待补」字样。两处状态已同步。下面的步骤留作对照。

这是早期遗留第 1 条的最后一处。行号按 HEAD 核过，`24c13a311` 之后这两个文件没再改过。

- `go/guild/internal/data/asset_store.go:259`：`sqlResolveAssetOpQuiet` 末尾加 `` AND `attempts` = ? ``。
- 同文件约 739 行：`ResolveManuallyIfQuiet` 的实参在 `witness.LastSendMs` 之后加 `witness.Attempts`。
- 同步改约 257~258 行与 727~732 行的两处注释，写明两种落空：只领了没发出，见证落空；钟慢的副本发过请求但推不动 `last_send_ms`，靠 attempts 落空。
- `go/guild/internal/data/economy_repo_test.go`：
  - 约 1901 行的见证字面量补 `Attempts: read.Attempts`。那条用例先 Reschedule 过，attempts 是 1，SQL 一改不补就红。
  - 约 1863、1877 行分别补 `read.Attempts`、`fresh.Attempts`，保持口径一致。
- 加一条真库用例，照抄 `go/data_service/internal/store/recall_store_integration_test.go` 里的 `TestRecallStore_ResolveManuallyIfQuiet_MissesWhenOnlyAttemptsMoved`：
  - 领取 → 本地 NOT_HERE 重排 → 取见证 → 再领取 → 再本地重排；
  - 断言旧见证落空、新见证能终结。
- 口径以 `docs/design/data-consistency-guarantee-map-20261020.md` §6.2 第 2 条的补记为准；补完核对 `docs/design/guild-phase2/05-economy.md` §5.23 的说法与代码一致。
- 验证（交 Gemini）：`cd go/guild`，设 `GUILD_TEST_MYSQL_DSN`（库名 `guild_test`），跑 `go test ./internal/data -run 'TestResolveManuallyIfQuiet_' -count=1 -v`，要全 PASS、不得 SKIP；再跑 `go test ./cmd/assetopfix/... -count=1`。
- 做完把这一项的状态同步标到两处：`review-suggestions-remaining-handoff-20261126.md` §4 第 1 条，第 4 轮单 §6.1「还没做的」第 2 条。

### 3.3 各批记下、还没处理的点

逐条判断：是缺陷就修；确是有意的取舍，就在所属设计稿写明不改的理由。至少包括：

1. 删角收尾不删 `guild_war_player_state` 与 `guild_war_garrison`，后者还含玩家名与快照。要补进账号注销清单，并在 `docs/design/account-deletion.md` 登记新挂起项 `guild_war_credit_owed`（第 4 轮单 §7.4「顺带发现」）。**2026-11-27 已做（未编译）**：`data/role_fence_purge.go` 新增 `purgeWarRowsOfPlayer`，在清理事务里按锁序删他的守旗快照、个人帮战状态与待入账行；`logic/role_fence_purge.go` 在删角触发解散时提交后调 `CleanupGuildWarOnDisband` 释放灵脉；`role_fence_purge_tidb_test.go` 补三张表的断言；注销设计稿 §6.2 guild 行、挂起原因清单与五项已写。验证：`cd go/guild`，设 `GUILD_ROLE_TEST_DSN`，跑 `go test ./internal/data -run 'TestTiDBRoleFencePurge' -count=1 -v`，全 PASS、不得 SKIP。
2. 两处客户端可见的文字：
   - `guild.proto` 里 EXPIRED 的注释仍写「已原路退回」，实际还包括「退款排在待入账里」和「发布者已删除、退款放弃」（§7.4）；
   - 客户端 `BountyStatusLabel` 没有状态 6（ESCROW_FAILED）的文案（§7.1）。
   - 改之前先查 Java 仓 `PARITY.md` 与 `docs/reference/mmorpg-client-contract-*.md`，两版共享的契约同批改。
   - **2026-11-27 已做（未生成、未编译）**：`proto/guild/guild.proto` 的 EXPIRED 注释改成实际含义；客户端 `Assets/Scripts/Game/Guild/GuildWarModels.cs` 的 `BountyStatusLabel` 补 `EscrowFailed`「扣款未成，已作废」（客户端生成代码里已有这个枚举值）；帮战设计稿 `guild-play-v2-implementation-plan.md` 代价与边界一条改写；Java `PARITY.md` 批 X 那条补一句。验证：`proto-gen-run` 之后在客户端仓依次跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`，0 错误。
3. 两处缺告警：待入账积压 `guild_war_credit_owed` 没有指标也没有告警（§7.4）；生死状待退银两超期打的 `metric=jianghu_duel_fee_overdue` 没有告警规则（§7.3）。**2026-11-27 已做（未编译）**：guild 加 gauge `guild_war_credit_owed_rows`（`WarRepo.CountWarCreditOwed`、`reportWarCreditOwedBacklog`，与历练待入队同一循环、同一封顶）与告警 `GuildWarCreditOwedBacklog`；`jianghu_duel_fee_overdue` 登记进 `asset-channel-alerts.yaml` 的 LogQL 值班查询段（与同段 scene 信号一样，Loki ruler 接线前不会自己响）。两份设计稿的对应句已改写。验证：`cd go/guild; go build ./...; go vet ./internal/logic/ ./internal/data/`；部署契约 `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1` 全 `[PASS]`（含 LogQL 契约对 `asset-channel-alerts.yaml` 的选择器核对）。
4. `CheckJianghuDuelEligibility` 不判「旧局的待退银两还没结清」，要到备战时才拒（§7.3）。**2026-11-27 已做（导表、生成、编译都没跑）**：
   - Tip 表 `//jianghu_error` 段末加 `JianghuDuelSilverFeePending`（`tools/scripts/jianghu_fee_pending_tip_xlsx_patch.py`，已写入表，幂等）；`proto/scene/scene.proto` 的 `CheckJianghuDuelEligibilityResponse` 加 `payer_silver_fee_pending = 4`。
   - scene `player_jianghu.cpp` 的 `CheckDuelEligibility`：普通战书发起方的待退标记还在时回 pending、不放行；match `jianghu_duel_logic.go` 先认 pending 回新码；`constants/jianghu_errors.go` 与其测试加一行。
   - 用例：`PlayerJianghuTest.SilverWritPendingForAnotherBattleRejectsTheNewPreparationWithoutTouchingTheMarker` 补三段。五项在 `battle-jianghu-grudge-duel-phase1-implementation-plan.md`「问题 4」。
   - 验证（顺序）：`dev.bat gen`（开跑前 `git status --short data/` 核别人的表改动），`proto-gen-run`；Tip 表客户端可见，客户端仓跑 `gen_messageids.ps1` 与 `client_compile_check.ps1`；`msbuild` 串行 `/m:1` 依次 `cpp/generated/table/table.vcxproj`、`cpp/generated/proto/proto.vcxproj`、`cpp/libs/services/scene/scene.vcxproj`、`cpp/tests/bag_test/bag_test.vcxproj`，跑 `bag_test.exe --gtest_filter=PlayerJianghuTest.*`；`cd go/match; go build ./...; go test ./internal/constants/ ./internal/logic/ -count=1`。
5. 锦标赛领奖遇到邮箱满，玩家看到的是内部错误，mail 的满箱提示没透传。响应里同时带着错误和已领取的部分，客户端怎么显示还没核（§7.2）。**2026-11-27 透传已做（未编译）**：只透传 `kMailRecipientBoxFull`，五项在 `season-team-tournament-v1.md` §7.7；客户端那一半 11-27 也改了（`mmorpg-client` 的 `TournamentClient.ClaimReward` 先并入已领取的条目再报错误，未编译；客户端仓跑 `tools/client_compile_check.ps1` 0 错误、EditMode `TournamentClientTests` 全绿）。
6. 寄售交付腿部分发放后，trade 侧没有出口。这是 mall#1 的残余，补法在 `docs/design/jubaozhai-market.md` §16.1。
   - 要改的是 `go/trade/internal/commerce/manual_plan.go`。当时它和双人复核会话的改动冲突，所以没做；11-27 核对，该文件已没有在途改动。
   - 双人复核的定案不改：规则只在后台流水里，批准号就是幂等键，OPS 只有一张目录。
   - **2026-11-27 已做（未编译）**：`manual_plan.go` 的 `planPartial` 放开人民币寄售交付腿的缺口补发（托管扣款腿的部分扣出仍拒绝）；`rmb.go` 的 `planRmb` 交付阶段沿 `currentLeg` 判结局，补发没到账转人工（`final_credit_failed`，可再 `reissue`），不走退款。用例 `TestPartialRmbDeliveryReissuesTheShortfallAndNeverRefunds`，五项在 `jubaozhai-market.md` §16.1「trade 侧的出口」。验证：`cd go/trade; go build ./...; go test ./internal/commerce/ -run 'TestPartial|TestFailedCredits|TestRedirect' -count=1 -v`，再跑 `go test ./internal/commerce/ -count=1`；真库 `COMMERCE_TEST_DSN` + `COMMERCE_REQUIRE_TIDB=1` 跑 `TestTiDBRmbCancelRefusesManualContractAfterPaidDeliveryWentPartial` 不回归。
7. 第 4 轮单 §7.1～§7.4「剩余与取舍」里别的条目也逐条过一遍。例如：开战入口没有走完整请求路径的集成用例；`PrepareBattle` 把复核时刻传给 `CommitPrepareSilverFee` 这一处没有直接单测。

### 3.4 选角页回选上限的用例（客户端仓，只加用例，不改产品代码）

- 全文在总入口 `new-computer-start-here-handoff-20261127.md` §3.1（11-27 从 review-suggestions 交接单 §8 移过去）：要钉的行为、用例步骤、可以抄的夹具都写了。做完在那边与本节都标状态。
- 它不依赖上面各项，在第 5 轮查找之前做完即可。写完在那一节标状态。

## 4. 文档（每改一条同批写，不留到最后）

- **设计稿**：在所属功能的设计稿写五项：问题、方案、为什么这样选、被否决的做法、代价与边界。不容易想到的好方案一定写进去。
  - 帮战：`docs/planning/guild-play-v2-implementation-plan.md`；
  - 生死状：`docs/planning/battle-jianghu-grudge-duel-phase1-implementation-plan.md`；
  - 锦标赛：`docs/planning/season-team-tournament-v1.md`；
  - 悬赏求书：求书设计稿；
  - 其余先 grep 找到所属的稿。
- **r2 文档**：
  - §3.12 写第 4 轮的复核结论；
  - §5.0 补第 4 轮各编号的最终状态：r2-gw#1~#8、r2-tn#1~#3、r2-jh#1~#3、r2-craft#1~#4、r2-mall#1、r2-ops#1~#2、r2-oa#1；
  - §1a 每条补一行入口；
  - §1「仍开着」把做完的划掉。
- **PROGRESS**：`docs/PROGRESS.md` 只追加，写清改了什么和验证命令，不贴大段原始输出。
- **文档索引**：新建文档后先跑 `python tools/scripts/gen_docs_index.py --check`，再生成。diff 只许加行；删文档时只许删那几行。

## 5. 交 Gemini 只跑（生成、编译、测试、冒烟）

- **顺序**：
  1. 先统一生成一遍，做法在总入口 §4 第 1 步。
  2. 再按这些命令跑：第 4 轮单 §7.1～§7.4 的「验证」、§6.1「Gemini 验证顺序」、求书设计稿 §11.1 末与 §9、本单 §3.2。
  3. r2 全线的总验证 `data-overwrite-audit-r2-handoff-20261126.md`，等本单 §2～§4 做完再整体跑。
- **红了**：Gemini 保留首错原文交回 Claude Code，自己不改代码；Claude Code 修完再交。
- **结果**：Gemini 跑完在 PROGRESS 追加结果。

## 6. 第 5 轮查找（§2～§4 落完后开）

- **为什么要跑**：规则是连续两轮没有新发现才停。第 4 轮有新发现，所以要跑第 5 轮。第 5 轮再有新发现，照第 4 轮的路子修完，再跑第 6 轮。
- **查什么**：`D_ROUNDS[4]` 自由视角三路。第 4 轮没重做旧核心机制的网络分区推演，并进这一轮一起做：player_store 写权、handoff_fence、发号器、写者名册、shardowner，以及 muduo 半开连接。
- **脚本与旧参数**：先读 `git show ccf2840b2:docs/handoff/data-overwrite-audit-r2-20261019/README.md`。同目录有脚本 `audit-workflow.js`、`audit-workflow-wave34.js`（`D_ROUNDS` 定义在里面）、第 4 轮的参数 `dsweep-round4-args.json`，以及第 2、3 轮的结果目录。
- **去重清单共 133 条**：
  - 第 3 轮 104 条，在上面的旧目录；
  - 11-19 / 11-20 评审修正 7 条；
  - 第 4 轮 17 条，在第 4 轮单 §1；
  - 修复时新发现的 5 条，在第 4 轮单 §7.4。
- **顺带看两处线索**（第 4 轮单 §6.1 末条）：
  - 官方订单的 `CREATE_REFUND` 同样不等 trade kind-1 合同的结局；
  - `applyJianghuDuelResult` 查档案失败时按 owner_revision=0 入账，取的是处理时刻的 owner_revision。
- **用量**：
  - 这一轮要跑多代理 Workflow，开跑前先向负责人说明大概用量，拿到同意。
  - 旧账号上每个只读查找代理约占 7% 周额度，三路查找约 20%，对抗复核每条另算。全部模型周额度过 80% 就按总入口 §2 停。
  - 分批跑，别同时开多条 workflow。别给正在跑的代理发 SendMessage，那会把它「恢复」成认不出自己刚做的改动。
- **结果放哪**：原文存到 `docs/handoff/data-overwrite-audit-r2-round5-<日期>/`；发现写进 r2 文档新开的 §3.13 和 §5.0。有要修的，照本单的格式在本单追加一节。
- 别把已定案的行为当缺陷报，清单在 §7 末条。

## 7. 做事要点（旧电脑 Claude 记忆里的，AGENTS 和总入口没写全）

- **多会话共用工作区**：
  - 动手前先分清哪些改动是自己的；交接里写的提交与验证状态可能已经过时，先查后续的 PROGRESS 条目。
  - 别的会话的快照提交，可能正好在你写文件时把半截文件提交进去。看到快照带走了自己的文件，用 `git show HEAD:<文件> | gofmt -l` 核一遍 HEAD 里的副本。
- **子代理**：提示里写明不许把 `git show` 的输出重定向到 `%TEMP%`；结束后核对，有临时副本就删。
- **C++**：
  - 类和结构体都不放任何指针成员，`const char*` 和测试里的局部结构体也算，改用 `string_view` 或引用。门禁遇到第一个错就停，整个工程都不跑。
  - 新增 `.cpp` 只登记 `.vcxproj` 与 `.filters`。CMakeLists 由 `tools/scripts/build_linux.sh` 按 vcxproj 生成，别手改。
  - ECS 按需组件在写入口懒建，读路径不建，持久数据不懒建。
- **改名或删 C++ 成员**：之后 grep `tools/scripts/tests`。文本契约测试钉着成员名，脱节了 NotMatch 会空转、照样通过。
- **新加 match 后台循环**：同批登记 `loop_liveness_alerts_contract.tests.ps1` 的三处，并更新 assetop-redelivery 设计稿的点位清单。
- **新的 data_service 调用方**：配置要过 `go/shared/dsclient` 的 `Conf`。go-zero 的熔断按方法计失败、不分分片，一片失联会连健康的分片一起挡掉。
- **玩家存档里的集合要有上界**；裁记录之前先查它是不是去重键。r2-jh#3 就是把会被裁掉的行当成了去重锚点。
- **PowerShell**：双引号里变量紧跟冒号时要写 `${name}:`，否则整份脚本解析失败。改完 ps1 用 `[System.Management.Automation.Language.Parser]::ParseFile` 静态查语法。
- **Bash 工具**：长 heredoc 会解析失败，`awk -v` 会吞掉 Windows 路径里的反斜杠。
- **导表器**：只改模板时单独渲染核对，别跑整套导表，那会带进别的会话的表改动。
- **过时文档**：直接改正文，不能只加一句「已被取代」。
- **「按最标准的方法做」**：意思是不丢数据、强一致、性能高、可水平扩展、代码易懂，待拍板项直接按有依据的标准做法定。但一批改动超过 30 个文件，仍要先拿到负责人对这一批的同意。
- **Java 仓**：只在 `PARITY.md` 登记待做，不适用或已有等价实现时不加行。第 4 轮各批 11-26、11-27 已经登记了两节：「主仓 …round4-handoff… §7.1～§7.4」与「悬赏求书改走 trade 托管合同」。新改的照样补。
- **负责人用语音输入**：同音错字按读音还原，例如「代做」就是「待做」。
- **上下文快满**：还有活要做时，自己开新会话交接并接着做，不让负责人去开，也不因此把写代码交给 Gemini。
- **提交**：负责人没要求就不提交、不推送。要求提交时只带自己的改动，因为工作区常有别的会话的未提交文件：
  - 用私有索引暂存；
  - 混合文件按「HEAD + 自己的替换」造 blob；
  - `update-ref` 带旧值推进 main，最后对齐真索引。
- **已定案的行为，第 5 轮别当缺陷报**：出处在各设计稿，与设计稿不符时以设计稿为准。
  - 排空框架遇到位点越过保留范围就 HOLD，不跳过；
  - 混版设计 §8.2 的世代预检不做；
  - FIGHTING 冻结续冻上限是到期 + 15 分钟；
  - match 不可用时，ABORT 最晚约在截止 + 14.2 分钟；
  - go/db 运行期冷却已删，别接回；
  - 「单边失联丢 5 秒」已定案；
  - 发号器：声明指针不严格化，丢码时 rebind 不放行，本地超上界就拒绝；
  - 角色换主人：修订号未知不作废，不按修订号分目录；
  - 换场景节点时的单向消息顺序与旧场景消息闸；
  - 连接与重连时不做业务逻辑；
  - 玩家存档不进 Redis；
  - C++ 节点除主线程外，只保留 Kafka、日志、Agones 线程；
  - 客户端请求幂等账本一律不许整体清空。

## 8. 待负责人定

- 悬赏求书批 C 前提②：K8s 各档位要不要打开 trade 的 `AssetOp.Enabled`。
- 复核和写代码时拿不准的，追加在这里。

## 9. 收尾：做完就删

下面全部满足才算做完：

- §2 每批都有复核结论；
- §3 全部写完；
- §4 的文档写齐；
- §5 Gemini 跑完全绿，PROGRESS 里有结果；
- §6 起连续两轮没有新发现，中间的新发现也都修完、验完。

然后：

1. 把 r2 文档 §1「仍开着」改成收口状态，写上日期。
2. 删掉本单。下面两份交接单在 r2 这条线上已没有待做时，一并删：
   - 第 4 轮单和它的原始结果目录 `data-overwrite-audit-r2-round4-20261126/`；
   - `data-overwrite-audit-r2-handoff-20261126.md`。
   - 删之前，在 r2 文档 §1a 记一行：「交接原件在提交 `<hash>`，用 git show 读」。
   - `review-suggestions-remaining-handoff-20261126.md` 里还有别的线的待做，只标本线那项（§4 第 1 条）的状态，不删；§8 的用例已移到总入口 §3.1。
3. 在总入口 `new-computer-start-here-handoff-20261127.md` 里删掉对应的行，再更新文档索引（做法见总入口 §7.2「文档索引」）。这次 diff 只许删掉被删文档的那几行。
