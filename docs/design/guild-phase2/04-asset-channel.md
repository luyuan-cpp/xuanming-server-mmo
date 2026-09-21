# S4 通用资产通道(B4a/B4b/B4c)
> 本节由 11 个分部合并而成(原分部名保留在小标题里),另附对抗评审处理记录。

<!-- s4_asset_part1.md -->

# S4 通用资产通道(批次 B4a C++ / B4b Go)— 第 1 部分:结论、现状、不变量、协议

> 本节只设计"Go 服务让在线玩家身上的货币/物品发生一次且仅一次的变化"这条通道本身。帮会捐献、商店、活动奖励怎么用它,由 S5/S6 设计;聚宝斋 P2 以后复用它。所有代码均未编译,待 Codex 验证。

## 4.0 结论(先读这段)

- **一句话**:Go 服务先在自己库里写一行"待办资产指令"(outbox),给它一个"该玩家该流"的递增流水号 seq;再按 `player:{id}:location` 找到玩家所在 scene,调 `SceneNodeGrpc.AssetDebit / AssetCredit / AssetAbortDebit`;scene 在 loop 线程**同一次处理**里做"查账本 → 闸门 → 改资产 → 记账本 → 触发存盘";账本和资产在 `player_database` 同一条记录里,**一起落盘、一起丢**;Go 只在响应为 `(APPLIED 或 REJECTED) 且 durable=true` 时才终结这一行。
- **类比**:像银行柜台办业务要"流水号 + 回执"。流水号(seq)由 Go 发;柜台(scene)见过的流水号直接把原回执再给你一遍,绝不重办;回执要"盖了入库章"(durable,已写进 Redis 玩家数据)才算数。
- **不在 gRPC 线程上等存盘**:生成器模板在 `future.get()` 之后没有守护段(`tools/proto_generator/protogen/internal/grpc_handler_gen.go:130-141`),硬塞等待会被重生成吞掉。改为:scene 应用后立刻 `SavePlayerToRedis`,响应里如实报 `durable`;Go 用**同一 seq** 按 100/200/400ms 重查,重查时 scene 只读答复(不会重办)。实测 Redis SET 为毫秒级,一般第二次就拿到 `durable=true`。
- **durable 的判定只看"确实落盘的字节"**:`PlayerLastPersistedSnapshotComp.snapshot` 是 `HandlePlayerAsyncSaved` 回调里写入的、刚写进 Redis 的整份 `PlayerAllData`(`player_lifecycle.cpp:315-319`,`last_persisted_snapshot_comp.h:9-16`)。某 seq 的结局出现在这份快照的账本里 = durable。不另建"已持久化水位"状态,不存在两份真相。
- **账本窗口**:每条流一个 1024 位窗口,窗口上沿始终跟着"见过的最大 seq"滑动;Go 端用"每流未决 ≤16 行、未决跨度 <512"守卫,保证任何仍未决的 seq 永远在窗口内(证明见第 2 部分)。
- **第二轮评审补强**(第 8–11 部分,优先于前文):seq 空间加"流纪元" `stream_epoch`,帮会库重建/恢复后旧 seq 不会被误认,另有跳号上限;请求体带 HMAC 签名,每条流只认对应服务;Credit 部分发放如实报 `partial`;客户端 `Gm*` 指令在分发入口统一闸住;单写者只在重连租约内成立,根治放新批次 B4c(**2026-09-21 订正**:已被 [08-save-owner-fence.md](./08-save-owner-fence.md) 取代,不造 token 围栏;共享环境门禁见 08 §8.3)。

## 4.1 已核验的现状(本节依赖的事实)

| 事实 | 出处 |
|---|---|
| SceneNodeGrpc 只有 CreateScene/DestroyScene/ReleasePlayer/PrepareBattle/CancelBattlePrepare | `proto/scene_manager/scene_node_service.proto:12-29` |
| gRPC 包装函数 = 投递前守护段 + runInLoop + promise + `future.get()`;Handle* 在 loop 线程、在守护段内写逻辑 | `grpc_handler_gen.go:118-141`;`scene_node_service.cpp:146-154,233-247` |
| 包装函数的 context 参数被注释掉(`grpc::ServerContext* /*context*/`),守护段读不到 metadata;scene gRPC 为明文不安全凭据 | `grpc_handler_gen.go:125`;`node.cpp:623` |
| SceneNodeGrpc 在路由表里 `ClientProtocol: false`,客户端调不到 | `go/client_rpc_router/generated/pb/game/route_table.go:119` |
| 玩家查找 `tlsEcs.playerList.find(playerId)` | `scene_node_service.cpp:130` |
| `player_database` 已用到字段 14,下一个空闲 15(B3a 的 `PlayerProfileComp` 也要一个,谁先落谁拿 15) | `mysql_database_table.proto:107-130` |
| 存盘三处接线:`PlayerDatabaseMessageFieldsUnmarshal/Marshal`;跨 zone 迁移在 `HandleCrossZoneTransfer` 里**当场** `PlayerAllDataMessageFieldsMarshal` | `player_database_loader.cpp:75-130`;`player_data_loader.h:12-19`;`player_lifecycle.cpp:843-846` |
| `SavePlayerToRedis` 与上次落盘快照相等时返回 false 且**不会**有回调;写盘时 Redis 保存 + Kafka DBTask(发送失败只打日志) | `player_lifecycle.cpp:1259-1268,1277,1309-1314` |
| 同一 key 只有一个写在途;更新的整份快照覆盖旧的;只有最新值成功才回调 `save_callback_` | `redis_client.h:152-160,574-590` |
| 存盘失败只通知不更新快照(快照仍是旧值) | `player_lifecycle.cpp:154-183` |
| `CurrencySystem::Add/DeductCurrency(entity, type, int64)` 无 tx/correlation;冻结、余额不足、类型错误都回 `kInvalidParameter`;Add 有全局/个人封禁与补缴 | `currency_system.h:36-41`;`currency_system.cpp:59-226` |
| `BagService::AddItems(ItemCountMap, txType)`:冻结/封禁回 `kInvalidParameter`;空间不足整批拒且回 `kBagItemNotStacked(6006)`;无战斗拦截 | `bag_service.cpp:135-204`;`bag_system.cpp:348-360` |
| 写操作前置惯例:`PlayerFrozenComp` → 拒;`PlayerBattleSystem::IsInBattle`(= `InBattleComp`) → 拒 | `player_attribute.cpp:279-291`;`player_battle.cpp:412-416` |
| `TransactionType` 最大值 23(TX_BUFF_GAIN);`TransactionLogEntry.correlation_id=13` | `transaction_log.proto:20-69,96-97` |
| GM 货币消息客户端可发、无鉴权 | `player_currency_handler.cpp:14-49` |
| tip 段已占到 26000(pet);27000 空闲 | `go/shared/generated/tip/segments.go:30-48` |
| match 的 Go→scene 定位:SharedRedis 读 `player:%d:location`(`PlayerLocation{scene_id,node_id,update_time,zone_id}`)→ etcd 前缀 `SceneNodeService.rpc/` 的 NodeWatcher `EndpointOf(zone,node)` → 拨号 | `go/match/internal/logic/location.go:17-30`、`keys.go:140`、`gather.go:263-313`;`discovery/node_watcher.go`;`storage.proto:17-24` |
| `go/shared/go.mod` 目前**不依赖** `proto` 模块;所有依赖 shared 的服务 go.mod 都已有 `replace proto => ../proto` | shared/go.mod;chat/client_rpc_router/data_service/db/friend/guild/login/match/player_locator/scene_manager/trade 的 go.mod |

## 4.2 不变量(违反即拒合)

- **I1 单写者(在重连租约内成立)**(**2026-09-21 订正**:本条已被 [08-save-owner-fence.md](./08-save-owner-fence.md) 取代 —— owner_epoch 机制已堵住跨节点版 K1,B4c 改为补它的缺口,不造 token 围栏;共享环境门禁见 08 §8.3):某玩家的资产与账本任一时刻只在一个 scene 实体上被**改**。改动类操作(应用、中止占位)要求:在本节点、无 `PlayerFrozenComp`、无 `UnregisterPlayer`。只读答复(已见 seq)不受限。**边界**:旧节点退出存盘超过 player_locator 30s 租约时,晚到的旧存盘可覆盖新节点已确认的数据(已知风险 K1,第 9 部分 4.35),由 B4c 存盘属主围栏根治。
- **I2 结局固定**:某 (player, stream, seq) 一旦记为 APPLIED 或 REJECTED,之后任何 RPC 都回同一结局。RETRY / NOT_HERE / UNKNOWN 永不记账。
- **I3 同记录**:账本组件和 `currency`、`bag_component` 同在 `player_database`,由同一次 Marshal、同一次 Redis SET 写出,不存在"资产落了账本没落"的中间态。
- **I4 Go 终结须 durable**:Go 只有拿到 `(APPLIED|REJECTED) && durable` 才把行从 PENDING 改走(比契约 §3.3 更严:REJECTED 也要 durable,理由见第 2 部分 C9)。
- **I5 未决在窗**:Go 分配新 seq 前检查该 (player, stream) 未决行数 <16 且 `next_seq − 最小未决 seq < 512`,否则拒绝发起(帮会回 `GuildAssetPending`)。
- **I6 流独占**:每条流只有一个服务分配 seq(GUILD_* 归 guild,TRADE_* 归 trade,SYSTEM_CREDIT 归将来的邮件/GM 发物服务)。
- **I7 只在线应用**:scene 不在线就 NOT_HERE;Go 不写离线玩家 blob。
- **I8 GM 默认拒绝**:客户端发来的**全部** `Gm*` 消息默认拒绝,仅在运行模式为 dev/test 时受理。判据是 `GATE_RUN_MODE` / `SCENE_RUN_MODE`(**未设置 = prod = 拒绝**),gate 按消息号闸、scene 再拦一次防直连。落码形状见 `cpp/nodes/gate/SECURITY.md` §3;本文件第 3 部分 4.12 与第 9 部分 4.34 的原设计(`client_gm_gate.h` + `MMORPG_ALLOW_CLIENT_GM`)**已被实际实现取代**,只作留痕。
- **I9 资产 RPC 须签名**:`AssetOpRequest.auth` 用调用方独有密钥做 HMAC;每条流只认对应调用方;验签失败回 UNKNOWN、不记账(第 8 部分 4.32)。
- **I10 纪元单调**:同一 (player, stream) 的纪元只增不减;请求纪元小于账本纪元回 UNKNOWN;同纪元内 `seq > max_seq + 1024` 回 UNKNOWN(第 8 部分 4.29)。

## 4.3 协议

### 4.3.1 新文件 `proto/common/asset/asset_op.proto`

```proto
syntax = "proto3";
option go_package = "common/asset";

import "proto/common/base/tip.proto";

// 枚举值带前缀:本文件无 package,C++ 枚举值落在全局作用域,裸 UNKNOWN/APPLIED 会与他处冲突。
enum AssetOpStream {
  ASSET_OP_STREAM_UNSPECIFIED = 0;
  ASSET_OP_STREAM_GUILD_DEBIT = 1;    // guild 独占
  ASSET_OP_STREAM_GUILD_CREDIT = 2;   // guild 独占
  ASSET_OP_STREAM_TRADE_DEBIT = 3;    // trade 独占
  ASSET_OP_STREAM_TRADE_CREDIT = 4;   // trade 独占
  ASSET_OP_STREAM_SYSTEM_CREDIT = 5;  // 预留:邮件/GM 发物(D1)
}

enum AssetOpOutcome {
  ASSET_OP_OUTCOME_UNKNOWN = 0;   // 请求畸形 / 验签失败 / 纪元过期 / 跳号过远 / seq 已滑出窗口:Go 告警,不终结
  ASSET_OP_OUTCOME_APPLIED = 1;   // 可能带 partial=true(部分发放)
  ASSET_OP_OUTCOME_REJECTED = 2;
  ASSET_OP_OUTCOME_RETRY = 3;     // 暂时条件(冻结/战斗/背包满/退出中),未记账
  ASSET_OP_OUTCOME_NOT_HERE = 4;  // 玩家不在本节点,未记账
}

message CurrencyAmount { uint32 currency_type = 1; uint64 amount = 2; }
message ItemGrant      { uint32 config_id = 1;     uint32 count = 2; }
// 以后按 guid 扣物、宠物、角色以新增字段扩展(聚宝斋 P2),不改已有字段号。
message AssetBundle {
  repeated CurrencyAmount currencies = 1;
  repeated ItemGrant items = 2;
}

// 调用方签名(第 8 部分 4.32)。放在消息体里:生成的 gRPC 包装读不到 metadata。
message AssetOpAuth {
  string caller = 1;          // "guild" / "trade";按流白名单
  uint64 timestamp_ms = 2;    // 签名时刻;与 scene 时钟偏差须 <= 300000
  string signature_hex = 3;   // lowercase-hex(HMAC-SHA256(调用方密钥, canonical))
}

message AssetOpRequest {
  uint64 player_id = 1;
  AssetOpStream stream = 2;
  uint64 seq = 3;             // 从 1 开始;0 非法
  uint64 correlation_id = 4;  // 进 transaction_log.correlation_id(帮会 = op_id)
  uint32 tx_type = 5;         // TransactionType 数值;按流白名单校验
  AssetBundle bundle = 6;     // AssetAbortDebit 不使用,但仍进签名串
  uint64 stream_epoch = 7;    // 流纪元(建 seq 行时的毫秒);0 非法
  AssetOpAuth auth = 8;
}

message AssetOpResponse {
  AssetOpOutcome outcome = 1;
  TipInfoMessage reason = 2;  // asset_error 段码;无原因时 id=0
  bool durable = 3;           // 该 seq 的结局已在"最后一次成功写入 Redis 的玩家数据"里
  bool partial = 4;           // APPLIED 但只发放了一部分(reason=kAssetPartialApplied);Go 不做对侧入账,转人工补偿
  // 聚宝斋 P2 追加字段从 5 起
}
```

### 4.3.2 新文件 `proto/common/component/asset_op_ledger_comp.proto`

```proto
syntax = "proto3";
option go_package = "common/component";

import "proto/common/asset/asset_op.proto";

message AssetOpRejection {
  uint64 seq = 1;
  uint32 reason_tip_id = 2;   // 0 = 中止占位
}

// 单条流的账本。窗口覆盖 (watermark, watermark + 1024]。
// 位 i(i∈[0,1024))对应 seq = watermark + 1 + i;第 i/64 个字、第 i%64 位。
message AssetOpStreamLedger {
  AssetOpStream stream = 1;
  uint64 watermark = 2;               // 窗口下沿;<= watermark 的 seq 结局不可知
  repeated fixed64 seen_bits = 3;     // 恰好 16 个
  repeated fixed64 applied_bits = 4;  // 恰好 16 个;applied ⊆ seen
  repeated AssetOpRejection rejections = 5;  // 按 seq 升序,<= 64 条,只收窗口内的 REJECTED
  uint64 max_seq = 6;                 // 本纪元见过的最大 seq;跳号上限 seq <= max_seq + 1024
  uint64 stream_epoch = 7;            // 本账本所属纪元;请求纪元更大时整条流重置(第 8 部分 4.29)
  repeated uint64 partial_seqs = 8;   // 升序,<= 64 条,只收窗口内"部分发放"的 APPLIED seq
}

message PlayerAssetOpLedgerComp {
  repeated AssetOpStreamLedger streams = 1;  // 按 stream 升序,每流至多一条;首次记账时创建
}
```

### 4.3.3 改动 `proto/scene_manager/scene_node_service.proto`

在 import 区加 `import "proto/common/asset/asset_op.proto";`,在 `CancelBattlePrepare` 之后加:

```proto
  // ---- 通用资产通道(docs/design/guild-phase2.md §S4)----
  // 结局写在 response.outcome;gRPC status 恒为 OK。同 seq 重复调用只读答复。
  rpc AssetDebit(AssetOpRequest) returns (AssetOpResponse) {}
  // 未见过的 seq 记 REJECTED 占位(reason=0);见过则回原结局。允许用于任何流。
  rpc AssetAbortDebit(AssetOpRequest) returns (AssetOpResponse) {}
  rpc AssetCredit(AssetOpRequest) returns (AssetOpResponse) {}
```

### 4.3.4 改动 `proto/common/database/mysql_database_table.proto`

import 区加 `import "proto/common/component/asset_op_ledger_comp.proto";`;`player_database` 在 `mission_component = 14;` 后加:

```proto
  // 通用资产通道账本(每流 seq 窗口);必须与 currency / bag_component 同记录同 DBTask。
  PlayerAssetOpLedgerComp asset_op_ledger = 16;
```

**字段号已定为 16,本节原文的"= 15"作废**(裁决见 90-consistency.md G-03:按 91 的批次顺序
B3a-1 `profile_component` 取 15、B4a-1 `asset_op_ledger` 取 16)。2026-09-18 落码时已按 16 写入,
proto 里也记了归属与作废说明,后续不要再按"查下一个空闲号"重新推导 —— 15 虽然此刻还空着
(B3a-1 未落码),但它是留给 `profile_component` 的。加列后必须跑 go/db 迁移(4.42 第 1b 步)。

### 4.3.5 改动 `proto/common/rollback/transaction_log.proto`

在 `TX_BUFF_GAIN = 23;` 之后追加(只追加,不复用):

```proto
  TX_GUILD_DONATE = 24;           // 帮会捐献扣货币(GUILD_DEBIT)
  TX_GUILD_SHOP = 25;             // 帮会商店发物(GUILD_CREDIT)
  TX_GUILD_ACTIVITY_REWARD = 26;  // 帮会活动发奖(GUILD_CREDIT)
```

### 4.3.6 Tip(只改 `data/tip/Tip.xlsx`)

在 pet_error 组之后新增组头 `//asset_error base=27000 width=1000`,按序加 9 行(A 列码名 / B 列文案 / fault)。文案不承诺"一定自动完成":超过业务截止时间时,帮会会改发中止并按规则退还(第二轮评审修订):

| 码名 | 文案 | fault |
|---|---|---|
| AssetCurrencyInsufficient | 货币不足 | |
| AssetBagFull | 背包已满,请腾出空间,之后会重试发放;超过时限可能取消 | |
| AssetInBattle | 战斗中暂不结算,稍后自动重试;超过时限可能取消 | |
| AssetFrozen | 角色迁移中,稍后自动重试;超过时限可能取消 | |
| AssetInvalidBundle | 资产指令无效 | 1 |
| AssetBlocked | 该资产当前被禁止获取 | |
| AssetPlayerNotHere | 角色不在线,上线后重试;超过时限可能取消 | |
| AssetPartialApplied | 资产只发放了一部分,已记录,将由客服补偿 | 1 |
| AssetAuthFailed | 资产指令校验失败 | 1 |

具体时限由帮会界面按 `GuildRule.asset_op_deadline_seconds` 显示(S5)。导表后应得 `generated/code/proto/tip/asset_error_tip.proto`(`kAsset_errorOK = 0; kAssetCurrencyInsufficient = 27000; … kAssetPlayerNotHere = 27006; kAssetPartialApplied = 27007; kAssetAuthFailed = 27008;`)。按 AGENTS §4.4,新域的 `asset_error_tip.pb.cc/.pb.h` 手工登记进 `cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`。C++ 头 `table/proto/tip/asset_error_tip.pb.h`;Go 常量 `uint32(table.AssetError_kAssetXxx)`(命名按导表器现有规则,落码时照 `GuildError_k…` 核对)。

<!-- s4_asset_part2.md -->

# S4 通用资产通道 — 第 2 部分:账本语义、持久化接线、durable 确认、崩溃窗口

## 4.4 账本算法(纯函数,无 ECS,可单测)

新文件 `cpp/libs/services/scene/player/system/asset_op_ledger.{h,cpp}`,只依赖 `asset_op_ledger_comp.pb.h`。

```cpp
constexpr uint32_t kAssetOpWindowBits = 1024;
constexpr int kAssetOpWindowWords = 16;           // static_assert(16 * 64 == 1024)
constexpr int kAssetOpMaxRejections = 64;

constexpr uint64_t kAssetOpMaxSeqJump = 1024;     // 同纪元内 seq <= max_seq + 1024

enum class AssetOpSeqState : uint8_t {
    kInvalid,        // seq == 0、epoch == 0,或 watermark 溢出
    kStaleEpoch,     // 请求纪元 < 账本纪元
    kBehindWindow,   // seq <= watermark:结局不可知
    kUnseen,         // 窗口内未见(含"请求纪元更大、按空账本看"的情况)
    kAheadOfWindow,  // seq > watermark + 1024:必为未见
    kJumpTooFar,     // seq > max_seq + 1024(新纪元时 seq > 1024)
    kApplied,
    kRejected,
    kCount
};
enum class AssetOpRecordKind : uint8_t { kApplied, kAppliedPartial, kRejected, kCount };

// 流不存在时返回 nullptr(视作 epoch=0、watermark=0 的空账本)
const AssetOpStreamLedger* FindAssetOpStream(const PlayerAssetOpLedgerComp&, AssetOpStream);
AssetOpStreamLedger& MutableAssetOpStream(PlayerAssetOpLedgerComp&, AssetOpStream); // 缺则按序插入并补 16+16 个 0,epoch=0
AssetOpSeqState ClassifyAssetOpSeq(const AssetOpStreamLedger* ledger, uint64_t epoch, uint64_t seq);
uint32_t AssetOpRejectionReason(const AssetOpStreamLedger& ledger, uint64_t seq); // 查不到回 0
bool IsAssetOpPartial(const AssetOpStreamLedger& ledger, uint64_t seq);            // seq 在 partial_seqs 内
void ResetAssetOpStreamForEpoch(AssetOpStreamLedger& ledger, uint64_t epoch);      // 第 8 部分 4.29
// 前置:Classify(epoch, seq) ∈ {kUnseen, kAheadOfWindow}。epoch > 账本纪元时内部先 Reset。
// 返回 false 表示前置不满足(调用方 LOG_ERROR,不改状态)。
bool RecordAssetOpOutcome(AssetOpStreamLedger& ledger, uint64_t epoch, uint64_t seq, AssetOpRecordKind kind, uint32_t reasonTipId);
// 加载校验;失败返回原因字符串(空 = 通过)
std::string ValidateAssetOpLedger(const PlayerAssetOpLedgerComp&);
```

**Classify(ledger, epoch, seq)**:
1. `seq == 0 || epoch == 0` → kInvalid。
2. E = ledger ? stream_epoch : 0。`epoch < E` → kStaleEpoch。`epoch > E` → `seq <= 1024 ? kUnseen : kJumpTooFar`(按空账本看,到此返回)。
3. W = watermark;`W > UINT64_MAX − 1024` → kInvalid(防溢出)。
4. `seq <= W` → kBehindWindow;`seq > max_seq + 1024` → kJumpTooFar;`seq > W + 1024` → kAheadOfWindow。
5. i = seq − W − 1;seen 位为 0 → kUnseen;applied 位为 1 → kApplied;否则 kRejected。

