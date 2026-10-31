# 落地恢复：按域版本摘要、场景侧待办唤醒与超窗处置（2026-10-14）

> **状态**：v1.1（2026-10-16）。代码已按本文落码并随用户快照入库，**未编译、未测试**（AGENTS.md §10.1）。
> 剩余工作与验证顺序见交接 [handoff-landing-recovery-and-fencing-20261016.md](../handoff/handoff-landing-recovery-and-fencing-20261016.md)，主要是三件事：客户端复核与 UI 绑定、robot 冒烟复核、进场链余项 a–e。
> **上游**：[reliable-player-inbox-20261014.md](reliable-player-inbox-20261014.md) §5.2 的第 1 项「落地恢复」与第 4 项「超窗处置与对账」；[cross-zone-scene-travel.md](cross-zone-scene-travel.md) §7（传送窗口内全局推送丢失、客户端落地主动拉）；[guild-phase2/04-asset-channel.md](guild-phase2/04-asset-channel.md)（资产通道不变量 I1–I10）。
> **用户决定（2026-10-14）**：范围含落地恢复、超窗处置与对账；授权改客户端 `../mmorpg-client`；**Java 版只登记待做**（偏离 AGENTS §12「客户端契约两版同批改」，由用户明确决定，见 §11）；资产长期离线采用「停放 + 落地唤醒 + 日对账 + 180 天自动放弃并退回帮会侧限购与帮贡」；战斗结算待办「去掉到期删除」。
> **分工**：进场门禁（登录代次）归「旧入场请求覆盖新会话问题」会话（[scene-enter-session-fence.md](scene-enter-session-fence.md)）；统一玩家收件箱服务归「玩家收件箱服务设计」会话，v1 不接入本摘要（用户拍板 D4），将来接入届时另行拍板（§3.3）；MySQL 行内围栏另见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md)。
> **证据口径**：[C] 读代码确认（工作区，2026-10-14）；[D] 文档记载；[I] 推断。行号会漂移，以函数名为准。

---

## 0. 结论速览

1. **每个玩家一份版本摘要**：核心 Redis 上一个 hash `player:{id}:sync_digest`，字段 `g` 是代次，其余字段是「域号 → 变更计数」。业务在**自己提交成功之后**给受影响的玩家 bump 对应域。摘要只是提示，不是数据；丢了就全量拉一次。
2. **客户端落地后主动取摘要**，不依赖推送：登录、重登、跨 zone 落地完成后发 `GetSyncDigest`，scene 异步读 hash 后推 `NotifySyncDigest`。客户端与本地缓存逐域比对，**只拉变了的域**；`g` 变了或读不到摘要就全量拉。某域拉取成功之后才把该域的缓存版本推进。
3. **场景侧待办落地即唤醒**：scene 在玩家进场完成时向核心 Redis 发布 `assetop:wake <player_id>`；guild / trade 的资产通道收到后立即重投该玩家的 PENDING 行，不再等最长 60 秒的退避。唤醒丢了只退回原来的退避，不影响正确性。
4. **资产待办超窗**：连续找不到玩家后降到约 30 分钟一次（停放档），落地唤醒兜住体感；每日只读对账出报告和指标；PENDING 超过 180 天且玩家仍不在线、账本确认未见时，自动终结为新状态「已放弃」（ABANDONED），按 REJECTED / ABORTED 的对侧账退回帮会侧限购与帮贡。这是对不变量 I7 的**有证明的窄例外**（§6.3）。
5. **战斗结算待办去掉到期删除**：每玩家单槽，记录一直保留到应用为止；新增积压索引，battle 节点定期报积压数与最老年龄；在线玩家战斗冻结到期时 scene 主动补读并应用。
6. **不新建第三套投递器**：摘要只索引现有业务的权威数据；唤醒只加速现有资产通道；结算仍走原发件箱。

---

## 1. 问题与目标

### 1.1 问题

- 跨 zone 传送窗口（最坏约 60 秒，另有 70 秒冻结上限）内，全局服务推给玩家的 S2C 会丢，且不补发 [D]（cross-zone-scene-travel.md §7）。
- 客户端没有统一的落地恢复：组队每次落地都拉；帮会只在打开窗口时拉；好友没有客户端模块，推送到达只打 `[unhandled]` 日志 [C]（`TeamClient.Reset`、`GuildUiRoot.Update`、`HandlerRegistry`）。
- 资产 PENDING 行在玩家离线、交接、刚落地时按退避重投，最长 60 秒一次，没有「玩家到了」的信号 [C]（`go/shared/assetop/decide.go` `NextAttemptMs`）。
- 离线资产行永远按约 60 秒重投，每次还要整份读一遍已落盘账本；商店兑换、活动发奖永不到期；没有告警规则 [C]（`reconcile.go` `ProcessOne`、`deploy/k8s` 只有 4 份告警文件）。
- 战斗结算待办 TTL 7 天，到期由 Redis 静默删除，奖励丢失且没有任何信号 [C]（`settlement_outbox.h` 常量、`battle_room_manager.cpp` 写入 Lua）。

### 1.2 目标（不变量）

| # | 不变量 | 靠什么保证 |
|---|---|---|
| L-1 | 任何一次「客户端可见状态」的变化，只要它发生在玩家下线、传送或断线期间，玩家下次落地后最终能看到 | bump 在提交之后；客户端落地必取摘要；读摘要失败或 `g` 变化时全量拉 |
| L-2 | 客户端缓存的域版本永远不领先于它实际拉到的数据 | 先读摘要、记下版本，再拉域；拉取成功后才推进该域版本（§4.3） |
| L-3 | 摘要丢失、过期、回档只会导致多拉，不会导致漏拉 | 比较用「不相等」；`g` 在 hash 重建时必然变化（§3.2） |
| L-4 | 唤醒只加速，不改变资产通道的任何正确性论证 | 唤醒走与退避重投完全相同的 `Claim + ProcessOne`（§5.3） |
| L-5 | 资产待办不会在玩家仍可能被投递时被放弃 | 放弃只在持租约、无位置、账本未见、距最后一次发送超过安全间隔时进行（§6.3） |
| L-6 | 战斗结算待办不会因为时间流逝而静默消失 | 去掉 TTL；积压索引与定期报数（§7） |

### 1.3 不在范围内

- 进场门禁（登录代次）：另一会话负责。本设计的挂点放在它之后（§5.1）。
- 统一玩家收件箱服务：另一会话负责，v1 不接入本摘要（用户拍板 D4）；将来接入届时另行拍板，下发时必须过滤（§3.3）。
- scene 自己持有的数据（背包、货币、宠物、属性、任务）不进摘要：它们在进场时由 scene 加载为权威数据，客户端在打开面板时拉取（§3.4）。
- 聊天私聊：没有「最近会话」接口，进摘要也无从拉起；保持「7 天历史，按会话拉取」的尽力而为口径。
- 邮件：`go/mail` 尚未落码。邮件 M1 落地时按 §3.3 追加一个域。
- Java 版：只登记待做（§11）。

---

## 2. 选型与理由

