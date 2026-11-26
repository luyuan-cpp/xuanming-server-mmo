# 激励视频广告与绑定银两（`CURRENCY_TYPE_BIND_GOLD`）全端技术与零拒审工程设计

> 日期：2026-11-26  
> 状态：**技术设计定稿落盘 — 已定案「新增 `CURRENCY_TYPE_BIND_GOLD = 3`（绑定银两）全栈双银两隔离 + 零拒审激励视频」（本轮未修改任何代码、协议、配表或并行会话未提交文件，待工作区收敛后分批开工）**  
> 配套策划与红线：[零拒审激励视频广告与绑定银两合规创收策划方案（V1）](../planning/rewarded-ad-compliance-and-economy-v1.md)  
> 关联技术基准：[客户端请求幂等](client-request-idempotency.md) · [官方灵石百宝阁 V1](../planning/spirit-shop-and-starter-packs-v1.md) · [角色升级与战斗银两经济 V1](../planning/player-leveling-and-battle-economy-v1.md) · [真钱充值设计](payment-recharge.md) · [小游戏 WebSocket 网关](minigame-websocket-gate.md) · [客户端双形态强制规范](../../../mmorpg-client/Docs/rules/minigame-and-app-platforms.md)  
> 适用仓库：`xuanming-server-mmo-cpp`（C++ Scene + Go 共享库/业务白名单 + Java GM + 共享 proto/配表）、`xuanming-server-mmo`（Java 独立服同步对齐）、`mmorpg-client`（Unity 小游戏 `.jslib` + 双银两展示 + 零拒审编译宏门禁）

---

## 一、需求分析

### 1.1 工程目标与硬约束

1. **物理双银两隔离（`CURRENCY_TYPE_BIND_GOLD = 3`）**：
   - 趁未上线窗口在币种唯一事实源 `proto/common/base/currency_type.proto` 末尾正式追加 `CURRENCY_TYPE_BIND_GOLD = 3`（绑定银两）；
   - 统一封装原子双扣原语 `CurrencySystem::DeductGoldPreferBind`，只对经过逐点审计的**纯个人绑定养成消耗（A 类）**开放优先抵扣绑定银两；
   - 严保 `CurrencySystem::DeductCurrency` 原单币种语义不变，所有涉及**玩家间转账、代工费、可交易（T）装备、神秘商人及通用资产通道（B 类）**严格只扣流通银两 `kCurrencyGold`，实现物理级 0 洗白漏洞；
   - 同步修正全仓（Go 共享包、Java 独立服硬编码 `<= 2` 魔数、Java GM 管理台、Unity 客户端 8 处重复币种文案映射）。
2. **零拒审架构优先（强制一票否决）**：
   - 任何商店或小游戏首审阶段都必须物理保证**默认 100% 关闭广告入口与零 App 广告 SDK / ATT 残留**；
   - 正式开启后，未加载就绪的广告位绝不展示“死按钮”，文案红线由自动化单测/预检强拦截。
3. **最小基础设施侵入**：
   - **不新增微服务、不新增线程（守 `AGENTS.md §9`）、不新增数据库表、不新增 `player_database` 顶层字段**；
   - 激励视频账本复用 `ClientRequestLedgerComp`（`WALLET` 存盘组，与 `CurrencyComp` 同帧原子提交，不变量 I3）。
4. **双形态技术红线全兼容（[minigame-and-app-platforms.md](../../../mmorpg-client/Docs/rules/minigame-and-app-platforms.md)）**：
   - 小游戏 WebGL2 单线程禁阻塞、无 `Task.Run`/`Task.Delay`、先判 `tt` 再判 `wx`、单实例 `RewardedVideoAd` 防 `onClose` 多次触发；
   - 播 30 秒全屏视频暂停主循环引发 Gate 30 秒空闲踢线后，靠落盘 `active_session_id` + 幂等账本实现重连无缝自动补领。

---

## 二、思想分析

1. **「显式白名单式双扣」胜过「默认全局混扣」**：
   - 绝不能把 `CurrencySystem::DeductCurrency(player, kCurrencyGold, ...)` 暗改成自动先扣绑银——那样会立刻击穿通用资产通道（集市/摆摊/当面交易/聚宝斋）、代工费（`proxyFeeGold`）与可交易（T）装备强化，酿成毁灭性洗白与绑定染毒事故！
   - 正确架构是**默认隔离、显式开通**：`DeductCurrency` 永远只扣指定单币种；只有明确审计为纯个人绑定消耗的调用点，才显式调用新原语 `CanAffordGoldWithBind` / `DeductGoldPreferBind`。新增任何涉及交易或他人收款的模块，只要默认调 `DeductCurrency(..., kCurrencyGold, ...)`，天然安全。
2. **「三道锁」把广告拒审风险彻底工程化消灭**：
   - **锁 1（编译期锁）**：App 端广告 SDK 全包在 `#if MMORPG_ENABLE_APP_AD_SDK` 内且默认关闭；`RewardedAdBuildGuard` 构建门禁在宏关闭时强制校验 Xcode `Info.plist` 无 `NSUserTrackingUsageDescription`、无广告 SDK 链接，遇残留直接打断出包。
   - **锁 2（服务端热更锁）**：`RewardedAdSlot.enabled` 默认 `0` 且 `platform_mask` 默认 `3`（仅小游戏，App 位 `4/8` 默认关闭）。
   - **锁 3（客户端就绪锁）**：仅当 `serverSlot.Enabled && !string.IsNullOrEmpty(adUnitId) && adBridge.IsAdReady && playerLevel >= serverSlot.MinLevel && remainingToday > 0` 才显示入口按钮，未就绪彻底 `SetActive(false)`，绝不留死按钮。