**Record(ledger, epoch, seq, kind, reason)**:
0. **先**对原账本做前置检查 `Classify(&ledger, epoch, seq) ∈ {kUnseen, kAheadOfWindow}`,不满足返回 false 且不改任何状态;满足后,若 `epoch > ledger.stream_epoch` 再 `ResetAssetOpStreamForEpoch(ledger, epoch)`(顺序不能反,否则前置失败时账本已被清空)。
1. 若 `seq > W + 1024`:`shift = seq − (W + 1024)`。`shift >= 1024` → 两组位全清;否则两组位整体右移 `shift` 位(低位丢弃,16 个字作为 1024 位大整数右移)。`watermark += shift`,删除 `rejections` 与 `partial_seqs` 中 `seq <= 新 watermark` 的条目。
2. 置 seen 位;kind 为 kApplied / kAppliedPartial 再置 applied 位,kAppliedPartial 另把 seq 升序插入 `partial_seqs`(超过 64 条删最小);kRejected **且 reason != 0** 才把 `{seq, reason}` 按升序插入 `rejections`,超过 64 条删最小 seq。

   > **中止占位(reason == 0)刻意不进 `rejections` 环**(2026-09-19 落码):它没有业务原因可看,进环只会挤掉 64 个名额里的一格真原因。
   >
   > **随之而来的一条已知退化,写成明文契约而不是留给下一个人重新发现**:环满 64 条后,被挤掉的那条**业务拒绝**再被重查时 `AssetOpRejectionReason` 回 0,与中止占位的答复逐项相同,于是 Go 的 `FinalStatus`(判据是 `rpc == Abort && reason == 0`)会把它判成 `ABORTED` 而不是 `REJECTED`。
   >
   > **为什么接受**:两者的账务处理完全相同 —— 帮会 B5 对 REJECTED 与 ABORTED 都是退次数 / 退帮贡 / 退限购(`05-economy.md:673-675`),聚宝斋同理。损失只有订单文案从"背包已满 / 余额不足"退化成"已中止",是**展示与排障**的损失,不是资产损失。**不要为它加 proto 字段**(`repeated uint64 abort_seqs` 要动账本 proto + C++ 存取 + 跨 zone 快照三处,代价远大于一行文案)。
   >
   > 这条边界由 `asset_op_ledger_test.cpp` 的 `LedgerEvictedRejectionReasonDegradesByDesign` 钉死。看到它失败,**不要去修原因环**,先确认是不是有人改了判据。
3. `max_seq = max(max_seq, seq)`。

**为什么窗口只"跟着最大 seq 滑",不做"连续已见前缀压缩"**:压缩会把仍未决、但 scene 已记账的 seq 挤出窗口,Go 重查时就分不清当初是 APPLIED 还是 REJECTED。只按最大 seq 滑,再加上 Go 端的 I5 守卫,可以证明**同一纪元内**未决 seq 永远在窗内(帮会库重置/恢复会破坏 `max_seq <= next_seq − 1`,所以才需要纪元,见第 8 部分 4.29;跳号上限的证明也在那里):
- 设窗口下沿 W,则 `W <= max(0, max_seq − 1024)`,且 `max_seq <= next_seq − 1`。
- 未决 s 满足 `s >= min_pending > next_seq − 512`,所以 `s > max_seq + 1 − 512 >= W + 513 > W`。

**拒绝原因**只是展示用。超过 64 条被挤掉时 `reason=0`,Go 按"通用拒绝"映射,不影响正确性。

**体积**:每条流 256 字节位图 + 至多 64×约 6 字节原因 ≈ 0.7KB;5 条流最坏约 3.5KB,常见玩家 0–2 条流。

**加载校验** `ValidateAssetOpLedger`,任一不满足即判损坏:
- stream ∈ [1,5],升序且不重复;
- 两组位各恰好 16 个;applied ⊆ seen;
- `watermark <= UINT64_MAX − 1024`;
- rejections 升序、都在窗口内,且对应位为"seen 且非 applied";
- partial_seqs 升序、≤64、都在窗口内,且对应位为 applied;
- `stream_epoch != 0`;
- `max_seq >= watermark`。

未上线无存量数据,损坏只可能来自 bug。处理是 fail-closed:给实体挂标签 `PlayerAssetOpLedgerInvalidComp`(空结构,不入库),该玩家所有资产 RPC 回 `RETRY + kAssetBlocked` 并打 ERROR 日志。**不改写**原数据,人工排查。

## 4.5 持久化接线

`player_database_loader.cpp`:
- **Unmarshal**,在 `mission_marshal::Unmarshal(...)`(:98)之后:
  ```cpp
  auto& ledger = tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player, message.asset_op_ledger());
  if (auto err = ValidateAssetOpLedger(ledger); !err.empty()) {
      LOG_ERROR << "[AssetOp] 账本损坏,资产通道对该玩家关闭 player_id=" << message.player_id() << " reason=" << err;
      tlsEcs.actorRegistry.emplace_or_replace<PlayerAssetOpLedgerInvalidComp>(player);
  }
  ```
- **Marshal**,在 `mission_marshal::Marshal(...)`(:125)之后:
  `message.mutable_asset_op_ledger()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerAssetOpLedgerComp>(player));`
  (存盘路径非逐帧,沿用本函数现有 get_or_emplace 写法。)
- **跨 zone 快照**:`HandleCrossZoneTransfer` 走 `PlayerAllDataMessageFieldsMarshal → PlayerDatabaseMessageFieldsMarshal`(`player_lifecycle.cpp:843-846`),目的地走同一 Unmarshal,**无需额外改动**。
- **登录 / 同 zone 换节点 / 回档加载**:全部经 `PlayerDatabaseMessageFieldsUnmarshal`,同样无需额外改动。

## 4.6 durable 确认

运行时组件(不入库),放 `player_asset_op.h`:
```cpp
struct PlayerAssetOpPersistRequestComp { int64_t lastRequestMs{0}; };
struct PlayerAssetOpLedgerInvalidComp {};
constexpr int64_t kAssetOpResaveMinIntervalMs = 500;
```

**IsDurable(player, stream, epoch, seq, expectApplied)**:
1. 取 `PlayerLastPersistedSnapshotComp`;无组件或 `!HasSnapshot()` → false。
2. 在 `snapshot->player_database_data().asset_op_ledger()` 里 Find 该流;流缺失或 `stream_epoch != epoch` → false;否则 Classify(epoch, seq)。
3. 结果为 kApplied 且 expectApplied → true;为 kRejected 且 !expectApplied → true。
4. 结果为 kApplied/kRejected 但与内存结局不一致 → LOG_ERROR 并返回 false(说明快照早于某次改写,正常不可能)。
5. 其余 → false。

**RequestPersist(player)**,只在"可改"时调用(无冻结、无 UnregisterPlayer):
```
comp.lastRequestMs = NowMs
bool wrote = PersistFn(player)        // 默认 &PlayerLifecycleSystem::SavePlayerToRedis;单测可替换
return wrote
```
- 每次 Record 之后都调用一次。
- 已见但未 durable 的重查:仅当 `NowMs − lastRequestMs >= 500` 才再调,限制 marshal+比较的频率。
- 返回 false = 内存与上次落盘快照逐字段相等(`player_lifecycle.cpp:1259-1268`)。既然结局已在内存,就必然已在快照里,于是立即重算 IsDurable;若仍为 false 打 ERROR。

**各状态下是否触发存盘**:

| 玩家状态 | 已见 seq 的只读答复 | 触发存盘 |
|---|---|---|
| 正常在线 | 回原结局 + IsDurable | 未 durable 且距上次请求 ≥500ms |
| `UnregisterPlayer`(退出存盘在途) | 回原结局 + IsDurable | 不触发:退出存盘的 marshal 晚于任何已记账的改动,回调在 `FinishExitAfterPersist` 之前更新快照(见下) |
| `PlayerFrozenComp`(跨 zone) | 回原结局 + IsDurable | 不触发:冻结后本端再写 Redis 无意义,真相已随迁移包走 |

**快照更新顺序**:`HandlePlayerAsyncSaved` 在 `FinishExitAfterPersist`(:306)之后才 `snap.Replace`(:315-319),实体被销毁时快照也随之消失。这不影响正确性:实体没了,Go 下次拿到的是 NOT_HERE;玩家重新上线后已见 seq 在账本里,但新实体没有快照,于是触发一次存盘,回调后 durable=true。**本批不改 HandlePlayerAsyncSaved。**

**存盘失败**:`HandlePlayerAsyncSaveFailed` 不更新快照(:170-172),durable 保持 false,Go 行保持 PENDING,直到 MessageAsyncClient 的封顶退避重试成功。

**durable 的范围**:只保证写进 zone Redis 的 `PlayerAllData`。MySQL 回写是 fire-and-forget 的 Kafka DBTask(:1309-1314),这是全体玩家数据共同的既有风险(见 C5)。

## 4.7 崩溃与并发窗口

| # | 场景 | 结果 |
|---|---|---|
| C1 | scene 应用后、存盘前崩溃 | 内存里的资产与账本一起丢;Go 没见过 durable,重投同 seq;新进程见 kUnseen,只应用一次 ✓ |
| C2 | Redis 写成功、响应前崩溃 | 重投 → 加载后账本已见 → 回原结局;新实体无快照 → 触发存盘 → durable ✓ |
| C3 | 响应丢失 / Go 进程崩溃 | reconcile 按行重投,同 C2 ✓ |
| C4 | Go 终结事务提交失败 | 行仍 PENDING → 重投 → 同结局 → 再终结;终结以 `status=PENDING` 做 CAS,重复无效 ✓ |
| C5 | durable 之后 Redis 丢数据(FLUSH/主从切换)回退到 MySQL 旧值 | 资产与账本一起回退,Go 已终结 → 该笔可能重复。与全部玩家数据同级的基础设施风险;缓解:Redis AOF + 交易流水 `correlation_id=op_id` 可对账。文档化接受 |
| C6 | 跨 zone:最后一次存盘在途时应用了指令 | 迁移包在冻结当刻整份 marshal(:843-846);冻结后本端拒绝一切改动。于是"迁移包 ⊇ 源端任何快照",目的地账本不会落后于 Go 已看到的 durable ✓ |
| C7 | 同 zone 换节点(ReleasePlayer) | 旧节点挂 `UnregisterPlayer` 后改动回 RETRY;退出存盘覆盖账本;新节点从 Redis 加载 ✓ |
| C8 | Abort 与 Debit 同时发给同一玩家 | 同节点在 loop 线程串行,账本保证二者只有一个成立;跨节点只在单写者被破坏时才可能。I1 只在 player_locator 30s 租约内成立,租约外见 C14 ✓(租约内) |
| C9 | REJECTED(或中止占位)未 durable 就终结,随后 scene 崩溃 | 若 Go 此时已终结,旧副本租约过期后重发的请求落到新进程,见 kUnseen → APPLIED,但 Go 的行已是 REJECTED → 玩家资产被扣却无对侧账。**所以 I4 要求 REJECTED 也必须 durable**,代价只是拒绝场景多一次重查 |
| C10 | 数据服务回档(RollbackPlayer / Zone / All) | blob 与账本一起回到旧快照;已终结的 guild 行不回滚(`rollback_logic.go:378-382`),会复制帮会资金与帮贡。**必须 fail-closed**:回档前查帮会侧已应用操作,有则默认拒绝(第 10 部分 4.39,B5 落地;过渡期手册禁止对有帮会资产操作的 zone 做批量回档)。PENDING 行在回档后按旧余额重新应用,结果一致 |
| C11 | 滑窗后旧 seq 重查 | kBehindWindow → UNKNOWN。I5 保证不会发生在未决行上;若发生说明 Go 守卫失效或回档,Go 计 `assetop_unknown_total` 并告警,不终结 |
| C13 | 帮会库删库重建 / 按库恢复 / 测试助手重置 | next_seq 回到小号;新纪元 > 账本纪元 → 该流重置后按新操作处理;恢复出来的旧纪元未决行拿到真实旧结局或 UNKNOWN(转人工)。手册见第 8 部分 4.31 ✓ |
| C14 | 旧节点退出存盘超过 30s 租约后才落地(K1) | 可能覆盖新节点已 durable 的数据 → 复制。B4a 只告警,B4c 存盘属主围栏根治(第 9 部分 4.35)。✗ 已知风险 → **2026-09-21:epoch 非 0 时已由 owner_epoch 堵住;epoch = 0、Z1、GO-2 等残余见 [08](./08-save-owner-fence.md) §8.2 / §8.3** |

<!-- s4_asset_part3.md -->

# S4 通用资产通道 — 第 3 部分:scene 处理流程、Currency/Bag/流水改造、GM 闸门

## 4.8 流与交易类型白名单

`player_asset_op.cpp` 内平铺常量表(不用宏),加 `static_assert(std::size(kAssetOpStreamRules) == 5)`:

| stream | 方向 | 允许的 tx_type |
|---|---|---|
| GUILD_DEBIT | Debit | TX_GUILD_DONATE |
| GUILD_CREDIT | Credit | TX_GUILD_SHOP、TX_GUILD_ACTIVITY_REWARD |
| TRADE_DEBIT | Debit | TX_AUCTION_SELL |
| TRADE_CREDIT | Credit | TX_AUCTION_BUY、TX_TRADE |
| SYSTEM_CREDIT | Credit | TX_SYSTEM_GRANT、TX_MAIL_ATTACHMENT、TX_GM_GRANT |

- `AssetDebit` 只收 Debit 流;`AssetCredit` 只收 Credit 流;`AssetAbortDebit` 收全部 5 条流,且不校验 tx_type。
- TRADE_* 两行是给聚宝斋 P2 的占位,由交易会话按需改,改动只动这张表。
- 本表只管"流 ↔ tx_type";"流 ↔ 调用方"由验签白名单管(第 8 部分 4.32)。SYSTEM_CREDIT 在 v1 没有合法调用方,验签一律拒绝,本行 tx 白名单只是预留。

## 4.9 统一处理流程

`class PlayerAssetOpSystem`(`player_asset_op.h/.cpp`),三个入口:
```cpp
static void Debit(const AssetOpRequest&, AssetOpResponse&);
static void AbortDebit(const AssetOpRequest&, AssetOpResponse&);
static void Credit(const AssetOpRequest&, AssetOpResponse&);
using PersistFn = bool (*)(entt::entity);
static void SetPersistFnForTest(PersistFn fn);   // nullptr 恢复默认
using AddCurrencyFn = uint32_t (*)(entt::entity, CurrencyType, int64_t, TransactionType, uint64_t);
static void SetAddCurrencyFnForTest(AddCurrencyFn fn);          // 第 9 部分 4.33;nullptr 恢复 &CurrencySystem::AddCurrency
static void SetSecretLookupForTest(AssetOpSecretLookup fn);     // 第 8 部分 4.32;nullptr 恢复读环境变量
```
三者都走私有的 `Process(Kind kind, req, resp)`。响应默认值为 `outcome=UNKNOWN, reason.id=0, durable=false, partial=false`。下文 `Record(X, reason)` 一律是 `RecordAssetOpOutcome(MutableAssetOpStream(ledger, stream), req.stream_epoch(), req.seq(), AssetOpRecordKind::kX, reason)` 的简写。步骤如下,顺序固定:

1. **信封校验**(不记账):`player_id != 0`、`seq != 0`、`stream_epoch != 0`、流方向与入口匹配、tx_type 在白名单内(Abort 跳过)。任一失败 → `UNKNOWN + kAssetInvalidBundle`,打 WARN。
1b. **验签**(不记账,第 8 部分 4.32):`VerifyAssetOpAuth(rpc, req, NowMs, lookup)`,rpc 取 `debit / abort_debit / credit`;非 kOk → `UNKNOWN + kAssetAuthFailed`,WARN 带 verdict 名。
2. **找人**:`tlsEcs.playerList.find(player_id)`;没找到或实体无效 → `NOT_HERE + kAssetPlayerNotHere`。
3. **账本损坏**:有 `PlayerAssetOpLedgerInvalidComp` → `RETRY + kAssetBlocked`,打 ERROR。
4. **分类**:`ClassifyAssetOpSeq(FindAssetOpStream(ledger, stream), stream_epoch, seq)`。
   - kInvalid → `UNKNOWN + kAssetInvalidBundle`。
   - kStaleEpoch / kBehindWindow / kJumpTooFar → `UNKNOWN`(reason 0),打 ERROR(带 player_id/stream/epoch/账本纪元/seq/watermark/max_seq)。
   - kApplied / kRejected → 回原结局;kRejected 的 reason 取 `AssetOpRejectionReason`;kApplied 且 `IsAssetOpPartial` → `partial=true, reason=kAssetPartialApplied`;`durable = IsDurable(..., epoch, seq, ...)`。若未 durable、玩家可改、且距上次请求 ≥500ms → RequestPersist 后重算 durable。**到此返回**,已见 seq 不经过任何闸门。
5. **改动闸门**(kUnseen / kAheadOfWindow):
   - 有 `PlayerFrozenComp` → `RETRY + kAssetFrozen`;
   - 有 `UnregisterPlayer` → `RETRY + kAssetPlayerNotHere`。
6. **Abort**:`Record(seq, applied=false, reason=0)` → RequestPersist → `REJECTED`,durable 取 IsDurable(通常为 false,Go 重查)。返回。
7. **应用闸门**:`PlayerBattleSystem::IsInBattle(player)` → `RETRY + kAssetInBattle`;缺 `CurrencyComp` 或(Credit 带物品时)缺 `PlayerBagsComp` → `RETRY + kAssetPlayerNotHere`(仍在加载)。
8. **包内容校验**(确定性,记账):失败 → `Record(REJECTED, kAssetInvalidBundle)` + RequestPersist → REJECTED。
   - **Debit(v1)**:恰好 1 个 currency、0 个 item;`currency_type < kCurrencyMax`;`1 <= amount <= INT64_MAX`。
   - **Credit**:currencies + items 至少 1 条;currencies ≤4 条且类型互不相同、各自合法;items ≤16 条、config_id 互不相同、`count >= 1`、`ItemTableManager::Instance().FindByIdSilent(config_id).first != nullptr`。
9. **Debit 应用**:
   - `!CurrencySystem::CanAfford(player, type, amount)` → `Record(REJECTED, kAssetCurrencyInsufficient)` → RequestPersist → REJECTED。
   - 否则 `err = CurrencySystem::DeductCurrency(player, type, amount, txType, correlation_id)`。
     - `err == kSuccess` → `Record(APPLIED)` → RequestPersist → APPLIED。
     - 其它 → 前面的闸门都已通过,这里不应失败:LOG_ERROR,回 `RETRY`、reason=err,不记账。
10. **Credit 应用**(先物品后货币:只有物品会暂时失败,先做它就不会留下半截)。**本步以第 9 部分 4.33 为准**:货币封禁预检 → 物品 guid 余量预检 → `AddItems(..., txType, correlation_id, &mutated)`(blockList 取法照 `player_mission.cpp:209-213`,`!mutated` 各分支同原稿:背包满 RETRY kAssetBagFull、kInvalidParameter 记 REJECTED kAssetBlocked、其它 RETRY)→ 逐条 AddCurrency(补缴抵扣属于正常应用)。一旦已有改动后又失败,记 `kAppliedPartial` 并回 `APPLIED + partial=true + kAssetPartialApplied`,**不再**把部分发放记成完整成功。
11. 每次调用结束打一行 INFO:`[AssetOp] rpc=… player_id=… stream=… seq=… corr=… outcome=… reason=… durable=…`(日志不是指标,可以带 player_id)。

**客户端余额/背包刷新**:本批不加 scene 推送。帮会写 RPC 返回后,由客户端主动拉背包(S5 负责)。

`scene_node_service.cpp`,**只改守护段**。proto 生成器会自动补出新的 `HandleAssetDebit / HandleAssetAbortDebit / HandleAssetCredit` 空守护段和包装函数:
```cpp
void SceneNodeGrpcImpl::HandleAssetDebit(const ::AssetOpRequest* request, ::AssetOpResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
    PlayerAssetOpSystem::Debit(*request, *response);
///<<< END WRITING YOUR CODE
}
```
另两个同理。顶部守护段加 `#include "player/system/player_asset_op.h"`。投递前守护段保持为空。

## 4.10 错误映射一览(scene → 响应)

| 条件 | outcome | reason | 记账 |
|---|---|---|---|
| 信封畸形 / seq=0 / epoch=0 / 流方向或 tx 不符 | UNKNOWN | kAssetInvalidBundle | 否 |
| 验签失败(调用方不符 / 密钥未配 / 时钟偏差 / 签名不符) | UNKNOWN | kAssetAuthFailed | 否 |
| 不在本节点 | NOT_HERE | kAssetPlayerNotHere | 否 |
| 账本损坏 | RETRY | kAssetBlocked | 否 |
| seq 落后窗口 / 纪元过期 / 跳号过远 | UNKNOWN | 0 | 否 |
| 物品 guid 号段余量不足 | RETRY | 0 | 否 |
| 已有改动后又失败(部分发放) | APPLIED,partial=true | kAssetPartialApplied | 是 |
| 已见 | 原结局 | 原原因 / 0 | 否(只读) |
| 冻结 | RETRY | kAssetFrozen | 否 |
| 退出中 | RETRY | kAssetPlayerNotHere | 否 |
| 战斗中(Debit/Credit) | RETRY | kAssetInBattle | 否 |
| 包内容非法 | REJECTED | kAssetInvalidBundle | 是 |
| 余额不足 | REJECTED | kAssetCurrencyInsufficient | 是 |
| 货币或物品被封禁 | REJECTED | kAssetBlocked | 是 |
| 背包满 | RETRY | kAssetBagFull | 否 |
| 中止未见 seq | REJECTED | 0 | 是 |
| 成功 | APPLIED | 0 | 是 |

## 4.11 CurrencySystem / TransactionLogSystem / BagService 改造

**`currency_system.h`**,签名加两个带默认值的参数,现有调用点(`player_attribute.cpp`、`player_pet.cpp`、`player_battle.cpp`、GM 处理器)不用改:
```cpp
static uint32_t AddCurrency(entt::entity player, CurrencyType type, int64_t amount,
                            TransactionType txType = TX_CURRENCY_ADD, uint64_t correlationId = 0);
static uint32_t DeductCurrency(entt::entity player, CurrencyType type, int64_t amount,
                               TransactionType txType = TX_CURRENCY_DEDUCT, uint64_t correlationId = 0);
```

**`currency_system.cpp`**,区分错误码(`#include "table/proto/tip/asset_error_tip.pb.h"`):

| 位置 | 旧 | 新 |
|---|---|---|
| Add 冻结(:78-85) | kInvalidParameter | kAssetFrozen |
| Add 全局封禁(:88-94)、个人封禁(:97-102) | kInvalidParameter | kAssetBlocked |
| Deduct 冻结(:186-193) | kInvalidParameter | kAssetFrozen |
| Deduct 余额不足(:203-210) | kInvalidParameter | kAssetCurrencyInsufficient |
| amount<=0、类型越界、缺组件 | kInvalidParameter | 不变 |

流水调用改为 `LogCurrencyAdd(player, type, amount, before, after, txType, correlationId)` 和 `LogCurrencyDeduct(..., txType, correlationId)`。

**这是客户端可见变化**(第二轮核实后修正):宠物、加点在扣币前先 `CanAfford` 并回各自的 `kPetGoldNotEnough / kAttributeGoldNotEnough`(`player_pet.cpp:582-583,675-676`,`player_attribute.cpp:658-659,750-751`),27000 走不到;但冻结(27003)、封禁(27005)与 GM 指令会把 27000 段码透传给客户端,而 Unity 的 `PetClient.DescribeTip` 等是手写镜像(`PetClient.cs:80-98`),未收录的码显示"tip=N"。服务端调用方不改;客户端补镜像,列为 B4a-client(第 11 部分 4.41)。`cpp/tests/currency_test/currency_test.cpp` 里断言 `kInvalidParameter` 的"余额不足 / 冻结 / 封禁"用例要跟着改期望值。Codex 用 `rg -n "kInvalidParameter" cpp/tests/currency_test/currency_test.cpp` 逐条核对。

**`transaction_log_system.h/.cpp`**:`LogCurrencyAdd`、`LogCurrencyDeduct`、`LogItemCreate` 末尾各加 `uint64_t correlationId = 0`,实现里 `entry.set_correlation_id(correlationId)`。