| 决策 | 选择 | 否决的方案与原因 |
|---|---|---|
| 摘要存哪 | 核心 Redis（`redis` 实例，DB 0） | C++ scene 只能连这一个实例 [C]（`redis_manager.h`，不发 AUTH/SELECT）；friend / match / guild(PlayerLocatorRedis) / trade(SharedRedis) 都能连 [C]。私有实例（FriendRedis、guild DB 2、match 集群）C++ 读不到 |
| 摘要粒度 | 每个域对应一组现成的拉取 RPC | 一个总版本号会让任何变化触发全量拉；按记录粒度要业务改存储 |
| 版本语义 | 变更计数，按「不相等」比较 | 复用业务自己的 generation：friend / guild 的 generation 在私有 Redis、无 TTL、不覆盖黑名单与申请 [C]；team 的 epoch 只覆盖成员关系 |
| 谁对客户端提供摘要 | scene 的 `SceneSceneClientPlayer` 服务，形态照 `SceneInfoC2S`（Empty 应答 + 推送） | Go 服务经 client_rpc_router：没有现成宿主（player_locator 等被占用），新建服务成本高；塞进 NotifyEnterScene：同 zone 换图也会触发，且要改被占用的进场链 |
| 为什么 Empty + 推送 | player 服务没有异步回包机制 [C]（`ProcessClientPlayerMessage` 同步序列化应答） | — |
| 唤醒通道 | 核心 Redis Pub/Sub，频道 `assetop:wake`，载荷十进制 player_id | 新 Kafka topic：trade 没有 Kafka 依赖，要改被占用的 `k8s_deploy.ps1` 与 topic 预建；scene 直连 guild/trade gRPC：东西向新边，部署白名单不含这些服务。唤醒是提示，Pub/Sub 的「可能丢」可以接受（L-4） |
| 唤醒发出点 | scene 进场完成（`EnterScene` 第 6 步之后）+ 战斗冻结解除 | scene_manager 写位置时：scene 还没加载完，会撞 NOT_HERE / RETRY |

---

## 3. 版本摘要契约

### 3.1 存储

- **键**：`player:{%d}:sync_digest`（Go）/ `player:{%llu}:sync_digest`（C++）。花括号是 hash tag，与同一玩家的其它协调键同槽（[玩家态 Redis 集群化](redis-player-store-cluster.md) §5.2）。
- **字段**：`g` = 代次（hash 创建时刻的 Redis 服务器毫秒）；`"<域号十进制>"` = 该域的变更计数（uint64）。
- **TTL**：30 天，每次 bump 续期。过期即「摘要丢失」，按 L-3 处理。
- **Go 侧单一来源**：新包 `go/shared/playersync`（键格式、字段名、TTL、bump Lua）。C++ 只镜像键格式（`player_landing.h` 的 `kSyncDigestKeyFmt`），由 Go 测试解析 C++ 头文件常量逐字比对（沿用 `owner_epoch_crosslang_test.go` 的 `cppStringConstant` 写法）。不要再新增手工维护的「另有 N 处实现」清单。
- **键族登记**：在 [shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) §2.2 F 族登记「`player:{id}:sync_digest`，核心，可丢，TTL 30 天，不进清库 / 回档 / 合服清单」（参照 `player:{id}:login_generation` 的先例）。该文件当前有他人未提交改动，登记时只追加一行。

### 3.2 bump 脚本

单键 Lua（`playersync.BumpScript`），`KEYS[1]` = 摘要键，`ARGV[1]` = TTL 秒，`ARGV[2..]` = 域号：

```lua
if redis.call('HEXISTS', KEYS[1], 'g') == 0 then
  local t = redis.call('TIME')
  redis.call('HSET', KEYS[1], 'g', tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000))
end
for i = 2, #ARGV do
  redis.call('HINCRBY', KEYS[1], ARGV[i], 1)
end
redis.call('EXPIRE', KEYS[1], ARGV[1])
return 1
```

- `g` 只在 hash 不存在时写入。hash 被删、过期、FLUSHALL 后重建，`g` 必然与旧值不同（服务器时间前进），满足 L-3。
- 比较一律用「不相等」：AOF everysec 最多丢 1 秒，回档可能把旧计数连同旧 `g` 一起恢复，计数可能变小。
- 回档 Go 域权威数据的工具（MySQL 恢复 friend / guild 库等）**必须删掉受影响玩家的摘要键**，否则可能出现「计数恰好相等、数据已变」。现有回档工具都只回档 scene 的玩家数据（不在摘要里），所以 v1 只把这条写进运维手册（§10），不改工具。

### 3.3 域表（客户端可见契约）

proto 枚举 `SyncDomain`，追加在 `proto/scene/player_scene.proto`（与读取 RPC 同文件；该文件已在客户端 `gen_proto.ps1` 白名单里，不需要新登记）。**域号上线后不复用**，只追加。

| 域号 | 枚举 | 客户端拉取 RPC | 谁 bump、给谁 bump |
|---|---|---|---|
| 1 | `SYNC_DOMAIN_FRIEND_LIST` | `ClientPlayerFriend.GetFriendList` | friend：Accept（双方）、Remove（双方，仅当好友边真的被删除）、Block（本人；对方仅当好友边真的被删除） |
| 2 | `SYNC_DOMAIN_FRIEND_REQUEST` | `ClientPlayerFriend.GetPendingRequests` | friend：Add（接收方）、Accept（双方，两向 pending 都会变）、Reject（本人）、Block（本人；对方仅当本人发给对方的 pending 真的被拒） |
| 3 | `SYNC_DOMAIN_FRIEND_BLOCK` | `ClientPlayerFriend.ListBlocks` | friend：Block / Unblock（本人） |
| 4 | `SYNC_DOMAIN_TEAM` | `ClientPlayerTeam.GetMyTeam` | team：每次名册提交后，给新旧成员全集（含操作者）；孤儿修复置空时给本人；纯过期清理不 bump |
| 5 | `SYNC_DOMAIN_TEAM_INVITE` | `ClientPlayerTeam.ListMyInvites` | team：邀请新增 / 撤销时给被邀请人（按时间过期移除的不 bump） |
| 6 | `SYNC_DOMAIN_GUILD` | `GuildService.GetPlayerGuild` | guild：<br>- 创建、加入、离开、解散、公告、职位、踢人、转让、升级：给全帮成员（≤100）与被踢者 / 离开者。<br>- 入会申请新增、撤回、审批：给审批人（长老 / 帮主），因为待审数在 GuildInfo 里；审批通过另给全帮与申请人。<br>- 本人帮贡变化：兑换预留、捐献 / 兑换终结、点灯 / 团圆、历练得奖，只给本人。<br>- 活动给帮会发资金（灯会 / 团圆首次发资金、计资金的历练胜局）：在已有的全帮扇出里顺带 |
| 7 | `SYNC_DOMAIN_GUILD_APPLICATION` | `GuildService.ListMyGuildApplications` | guild：申请、撤回、审批结果（申请人本人）；建帮时本人 |
| 8 | `SYNC_DOMAIN_GUILD_ECONOMY` | `GuildService.GetGuildDonateOptions` / `GetGuildShop` | guild：<br>- 捐献 / 兑换预留成功：本人。<br>- 资产指令终结：本人，**含同步投递路径**。bump 放在 `logic.OnAssetFinalized` 开头，由 `GuildAssetStore.terminate` 在提交、缓存失效之后回调；这里是**唯一出口**，terminate 本身不再加 bump。DONATE / SHOP 终结时同时 bump `GUILD`。活动发奖类指令终结 bump 的是 `GUILD_ACTIVITY`：两个经济页按 kind 把它过滤掉了，变的是活动页的待发数。<br>- 升级：全帮（解锁档位随等级变化）。<br>- `assetopfix` 人工终结不经这条回调，不 bump |
| 9 | `SYNC_DOMAIN_GUILD_ACTIVITY` | `GuildService.GetGuildActivities` | guild：<br>- 点灯、团圆、历练房间建立 / 响应 / 开战 / 结束 / 结算：本人。<br>- 达阈值扇出时：全帮。<br>- 升级：全帮（活动等级门槛随等级变化） |

- 帮会级数值（资金、灯会进度）不**为此专门**逐次扇出 bump。每笔捐献都给 100 人写一遍不值得，由打开面板时的拉取覆盖。这条豁免只针对专门为帮会级数值做的扇出；本人那一次 bump、以及已经在进行的扇出，都照常顺带相关域号（同一段 Lua 多几个 ARGV，不增加 Redis 往返）。
- friend 对被动方的 bump 以 repo 在同一事务里报告的**实际效果**为准（好友边是否真被删、pending 是否真被改）。Block / Unblock 的目标可以是任意 player_id；如果无条件给被动方 bump，客户端循环拉黑不同目标，就能在核心 Redis 上为任意 id 造出 30 天的摘要键。
- 按时间自然过期的状态（组队邀请 60 秒、申请 120 秒、帮会申请过期）**不 bump**：客户端按 `expire_at_ms` 自己算。
- **追加新域的规则**：在枚举末尾追加；在本表追加一行（拉取 RPC、bump 点）；客户端在 `LandingSyncClient` 注册该域的消费者。邮件按此规则在落地时追加，号码届时分配。
- **玩家收件箱 v1 不接摘要**：2026-10-15 用户拍板 D4，见 [player-inbox-service.md](player-inbox-service.md)。它的做法是落地时无条件拉一次，加 Kafka 提醒和有界兜底。将来要接入，接法届时另行拍板：
  - 不能直接按上一条规则追加。摘要会原样下发给客户端，收件箱域的计数一变，就等于提前告诉被 GM 处置的玩家「有处置待执行」。
  - 真要接入，下发时必须把这个域过滤掉。见 player-inbox-service.md §7.14 / §13.3。