3. **「会话落盘 + 幂等账本」根治播广告断线丢奖与重放双发**：
   - 20 字节活跃会话存入 `RewardedAdLedger`（`WALLET` 组，TTL 300 秒），既能抗住 30 秒广告视频触发 `OnApplicationPause` 断线重连，又能和 `ClientRequestLedgerComp` 同事务清空核销，零吞单、零双发。

---

## 三、设计分析

### 3.1 币种全栈扩展：`CURRENCY_TYPE_BIND_GOLD = 3`

#### 1. 唯一事实源与各语言派生

| 仓库与文件 | 变更内容 | 不变量校验 |
|---|---|---|
| `xuanming-server-mmo-cpp/proto/common/base/currency_type.proto` | 枚举末尾追加 `CURRENCY_TYPE_BIND_GOLD = 3; // 绑定银两` | 从 0 连续编号（0..3），只增不删不复用 |
| `xuanming-server-mmo-cpp/cpp/libs/modules/currency/constants/currency.h` | 追加 `kCurrencyBindGold = static_cast<uint32_t>(common::base::CURRENCY_TYPE_BIND_GOLD)`；`kCurrencyMax` 自动随 `CurrencyType_ARRAYSIZE` 变为 `4` | `CurrencyComp.values` 自动扩容到 4 槽；不手写魔数 |
| `xuanming-server-mmo-cpp/go/shared/currencytype/currencytype.go` 及 `_test.go` | 追加 `BindGold = uint32(basepb.CurrencyType_CURRENCY_TYPE_BIND_GOLD)`；补充连续性单测映射表 | `IsKnown(3)` 自动由 `CurrencyType_name` 放行；交易/帮会/玩家邮件业务白名单显式不包含 `BindGold` |
| `xuanming-server-mmo-cpp/java/gm_admin/.../GmControllerTest.java` | `GmGamePort.defaultCurrencyTypes` 已动态遍历 `CurrencyType.values()`，仅同步补齐单测断言覆盖 `id = 3` | GM 管理台自动支持查询/补偿/封禁绑定银两 |
| `xuanming-server-mmo`（Java 独立服）<br>• `game-api/.../MailGrants.java:20`<br>• `game-battle-engine/.../SeasonRules.java:140` | 同步 `currency_type.proto` 生成物；**清除硬编码上限 `2` 与 `MAX_VALID_CURRENCY_TYPE` 魔数**，改为基于 `CurrencyType.forNumber(v) != null` 校验，并明确系统发奖允许 `BIND_GOLD`、玩家交易/寄售严格拒绝 `BIND_GOLD` | 杜绝 Java 服把 `currency_type = 3` 误拒或漏校验 |
| `mmorpg-client/Assets/Scripts/` | 提取统一纯 C# 辅助 `CurrencyLabels.Name(uint)`（`0 => "银两"`, `1 => "灵石"`, `2 => "绑定灵石"`, `3 => "绑定银两"`），收口现存 8 处重复 switch（`AfkPassClient` / `EconomyTradeModels` / `MysteryMerchantModels` / `SeasonModels` / `SpiritShopModels` / `BattleResultPanel` / `GuildWindow` / `MailClient`） | 全客户端币种名称单一事实源，零遗漏 |

#### 2. 原子双扣原语设计（C++ `CurrencySystem` 与 Java 同构）

在 `cpp/libs/modules/currency/system/currency_system.{h,cpp}` 新增 3 个受审计接口（保持原 `DeductCurrency` 单币种语义不动）：

```cpp
// 返回绑定银两 + 流通银两总可用额（溢出夹紧至 uint64::max；缺组件返回 0）。
static uint64_t GetSpendableGoldWithBind(entt::entity player);

// 纯只读预检：amount > 0、未跨服冻结、组件存在、且 bindGold + gold >= amount。
static bool CanAffordGoldWithBind(entt::entity player, int64_t amount);

// 原子优先抵扣绑定银两，不足部分自动补扣流通银两：
//   1. 严格先完成全部零副作用预检（amount > 0、未跨服冻结、CurrencyComp/PlayerCurrencyComp 齐全、总额足够）；
//   2. 计算 spentBind = min(bindBalance, amount)，spentGold = amount - spentBind；
//   3. 依次扣减非 0 部分并写流水（共用同一 txType 与 correlationId，便于对账）；
//   4. 任一前置校验不通过则两项余额与流水均原封不动，杜绝“半扣”。
static uint32_t DeductGoldPreferBind(entt::entity player, int64_t amount,
                                     TransactionType txType = TX_CURRENCY_DEDUCT,
                                     uint64_t correlationId = 0,
                                     uint64_t* outSpentBindGold = nullptr,
                                     uint64_t* outSpentGold = nullptr);
```