**`bag_service.h/.cpp`**:`ItemCountMap` 重载改为:
```cpp
static uint32_t AddItems(entt::entity playerEntity, Bag& bag, const PlayerItemBlockList& blockList,
                         const ItemCountMap& itemsToAdd, TransactionType txType = TX_SYSTEM_GRANT,
                         uint64_t correlationId = 0, bool* mutated = nullptr);
```
- 函数开头 `if (mutated) *mutated = false;`。
- `ReserveForBatchAdd` 若淘汰了实例(`!evicted.empty()`),或逐配置循环里任一 `bag.AddItem` 写出了 guid,都置 `*mutated = true`。
- `LogItemCreate(..., txType, correlationId)`。

资产通道只往 `kInventory` 发放。主背包的策略是 RejectWhenFull,不会淘汰;这里保留"淘汰也算改动"的判定,是为了防御以后调用方传入其它背包。

## 4.12 GM 货币指令闸门

> **2026-09-18 实际落码形状与本节不同,以代码为准**(P0-a 一并做掉了)。**不存在** `client_gm_gate.h`,也**没有** `MMORPG_ALLOW_CLIENT_GM` 这个环境变量。实际是两道锁:
> gate 按**消息号**闸(`cpp/nodes/gate/gate_gm_client_messages.h` 的 `kGmClientMessageIds` 清单 + `gate_security.h::ClassifyGmClientMessage`,接在 `client_message_processor.cpp:925`),
> scene 再拦一次防绕开 gate 直连(`cpp/nodes/scene/handler/rpc/player/player_gm_guard.h`)。
> 判据是运行模式 `GATE_RUN_MODE` / `SCENE_RUN_MODE`,**未设置 = prod = 拒绝**,部署链从不注入这两个变量;
> 本机启动器兜底 dev,robot 冒烟不受影响。细节见 `cpp/nodes/gate/SECURITY.md` §3。
> 下面的原文保留作设计留痕,**不要照它落码**——照落会多出一个与现有闸门互不知情的第二开关。

> **第二轮评审后作废其中"逐 handler 加闸"的做法**:改为在 `ProcessClientPlayerMessage` 分发入口统一拦截全部客户端 `Gm*`(第 9 部分 4.34,批次 B4a-2)。下面保留的只有 `client_gm_gate.h` 的两个函数与启动脚本约定;`player_currency_handler.cpp` 不再写闸门代码,只改 TX_GM_*。

新头文件 `cpp/libs/services/scene/player/system/client_gm_gate.h`(header-only,无需工程登记):
```cpp
#pragma once
#include <cstdlib>
#include <string_view>
// 客户端 GM 指令(免费造币)默认拒绝;仅本地/冒烟环境显式 MMORPG_ALLOW_CLIENT_GM=1。
inline bool IsClientGmAllowedValue(const char* v) { return v != nullptr && std::string_view(v) == "1"; }
inline bool IsClientGmAllowed() {
    static const bool allowed = IsClientGmAllowedValue(std::getenv("MMORPG_ALLOW_CLIENT_GM"));
    return allowed;
}
```

`player_currency_handler.cpp` 中 `GmAddCurrency`、`GmDeductCurrency` 的守护段开头各加:
```cpp
if (!IsClientGmAllowed()) {
    LOG_WARN << "GmAddCurrency 拒绝: 未开启 MMORPG_ALLOW_CLIENT_GM";
    tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(kFeatureUnavailable);
    return;
}
```
- GM 处理器的流水 tx 同时改成 `TX_GM_GRANT` / `TX_GM_DEDUCT`。
- `GmBlockCurrency`、`GmUnblockCurrency`、`GmGrantPet` 不在契约范围内,列入契约偏差。

**启动脚本**:
- ~~`tools/scripts/cpp_nodes.ps1` … 设 `$env:MMORPG_ALLOW_CLIENT_GM = "1"`,新增 `[switch]$NoClientGm`~~ —— **作废,不要做**:GM 放行已由 `SCENE_RUN_MODE` 兜底 dev 决定,再加一个开关就是第二个互不知情的闸。该脚本**仍要改**的是注入两把资产通道开发密钥,写法见第 11 部分 4.41。
- `tools/scripts/dev_mprocs_proc.ps1` 若直接拉起 scene,也设同一变量。
- `tools/scripts/currency_crash_window.ps1`:若在 :180 附近直接 Start-Process scene,而不是经过 cpp_nodes.ps1,也在那里设。
- K8s 清单**一律不设**,默认拒绝。

## 4.13 与回档/补缴/封禁的交互小结

- **补缴**:Credit 走 AddCurrency 统一入口,欠款先扣,记 APPLIED(与聚宝斋 §6.2 一致)。
- **封禁**:REJECTED + kAssetBlocked,记账。Go 按业务退款(帮会商店退帮贡)。
- **回档**:见 C10,必须 fail-closed。`data_service` 回档代码本批不改;前置检查与 guild 查询 RPC 随 B5 落(第 10 部分 4.39)。

<!-- s4_asset_part4.md -->

# S4 通用资产通道 — 第 4 部分:C++ 测试、B4a 文件清单、Codex 验证

## 4.14 C++ gtest

全部加进现有 `cpp/tests/currency_test` 工程。该工程的 AdditionalDependencies 已链接 `scene.lib`、`modules.lib`、`table.lib`(`currency_test.vcxproj:138`),不用新建工程。

**第二轮评审统一约定**:下表用例一律按新签名书写——Classify/Record 带纪元参数,未特别说明时纪元取 100;ECS 用例的请求都带纪元 100,并经测试助手 `SignForTest(rpc, req)` 用夹具密钥签名(夹具 `SetSecretLookupForTest` 返回 32 字节固定串)。新增用例见第 8 部分 4.30、4.32 与第 9 部分 4.33;`ClientGmGateValue` 移到 `client_gm_gate_test.cpp`(第 9 部分 4.34)。

### 4.14.1 `asset_op_ledger_test.cpp`(纯函数,无 ECS)

| 用例 | 断言 |
|---|---|
| `LedgerEmptyClassify` | 空流:seq 0 → kInvalid;1、1024 → kUnseen;1025 → kAheadOfWindow |
| `LedgerRecordApplyReject` | 记 3=APPLIED、5=REJECTED(27000) → 3 kApplied、5 kRejected、4 kUnseen;原因(5)=27000,原因(3)=0 |
| `LedgerAppliedSubsetOfSeen` | 每次 Record 后 `(applied & ~seen) == 0` |
| `LedgerSlideByOne` | 记 1..1024 全 APPLIED 后记 1025 → watermark=1;1 → kBehindWindow;2 → kApplied;1025 → kApplied |
| `LedgerSlideExactly1024` | 记 1 后记 2048 → watermark=1024;1 → kBehindWindow;2048 → kApplied |
| `LedgerSlideBeyondWindowClears` | 记 1 后记 5000 → watermark=3976,位只剩最高位 |
| `LedgerRejectionsPrunedOnSlide` | 记 2=REJECTED 后记 1030 → rejections 为空 |
| `LedgerAbortPlaceholderNotInRejectionRing` | 中止占位(reason 0)不占环:63 个占位夹在中间,仍能攒满 64 条真原因且最早一条不被挤掉 |
| `LedgerEvictedRejectionReasonDegradesByDesign` | 65 条业务拒绝挤掉第 1 条 → 重查 reason 回 0,答复与中止占位逐项相同。**这是刻意接受的退化**(见 4.9 第 2 步),失败时先查判据有没有被改,别去修原因环 |
| `LedgerJumpCapOverflowGuardBoundary` | 跳号上限的 uint64 溢出边界三取值:`max−jump+1` 任何 seq 都不判跳号;`max−jump` 上限恰为 `kUint64Max`(无 seq 可超,注释写明);`max−jump−1` 才观测得到 kJumpTooFar |
| `LedgerHigherEpochResetsWatermarkNearOverflow` | watermark 接近上限时:同纪元判 kInvalid;更大纪元记账成功且记账后 watermark == 0(堵住 Classify 不看 watermark 的那条腿) |
| `LedgerRejectionRingCap` | 连续 70 个 REJECTED → 只剩最大 64 个,升序;最早的原因 → 0 |
| `LedgerRecordPreconditionFails` | 对已见 seq 再 Record → 返回 false 且状态不变 |
| `LedgerWatermarkOverflowInvalid` | `watermark = UINT64_MAX − 1000` → kInvalid |
| `LedgerStreamsSortedUnique` | 先 Mutable(5) 再 Mutable(1) → streams 顺序为 1、5;重复 Mutable 不新增 |
| `LedgerValidate*`(6 个) | 位数组 15 个、applied 非 seen 子集、流重复、流=0、rejection 在窗外、rejection 对应位为 applied → 均返回非空原因 |
| `LedgerGapGuardProof` | 模拟 Go 守卫:随机 10000 步,每步分配 seq 并随机终结,始终保证未决 ≤16、跨度 <512;scene 随机记账。断言任一未决 seq 的分类都 ≠ kBehindWindow,且每次新分配的 seq 分类都 ≠ kJumpTooFar(固定随机种子,纪元固定 100) |

### 4.14.2 `asset_op_system_test.cpp`(ECS 级)

**夹具**:
- 建实体,emplace `Guid`、`CurrencyComp`、`PlayerCurrencyComp`、`PlayerAssetOpLedgerComp`;`tlsEcs.playerList[playerId] = entity`。
- 需要物品的用例再按 `cpp/tests/bag_test` 现有做法构造 `PlayerBagsComp`,并加载 Item 表测试数据(照 `bag_test` 的配表初始化)。
- `PlayerAssetOpSystem::SetPersistFnForTest(&FakePersist)`。`FakePersist` 计数加一,返回 `g_fakeWrite`(默认 true),**不**立即更新快照。
- 助手 `CompleteFakeSave(entity)`:把当前 `PlayerAssetOpLedgerComp` 拷进一份 `PlayerAllData.player_database_data().asset_op_ledger()`,再 `get_or_emplace<PlayerLastPersistedSnapshotComp>().Replace(...)`,模拟回调到达。
- TearDown:销毁实体、清 playerList、`SetPersistFnForTest(nullptr)`。

| 用例 | 步骤与断言 |
|---|---|
| `DebitAppliedThenDurable` | 金币 100,Debit(GUILD_DEBIT, seq 1, 30, TX_GUILD_DONATE, corr 77) → APPLIED、durable=false、余额 70、persist 计 1;同 seq 再调 → APPLIED、余额仍 70;CompleteFakeSave 后再调 → durable=true |
| `DebitInsufficientRejectedFixed` | 余额 10,扣 30 → REJECTED、reason=kAssetCurrencyInsufficient;加币到 100 后同 seq → 仍 REJECTED、余额 100 |
| `NotOnNode` | playerList 无该玩家 → NOT_HERE,账本无流 |
| `FrozenRetryNotRecorded` | emplace PlayerFrozenComp → RETRY kAssetFrozen、账本无流;移除后 → APPLIED |
| `InBattleRetryAbortAllowed` | emplace InBattleComp,Debit → RETRY kAssetInBattle;Abort 同 seq → REJECTED;移除 InBattleComp 后 Debit → REJECTED、余额不变 |
| `AbortSeenAppliedReturnsApplied` | Debit seq1 APPLIED,Abort seq1 → APPLIED |
| `UnregisterRetryButReadOnlyAnswers` | Debit seq1 APPLIED;emplace UnregisterPlayer;Debit seq2 → RETRY kAssetPlayerNotHere;Debit seq1 → APPLIED 且 persist 计数不增加 |
| `BehindWindowUnknown` | Debit seq1,再 Debit seq 5000 → 两次 APPLIED;seq1 → UNKNOWN |
| `EnvelopeInvalid` | GUILD_CREDIT 调 AssetDebit → UNKNOWN kAssetInvalidBundle;tx=TX_GUILD_SHOP 调 GUILD_DEBIT → UNKNOWN;seq=0 → UNKNOWN;账本均无记录 |
| `DebitBundleInvalidRecorded` | Debit 带 1 个 item → REJECTED kAssetInvalidBundle 且已记账;2 个 currency → 同样 |
| `CreditWithDebtClawback` | AttachDebt(金币 50),Credit 30 → APPLIED、余额 0、debt.paid=30 |
| `CreditBlockedRejected` | BlockCurrency(金币),Credit → REJECTED kAssetBlocked |
| `CreditBagFullRetry` | 主背包塞满,Credit 物品 → RETRY kAssetBagFull、未记账、背包不变;腾一格后同 seq → APPLIED |
| `CreditUnknownItemRejected` | config_id 不存在 → REJECTED kAssetInvalidBundle |
| `SkippedSaveMeansDurable` | Debit APPLIED 后 CompleteFakeSave;令 `g_fakeWrite=false`,过 500ms(注入时钟,或直接改 `PlayerAssetOpPersistRequestComp.lastRequestMs`)后重查 → durable=true |
| `LedgerInvalidTagRetry` | emplace PlayerAssetOpLedgerInvalidComp → RETRY kAssetBlocked |
| `ResaveThrottle` | APPLIED 后连续重查 3 次(时间不前进)→ persist 计数仍为 1 |

### 4.14.3 `currency_test.cpp` 追加与修正

- `DeductInsufficientReturnsAssetCode`:余额 10 扣 20 → `kAssetCurrencyInsufficient`。
- `DeductFrozenReturnsAssetFrozen`、`AddFrozenReturnsAssetFrozen`:emplace `PlayerFrozenComp` 后断言。前提是 `IsCrossZoneFrozen` 基于该组件,落码时核对 `player_lifecycle.h:67` 的实现。
- `AddBlockedReturnsAssetBlocked`。
- `ClientGmGateValue`:`nullptr` / `"0"` / `"true"` → false;`"1"` → true。
- 修正既有用例里余额不足、冻结、封禁的期望值(原 `kInvalidParameter`)。

## 4.15 B4a 文件清单(手改 30 个)

> **已作废**:第二轮评审后 B4a 拆为 B4a-1(30)/ B4a-2(6)/ B4a-client(2),清单以第 11 部分 4.41 为准。下表仅留作对照。

| # | 文件 | 性质 |
|---|---|---|
| 1 | `proto/common/asset/asset_op.proto` | 新增 |
| 2 | `proto/common/component/asset_op_ledger_comp.proto` | 新增 |
| 3 | `proto/scene_manager/scene_node_service.proto` | 改 |
| 4 | `proto/common/database/mysql_database_table.proto` | 改 |
| 5 | `proto/common/rollback/transaction_log.proto` | 改 |
| 6 | `data/tip/Tip.xlsx` | 加组头 + 7 行 |
| 7–8 | `cpp/libs/services/scene/player/system/asset_op_ledger.h/.cpp` | 新增 |
| 9–10 | `cpp/libs/services/scene/player/system/player_asset_op.h/.cpp` | 新增 |
| 11 | ~~`cpp/libs/services/scene/player/system/client_gm_gate.h`~~ | **作废,不要新增**:闸门已由 P0-a 落在 `cpp/nodes/gate/gate_gm_client_messages.h` + `cpp/nodes/scene/handler/rpc/player/player_gm_guard.h` |
| 12 | `cpp/libs/services/scene/CMakeLists.txt` | 登记 7、9(源文件显式列表,照 :91 `player_lifecycle.cpp` 行) |
| 13 | `cpp/libs/services/scene/scene.vcxproj` | 登记 7–11(.filters 可选,不计) |
| 14 | `cpp/libs/services/scene/player/system/player_database_loader.cpp` | 改 |
| 15 | `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | 仅守护段 |
| 16 | `cpp/nodes/scene/handler/rpc/player/player_currency_handler.cpp` | 仅守护段 |
| 17–18 | `cpp/libs/modules/currency/system/currency_system.h/.cpp` | 改 |
| 19–20 | `cpp/libs/modules/transaction_log/transaction_log_system.h/.cpp` | 改 |
| 21–22 | `cpp/libs/modules/bag/bag_service.h/.cpp` | 改 |
| 23–24 | `cpp/generated/table/CMakeLists.txt`、`table.vcxproj` | 登记 asset_error_tip.pb.cc(AGENTS §4.4 规定手工) |
| 25–26 | `cpp/tests/currency_test/asset_op_ledger_test.cpp`、`asset_op_system_test.cpp` | 新增 |
| 27 | `cpp/tests/currency_test/currency_test.cpp` | 改 |
| 28 | `cpp/tests/currency_test/currency_test.vcxproj` | 登记 25–26 |
| 29 | `tools/scripts/cpp_nodes.ps1` | GM 环境变量 |
| 30 | `tools/scripts/dev_mprocs_proc.ps1` | 同上(不拉起 scene 就不改,名额留给 `currency_crash_window.ps1`) |

- PROGRESS.md 与 `docs/design/guild-phase2.md` 属于文档,随批次追加,不计入手改代码名额。
- 生成物(proto 输出、Handle 桩、tip 生成文件)不计入。

## 4.16 B4a Codex 验证步骤(串行,工作目录 `E:\work\xuanming-server-mmo`)

> 第二轮评审增补(第 0 步目录与 message_id 对比、第 1 步路由表检查、新第 1b 步 go/db 迁移、第 4 步过滤器、第 6 步字段号检查、B4a-2 与 B4a-client 的验证)见第 11 部分 4.42,与下文冲突时以 4.42 为准。

0. **前置**:按"xuanming 构建工具链"现行入口加载 buildenv。`git status --short proto cpp/generated go/proto robot/generated data/tip generated tools/data_table_exporter/state` 存为 `pre-b4a-status.txt`,确认并行会话(trade / team / chat)的未提交生成物在跑生成前后不被覆盖;有冲突先停下报告。
1. **proto 生成**:`cd go; .\build.bat`。通过标准:
   - `Test-Path cpp/generated/proto/common/asset/asset_op.pb.cc`;
   - `Select-String cpp/generated/proto/CMakeLists.txt -Pattern 'common/asset/asset_op.pb.cc'`;
   - `Select-String cpp/nodes/scene/handler/grpc/scene_node_service.h -Pattern 'HandleAssetDebit'`;
   - `Test-Path go/proto/common/asset`。
   - 若 `proto/common/asset/` 目录没被扫到(上面任一条失败),停下报告:回退方案是把 asset_op.proto 挪到 `proto/common/component/`,只改 import 路径,需设计确认。
2. **导表**(按 `data/AGENTS.md` 流程)。通过标准:
   - `generated/code/proto/tip/asset_error_tip.proto` 含 `kAssetCurrencyInsufficient = 27000`;
   - `go/shared/generated/tip/segments.go` 出现 `Group: "asset_error", Base: 27000`;
   - `tip_enum_ids.json` 的 diff 只新增不改动。
3. **C++ 编译**,MSBuild Debug x64,必须 `/m:1` 串行:table → proto → modules → scene(lib)→ scene 节点工程 → currency_test。通过标准:0 error;新增 warning 不超过基线。
4. **单测**:运行 currency_test 产物(路径以 vcxproj OutDir 为准),`--gtest_filter=AssetOp*:CurrencyTest*:*Ledger*`。全绿;保留失败用例的完整输出。
5. **Linux CMake**:`build_linux.sh`(或现行 Linux 构建入口)只编 scene 相关目标,验证 CMake 登记没有漏。
6. **静态核对**:
   - `rg -n "CurrencySystem::(Add|Deduct)Currency\(" cpp --glob '!cpp/generated/**'`:调用点编译通过即可;
   - `rg -n "asset_op_ledger = 1[56];" proto`:确认字段号唯一。
7. **冒烟**(需本地全栈,可与 B4b 合跑,步骤以第 11 部分 4.40 为准):GM 闸门部分在 B4a-2 之后验证——`cpp_nodes.ps1 -NoClientGm` 启动 scene,robot GmAddCurrency、GmGrantPet 应回 1006;按默认脚本启动应回成功。

失败时保留:生成器输出末 200 行、MSBuild 首个 error 上下文、gtest 失败用例输出。未跑到的步骤如实标注"未验证"。

<!-- s4_asset_part5.md -->

# S4 通用资产通道 — 第 5 部分:Go 侧(B4b)`go/shared/scenenode` 与 `go/shared/assetop`

## 4.17 模块依赖

- `go/shared/go.mod` 增加 `require proto v0.0.0-00010101000000-000000000000`(**不是** `v0.0.0`,理由见第 10 部分 4.36)和 `replace proto => ../proto`,然后 `go mod tidy`。
  - 已核实:所有依赖 shared 的服务模块都已有自己的 `replace proto => ../proto`,新依赖不会让它们解析失败。
  - shared 自己跑单测需要这条 replace。
- 生成的 Go 包:`proto/common/asset`(别名 `assetpb`),`proto/scene_manager`(别名 `smpb`,含 `PlayerLocation` 与 `SceneNodeGrpcClient`,与 match `location.go:8`、`gather.go:312` 用法一致)。
- **match 不迁移**:`go/match/internal/discovery` 保持原样,日后单独一批改用 shared。

## 4.18 `go/shared/scenenode`

### watcher.go

从 `go/match/internal/discovery/node_watcher.go` 复制 `nodeRegistrationJSON`、`NodeEntry`、list-watch 循环(fullSync / watch / handleEvent / Upsert / parse / EndpointOf / Count),类型改名 `Watcher`。解析语义逐字保留:
- 跳过 `/allocated/`;
- 只认 `grpcEndpoint`;
- 同身份多条注册即拒绝选中。

不复制 `EndpointOfNode`、`PickRandom`,battle 选点仍留在 match。新增:
```go
func NewWatcher(name, prefix string, onCount func(string, int), onRemove func(NodeEntry)) *Watcher
func (w *Watcher) Run(ctx context.Context, etcd *clientv3.Client)   // 调用方 safego.Go 启动
func (w *Watcher) Synced() bool                                     // 首次 fullSync 成功后为 true(atomic.Bool)
```

### conn.go

连接缓存改为实例,不用包级全局变量,测试可隔离:
```go
type ConnCache struct { mu sync.RWMutex; conns map[string]*grpc.ClientConn }
func NewConnCache() *ConnCache
func (c *ConnCache) Dial(endpoint string) (*grpc.ClientConn, error) // grpc.NewClient + insecure,双检加锁,同 match
func (c *ConnCache) Remove(endpoint string)
func (c *ConnCache) Close()
```

### locator.go

```go
const SceneNodeRpcPrefix = "SceneNodeService.rpc/"
func LocationKey(playerID uint64) string { return fmt.Sprintf("player:%d:location", playerID) }

// 键不存在返回 (nil, nil)
type LocationReader interface {
    GetLocation(ctx context.Context, key string) ([]byte, error)
}
type GoRedisReader struct{ Client *redis.Client } // redis.Nil → (nil, nil)

var (
    ErrNotOnline   = errors.New("scenenode: player location absent")
    ErrNodeUnknown = errors.New("scenenode: scene node not registered or ambiguous")
)

type Target struct {
    Location *smpb.PlayerLocation
    Endpoint string
    Client   smpb.SceneNodeGrpcClient
}