### 3.4 为什么 scene 持有的数据不进摘要

背包、货币、宠物、属性、任务在进场时由 scene 从 PlayerAllData 加载，是 scene 进程内的权威状态；客户端在打开面板时拉取，宠物另有 15 秒自动拉取 [C]（`GameClient.Tick`）。把它们放进摘要，需要 scene 在内存提交之后写 Redis，而节点崩溃回档时会出现「版本领先于数据」。落地之后由唤醒触发的资产到账、结算应用都发生在客户端已连接期间，由各业务已有的推送（例如 guild 的 `FUNDS_CHANGED` / `DELIVERY_DONE`）和面板拉取覆盖。残余风险见 §12。

### 3.5 bump 的时序规则（所有调用方必须遵守）

1. **只在提交成功之后 bump**；提交失败不 bump。先 bump 后提交，客户端会把旧数据记成新版本，在下一次变化之前一直看不到这次改动（违反 L-2）。
2. **cache-aside 的服务先失效缓存、再 bump**（friend、guild）。缓存失效失败时，仍然 bump；残余风险与现状相同（缓存陈旧上界 = 缓存 TTL），见 §12。
3. **bump 集合 = 「拉取结果会变的所有玩家」**，包括操作者本人。推送刻意排除操作者，但操作者的应答可能在断线时丢失。
4. **被动方隐私**：Remove、Block、Reject 刻意不推送给对方 [C]（`friend_logic.go`）。给被动方 bump 之后，客户端**不得**把「摘要变化」直接显示成红点或提示；红点只能来自拉回的内容（例如 pending 数）。
5. **bump 失败不影响业务结果**：只记一行 WARN 和低基数指标 `player_sync_bump_total{service,result}`（不带 player_id）。漏掉的一次由打开面板时的拉取兜底。
6. 跨库扇出（guild 全帮最多 100 人）放进与推送同一个异步上下文里做，带总预算（2 秒），不阻塞 RPC 应答。
   - guild 为 bump 单独建了一个 go-redis 客户端：与 PlayerLocatorRedis 同址同库，开 `ContextTimeoutEnabled`，读写超时 2 秒，连接池 4。
   - 不开 `ContextTimeoutEnabled` 时，套接字读写不看 ctx 截止时间，总预算管不住单条命令。
   - 单独的客户端也让 bump 不与在线查询、资产循环抢连接池。

---

## 4. 落地取摘要

### 4.1 协议（追加到 `proto/scene/player_scene.proto`）

```protobuf
// 落地恢复:按域版本摘要(docs/design/landing-recovery-sync-digest.md)。
// 域号是客户端契约:只追加,上线后不复用;hash 字段名 = 域号十进制。
enum SyncDomain {
  SYNC_DOMAIN_UNSPECIFIED = 0;
  SYNC_DOMAIN_FRIEND_LIST = 1;
  SYNC_DOMAIN_FRIEND_REQUEST = 2;
  SYNC_DOMAIN_FRIEND_BLOCK = 3;
  SYNC_DOMAIN_TEAM = 4;
  SYNC_DOMAIN_TEAM_INVITE = 5;
  SYNC_DOMAIN_GUILD = 6;
  SYNC_DOMAIN_GUILD_APPLICATION = 7;
  SYNC_DOMAIN_GUILD_ECONOMY = 8;
  SYNC_DOMAIN_GUILD_ACTIVITY = 9;
}

enum SyncDigestStatus {
  SYNC_DIGEST_STATUS_OK = 0;
  SYNC_DIGEST_STATUS_UNAVAILABLE = 1;  // 读不到摘要(Redis 不可用 / 命令未发出 / 应答畸形):客户端按全量拉处理
}

message SyncDigestEntry {
  SyncDomain domain = 1;
  uint64 version = 2;
}

message GetSyncDigestRequest {
}

message SyncDigestS2C {
  SyncDigestStatus status = 1;
  uint64 generation = 2;                 // 摘要代次 g;0 = 摘要不存在(从未 bump 或已过期)
  repeated SyncDigestEntry entries = 3;  // 只含出现过的域;缺席的域版本按 0 处理。顺序无业务含义
}
```

`SceneSceneClientPlayer` 服务末尾（`TravelToZone` 之后）**追加**两个方法：

```protobuf
  rpc GetSyncDigest(GetSyncDigestRequest) returns (Empty);
  rpc NotifySyncDigest(SyncDigestS2C) returns (Empty);
```

- 消息号由生成器发号（AGENTS §3），手写代码只引用生成常量。
- 限流：负责人 2026-10-16 拍板 `GetSyncDigest` **不加 `MessageLimiter` 行，接受默认档**（`SceneInfoC2S` 自己也不在表里，原定的「同 43 号同档」本就等于默认档）。默认档按会话、按消息号计数，同一消息号在当前与上一整秒内合计最多 3 次（真实时间窗 1~2 秒，持续约 1.5 次/秒），超限计非法包、累计 50 次断线 [C]。客户端单飞加 15 秒起的退避、robot 冒烟 1.1 秒间隔都远在其下。
- 返回 Empty 的 handler 没有应答错误字段。所有失败都推一条 `status = UNAVAILABLE` 的 `NotifySyncDigest`，让客户端结束等待；请求级全局 tip 已移除，不能借下一条非 Empty 请求传递拒绝码。

### 4.2 scene 实现

新系统 `PlayerLandingSystem`（`cpp/libs/services/scene/player/system/player_landing.{h,cpp}`）：

- `static void SendSyncDigest(entt::entity player)`：
  1. Redis 未就绪（`tlsRedis.GetZoneRedis()` 为空或未连接）→ 立即推 `UNAVAILABLE`。
  2. 按值捕获 `playerId` 发 `HGETALL player:%llu:sync_digest`；返回值 `!= REDIS_OK` 时回调不会被调，就地推 `UNAVAILABLE`。
  3. 回调里按 playerId 重新找实体（实体可能已销毁或换人），找不到就丢弃（客户端会超时重试）。
  4. 解析：`g` → generation；其余字段名能解析成已知 `SyncDomain` 值的进 entries，未知字段跳过（新服务端先上、旧 scene 未升级时字段会多于枚举）。应答类型不是数组、字段值不是十进制无符号整数 → `UNAVAILABLE`，打 ERROR。
  5. 推 `NotifySyncDigest`（`SendMessageToClientViaGate`）。
- 解析做成 `player_landing.h` 里的纯函数（入参：字段名/值对的列表），单测只测它。
- handler（`player_scene_handler.cpp` 守护段内）：`GetSyncDigest` 只委托 `PlayerLandingSystem::SendSyncDigest(player)`；`NotifySyncDigest` 照 `NotifySceneInfo` 转发给客户端。
- 实体还在异步载入时，客户端请求会被 scene 丢弃且不回包 [C]（`ProcessClientPlayerMessage`）。客户端在收到 `NotifyEnterScene` 之后才发（§4.3），此时实体一定已建好。

### 4.3 客户端实现（`../mmorpg-client`）

新增纯数据类 `LandingSyncClient`（`Assets/Scripts/Game/Sync/`，不依赖 UnityEngine），由 `GameClient` 构造并在 `Tick` 里驱动：