- **补缴（Deferred Clawback）不变量守恒**：`AddCurrency` 仍是全仓唯一入账入口；`kCurrencyBindGold` 的入账、封禁（`GainBlockService` + 单玩家 `BlockCurrency`）与欠款（如有）按独立币种槽位自然运作，与 `kCurrencyGold` 互不串账。

### 3.2 全仓银两调用点精确改造清单（逐文件审计结果，0 洗白洞）

#### A 类：改造为 `CanAffordGoldWithBind` / `DeductGoldPreferBind`（纯个人绑定养成消耗）

| 源码位置（C++ 权威服；Java 独立服同构对齐） | 业务场景 | 精确启用条件（严防误放行） |
|---|---|---|
| `player_attribute.cpp:1097,1100,1203,1206` | 人物洗点 / 方案开启 | 无条件允许（纯绑定角色能力） |
| `player_skill.cpp:800,1009,1018` | 人物技能解锁与修习升级 | 无条件允许（纯绑定技能成长）；面板同步返回 `gold_balance` 与 `bind_gold_balance` |
| `player_pet.cpp:668,731,737,921,924,1040,1044,1323,1355,1359` | 宠物商店购宠（L1345 已强检 `transfer_policy() == 0` 不可转让）、宠洗点、宠培育 | 保持 `transfer_policy() == 0` 前置校验，允许优先扣绑银；回包同步补充绑银余额 |
| `player_pet_entries.cpp:587,602,627,675,684,738` | 宠物升阶与词条培养 | 无条件允许（纯绑定宠物养成） |
| `player_equip.cpp:1357,1377` | 装备强化 | **仅当 `equipCurrentlyBound == true`（目标装备已绑定）** 时调 `DeductGoldPreferBind`；`equipCurrentlyBound == false`（可交易 T 装）保持严格 `DeductCurrency(..., kCurrencyGold, ...)` |
| `player_equipment_v1.cpp:906,1094,1122` | 装备打造 / 词条装条 / 打磨 / 重铸 / 继承 / 原地升档 | **严格分拆费用**：`proxyFeeGold`（付给 `proxyMaster`）永远只扣 `kCurrencyGold`；仅当 `op != UNSEAL_TO_TRADABLE` 且目标/产物为绑定自用时，**系统手续费 `systemFeeGold = totalGold - proxyFeeGold`** 才允许优先扣绑银（且必须先验证 `gold >= proxyFeeGold` 且 `bindGold + (gold - proxyFeeGold) >= systemFeeGold`） |
| `player_craft.cpp:465,512,560` | 技能制书 | 同上严格分拆：`proxyFeeGold` 永远只扣 `kCurrencyGold`；仅当产物为绑定技能书时，`systemFeeGold` 允许优先扣绑银 |

#### B 类：强制保持单币种 `DeductCurrency`（严禁扣绑银，防洗白与防染毒）

- `player_equip.cpp`（T 装强化）、`player_equipment_v1.cpp`（`UNSEAL_TO_TRADABLE` 解封、T 装打造/打磨与全部 `proxyFeeGold`）、`player_craft.cpp`（可交易技能书与全部 `proxyFeeGold`）；
- `player_mystery_merchant.cpp:662,699`（神秘商人：严格按 `goods.currencyType` 扣款，防止绑银购出可交易道具套现）；
- `asset_op_system.cpp:630,906,999`（通用资产通道：集市、摆摊、当面交易、聚宝斋、帮会捐献、帮战悬赏、玩家邮件）；
- `afk_pass.cpp` / `player_spirit_shop.cpp` / `player_travel_pass.cpp` / `player_season.cpp`（严格按表定 `currency_type` 单币种扣款）。

### 3.3 激励视频权威配置表设计（1 张新表，零硬编码、零触碰脏表）

新增成对文件：`data/schema/rewardedadslot_table.proto` + `data/RewardedAdSlot.xlsx`（`owner = server`，客户端零配表）：

```proto
syntax = "proto3";
package mmorpg.cfgtable.v1;
import "cfg_options.proto";

message CfgRewardedAdSlotRow {
  option (cfg_sheet) = "RewardedAdSlot";
  option (cfg_owner) = "server";

  uint32 id = 1 [(cfg_key) = true];            // 广告位业务编号（首批固定 1 = 观影绑定银两小礼）
  uint32 enabled = 2;                          // 0 = 关闭（默认/首审态）, 1 = 开启
  uint32 min_level = 3;                        // 解锁最低角色等级（默认 10）
  uint32 daily_limit = 4;                      // 每日最大领取次数（UTC+8 05:00 切日，默认 2）
  uint32 cooldown_sec = 5;                     // 两次成功领取最短间隔秒数（默认 60）
  uint32 min_watch_sec = 6;                    // 签发会话到领奖的最短观看秒数（默认 15）
  uint32 session_ttl_sec = 7;                  // 会话有效 TTL 秒数（默认 300，容忍看广告断线重连）
  uint32 currency_type = 8;                    // CurrencyType：固定 3（CURRENCY_TYPE_BIND_GOLD）
  uint64 reward_amount = 9;                    // 单次发放绑定银两数（默认 300）
  uint32 require_ssv = 10;                     // 0 = V1 轻量会话校验, 1 = V2 强制服务端 SSV 验签
  uint32 platform_mask = 11;                   // 允许开启宿主掩码：1=微信小游戏 2=抖音小游戏 4=iOS 8=Android（默认 3，仅小游戏）
}
```

