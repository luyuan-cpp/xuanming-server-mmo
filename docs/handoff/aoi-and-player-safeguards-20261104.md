# Claude Code 交接：AOI 单向关注与两项玩家止损能力

2026-11-04。需求已确认，尚未落码。此前的 AOI 草稿已还原，从当前仓库实现开始。以下三项分批处理，复用现有接口、存盘和测试设施。

## 落码状态（2026-11-05，Claude，未编译）

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 1. AOI 单向关注 | 服务端已落码，待 Codex 验证 | 实现与验证命令见[设计文档](../design/aoi-directed-interest.md)「实现」 |
| 2. 单玩家物品封禁 | 进行中 | 见下方第 2 节 |
| 3. 异常收益窗口重置 | 服务端已落码，待 Codex 验证 | `AnomalyDetector::ClearPlayer` 与 `anomaly_detector_test.cpp` 四个新用例；跑 `currency_test` |

Java 版三项均登记为 `PARITY.md`「Java 待做」，本轮不写 Java 代码。

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