1. **落地判定**沿用仓库现有的身份观测模式：`(PlayerId, GateConnectionIdentity)` 变化，且 `InGame && IsGateReady` 为真，即一次落地。这一个判据覆盖首登、手动重登（客户端没有自动重连）、跨 zone 重定向的第二条腿 [C]（`GameClient.cs` `EnterGameAndWaitScene`、`RedirectFlow`）。同 zone 换图连接不变，不算落地。
2. **单飞请求**：发 `GetSyncDigest` 之前先置在途标志（`GameClient.Tick` 会在 `Call` 的等待循环里重入 [C]）；等 `NotifySyncDigest`，15 秒超时；失败按 15 / 30 / 60 秒封顶退避。丢弃旧落地迟到回包的实际机制（2026-10-16 按实现订正，原稿的「序号戳」已删）：推送只在「本次落地已发过请求、尚未拿到摘要、连接引用一致」时才被采纳；新连接上无法区分来源的迟到推送，由「已发请求数 > 已收推送数时本次全量拉」兜住；gate 拒绝的请求按「已收一条推送」计数，避免下一次落地无谓全量拉。
3. **比对**：本地缓存按 gateway 与 player_id 分键存于 PlayerPrefs（`mmorpg.syncdigest.{gateway 短哈希}.{playerId}`，只存 `g` 与各域计数，不存 secret）。gateway 命名空间只隔离不同环境；同一 gateway 清库后 player_id 被重新发号，仍靠 `g` 变化触发全量拉。
   - `status = UNAVAILABLE` 或 `generation` 与缓存不同 → 所有已注册域标脏，并清空该玩家的缓存计数（`g` 改记为新值）。
   - 否则逐域比较，计数不等或本地没有记录即标脏。
4. **拉取与推进**：标脏的域通知其消费者拉取；消费者拉取成功后回调 `Ack(domain, versionReadAtLanding)`，此时才把该域的缓存版本写成摘要里的值（L-2）。拉取失败保留脏标，下次落地或打开面板时再拉。
5. **消费者**（第一期）：
   以下为 2026-10-16 按实现订正后的现状（接线由 `LandingSyncBindings.Bind*` 完成，三个 UI 宿主 `TeamUiRoot` / `SocialUiRoot` / `GuildUiRoot` 在创建域客户端时 Bind、销毁前 Dispose）：
   - `FRIEND_REQUEST`：复用现有 `FriendClient`（没有新增 `FriendInboxClient`），标脏时排队拉 `GetPendingRequests`，成功后 Ack。界面上目前没有 pending 数角标，后台拉回的数据要等玩家打开申请窗口才看得到；加「只来自拉回内容」的角标属于另立项。`FRIEND_LIST` / `FRIEND_BLOCK` 暂无客户端面板，不注册消费者（这两个域每次落地都会是「脏」的，代价为零，因为没人拉）。
   - `GUILD` / `GUILD_APPLICATION`：后台主动拉（窗口隐藏、非战斗时也消费）。`GUILD` 拉 `GetPlayerGuild`；这次读取同时为 `GUILD_APPLICATION` 取票，结果是「在帮」时一并确认（入帮即删光本人申请），不在帮时由 `ListMyGuildApplications` 确认。
   - `GUILD_ECONOMY` / `GUILD_ACTIVITY`：**只被动确认**——不主动拉，玩家进经济页、活动页时的自动拉取顺带 Ack；`ECONOMY` 要捐献选项与商店两次读取都在摘要之后成功才确认。理由：窗口外没有任何显示，后台拉只增加请求，且 `GuildClient.Request` 的传输失败会让客户端进入需重登的隔离态。
   - `TEAM*`：`TeamClient` 已在每次落地拉取 [C]，只做被动确认（读取发出晚于摘要且成功时 Ack），不改其触发方式。
6. **测试**：`Assets/Tests/EditMode/Net/LandingSyncClientTests.cs`，自带假 transport（照 `TeamFakeTransport`）与内存 store：首登全拉、`g` 变化全拉、只拉变化的域、拉取失败不推进、连续两次落地丢旧回包、摘要失败退避、`UNAVAILABLE` 全拉、身份变化复位。

### 4.4 先读后拉的竞态

客户端先读摘要得到版本 v，再拉域数据。若业务在两步之间提交并 bump 到 v+1：客户端拉到的是新数据，但缓存记的是 v；下次落地时 v ≠ v+1，再拉一次。最坏多拉一次，不会漏（L-2）。

---

## 5. 场景侧待办唤醒

### 5.1 挂点

`PlayerLifecycleSystem::EnterScene` 末尾、`PlayerBattleSystem::OnPlayerEnterScene` 之后，加一行 `PlayerLandingSystem::OnEnteredScene(player, enterInfo.enter_gs_type());`，写法照 `PlayerTeamSystem::OnEnteredScene` 那种一行系统钩子。

- 三条路径都经过这里：新载入（首登、跨节点换图、跨 zone 落地、重登）、复用实体（同节点重连、顶号、同节点换图）、gate 补发的重复路由 [C]。入口栅栏拒绝或场景不存在时不会到达。
- 不依赖 `enter_gs_type` 的具体取值（跨 zone 第二条腿是 RECONNECT 还是 REPLACE，代码与文档说法不一 [C]）。
- `player_lifecycle.cpp`、`scene.vcxproj` / `.filters`、`CMakeLists.txt` 正被进场门禁会话修改。**这一行与新文件的工程登记等它落地后再加**（批次 D3，§9）。
- 统一收件箱服务若落地，它在 scene 侧的消费触发挂在 `OnEnteredScene` 里（由该会话提供一行调用）。

### 5.2 发布

`PlayerLandingSystem::OnEnteredScene` 调 `PublishAssetWake(player)`：

- 实体带 `PlayerFrozenComp`（交接在途）时跳过：资产写路径此时本来就会拒绝；交接结果若是销毁实体，新节点进场时会再唤醒；若是解冻，则退回原退避（≤60 秒）。
- 合并窗口：同一实体 5 秒内只发一次，只对进场来源生效（纯运行时组件 `PlayerLandingWakeComp{ uint64_t lastWakeMs; }`，不入库，单调时钟）。目的是同 zone 频繁换图、gate 补发重复路由时避免刷屏。
- 命令：`PUBLISH assetop:wake <player_id 十进制>`，经 `tlsRedis.GetZoneRedis()->command(...)`，回调只在出错时打 WARN。
- 额外发出点：战斗冻结解除，覆盖 `kAssetInBattle` 的 RETRY [C]（`asset_op_system.cpp` 资产 RPC 在战斗中回 RETRY）。
  - 实现挂在 `player_battle.cpp` 的 `RemoveInBattleComp`，这是摘 InBattleComp 的唯一入口。最常见的解冻是结算应用，走的是 `ReleaseFreezeKeepLock`，并不经过 `ClearBattleFreeze`；挂在唯一入口上才不会漏。
  - 这一来源以 `AssetWakeReason::kBattleFreezeCleared` 发出，不受合并窗口限制。典型场景是重连复用实体时玩家仍在战斗中：进场唤醒撞上 RETRY，几秒后战斗结束。如果解冻唤醒被同一个窗口吞掉，它就失去了唯一要覆盖的场景。

### 5.3 Go 侧接收（`go/shared/assetop` + 新包 `go/shared/assetopwake`）

- `assetop` 增加两个**可选**能力接口，不改 `Store` 主接口（避免两个实现被强制同改）：
  - `PlayerPendingLister`：`ListPendingByPlayer(ctx, playerID uint64, limit int) ([]uint64, error)`，返回该玩家 PENDING 行的 op_id（guild 走 `idx_guild_asset_op_2`，trade 走 `idx_trade_asset_op_1`，非锁读）。
  - `Loop.WakePlayer(ctx, playerID)`：列出 op_id（≤32），对每条走与退避重投**完全相同**的 `Claim` CAS + `ProcessOne`。`Claim` 只看 status 与 lease，不看 `next_attempt_ms` [C]，所以不需要改 `next_attempt_ms`，也避开 TiDB 下修改 idx_0 列必须先加锁的规则。并发并入 Loop 现有的 Workers 预算。