- **默认安全首审行**：`id = 1, enabled = 0, min_level = 10, daily_limit = 2, cooldown_sec = 60, min_watch_sec = 15, session_ttl_sec = 300, currency_type = 3, reward_amount = 300, require_ssv = 0, platform_mask = 3`。
- **强校验导表/加载断言**：服务端加载 `RewardedAdSlot` 时，校验 `currency_type == CURRENCY_TYPE_BIND_GOLD`（或在配置白名单内，严禁误配成 `CURRENCY_TYPE_DIAMOND = 1`），一旦发现非法币种直接拒载该行并打 ERROR 告警。

### 3.4 激励视频持久化设计（零新 DB 表、零新 `player_database` 顶层字段）

1. **C++ 权威服（`xuanming-server-mmo-cpp`）**：
   - 在 `proto/common/component/client_request_ledger_comp.proto` 中新增 `RewardedAdLedger` 并挂为 `ClientRequestLedgerComp` 的 **6 号字段**（位于 `WALLET` 存盘组，与 `CurrencyComp`、`BattleEconomyLedger` 同事务原子落盘）：
     ```proto
     message RewardedAdLedger {
       uint64 day_starts_at_ms = 1;         // 当前游戏日起点（UTC+8 05:00，抗时钟回拨）
       uint32 claimed_today = 2;            // 今日已成功领取次数
       uint64 last_claim_at_ms = 3;         // 最近一次成功领取时间（UTC 毫秒）
       uint64 total_claimed_lifetime = 4;   // 终身累计成功次数（仅监控统计）
       uint64 active_session_id = 5;        // 当前待核销的短效会话号（0 = 无）
       uint64 active_session_issued_ms = 6; // 会话签发时刻（UTC 毫秒）
       uint32 active_session_slot_id = 7;   // 会话对应广告位 ID
     }
     ```
   - 在 `cpp/libs/services/scene/player/system/client_request_ledger.h` 的 `ClientRequestKind` 末尾追加（现役最大值为 `kEquipSkillVfx = 19`）：
     ```cpp
     kClaimRewardedAd = 20, // SceneRewardedAdClientPlayer.ClaimRewardedAd: 激励视频完播领绑定银两
     ```
   - 在 `ClientRequestRecord` 末尾追加 2 个重放结果字段（现役最大号为 `34`）：
     ```proto
     uint32 rewarded_ad_currency_type = 35; // 激励视频领奖：首次实际发放币种（默认 3 = 绑定银两）
     uint64 rewarded_ad_amount = 36;        // 激励视频领奖：首次实际发放金额
     ```
2. **Java 独立服（`xuanming-server-mmo`）**：
   - 在 `game-player-store/src/main/proto/game/storage/player_state.proto` 末尾追加 `PlayerRewardedAdState rewarded_ad = <next_free_tag>;`（字段与 `RewardedAdLedger` 逐项同构），通过 `PersistentSchemaTool` 追加最新持久 schema 世代台账并补未知字段往返单测。

### 3.5 客户端 ↔ Scene 协议设计（3 个轻量 RPC + 双银两展示）

新增 `proto/scene/player_rewarded_ad.proto`（挂载 `NODE_SCENE`，`OptionIsPlayerService = true`，`OptionIsClientProtocolService = true`）：

