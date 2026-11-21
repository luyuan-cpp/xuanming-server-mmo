# 角色形象切换（10 元角色形象卡）与 1 小时免费试用设计

## 1. 目标与边界

1. **单永久形象模型**：每名角色同一时刻只持有 **1 个永久角色形象**（`PlayerProfileComp.appearance_id`），不维护永久解锁衣柜。切换其他形象时，固定消耗 **1 张角色形象卡**（道具 `3402`，官方商城定价 **10 元 CNY**，`price_minor = 1000`），覆盖永久形象；跨职业/性别在全部已启用的可选形象间自由切换。
2. **1 小时免费试用（bit 位图存储）**：每个可选形象每名角色享有 **1 次免费试用**（默认 `3600` 秒 = 1 小时）。试用记录采用 `CharacterAppearance` 表的 `(cfg_bit_index) = true` 紧凑位图存储在 `PlayerProfileComp.appearance_trial_used_bits`（低位在前，未知高位 bit 完整保留）。
   - 试用期间大世界 AOI（`ActorCreateS2C` + `ActorAppearanceChangedS2C` 实时广播）与战斗快照（`BattlePlayerSnapshot`）均展示试用形象，不改选角永久形象。
   - 试用期间支持手动提前取消（`CancelAppearanceTrial`）或直接试用另一个未试用形象（覆盖当前试用并记录新 bit）；1 小时到期由 scene 1 秒节拍自动还原永久形象并广播。
3. **跨端对齐**：权威 C++/Go 服（`xuanming-server-mmo-cpp`）、Java 版（`xuanming-server-mmo`）与 Unity 客户端（`mmorpg-client`）共享同一份表结构、proto 协议、Tip 错误码（`33000..33006`）与 bit 索引映射。

## 2. 配表与稳定位索引

| 表名 | 文件 | 关键列 | 说明 |
|---|---|---|---|
| `CharacterAppearance` | `data/CharacterAppearance.xlsx` + `data/schema/characterappearance_table.proto` | `id [(cfg_bit_index) = true]`、`appearance_id`、`name`、`enabled`、`sort_order`、`desc` | 开启 `cfg_ids_permanent = true`，主键永久不可复用；映射落在 `tools/data_table_exporter/state/mapping/table_index_mapping/characterappearance_mapping.json`（`1..11 -> bit 0..10`） |
| `AppearanceRule` | `data/AppearanceRule.xlsx` + `data/schema/appearancerule_table.proto` | `id=1`、`card_item_id=3402 [(cfg_fk)="Item"]`、`card_item_count=1`、`trial_duration_seconds=3600`、`mall_sku_id=3402`、`mall_price_fen=1000` | 单行规则表，支持热更校验 |
| `Item` | `data/Item.xlsx` | `id=3402`、`name=角色形象卡`、`max_stack_size=99`、`icon_key=UI/Icons/item_appearance_card` | 商城直发进玩家人物背包 |
| `Tip` | `data/tip/Tip.xlsx` | `//appearance_error base=33000 width=1000`（`33000..33006`） | `kAppearanceInvalid` ~ `kAppearanceUnavailable (fault=1)` |

## 3. 官方商城 10 元 SKU 配置（`java/mall_game`）

官方商城通过 `pms_sku_stock` + `commerce_sku_grant` 承载人民币 SKU；部署或初始化测试环境时使用运维/管理接口 `POST /internal/admin/skus/create` 创建 10 元「角色形象卡」SKU（与 `AppearanceRule` 保持一致）：

```json
{
  "skuId": 3402,
  "productId": 3402,
  "skuCode": "role-appearance-card",
  "priceMinor": 1000,
  "currency": "CNY",
  "stock": 999999,
  "published": true,
  "itemConfigId": 3402,
  "countPerUnit": 1,
  "title": "角色形象卡"
}
```

支付成功后由既有商城发货链发放 `Item 3402 × quantity` 至玩家背包/收件箱，客户端易容面板的「商城购买 ¥10」按钮直接跳转官方商城页签。

## 4. 协议与幂等契约

协议文件：`proto/scene/player_appearance.proto`（`SceneAppearanceClientPlayer`，已登记进 `player_handoff_gate.h` 换节点可重投白名单）。

- `GetAppearanceInfo`：返回永久形象、当前生效形象、试用态与服务器墙钟 `server_now`、规则参数、可用形象卡张数（已扣预留）、紧凑位图原样与排序后的可选形象清单。
- `SwitchAppearance(client_request_id, appearance_id)`：
  - `ClientRequestKind::kSwitchAppearance = 7`，查账排在一切状态预检之前；同键同参重放直接回 `replayed = true`；同键换参回 `kInvalidParameter`。
  - 扣卡走 `BagService::RemoveItemsExact(..., TX_APPEARANCE_SWITCH, client_request_id)`，尊重物品预留并写资产流水。
  - 成功后覆盖 `appearance_id`、清理当前试用、递增 `appearance_switch_seq`、请求关键进度存盘、实时广播 `ActorAppearanceChangedS2C`；存盘提交后由 `OnSaveCommitted` 签名调用 `DataService.UpdateRoleAppearance` 更新 `account_role.role_proto`（瞬时故障按 2s/4s/8s/16s/30s 退避重试）。
- `StartAppearanceTrial(client_request_id, appearance_id)`：
  - `ClientRequestKind::kStartAppearanceTrial = 8`。
  - 校验目标启用且非当前永久形象、对应 bit 未置位；置位 `appearance_trial_used_bits`（保留未知高位 bit）、写 `trial_appearance_id` 与 `trial_expires_at = now + trial_duration_seconds`、请求存盘并实时广播 `ActorAppearanceChangedS2C(is_trial=true)`。
- `CancelAppearanceTrial`：清除试用态、请求存盘并广播还原永久形象。
- `NotifyActorAppearanceChanged` / `NotifyAppearanceState`：`CLIENT_RPC_PUSH`，用于本人与 AOI 周边玩家实时换皮以及 1 小时试用到期自动还原通知。

## 5. 验证清单

- C++ 预检：`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`
- Go `data_service` 单测：`cd go/data_service && go test ./internal/server/... -run TestUpdateRoleAppearance -v`
- C++ 构建与单测（需单线程 MSBuild 门禁）：`msbuild /m:1 cpp/game.sln` 后运行 `bag_test --gtest_filter="PlayerAppearance*:ClientRequestLedgerTest*"`