- 新包 `assetopwake`（依赖 go-redis，`assetop` 包按约定不引 Redis 客户端）：
  - 订阅 `assetop:wake`，解析 player_id；
  - `SET assetop:wake:{service}:{player_id} 1 NX EX 5` 做跨副本去重，每次落地每个服务只有一个副本去查 MySQL；
  - 拿到的副本在独立超时内调 `Loop.WakePlayer`。超时默认 10 秒，配置下界是 2 × `OpBudget`（默认 OpBudget 2.5 秒）。只比 `OpBudget` 略大不够：WakePlayer 先要列一次行（一次库往返，≤1 秒），之后剩余预算不足一整个 `OpBudget` 就一行也领不到，静默退化成从不唤醒。父 ctx 只决定「还要不要开始下一行」：剩余预算不足一个 `OpBudget` 就不再领取，已经开始的行不会被中途截断。没轮到的停放行，最多再等一个 `OfflineBackoff`；
  - 有界并发，默认 4，超出直接丢（唤醒只是提示）；
  - go-redis 的 PubSub 自动重连，重连期间丢的唤醒退回原退避。
- 指标：`assetop_wake_total{service,result}`，result ∈ received / deduped / dropped / woke / none / error。
- 正确性：唤醒路径没有任何新的状态迁移；`ProcessOne` 的结局与退避重投路径一致。唤醒到达时玩家若仍在加载、战斗中、冻结，scene 回 RETRY，按原有退避重排（L-4）。

---

## 6. 资产待办超窗处置与对账

### 6.1 停放档（长期离线降频）

- 判据：本次结局是本地合成的 NOT_HERE，且**位置键不存在**（`Result.Absent`，即 `ErrNotOnline`），且 `Attempts ≥ OfflineAfterAttempts`（默认 5）。
  - 账本读到未见、没有 blob、读失败，都照样停放。
  - 位置记录存在但当前没有持有者的，不停放，保持正常退避（≤60 秒）：等待落点、节点未注册或有歧义、镜像未同步。这几种都是短暂态，玩家很快就会出现在某个节点上。
- 处置：`next_attempt_ms = now + OfflineBackoff × (0.8 + 0.4 × rnd)`，`OfflineBackoff` 默认 30 分钟。
- 实现：`decide.go` 的 `NextAttemptMs` 增加离线档纯函数分支；`ProcessOne` 把「本地 NOT_HERE」这个事实传给它（`Result.Local` 已有 [C]）。
- 玩家体感不变：上线即被 §5 唤醒。
- 配置：guild 的 `AssetOpConf` 与 trade 的 reconcile 配置各加 `OfflineAfterAttempts`、`OfflineBackoffMs`，`Validate` 同形约束（`OfflineBackoffMs ≥ MaxBackoffMs`）。

### 6.2 日对账（只读）

`assetop` 新增可选能力接口 `PendingClassCounter.CountPendingByClass(ctx, nowMs, abandonBeforeMs)`，由 `Loop.AuditOnce` 驱动：每 `AuditInterval`（默认 24 小时）扫一遍 PENDING 行，按类别计数并打一行汇总日志，**不改任何行**。Store 实现必须用 `assetop.ClassifyPending`（或 `PendingTally`）分类，不另写一套口径。传输失败的那次投递按 `RETRY`、原因码 0 落库，归 `online_retry`；否则对账那一刻只要 scene 短暂不可达，`alert` 类就会误报一整天。

| 类别 | 判据 | 用途 |
|---|---|---|
| `online_retry` | 最近一次结局不是本地 NOT_HERE，且不是告警类 | 正常重试中；最老年龄告警只看这一类 |
| `offline` | 最近一次结局是本地 NOT_HERE | 趋势观察 |
| `alert` | UNKNOWN、结局翻转、毒行 | 立即告警 |
| `abandon_candidate` | 满足 §6.3 的时间条件 | 由 §6.3 处理，计数供观察 |

- 指标：`assetop_pending_rows{service,stream,class}`（gauge）、`assetop_pending_class_oldest_age_seconds{service,stream,class}`（gauge，按类拆分）。原有的 `assetop_pending_oldest_age_seconds{service,stream}` 保留不动；同一个指标名不能有两套 label，所以按类拆分的 gauge 必须用新名字。
- guild 与 trade 都运行；多副本时用 `go/shared/leader` 选一个副本执行，其余副本不报这组 gauge。

### 6.3 自动放弃（ABANDONED）

用户决定：PENDING 超过 180 天、玩家仍不在线时自动终结，并按 REJECTED / ABORTED 的对侧账退回帮会侧限购与帮贡。

**放弃条件（全部满足，逐行判定）**：

1. `status = PENDING` 且 `created_ms ≤ now − AbandonAfterDays`（默认 180，最小 30）。
2. 以普通 `Claim` CAS 拿到租约（与重投循环互斥）。
3. 重新解析位置：**没有位置**（玩家不在任何节点）。有位置就释放租约，交给正常流程。
4. 距该行最后一次**可能**向 scene 发送请求的时刻已超过 `AbandonSafetyGap`（默认 1 小时，下限 max(10 分钟, Lease)）。gRPC 请求的生命期以秒计，所以此时不存在在途请求。判据是新增列 `last_send_ms`，见下面的「last_send_ms 契约」；**不能用 `updated_ms`**：Claim 本身会写 `updated_ms`，停放档每约 30 分钟的本地 NOT_HERE 重排也会写，结果 `updated_ms` 永远新于 1 小时，放弃永远不会发生。
5. 读已落盘账本：
   - 已见（APPLIED / REJECTED）→ 按已见结局走正常 `Finalize`，不放弃。
   - 未见 → 进入第 6 步。
   - 读失败 → 本轮跳过，释放租约。
6. 以 `(op_id, status=PENDING, lease_token)` 为条件，在一个事务里终结为 `ABANDONED`，并做对侧账：
   - `GUILD_DEBIT`（捐献）：同 ABORTED，退回当日捐献次数，资金与帮贡不入账。
   - `GUILD_CREDIT` 的商店兑换：同 REJECTED，退回帮贡、退回限购占用（`used_count`）。
   - `GUILD_CREDIT` 的活动发奖、历练奖励：没有可退的东西，奖励作废，审计行留痕。
   - trade：对侧账尚未实现（P3 TODO）[C]，v1 只终结并留痕。
     - **2026-10-20 修复**（第一轮审计 §2.5 go-asset-services#1）：交易合同的扣款腿（`ref_kind=ORDER`）不等这条 TODO。合同引擎读腿结局时只看 `status`，不看 `durable`（`CommerceRepo.LegResult`）。读到 `ABANDONED` 按「没扣到」走与 scene 判拒同一条补偿，在合同自己的事务里做，不在 `Abandon` 的事务里。人工终结同理，详见 [jubaozhai-market.md](jubaozhai-market.md) §15.3。TODO 只剩旧 P1 商品（`ref_kind=LISTING`）。

**为什么这是 I7 的窄例外而不破坏它**：I7 禁止 Go 自判中止，理由是「scene 没记账，就不能保证晚到的请求不会再应用它」。在上述条件下：

- 对这条 seq 发请求的只有持租约的 Loop；
- 第 4 条保证此前所有请求都已超时；
- 第 2 条保证放弃期间不会再发。

所以 scene 不可能再收到这条 seq。

**last_send_ms 契约**（实现以 `go/shared/assetop/abandon.go` 文件头为准；guild / trade 的 Store 必须照此维护）：

`last_send_ms` 表示「此前任何一次投递请求**可能**发出的最晚时刻」。它必须同时满足：
- **安全**：不小于任何一次真实发送的时刻，包括发完请求就崩溃、没来得及重排的那一次；
- **活性**：本地 NOT_HERE、放弃方自己的领取与归还都不推进它。

Store 按下面几条维护这一列：