```proto
syntax = "proto3";
option go_package = "scene";

import "proto/db/proto_option.proto";
import "proto/common/base/tip.proto";

option cc_generic_services = true;
option (OptionFileDefaultNode) = NODE_SCENE;

enum AdClientPlatform {
  AD_CLIENT_PLATFORM_UNSPECIFIED = 0;
  AD_CLIENT_PLATFORM_WECHAT_MINIGAME = 1; // 对应 platform_mask bit 0 (1)
  AD_CLIENT_PLATFORM_DOUYIN_MINIGAME = 2; // 对应 platform_mask bit 1 (2)
  AD_CLIENT_PLATFORM_IOS_APP = 3;         // 对应 platform_mask bit 2 (4)
  AD_CLIENT_PLATFORM_ANDROID_APP = 4;     // 对应 platform_mask bit 3 (8)
}

message RewardedAdSlotView {
  uint32 slot_id = 1;
  bool enabled = 2;                // 服务端结合总开关、platform_mask、角色等级算出的当前是否可用
  uint32 min_level = 3;
  uint32 daily_limit = 4;
  uint32 claimed_today = 5;
  uint64 next_reset_ms = 6;        // 下次切日时间戳（UTC+8 05:00，毫秒）
  uint64 cooldown_until_ms = 7;    // 冷却结束时间戳（0 = 无冷却）
  uint32 currency_type = 8;        // CurrencyType（默认 3 = CURRENCY_TYPE_BIND_GOLD）
  uint64 reward_amount = 9;
}

message GetRewardedAdInfoRequest {
  uint32 client_platform = 1;      // AdClientPlatform
}

message GetRewardedAdInfoResponse {
  TipInfoMessage error_message = 1;
  uint64 server_time_ms = 2;
  uint64 bind_gold_balance = 3;    // 当前绑定银两余额（供入口/小窗即时展示）
  uint64 gold_balance = 4;         // 当前流通银两余额（区分展示防误解）
  repeated RewardedAdSlotView slots = 5;
}

message BeginRewardedAdSessionRequest {
  uint32 slot_id = 1;
  uint32 client_platform = 2;      // AdClientPlatform
}

message BeginRewardedAdSessionResponse {
  TipInfoMessage error_message = 1;
  uint64 ad_session_id = 2;        // 服务端签发的一次性随机非 0 会话号（非发号器永久 ID、无位语义）
  uint64 expires_at_ms = 3;
}

message ClaimRewardedAdRequest {
  uint64 client_request_id = 1;    // 必填非 0 客户端幂等键
  uint32 slot_id = 2;
  uint64 ad_session_id = 3;        // BeginRewardedAdSession 签发的会话号
  string ssv_proof = 4;            // V1 留空；V2 强制 SSV 模式用于传递平台回执标识
}

message ClaimRewardedAdResponse {
  TipInfoMessage error_message = 1;
  uint32 currency_type = 2;
  uint64 reward_amount = 3;
  uint64 currency_balance = 4;     // 领奖后该币种（绑定银两）最新余额
  uint64 gold_balance = 5;         // 当前流通银两余额
  RewardedAdSlotView slot = 6;     // 领奖后最新槽位状态
  bool replayed = 7;               // true = 幂等重放命中首次记录
}

service SceneRewardedAdClientPlayer {
  option (OptionIsPlayerService) = true;
  option (OptionIsClientProtocolService) = true;

  rpc GetRewardedAdInfo (GetRewardedAdInfoRequest) returns (GetRewardedAdInfoResponse);
  rpc BeginRewardedAdSession (BeginRewardedAdSessionRequest) returns (BeginRewardedAdSessionResponse);
  rpc ClaimRewardedAd (ClaimRewardedAdRequest) returns (ClaimRewardedAdResponse);
  // ── 以下只许在末尾追加 ──
}
```

- **关联现有协议补充（纯末尾追加只读字段，不破坏老协议）**：在已有返回银两余额的面板消息（如技能/宠物/属性/装备相关 `Info` 响应或通用钱包同步包）末尾追加 `uint64 bind_gold_balance`，让客户端无需硬猜即可双栏显示「银两 + 绑定银两」。

### 3.6 服务端核心校验与原子执行流程（C++ Scene 单线程同步）

1. **纯规则模块（无 ECS / 无 I/O，C++ 与 Java 100% 同构单测）**：
   - `ResolveGameDayStartMs(uint64_t nowMs)`：直接复用 `player_equipment_material_reward.cpp` 同源口径（`kDayMs = 86400000`, `kShiftMs = 10800000`，即 UTC+8 每日 `05:00`），且抗时钟回拨（仅当 `computedDayStart > ledger.day_starts_at_ms()` 才推进并清零 `claimed_today`）。
   - `ValidateAdPlatformMask(uint32_t clientPlatform, uint32_t platformMask)`：校验枚举在 `[1..4]` 且对应 bit 置位。
2. **`BeginRewardedAdSession` 执行顺序**：
   1. 查 `RewardedAdSlotTable::FindById(slot_id)`（空即拒绝）；校验 `row->enabled() == 1`、`ValidateAdPlatformMask`、角色 `LevelComp.level() >= row->min_level()`；
   2. 推进日账本（跨日清零 `claimed_today`）；校验 `claimed_today < row->daily_limit()` 与冷却 `nowMs >= last_claim_at_ms + cooldown_sec * 1000`；
   3. 预检 `GainBlockService::IsGloballyBlocked(kCurrency, row->currency_type())`、单玩家货币封禁与 `CurrencySystem::CreditOverflows`（不可入账直接拒开播，不让玩家白看广告）；
   4. 生成非 0 随机 `ad_session_id`（64 位 CSPRNG，不消耗发号器永久 ID），写入 `active_session_*` 并触发 `PlayerLifecycleSystem::SavePlayerForAssetCommit` 落盘。
3. **`ClaimRewardedAd` 执行顺序（单事务原子提交）**：
   1. 强制 `client_request_id != 0`；查 `ClientRequestLedgerComp`：`kUnavailable` → `kServiceUnavailable`（fail-closed）；`kMismatch` → `kInvalidParameter`；`kReplay` → 直接回首次结果并置 `replayed = true`；
   2. 查表校验开启、等级、跨日限次（`< daily_limit`）、冷却；
   3. 严格核销会话：`ad_session_id != 0 && ad_session_id == ledger.active_session_id() && slot_id == ledger.active_session_slot_id()`；校验 `nowMs >= issuedMs + min_watch_sec * 1000` 且 `nowMs <= issuedMs + session_ttl_sec * 1000`；若 `require_ssv == 1`（V2）核验平台 SSV 凭证；
   4. 再次检查封禁与溢出，调用 `CurrencySystem::AddCurrency(player, kCurrencyBindGold, rewardAmount, TX_SYSTEM_GRANT, ad_session_id)`；
   5. 同一帧更新 `claimed_today++`、`total_claimed_lifetime++`、`last_claim_at_ms = nowMs`、清零 `active_session_* = 0`、写入 `ClientRequestLedgerComp` 幂等记录；
   6. 立即调用 `PlayerLifecycleSystem::SavePlayerForAssetCommit` 原子提交 `WALLET` 组，返回最新绑定银两/流通银两余额与槽位状态。

