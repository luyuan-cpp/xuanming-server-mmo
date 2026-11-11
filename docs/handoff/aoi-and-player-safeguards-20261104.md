# Claude Code 交接：AOI 单向关注与两项玩家止损能力

2026-11-04。需求已确认，尚未落码。此前的 AOI 草稿已还原，从当前仓库实现开始。以下三项分批处理，复用现有接口、存盘和测试设施。

## 落码与验证状态（2026-11-10 核对，Claude）

三项服务端代码都已进 main，协议已生成并提交。下表的「已验证」都是 Codex 的运行原件，路径相对 `build/verification/`。

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 1. AOI 单向关注 | 已验证 | 11-08 `naming-audit-20261108/cpp-tests`：`aoi_test` 84 条、`aoi_delivery_test` 32 条、`buff_test` 37 条、`skill_test` 80 条、`cross_zone_test` 458 条全过。实现见[设计文档](../design/aoi-directed-interest.md)「实现」 |
| 2. 单玩家物品封禁 | 已验证，剩查询接口的两条真库用例 | 判定唯一入口 `GainBlockService::IsItemGainBlocked`；封禁表 `PlayerItemGainBlockComp` 存档字段 24（PROFILE 行），与收件箱账本同一次存盘、同一事务提交；GM 类型 5 / 6 与载荷 `GmItemGainPayload`。11-06 `aps-1106-014851`：协议生成两轮摘要无差异，player_inbox 单测、vet、gofmt 与真库集成 38 条全过（含 `TestIntegrationItemGainSanctionsShareTheGmStreamInOrder`）。11-08 `naming-audit-20261108`：`bag_test` 932 条全过（物品封禁 12 条），`currency_test` 324 条全过（GM 执行器与收件箱的物品封禁 8 条）。GM 查询 `PlayerInboxAdmin.GetItemGainBlocks`（data_service 新增只读 `GetPlayerItemGainBlock`）：11-06 `battle-handoff-20261106-r4` player_inbox 单测 191 条全过（含本功能 6 条），11-08 data_service 的 `TestGetPlayerItemGainBlock_*` 5 条与 `TestReadPlayerStoreProfile*` 3 条通过。**还没跑**：`TestIntegrationItemGainBlocksListsPendingItemSanctions`（晚于 aps 那轮写的）与 `TestTiDBReadPlayerStoreProfile`（没配专用 TiDB 时跳过）。设计与运维见 [player-inbox-service.md](../design/player-inbox-service.md) §2.2、[player-inbox-runbook.md](../ops/player-inbox-runbook.md) §4.1 |
| 3. 异常收益窗口重置 | 已验证 | `AnomalyDetector::ClearPlayer` 与 `anomaly_detector_test.cpp` 四个新用例，随 11-08 `currency_test` 324 条全过 |

11-10 只改了日志与错误文案（AGENTS §2 要求日志用英文）和三处过时文档，要补跑的命令见下面「Codex 验证顺序」第 6 步。

Java 版三项均登记为 `PARITY.md`「Java 待做」，本轮不写 Java 代码。

需要负责人知道的口径（已写进对应设计文档）：

- 战斗结算遇到被封的掉落是**剔除该项、其余照发**，这一局不补发；其余入口（背包三个入包函数、消耗换取、资产转入、商店）是**整批拒绝**。前者由战斗线定，避免整笔结算因一件掉落卡住。
- BagService 全服封禁的拒绝码由 `kInvalidParameter`（带调用栈）改为 `kAssetBlocked`（27005），任务、活跃度、GM 发物、取下宝石等入口的客户端可见码随之变化，Java 版要同批改。
- 取下宝石时宝石被封：宝石留在装备上，回 27005，解封后照常取下。
- `player_inbox.yaml` 默认 `EnqueueTypes` 不含任何 GM 类型（与货币封禁现状一致），要用 GM 处置需改部署配置。
- ~~没有「查询某玩家物品封禁表」的 GM 接口~~ 2026-11-06 已补（负责人要求做完）：`PlayerInboxAdmin.GetItemGainBlocks` 回已落盘的本人封禁表与还没生效的封禁 / 解封单，先读收件箱、后读存档，保证一条单至少出现在其中一边；不含全服封禁。见设计 §2.2「运维查询」。

### Codex 验证顺序（三项合并）

在仓库根目录 `D:\luyuan\xuanming-server-mmo-cpp` 用 PowerShell 执行；工作区有多条线的未提交改动，先 `git status` 记现场，不清理别人的改动。