1. 插行时写 `created_ms`：插行即持租约，同步投递紧接着就可能发出。
2. Claim 的那条 CAS 里，**先于**改写 `lease_until_ms`，写 `last_send_ms = GREATEST(last_send_ms, lease_until_ms)`。上一个租约若没有正常收尾，`lease_until_ms` 仍是那个已过期的非零值，折进来就盖住了那次发送。回读时填 `Op.LastSendMs` 与 `Op.CreatedMs`。
3. Reschedule 写 `last_send_ms = IF(!res.Local, GREATEST(last_send_ms, now), last_send_ms)`。传输失败、坏流号这类非 Local 的结果，一律按「发过」处理。
4. 毒行推迟与 `ReleaseLease` 只清租约，不动 `last_send_ms`。
5. **存量行懒初始化**，不做单独的回填迁移。新增列的默认值是 0。Claim 的 CAS 里，`last_send_ms = 0` 时先取 `GREATEST(created_ms, updated_ms)` 作初值，再按第 2 条与 `lease_until_ms` 取较大值。
   - 这个初值偏保守：此前每一次发送之后都有一次写 `updated_ms` 的重排或终结；崩溃没收尾的那次由 `lease_until_ms` 覆盖。
   - 保守的代价只是让放弃晚一个安全间隔。
   - 放弃路径读到 `last_send_ms = 0`（Store 没维护）时一律跳过，计 `not_candidate`，绝不拿 0 去算间隔。

**Abandoner 接口**有三个方法：
- `ListAbandonCandidates`：非加锁读，按 op_id 翻页；
- `Abandon`：带令牌条件的终结加对侧账，同一事务；`RowsAffected == 0` 必须回 `ErrLeaseLost`；
- `ReleaseLease`：只清租约。

**滚动升级**：混版窗口内，旧实例的发送不推进 `last_send_ms`。所以新二进制全量上线之前，guild / trade 的配置里不要开启放弃（`AbandonAfterDays = 0`）；全量之后再设为 180。本项目尚未上线，首发时直接开启即可。

**seq 窗口的后果**：放弃一条 scene 从未见过的 seq，与人工终结相同，会拉开 `next_seq` 与 scene `max_seq` 的距离 [D]（04 §4.29：累计超过 511 时新 seq 被判 kJumpTooFar，按 §4.31 抬纪元）。每个 (玩家, 流) 的未决数上限是 16（MaxPending）[C]，一次放弃最多拉开 16，远低于 511。手册中仍写明这一条。

**状态与展示**：
- `assetop.Status` 追加 `StatusAbandoned`（平铺名字表与编译期长度断言同改）。
- `guild_db.proto` 的 `GuildAssetOpStatus` 与 `trade_table.proto` 的 `TradeAssetOpStatus` 各追加 `ABANDONED = 6`。这两个是库内状态，不是客户端契约。
- guild 的客户端视图把 ABANDONED 映射为已有的 `GUILD_ASSET_ORDER_STATUS_ABORTED`（「超时或人工撤销」），不新增客户端可见枚举，也不新增 tip 码。
- 终态清理（30 天后删）把 ABANDONED 与其它终态同等对待；APPLIED_PARTIAL 仍不自动清理。

**执行者**：与 §6.2 同一个 leader 副本，每 `AbandonInterval`（默认 1 小时）分页处理，每轮上限 200 行。
- 指标：`assetop_abandoned_total{service,stream}`、`assetop_abandon_skipped_total{service,reason}`。reason 取值如下（实现见 `abandonSkipNames`）：
  - `located`：玩家有位置；
  - `locate_error`：定位失败；
  - `recent`：距最后一次可能的发送不足安全间隔；
  - `ledger_seen`：账本已见，转正常 Finalize；
  - `ledger_error`：读账本失败；
  - `ledger_unknown`：账本给不出「确定未见」，例如纪元更新、滑出窗口、无 blob、损坏；
  - `lease_lost`：租约被接管；
  - `store_error`：存储出错；
  - `not_candidate`：复核后不再满足条件，或 `last_send_ms` 未维护。

### 6.4 trade 补齐

trade 的资产通道默认关闭、P3 未开工，但为避免两份实现分叉，本批与 guild 同步补齐：
- 挂 `Ledger`（`DataServiceLedger`）；
- 终态行清理（保留 30 天，对齐 guild）；
- 唤醒订阅；
- 停放档；
- 对账；
- 放弃。

开关仍是 `AssetOp.Enabled`。

### 6.5 告警（新文件 `deploy/k8s/asset-channel-alerts.yaml`）

格式照 `deploy/k8s/owner-epoch-alerts.yaml`：

| 规则 | 级别 |
|---|---|
| `increase(assetop_unknown_total[10m]) > 0` | critical |
| `increase(assetop_outcome_flip_total[10m]) > 0` | critical |
| `increase(assetop_partial_total[1h]) > 0` | warning |
| `increase(assetop_store_errors_total{op="decode"}[1h]) > 0` | warning |
| `max(assetop_pending_class_oldest_age_seconds{class="online_retry"}) > 3600` 持续 10m | warning |
| `max(assetop_pending_rows{class="alert"}) > 0` 持续 30m | warning |
| `increase(assetop_abandoned_total[1d]) > 0` | info（审计提醒） |
| `max(guild_trial_result_overdue) > 0` 持续 30m | warning |
| `guild_trial_owed_rows > 1000` 持续 1h | warning |

battle / scene 进程没有 Prometheus 端点 [C]，结算类信号只有结构化日志（`metric=battle_settlement_*`）。Loki ruler 尚未接线 [C]，所以在文件头注释里写出对应的 LogQL，作为值班固定查询，等上线批接线后转规则。

---

## 7. 战斗结算待办：去掉到期删除

- **写入**（battle 的结算写入 Lua、scene 在实体不在时的写入）：
  - 去掉 `EX`，记录不再过期；
  - 同一段 Lua 里 `ZADD battle:settlement:pending:index <now_ms> <player_id>`（积压索引）。
  - 两处写入的 TTL 常量一并删除（battle 与 scene 各一处，原注释要求两处同值）。
- **销账**（scene 条件删除 Lua）：同一段 Lua 里 `ZREM battle:settlement:pending:index <player_id>`。
- **滚动升级兼容**：
  - 旧版写入仍带 TTL，7 天后照旧过期，行为与今天相同；
  - 新版写入不过期；
  - 旧版 scene 销账不清索引，索引里会留下孤儿，由下面的报数任务清理。
- **报数**：battle 节点在首次结算后装上 60 秒定时器（只捕获 `weak_ptr<自己>`）。每轮做三件事：
  - 取一批索引成员。窗口在「头部」与「轮转游标」之间交替：偶数轮查头部，奇数轮按游标后移，扫到底绕回开头。对每个成员，用一段 Lua 原子地完成 `EXISTS battle:settlement:pending:id:<pid>`，不存在就 `ZREM`（孤儿）。
    - 只查最老 10 条不够：记录不再过期之后，再也不上线的玩家会永久占住索引头部，排在后面的滚动升级孤儿就永远查不到。
  - 清理完之后再读 `ZCARD` 与最老分数。这样头部的孤儿不会抬高年龄。
  - 打一行 `metric=battle_settlement_pending_backlog count=<n> oldest_age_s=<s> orphans_removed=<k>`。

  多个 battle 节点会重复报同一个全局数，无害。从未结算过的空闲节点不报。
- **索引分数**：取写入方节点的墙钟毫秒，不在 Lua 里读 Redis TIME，脚本保持确定性。节点间秒级的墙钟偏差，对以天计的年龄指标没有影响。
- **单槽语义不变**：同一时间每个玩家最多一局待结算，下一局的写入会覆盖。靠战斗锁一直保留到落盘销账来保证 [C]。去掉 TTL 不改变这一点；内存上界 = 有待结算的玩家数 × 一份结算 blob。
- **在线玩家补读**：reaper 判定 FIGHTING 到期、只解冻时，对仍在线的实体顺手 GET 一次待结算记录，命中就走 `ApplyPendingSettlement`（与进场补应用同一函数）。今天这类奖励要等下一次 EnterScene 才补 [C]。
- **用尽日志分两类**：`metric=battle_settlement_undelivered` 增加 `reason=no_target|not_acked`，便于设阈值（离线玩家 120 秒后必然出现 `no_target`）。
- **删号**：`DeletePlayerData` 目前没有生产调用方 [C]。删号流程落地时要一并删 `battle:settlement:pending[:id]:{pid}` 与索引成员，并关闭该玩家在 guild / trade 的 PENDING 行（走 §6.3 的放弃路径）。这一条登记为删号设计的输入，本批不改 data_service（该模块有他人在途改动）。