### 3.7 客户端架构（SRP 三层解耦 + WebGL2/IL2CPP 隔离 + 零拒审构建门禁）

```text
Assets/Plugins/WebGL/
  └── MinigameAd.jslib               // 仅 WebGL：先判 tt 再判 wx，单例管理 RewardedVideoAd

Assets/Scripts/Game/Common/
  └── CurrencyLabels.cs              // 纯 C# 统一币种名称与格式化（0=银两 1=灵石 2=绑定灵石 3=绑定银两）

Assets/Scripts/Game/RewardedAd/
  ├── IRewardedAdBridge.cs           // 平台抽象（IsSupported / IsAdReady / Preload / Show / Cancel）
  ├── NullRewardedAdBridge.cs        // 默认实现（PC Editor、首审无广告 SDK App、未知浏览器恒返回 false）
  ├── MinigameAdBridge.cs            // #if UNITY_WEBGL && !UNITY_EDITOR（同 MinigamePayBridges.cs 风格）
  ├── AppAdBridge.cs                 // #if MMORPG_ENABLE_APP_AD_SDK && (UNITY_IOS || UNITY_ANDROID) && !UNITY_EDITOR
  ├── RewardedAdModel.cs             // 纯 C# 状态推导（零 UnityEngine 依赖，可 EditMode 单测）
  ├── RewardedAdClient.cs            // 协调 RPC、会话、30 秒断线自动补领与事件广播
  └── RewardedAdEntryView.cs         // uGUI 入口与等权双按钮确认小窗（未就绪彻底 SetActive(false)）

Assets/Editor/
  └── RewardedAdBuildGuard.cs        // IPostprocessBuildWithReport 零拒审编译期拦截门禁
```

#### 关键客户端工程细节

1. **`MinigameAd.jslib` 单例与频控铁律**：
   - `MinigameAdHost()`：**先判 `typeof tt !== 'undefined' && tt && typeof tt.createRewardedVideoAd === 'function'` 返回 `2`（抖音）**，再判 `typeof wx !== 'undefined' && wx && typeof wx.createRewardedVideoAd === 'function'` 返回 `1`（微信），其余返回 `0`；
   - 按 `adUnitId` 缓存唯一 `RewardedVideoAd` 单例，**只绑定一次** `onLoad` / `onError` / `onClose`，按递增 `_activeRequestId` 路由单次回调，根治多次绑定导致重复发奖；
   - `onClose(res)` 严格判定 `!!(res && res.isEnded)`，只有明确 `true` 才回报完播；
   - `load()` 与 `show()` 强制保留 **≥ 1,000 ms** 最小调用间隔（遵守抖音频控）；
   - 严禁多线程/阻塞等待，所有超时与重试在 `Update` / `Tick` 按 `unscaledTime` 推进。
2. **播广告 30 秒断线自动补领闭环**：
   - 开播前缓存 `PendingClaim(slotId, adSessionId, clientRequestId)`；完播回前台若因 `OnApplicationPause` 暂停超过 30 秒触发重连，待 `GameClient` 恢复 `InGame` 且 `(playerId, sessionGeneration)` 校验通过后自动重发同一 `ClaimRewardedAd`，成功或收到明确终局拒绝后清理。
3. **零拒审构建门禁（`RewardedAdBuildGuard.cs`）**：
   - 未定义 `MMORPG_ENABLE_APP_AD_SDK`（首审默认态）：检查 Xcode `Info.plist` 不含 `NSUserTrackingUsageDescription`、Xcode 工程不链接 `AppTrackingTransparency.framework` 或广告 SDK（`GoogleMobileAds` / `BUAdSDK` / `GDTMobSDK`）、Android Gradle 无广告 SDK 依赖——检出任意残留直接 **Build Failed**；
   - 显式定义 `MMORPG_ENABLE_APP_AD_SDK`（二期开启 App 广告）：iOS 侧强制校验 `NSUserTrackingUsageDescription` 非空且存在 ATT 调起符号，缺一即打断出包；
   - EditMode 静态文案测试：扫描广告相关 UI 文案，禁用「支持作者」「支持开发」「捐赠」「打赏」「帮点广告」「点一下广告」「收款码」「爱发电」等任何诱导/切支付词。

### 3.8 可观测性指标（低基数，严禁 `player_id` 当标签）

| 指标名 | 类型 | 标签（全部低基数枚举） | 用途 |
|---|---|---|---|
| `rewarded_ad_session_total` | Counter | `platform`, `result`（`ok`/`disabled`/`limit`/`cooldown`/`blocked`） | 监控各端开播与拦截分布 |
| `rewarded_ad_claim_total` | Counter | `platform`, `result`（`ok`/`replay`/`too_fast`/`expired`/`mismatch`/`blocked`） | 监控领奖成功率与 `too_fast` 刷量告警 |
| `rewarded_ad_bind_gold_granted_total` | Counter | `slot_id` | 监控绑定银两日增发总量 |
| `currency_gold_prefer_bind_deduct_total` | Counter | `source`（`bind_only`/`mixed`/`gold_only`） | 观测绑定银两真实消化率 |