type Locator struct {
    Reader  LocationReader
    Watcher *Watcher
    Dial    func(endpoint string) (smpb.SceneNodeGrpcClient, error) // nil 时用 Conns
    Conns   *ConnCache
    Metrics *Metrics // 可为 nil
}
func (l *Locator) Resolve(ctx context.Context, playerID uint64) (Target, error)
```

`Resolve` 的步骤:
1. 读键;为 nil → `ErrNotOnline`。
2. `proto.Unmarshal` 失败 → 包装错误(不是 ErrNotOnline)。
3. `Watcher.EndpointOf(loc.ZoneId, loc.NodeId)` 失败 → `fmt.Errorf("%w: %v", ErrNodeUnknown, err)`。
4. 拨号,拿到客户端。

Redis 句柄:guild 复用 `PlayerLocatorRedisClient`(本地与 SharedRedis 同实例;K8s 部署时须指向 SharedRedis,critic.md 已记)。

### metrics.go

- `scenenode_nodes{service}` Gauge,由 onCount 回调设置;
- `scenenode_resolve_total{service,result}` Counter,result ∈ found / not_online / node_unknown / error。
- `func NewMetrics(reg prometheus.Registerer, service string) *Metrics`,内部 `promauto.With(reg)`。
- 不带 player_id label。

## 4.19 `go/shared/assetop`

> **第二轮评审修订**:`Status` 改为纯语义枚举并新增 `StatusAppliedPartial`;`Result.Partial`、`Op.StreamEpoch / LastReason`;seq.go 带纪元与 `WithTxRetry`;Caller 必须带 `Signer`;`FinalStatus` 多一个 op 参数;reconcile 的 `ClaimDue` 换成 `ListDue + Claim` 两步、`Workers` 有界并发、毒行跳过、可选 `LedgerReader` 与 `ManualResolver`;新文件 `auth.go`。以上均以第 10 部分 4.37–4.38、第 8 部分 4.32 为准,本节下文与之冲突处作废。

### types.go

```go
type Status uint32 // 业务 outbox 表的 status 列,数值固定
const (
    StatusPending  Status = 0
    StatusApplied  Status = 1
    StatusRejected Status = 2 // scene 判拒
    StatusAborted  Status = 3 // 中止占位(Abort 回 REJECTED 且 reason==0)
)

type RPC uint8
const ( RPCDebit RPC = iota + 1; RPCCredit; RPCAbort )
func ApplyRPCOf(stream assetpb.AssetOpStream) (RPC, bool) // *_DEBIT → Debit;*_CREDIT → Credit

type Result struct {
    Outcome assetpb.AssetOpOutcome
    Reason  uint32
    Durable bool
    Local   bool // 未发 RPC(无位置/节点未知)时本地合成的 NOT_HERE
}
func (r Result) Terminal() bool // (APPLIED||REJECTED) && Durable

type Op struct {
    OpID, PlayerID uint64
    Stream         assetpb.AssetOpStream
    Seq, CorrelationID uint64
    TxType         uint32
    Bundle         *assetpb.AssetBundle
    Attempts       uint32
    DeadlineMs     uint64 // 0 = 永不中止
    LeaseToken     uint64
}
func (o Op) Request() *assetpb.AssetOpRequest

type Limits struct{ MaxPending uint32; MaxSpan uint64 }
var DefaultLimits = Limits{MaxPending: 16, MaxSpan: 512} // 与 scene 窗口 1024 的证明绑定,改前先改证明

var (
    ErrTooManyPending = errors.New("assetop: too many pending ops for player stream")
    ErrSeqRowMissing  = errors.New("assetop: seq row missing, call EnsureSeqRow first")
    ErrOutcomeFlip    = errors.New("assetop: scene outcome changed between queries")
)
```

### seq.go(seq 分配,锁序在业务事务内)

```go
type SeqTables struct{ SeqTable, OpTable string }
func NewSeqTables(seq, op string) (SeqTables, error) // 表名须匹配 ^[a-z][a-z0-9_]{0,62}$,防拼接注入

// 事务外 autocommit 执行,避免首次并发时两个共享锁升级成死锁
func EnsureSeqRow(ctx context.Context, db *sql.DB, t SeqTables, playerID uint64, s assetpb.AssetOpStream) error
//   INSERT IGNORE INTO {seq} (player_id, stream, next_seq) VALUES (?, ?, 1)

func AllocateSeq(ctx context.Context, tx *sql.Tx, t SeqTables, playerID uint64, s assetpb.AssetOpStream, lim Limits) (uint64, error)
//   SELECT next_seq FROM {seq} WHERE player_id=? AND stream=? FOR UPDATE      -- 无行 → ErrSeqRowMissing
//   SELECT seq FROM {op} WHERE player_id=? AND stream=? AND status=0 ORDER BY seq LIMIT ? FOR UPDATE   -- LIMIT = MaxPending+1
//   n=len(rows); min=rows[0]
//   n >= MaxPending || (n>0 && next_seq-min >= MaxSpan) → ErrTooManyPending
//   UPDATE {seq} SET next_seq = next_seq + 1 WHERE player_id=? AND stream=?
//   return next_seq
```

- 未决行用**加锁读**而不是 `COUNT(*)`:业务事务里可能已经做过普通读,MySQL RR 的一致性快照会看不到并发分配者的插入;加锁读总读最新版本,且 MySQL 与 TiDB 都支持。
- **全局锁序**(帮会):`guild → guild_member → guild_player_op_seq → guild_asset_op`。
- op 行由业务仓库在同一事务插入:`status=0`、`lease_token=提交令牌`、`lease_until_ms=now+10000`、`next_attempt_ms=now+10000`。这样重投循环不会与提交后的即时投递抢同一行。

**给 B1/B5 的表形状要求**(`guild_asset_op`,trade 同理):
- 列:`op_id`、`player_id`、`stream`、`seq`、`kind`、`status`、`durable`、`attempts`、`next_attempt_ms`、`deadline_ms`、`lease_until_ms`、`lease_token`、`tx_type`、`guild_id`、`ref_id`、`payload`(AssetBundle 序列化)、`last_outcome`、`last_reason`、`created_ms`、`updated_ms`。
- 唯一 `(player_id, stream, seq)`。
- 普通索引 `(status, next_attempt_ms)`、`(player_id, stream, status, seq)`。
- `guild_player_op_seq` 列:`player_id`、`stream`、`next_seq`。

### caller.go(一次投递 + durable 重查)

```go
type Resolver interface{ Resolve(ctx context.Context, playerID uint64) (scenenode.Target, error) }
type Caller struct {
    Resolver    Resolver
    CallTimeout time.Duration   // 800ms
    Requery     []time.Duration // DefaultRequery = 100ms, 200ms, 400ms
    Metrics     *Metrics
}
func (c *Caller) Do(ctx context.Context, rpc RPC, req *assetpb.AssetOpRequest) (Result, error)
```

`Do` 的步骤:
1. `Resolve`。`ErrNotOnline` 或 `ErrNodeUnknown` → `Result{Outcome: NOT_HERE, Local: true}, nil`;其它错误原样返回。
2. `call`:单次超时 = `min(CallTimeout, ctx 剩余)`,按 rpc 调 `AssetDebit / AssetCredit / AssetAbortDebit`。gRPC 错误原样返回(调用方按重试处理)。
3. 结果为 `NOT_HERE` 时,位置可能过期:再 Resolve 一次;endpoint 变了才对新节点重调一次。
4. 结果为 APPLIED/REJECTED 但未 durable:按 Requery 依次等待后以**同一请求**重调。
   - ctx 到期 → 返回最后一次(未 durable)结果,err=nil;
   - 重调出错 → 同样返回最后结果;
   - 结局在两次之间变了 → `ErrOutcomeFlip`,计数告警。
5. 重查对 Debit/Credit 安全:scene 见过的 seq 只读答复;若 scene 在两次之间崩溃丢了状态,重调就是那唯一一次应用。

### decide.go

```go
type Action uint8
const ( ActionFinalize Action = iota + 1; ActionAwaitDurable; ActionRetry; ActionAlert )
func Decide(res Result, err error) Action
//   err != nil(含 ErrOutcomeFlip 以外的错误)        → Retry
//   ErrOutcomeFlip / UNKNOWN                          → Alert
//   Terminal()                                        → Finalize
//   APPLIED/REJECTED 且 !Durable                       → AwaitDurable
//   RETRY / NOT_HERE                                  → Retry
func FinalStatus(rpc RPC, res Result) Status // APPLIED→Applied;REJECTED: rpc==Abort && Reason==0 → Aborted,否则 Rejected
func NextAttemptMs(nowMs uint64, attempts uint32, base, max time.Duration, rnd func() float64) uint64
//   delay = min(base << min(attempts,16), max) × (0.8 + 0.4×rnd()),结果不小于 base×0.8
```

### reconcile.go(重投循环骨架)

```go
type Store interface {
    // UPDATE {op} SET lease_until_ms=?, lease_token=? WHERE status=0 AND next_attempt_ms<=? AND lease_until_ms<?
    //   ORDER BY next_attempt_ms LIMIT ?;  然后 SELECT … WHERE lease_token=? AND status=0
    ClaimDue(ctx context.Context, nowMs uint64, limit int, leaseUntilMs, token uint64) ([]Op, error)
    // 单事务:业务锁序加锁 → UPDATE {op} SET status=?, durable=1, last_outcome=?, last_reason=?, updated_ms=?
    //   WHERE op_id=? AND status=0;RowsAffected==1 才做对侧账(加资金 / 退帮贡)。返回是否本次终结
    Finalize(ctx context.Context, op Op, status Status, res Result, nowMs uint64) (bool, error)
    // UPDATE {op} SET attempts=attempts+1, next_attempt_ms=?, lease_until_ms=0, durable=?, last_outcome=?,
    //   last_reason=?, updated_ms=? WHERE op_id=? AND status=0 AND lease_token=?
    Reschedule(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64) error
}
type Applier interface{ Do(ctx context.Context, rpc RPC, req *assetpb.AssetOpRequest) (Result, error) }

type LoopConfig struct {
    Interval          time.Duration // 2s
    Batch             int           // 100
    Lease             time.Duration // 10s
    OpBudget          time.Duration // 2500ms,须 < Lease
    BaseBackoff       time.Duration // 帮会取 GuildRule.asset_op_retry_base_ms,默认 1000ms
    MaxBackoff        time.Duration // 60s
    AwaitDurableDelay time.Duration // 500ms
}
type Processed struct{ Result Result; Finalized bool; Status Status }

func NewLoop(cfg LoopConfig, store Store, applier Applier, m *Metrics, now func() time.Time) (*Loop, error) // 校验 OpBudget < Lease 等
func (l *Loop) Run(ctx context.Context)                                    // 按 Interval 反复 Tick;调用方 safego.Go
func (l *Loop) Tick(ctx context.Context) int                               // 领一批,逐行 ProcessOne
func (l *Loop) ProcessOne(ctx context.Context, op Op) (Processed, error)   // 业务 RPC 提交后同步调用同一函数
```

`ProcessOne` 的步骤:
1. `rpc = DeadlineMs != 0 && now >= DeadlineMs ? RPCAbort : ApplyRPCOf(stream)`。
2. 在 `ctx(OpBudget)` 内调 `applier.Do`。
3. 按 Decide 分支:
   - Finalize → `store.Finalize(FinalStatus)`;
   - AwaitDurable → Reschedule(now+500ms);
   - Retry → Reschedule(NextAttemptMs);
   - Alert → ERROR 日志(op_id / stream / seq)+ Reschedule(now+MaxBackoff)。
4. store 出错只计数、打日志,继续下一行。
5. 离线玩家的行会逐步退避到 60s 一次;中止在离线期间同样拿不到结局。第二轮修订:无位置且尝试 ≥3 次时,经可选 `LedgerReader` 读取已落盘账本,已见结局直接终结;未见的仍等上线(第 10 部分 4.37)。

**帮会同步路径时间预算**:guild 写 RPC 给 `ProcessOne` 的 ctx 为 2500ms,`CallTimeout 800ms` + 重查 700ms 仍在契约 §2 的 3000ms 内。

### metrics.go

`NewMetrics(reg prometheus.Registerer, service string)`,label 全部低基数:
- `assetop_rpc_total{service,stream,rpc,outcome}`,outcome ∈ applied / rejected / retry / not_here / unknown / error / no_location;
- `assetop_rpc_seconds{service,rpc}` 直方图,桶 .005 .01 .025 .05 .1 .25 .5 1 2.5;
- `assetop_requery_total{service,rpc,result}`,result ∈ durable / timeout / error;
- `assetop_finalize_total{service,stream,status}`;
- `assetop_reschedule_total{service,stream,reason}`,reason ∈ await_durable / retry / alert;
- `assetop_unknown_total{service,stream}`、`assetop_outcome_flip_total{service,stream}`;
- `assetop_store_errors_total{service,op}`,op ∈ claim / finalize / reschedule;
- `assetop_pending_oldest_age_seconds{service,stream}`:Store 若实现可选接口 `OldestPendingCreatedMs(ctx, stream) (uint64, bool, error)`,Run 每 30s 更新一次。

## 4.20 Go 测试

| 文件 | 用例 |
|---|---|
| `scenenode/watcher_test.go` | EndpointOf 命中 / 未注册 / 同身份两条歧义;parse 跳过 `/allocated/` 与缺 grpcEndpoint;Upsert 换 endpoint 触发 onRemove;Synced 首次为 false |
| `scenenode/locator_test.go` | 假 Reader:键缺失 → ErrNotOnline;坏字节 → 非 ErrNotOnline 错误;节点未注册 → `errors.Is(err, ErrNodeUnknown)`;命中 → Dial 收到正确 endpoint |
| `assetop/decide_test.go` | Decide 全表;FinalStatus(Abort+reason0=Aborted,Abort+27000=Rejected,Credit REJECTED=Rejected);NextAttemptMs:attempts=3、base=1s → [6.4s, 9.6s],attempts=20 封顶 60s×[0.8,1.2] |
| `assetop/caller_test.go` | 假 Resolver + 嵌入 `smpb.SceneNodeGrpcClient` 的假客户端:①先 APPLIED 未 durable、第二次 durable → 共 2 次调用;②ctx 150ms → ≤2 次调用,返回未 durable、无错;③NOT_HERE 且位置换节点 → 新节点被调 1 次;④NOT_HERE 同节点 → 不重调;⑤APPLIED→REJECTED → ErrOutcomeFlip;⑥ErrNotOnline → Local NOT_HERE、零调用;⑦gRPC 错误原样返回 |
| `assetop/reconcile_test.go` | 假 Store/Applier/时钟:到期行 durable APPLIED → Finalize(Applied) 1 次;过期行走 Abort;未 durable → Reschedule(+500ms);RETRY → 退避区间;UNKNOWN → +60s 且 `testutil.ToFloat64(unknown)==1`;Finalize 出错不影响下一行;同一批 Reschedule 带回领取令牌 |
| `assetop/seq_integration_test.go` | 需 `ASSETOP_TEST_MYSQL_DSN`,未设则 `t.Skip`。测试内自建临时表 `assetop_it_seq`、`assetop_it_op`(测试专用 DDL,结束 DROP):20 协程并发分配并插入 → seq 恰为 1..20;MaxPending=16 时第 17 次 → ErrTooManyPending;只留 seq1 未决、其余终结,next=601 → ErrTooManyPending;未 Ensure → ErrSeqRowMissing |
| `assetop/scene_smoke_test.go` | `//go:build assetop_smoke`,端到端,步骤见第 11 部分 4.40 |
| `assetop/auth_test.go` | 第 8 部分 4.32:CanonicalGolden、NewSignerRejectsWeak、CallerSignsEachAttempt、CallerNoSigner |
| 第二轮追加 | `decide_test`:FinalStatus 部分发放两种来源;`reconcile_test`:`ListDueThenClaimLostSkips`(Claim 回 false 不调 Applier)、`PoisonRowSkipped`(ErrPoisonRow 计 decode、下一行照常)、`WorkersBounded`(假 Applier 阻塞时并发数 ≤ Workers)、`NewLoopRejectsBudgetOverLease`、`LedgerReadFinalizesSeenOffline`、`LedgerReadUnseenStaysPending`、`PartialNotBookedCounterSide`;`seq_integration_test`:`EnsureSeqRowWritesEpoch`、`AllocateSeqCountsOnlyCurrentEpoch`、`WithTxRetryRetriesOnce`(注入分类函数);新增 `classify_test.go`:与 C++ `AssetOpLedgerTest` 同一张用例表跑 `ClassifyPersisted` |

<!-- s4_asset_part6.md -->

# S4 通用资产通道 — 第 6 部分:聚宝斋文档改名、B4b 清单与验证、端到端冒烟、契约偏差

## 4.21 `docs/design/jubaozhai-market.md` 精确修改

该文件正被交易会话修改。动手前先 `git diff docs/design/jubaozhai-market.md` 看清对方未提交的内容,只做下列替换,不改其它段落。行号以 2026-09-15 工作区为准,落码时按原文字串定位。

> **第二轮评审收窄范围**(用户决策 2 只授权"改通用名";聚宝斋 09-14 已定为人民币寄售):本节只替换 RPC 名、结局枚举名、流名,**不删、不改**对方的结果环、资产快照、持久化等级语义。与通用通道 v1 的差异一律写成"待交易会话确认的开放项"追加在 §6.4 末尾,不替对方下结论。下列第 3、4、6、7、8 条已按此改写。

1. **§6.1 第 3 条第 1 小点**(约 :124),原文"两条独立 seq 流:`DEBIT`(托管扣出)与 `CREDIT`(发放)。满足 D1 硬边界"扣减不进同一条 seq 流"。"替换为:
   > 两条独立 seq 流:`ASSET_OP_STREAM_TRADE_DEBIT`(托管扣出)与 `ASSET_OP_STREAM_TRADE_CREDIT`(发放)。枚举 `AssetOpStream` 定义在 `proto/common/asset/asset_op.proto`,与帮会的 `GUILD_DEBIT/GUILD_CREDIT`、预留的 `SYSTEM_CREDIT` 并列;**每个服务独占自己的流,各自分配 seq**。满足 D1 硬边界"扣减不进同一条 seq 流"。
2. **§6.1 第 3 条第 3 小点**(约 :126)句末追加:
   > 窗口语义与 Go 端守卫(每流未决 ≤16、跨度 <512)见 `guild-phase2.md` §S4;seq 分配与重投循环复用 `go/shared/assetop`。
3. **§6.1 第 3 条第 4 小点**(约 :127)"DEBIT 另存最近 8 条结果环(含资产快照)……":**原文不动**,句末追加:
   > (开放项 J-A1:帮会 B4a 落地的通用通道 v1 没有结果环与资产快照,是否在 P2 以追加字段实现,待交易会话确认。)
4. **§6.1 第 4 条**(约 :128)**不改**。durable 语义写进开放项 J-A2(见第 8 条末尾)。
5. **§6.1 第 5 条**(约 :129):`**\`TradeAbortDebit(seq)\`**` → `**\`AssetAbortDebit(seq)\`**`。
6. **§6.2 表"游戏币"行"需要新写的原语"列**(约 :137):**原文不动**,句末追加"(帮会 B4a 已落 txType + correlationId 与 `kAsset*` 错误码)"。
7. **§6.3 第 4 步**(约 :152):只把 `TradeDebit` 换成 `AssetDebit`,括号内原有的 `CHARACTER_LOCK` 保留。
8. **§6.4 标题**改为 `### 6.4 \`SceneNodeGrpc\` 资产 RPC(通用名,帮会 B4a 落地)`,**只把 RPC 定义所在的 proto 代码块**替换为下列内容(代码块之后的三条说明作为新增段落插入,不覆盖原有段落):
   ````markdown
   ```proto
   // 定义:proto/scene_manager/scene_node_service.proto + proto/common/asset/asset_op.proto
   rpc AssetDebit(AssetOpRequest) returns (AssetOpResponse) {}
   rpc AssetAbortDebit(AssetOpRequest) returns (AssetOpResponse) {}
   rpc AssetCredit(AssetOpRequest) returns (AssetOpResponse) {}
   ```
   - 交易使用 `TRADE_DEBIT / TRADE_CREDIT` 流;`tx_type` 受 scene 白名单 `kAssetOpStreamRules` 约束(当前占位 TRADE_DEBIT: `TX_AUCTION_SELL`;TRADE_CREDIT: `TX_AUCTION_BUY`、`TX_TRADE`),交易会话按需只改该表。
   - 结局枚举用 `AssetOpOutcome`(`ASSET_OP_OUTCOME_*`),原 `TradeAssetOpOutcome` 改名为它。
   - 请求须带 `stream_epoch` 与 `auth`(caller=`trade`,密钥 `MMORPG_ASSET_OP_SECRET_TRADE`),见 `guild-phase2.md` §S4。
   ````
   其后原有的请求/响应字段说明(含快照、结果环)**保留原文**,只把其中出现的 `Trade*` RPC 名换成通用名。§6.4 末尾追加"开放项(待交易会话确认)":
   > - J-A1 结果环与资产快照:通用通道 v1 没有;若需要,P2 在 `AssetBundle` 从 3、`AssetOpResponse` 从 5 起追加字段(4 已被 `partial` 占用)。
   > - J-A2 持久化等级:通用通道的 `durable` 只保证写进 zone Redis;Redis 丢数据回退到 MySQL 时可能重复(guild-phase2 §S4 C5)。人民币寄售是否接受,或需要"MySQL 落库确认",由交易会话决定。
   > - J-A3 correlation_id 取值(listing_id / order_id)与按 guid 扣物、宠物、角色锁的字段形状。
9. **全文残留**:`rg -n "TradeDebit|TradeAbortDebit|TradeCredit|TradeAssetOpOutcome|TradeDebitRequest|TradeCreditRequest" docs/design/jubaozhai-market.md`,每处按上表换成通用名(第 7 部分 E5 核实:§6 之外只有 :303 分期行)。消息名 `TradeDebitRequest/TradeCreditRequest` 换成 `AssetOpRequest` 时,其字段说明若与通用消息不一致,不改字段,写入开放项 J-A3。
   - PROGRESS.md 只追加一条"聚宝斋资产 RPC 改通用名"的流水,不改旧条目。

## 4.22 B4b 文件清单(手改 21 个)

> **已作废**:第二轮修订后为 25 个,以第 11 部分 4.41 为准。下方"接线示意"仍有效,另需 `assetop.NewSigner("guild", os.Getenv("MMORPG_ASSET_OP_SECRET_GUILD"))` 并赋给 `Caller.Signer`。

| # | 文件 |
|---|---|
| 1 | `go/shared/go.mod`(go.sum 由 tidy 生成,不计) |
| 2–5 | `go/shared/scenenode/watcher.go`、`conn.go`、`locator.go`、`metrics.go` |
| 6–7 | `go/shared/scenenode/watcher_test.go`、`locator_test.go` |
| 8–13 | `go/shared/assetop/types.go`、`seq.go`、`caller.go`、`decide.go`、`reconcile.go`、`metrics.go` |
| 14–18 | `go/shared/assetop/decide_test.go`、`caller_test.go`、`reconcile_test.go`、`seq_integration_test.go`、`scene_smoke_test.go` |
| 19 | `docs/design/jubaozhai-market.md` |
| 20 | `docs/design/guild-phase2.md`(§S4 落档) |
| 21 | `PROGRESS.md`(追加) |

- 本批**不改** `go/guild`。ServiceContext 接线、`guild_asset_op` 表的 Store 实现、业务终结事务都在 B5。
- 接线示意(给 B5 参考):
  - 用 guild 配置里已有的 Etcd 地址建 `clientv3.Client`;
  - `watcher := scenenode.NewWatcher("scene", scenenode.SceneNodeRpcPrefix, m.SetNodes, func(e scenenode.NodeEntry){ conns.Remove(e.Endpoint) })`,照 match 的方式用 safego 启动 `watcher.Run`;
  - `locator := &scenenode.Locator{Reader: scenenode.GoRedisReader{Client: sc.PlayerLocatorRedisClient}, Watcher: watcher, Conns: conns}`;
  - `caller := &assetop.Caller{Resolver: locator, CallTimeout: 800*time.Millisecond, Requery: assetop.DefaultRequery}`;
  - `loop, _ := assetop.NewLoop(cfg, guildAssetStore, caller, m, time.Now)`。

## 4.23 B4b Codex 验证(工作目录 `E:\work\xuanming-server-mmo\go\shared`)

B4a 的 proto 生成必须先完成,`go/proto/common/asset` 要已存在。