1. 协议生成：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`（生成器源码有变化或无工具时先跑 `-Command proto-gen-build`）。通过标准：`player_item_gain_block_comp.pb.{h,cc,go}` 存在并登记进 `proto.vcxproj`、`.filters`、`CMakeLists.txt`；`player_inbox.pb.h` 有类型 5、6 与 `PlayerInboxMessageType_ARRAYSIZE = 7`；`proto/persist_schema_gen.txt` 有 `PlayerItemGainBlockComp#1` 与 `player_database#24` 两行；`go/proto/data_service/data_service{,_grpc}.pb.go` 有 `GetPlayerItemGainBlock`，`go/proto/player_inbox/player_inbox_admin{,_grpc}.pb.go` 有 `GetItemGainBlocks` 与 `PlayerItemGainSanctionPending`；重跑后除别人的改动外无新差异。
2. C++ 编译，Debug|x64，`MSBuild <proj> /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal`，按序：`cpp/generated/proto/proto.vcxproj` → `cpp/libs/modules/modules.vcxproj` → `cpp/libs/services/scene/scene.vcxproj` → `cpp/tests/currency_test/currency_test.vcxproj` → `cpp/tests/bag_test/bag_test.vcxproj` → `cpp/tests/cross_zone_test/cross_zone_test.vcxproj` → `cpp/tests/aoi_test/aoi_test.vcxproj` → `cpp/tests/aoi_delivery_test/aoi_delivery_test.vcxproj` → `cpp/tests/buff_test/buff_test.vcxproj` → `cpp/tests/skill_test/skill_test.vcxproj` → `cpp/nodes/scene/scene.vcxproj`。库必须先于测试编（测试链接预编好的 lib）。通过标准 0 error。
3. C++ 测试：`pwsh tools/scripts/run_cpp_tests.ps1 -Filter <工程> -LogDir build/verification/cpp-tests`，依次跑 `currency_test`、`bag_test`、`cross_zone_test`、`aoi_test`、`aoi_delivery_test`、`buff_test`、`skill_test`，全部用例通过。资产通道的三个物品用例要求 `bin/etc` 的物品表可读，读不到算环境问题，要报告。
4. Go：在 `go/player_inbox` 下跑 `gofmt -l .`（无输出）、`go vet ./...`、`go test ./... -count=1`；集成测试用自起的一次性 MySQL/TiDB（库名 `player_inbox_it`，不要借 `codex-*` 容器），设 `PLAYER_INBOX_TEST_MYSQL_DSN` 与 `PLAYER_INBOX_REQUIRE_MYSQL_TESTS=1` 后跑 `go test -tags integration ./internal/inbox/... -count=1 -v`，`TestIntegrationItemGainSanctionsShareTheGmStreamInOrder` 与 `TestIntegrationItemGainBlocksListsPendingItemSanctions` 必须 PASS，SKIP 不算；Access denied 时改 `go test -c` 编出再跑。在 `go/shared` 下跑 `go vet ./inboxcontract/...` 与 `go test ./inboxcontract/... -count=1`。在 `go/data_service` 下跑 `gofmt -l ./internal ./data_service.go`（无输出）、`go vet ./...`、`go test ./internal/logic/... ./internal/server/... ./internal/store/... -count=1`，新增 `TestGetPlayerItemGainBlock_*` 五个与 `TestReadPlayerStoreProfile*` 三个必须 PASS；有专用 TiDB 时 `TestTiDBReadPlayerStoreProfile` 也要 PASS（未配置时 SKIP，按环境缺失报告）。player_inbox 的新单测是 `TestItemGainSanctionDomainIsTheGmDomain`、`TestItemGainBlocksRejectsBeforeTouchingStores`、`TestItemGainPendingViews`、`TestDataServiceItemGainBlocks`，以及 `TestAdminServerFoldsStoreFaultsInBand` / `TestAdminServerPassesThroughAndWires` 里新增的断言。
5. 失败时保留 MSBuild 错误摘要（文件、行号、错误码）、gtest 失败用例名与断言输出、`go test -v` 完整输出，回报后等待决策，不要自行改断言或跳过用例。
6. **2026-11-10 补跑（前 1~4 步其余部分已有原件，见开头状态表）**，在仓库根目录用 PowerShell，按序：
   1. data_service（只改了 `readOwnerAndRow` 的错误文案）：`cd go/data_service; gofmt -l ./internal/store; go vet ./internal/store/...; go test ./internal/store/... -run 'TestReadPlayerStore' -count=1 -v`。通过标准：gofmt 无输出，`TestReadPlayerStoreAssetReportsABadStorage`、`TestReadPlayerStoreAssetReadsOwnerAndAsset` 与三条 `TestReadPlayerStoreProfile*` 全 PASS。
   2. data_service 真库用例：有专用 TiDB 时设 `ACCOUNT_DIRECTORY_TEST_DSN`（只能指一次性实例，用例会建随机库）与 `ACCOUNT_DIRECTORY_REQUIRE_TIDB=1`，跑 `go test ./internal/store/... -run 'TestTiDBReadPlayerStore' -count=1 -v`，`TestTiDBReadPlayerStoreAsset` 与 `TestTiDBReadPlayerStoreProfile` 必须 PASS，SKIP 不算；没有专用 TiDB 就如实报告「环境缺失、未跑」。
   3. player_inbox 真库集成：同第 4 步的一次性库与两个环境变量，跑 `cd go/player_inbox; go test -tags integration ./internal/inbox/... -run 'TestIntegrationItemGain' -count=1 -v`，`TestIntegrationItemGainSanctionsShareTheGmStreamInOrder` 与 `TestIntegrationItemGainBlocksListsPendingItemSanctions` 都必须 PASS。
   4. C++（`player_equip.cpp` 只改了一条 WARN 的文案）：`MSBuild cpp/libs/services/scene/scene.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal`，再编 `cpp/tests/bag_test/bag_test.vcxproj`（测试链接预编好的 lib，库必须先编），然后 `build\cpp\tests\bag_test.exe --gtest_filter=PlayerEquipTest.RemoveGem*:PlayerItemGainBlockTest.*`。通过标准 0 error、用例全过。