---

## 四、漏洞分析（技术、并发与资金安全视角）

| 编号 | 漏洞 / 故障场景 | 攻击或触发方式 | 工程防线 |
|---|---|---|---|
| **T-1** | **双扣原语“半扣”损坏玩家资产** | 先扣完绑定银两，再扣流通银两时发现余额不足或跨服冻结，留下半扣状态且仓库无安全货币退款原语 | `DeductGoldPreferBind` 必须先把 `amount > 0`、跨服冻结、组件存在、`bindGold + gold >= amount` 全部预检完毕，才进入无失败顺序扣减 |
| **T-2** | **代工场景复合扣款算错（把代工费算进绑银）** | 玩家有 500 绑银 + 100 流通银两，代工费 200 + 系统费 100 = 300；若只判 `500 + 100 >= 300` 放行，会用绑银支付代工费洗白 | 强制两段独立预检与扣款：`proxyFeeGold` 只验且只扣 `kCurrencyGold`（`100 < 200` 直接拒）；仅剩余流通银两 `gold - proxyFeeGold` 与 `bindGold` 共同承接 `systemFeeGold` |
| **T-3** | **Java 独立服硬编码币种上限 `<= 2` 拒绝新币种 `3`** | `MailGrants.java:20` / `SeasonRules.java:140` 未改导致系统发绑银报错 | 明确列入同步改造清单，全部改为 `CurrencyType.forNumber(v) != null` 枚举驱动并补单测 |
| **T-4** | **客户端跳过视频直调 `ClaimRewardedAd`** | 抓包在开播后 0 秒直接发领奖请求 | V1：校验 `nowMs - issuedMs >= 15s` + ≥10 级 + 每日 2 次 + 冷却 60s + 发放物理不可交易的绑银；V2：开 `require_ssv = 1` 强制平台签名回调 |
| **T-5** | **超时重发 / 双点导致一次广告双倍发奖** | 领奖回包丢包触发重试 | `ClientRequestLedgerComp` 按 `(player, client_request_id)` 幂等落盘，首次成功同帧清零 `active_session_id` |
| **T-6** | **30 秒全屏广告暂停心跳引发 Gate 空闲断线吞奖** | 手机切入广告视图超 30 秒 | `active_session_id` 落盘 `WALLET`（TTL 300s），回前台走 20 秒令牌重连后自动补发 `ClaimRewardedAd` |
| **T-7** | **小游戏 `onClose` 重复绑定 / 抖音误识为微信** | 重复创建监听或先判 `wx` 再判 `tt` | `MinigameAd.jslib` 单例只绑一次监听 + 递增 `requestId` 路由 + 严格先判 `tt` 再判 `wx` |
| **T-8** | **首审 App 包残留广告 SDK 或 ATT plist 遭拒审** | 试验 SDK 后漏清即打首审包 | `MMORPG_ENABLE_APP_AD_SDK` 默认关闭 + `RewardedAdBuildGuard` 构建期直接失败拦截 |
| **T-9** | **宿主机时钟回拨反复刷日上限** | NTP 校时回跳前一日 | 仅当 `computedDayStart > ledger.day_starts_at_ms()` 才重置 `claimed_today` |

---

## 五、步骤分析（依赖顺序与跨仓拆分）

为避免与当前工作区并行会话未提交改动（`client_request_ledger_comp.proto`、`client_request_ledger.h`、`Tip.xlsx`、`Item.xlsx`）冲突，分 4 个独立可验收批次推进：

1. **Batch 1 — 全栈 `CURRENCY_TYPE_BIND_GOLD = 3` 底层闭环**：
   - `currency_type.proto` → `dev.bat proto` → C++ `currency.h` + `CurrencySystem::DeductGoldPreferBind` + Go `currencytype` + Java 独立服消除硬编码 `<= 2` 与同构原语 → 全仓 A 类消费点接入 + B 类防洗白单测。
2. **Batch 2 — 激励视频服务端与双服对齐**：
   - `RewardedAdSlot` 表与 schema（默认 `enabled = 0, currency_type = 3`）+ `Tip.xlsx` + `client_request_ledger_comp.proto`（字段 6 / 35 / 36）+ `player_rewarded_ad.proto` → C++ `RewardedAdSystem` + Java 同构实现与全覆盖单测。
3. **Batch 3 — Unity 客户端双银两展示与小游戏桥接**：
   - `.\gen.bat client` 同步表/协议/消息号 → `CurrencyLabels` 统一 8 处文案 + 资产栏/养成面板双银两展示 → `MinigameAd.jslib` + `RewardedAd/*` + `RewardedAdBuildGuard` + EditMode 单测（含诱导文案静态红线扫描）。
4. **Batch 4 — 首审纯净验证与二版流量主上线**：
   - 首审按默认关闭态通过全部门禁；小游戏过首审达标后补 1 枚标准视频角标（交 Codex 纯功能作图），配置正式 `adUnitId` 与 `enabled = 1` 发二版。

---

