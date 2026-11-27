# 悬赏求书迁 trade 托管合同:剩余工作交接(2026-11-27,换电脑接着做;全部做完后删掉本文件)

> 用法:在另一台电脑对 Claude Code 说「继续做完 handoff」,本文件就是数据覆写审计 r2-craft#1(悬赏求书)剩余工作的**唯一入口**。按 §2 → §3 → §4 → §5 → §6 的顺序做;每一步做完在本文件对应小节末尾写「已完成(日期,做了什么)」,全部做完按 §6 删掉本文件。
> 原来那台电脑已回收,它上面的会话记忆、草稿目录全部不在了,需要的东西都写在本文件和下面点名的仓库文档里。

## 0. 开工前

1. 会话门禁照 `AGENTS.md`:读 `AGENTS.md`、`docs/rules/ai-workflow.md`、`engineering-principles.md`、`invariants.md`、`proto-and-tables.md`、`cpp-runtime-and-build.md`;`git log -20 --oneline`。
2. 确认拿到的是负责人同步过来的最新 main:`git log --oneline -S "attemptAllowed" -- go/trade/internal/commerce/engine.go` 要有结果(批 A 的最后一处改动),`go/trade/internal/commerce/wanted_skill_book.go` 存在。查不到就停下问负责人,不要凭本文件重写批 A。
3. 硬规则(负责人定):
   - 只在 main 工作区改,不开分支、不开 worktree;不 commit / push(提交由负责人做)。
   - Claude Code 用最高档模型写服务端与客户端代码;**协议生成、导表、编译、测试、冒烟一律交 Gemini**(只读检查可以自己跑:`gofmt -l`、`python tools/scripts/check_log_text_english.py`、改了 C++ 跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`)。没有运行证据一律写「未编译,待 Gemini 验证」。
   - 项目没上线,玩家数据可以全部清空,上线前清档,不读旧格式、不做旧数据迁移。
   - Java 版(独立仓 `luyuan-java/xuanming-server-mmo`)不写代码,只在它的 `PARITY.md` 登记待做(已登记,见 §5)。
   - 查到的问题现在修,不留「以后再做」;要拍板的先按最标准的做法给出推荐,问负责人一句,负责人说「按最标准」就照推荐做。
   - 工作区常有别的会话的未提交改动:改文件前 `git diff HEAD -- <文件>`,只动自己的行;日志英文,注释与文档中文;tip 码只经 `data/tip/Tip.xlsx`(用幂等脚本加行,写法照 `tools/scripts/trade_wanted_skill_book_xlsx_patch.py`),不手写数字;生成物不手改。
   - 每改完一部分,同批把「问题、方案、为什么这样选、被否决的做法、代价与边界」写进设计稿 `docs/design/skill-book-wanted-order-trade-escrow.md` §11(负责人要求:重要流程、设计步骤、问题分析与解决办法都要进文档,没想到的好方案一定要记)。

## 1. 背景与已经做完的

- **问题(r2-craft#1,medium)**:scene 的 `PlayerCraftSystem::FulfillWantedOrder` 在同一帧里改两名玩家(删卖家的书、给卖家加 95% 银两、删发榜人的悬赏单、往发榜人书架加书),再对两人各存一次盘;两笔存盘互不相干,只落一边就多出一份银两或一本书。两名玩家各存各的盘,原子交换只能靠托管方,所以负责人 11-26 定:改走 go/trade 托管合同,顺带让悬赏榜全服可见(原先只看得到同场景节点、在线可改的玩家)。
- **方案**:trade 加第 7 种合同「求购单」(`COMMERCE_ORDER_KIND_SKILL_BOOK_WANTED = 7`)。发榜人是合同的买家,建单后托管悬赏银两(只扣流通银两)→ 挂榜 → 揭榜人托管一枚封卷道籍(道具 3414)→ trade 按 scene 扣出时交回的完整实例核对条款 → 相符先把书交给发榜人、到账后再把税后 95% 交给揭榜人(5% 不发即销毁);不符原样退书、单子回挂榜;发榜人收书被拒整单退回且退全额;每人至多 3 张生效单;撤单只在托管与挂榜阶段。完整设计在设计稿 §0~§9,落地口径以 §11.1 为准。
- **三批**:A trade 侧合同 → C 部署开通 → B scene 退役旧揭榜 + 客户端切换(负责人 11-26 晚批准)。
- **已做完(都在 main,未编译、未测试)**:
  - 批 A 全部:协议 `proto/trade/{commerce,jubaozhai,trade_table}.proto`(新 RPC `CreateWantedSkillBookOrder` / `FulfillWantedSkillBookOrder` / `ListWantedSkillBookOrders`,message_id 582 / 583 / 584);`go/trade/internal/commerce/{wanted_skill_book.go,engine.go,delivery_order.go,leg_tx_type.go,schedule.go}` 与用例;logic / server / config / svc / `go/trade/etc/trade.yaml`;Tip 两条(`TradeWantedOrderLimitReached`、`TradeWantedBookMismatch`)与 MessageLimiter 582~584 三行(脚本 `tools/scripts/trade_wanted_skill_book_xlsx_patch.py` 已跑)。
  - 复查修正:揭榜与寄售购买的尝试次数改成每人 3 次、整单 256 次(`engine.go` 的 `attemptAllowed`,设计稿 §11.1 第 7 条)。
  - 旧路径的书架上限(r2-craft#2)早已落码,导表未跑。
- **还没做**:§2 的批 A 验证(Gemini)、§3 批 C、§4 批 B、§5 文档收尾。批 B 落地之前 scene 的旧揭榜(两笔存盘)照旧运行,问题没有真正消失。

## 2. 第一步:交 Gemini 验批 A(只跑,红了把首错原文交回 Claude 改)

仓库根执行:
1. 导表:先 `git status --short data/` 看别的会话的表改动,再 `dev.bat gen`;`go/shared/generated/pb/table` 里要有 `TradeError_kTradeWantedOrderLimitReached`、`TradeError_kTradeWantedBookMismatch`(不导出来 go/trade 编不过),`skill_error` 段要有 `kSkillBookshelfFull`、`kSkillWantedPosterBookshelfFull`(scene 编不过的原因)。
2. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`;`proto/message_id.txt` 的 582~584 不变。
3. `go/trade` 下 `go build ./...`、`go vet ./...`、`go test ./internal/commerce/... ./internal/logic/... ./internal/config/... ./internal/constants/...`,点名 `TestWanted`、`TestAttemptAllowed`、`TestLegTxType`、`TestCommerceEntriesAnswerMissingContractWithOrderNotFound`、`TestCommerceEntriesKeepContractReadFaultsAsFaults`、`TestEtcYamlContractValues`、`TestWantedSkillBookRequiresAssetChannelAndTables`、`TestNoHandWrittenTipCodes`。
4. 真库:隔离 TiDB `COMMERCE_TEST_DSN` + `COMMERCE_REQUIRE_TIDB=1`,`go test ./internal/commerce/ -run 'TestTiDBWantedSkillBook|TestTiDBConsignment|TestTiDBRejectedBuyRetry|TestTiDBPlayerTrade' -count=1`。
5. 客户端 `mmorpg-client`:`tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`。
通过标准:全绿;`git diff` 里 `data/tip/Tip.xlsx` 只多两行、`data/MessageLimiter.xlsx` 只多三行。

## 3. 批 C:部署开通

目标:K8s 上 trade 能托管求购单(否则批 B 一切,集群里的悬赏求书就用不了)。

**要先问负责人的一件事**:K8s 各档位打不打开 trade 的资产通道 `AssetOp.Enabled`(现在 `tools/scripts/k8s_deploy.ps1` 给 trade 的 ConfigMap 所有档位固定写 false)。
- 推荐(最标准):打开,同时打开 `Commerce.WantedSkillBook.Enabled`。理由:悬赏求书是已有玩法,旧路径在每个档位都能用,不开就是功能倒退;打开资产通道不会顺带放开寄售、拍卖、撮合——它们还各有白名单(`Commerce.ItemConfigs`、`CurrencyTypes`、`AllowInstances`),开工时核对 K8s ConfigMap 里这几项是空的,空就什么都交易不了。
- 代价:打开要两边的签名密钥都在 Secret 里(trade 签发 `MMORPG_ASSET_OP_SECRET_TRADE`,scene 按调用方 trade 验签);部署前只读核对 Secret 的键名与长度,不读值(工程原则 §11.3「外部前置条件只读核对」)。
- 负责人说不开:批 C 只做下面的表与文档部分,批 B 照做,在设计稿写明「K8s 上悬赏求书不可用,直到打开资产通道」。

**改动**(改 `k8s_deploy.ps1` 前 `git diff HEAD` 看别的会话的未提交改动):
1. `tools/scripts/k8s_deploy.ps1` 的 trade ConfigMap:`AssetOp.Enabled`;打开时把 `go/trade/etc/trade.yaml` 里 AssetOp 的落地恢复与 `Cleanup*` 键一并补进去(trade.yaml 里 `AssetOp:` 段上方的注释写着这条要求);`Commerce.WantedSkillBook.Enabled`;`TableDir`。密钥只经 `secretKeyRef` 注入,ConfigMap 与 `-DryRun` 输出里只出现变量名(`deploy/k8s/AGENTS.md`)。
2. 配表:trade 开了求购单才读表(`go/trade/internal/svc/servicecontext.go` 里 `tablebundle.LoadAtStartup`,校验钩子要求 `Skill` 与 `SkillScrollCraft`)。核对 trade 镜像里有没有 `generated/tables`;按 `docs/design/config-table-hot-reload.md` 的服务接入口径让 trade 和 match / mail 一样拿到表(镜像自带或表包 sidecar,STATUS.json、POINTER.json 的字段名照它对齐,不自创);trade 的 main(`go/trade/trade.go`)补 `--validate-tables` 入口,写法照 `go/mail/mail.go` 的 `tablebundle.MaybeRunValidateTablesCLI`。
3. 契约测试:`tools/scripts/tests/k8s_*_contract.tests.ps1` 里钉 trade ConfigMap 的几份同批改;改完把断言模式在现代码上逐条文本匹配一遍(`Assert-NotMatch` 按旧名会空转)。
4. `docs/ops/trade-commerce-quarantine-runbook.md` 加 kind 7 口径:①退书腿 `seller_return_<n>` 没到账用 reissue 补;②有过「不符退书」的单不能整单退回(`manual_plan.go` 的 `refundCommon` 见到已到账的退书腿就拒绝,这是有意的 fail-closed,防止把书退两遍),用 reissue / resume;③发榜人收书被拒由引擎自动整单退回、退全额,不用人工。`deploy/k8s/README.md` 加一行。
5. 验证(Gemini):契约测试;`k8s_deploy.ps1 -DryRun` 输出里 trade ConfigMap 的新键;kind 冒烟见 §4 第 9 条。

## 4. 批 B:scene 退役旧揭榜 + 客户端切换(必须同一批,否则客户端调不存在的接口)

**服务端(C++ 与协议)**:
1. `proto/scene/player_skill.proto`:删 `PostSkillBookWantedOrder`、`CancelSkillBookWantedOrder`、`ListSkillBookWantedOrders`、`FulfillSkillBookWantedOrder` 四条 RPC(message_id 560~563)及其请求 / 响应消息与 `SkillBookWantedOrderInfo`;`SkillCraftingStateSnapshot.my_wanted_orders = 23` 写 `reserved 23;`。生成器会把四个号自动转墓碑:一轮生成最多新转 4 条(`maxNewMessageTombstonesPerRun = 4`),正好卡线,**同一轮生成里不要再删别的 RPC**。
2. `proto/common/component/player_craft_comp.proto`:`wanted_orders = 16`、`next_wanted_order_seq = 17` 写 `reserved`,删 `SkillBookWantedOrder` 消息。这是玩家存档字段:按混版本字段保护台账(`docs/design/mixed-version-persist-field-protection.md`),生成器会在 `proto/persist_schema_gen.txt` 写 `retired=` 行,diff 只许多退役行。
3. `proto/common/component/client_request_ledger_comp.proto`:`wanted_order_id = 40`、`wanted_order_gold = 41` 写 `reserved`。
4. `cpp/libs/services/scene/player/system/player_craft.{h,cpp}`:删 `PostWantedOrder`、`CancelWantedOrder`、`ListWantedOrders`、`FulfillWantedOrder` 与只给它们用的辅助函数(`FindWantedOrderIndex`、`FillWantedOrderInfo`、`RemoveWantedOrderAt` 等,删前 grep 确认没有别的调用者),以及 `FillStateSnapshot` 里填 `my_wanted_orders` 的部分。`SealSkillBook` / `UnsealSkillBook` 保留(揭榜书架书要先封卷)。
5. `client_request_ledger.{h,cpp}`:`ClientRequestKind` 的 `kPostWantedOrder = 26`、`kCancelWantedOrder = 27`、`kFulfillWantedOrder = 28` 只退役不复用(枚举值留着并注明退役,或按该文件既有的退役写法);删 `ClientRequestResult` 里的 `wantedOrderId` / `wantedOrderGold` 及其读写。
6. `equip_craft_quality_rules.h`:删 `kWantedOrderMinGold`、`kWantedOrderMaxGold`、`kMaxActiveWantedOrders`、`ValidWantedOrderGold`。**`kProxyTaxBps`、`ProxyTaxGold`、`ProxyNetGold`、`kSealedSkillBookConfigId` 不能删**(名匠代打分成与封卷还在用;trade 的跨语言用例 `wanted_skill_book_crosslang_test.go` 照比它们,对已删的三项会自动跳过)。
7. `cpp/nodes/scene/handler/rpc/player/player_skill_handler.{h,cpp}`:删四个 handler 守护段里的代码(生成器重新生成后方法会消失,守护段内自写代码要跟着删);`player_handoff_gate.h`:删四条消息的交接分类。`kSkillWantedPosterBookshelfFull` 随旧揭榜不再使用,Tip 行保留、不复用。
8. `cpp/tests/bag_test/player_craft_quality_test.cpp`:删 V5 悬赏用例,以及书架上限用例 `BookshelfCapRejectsCraftUnsealAndFulfillWithoutSideEffects` 里的揭榜段(制书、开卷两处满架拒绝的断言保留,用例改名去掉 Fulfill)。改完跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`。
9. `data/MessageLimiter.xlsx`:删 560~563 四行(写一个幂等脚本,照 `tools/scripts/trade_wanted_skill_book_xlsx_patch.py`;先核对导表器允不允许限流行指向墓碑号,不允许就必须删)。

**客户端(`mmorpg-client`,同批改)**:
10. `Assets/Scripts/Game/Jubaozhai/CommerceClient.cs`:加三条调用(发榜、揭榜、悬赏榜);撤单用已有的 `CancelCommerceOrder`,查单用 `GetCommerceOrder`。
11. `Assets/Scripts/Game/Skills/SkillCraftClient.cs`、`Assets/Scripts/UI/Ugui/Skills/SkillCraftModels.cs`、`SkillCraftWindow.cs`、`Assets/Scripts/Game/GameClient.cs`:改调 trade,删掉对 scene 四条消息与 `my_wanted_orders` 的引用。流程:
    - 发榜:`CreateWantedSkillBookOrder` 回来通常是「托管中」;按 `GetCommerceOrder` 轮询到挂榜(状态 1)或撤单结案(银两不足,状态 7),界面有过渡态。
    - 揭榜书架里的书:先调 scene 现有的 `SealSkillBook`(免费)拿到封卷道籍的道具号,再 `FulfillWantedSkillBookOrder`;界面上仍是一键。揭榜失败书留在背包,可再开卷,不丢。
    - 揭榜结果:**按同一个请求号重发揭榜**轮询——交易中、交付中、已成交回订单;书没扣到回 `TradeAssetNotTradable`;书不符、已原样退回回 `TradeWantedBookMismatch`;换了书回 `TradeRequestConflict`。别人正在交书时揭榜回 `TradeOrderStateConflict`(显示「交易中」)。同一角色对同一张单最多试 3 次。
    - 「我的悬赏」读 `ListWantedSkillBookOrders(only_mine=true)`(含托管中、交易中、交付中);公开榜按悬赏额从高到低,技能筛选是页内过滤,一页可能不满,按 `next_cursor` 继续取,空游标表示没有下一页。
    - 阶段文案按 `WantedSkillBookOrderPhase`;税后实得用服务端给的 `seller_net_gold`,客户端不自己算税。成交后发榜人收到的是背包里一枚封卷道籍(可一键开卷入书架)。
12. `Assets/Tests/EditMode/Skills/SkillCraftClientTests.cs`:用例改写成调 trade 的新流程。
13. 验证(Gemini):`proto-gen-run` → 串行 MSBuild(`/m:1`)scene 与 `bag_test` → `bag_test.exe --gtest_filter=PlayerCraftQualityTest.*:EquipCraftQualityRulesTest.*` → `test_cpp_no_raw_pointer_member.py` → 客户端 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1` 与 EditMode 用例 → kind 冒烟:发榜、揭榜(书架书与背包封卷两种)、撤单、发榜人背包满(书等腾出一格才到账,揭榜人银两随后到账)、发榜人被获取封禁时整单退回且退全额。

## 5. 文档收尾(每批做完同批写)

- 设计稿 `docs/design/skill-book-wanted-order-trade-escrow.md`:§11.2 改成批 C、B 的实施记录(五项:问题、方案、为什么这样选、被否决的做法、代价与边界);文件头状态改成实际状态。
- 策划稿 `docs/planning/equipment-quality-and-skill-crafting-v2-v5.md` §2.4 / §4:同步体验变化(成交后发榜人收到背包里的封卷道籍;有托管中 / 交易中过渡态;悬赏榜全服可见;同一角色对同一张单最多试 3 次)。
- 第 4 轮交接单 `docs/handoff/data-overwrite-audit-r2-round4-handoff-20261126.md` §6.1 的 r2-craft#1 行改成实际状态;审计文档 `docs/design/data-overwrite-rollback-audit-r2-20261019.md` §5.0 补 r2-craft#1 的最终状态。
- `docs/PROGRESS.md` 纯追加一条(改了什么、验证命令与结果)。
- Java 仓 `PARITY.md` 已有「2026-11-27 悬赏求书改走 trade 托管合同」一节(客户端契约变化与 Java 待做),批 B 落地后补一句「客户端已切、scene 560~563 已退役」。

## 6. 完成判据与收尾

全部满足才算做完:
1. 批 C、批 B 的服务端与客户端代码写完,§3、§4 逐条有「已完成」记录;scene 里不再有同帧改两名玩家的揭榜代码(`grep -rn "FulfillWantedOrder\|wanted_orders" cpp/libs cpp/nodes` 没有结果)。
2. §5 的文档都写完。
3. 交给 Gemini 的命令写在 `docs/PROGRESS.md` 的新条目里(Gemini 跑完红了交回 Claude 改,不算做完)。
4. 删掉本文件,跑 `python tools/scripts/gen_docs_index.py` 更新 `docs/README.md`(diff 只许少这一行);第 4 轮交接单与设计稿 §11.2 里指向本文件的链接一并删掉。