1. `go mod tidy`,确认 go.mod 只新增 `proto` 的 require/replace。
2. `go vet ./scenenode/... ./assetop/...`,零告警。
3. `go test ./scenenode/... ./assetop/... -count=1`,全绿;集成测试因无 DSN 应显示 SKIP。
4. **集成测试**:在本地 dev MySQL 建库 `CREATE DATABASE IF NOT EXISTS assetop_it`,设 `$env:ASSETOP_TEST_MYSQL_DSN = "<本地 dev 账号>@tcp(127.0.0.1:3306)/assetop_it"`(账号不写进任何文件),再跑 `go test ./assetop -run Integration -count=1 -v`,全绿。
5. **下游编译**:对 `go/chat go/client_rpc_router go/data_service go/db go/friend go/guild go/login go/match go/player_locator go/scene_manager go/trade` 逐个 `go build ./...`,确认 shared 新增依赖不破坏它们。trade 如因自身在途改动失败,记录但不归因于本批。

## 4.24 端到端冒烟(B4a + B4b 合跑)

> **已作废**:本节原用 `robot_9219`(帮会冒烟段)在 GUILD_* 流上以 Unix 秒作 seq,会把该账号账本下沿永久推到约 1.79e9,B5 之后 guild 从 seq 1 发起的操作全部 UNKNOWN,且测试抢占了 guild 独占的流。改为专用账号 `robot_9501` + 每次运行新纪元,步骤以第 11 部分 4.40 为准。下文仅留作对照。

1. **起全栈**:按"xuanming 本地开机 runbook"启动;scene 经 `cpp_nodes.ps1` 启动时运行模式兜底 dev,GM 客户端消息因此放行(**不是**靠 `MMORPG_ALLOW_CLIENT_GM`,那个变量不存在;见 `cpp/nodes/gate/SECURITY.md` §3)。启动前 `redis-cli GET player:<id>:location` 应为空。
2. **准备玩家**:robot 账号 `robot_9219`(契约 §6 的帮会冒烟段)登录进场并**保持在线**(Codex 选用能保持在线的 robot 模式,记录实际命令),经 GmAddCurrency 加金币 1000。记下 player_id 与余额 B0。
3. **跑冒烟**:
   ```
   $env:ASSETOP_SMOKE_PLAYER_ID="<id>"
   $env:ASSETOP_SMOKE_REDIS_ADDR="127.0.0.1:6379"
   $env:ASSETOP_SMOKE_ETCD="127.0.0.1:2379"
   go test -tags assetop_smoke ./assetop -run TestSceneAssetOpSmoke -count=1 -v
   ```
   (作废写法)测试内 `base := uint64(time.Now().Unix())`。依次:
   - ① `Debit(GUILD_DEBIT, base, 金币 100, TX_GUILD_DONATE, corr=base)` → 2.5s 内 `Terminal()` 且 APPLIED;
   - ② 同请求再发 → APPLIED,durable;
   - ③ `Abort(GUILD_DEBIT, base+1)` → REJECTED,reason 0,durable;
   - ④ `Debit(base+1)` → REJECTED;
   - ⑤ `Debit(base+2, 金币 1e12)` → REJECTED,reason 27000;
   - ⑥ `Credit(GUILD_CREDIT, base, 金币 100, TX_GUILD_SHOP)` → APPLIED,durable;
   - ⑦ `Debit(GUILD_CREDIT 流)` → UNKNOWN,reason 27004。
4. **核对**:robot 拉背包,金币 == B0(①扣 100、⑥加 100);scene 日志恰有 1 条 `rpc=debit … seq=<base> outcome=1`,其余为只读答复;`transaction_log` 落库后 correlation_id=base 的扣、加各一条(若本地开了流水落库)。
5. **崩溃窗口(可选)**:执行 ① 后立刻强杀 scene,重启、robot 重新进场,再发 ① → APPLIED、余额只扣一次。
6. **GM 闸门**:`cpp_nodes.ps1 -NoClientGm` 重启 scene,robot GmAddCurrency 应收到 tip 1006。

保留:冒烟 `-v` 输出、scene 日志中 `[AssetOp]` 行、各步余额。

## 4.25 契约偏差(需主设计确认)