---

## 8. 帮会活动结果持久投递

链路已基本闭环：落库 → Kafka → 重发 → 巡检器兜底 → EXPIRED [C]。本批只补 §6.5 中 `guild_trial_*` 的告警，并在运维手册写明「guild 停机超过 7 天的胜场不补发」这一已接受的偏差。

---

## 9. 改动清单与批次

所有改动遵守 AGENTS §3：不手改生成物；生成式 handler 只在守护段内写。proto 生成、导表、编译、测试由 Codex 执行（§10）。

| 批次 | 内容 | 主要文件 | 前置 |
|---|---|---|---|
| A | 摘要包 + friend / team bump | 新：`go/shared/playersync/{digest.go,gozero.go,goredis.go,digest_test.go}`；改：`go/friend/internal/logic/friend_logic.go`（+测试）、`go/friend/internal/svc/servicecontext.go`、`go/match/internal/team/`（提交与推送点）、`go/match/internal/svc/servicecontext.go` | proto（批 D1）生成后才能编译 |
| B | guild bump | `go/guild/internal/logic/{guild_logic,guild_manage_logic,economy_logic,activity_logic,activity_trial,push}.go`、`go/guild/internal/svc/servicecontext.go` | 同上 |
| C1 | 资产通道共享层 | `go/shared/assetop/{types,decide,reconcile,metrics}.go`（+测试）；新：`go/shared/assetopwake/`、`assetop/audit.go`、`assetop/abandon.go` | 无 |
| C2 | guild 资产接入 | `go/guild/internal/data/asset_store.go`：<br>- 实现 `PlayerPendingLister` / `PendingClassCounter` / `Abandoner`（含 `ReleaseLease`）；<br>- 按 §6.3「last_send_ms 契约」改插行、Claim（含懒初始化与回读 `CreatedMs` / `LastSendMs`）、Reschedule、毒行推迟的 SQL；<br>- 放弃的对侧账同 REJECTED / ABORTED。<br><br>其他文件：<br>- `proto/guild/guild_db.proto`：追加 `ABANDONED = 6` 与 `last_send_ms` 字段；<br>- `go/guild/internal/{config/config.go,svc/asset_op.go}`：停放 / 放弃 / 对账配置，Locator / Ledger，leader 选举下的 `RunMaintenance`，`assetopwake` 订阅；<br>- `go/guild/guild.go`：上述接线，**以及 B 批遗留的 `logic.WithSyncBumper(logic.NewGuildSyncBumper(svcCtx.PlayerSyncBumper))`**，不接这一行 B 批的 bump 全不生效；<br>- `go/guild/etc/guild.yaml`；<br>- 客户端视图把 ABANDONED 映射为 ABORTED；<br>- `go/guild/cmd/assetopfix/main.go`：说明文字同步「传输失败按 RETRY 落库」。<br><br>**不在 terminate 里加 bump**：B 批的 `OnAssetFinalized` 是唯一出口 | C1 |
| C3 | trade 资产接入 | - `go/trade/internal/data/asset_op_repo.go`：同 C2 的三个接口与 last_send_ms 契约，加终态清理（30 天）；<br>- `go/trade/internal/reconcile/pipeline.go`：挂 Ledger，接唤醒 / 停放 / 对账 / 放弃；<br>- `go/trade/internal/svc/assetchannel.go`；<br>- `go/trade/internal/config/config.go`；<br>- `go/trade/etc/trade.yaml`；<br>- `proto/trade/trade_table.proto`：`ABANDONED = 6` 与 `last_send_ms` | C1 |
| A2 | friend 被动方 bump 收紧 | - `go/friend/internal/data/{block_repo.go,friend_repo.go}` 及其 MySQL 测试：Block 在同一事务里返回实际效果（好友边是否真删、本人发出的 pending 是否真被拒），RemoveFriend 返回是否真删；<br>- `go/friend/internal/logic/friend_logic.go`：按效果给被动方 bump，删掉删除前的预读 `listedAsFriend`；<br>- 对应单测 | A |
| D1 | scene 摘要读取与唤醒发布 | `proto/scene/player_scene.proto`；新：`cpp/libs/services/scene/player/system/player_landing.{h,cpp}`、`cpp/libs/services/scene/player/comp/player_landing_comp.h`；`cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp`（守护段）；新测试工程 `cpp/tests/player_landing_test/` | 无 |
| D2 | 战斗结算保留期 | `cpp/libs/services/battle/settlement/settlement_outbox.h`、`cpp/nodes/battle/logic/battle_room_manager.{h,cpp}`、`cpp/libs/services/scene/battle/system/player_battle.{h,cpp}` | D1 的 `PublishAssetWake` 声明 |
| D3 | 挂点与工程登记 | `player_lifecycle.cpp` 一行、`cpp/libs/services/scene/{scene.vcxproj,.filters,CMakeLists.txt}`、`game.sln`（测试工程） | **进场门禁会话落地之后** |
| E | 客户端 | `../mmorpg-client`：新 `Game/Sync/{LandingSyncClient,SyncDigestStore}.cs`（`FriendInboxClient` 未做，复用 `FriendClient`，见 §4.3 第 5 条）；2026-10-16 补三个 UI 宿主的 Bind 与 `BindGuild`；改 `GameClient.cs`（构造 + Tick + 推送注册）、`Game/Guild/GuildClient.cs`、`UI/Ugui/Guild/GuildUiRoot.cs`、`Game/Team/TeamClient.cs`（Ack）；新测试 | D1 生成之后才能编译 |
| F | robot 冒烟 | 新：`robot/sync_smoke_scenario.go`（+纯函数测试）、`robot/etc/sync_smoke.yaml`、两个 handler 桩；改：`robot/main.go`、`robot/config/config.go`、`robot/README.md` | D1 生成之后 |
| G | 告警与文档 | 新：`deploy/k8s/asset-channel-alerts.yaml`；改：本文、`guild-phase2/04-asset-channel.md`（追加唤醒 / 停放 / 放弃）、`shared-redis-split-by-purpose.md` §2.2（追加键族）、`reliable-player-inbox-20261014.md`（追加入口）、`docs/README.md`、`docs/PROGRESS.md`；Java 仓 `PARITY.md` 登记待做 | — |

**与其他会话的冲突**：
- **进场门禁会话**：占用 `player_lifecycle.*`、`scene.vcxproj/.filters`、`scene_handler.cpp`、gate 进场链、4 个 proto。D3 必须等它落地。
- **client-request-ordering 会话**：计划改 guild logic（E2）与 match logic。B 批动手前重新 `git status`，有改动就先协调。
- **proto 生成**：全量运行，会把他人未提交的 proto 改动一起生成进产物。运行时机由 Codex 与各会话协调（§10）。

---

## 10. 验证（交 Codex 执行）

顺序：proto 生成 → 导表 → Go → C++ → 客户端 → 联机冒烟。C++ MSBuild 一律串行 `/m:1`。