## 六、执行步骤（开工与提审前逐项检查单）

### 6.1 编码与测试执行清单（高档模型改码 + Gemini 低档模型编译/生成/跑测）

- [ ] **Batch 1（双银两底层 + 防洗白分流）**：
  - [ ] 修改 `proto/common/base/currency_type.proto` 追加 `CURRENCY_TYPE_BIND_GOLD = 3`，同步 C++ `currency.h`、Go `go/shared/currencytype/currencytype.go` 及 `_test.go`、Java GM `GmControllerTest.java`；
  - [ ] 在 `CurrencySystem` 实现 `GetSpendableGoldWithBind` / `CanAffordGoldWithBind` / `DeductGoldPreferBind`，并按 §3.2 A/B 矩阵精确改造 7 处 A 类消费点（重点单测验证：代工费、T 装强化/解封、神秘商人、资产通道 100% 拒绝扣绑银；跨绑银+银两边界原子扣款零半扣）；
  - [ ] 同步 `xuanming-server-mmo`（Java 独立服）：修复 `MailGrants.java:20`、`SeasonRules.java:140` 硬编码上限，实现同构双扣原语与单测；
  - [ ] C++ 源码先跑只读预检 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`，再交 Gemini 执行 `dev.bat proto`、C++ MSBuild `/m:1`、`go test ./...` 与 Java `mvn test`。
- [ ] **Batch 2（激励视频服务端）**：
  - [ ] 新增 `data/schema/rewardedadslot_table.proto`、`data/RewardedAdSlot.xlsx`（默认 `enabled = 0, currency_type = 3, reward_amount = 300`）、`proto/scene/player_rewarded_ad.proto`，扩展 `client_request_ledger_comp.proto`（字段 6 / 35 / 36）、`client_request_ledger.h`（`kClaimRewardedAd = 20`）与 `data/tip/Tip.xlsx`；
  - [ ] 实现 C++ `rewarded_ad_rules.h` / `rewarded_ad_system.{h,cpp}` 与 Java 同构服务，覆盖跨日抗回拨、断线补领、幂等重放、`too_fast`、封禁、`platform_mask` 隔离与非绑银配置拒绝单测；交 Gemini 跑生成、编译与测试。
- [ ] **Batch 3（Unity 客户端）**：
  - [ ] 执行 `.\gen.bat client` 同步客户端生成物；
  - [ ] 落地 `CurrencyLabels.cs` 收口 8 处币种文案并补齐双银两余额展示；
  - [ ] 落地 `MinigameAd.jslib`、`Assets/Scripts/Game/RewardedAd/*`、`Assets/Editor/RewardedAdBuildGuard.cs` 及 EditMode 单测；交 Gemini 跑 `tools/client_compile_check.ps1` 与测试集。
- [ ] **Batch 4（角标作图交接，仅在二版开启广告前）**：
  - [ ] 落盘 `docs/planning/ui-handoff/rewarded-ad-ui-handoff.md`，按 `project-vision.md §3.3` 在对话中给出零风格词/零色号的纯功能作图清单交 Codex 制作标准高对比度「视频/广告」角标。

### 6.2 提审前「零拒审」硬门禁检查单（一票否决）

- [ ] **首审默认关闭态验证**：确认 `RewardedAdSlot.enabled = 0`，实机启动游戏后全界面零广告入口、零红点、零广告网络请求。
- [ ] **App 首审零 SDK 残留验证**：确认未定义 `MMORPG_ENABLE_APP_AD_SDK`，`RewardedAdBuildGuard` 通过，Xcode `Info.plist` 无 `NSUserTrackingUsageDescription`，Android Gradle 无第三方广告 SDK 依赖。
- [ ] **防洗白专项回归通过**：自动化单测证明绑定银两无法用于代工费、T 装强化/解封、神秘商人（非绑银专柜）、集市、摆摊、当面交易、聚宝斋与玩家间寄钱。
- [ ] **小游戏二版开启广告前验证**：
  - [ ] 微信/抖音后台已正式开通流量主并填入真实线上 `adUnitId`（非测试 ID）；
  - [ ] 广告未 `load()` 成功时入口按钮彻底隐藏，绝无点击报错的死按钮；
  - [ ] 确认弹窗同时显眼展示「观看广告领取」与「关闭」双按钮，触发按钮含清晰高对比度「视频/广告」角标；
  - [ ] 中途关闭（`isEnded == false`）不发奖、完整观看（`isEnded == true`）正常发 `300` 绑定银两、播广告 35 秒断线重连后自动补领且不双发；
  - [ ] 静态扫描通过：客户端零「支持作者/捐赠/点广告/收款码/爱发电」字样。
- [ ] **App 端若未来启用广告（仅当显式开启 `MMORPG_ENABLE_APP_AD_SDK`）**：
  - [ ] iOS `Info.plist` 配齐 `NSUserTrackingUsageDescription` **且实机冷启动真实弹出系统 ATT 授权框**；
  - [ ] 实机点「要求 App 不跟踪」后仍可正常游玩与看非个性化激励视频，且无任何利诱点允许的文案或奖励；
  - [ ] App Store Connect 隐私问卷、Google Play 广告 ID 声明与国内 App 隐私政策 SDK 清单同步更新并通过核对。