1. **枚举值名加前缀**:`ASSET_OP_STREAM_*`、`ASSET_OP_OUTCOME_*`。asset_op.proto 无 package,C++ 枚举值在全局作用域,裸 `UNKNOWN / APPLIED` 会冲突。数值与含义不变。
2. **REJECTED 也要 durable 才能终结**(契约只写了 APPLIED)。理由见 C9:未落盘的拒绝可能被崩溃抹掉,之后又被应用。
3. **`AssetAbortDebit` 接受所有流**,包括 CREDIT,供商店超时退帮贡使用;RPC 名不改。
4. **"watermark"是窗口下沿**,不是 D1 原意的"连续已应用水位"。原因:压缩会丢失未决 seq 的结局。
5. **Debit v1 只支持恰好 1 个货币条目**(帮会捐献够用)。多币种原子扣除在 AddCurrency 带补缴语义下无法干净回滚,等真实需求再说。
6. **Go 守卫常量** `MaxPending=16 / MaxSpan=512` 放在 assetop,不进 GuildRule。它和 scene 窗口 1024 是一条正确性证明,不是可调业务数值。
7. **`guild_asset_op` 要比契约 §3.2 多出若干列和索引**:`lease_until_ms`、`lease_token`、`tx_type`、`last_outcome`、`last_reason`、`created_ms`、`updated_ms`;普通索引 `(status,next_attempt_ms)`、`(player_id,stream,status,seq)`。status 数值固定 0 PENDING / 1 APPLIED / 2 REJECTED / 3 ABORTED。交 B1/B5 写进表 proto。**(status 数值部分已作废:见第 7 部分 E1 与第 11 部分偏差 #23;列与索引以第 7 部分 E2 修订版为准)**
8. **`asset_op_deadline_seconds` 语义**应为"到期后改发中止",不是"到期即关闭"。离线玩家的中止拿不到结局,行会一直 PENDING 到玩家上线;帮会侧须持续显示"资产结算中",并靠 I5 限制新发起。**(第二轮:已见结局可经 LedgerReader 离线终结,见偏差 #30)**
9. **B4a 手改正好 30 个**。若 `dev_mprocs_proc.ps1` 与 `currency_crash_window.ps1` 都要改,把 GM 闸门的脚本改动拆成 B4a-2(≤4 个文件)。
10. **`go/shared/go.mod` 新增 `proto` 依赖**,契约未提;已核实下游模块都有 replace。
11. ~~GM 闸门覆盖不全~~ **已关闭**(第二轮):改为分发入口统一拦截全部 18 个客户端 `Gm*`,见第 9 部分 4.34 与偏差 #27。
12. **durable 只到 Redis**(C5)。Redis 丢数据回退到 MySQL 时可能出现重复,与全部玩家数据同级风险;帮会侧文档化接受,跳号上限(4.29)可检出部分回退;聚宝斋是否接受列为开放项 J-A2。
13. ~~回档与帮会账不一致 v1 接受 + 审计~~ **已改为 fail-closed**(第二轮),见偏差 #25。
14. **本批无 scene 余额推送**。客户端在帮会写 RPC 返回后主动拉背包(S5 负责)。
15. **`player_database` 字段号与 B3a 可能撞 15**,谁先落谁拿 15,后落者用 16;验证步骤已含检查命令。
16. **新目录 `proto/common/asset/` 是否被 proto-gen 扫描未能静态确认**。B4a 验证第 1 步会检查;失败的回退方案是移到 `proto/common/component/`,需确认。
17. **scene 流白名单里 TRADE_* 两行只是占位**,由交易会话在 P2 定稿。

<!-- s4_asset_part7.md -->

# S4 通用资产通道 — 第 7 部分:复核勘误(2026-09-16 恢复会话,逐条对照工作区核验)

> 第 1–6 部分完整、主体结论经核验成立。本部分只列**更正与补充**;与前文冲突处以本部分为准。

## 4.26 复核通过的关键事实(无需改动)

- `PlayerLastPersistedSnapshotComp{unique_ptr<PlayerAllData> snapshot; Replace(); HasSnapshot()}`(`last_persisted_snapshot_comp.h`);`HandlePlayerAsyncSaved` 冻结分支也会走到 `snap.Replace`(`player_lifecycle.cpp:257-263,315-319`)。
- `SavePlayerToRedis` 返回 false 只有两种:实体无效(:1207-1211)、与快照相等跳过(:1253-1267)。资产通道只对有效实体调用,false 即"已等于落盘快照"。
- `InitPlayerFromAllData` 在同一次 loop 回调里先 `playerList.emplace`(:1161)再 `PlayerAllDataMessageFieldsUnMarshal`(:1171),RPC 看不到"半加载"实体。
- `PlayerAllData.player_database_data` 内含 `bag_component=13`、`mission_component=14`,顶层 `bag_data` 只是兼容副本(`player_data_loader.h:12-19`),I3 成立。
- Currency/Bag/流水/闸门函数名与行号(`currency_system.cpp:59-223`、`bag_service.cpp:135-204`、`bag_system.cpp` ReserveBatch 满包回 `kBagItemNotStacked`、`player_battle.cpp:412-416`、`IsCrossZoneFrozen` = `any_of<PlayerFrozenComp>`(:1350-1356))全部属实;`kFeatureUnavailable = 1006`。
- match `NodeWatcher` 函数表、`guild` 的 `PlayerLocatorRedisClient`、`go/proto` 不依赖 shared(无环)均属实。

## 4.27 勘误

**E1 outbox status 数值改为跟 S1 一致(冲突,必改)**。S1 已定 `GuildAssetOpStatus`:0 UNSPECIFIED、1 PENDING、2 APPLIED、3 REJECTED、4 ABORTED(`s1_storage_part2.md:32-38`)。第 5 部分 `types.go` 改为:
```go
const (
    StatusPending  Status = 1
    StatusApplied  Status = 2
    StatusRejected Status = 3
    StatusAborted  Status = 4
) // 0 保留为 UNSPECIFIED;trade 的 outbox 表必须用同一编号
```
第 5 部分所有 SQL 里的 `status=0` 一律改为 `status=1`(AllocateSeq 的未决加锁读、ClaimDue、Finalize/Reschedule 的 CAS 条件)。第 6 部分契约偏差 #7 的"0 PENDING / 1 APPLIED / 2 REJECTED / 3 ABORTED"作废。

> **第二轮再修订(取代上面的代码块)**:S5 偏差 #1(`s5_economy_part7.md:95`)又提议 0 基,与 S1 的 1 基冲突。assetop 不再写死数值:`Status` 为纯语义枚举(含新增 `StatusAppliedPartial`),SQL 里的未决状态值取 `SeqTables.PendingStatus`,落库数值由业务 Store 映射(第 10 部分 4.37,偏差 #23)。帮会表最终取值由主设计裁决;"trade 必须用同一编号"作废。

**E2 `guild_asset_op` 列对齐 S1**。S1 的表 message 缺通道所需的租约列和索引。在 `GuildAssetOpRecord` 追加区(S1 注明从 21 起)加:
```proto
  uint64 lease_until_ms = 21;   // 行租约到期;0 = 未领取
  uint64 lease_token = 22;      // 领取令牌(随机 uint64),Reschedule 以此做 CAS
  uint32 tx_type = 23;          // TransactionType 数值,重投时原样带上
  uint32 last_outcome = 24;     // 最近一次 AssetOpOutcome 数值,仅诊断
```
并把 `option(OptionIndex)` 改为 `"status,next_attempt_ms;guild_id,op_id;player_id,stream,status,seq"`。第三个索引不能省:没有它,`AllocateSeq` 的 `... AND status=1 ORDER BY seq LIMIT 17 FOR UPDATE` 会顺着唯一键扫过并锁住该玩家该流的全部历史行。

> **第二轮再修订(取代上面的代码块与索引串)**:与 S5 已定的 21–25 对齐(`s5_economy_part2.md:152-156`:lease_until_ms / lease_token / tx_type / last_outcome / **last_reason = 25**),再追加:
> ```proto
>   uint64 stream_epoch = 26;     // 流纪元,来自 guild_player_op_seq.epoch(第 8 部分 4.29)
>   string resolved_by = 27;      // 人工终结操作人,<=64(第 10 部分 4.38)
>   string resolve_reason = 28;   // 人工终结理由,<=191
> ```
> `OptionUniqueKey` 改为 `"player_id,stream,stream_epoch,seq"`;`OptionIndex` 改为 `"status,next_attempt_ms;guild_id,op_id;player_id,stream,stream_epoch,status,seq"`。`guild_player_op_seq` 追加 `uint64 epoch = 5;`。`last_reason` 保留(Reschedule 写暂时原因,并承担部分发放的粘性标记),`reason_tip_id` 只在终结时写。
- ~~第 5 部分 Store 注释里的 `last_reason` 列名改用 S1 的 `reason_tip_id`。~~(第二轮作废,见上)
- S1 第 142 行 `durable` 注释应为"(APPLIED 或 REJECTED)且 durable 才为 true"(I4)。
- 放 B1 还是 B5 由主设计定(见 #18)。推荐 B1 一次建全,零迁移成本。

**E3 启动脚本(关闭原偏差 #9)**。
- `tools/scripts/dev_mprocs_proc.ps1:152` 直接拉起 scene:`"cpp-scene" { Invoke-CppNode -Exe "scene.exe" }`。`Invoke-CppNode` 用 `& $exePath` 前台运行后 `exit`(:94-114),不需要恢复旧值。精确改为:
  `"cpp-scene" { Invoke-CppNode -Exe "scene.exe" }` —— **GM 开关那半句作废**:运行模式已由启动器兜底 dev,
  这个分支只需注入两把资产通道开发密钥(`MMORPG_ASSET_OP_SECRET_GUILD` / `_TRADE`)。
- `tools/scripts/currency_crash_window.ps1:213-214` 经 `cpp_nodes.ps1 -Command start -Nodes scene` 启动,自动继承默认开启,**不改**。
- 其它出现 `scene.exe` 的脚本:`stress_round19.ps1` 只有注释;`k8s_image.ps1` 只打包,默认拒绝。B4a 清单第 30 项确定为 `dev_mprocs_proc.ps1`,总数仍为 30。
- `cpp_nodes.ps1` 的写法:**GM 开关部分作废**(闸门判据已是 `SCENE_RUN_MODE`,启动器兜底 dev,不需要额外开关)。仍要做的是**注入两把资产通道开发密钥**:在逐进程环境覆盖块(`$prevZoneEnv` 那一段)里,仅对 scene 进程、且当前未设置时,给 `MMORPG_ASSET_OP_SECRET_GUILD` / `MMORPG_ASSET_OP_SECRET_TRADE` 设开发默认值;finally 按 `$null -eq $prev` 分支恢复(直接赋空串会把变量留成空值,和"未设置"不是一回事)。值必须与 `go/shared/assetop/scene_smoke_test.go` 里的 smoke 开发密钥同值,否则本机冒烟全判 `kAssetAuthFailed`。
- **第二轮追加**:同一 scene 分支里,`MMORPG_ASSET_OP_SECRET_GUILD`、`MMORPG_ASSET_OP_SECRET_TRADE` 若调用前未设,则设为开发值 `change-me-dev-asset-op-guild-secret-000000` / `change-me-dev-asset-op-trade-secret-000000`,同样保存旧值、finally 恢复(第 8 部分 4.32)。`dev_mprocs_proc.ps1` 的 `"cpp-scene"` 分支同理注入,该文件改动移入 B4a-2;B4a-1 第 30 项只剩 `cpp_nodes.ps1`。

> **2026-09-19 作废这条「固定开发值」约定**:本机两把密钥改为由 `tools/scripts/lib/assetop_dev_secret.ps1` **随机生成一次**、落在 `run/secrets/assetop-dev.env`(在 .gitignore 里),再由 `cpp_nodes.ps1`(仅 scene 分支)与 `start_game.ps1` 注入。
> 理由:写死在仓库里的开发密钥一定会被人抄进预发环境 —— 它看起来就是「配置」。随机值抄不走。
> 连带:`go/shared/assetop/scene_smoke_test.go` **去掉了写死的兜底值**,只从环境变量读,读不到就 `t.Skip`。
> 兜底值留着比没有更坏:生成值改随机之后它与 scene 实际拿到的值必然不同,于是每一次资产 RPC 都回 27008,而密钥值不许进日志,排障看到的只是「全红且毫无线索」。


**E4 proto 新目录(原偏差 #16 降级)**。`proto_gen.yaml` 的 `proto_directories` 没列 `common/rollback/`、`common/options/`,但二者都已生成(`cpp/generated/proto/CMakeLists.txt:92-94`),说明生成器按目录递归遍历(`prototools/descriptor.go:87` WalkDir)。`proto/common/asset/` 预期会被扫到,B4a 验证第 1 步保留作确认,回退方案不变。

**E5 聚宝斋文档定位更新**。§6.4 现位于 :163;§6.1 各条行号与第 6 部分一致。§6 之外的旧名**只有** :303 的 P2 分期行(第 6 部分第 9 条说的"J-5 行、§2 状态表、§12"经 grep 不存在)。该行精确替换:
- "C++ 资产原语:账本组件 + 持久化;\`TradeDebit/AbortDebit/Credit\`;" → "C++ 资产原语:通用通道(账本、`AssetDebit/AssetAbortDebit/AssetCredit`、Currency txType)已由帮会 B4a 落地;P2 追加按 guid 扣物、宠物、角色字段;"
- 同一行删去后面重复出现的"Currency txType;"。测试列不动。

**E6 robot 也 vendor 了 shared**(`robot/vendor/modules.txt:191`)。B4b 验证第 5 步追加 `cd robot; go build ./...`。robot 不导入 scenenode/assetop,正常不需要重新 vendor;若报 inconsistent vendoring,停下报告,不要擅自 `go mod vendor`,因为 vendor 目录与并行会话共用。

**E7 Process 取账本**。分类用 `try_get<PlayerAssetOpLedgerComp>`(nullptr 视作空账本);只在 Record 前 `get_or_emplace`。第 7 步"缺 CurrencyComp → RETRY"仅作防御(见 4.26 第 3 条)。

**E8 新增崩溃窗口 C12**:`HandlePlayerAsyncSaved` 遇到"过期的 UnregisterPlayer + 活会话"时会在 :298 提前 `return`,跳过 `snap.Replace`。后果:本次已落盘,但 durable 仍算 false。Go 重查时玩家已去掉该标签、处于可改状态,`RequestPersist` 与旧快照比较不相等,于是再写一次,回调后得到 durable。只多一次写,正确性不受影响。本批不改该函数。

**E9 GM 闸门细节**。`player_currency_handler.cpp` 顶部守护段加 `#include "services/scene/player/system/client_gm_gate.h"`(`common_error_tip.pb.h` 已包含)。不复用 `gate_security::VerifyGmRequestFromEnv`(`gate_security.h:411`):它校验管理 RPC 的 HMAC 签名、operator 和 nonce,客户端 GM 消息没有这些字段。

**E10 gtest 命名**。`asset_op_ledger_test.cpp` 用 `TEST(AssetOpLedgerTest, …)`;`asset_op_system_test.cpp` 用 `TEST_F(AssetOpSystemTest, …)`;currency 新增用例沿用 `TEST(CurrencyTest, …)`。过滤器定为 `--gtest_filter=AssetOp*:CurrencyTest*`。currency_test 只有 vcxproj,没有 CMake,所以 Linux 那步不编测试。

**E11 流水签名**。`LogCurrencyAdd/Deduct` 已有 `TransactionType txType` 默认参数(`transaction_log_system.h:33-47`),只需末尾加 `correlationId`;`LogItemCreate`(:69-74)的 txType 无默认值,末尾加 `uint64_t correlationId = 0`。`currency_system.cpp:158,223` 目前没传 tx,改造后传 `txType, correlationId`。

**E12 生成签名**。`scene_node_service.proto` 有 `package scene_node;`,但请求/响应定义在无 package 的 asset_op.proto,所以 C++ 守护段签名是 `::AssetOpRequest / ::AssetOpResponse`(同 `::PrepareBattleRequest`)。Go 侧 `smpb "proto/scene_manager"` 的客户端方法参数是 `*assetpb.AssetOpRequest`(`assetpb "proto/common/asset"`)。

**E13 并行未提交改动(落码前必须 diff)**。2026-09-16 `git status` 显示下列文件已被其它会话修改但未提交:`player_database_loader.cpp`、`scene_node_service.cpp`(CreateScene 投递前守护段恢复了 Agones 许可块)、`scene.vcxproj`、`cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`、`data/tip/Tip.xlsx`、`MessageLimiter.xlsx`、`jubaozhai-market.md`、`PROGRESS.md`。B4a 验证第 1 步追加:生成后 `Select-String cpp/nodes/scene/handler/grpc/scene_node_service.cpp -Pattern AcquireCreatePermitBlocking` 必须命中,否则说明重生成吞掉了他人的守护段,立即停下。

## 4.28 契约偏差更新

- **#7 修订**:status 编号不由 S4 决定(第二轮,偏差 #23);新增列与索引见 E2 的第二轮修订版。
- **#9 关闭**:B4a 正好 30 个文件,不用拆 B4a-2。**(第二轮重开并改为拆分:B4a-1 / B4a-2 / B4a-client,见第 11 部分 4.41)**
- **#16 降级**:只作验证确认项(E4)。
- **#18 新增**:S1 与 S4 在 `guild_asset_op` 列上不一致(E2),需主设计定"B1 一次建全"还是"B5 追加"。本节推荐 B1。
- **#19 新增**:C12(E8)。接受,不改生命周期代码。

<!-- s4_asset_part8.md -->

# S4 通用资产通道 — 第 8 部分:第二轮评审修订(一)流纪元、跳号上限、资产 RPC 鉴权

> 2026-09-16 第二轮对抗评审后的修订。与前文冲突时优先级:第 11 > 10 > 9 > 8 > 7 > 1–6 部分。第 1–7 部分里能就地改的已就地改,并指向这里。

## 4.29 流纪元 `stream_epoch`(阻断问题的修法)

**问题**:seq 与"是哪一笔操作"没有绑定。帮会库删库重建、测试 schema 助手、TiDB BR 按库恢复(`s1_storage_part5.md` §8 已把 `mmorpg_guild` 列入按库恢复清单)都会让 `guild_player_op_seq.next_seq` 回到小号,而玩家 blob 里的账本不跟着回退。新操作拿到旧 seq,scene 回旧结局:捐献不扣钱帮会照样入资金(复制),或商店扣了帮贡不发物(丢失)。第 2 部分 4.4 的证明默认"max_seq ≤ next_seq−1",库重置后不成立。

**类比**:银行换了一本新流水簿,旧簿上的"第 7 号"和新簿上的"第 7 号"不是一笔业务。纪元就是"簿子编号"。

**规则**:
- 纪元由独占该流的服务在**建 seq 行时**写入 `epoch = 当前毫秒`(>0),之后不变。删库重建时 EnsureSeqRow 重新建行,自然得到更大的纪元;按库恢复后由 4.31 手册显式抬高。
- 请求带 `stream_epoch`;账本每条流存 `stream_epoch`(proto 见第 1 部分 4.3.1/4.3.2 已改)。

**scene 分类**(`ClassifyAssetOpSeq(ledger, epoch, seq)`,顺序固定;ledger 为空时 E_L=0):

| 条件 | 状态 | 响应 | 记账 |
|---|---|---|---|
| seq==0 或 epoch==0 | kInvalid | UNKNOWN + kAssetInvalidBundle | 否 |
| epoch < E_L | kStaleEpoch | UNKNOWN,reason 0,ERROR 日志 | 否 |
| epoch > E_L | 按空账本分类:seq ≤ 1024 → kUnseen;否则 kJumpTooFar | 过全部闸门后,Record 先重置该流再记账 | 是/否 |
| epoch == E_L | 原算法(4.4),另加跳号上限:`seq > max_seq + 1024` → kJumpTooFar | kJumpTooFar → UNKNOWN,reason 0,ERROR 日志 | 否 |

**重置** `ResetAssetOpStreamForEpoch(ledger, epoch)`:watermark=0、两组位各 16 个 0、`rejections` 与 `partial_seqs` 清空、max_seq=0、stream_epoch=epoch。**只在 Record 内部**、确定要记账时调用;闸门回 RETRY 时不重置,已见结局保持不变(I2)。

**跳号上限的证明**(同一纪元内):设新分配的 seq 为 n。
- 无未决行:n−1 已终结;终结要求 scene 答复过,于是 n−1 ≤ max_seq,n ≤ max_seq+1。
- 有未决行:I5 保证 `n − min_pending < 512`;min_pending−1 已终结(或 min_pending=1),所以 max_seq ≥ min_pending−1 > n−513,即 n < max_seq+513 ≤ max_seq+1024。
- 例外:人工终结(第 10 部分 4.38)一个 scene 从未见过的 seq,会让差距变大;累计超过 511 时新 seq 被判 kJumpTooFar。这是 fail-closed,按 4.31 抬纪元。Redis 回退到旧 blob(C5)使 max_seq 变小时,也会命中此上限并告警,这正是想要的。

**Go 侧改动**(表 message 属 S1/S5,列为契约偏差 #20):
- `guild_player_op_seq` 追加 `uint64 epoch = 5;`(S1 追加区从 5 起)。
- `guild_asset_op` 追加 `uint64 stream_epoch = 26;`(S5 已占 21–25)。UNIQUE 改为 `player_id,stream,stream_epoch,seq`;第 3 个普通索引改为 `player_id,stream,stream_epoch,status,seq`。不改 UNIQUE 的话,纪元抬高、next_seq 回 1 之后,新行会与恢复出来的旧行撞唯一键。
- I5 按纪元计:只数**本纪元**的未决行。旧纪元的未决行不挡新操作,它们会拿到真实旧结局(scene 账本仍在旧纪元时)或 UNKNOWN(新纪元已到),后者走人工处置。
- SQL 见第 10 部分 4.37。

## 4.30 纪元与跳号的测试

C++ 纯函数(`asset_op_ledger_test.cpp`,`TEST(AssetOpLedgerTest, …)`):
| 用例 | 断言 |
|---|---|
| `EpochZeroInvalid` | epoch=0 → kInvalid |
| `EpochHigherResets` | 纪元 100 下记 1..3;以纪元 200 Record seq 1 → watermark 0、max_seq 1、stream_epoch 200,seq 2 → kUnseen |
| `EpochHigherFarSeqJump` | 纪元 100 已有记录;纪元 200、seq 1025 → kJumpTooFar,账本不变 |
| `EpochStale` | 纪元 200 的账本,纪元 100 查 seq 1 → kStaleEpoch |
| `JumpCapSameEpoch` | 纪元 100、只记过 seq 10(watermark 0):seq 1034 → kAheadOfWindow;seq 1035 → kJumpTooFar |
| `ValidateEpochZero` | 已存在的流 stream_epoch=0 → Validate 返回非空 |

ECS 级(`asset_op_system_test.cpp`,`TEST_F(AssetOpSystemTest, …)`):
- `EpochResetAppliesFresh`:纪元 100 Debit seq1 扣 30 → APPLIED;纪元 200 Debit seq1 扣 30 → APPLIED,余额共扣 60(两笔不同操作)。
- `StaleEpochUnknownNoRecord`:接上例,纪元 100 查 seq1 → UNKNOWN,账本仍为纪元 200。
- `RetryDoesNotResetEpoch`:纪元 100 记 seq1;挂 PlayerFrozenComp;纪元 200 Debit → RETRY kAssetFrozen;账本仍为纪元 100,纪元 100 查 seq1 → APPLIED。
- `JumpTooFarUnknown`:纪元 100 记 seq1;seq 2000 → UNKNOWN,余额不变。

Go(`seq_integration_test.go`):`EnsureSeqRowWritesEpoch`(epoch>0 且二次 Ensure 不改);`AllocateSeqCountsOnlyCurrentEpoch`(旧纪元 16 行未决时新纪元仍可分配)。

## 4.31 运维手册条目:帮会库恢复 / 重建(随 B5 落 `docs/design/guild-phase2.md`)

1. 停全部 guild 副本。
2. 执行恢复(BR 或逻辑导入)。
3. 抬纪元:`UPDATE guild_player_op_seq SET epoch = <当前毫秒>, next_seq = 1, updated_ms = <当前毫秒>;`。当前毫秒必然大于建行时写的旧纪元;不要用恢复点时间。
4. 导出 `SELECT op_id, player_id, stream, stream_epoch, seq, kind, created_ms FROM guild_asset_op WHERE status = <PENDING>` 存档。这些行照常重投:scene 账本还在旧纪元时回真实结局,正常终结(恢复丢了帮会侧入账,再终结一次正好补回);新纪元操作先到的,旧行回 UNKNOWN,转人工(4.38)。
5. 对账:scene 流水里 `tx_type ∈ {24,25,26}`、`correlation_id` 在恢复后的 `guild_asset_op` 里找不到的记录,是恢复点之后丢失的操作,按流水人工补偿。
6. 删库重建(开发期)只需第 1、2 步;EnsureSeqRow 自动得到新纪元。

## 4.32 资产 RPC 鉴权

**已核实的现状**:
- scene gRPC 用明文不安全凭据:`node.cpp:623` `AddListeningPort(serverAddress, grpc::InsecureServerCredentials())`,集群内任何进程都能连。
- 生成器把包装函数的 context 参数注释掉了:`grpc_handler_gen.go:125` `grpc::ServerContext* /*context*/`。守护段里拿不到 metadata,除非改生成器并重生成全部 gRPC 包装。
- 先例:login 的 `callerauth`(`go/login/internal/logic/pkg/callerauth/callerauth.go:155` Canonical、:174 SignCanonical);C++ `token_security::HmacSha256Hex` / `ConstantTimeEquals`(`cpp/libs/engine/core/security/token_security.h:46,67`),引入写法照 `gate_security.h:20` 的 `#include "security/token_security.h"`。

**决定**:签名放进**请求消息体**(`AssetOpRequest.auth`),不改生成器。scene 在 loop 线程里校验,纯函数可单测。

**canonical 串**(LF 分隔,末尾无换行,全部十进制):
```
mmorpg-asset-op/v1
<caller>
<rpc>                 // debit | abort_debit | credit
<player_id>
<stream 数值>
<stream_epoch>
<seq>
<correlation_id>
<tx_type>
<bundle>              // "c=" 货币按请求顺序 "<type>:<amount>" 逗号分隔 ";i=" 物品 "<config_id>:<count>" 逗号分隔
                      // ";u=" item_uuids 按请求顺序逗号分隔 ";p=" pet_id(单值);全空则 "c=;i=;u=;p=0"
<timestamp_ms>
```
`signature_hex = lowercase-hex(HMAC-SHA256(secret_of(caller), canonical))`。rpc 进串,防止拿 Abort 的签名去调 Credit;bundle 进串,防止改金额。

> **`;u=` / `;p=` 两段是 2026-09-19 补的,不是可选装饰。** 它们对应 `AssetBundle.item_uuids` / `pet_id`,
> 会真改玩家资产(按 guid 扣装备 / 扣宝宝)。此前 canonical 不覆盖它们,而 scene 的 gRPC 是
> `InsecureServerCredentials()`(`node.cpp:623`),集群内任意进程可连可嗅:攻击者截下一条合法的
> TRADE_DEBIT、只把 `item_uuids` 换成该玩家的其它装备再发出去,这个 seq scene 没见过、签名照样通过,
> 扣掉的就是被换的那件。**seq 幂等与 300s 时间窗都挡不住这种「同 seq 抢跑改载荷」**。
> 守卫用例:C++ `AssetOpAuthTest.SignatureCoversGuidAndPetTamper` 与 Go `TestSignatureCoversGuidAndPetTamper`。
>
> 连带后果:`asset_op_system.cpp` 原先把「带这两个字段」判在信封档(回 UNKNOWN 且**不记账**),
> 理由正是「字段不受签名保护,记账就等于谁都能永久杀掉任意一条在途 seq」。签名覆盖之后这条理由消失,
> 已改回 §4.9 第 8 步的记账式 REJECTED —— v1 仍不支持按 guid 扣物(那是 P3 的活),但拒绝方式变了。

**为什么不要 nonce**:同一 (player, stream, epoch, seq) 重放,要么只读答复,要么就是那唯一一次应用,Go 总以 scene 结局为准。时间窗只用于限制截获包的寿命(应对回档后旧 seq 重新变成"未见"):`|now − timestamp_ms| ≤ 300000`。

**调用方白名单与密钥**(每个调用方一把,互不共用):
| stream | 允许的 caller | scene 读取的环境变量 |
|---|---|---|
| GUILD_DEBIT / GUILD_CREDIT | `guild` | `MMORPG_ASSET_OP_SECRET_GUILD` |
| TRADE_DEBIT / TRADE_CREDIT | `trade` | `MMORPG_ASSET_OP_SECRET_TRADE` |
| SYSTEM_CREDIT | 无(v1 一律拒) | — |

密钥去首尾空白后不足 32 字节视同未配;未配 → 该调用方全部拒绝,首次 ERROR 日志(不打印密钥)。**不做** `ClassifyTokenSecret` 的 dev 放行:资产路径一律 fail-closed,本地由脚本显式注入开发值。

**C++ 新文件** `cpp/libs/services/scene/player/system/asset_op_auth.{h,cpp}`:
```cpp
enum class AssetOpAuthVerdict : uint8_t { kOk, kCallerNotAllowed, kSecretMissing, kClockSkew, kSignatureMismatch, kCount };
using AssetOpSecretLookup = std::string (*)(std::string_view caller);  // 默认读环境变量并缓存
constexpr int64_t kAssetOpAuthMaxSkewMs = 300000;
std::string AssetOpCanonical(std::string_view rpc, const AssetOpRequest& req);
AssetOpAuthVerdict VerifyAssetOpAuth(std::string_view rpc, const AssetOpRequest& req, int64_t nowMs, AssetOpSecretLookup lookup);
const char* AssetOpAuthVerdictName(AssetOpAuthVerdict v);   // 平铺数组 + static_assert(kCount)
```
`PlayerAssetOpSystem` 增 `SetSecretLookupForTest(AssetOpSecretLookup)`。Process 第 1 步信封校验之后、找人之前调用;非 kOk → `UNKNOWN + kAssetAuthFailed`,WARN 日志带 verdict 名、caller、stream、seq,不记账。

**Go 新文件** `go/shared/assetop/auth.go`:
```go
type Signer struct{ caller string; secret []byte }
func NewSigner(caller, secret string) (*Signer, error)   // TrimSpace 后 <32 字节 → ErrWeakSecret
func Canonical(rpc RPC, req *assetpb.AssetOpRequest, tsMs uint64) []byte
func (s *Signer) Sign(rpc RPC, req *assetpb.AssetOpRequest, nowMs uint64) // 就地写 req.Auth
```
`Caller` 增字段 `Signer *Signer`,为 nil 时 `Do` 直接返回 `ErrNoSigner`;每次调用(含重查)都以**当前时间**重签克隆出的请求。

**本地脚本**:`cpp_nodes.ps1` 对 scene 进程,在变量未设时注入开发值 `change-me-dev-asset-op-guild-secret-000000` 与 `change-me-dev-asset-op-trade-secret-000000`(与 `k8s_deploy.ps1:281` 的 DevFallback 同一约定);`dev_mprocs_proc.ps1` 同(B4a-2)。guild 读取与 `go_services.ps1` 注入属于 B5。K8s:`k8s_deploy.ps1` 以 `Resolve-InjectedSecret -MinLength 32` 注入,上线项。

> **2026-09-19 作废这条「固定开发值」约定**:本机两把密钥改为由 `tools/scripts/lib/assetop_dev_secret.ps1` **随机生成一次**、落在 `run/secrets/assetop-dev.env`(在 .gitignore 里),再由 `cpp_nodes.ps1`(仅 scene 分支)与 `start_game.ps1` 注入。
> 理由:写死在仓库里的开发密钥一定会被人抄进预发环境 —— 它看起来就是「配置」。随机值抄不走。
> 连带:`go/shared/assetop/scene_smoke_test.go` **去掉了写死的兜底值**,只从环境变量读,读不到就 `t.Skip`。
> 兜底值留着比没有更坏:生成值改随机之后它与 scene 实际拿到的值必然不同,于是每一次资产 RPC 都回 27008,而密钥值不许进日志,排障看到的只是「全红且毫无线索」。


**网络面**:仓库内没有任何 `kind: NetworkPolicy` 清单(已 grep)。上线项:scene gRPC 端口只放行 scene_manager、match、guild、trade。签名是主防线,NetworkPolicy 是纵深防御。

**测试**:C++ `asset_op_auth_test.cpp`(`TEST(AssetOpAuthTest, …)`):`CanonicalGolden`、`VerifyOk`、`CallerNotAllowed`、`SecretMissing`、`SecretTooShort`、`ClockSkew`、`TamperedBundle`、`TamperedRpc`、`SystemCreditAlwaysRejected`。Go `auth_test.go`:`TestCanonicalGolden`、`TestNewSignerRejectsWeak`、`TestCallerSignsEachAttempt`、`TestCallerNoSigner`。

两边的 golden 用同一输入:player 42、stream 1、epoch 1700000000000、seq 7、corr 99、tx 24、货币 [(1,30)]、无物品、rpc `debit`、caller `guild`、ts 1700000000123。期望 canonical 字面量为:
`"mmorpg-asset-op/v1\nguild\ndebit\n42\n1\n1700000000000\n7\n99\n24\nc=1:30;i=;u=;p=0\n1700000000123"`

<!-- s4_asset_part9.md -->

# S4 通用资产通道 — 第 9 部分:第二轮评审修订(二)部分发放、GM 统一闸门、单写者风险与 B4c 存盘围栏

## 4.33 Credit 部分发放不得记成成功(替换第 3 部分 4.9 第 10 步)

**问题**:原稿物品只发一半、或预检后 AddCurrency 仍失败时"LOG_ERROR 后继续、记 APPLIED",Go 照常终结,玩家永久少拿且无补偿线索。违反 AGENTS §11.3"错误不得伪装成功;玩家资产默认 fail-closed"。

**新第 10 步**(Credit 应用,顺序固定;`anyMutated` 初值 false):
- (a) **货币预检**:任一 `GainBlockService::IsGainBlocked(kCurrency, type)` 或 `CurrencySystem::IsCurrencyBlocked(player, type)` → `Record(kRejected, kAssetBlocked)` → REJECTED。
- (b) **物品 guid 余量预检**(有物品时):`tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available()`(`guid_segment_client.h:158`;取法同 `item_store.cpp:375-377`)< Σcount → `RETRY`,reason 0,WARN,不记账。Σcount 是新实例数的上界(每堆至少 1 个),只会偏严。
- (c) **物品**:`AddItems(..., &mutated)`。
  - `kSuccess` → `anyMutated = true`,继续;
  - `!mutated`:同原稿(背包满 → RETRY kAssetBagFull;kInvalidParameter → REJECTED kAssetBlocked;其它 → RETRY reason=err),均不改资产;
  - `mutated && err != kSuccess` → 记下失败信息,`anyMutated = true`,**跳到 (e) 记部分发放**,不再发货币。
- (d) **货币**:逐条 `AddCurrencyFn(player, type, amount, txType, correlationId)`(默认 `&CurrencySystem::AddCurrency`,测试可换)。失败时:`anyMutated == false` → `RETRY`、reason=err、不记账;否则记下失败信息,跳到 (e)。成功一条就置 `anyMutated = true`。
- (e) 全部成功 → `Record(kApplied)` → RequestPersist → APPLIED。有失败且 `anyMutated` → `Record(kAppliedPartial)` → RequestPersist → `APPLIED + partial=true + reason=kAssetPartialApplied`,并打 ERROR:`[AssetOp] partial player_id=… stream=… epoch=… seq=… corr=… failed=<item config_id 或 currency type> err=…`(补偿依据)。

**账本**:`RecordAssetOpOutcome` 的第三参数改为 `enum class AssetOpRecordKind : uint8_t { kApplied, kAppliedPartial, kRejected, kCount }`。kAppliedPartial 置 seen+applied 位,并把 seq 升序插入 `partial_seqs`(≤64,超出删最小)。重查时 kApplied 且 seq 在 `partial_seqs` → `partial=true, reason=kAssetPartialApplied`。Validate:`partial_seqs` 升序、在窗口内、对应位为 applied。

**Go**:`Result.Partial bool`;`Reschedule` 把 `last_reason` 写成 `res.Reason`,于是"曾见 partial"是粘性的(即使 64 条环被挤掉)。`FinalStatus`:APPLIED 且 `(res.Partial || op.LastReason == PartialReason)` → `StatusAppliedPartial`。Store 对它**不做对侧入账、不退款**,只终结并计 `assetop_partial_total{service,stream}`,转人工补偿清单。`PartialReason = uint32(table.AssetError_kAssetPartialApplied)`(命名以导表产物为准)。表状态枚举在 ABORTED 之后追加 APPLIED_PARTIAL(偏差 #21)。

**测试**:
- C++ `CreditPartialFlagged`:`SetAddCurrencyFnForTest` 让第 2 条货币返回 kAssetBlocked;Credit 两条货币 → APPLIED、partial=true、reason=kAssetPartialApplied;第 1 条已到账;同 seq 重查仍 partial;账本 `partial_seqs` 含该 seq。
- C++ `CreditFirstCurrencyFailsRetry`:只有 1 条货币且失败 → RETRY,账本无流。
- C++ `LedgerPartialSeqsCapAndValidate`:70 次 partial 只留 64;partial seq 对应位非 applied → Validate 非空。
- Go `decide_test`:FinalStatus 对 partial 与 `LastReason==PartialReason` 均得 AppliedPartial;`reconcile_test` `PartialNotBookedCounterSide`。

## 4.34 客户端 GM 指令统一闸门(替换第 3 部分 4.12 的逐 handler 闸门)

> **2026-09-18 实际落码形状与本节不同,以代码为准**(P0-a 一并做掉了)。**不存在** `client_gm_gate.h`,也**没有** `MMORPG_ALLOW_CLIENT_GM` 这个环境变量。实际是两道锁:
> gate 按**消息号**闸(`cpp/nodes/gate/gate_gm_client_messages.h` 的 `kGmClientMessageIds` 清单 + `gate_security.h::ClassifyGmClientMessage`,接在 `client_message_processor.cpp:925`),
> scene 再拦一次防绕开 gate 直连(`cpp/nodes/scene/handler/rpc/player/player_gm_guard.h`)。
> 判据是运行模式 `GATE_RUN_MODE` / `SCENE_RUN_MODE`,**未设置 = prod = 拒绝**,部署链从不注入这两个变量;
> 本机启动器兜底 dev,robot 冒烟不受影响。细节见 `cpp/nodes/gate/SECURITY.md` §3。
> 下面的原文保留作设计留痕,**不要照它落码**——照落会多出一个与现有闸门互不知情的第二开关。

> 本节仍然有效的只有**清点结论**(客户端可调的 `Gm*` 共 18 个、客户端消息只有一个入口)与"将来若有路径转发客户端消息必须调用同一函数"这条约束。

**已核实**:scene 客户端可调的 `Gm*` 共 18 个——`GmAddCurrency`、`GmDeductCurrency`、`GmBlockCurrency`、`GmUnblockCurrency`(`player_currency_handler.cpp:14,34,64,93`)、`GmGrantPet`(`player_pet_handler.cpp:145`)、`GmSetPlayerLevel`(`player_attribute_handler.cpp:147`)、`player_rollback_handler.cpp:40-167` 的 12 个(GmAttachDebt/GmWaiveDebt/GmAdjustDebt/GmFreezeDebt/GmQueryDebt/GmCreateSnapshot/GmListSnapshots/GmPreviewRollback/GmExecuteRollback/GmQueryTransactionLog/GmTraceItem/GmClawbackItem,均挂在 SceneRollbackClientPlayerHandler 下,多数仍是 TODO 桩)。被 GM 封币的玩家可以自己调 `GmUnblockCurrency` 解封;回档类桩一旦实现就是客户端自助回档。客户端消息只有一个入口:gate 发 `SceneProcessClientPlayerMessageMessageId`(`client_message_processor.cpp:699-711`)→ `SceneHandler::ProcessClientPlayerMessage`(`scene_handler.cpp:307`),在 :418 `CallMethod`。`InvokePlayerService`(:441)与 `SendMessageToPlayer`(:202)是节点路由,不对客户端开放,不加闸;将来若有路径转发客户端消息,必须调用同一函数。

**`client_gm_gate.h`**(header-only)在原两个函数外增加:
```cpp
// 方法名形如 Gm + 大写字母开头
inline bool IsClientGmMethodName(std::string_view name) {
    return name.size() >= 3 && name[0] == 'G' && name[1] == 'm' && name[2] >= 'A' && name[2] <= 'Z';
}
```

**`scene_handler.cpp` 守护段**:顶部 include `services/scene/player/system/client_gm_gate.h` 与 `table/proto/tip/common_error_tip.pb.h`(已包含则不重复)。:416–418 改为:
```cpp
	// Dispatch to the concrete player service method
	const MessageUniquePtr playerResponse(service->GetResponsePrototype(method).New());
	if (IsClientGmMethodName(method->name()) && !IsClientGmAllowed())
	{
		// 客户端 GM 指令统一闸门(guild-phase2 §S4 4.34):逐 handler 加闸必漏。
		LOG_WARN << "ProcessClientPlayerMessage: client GM rejected method=" << method->name()
				 << " player_id=" << it->second;
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(kFeatureUnavailable);
	}
	else
	{
		serviceIt->second->CallMethod(method, player, playerRequest.get(), playerResponse.get());
	}
```
报错通道与现有 handler 写全局 TipInfoMessage 的方式完全一致(如 `player_currency_handler.cpp:26`);其后的同步应答序列化块不变。`player_currency_handler.cpp` 不再加闸门代码,只把 GM 流水 tx 改为 `TX_GM_GRANT / TX_GM_DEDUCT`。

**测试** `cpp/tests/currency_test/client_gm_gate_test.cpp`(`TEST(ClientGmGateTest, …)`):
- `GateValue`:`nullptr`/`"0"`/`"true"` → false;`"1"` → true。
- `MethodNamePrefix`:`GmAddCurrency` true;`Gm`、`Gmx`、`GetGmList`、`gmAdd`、`GMAdd` false。
- `AllKnownClientGmMethodsMatch`:上面 18 个方法名字面量逐个断言 true。
- `DescriptorScan`:对 `SceneCurrencyClientPlayer`、`ScenePetClientPlayer`、`SceneAttributeClientPlayer`、`SceneRollbackClientPlayer` 四个服务的 `descriptor()`(类名落码时 `rg -n "class Scene(Currency|Pet|Attribute|Rollback)ClientPlayer\b" cpp/generated/proto` 核对),遍历方法:名字不区分大小写以 `gm` 开头的,必须满足 `IsClientGmMethodName`。防止有人写出 `GMGrant` 绕过前缀。
- attribute_smoke、pet_smoke 经 `cpp_nodes.ps1` 启动,默认开启,不受影响。

## 4.35 单写者(I1)的真实边界:已知风险 K1 与 B4c

> **2026-09-21 覆盖(效力高于本节下文)**:本节写于 owner_epoch 机制落地之前。下文的 `kClaimAndLoadLuaScript` / `kFencedSaveLuaScript` / `<blob>:owner` / `PlayerSaveOwnerComp` / `PlayerSaveFencedComp` / 搬 DBTask / `redis_fence_test` **一律不实施** —— 它们已由 owner_epoch(`kSaveIfGuardLuaScript`、`player:{id}:owner_epoch`、`PlayerOwnerEpochComp`、`HandlePlayerSaveRejected`、DBTask.owner_epoch)覆盖,再造一套违反 AGENTS §11.5-3。逐项对照、B4c 实际要补的四处缺口与新的共享环境门禁,见 [08-save-owner-fence.md](./08-save-owner-fence.md)。

**已核实**:
- `HandlePlayerAsyncSaved` 只对超过 30s 重连租约的退出存盘打 WARN "save outran reconnect lease"(`player_lifecycle.cpp:267-284`);:610-627 注释承认"存盘超租约时玩家可能已在新节点读到旧数据"。
- 存盘脚本是无围栏的普通 SET(`redis_client.h:18-22`),`IssueSave` 以 1 个 key 调用(:459-474);失败无限重试(`HandlePlayerAsyncSaveFailed`,:154-183)。
- 玩家存盘 `GetPlayerDataRedis()->Save(message, playerId)`(`player_lifecycle.cpp:1277`),键 `full_name() + ":" + key`(`redis_client.h:140`),单条 hiredis 连接。

**K1 场景**:旧节点退出存盘因 Redis 抖动超过 30s → 玩家在新节点加载旧数据 → 新节点应用 Y、存盘、durable,Go 终结 → 旧节点排队的存盘晚到,覆盖 Redis → 新节点再无改动时脏比较判相等不再存盘 → 捐献的钱回到玩家手里,帮会资金保留(复制)。

**B4a 内的处理**:
1. I1 改写为"在重连租约内成立"(第 1 部分已改),K1 列为已知资产复制风险(偏差 #22)。
2. 告警:scene 以日志作指标源(`dirty_save_stats.h:129`)。LogQL `sum(count_over_time({service="scene"} |= "save outran reconnect lease" [5m])) > 0` 触发即查,规则文件随 B4c 交付。
3. **不采纳**"加载后 N 秒内资产改动回 RETRY":旧节点存盘无限重试,N 取多少都不成立;且同 zone 换节点(每次过图)都会挨这 N 秒,体验代价大而保护不完整。

**B4c 玩家存盘属主围栏**(新批次,需用户授权;**B5 可以先写代码,但在任何共享/预发环境打开帮会资产操作之前必须先落 B4c**):
- `redis_client.h` 新增两段脚本:
  ```lua
  -- kClaimAndLoadLuaScript: KEYS[1]=blob KEYS[2]=owner ARGV[1]=token
  redis.call('SET', KEYS[2], ARGV[1])
  return redis.call('GET', KEYS[1])
  -- kFencedSaveLuaScript: KEYS[1]=blob KEYS[2]=owner ARGV[1]=payload ARGV[2]=token
  local owner = redis.call('GET', KEYS[2])
  if not owner then redis.call('SET', KEYS[2], ARGV[2])
  elseif owner ~= ARGV[2] then return 0 end
  redis.call('SET', KEYS[1], ARGV[1])
  redis.call('SADD', 'dirty_keys_set', KEYS[1])
  return 1
  ```
  属主键 `<redis_key>:owner`,不设 TTL。加载与认领在同一段 Lua 里原子完成,认领之前落地的旧存盘一定被读到,之后的一律被拒。
- `MessageAsyncClient`:`Element` 增 `owner_token`;新增 `AsyncLoadAndClaim(key, token)`、`SaveFenced(message, key, token)`;`OnSaved` 收到整数 0 → 调新回调 `fenced_callback_(key)`,丢弃该 key 排队中的更新存盘,不重试。
- 令牌 `<节点 UUID>:<加载毫秒>:<进程内自增>`,存运行时组件 `PlayerSaveOwnerComp{std::string token;}`(不入库)。
- Kafka DBTask 从发起存盘处(`player_lifecycle.cpp:1309-1314`)挪到 `HandlePlayerAsyncSaved`,只有 Redis 接受的 blob 才进 MySQL。
- `HandlePlayerSaveFenced`:实体带 `UnregisterPlayer` → ERROR 后按退出完成销毁,不更新快照;活实体 → 挂 `PlayerSaveFencedComp`(资产 RPC 回 RETRY + kAssetBlocked)并踢下线。
- data_service 回档写 Redis blob 时同样先 SET 属主为 `rollback:<毫秒>`,否则旧 scene 的晚到存盘能覆盖回档结果。
- 落码前核对点:`rg -n "AsyncLoad\(" cpp/libs/services/scene`(玩家加载调用点)、节点 UUID 取法(`target_instance_id` 的来源,AGENTS §7.2)、`rg -n "PlayerAllData" go/data_service/internal/logic/rollback_logic.go`(回档写 Redis 处)。
- 测试:`cpp/tests/redis_fence_test`,需 `MMORPG_TEST_REDIS_ADDR`,未设 `GTEST_SKIP`:认领后旧令牌存盘被拒、新令牌通过、无属主键时首存建属主。
- 文件预估 ≤12 个,本节只定约束与接口,落码前单独评审。

<!-- s4_asset_part10.md -->

# S4 通用资产通道 — 第 10 部分:第二轮评审修订(三)Go 侧 assetop 修订、卡死行处置、回档 fail-closed

## 4.36 `go/shared/go.mod`(替换第 5 部分 4.17 第 1 条)

`require proto v0.0.0-00010101000000-000000000000` + `replace proto => ../proto`。

理由(已核实):`go/data_service/go.mod:18` 要求的是这个伪版本,其余服务要求 `v0.0.0`。伪版本在语义上低于 `v0.0.0`,shared 取最低值就不会把任何下游的选中版本往上抬;写 `v0.0.0` 会让 data_service 在默认 `-mod=readonly` 下报"updates to go.mod needed",而 data_service/go.mod 不在本批清单、且有并行会话在改。仓库无 `go/go.work`。

## 4.37 assetop 包修订(覆盖第 5 部分对应段落)

### types.go
- `Status` 只表达**语义**,不再写死数据库数值:`StatusPending, StatusApplied, StatusRejected, StatusAborted, StatusAppliedPartial`(iota+1)。落库数值由业务 Store 映射。原因:S1 定 1 基(`s1_storage_part2.md:32-38`),S5 偏差 #1 提议 0 基,两节冲突待主设计裁决;assetop 不该卷进去(偏差 #23)。
- `Result` 增 `Partial bool`。
- `Op` 增 `StreamEpoch uint64`、`LastReason uint32`。`Op.Request()` 带上 `StreamEpoch`,不带 `Auth`(由 Caller 签)。

### seq.go
```go
type SeqTables struct{ SeqTable, OpTable string; PendingStatus uint32 }
func NewSeqTables(seq, op string, pendingStatus uint32) (SeqTables, error)   // 表名正则同前

type Alloc struct{ Epoch, Seq uint64 }

// autocommit;epoch 取调用时毫秒,必须 >0
func EnsureSeqRow(ctx context.Context, db *sql.DB, t SeqTables, playerID uint64, s assetpb.AssetOpStream, nowMs uint64) error
//   INSERT IGNORE INTO {seq} (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, 1, ?, ?)

func AllocateSeq(ctx context.Context, tx *sql.Tx, t SeqTables, playerID uint64, s assetpb.AssetOpStream, lim Limits, nowMs uint64) (Alloc, error)
//   SELECT next_seq, epoch FROM {seq} WHERE player_id=? AND stream=? FOR UPDATE        -- 无行 → ErrSeqRowMissing;epoch==0 → ErrSeqRowCorrupt
//   SELECT seq FROM {op} WHERE player_id=? AND stream=? AND stream_epoch=? AND status=? ORDER BY seq LIMIT ? FOR UPDATE
//        -- status=t.PendingStatus;LIMIT=MaxPending+1;走 (player_id,stream,stream_epoch,status,seq) 索引
//   n>=MaxPending || (n>0 && next_seq-rows[0] >= MaxSpan) → ErrTooManyPending
//   UPDATE {seq} SET next_seq=next_seq+1, updated_ms=? WHERE player_id=? AND stream=?
//   return Alloc{Epoch: epoch, Seq: next_seq}

// 业务事务遇死锁/锁等待超时/TiDB 写冲突时整体重试。shared 不引 MySQL 驱动,错误分类由调用方注入;
// 帮会(B5)的分类:errors.As 到 *mysql.MySQLError 且 Number ∈ {1213, 1205, 9007}。
func WithTxRetry(ctx context.Context, db *sql.DB, attempts int, isRetryable func(error) bool, fn func(*sql.Tx) error) error
```
op 行插入时写 `stream_epoch = Alloc.Epoch`。`ErrSeqRowCorrupt = errors.New("assetop: seq row epoch is zero")`。

### caller.go
`Caller` 增 `Signer *Signer`(第 8 部分 4.32)。`Do` 第 0 步:`Signer == nil` → `ErrNoSigner`。每次实际发 RPC 前 `r := proto.Clone(req).(*assetpb.AssetOpRequest); c.Signer.Sign(rpc, r, nowMs)`,重查同样重签。

### decide.go
`Decide` 不变;`FinalStatus(rpc, res, op)` 多一个参数:APPLIED 且 `(res.Partial || op.LastReason == PartialReason)` → `StatusAppliedPartial`,其余同前。

### reconcile.go:领取改两步、有界并发、毒行跳过
替换第 5 部分的 `ClaimDue`:
```go
type Store interface {
    // 非加锁一致性读,走 (status,next_attempt_ms) 索引,不持锁。
    // **两段查询,不是一条 SQL**(X-03 防饿死,2026-09-19 落码):
    //   第一段(新行优先):… AND attempts <  FreshAttemptLimit ORDER BY next_attempt_ms ASC, op_id ASC LIMIT ?limit
    //   第二段(仅当第一段不足 limit 才发):… AND attempts >= FreshAttemptLimit ORDER BY 同上 LIMIT ?(limit-第一段条数)
    // 两段谓词互斥,但两次独立非锁读之间 attempts 可能从 2 跳到 3,所以仍要按 op_id 去重;
    // 第一段在前、总数 <= limit。阈值取 assetop 导出的 FreshAttemptLimit,不要各抄一份 3。
    //
    // 为什么不能写成一条 SQL:老行的退避被 MaxBackoff 封顶在 60s,于是它们永远"早就到期",
    // 按 next_attempt_ms 排序必定霸占整批名额,新提交的指令永远排不上号。
    // **反向代价(已知并接受)**:新行持续满额时老行零名额,靠 assetop_pending_oldest_age_seconds 告警兜底。
    ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error)
    // 单行主键 CAS 领取(autocommit),紧挨着处理前调用:
    //   UPDATE {op} SET lease_until_ms=?, lease_token=? WHERE op_id=? AND status=<pending> AND lease_until_ms<?
    //   RowsAffected==0 → (Op{}, false, nil);==1 → SELECT 全列 WHERE op_id=? 并解 payload。
    //   解码失败:Store 自行 UPDATE last_outcome=0, next_attempt_ms=poisonUntilMs, lease_until_ms=0
    //            WHERE op_id=? AND lease_token=?,返回 (Op{}, false, ErrPoisonRow)
    //   poisonUntilMs 由循环按 LoopConfig.PoisonDelay 算好传进来(2026-09-19 加的第 6 个参数);
    //   **实现不得再自写毒行延迟常量** —— 那会让 yaml 里的 PoisonDelay 改了不生效,两份值迟早分叉。
    Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (Op, bool, error)
    Finalize(ctx context.Context, op Op, status Status, res Result, nowMs uint64) (bool, error)   // 语义同前,须包在 WithTxRetry 里
    // SET … last_reason=res.Reason …;**RowsAffected==0(租约已被别的副本抢走)必须回 ErrLeaseLost**
    // (可 %w 包裹)。不回的话"我这次的结果被丢弃了"在指标里永远看不见。
    Reschedule(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64) error
}
var ErrPoisonRow = errors.New("assetop: op payload undecodable")
var ErrLeaseLost = errors.New("assetop: lease lost before reschedule")
```
为什么两步:原 `UPDATE … WHERE status AND next_attempt_ms ORDER BY LIMIT` 在 MySQL 可重复读下按范围加 next-key 锁,和业务事务里 AllocateSeq 的 `FOR UPDATE` + 插入新 op 行互相等待,可能死锁,被选为牺牲者的是用户请求。改成非加锁读 + 主键 CAS 后,重投循环每次只锁一行。业务事务仍可能与 Reschedule 的单行更新撞锁(二级索引与主键加锁顺序相反),所以业务写路径一律包 `WithTxRetry(attempts=2)`。

`LoopConfig` 改为:
```go
Interval 2s; Batch 100; Workers 8; Lease 10s; OpBudget 2500ms;
BaseBackoff 1000ms; MaxBackoff 60s; AwaitDurableDelay 500ms; PoisonDelay 1h; LedgerReadMinAttempts 3
```
`NewLoop` 校验:`1 <= Workers <= 64`、`Batch >= Workers`、`OpBudget + 2s <= Lease`、`BaseBackoff <= MaxBackoff`、
`PoisonDelay > 0`、**`OpBudget > settleBudget`**,否则返回错误。

> **单行预算被切成两半**(2026-09-19,修的是一条会丢钱的路径)。原先 `ProcessOne` 把整个 `OpBudget`
> 交给 `applier.Do`,给后面的落库留 0:RPC 跑满预算之后,`Finalize` 必定拿到已过期的 ctx ——
> **scene 已经扣了钱,outbox 行却更新不了**,重投循环下次再扣一遍(靠 seq 幂等兜住,但行会一直卡着)。
> 现在投递只拿 `OpBudget - settleBudget`(默认 1800ms),落库另走 `settleBudget`(700ms)且
> **不继承父 ctx 的取消**(`context.WithoutCancel` + 自带超时):只切比例救不了"父 ctx 在进来之前就被花掉一部分",
> 只用 `WithoutCancel` 又会让关停无限期挂住。代价是关停时每行最多多等 700ms。
>
> 连带口径:1800ms 只保证**快路径**三轮重查全过;慢路径(每轮重查自己还要再发一次 RPC,各自上限
> `CallTimeout`)约只容得下一轮,之后转 AwaitDurable 500ms 后重排 —— 钱是安全的,但
> `assetop_requery_total{result="timeout"}` 与 `reschedule_total{reason="await_durable"}` 上线后预期上升。

`Tick`:`ids := ListDue(now, Batch)` → 投进容量 Batch 的通道 → `Workers` 个 goroutine(`shared/safego` 启动)各自 `Claim(id, now, now+Lease, 随机令牌)` 后立即 `ProcessOne`;`Tick` 等全部 worker 结束才返回。租约从"领到这一行"起算,处理耗时 ≤ OpBudget,不会再出现排在批尾的行租约早已过期、被别的副本重复领取的情况。Claim 返回 false 计 `assetop_claim_total{result="lost"}`;`ErrPoisonRow` 计 `assetop_store_errors_total{op="decode"}` + ERROR 日志(op_id),继续下一行。

### 离线已落盘结局读取(减少卡死行)
```go
// 读 zone Redis 中已落盘的玩家账本;玩家从未落盘 → (nil, nil)。实现在 B5(data_service 新 RPC,见 4.38)
type LedgerReader interface {
    ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error)
}
// 新文件 classify.go。Go 版只读分类,与 C++ ClassifyAssetOpSeq 同语义;两边单测用同一张用例表
func ClassifyPersisted(l *componentpb.PlayerAssetOpLedgerComp, s assetpb.AssetOpStream, epoch, seq uint64) (outcome assetpb.AssetOpOutcome, partial bool, reason uint32)
```
`Loop` 增可选字段 `Ledger LedgerReader`。`ProcessOne` 在 Decide 之前:若 `res.Local`(无位置)且 `op.Attempts >= LedgerReadMinAttempts` 且 `Ledger != nil` → 读账本;分类为 APPLIED/REJECTED 时按 `Result{Outcome, Reason, Partial, Durable: true}` 走 Finalize(已在 Redis,即 durable)。未见 → 照常退避。计 `assetop_ledger_read_total{service,result}`,result ∈ finalized / unseen / absent / error。

为什么安全:已落盘账本里的结局按 I2 永不改变,所以玩家在不在线都不影响这次读取。剩余风险只有 K1(旧存盘覆盖)与 C5(Redis 丢数据),与在线路径同级。离线且未见的行**不**在 Go 侧自行判中止(I7):scene 没记账就无法保证不会被晚到的请求应用。

## 4.38 卡死行处置

**会卡住的行**:UNKNOWN(配置不一致、纪元过期、跳号、鉴权失败)每 60s 重排;离线且未见的行一直 PENDING;玩家从不上线。前者修好配置会自动恢复;其余要人工路径。

**assetop 提供**(可选接口):
```go
type ManualResolution struct {
    OpID     uint64
    Final    Status  // Applied / Rejected / Aborted / AppliedPartial
    Operator string  // ≤64
    Reason   string  // ≤191
}
type ManualResolver interface {
    // 单事务(WithTxRetry):业务锁序加锁 → UPDATE {op} SET status=?, resolved_by=?, resolve_reason=?, updated_ms=?
    //   WHERE op_id=? AND status=<pending>;RowsAffected==1 才做对侧账(Applied 入账;Rejected/Aborted 退款;AppliedPartial 不动)
    ResolveManually(ctx context.Context, r ManualResolution, nowMs uint64) (bool, error)
}
```
计 `assetop_manual_resolve_total{service,status}`,并打 INFO `[AssetOp] manual op_id=… final=… operator=… reason=…`。

**交给 B5 的要求**(S5 认领,偏差 #24):
- `guild_asset_op` 追加 `string resolved_by = 27;`、`string resolve_reason = 28;`。
- guild 新增**管理** RPC `ResolveAssetOp{uint64 op_id; uint32 final_status; string operator; string reason}` → `{TipInfoMessage error_message; bool resolved}`,不进 `session.ClientMethods`。鉴权照 `gate_security.h:76-98` 的四段式 operator(`<操作人>|<unix 秒>|<nonce>|<hmac>`),canonical 含方法名、op_id、final_status、reason;密钥 `MMORPG_GUILD_ADMIN_SECRET`。
- 手册:人工终结前,玩家在线则先让循环发一次 `AssetAbortDebit`(REJECTED 且 durable 即自动终结,无需人工);只有 UNKNOWN 或长期离线才人工;判 APPLIED/REJECTED 以 scene 流水 `correlation_id = op_id` 为证据。
- data_service 新增内部 RPC `GetPlayerAssetOpLedger{uint64 player_id}` → `{bool found; PlayerAssetOpLedgerComp ledger}`,读 zone Redis 的玩家 blob,只读;实现 `LedgerReader`。
- 告警规则:`assetop_pending_oldest_age_seconds > asset_op_deadline_seconds + 3600` 持续 10 分钟 → 告警;`increase(assetop_unknown_total[10m]) > 0` → 告警;`increase(assetop_partial_total[1h]) > 0` → 告警。

## 4.39 回档必须 fail-closed(替换 C10 与 4.13 的"接受 + 审计")

**已核实**:`go/data_service/internal/logic/rollback_logic.go:378-382` 明确不回滚帮会。单人回档还能人工补,`RollbackZone / RollbackAll` 会批量复制帮会资金与帮贡;客户端侧的 `GmExecuteRollback` 已由 4.34 统一闸门关掉。

**交给 B5 的要求**(偏差 #25):
- guild 新增内部 RPC `ListAppliedAssetOpsSince{uint32 zone_id; repeated uint64 player_ids; uint64 since_ms; uint64 after_op_id; uint32 limit}` → `{repeated GuildAssetOpBrief ops; uint64 next_after_op_id}`;`GuildAssetOpBrief{op_id, player_id, guild_id, stream, kind, status, funds_delta, contribution_delta, updated_ms}`。zone 维度经 `guild.zone_id` 关联;limit ≤500。
- data_service 的 `Rollback*` 在任何写之前调用它(since_ms = 快照时间),条件 `status ∈ {APPLIED, APPLIED_PARTIAL} AND updated_ms > since_ms`。有结果时**默认拒绝**,返回清单条数;请求显式带 `accept_guild_divergence = true` 才放行,放行时把清单以 INFO 逐行写日志供补偿。guild 调用失败 → 拒绝回档(fail-closed)。
- 过渡期(B5 落地前):手册禁止对有帮会资产操作的 zone 执行 RollbackZone/RollbackAll。

<!-- s4_asset_part11.md -->

# S4 通用资产通道 — 第 11 部分:第二轮评审修订(四)冒烟重写、批次清单、Codex 验证增补、契约偏差

## 4.40 端到端冒烟(替换第 6 部分 4.24)

**账号**:专用 `robot_9501`。不在契约 §6 帮会冒烟段(9211–9219),也不在已占用的 9001–9006、9101–9102、9201–9203、9301–9304、9401–9403(已 grep robot/tools/docs 与本设计目录,95xx 无人使用)。该账号**只给本冒烟用**,永远不参与帮会或交易业务,这是 I6"流独占"的唯一例外。

**每次运行一个新纪元**:`epoch := uint64(time.Now().UnixMilli())`,seq 从 1 起。上一次运行的纪元更小,scene 按 4.29 规则重置该流,不会有窗口永久滑走的问题。

1. **起全栈**:按"xuanming 本地开机 runbook";scene 经 `cpp_nodes.ps1` 启动时运行模式兜底 dev(GM 客户端消息放行),并注入两把资产通道开发密钥。
2. **准备玩家**:`robot_9501` 登录进场并保持在线(Codex 选能保持在线的 robot 模式,记录实际命令);GmAddCurrency 加金币 1000;记下 player_id 与余额 B0。
3. **跑冒烟**:
   ```
   $env:ASSETOP_SMOKE_PLAYER_ID="<id>"
   $env:ASSETOP_SMOKE_REDIS_ADDR="127.0.0.1:6379"
   $env:ASSETOP_SMOKE_ETCD="127.0.0.1:2379"
   # MMORPG_ASSET_OP_SECRET_GUILD 与 scene 一致;未设时测试用 cpp_nodes.ps1 的开发值
   go test -tags assetop_smoke ./assetop -run TestSceneAssetOpSmoke -count=1 -v
   ```
   Signer caller=`guild`。依次(E=本次纪元):
   - ① `Debit(GUILD_DEBIT, E, seq1, 金币 100, TX_GUILD_DONATE, corr=E)` → 2.5s 内 Terminal 且 APPLIED;
   - ② 同请求再发 → APPLIED,durable;
   - ③ `Abort(GUILD_DEBIT, E, seq2)` → REJECTED,reason 0,durable;
   - ④ `Debit(E, seq2)` → REJECTED;
   - ⑤ `Debit(E, seq3, 金币 1e12)` → REJECTED,reason 27000;
   - ⑥ `Credit(GUILD_CREDIT, E, seq1, 金币 100, TX_GUILD_SHOP)` → APPLIED,durable,partial=false;
   - ⑦ GUILD_CREDIT 流调 AssetDebit → UNKNOWN,reason 27004;
   - ⑧ `Debit(GUILD_DEBIT, E−1, seq4)` → UNKNOWN,reason 0(纪元过期);
   - ⑨ `Debit(GUILD_DEBIT, E, seq 5000)` → UNKNOWN,reason 0(跳号);
   - ⑩ 用另一把 32 字节假密钥签名的 ① 型请求(seq5)→ UNKNOWN,reason 27008;
   - ⑪ caller=`trade`(用 trade 开发密钥)发 GUILD_DEBIT seq5 → UNKNOWN,reason 27008。
4. **核对**:robot 拉背包,金币 == B0;scene 日志里 `rpc=debit … seq=1 … outcome=1` 的应用行恰 1 条;⑧⑨⑩⑪ 之后账本不含 seq4/5/5000(可再发 `Abort(E, seq5)` 得到 REJECTED 验证 seq5 未被记账)。
5. **崩溃窗口(可选)**:① 之后立刻强杀 scene,重启、robot 重新进场,再发 ① → APPLIED,余额只扣一次。
6. **GM 闸门**(B4a-2 之后):`cpp_nodes.ps1 -NoClientGm` 重启 scene,robot 的 GmAddCurrency、GmGrantPet 均收到 tip 1006。

保留:冒烟 `-v` 输出、scene 日志中的 `[AssetOp]` 行、各步余额。

## 4.41 批次与文件清单(替换第 4 部分 4.15、第 6 部分 4.22)

**B4a-1 资产通道 C++ 本体(手改 30)**:
| # | 文件 |
|---|---|
| 1–2 | `proto/common/asset/asset_op.proto`、`proto/common/component/asset_op_ledger_comp.proto`(新) |
| 3–5 | `proto/scene_manager/scene_node_service.proto`、`proto/common/database/mysql_database_table.proto`、`proto/common/rollback/transaction_log.proto` |
| 6 | `data/tip/Tip.xlsx`(组头 + 9 行) |
| 7–12 | `cpp/libs/services/scene/player/system/asset_op_ledger.{h,cpp}`、`player_asset_op.{h,cpp}`、`asset_op_auth.{h,cpp}`(新) |
| 13–14 | `cpp/libs/services/scene/CMakeLists.txt`、`scene.vcxproj` |
| 15 | `cpp/libs/services/scene/player/system/player_database_loader.cpp` |
| 16 | `cpp/nodes/scene/handler/grpc/scene_node_service.cpp`(仅守护段) |
| 17–22 | `currency_system.{h,cpp}`、`transaction_log_system.{h,cpp}`、`bag_service.{h,cpp}` |
| 23–24 | `cpp/generated/table/CMakeLists.txt`、`table.vcxproj` |
| 25–27 | `cpp/tests/currency_test/asset_op_ledger_test.cpp`、`asset_op_system_test.cpp`、`asset_op_auth_test.cpp`(新) |
| 28–29 | `cpp/tests/currency_test/currency_test.cpp`、`currency_test.vcxproj` |
| 30 | `tools/scripts/cpp_nodes.ps1`(scene 注入 GM 开关与两把开发密钥,`-NoClientGm`) |

**B4a-2(手改 3)**:GM 闸门部分**整体作废**——P0-a 已经落地,形状是 gate 按消息号闸 + scene `player_gm_guard.h`,不要再做 `client_gm_gate.h` / `client_gm_gate_test.cpp` / `scene_handler.cpp` 守护段这三项。本批**仍要做**的只剩:`player_currency_handler.cpp`(GM 流水 tx 改 `TX_GM_GRANT / TX_GM_DEDUCT`)、`tools/scripts/cpp_nodes.ps1` 与 `tools/scripts/dev_mprocs_proc.ps1`(`"cpp-scene"` 分支注入两把资产通道开发密钥)。

**B4a-client(客户端仓,手改 2)**:`Assets/Scripts/Game/Pet/PetClient.cs`、`Assets/Scripts/Game/Attribute/AttributeClient.cs` 的 `DescribeTip` switch 各加 27000–27008 九行,文案与 Tip.xlsx 一致(照 `PetClient.cs:80-82` 注释的镜像约定)。已核实:宠物、加点扣币前先 `CanAfford`,余额不足回的是 `kPetGoldNotEnough / kAttributeGoldNotEnough`(`player_pet.cpp:582-583,675-676`,`player_attribute.cpp:658-659,750-751`),27000 走不到;但冻结(27003)与 GM 路径会出现新码,原先会显示"tip=N"。

**B4b 通用资产通道 Go(手改 25)**:`go/shared/go.mod`;`scenenode/` 的 watcher/conn/locator/metrics 与 2 个测试;`assetop/` 的 types/seq/caller/decide/reconcile/metrics/auth/classify 8 个;测试 decide/caller/reconcile/seq_integration/scene_smoke/auth/classify 7 个;`docs/design/jubaozhai-market.md`;`docs/design/guild-phase2.md`;`PROGRESS.md`。metrics.go 在第 5 部分基础上追加 `assetop_partial_total{service,stream}`、`assetop_claim_total{service,result}`(claimed/lost/poison)、`assetop_ledger_read_total{service,result}`、`assetop_manual_resolve_total{service,status}`,`assetop_store_errors_total` 的 op 增 list / decode / ledger_read。

**B4c 玩家存盘属主围栏(≤12,单独评审与授权)**:见第 9 部分 4.35。**2026-09-21 起以 [08-save-owner-fence.md](./08-save-owner-fence.md) 为准(5 个手改文件)。**

## 4.42 Codex 验证增补(在第 4 部分 4.16 与第 6 部分 4.23 基础上)

**B4a-1**:
- 第 0 步 `git status --short` 目录追加 `go/*/generated robot/generated generated/proto/_unified`;另存 `generated/proto/_unified/proto/message_id.txt` 副本为 `message_id.before.txt`。
- 第 1 步生成后:
  - `Compare-Object (Get-Content message_id.before.txt) (Get-Content generated/proto/_unified/proto/message_id.txt)`:只允许 3 条 `=>`,分别含 AssetDebit / AssetAbortDebit / AssetCredit;出现任何 `<=` 立即停下(说明与交易会话未提交的 196–200 号冲突)。
  - `Select-String go/client_rpc_router/generated/pb/game/route_table.go -Pattern 'Method: "Asset(Debit|AbortDebit|Credit)"'` 恰 3 行,且每行含 `ClientProtocol: false`(对照 :119 的 PrepareBattle 行)。
- **新第 1b 步 MySQL 迁移**(导表之后、编译之前均可,冒烟之前必须):`cd go/db; go run ./cmd/migrate -f etc/db.yaml -command plan`,计划只含 `player_database` 加列 `asset_op_ledger`(若 B3a 同批加字段,两者同一次);确认后 `-command up`(不加 `-allow-modify`)。本地多 zone 时对启动器实际使用的每份 db 配置各跑一次,记录用了哪些 `-f`。随后每个 zone 库 `SHOW COLUMNS FROM player_database LIKE 'asset_op_ledger'` 返回 1 行。原因:`go/db/etc/db.yaml:62` `AutoMigrateSchema: false`,`key_ordered_consumer.go:597` 按 descriptor 写全部列,缺列会让全部玩家回写失败。K8s 部署时由 db migrate Job 执行(`k8s_deploy.ps1:1616`)。
- 第 4 步过滤器 `--gtest_filter=AssetOp*:CurrencyTest*`(覆盖 AssetOpLedgerTest / AssetOpSystemTest / AssetOpAuthTest)。
- 第 6 步字段号检查改为只看 `player_database` 消息体:
  ```powershell
  $t = Get-Content proto/common/database/mysql_database_table.proto -Raw
  $m = [regex]::Match($t, '(?s)message\s+player_database\s*\{.*?\n\}')
  [regex]::Matches($m.Value, '=\s*(\d+)\s*;') | ForEach-Object { $_.Groups[1].Value } | Group-Object | Where-Object Count -gt 1
  ```
  输出为空才通过。

**B4a-2**:MSBuild `/m:1` 编 scene 节点工程与 currency_test;`--gtest_filter=ClientGmGateTest*` 全绿;冒烟第 6 步。

**B4a-client**:客户端仓 `client_compile_check.ps1` 通过。

**B4b**:
- 第 1 步追加 `Select-String go/shared/go.mod -Pattern 'proto v0.0.0-00010101000000-000000000000'` 命中。
- 第 5 步逐模块 `go build ./...` 时,`go/data_service` 的结果单独记录一行(它是唯一要求伪版本的模块)。

## 4.43 契约偏差(第二轮,接第 6 部分 #1–#17、第 7 部分 #18–#19)

20. **流纪元**:`AssetOpRequest.stream_epoch = 7`、账本 `stream_epoch = 7`;`guild_player_op_seq.epoch = 5`、`guild_asset_op.stream_epoch = 26`;`guild_asset_op` 的 UNIQUE 改为 `player_id,stream,stream_epoch,seq`,第 3 索引改为 `player_id,stream,stream_epoch,status,seq`。S1/S5 表 message 需同步。
21. **新增 2 个 asset tip**:`AssetPartialApplied`(27007,fault=1)、`AssetAuthFailed`(27008,fault=1);`AssetOpResponse.partial = 4`、账本 `partial_seqs = 8`;表状态枚举追加 APPLIED_PARTIAL。契约 §4 原列 7 个。
22. **已知风险 K1**:单写者只在 30s 重连租约内成立;根治靠新批次 B4c。建议把"任何共享环境打开帮会资产操作"列为 B4c 的硬前置。(**2026-09-21 订正**:本条已被 [08-save-owner-fence.md](./08-save-owner-fence.md) 取代 —— owner_epoch 机制已堵住跨节点版 K1,B4c 改为补它的缺口,不造 token 围栏;共享环境门禁见 08 §8.3)。
23. **assetop.Status 不绑数据库数值**:S1(1 基)与 S5 偏差 #1(0 基)冲突,请主设计裁决帮会表取值;assetop 经 `SeqTables.PendingStatus` 与 Store 映射兼容两者。第 7 部分 E1 的"trade 必须用同一编号"作废。
24. **卡死行处置交 B5**:`ResolveAssetOp` 管理 RPC、`guild_asset_op.resolved_by = 27 / resolve_reason = 28`、data_service `GetPlayerAssetOpLedger`、三条告警规则(4.38)。
25. **回档 fail-closed 交 B5**:guild `ListAppliedAssetOpsSince` + data_service `Rollback*` 前置检查与 `accept_guild_divergence`(4.39)。取代原偏差 #13。
26. **请求带签名** `AssetOpRequest.auth = 8`(`AssetOpAuth`),每个调用方一把密钥 `MMORPG_ASSET_OP_SECRET_GUILD / _TRADE`;SYSTEM_CREDIT v1 一律拒。聚宝斋 P2 在 `AssetOpResponse` 追加字段从 5 起。
27. **GM 闸门扩大到全部客户端 `Gm*`**(契约 §3.3 只写了两个),实现为分发入口统一闸门;B4a 拆为 B4a-1 / B4a-2 / B4a-client。原偏差 #11 关闭。
28. **冒烟专用账号 `robot_9501`** 不在契约 §6 账号段;契约 §6 建议补一行"95xx 归资产通道冒烟"。
29. **上线项**:K8s 注入两把资产密钥(`Resolve-InjectedSecret -MinLength 32`,不得与其它密钥相同);scene gRPC 端口 NetworkPolicy 只放行 scene_manager / match / guild / trade(仓库目前没有任何 NetworkPolicy)。
30. **原偏差 #8 部分缓解**:离线玩家行可经 LedgerReader 读取已落盘结局终结;离线且未见的行仍保持 PENDING(I7)。

---

## 附录:对抗评审处理记录

# S4 通用资产通道 — 第二轮对抗评审处置表(2026-09-16)

评审结论:1 阻断、8 主要、8 次要。逐条对照代码核验后:**15 条采纳,2 条部分采纳,驳回部分均给出代码证据**。修订落在第 8–11 部分(新增),第 1–7 部分就地改并指向新部分;优先级 11 > 10 > 9 > 8 > 7 > 1–6。全部未编译,待 Codex 验证。

| # | 级别 | 问题 | 处置 | 核验证据 / 理由 | 改动位置 |
|---|---|---|---|---|---|
| 1 | 阻断 | seq 与操作无绑定,帮会库重置/恢复后复制或丢失 | **已采纳** | `s1_storage_part5.md:166` 确把 `mmorpg_guild` 列入 BR 按库恢复;账本只按位图答复。采纳纪元 + 跳号上限 + 恢复手册 + 单测。在评审建议之外补了两处:`guild_asset_op` 的 UNIQUE 必须并入 `stream_epoch`(否则 next_seq 回 1 后与恢复出的旧行撞键);I5 改为只数本纪元未决行,旧纪元残行不挡新操作 | 第 8 部分 4.29–4.31;第 1 部分 proto;第 2 部分 Classify/Record/Validate/IsDurable、C13;第 7 部分 E2 修订 |
| 2 | 主要 | 冒烟用 robot_9219 + Unix 秒作 seq,永久卡死帮会冒烟账号 | **已采纳** | 契约 §6 确把 9211–9219 划给 guild_smoke。改用专用 `robot_9501`(已 grep robot/tools/docs 与本设计目录,95xx 无人用),每次运行取新纪元、seq 从 1 起;删去"测试账号可以接受" | 第 11 部分 4.40;第 6 部分 4.24 标作废;偏差 #28 |
| 3 | 主要 | AssetCredit/AssetDebit 无鉴权,集群内可任意发币 | **已采纳(实现方式调整)** | `node.cpp:623` 明文不安全凭据属实。调整三点:① 签名放**请求消息体** `AssetOpRequest.auth`,不放 metadata——`grpc_handler_gen.go:125` 把 context 注释成 `/*context*/`,读 metadata 要改生成器并重生成全部 gRPC 包装,安全性等价而改动面小得多;② **每个调用方一把密钥**(GUILD/TRADE 分开),比共用一把更能隔离流;③ 不设 nonce:同 seq 重放要么只读、要么就是唯一一次应用,Go 总以 scene 结局为准,只保留 5 分钟时间窗。密钥未配时**一律拒绝**,不做 dev 放行,本地脚本显式注入开发值。NetworkPolicy:仓库内无任何 NetworkPolicy 清单,列为上线项 | 第 8 部分 4.32;第 3 部分 4.9 第 1b 步、4.10;偏差 #26、#29 |
| 4 | 主要 | I1 单写者只靠 30s 租约,晚到旧存盘可覆盖 | **部分采纳** | `player_lifecycle.cpp:267-284` 的 WARN、:610-627 注释、`redis_client.h:18-22` 无围栏 SET、失败无限重试均属实。采纳:① I1 改写为"租约内成立"并列已知风险 K1;② 告警(scene 以日志为指标源,给出 LogQL)。**驳回 ③"加载后 N 秒内资产改动回 RETRY"**:旧节点存盘无限重试,N 取多少都不成立;同 zone 每次过图都要挨 N 秒,代价大且保护不完整。根治方案设计为新批次 **B4c 存盘属主围栏**(认领式加载 + 带属主比较的存盘 Lua、DBTask 挪到存盘成功后),并要求任何共享环境打开帮会资产操作前必须先落 B4c(**2026-09-21 订正**:已被 [08-save-owner-fence.md](./08-save-owner-fence.md) 取代,不造 token 围栏;共享环境门禁见 08 §8.3) | 第 9 部分 4.35;第 1 部分 I1;第 2 部分 C8、C14;偏差 #22 |
| 5 | 主要 | player_database 加列没有 MySQL 迁移 | **已采纳** | `go/db/etc/db.yaml:62` `AutoMigrateSchema: false`、`key_ordered_consumer.go:597` 全列写、`go/db/cmd/migrate` 存在(`-command plan/up`)均属实。验证加第 1b 步:plan → up(不加 `-allow-modify`)→ 每个 zone 库 `SHOW COLUMNS` 确认;K8s 走 migrate Job | 第 11 部分 4.42;第 1 部分 4.3.4 |
| 6 | 主要 | GM 闸门漏口多,逐 handler 加闸必漏 | **已采纳** | grep 实得客户端可调 `Gm*` 18 个(货币 4、宠物 1、属性 1、回档/欠款 12)。统一闸门放在唯一客户端入口 `ProcessClientPlayerMessage`(gate 经 `client_message_processor.cpp:699-711` 只走它),在 `CallMethod`(:418)前按 `Gm+大写` 前缀拦截;`InvokePlayerService`/`SendMessageToPlayer` 是节点路由,不加闸并写明约束。补描述符扫描测试防 `GMGrant` 类绕过。为守 30 文件上限拆出 B4a-2 | 第 9 部分 4.34;第 3 部分 4.12 标作废;第 1 部分 I8;偏差 #27 |
| 7 | 主要 | Credit 部分失败仍记 APPLIED,伪装成功 | **已采纳** | 原稿 4.9 第 10(b)(c) 步属实,违反 AGENTS §11.3。响应加 `partial`、tip `AssetPartialApplied`(fault=1)、账本 `partial_seqs`、Go 终态 APPLIED_PARTIAL(不入账不退款,计数并转人工);失败面前移:guid 号段余量预检(`guid_segment_client.h:158` `Available()`)。货币封禁预检原稿已有。无改动时失败回 RETRY 不记账 | 第 9 部分 4.33;第 3 部分 4.9、4.10;第 1 部分 proto 与 tip;偏差 #21 |
| 8 | 主要 | 卡住的行无处置路径 | **已采纳(细化)** | 增 `ManualResolver`(CAS status=PENDING + 操作人/理由列)、B5 管理 RPC `ResolveAssetOp`(签名照 `gate_security.h:76-98`)、`LedgerReader` 离线读已落盘结局、三条告警。细化:离线读取**不需要**"位置键与会话都不存在"前置——已落盘结局按 I2 永不改变,在线与否都安全;离线且未见的行不在 Go 侧自判中止(I7,scene 没记账就挡不住晚到请求) | 第 10 部分 4.37–4.38;第 5 部分 ProcessOne 第 5 步;偏差 #24、#30 |
| 9 | 主要 | 回档复制靠人工补偿,批量回档不可行 | **已采纳** | `rollback_logic.go:378-382` 属实。改为 fail-closed:B5 由 guild 提供 `ListAppliedAssetOpsSince`,data_service `Rollback*` 写前检查,有结果默认拒绝,显式 `accept_guild_divergence` 才放行并逐行记日志;guild 调用失败也拒绝。**不采纳**"自动生成反向 Debit":玩家可能离线或余额不足,补偿本身又会卡住。`GmExecuteRollback` 客户端路径已由 #6 关闭 | 第 10 部分 4.39;第 2 部分 C10;第 3 部分 4.13;偏差 #25 |
| 10 | 次要 | shared require `proto v0.0.0` 会抬高 data_service 选中版本 | **已采纳** | `go/data_service/go.mod:18` 是伪版本,其余是 v0.0.0,无 `go/go.work`。shared 改 require 伪版本;B4b 验证单独记录 data_service 构建结果 | 第 10 部分 4.36;第 5 部分 4.17;第 11 部分 4.42 |
| 11 | 次要 | ClaimDue 范围 UPDATE 在 RR 下与业务事务死锁 | **已采纳** | 改为非加锁 `ListDue` + 主键 CAS `Claim`;业务写路径包 `WithTxRetry(attempts=2)`。shared 不依赖 MySQL 驱动,死锁分类函数由调用方注入(帮会:1213/1205/9007) | 第 10 部分 4.37 |
| 12 | 次要 | 租约 10s 与批 100×2.5s 不匹配,重复领取 | **已采纳(与 #11 合并解决)** | 领取挪到"处理前一刻逐行 Claim",租约从领到起算;`Workers=8` 有界并发;`NewLoop` 校验 `OpBudget + 2s <= Lease`、`Batch >= Workers` | 第 10 部分 4.37 |
| 13 | 次要 | payload 解码失败的毒行卡住整个循环 | **已采纳** | Claim 内解码失败 → Store 写 UNKNOWN、推迟 1h、返回 `ErrPoisonRow`,计 `store_errors{op="decode"}`,其余行照常;补 `PoisonRowSkipped` | 第 10 部分 4.37;第 5 部分测试表 |
| 14 | 次要 | 27000 段码对客户端可见,Unity 手写镜像缺失 | **部分采纳** | **驳回其中"宠物或加点货币不足会显示兜底文案"**:`player_pet.cpp:582-583,675-676`、`player_attribute.cpp:658-659,750-751` 扣币前先 `CanAfford` 并回 `kPetGoldNotEnough / kAttributeGoldNotEnough`,27000 走不到。**采纳其余**:冻结(27003)、封禁与 GM 路径确会透传新码,`PetClient.cs:80-98` 是手写镜像。二选一定为客户端补镜像(B4a-client:PetClient、AttributeClient 各加 27000–27008);不做 scene 映射回原码——原码是 `kInvalidParameter`,映射回去等于丢信息。原稿"不用改"的说法已更正 | 第 3 部分 4.11;第 11 部分 4.41 |
| 15 | 次要 | 提示文案过度承诺"自动继续/自动发放" | **已采纳** | 改为"稍后自动重试;超过时限可能取消",具体时限由 S5 界面显示。用"可能"而非"将":S5 偏差 #3 定商店 `deadline_ms=0` 不取消,捐献才会取消 | 第 1 部分 4.3.6 |
| 16 | 次要 | Codex 验证缺口(生成物目录、路由表、字段号匹配) | **已采纳** | `route_table.go:119` SceneNodeGrpc 行 `ClientProtocol: false` 属实;`generated/proto/_unified/proto/message_id.txt` 存在。第 0 步补目录并留 message_id 副本,生成后只许新增 3 行;第 1 步查三个 RPC 均 `ClientProtocol: false`;字段号检查只截 `player_database` 消息体 | 第 11 部分 4.42;第 4 部分 4.16 指向 |
| 17 | 次要 | 聚宝斋文档改动超出"只改通用名" | **已采纳** | 第 6 部分 4.21 第 3、4、6、7、8 条改为只替换名字;结果环、快照、C5 持久化等级、字段形状写成开放项 J-A1/J-A2/J-A3 追加在 §6.4 末尾,由交易会话决定 | 第 6 部分 4.21 |

## 需主设计 / 其它分节跟进的事项

1. **S1 与 S5 的 `GuildAssetOpStatus` 数值冲突**:S1 定 1 基(`s1_storage_part2.md:32-38`),S5 偏差 #1(`s5_economy_part7.md:95`)提议 0 基。S4 已改为不绑数值(偏差 #23),请主设计裁决帮会表取值,并在 ABORTED 之后追加 APPLIED_PARTIAL。
2. **S5 要跟改**:`s5_economy_part4.md:126-137` 仍是单条范围 `UPDATE … LIMIT` 的 ClaimDue 与 `status = 0`,须换成 `ListDue + Claim`;`guild_asset_op` 追加 26–28 列、UNIQUE 与第 3 索引并入 `stream_epoch`;`guild_player_op_seq.epoch = 5`;业务写路径包 `WithTxRetry`;接 `Signer`(`MMORPG_ASSET_OP_SECRET_GUILD`,`go_services.ps1` 注入开发值);实现 `ResolveAssetOp`、`ListAppliedAssetOpsSince`、`LedgerReader`(data_service `GetPlayerAssetOpLedger`)与告警规则;APPLIED_PARTIAL 不入账不退款。
3. **S6**:给离线成员发活动奖励时,离线未见的行会一直 PENDING 并占 I5 名额(每流 16),发奖批量需考虑;已见结局可经 LedgerReader 离线终结。
4. **新批次 B4c**(存盘属主围栏)需用户单独授权;建议列为"任何共享/预发环境开放帮会资产操作"的硬前置。(**2026-09-21 订正**:已被 [08-save-owner-fence.md](./08-save-owner-fence.md) 取代,不造 token 围栏;共享环境门禁见 08 §8.3)。
5. **契约 §6** 建议补一行:`robot_95xx` 归资产通道冒烟。
6. **上线项**:K8s 注入两把资产密钥(`Resolve-InjectedSecret -MinLength 32`,不得与其它密钥复用);scene gRPC 端口 NetworkPolicy 只放行 scene_manager / match / guild / trade。