1. **proto 生成**（仓库根）：
   - 按 AGENTS.md §4.1：先跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`（生成器源码有变化，或还没有工具时），再跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。
   - `cd go && build.bat` 只重生成 db / login 的 go-zero 包装，不能代替完整的 proto 生成。
   - **不要用 `dev.bat proto`**：它失败时会 `pause` 卡住，见 scene-enter-session-fence.md §11。
   - 本批已获客户端授权，所以用默认配置（会写同级客户端仓），把新 handler 写进 `../mmorpg-client` 的 `Net/Generated/`，这是 E 批需要的。
   - 只想重生成服务端时，加 `-ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
   - 期望：
     - `proto/message_id.txt` 末尾新增 `SceneSceneClientPlayerGetSyncDigest`、`SceneSceneClientPlayerNotifySyncDigest` 两个号，原有号不变；
     - `player_scene_handler.{h,cpp}` 出现两个新方法，守护段内的手写代码原样保留、不重复；
     - guild / trade 的 db proto 生成物出现 ABANDONED 与 `last_send_ms`；
     - `go/proto/scene/player_scene.pb.go` 出现 `SyncDomain_SYNC_DOMAIN_*`。
2. **限流表**：不需要（2026-10-16 拍板接受默认档，见 §4.1），不跑导表器。
3. **Go**（各模块目录，`GOTOOLCHAIN=local`）：
   - `go/shared`：`go test ./playersync/... ./assetop/... ./assetopwake/...`
   - `go/friend`：`go test ./internal/logic/...`
   - `go/match`：`go test ./internal/team/...`
   - `go/guild`：`go test ./internal/...`
   - `go/trade`：`go test ./internal/...`
   - 以上各模块再跑 `go vet ./...`。
   - 通过标准：原有用例数不减且全绿；新增用例全绿。新增的关键用例：
     - bump Lua 起种 / 递增 / 续期，以及过期后 `g` 改变（miniredis）；
     - Go 与 C++ 键格式逐字一致；
     - 唤醒去重；
     - 停放档；
     - 放弃的 6 个条件逐条反例；
     - 对账分类。
4. **C++**：
   - `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`；
   - 再串行编 `cpp/tests/player_landing_test/player_landing_test.vcxproj`（D3 登记之后）；
   - 跑 `player_landing_test.exe`（摘要解析）与现有 `bag_test.exe`（结算用例），不带过滤器全量跑。
5. **客户端**（`D:\luyuan\mmorpg-client`）：
   - `tools/gen_proto.ps1 -ProtoRoot D:\luyuan\xuanming-server-mmo-cpp`；
   - `tools/gen_messageids.ps1 -ProtoRoot D:\luyuan\xuanming-server-mmo-cpp`；
   - `tools/client_compile_check.ps1`；
   - Unity `-batchmode -runTests -testPlatform EditMode -testFilter LandingSyncClient`。
   - 新文件的 `.meta` 由 Unity 生成后一并提交。不要夹带工作区里他人的资源与 ProjectSettings 改动。
6. **联机冒烟**：`dev-start` 之后：
   - 跑 `robot -c etc/sync_smoke.yaml`，期望输出 `SYNC_SMOKE_OK`。场景：登录取摘要 → 好友申请让对方某域 +1 → 对方再取只变该域 → 短重连后 `g` 不变；
   - 再跑既有 `friend-smoke`、`travel-smoke` 回归。
   - 双 zone 环境（`dev-start-zones -Zones 1,2`，跑 `travel-smoke` 时本来就要起）下，用一份 `sync_smoke.cross_zone=true`、`visit_zone=2` 的配置副本再跑一次，期望 `SYNC_SMOKE_OK ... cross_zone=checked`；前提是 `robot_9911` 首次在 `zone_id` 建角。不要直接改受跟踪的 `etc/sync_smoke.yaml`。
7. **资产唤醒**（需 guild 资产通道开启）：
   - 机器人离线时发起一笔兑换，行进入 PENDING；
   - 机器人登录后 5 秒内终结，日志可见 `assetop_wake_total{result="woke"}`。

失败时保留：生成日志、`go test -json` 输出、MSBuild 日志、Unity 测试 XML、冒烟日志。

---

## 11. 双版本（AGENTS §12）

- **客户端可见契约变化**：`player_scene.proto` 新增 `SyncDomain`、`SyncDigestStatus`、`SyncDigestEntry`、`GetSyncDigestRequest`、`SyncDigestS2C` 与两个 RPC（限流沿用默认档，不加配置行）。
- **用户决定 Java 版只登记待做**。这偏离了「客户端契约两版同批改」。代价是：在 Java 版补齐之前，连到 Java 服的客户端发 `GetSyncDigest` 会得不到 `NotifySyncDigest`，超时后按「摘要失败」退避并全量拉。功能退化为每次落地全量拉，不会出错。客户端第 2 步的超时与退避就是这条退化路径。
- 在 Java 仓 `PARITY.md` 登记一行：本批提交 ↔ Java 待做，写明上面的退化行为。
- 服务端内部改动（唤醒、停放、放弃、结算保留期、告警）不涉及契约，Java 版按自己的存储实现。ABANDONED 不透出到客户端。

---

## 12. 残余风险

1. **在线期间推送丢失**：玩家已连接时某域推送丢了，要等下一次落地或打开面板才恢复。第一期不做在线摘要推送（YAGNI），按需追加。
2. **缓存失效失败**：friend 失效失败只打日志，缓存最多陈旧 30 分钟 [C]。摘要触发的拉取可能拿到陈旧缓存，并把新版本号记上去。修法是让失效可靠，或拉取绕缓存，不在本批。
3. **Go 域回档**：回档 friend / guild 库而不删摘要键，可能出现「计数相等、数据不同」。v1 只写进运维手册。
4. **scene 域不进摘要**：唤醒后到账的资产只靠业务推送与面板拉取刷新；客户端断线重连后丢弃迟到的 `NotifyBattleEnd` [C]，结算对背包的影响要靠打开面板拉取。
5. **唤醒丢失**：Pub/Sub 断线期间的唤醒丢失，退回原退避（在线 ≤60 秒；停放档 ≤30 分钟）。停放档下这段延迟玩家可感知，可接受。
6. **放弃的不可逆性**：180 天后放弃的奖励作废，不会自动补偿（邮件 M2 之前也无法补发）。审计行和 `assetop_abandoned_total` 是人工补偿的依据。
7. **单槽覆盖**：玩家连续两局都未应用时，第二局的写入会覆盖第一局。这依赖战斗锁保证「同一时间只有一局」，去掉 TTL 不改变这一前提。
8. **帮会申请的连带清理不 bump 其他帮**：两类连带清理都不给其他帮会的审批人、也不给申请人 bump。这些人的待审数和申请列表要等打开面板才刷新。data 层不返回这两个受影响集合，本批也不改 data 层。
   - 建帮、审批通过时，按 I2 清掉该玩家在其他帮会的申请。
   - 解散帮会时，删掉发给本帮的申请。
9. **结算积压的最老年龄会单调增长**：玩家带着未应用的结算永久流失后，他的索引成员会一直停在头部，`oldest_age_s` 只升不降，这是预期行为。
   - 判断异常以 `metric=battle_settlement_undelivered reason=not_acked` 与积压数的增速为准。`oldest_age_s` 只作观察。
   - `deploy/k8s/asset-channel-alerts.yaml` 头部那条 `oldest_age_s > 86400` 的 LogQL 只作值班参考，不转成规则。
10. **team 的调用者总被 bump**：申请人、拒绝邀请者这类不是成员的调用者，GetMyTeam 的结果其实没变，也会 bump `TEAM`。代价只是多拉一次。
11. **D2 编译依赖 D1 的 proto 生成**：`player_battle.cpp` 引入了 `player_landing.h`，而后者依赖 `player_scene.pb.h`。proto 生成之前，scene 库与 bag_test 编不过。两批本来就一起验证，不再另拆窄头。
12. **帮会后台拉取扩大了隔离态的暴露面**：`GuildClient.Request` 一次传输超时（未断线）就会置 `RequiresReconnect`，直到真实断线才解除，玩家开窗看到「请重新登录角色后再试」。隔离是正确性所需（FIFO 回包无法区分超时后才到的旧回包）。后台只拉 `GUILD` / `GUILD_APPLICATION`，且只在该域变了时拉，暴露面已收窄；联机验证要专门观察集中落地（全服重连、帮会服务变慢）后是否有客户端进入隔离态。若出现，把这两个域也改成只被动确认（只删 `BindGuild` 里两行注册）。