## 1. 补完整 AOI 通用单向关注

以[独立设计文档](../design/aoi-directed-interest.md)为需求和验收依据。采用知乎文章的模型：网格提供附近候选，实际关注关系决定客户端看谁和收谁的移动。玩法可关注任意符合条件的同场景实体，任务、技能、好友、队伍等通过各自来源接入。维护正向关注和反向观察者索引；走出邻格保留显式关注，离场和销毁只清本实体的出入关系，禁止全节点扫描。旧 `Pin/Unpin` 仅作参考。

主要验证：`aoi_test`、`aoi_delivery_test`，以及涉及队伍、Buff 隐身和移动的现有测试。重点是范围外的持续移动、单向发送、多个来源的生命周期和离场只通知一次。

## 2. 单玩家物品获取封禁：持久化＋GM

- 按 `player_id + item_config_id` 封禁、解禁、判断。只限制该玩家今后获得指定配置的物品，不扣已有库存、不影响其他玩家。全服封禁继续独立生效。
- 复用 [GM 处置收件箱](../design/player-inbox-service.md)的鉴权、审计、有序执行和去重。封禁状态与对应执行账本同一次持久化，重登、换节点、服务器重启后继续生效。参考现有货币封禁的实现；旧 `PlayerItemBlockList` 和旧协议只供考察历史。
- 核对所有真实获得入口：[`BagService`](../../cpp/libs/modules/bag/bag_service.cpp) 单件和两种批量入包、战斗结算 `ConsumeInventoryAndGrant`、临时包兜底、商店、[`AssetOpSystem`](../../cpp/libs/services/scene/player/system/asset_op_system.cpp) 资产转入（含按预设实例直接入包）。复用同一个封禁判定；存档还原和同一所有者内部搬运不当作新增获得。
- 发放拒绝须发生在扣款、扣材料和提交前。混合批次中含封禁物品时，不能留下半笔发放或已扣未发；调用方不能把封禁当成容量不足改投临时包。保留现有资产拒绝、可重试和账本结局的区分。

主要验证：指定玩家与物品拦截、另一玩家正常获得、封禁与解禁重复执行、存档往返、封禁前已持有物品不受影响、批量和临时包不绕过。涉及 `bag_test`、`currency_test` 中的资产及 GM 收件箱测试，以及 Go `player_inbox` 的契约和入箱测试。

## 3. 单玩家异常收益检测窗口重置

恢复 [`AnomalyDetector`](../../cpp/libs/modules/transaction_log/anomaly_detector.h) 的内部接口 `ClearPlayer`。在玩家所属场景线程上，仅移除指定玩家的货币和物品收益统计窗口；空或过期句柄安全，重复调用安全，下一笔收益重新开始统计。不改阈值、既有告警、流水、其他玩家窗口，不需要新增 GM RPC。

预计改声明、实现和 [`anomaly_detector_test.cpp`](../../cpp/tests/currency_test/anomaly_detector_test.cpp) 共三个文件。用 `currency_test` 验证双玩家隔离、重置后重新计数及过期句柄不能影响复用槽位的玩家。

## 交付约束

遵循 [AGENTS.md](../../AGENTS.md) 的启动门禁和范围约束。Claude Code 编写代码、文档和测试，交付时列出目标工程、工作目录、验证命令和通过条件；编译、测试及压测由 Codex 执行，C++ MSBuild 串行 `/m:1`。若修改协议，使用服务端生成配置，不手改生成产物；按 §12 记录 Java 版对齐状态。移速 Buff、Buff 帧末移除、旧路由 RPC、旧副本建房协议不在这次任务中。
