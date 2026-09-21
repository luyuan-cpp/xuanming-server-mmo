# S7 回档 fail-closed(B5d 详细设计)

> **状态:设计稿,未落码、未编译。** 起草 2026-09-20,基线 `main = aecea79b0`(工作树另有 B5a 与跨 zone 会话的在途改动)。同日经三视角对抗评审回修一轮(17 条,处理记录见文末附录),复核确认 9 条 blocker / major 均已在正文修掉,复核另指出的 8 处 minor(1 处论证不严谨 + 7 处口径 / 计数不一致)已就地订正;**落码前需用户拍板 §7.11 的 U1(Q1–Q4)、U9、U10**。
> 本文是 91 批次表里 B5d 的硬前置("需先补详细设计")。接口形状取自 04 §4.38/§4.39 与 90 清单(part2 §3 末行、Y-06、Y-13、Y-14、G-01/02/06/07/10),**形状不改,只补细节**;确有缺陷的写进 §7.11。
> 效力:README §2/§3 > 05 顶部两个覆盖块 > 90 清单 > 本文 > 04/05/06 正文里与回档有关的旧句(05 W15、06 C14 的"接受 + 审计"已被 Y-13 作废)。
> 文中 `E:\work\xuanming-server-mmo` 一律对应本机 `D:\luyuan\wuxingqitan\mmorpg`。代码事实均带 `路径:行号`,落码前以磁盘为准再核一遍。

## 结论(先读这段)

**类比**:玩家在柜台把 100 金交给帮会,帮会账本记下"资金 +100、此人帮贡 +10"。回档相当于把**玩家的钱包**换回昨天拍的照片——100 金回到钱包里;可帮会账本不在那张照片里,+100 和 +10 还在。一笔钱变成了两笔。商店方向反过来:帮贡在帮会账本里已经扣掉,回档把玩家背包里买到的东西抹了,玩家白白亏一笔。单人回档还能人工补,`RollbackZone / RollbackAll` 是成批复制。

**B5d 怎么挡**:回档在碰玩家数据之前,先问 guild 一句"这些玩家自快照时刻以来,有没有**已经终结为已应用**的资产操作?"

1. 有 → **默认拒绝回档**,返回条数与前 20 条摘要;
2. 问不到(guild 没配、不可达、超时、旧版本没有这个 RPC、结果超上限)→ **同样拒绝**,放行开关也不管用(Y-13);
3. 快照比帮会流水保留期还老、**无法证明** → 默认拒绝;它与第 2 条的"问不到"是两回事(guild 答了,只是答"这段我证明不了";错误码也不同,§7.3.3),是除"查到分歧"之外**唯一**可以被第 4 条放行覆盖的情形(90:191 原文是"**默认**拒绝回档";§7.5.4、§7.11-U9);
4. 运维明知有分歧仍要回档 → 请求显式带 `accept_guild_divergence = true` **且**带 `x-admin-token`,放行,并把每一条分歧以固定格式写 ERROR 日志,供事后按 `op_id` 人工补偿;
5. 栅栏生效后先**沉降等待 30 秒**再做首次检查,让"踢下线前一两秒才发生"的操作先终结,在零写入阶段就被拒(§7.7);写完之后再等 10 秒**复查一次**,抓剩下的漏网行。

不做自动反向扣款(04:1691 已否决:玩家可能离线或余额不足,补偿本身又会卡住)。

**三个会改变预期的现状**(侦察所得,已复核):

- 生产回档今天整体停用:`RollbackFence` 生产恒为 nil,三个回档 RPC 全回 `ErrCodeNotImplemented`(`go/data_service/internal/svc/servicecontext.go:68-72`、`internal/logic/rollback_logic.go:54-56`)。B5d 是在已经关着的门上再加一道闸,上线风险低;但这道闸必须先于栅栏实现落地,否则栅栏一接上就是裸奔。
- 回档今天写的是 `player:{id}:<field>`(`data_logic.go:19-24`),scene 读写的是 `<PlayerAllData 全名>:{id}`(`docs/design/player-async-save-loss-windows.md:36-45`)。也就是说**"复制资产"要等回档真正接上 scene 读的那份 blob 才会发生**。B5d 的闸挂在回档 RPC 上,不依赖回档写哪套 key;账本读取(§7.8)则必须读 PlayerAllData key,否则永远读不到。
- 三个 `Rollback*` handler 不做任何鉴权(`dataserviceserver.go:356-421`),`operator` 是自报字符串。放行开关如果只是个请求字段,谁能拨到 data_service 谁就能放行——所以本文要求放行必须带 `x-admin-token`(§7.6)。

---

## 7.1 问题与不变量

### 7.1.1 分歧矩阵(kind × status)

记 `S` = 被选中快照的时刻,`F` = guild 侧该行**记下的**终结时刻(`next_attempt_ms`,05:690 的 CAS 把它与 `updated_ms` 同写为 now)。注意这个 now 不是事务提交时刻,而是该次 `ProcessOne` **开始**时取的 `nowMs`(`go/shared/assetop/reconcile.go:484` 取值 → `:509` 传入 `finalize` → `:569` 传给 `Store.Finalize`),可早于 scene 的实际应用 / 落盘时刻最多一个 `OpBudget`(2500ms,`reconcile.go:275`);§7.2 的余量据此定下界。只讨论 `F > S` 的行;`F ≤ S` 的行两边都在快照之前,不受回档影响。D2 之后**没有** DONATE_REFUND kind(05:5),矩阵只有三行。

| kind \ status | PENDING | APPLIED | APPLIED_PARTIAL | REJECTED | ABORTED |
|---|---|---|---|---|---|
| **DONATE**(玩家扣货币 → 帮会得资金、玩家得帮贡) | 不算分歧(注 1) | **拒绝**:玩家货币回来,资金与帮贡保留 = **复制** | **拒绝**(注 2) | 不算分歧(注 3) | 不算分歧(注 3) |
| **SHOP**(提交时扣帮贡 → 玩家得物品) | 不算分歧(注 1) | **拒绝**:物品被回掉,帮贡与限购次数不回 = **玩家净损失** | **拒绝 + 需人工**(本来就在人工补偿队列) | 不算分歧(帮贡已退,玩家侧本无变化) | 同左 |
| **ACTIVITY_REWARD**(入队事务里记完帮贡/资金 → 玩家得奖励) | 不算分歧(注 1) | **拒绝**:奖励被回掉,领奖记录不回 = **玩家净损失**(注 4) | **拒绝 + 需人工** | 不算分歧 | 不算分歧 |

- **注 1(PENDING)**:账本与 currency/bag 同一条记录、同一次 SET(I3,`proto/common/component/asset_op_ledger_comp.proto:8-9`),回档让两者同进同退。快照早于应用 → 该 seq 回到"未见",重投等于首次应用;快照晚于应用 → 重投拿到只读答复 APPLIED。两种都自洽(04:356 C10)。**但** PENDING 行可能在检查之后才终结,这是竞态,不在本表处理,见 §7.7。PENDING 行**不拦**:离线玩家的 SHOP / ACTIVITY_REWARD 行会长期 PENDING,拦了等于每次 zone 回档都必须带放行开关,开关就废了。
- **注 2**:APPLIED_PARTIAL 计入分歧是 90:191 的明文(`status IN (?A,?AP)`)。它不被清理(90:188),所以一条很老的 PARTIAL 只要 `F > S` 就会挡住回档——这是对的:它本身就是待人工处理的行,处理完(补偿)后仍是 PARTIAL 状态,此时用放行开关,日志里会如实列出。
- **注 3**:REJECTED / ABORTED 在玩家侧没有资产变化,guild 侧没有入账(SHOP 已退帮贡)。回档后账本里该 seq 的"已拒绝"记录消失、变回未见;guild 已终结不会重投,无害。残余风险只有 04 §4.32 已论述的"截获旧包在 ±300s 内重放",与本批无关。
- **注 4**:ACTIVITY_REWARD 的 `funds_delta / contribution_delta = 0 / 0`(06:497、05:704),`GuildAssetOpBrief` 又没有 `ref_id / payload`,日志说不清"丢了什么"。形状不许改,补偿时按 `op_id` 回查 `guild_asset_op.payload / ref_id`(§7.9.3 手册)。

### 7.1.2 不变量

- **R1(默认拒绝)**:任何回档 RPC,只要目标玩家存在 `status ∈ {APPLIED, APPLIED_PARTIAL} ∧ F > S − 余量` 的行,且请求未合法放行,则**零玩家数据写入**(含回档前安全快照)。
- **R2(无法证明即拒绝)**:guild 客户端未配置 / 调用失败 / 超时 / 任一页失败 / `Unimplemented` / 结果超上限 → 与 R1 同样零写入,**放行开关无效**。不存在"问不到就当没有"。
- **R2b(保留期不可证明:默认拒绝,可显式覆盖)**:玩家的 `since` 早于 guild 的保留期下界 → 未放行时零写入;合法放行(§7.5.4 三条件)时,保留期内仍可证明的那段照查照列,不可证明的玩家按块写 ERROR 日志后继续。这是"问不到"里**唯一**可覆盖的一类:它不是故障,重试一万次也还是问不到,不给出口只会把运维逼去绕闸(§7.11-U9)。
- **R3(放行必留痕)**:放行路径下,每一条分歧在**写之前**已以 ERROR 级写入日志,且审计表有一条带操作人与原因的记录;日志写不出不阻塞(logx 无回执),审计写不进则拒绝(沿用 `rollback_logic.go:135-146` 的口径)。
- **R4(批量先查后写)**:`RollbackZone` 对整份玩家清单查完才开始写第一个玩家;`RollbackAll` 对**全部 zone** 查完才开始写第一个 zone。
- **R5(计划与执行一致)**:执行时实际选中的快照时刻不得早于检查时用的时刻,否则该玩家失败不写(§7.5.3)。
- **R6(账本不单独回档)**:账本不得与 currency/bag 分开恢复;`ROLLBACK_PARTIAL` 同样过闸(§7.5.1)。
- **R7(I7 不变)**:guild 读已落盘账本只在"已见 APPLIED/REJECTED"时终结;读不到、读失败、玩家无 blob 都只是继续等(04:1532)。

---

## 7.2 术语与 `since_ms`

- **快照时刻 `S_p`**:玩家 p 本次回档实际选中的那份快照的 `created_at`,单位**秒**,由 data_service 写入(`snapshot_logic.go:81` `time.Now().Unix()`)。`snapshot_id != 0` 时取该快照;否则取 `target_time` 之前最近一份(`resolveSnapshot`,`snapshot_logic.go:271-289`;SQL 见 `snapshot_store.go:172-178`)。**不能直接用 `target_time`**:快照可能比它早几小时,中间那段的操作会漏。
- **`since_ms`(逐玩家)**:`playerSinceMs[p] = S_p × 1000 − GuildClockSkewMarginMs`。秒转毫秒取下界(×1000 即该秒的起点),方向 = 多报。结果 < 1 时钳到 1(guild 把 `since_ms == 0` 当漏填拒掉,§7.4.2;真实快照不会触发,只为单测里的小时间戳不下溢)。
- **`since_ms`(每次 RPC)**:接口每次调用只有一个 `since_ms`,取**该块内**所有玩家 `playerSinceMs` 的最小值。返回后 data_service 再按逐玩家值过滤:保留 `brief.updated_ms > playerSinceMs[brief.player_id]` 的行。
  - 为什么能用 `updated_ms` 过滤:终态行 `updated_ms ≥ next_attempt_ms`(Finalize 同写 now;此后没有任何路径再改终态行),按 `updated_ms` 过滤只会多留,不会少留。
  - 为什么要过滤:不过滤时,同块里一个快照很老的玩家会把别人一个月内的操作全拉进"分歧清单",放行日志就失去了"照单补偿"的价值。
  - 块内最小值早于 guild 的保留期下界时,guild 会拒掉整次调用;data_service 把 `since_ms` 钳到下界再查一次,并把早于下界的玩家单独记为"不可证明"(§7.5.3-2b)。
- **时钟**:`S_p` 是 data_service 的墙钟,`next_attempt_ms` 是 guild 的墙钟(05:690 绑定的 `?now` 来自 guild 进程)。两者不同机,所以要余量。
- **余量 = 跨机时钟差 + `OpBudget`,不只是时钟差。** `F` 是 `ProcessOne` 开始时刻(§7.1.1),scene 的应用时刻 `T_a` 与落盘时刻 `T_p` 可以**晚于** `F` 最多 2500ms。时间线 `F < S < T_p`:快照不含这笔扣款,而 `next_attempt_ms = F ≤ S`——余量小于 `S − F` 时该行不被列出,回档恢复货币、帮会资金保留 = 复制(fail-open)。所以即使同机部署、时钟差为零,余量也**不得为 0**。
- `GuildClockSkewMarginMs` 默认 **300000**(5 分钟),与 04 §4.32 的 ±300s 鉴权时间窗同口径——那是本仓库对"服务间时钟最多差多少"的既有承诺,不另立一个数。配置项放 data_service(§7.5.5),Validate ∈ **[5000, 3600000]**(下界 = `OpBudget × 2`;0 与 4999 启动即拒,config 注释写明原因,测试 C1)。
- **余量的代价(已知误报)**:`S − 300s < F ≤ S` 且 `T_p ≤ S` 的行其实在快照之内,会被误判为分歧。另有一类:scene 已应用并落盘(`T_p < S`),guild 终结晚于快照(`F > S`,典型是离线读账本)——快照里已含该效果,却仍被列出。两类都是**多拒**,方向安全;消除它需要 brief 带 `seq + stream_epoch` 以便对照快照里的账本,属形状变更,见 §7.11-U3。
- **`target_time` 在未来 / 等于现在**:不额外校验(现状也不校验)。此时选中的是最新快照,`S_p` 仍是真实值,逻辑不变。

---

## 7.3 协议

### 7.3.1 `proto/guild/guild_internal.proto`(新文件,全文)

```proto
syntax = "proto3";
package guildpb;

option go_package = "guild/proto/guild";

// 本文件不使用任何自定义 option,所以不 import proto_option.proto。
// 注意 guild.proto **有**该 import(:6),那只是为了服务级 OptionIsClientProtocolService(:395-396);
// 它同样没写 `option (OptionFileDefaultNode)`,节点类型由 protogen 三级回落
// (服务名 → package → 目录名 guild)派生,理由见 proto/friend/friend.proto:16-23。

// 帮会内部服务(docs/design/guild-phase2/07-rollback-fail-closed.md)。
// 刻意不标 OptionIsClientProtocolService,且与客户端协议分文件(照 proto/trade/trade_admin.proto:13-18):
//   - gate 的 IsClientMessageId 不收这些消息号;
//   - 路由服对 ClientProtocol=false 的条目回信封拒绝;
//   - guild 会话拦截器对带会话 metadata 的调用回 PermissionDenied(白名单外一律拒);
//   - 客户端 gen_proto.ps1 不列本文件。
// 占消息号,不进 MessageLimiter,不进客户端白名单(90 G-10)。

// 已终结为"已应用"的资产操作摘要。只读,不含 payload。
message GuildAssetOpBrief {
  uint64 op_id = 1;
  uint64 player_id = 2;
  uint64 guild_id = 3;              // 发起时绑定的帮会;该帮会可能已解散
  uint32 stream = 4;                // AssetOpStream 数值(与 guild_db.proto 一致,不 import asset_op.proto)
  uint32 kind = 5;                  // GuildAssetOpKind 数值
  uint32 status = 6;                // GuildAssetOpStatus 数值;只会是 APPLIED(2) 或 APPLIED_PARTIAL(5)
  uint64 funds_delta = 7;
  uint64 contribution_delta = 8;
  uint64 updated_ms = 9;
}

message ListAppliedAssetOpsSinceRequest {
  uint32 zone_id = 1;               // 0 = 不按 zone 收窄(data_service 恒传 0,理由见 07 §7.4.3)
  repeated uint64 player_ids = 2;   // 必填,1..100,不得含 0、不得重复
  uint64 since_ms = 3;              // 必填 > 0;只返回终结时刻严格晚于它的行
  uint64 after_op_id = 4;           // 游标;首页传 0
  uint32 limit = 5;                 // 0 → 500;> 500 → InvalidArgument
}

message ListAppliedAssetOpsSinceResponse {
  repeated GuildAssetOpBrief ops = 1;   // 按 op_id 升序
  uint64 next_after_op_id = 2;          // 0 = 已查尽;否则 = 本页最后一行的 op_id
}

service GuildInternal {
  rpc ListAppliedAssetOpsSince (ListAppliedAssetOpsSinceRequest) returns (ListAppliedAssetOpsSinceResponse);
}
```

- `kind / status / stream` 用 `uint32` 而不是枚举:避免本文件 import `guild_db.proto`(那是库表 schema,带 proto2mysql 选项,不该进一个 RPC 文件的依赖闭包)。Go 侧用 `uint32(pb.GuildAssetOpStatus_…)` 赋值,**不写数字字面量**。
- 节点归属的退路:若验证步 3 发现 `GuildInternal` 的路由条目没落到 guild 节点(三级回落对第二个 service 失手),再补 `import "proto/db/proto_option.proto"` + `option (OptionFileDefaultNode) = NODE_GUILD;`(枚举值已存在,`proto/db/proto_option.proto:35`),并同步核 `guild.proto` 的条目未受影响。**未能确认**回落对同目录第二个 service 的行为(未读 `internal/model.go`),所以留这条退路。
- 响应**没有** `TipInfoMessage`:内部 RPC 的拒绝全部走 gRPC status(§7.4.4),不占 tip 号段,不动 `Tip.xlsx`。

### 7.3.2 `proto/data_service/data_service.proto` 增量

```proto
import "proto/common/component/asset_op_ledger_comp.proto";   // 本文件第一次 import 仓内 proto,见 §7.11-U4

service DataService {
  // ── 资产通道:已落盘账本只读 ─────────────────────────────────
  // 读 zone Redis 里 scene 写的 PlayerAllData blob 的 asset_op_ledger 子字段。只读、不鉴权(与
  // BatchGetPlayerName 同级)。实现 go/shared/assetop 的 LedgerReader。07 §7.8。
  rpc GetPlayerAssetOpLedger(GetPlayerAssetOpLedgerRequest) returns (GetPlayerAssetOpLedgerResponse) {}
}

message GetPlayerAssetOpLedgerRequest { uint64 player_id = 1; }
message GetPlayerAssetOpLedgerResponse {
  bool found = 1;                          // false = 该玩家在 zone Redis 里没有 blob(从未入场,或 Redis 丢数据)
  PlayerAssetOpLedgerComp ledger = 2;      // found=true 且玩家从未有过资产操作时为空消息
}

// 回档被帮会资产分歧拦下时的摘要行(与 guildpb.GuildAssetOpBrief 同义;刻意不 import guild 的 proto,
// 避免 data_service 的 C++/Go 生成闭包依赖 guild)。
message RollbackGuildDivergence {
  uint64 op_id = 1;
  uint64 player_id = 2;
  uint64 guild_id = 3;
  uint32 kind = 4;
  uint32 status = 5;
  uint64 funds_delta = 6;
  uint64 contribution_delta = 7;
  uint64 updated_ms = 8;
}
```

三个回档请求各追加一个字段,三个响应各追加三个字段。**字段号 = 落码时该 message 的下一个空闲号**(今天磁盘上分别是下表,B5a / 别的会话若先占了就顺延,不复用、不插队):

| message | 追加 | 今天的空闲号 |
|---|---|---|
| `RollbackPlayerRequest`(`:231-239`) | `bool accept_guild_divergence` | 8 |
| `RollbackZoneRequest`(`:250-255`) | 同上 | 5 |
| `RollbackAllRequest`(`:271-275`) | 同上 | 4 |
| `RollbackPlayerResponse` | `uint32 guild_divergence_count`、`repeated RollbackGuildDivergence guild_divergences`、`uint32 guild_unprovable_player_count` | 5、6、7 |
| `RollbackZoneResponse` | 同上 | 7、8、9 |
| `RollbackAllResponse` | 同上 | 5、6、7 |

- `guild_unprovable_player_count`:快照早于帮会流水保留期、无法证明的玩家数(R2b)。只回计数,不回 id 清单——是谁,看 §7.5.4 的 `unprovable` 日志行;让运维第一次不带放行的试探就能看见"是保留期挡住了、挡了多少人",而不是只拿到一个笼统的 CheckFailed。

- 三个请求都加:`RollbackZone/All` 才是成批复制的主战场,只给单人加开关等于批量回档永远无法放行。默认 `false` = 安全,旧调用方不受影响。
- `guild_divergences` 最多 **20** 条(`maxDivergenceSample`),按 `op_id` 升序取前 20;`guild_divergence_count` 是过滤后的真实总数(上限见 §7.5.4)。

### 7.3.3 错误码(`internal/constants/error_codes.go`)

按 Y-14 **落码时末码 +1 起连号**(今天末码是 `ErrCodePlayerNameConflict = 26`,`:113`;不写死数字)。三个码:

| 名 | 含义 | 进 `FaultCodeSet`(`data_service.go:181-190`)? |
|---|---|---|
| `ErrCodeRollbackGuildDivergence` | 查到分歧、**或有玩家的快照早于保留期无法证明**(R2b),且未放行。规则拒绝,口径同 `ErrCodePlayerOnline`。**这个码的含义 = "带合法放行可以过"**;两种原因靠 `guild_divergence_count` 与 `guild_unprovable_player_count` 区分 | 否 |
| `ErrCodeRollbackGuildCheckFailed` | 查不成:guild 未配置 / 不可达 / 超时 / Unimplemented / 保留期拒绝但解析不出下界 / 结果超上限 / 预算耗尽。**这个码的含义 = "放行也过不了,先修环境或缩小范围"** | **是**(否则被记成业务拒绝、不告警) |
| `ErrCodeRollbackGuildDivergedAfterWrite` | 写后复查发现新分歧(§7.7)。数据已写,需人工 | **是** |

放行但没带合法 `x-admin-token`:复用现成的 `ErrCodeAdminAuthRequired` + gRPC `PermissionDenied`(`authorizeAdmin`,`dataserviceserver.go:163-185`),不新增码。

---

## 7.4 guild 侧实现

### 7.4.1 SQL 与游标

```sql
SELECT o.op_id, o.player_id, o.guild_id, o.stream, o.kind, o.status,
       o.funds_delta, o.contribution_delta, o.updated_ms
  FROM guild_asset_op o
  -- 仅当 zone_id != 0 时拼下面两行:
  LEFT JOIN guild g ON g.guild_id = o.guild_id
 WHERE o.status IN (?A, ?AP)               -- 绑定 uint32(pb.GuildAssetOpStatus_…APPLIED / …APPLIED_PARTIAL)
   AND o.next_attempt_ms > ?since
   AND o.player_id IN (?, ?, …)            -- 1..100 个占位符
   AND o.op_id > ?after
   AND (g.zone_id = ?zone OR g.guild_id IS NULL)   -- 仅当 zone_id != 0
 ORDER BY o.op_id ASC
 LIMIT ?limit_plus_1;                      -- 取 limit+1 行,多出的一行只用来判断"还有没有下一页"
```

**游标为什么按 `op_id` 而不按索引序,以及代价**:

- 给定的游标只有一个 `after_op_id`。idx_0 的列序是 `(status, next_attempt_ms)`(90:119),InnoDB / TiDB 的二级索引末尾隐含主键,实际是 `(status, next_attempt_ms, op_id)`。要"既走索引序又稳定"需要复合游标 `(status, next_attempt_ms, op_id)`,**单个 `after_op_id` 表达不了**——只按 `next_attempt_ms` 排、用 `op_id` 当游标会在同毫秒多行时丢行或重行。
- 所以定为:**谓词走 idx_0 的范围扫描收窄,排序按主键 `op_id`,接受一次 filesort**。`op_id` 唯一、不可变、与终结先后无关,所以翻页**稳定**(已返回的行不会因为别的行被终结而移位)。
- filesort 的规模有界:一次调用至多 100 个玩家 × `since` 之后的终态行;回档是罕见的运维操作,不在任何热路径上。
- **不写 `FORCE INDEX`**。玩家数少、时间窗长时,优化器可能改走 idx_2(`player_id` 前缀,90:119-120),两条路结果相同,那种形状下 idx_2 反而更省。验收标准**只有一条**:`EXPLAIN` 的 `key ∈ {idx_guild_asset_op_0, idx_guild_asset_op_2}`(§7.10.3 步 7)。"`type` 不为 `ALL`"**不够**——走 PRIMARY 的范围扫描 `type` 是 `range`,照样过得了那条标准。与 90:191 "走 idx_0" 的差异记入 §7.12-偏差 2。
  - **已知风险与预写退路(未实测,属优化器已知倾向)**:MySQL 对"`ORDER BY` 主键 + 小 `LIMIT`"有 `prefer_ordering_index` 倾向,可能改走 PRIMARY 顺序扫描、逐行过滤。回档检查的常态恰好是"零命中",此时 PRIMARY 要扫完 `op_id > after` 的全表才返回 → 单次调用超时 → `CheckFailed` → 所有回档被拒(方向安全,但闸的"通过"分支不可用)。若步 7 的 `EXPLAIN` 选了 PRIMARY:在 `FROM guild_asset_op o` 后加 `USE INDEX (idx_guild_asset_op_0, idx_guild_asset_op_2)`(MySQL 与 TiDB 都支持该写法;给两个候选,仍由优化器二选一),重跑步 7,并在偏差 2 补记"已加索引提示"。不预先加:没有 EXPLAIN 证据之前加提示是未经测量的优化(AGENTS §11.3)。
- **稳定但不"快照一致"**:翻页期间若有一行 `op_id < 游标` 才被终结,本轮看不到它。这不靠游标解决,靠 §7.7 的写后复查兜住;而拒绝路径只要任一页非空就已经成立,不受影响。
- `next_after_op_id`:取回 `limit+1` 行则 = 第 `limit` 行的 `op_id`(并丢掉多取的一行),否则 = 0。调用方循环到 0 为止。

**对 B5b 的硬要求:`Store.Finalize` 必须同写 `next_attempt_ms = nowMs`。** 整道闸的查询正确性建立在"终态行的 `next_attempt_ms` = 终结时刻"上。05:690 的 SQL 写了 `next_attempt_ms = ?now, updated_ms = ?now`,但 B5b 实现者真正照着写的 Store 契约——`go/shared/assetop/reconcile.go:136-138` 的 Finalize 注释——是 `SET status=?, durable=1, last_outcome=?, last_reason=?, updated_ms=?`,**没有 `next_attempt_ms`**。照注释落码,终态行的 `next_attempt_ms` 会停在最后一次重排时刻(早于真实终结),`next_attempt_ms > since` 漏行 = **fail-open**,清理判龄也偏早。以 05:690 为准;该注释需要订正(属 shared/assetop 持有会话或 B5b 的范围,B5d 不碰,交付说明里点名)。由测试 G5b 经**真实** `GuildAssetStore.Finalize` 钉住(G1–G5 都是直接插行,钉不住这条主路径)。

**人工终结的行**:`assetopfix` 的 `ResolveManually`(04:1547)原文只写 `updated_ms`,没写 `next_attempt_ms`。B5b 落码时必须让它与 Finalize 一样同写 `next_attempt_ms = now`,否则人工判 APPLIED 的行按旧的 `next_attempt_ms`(一个退避时刻)判龄。即便漏了,PENDING 行的 `next_attempt_ms` 本来就贴近"现在",`> since` 依然成立,回档检查不会漏;受影响的只是清理判龄。B5d 的 repo 测试里加一条断言钉住(§7.10.2-G5)。

### 7.4.2 入参校验(在 server 层,先于任何 SQL)

| 条件 | gRPC code |
|---|---|
| `player_ids` 为空、> 100、含 0、有重复 | `InvalidArgument` |
| `since_ms == 0` | `InvalidArgument` |
| `limit > 500` | `InvalidArgument`(0 → 500) |
| `since_ms < now − TerminalRetentionDays×86400000 + RetentionSafetyMs` | `FailedPrecondition`,message 固定格式 `since_ms older than terminal retention; cutoff_ms=<十进制>`(`cutoff_ms` = 上式右端,即本次可接受的最小 `since_ms`) |
| 资产 Store 未装配(B5b 未启用经济) | `Unavailable` |

- **`player_ids` 上限 100**:沿用 X-14 已统一的 IN 占位符上限(90:67),不另立。必填的理由:不给"按 zone 扫全表"的口子——内部 RPC 无凭据(§7.6),`player_ids` 必填 + 保留期拒绝把单次调用的扫描量钉死。
- **`RetentionSafetyMs` = 3600000(常量,不进配置)**:清理各副本有 `[0, CleanupIntervalMinutes)` 的随机起始偏移(05:754),那只会让行**多活**一会儿,方向安全;真正不安全的是副本间墙钟差与翻页期间流逝的时间,1 小时远大于两者。
- 保留期判定**放在 guild**:`TerminalRetentionDays` 是 guild 的配置(05:881),data_service 读不到,也不该复制一份(DRY)。所以下界由 guild **在拒绝里带回**(`cutoff_ms`),data_service 据此钳位重查(§7.5.3-2b)。
  - 用 status message 传一个数是权宜:给定的响应形状里没有放它的字段,形状不许改;gRPC status details 要为一个整数引入 `errdetails` 依赖,不值。格式由 guild 侧一个导出常量 + data_service 侧一个解析函数各自持有,**解析失败 = `CheckFailed`**(不可放行),所以格式漂移的方向仍是拒绝。测试 G8 / D15 两端各钉一次同一条样例字符串。
  - data_service 对其它任何非 OK 的 gRPC status 一视同仁地拒绝(§7.6.4)。

### 7.4.3 `zone_id` 的语义(偏差 1)

90:191 写"再按 `guild_id` 关联 `guild.zone_id`"。本文实现这个过滤,但 **data_service 恒传 0**,理由:

1. `guild.zone_id` 是**帮会**的 zone,合服会改写它;回档清单来自快照当时的 `zone_id`(`snapshot_store.go:249-252`),合服后两者不等。拿旧 zone 去 JOIN 会把该玩家的行**全部滤掉**——闸在最不该开的时候开了(fail-open)。
2. 帮会解散会删 `guild` 行(`guild_manage_repo.go:1574`),INNER JOIN 会丢掉"入账后解散"的行;所以实现用 LEFT JOIN 并保留 `g.guild_id IS NULL`。
3. `player_ids` 必填之后,zone 过滤只可能**减少**结果,没有任何一种场景靠它变得更安全。

保留实现而不是忽略该字段:形状里有这个字段,"收下却静默忽略"比"实现它"更危险。它留给运维手工排查用(grpcurl 按 zone 看某批玩家)。

### 7.4.4 拦截器与注册

- `go/guild/internal/session/session.go` **不改**:拦截器是"白名单外一律拒"(`:84-99`),`GuildInternal.*` 不登记进 `ClientMethods`,带会话 metadata 时自然得到 `PermissionDenied`;不带会话时按"内部调用"放行(`:88-90`)。
- `go/guild/guild.go:227-232` 的 `zrpc.MustNewServer` 回调里加一行 `pb.RegisterGuildInternalServer(grpcServer, server.NewGuildInternalServer(...))`。拦截器链(`buildUnaryInterceptors`,`:276-311`)对新 service 自动生效,含 killswitch 与请求预算。
- `session_test.go` 补一条:遍历 `GuildInternal_ServiceDesc.Methods`,断言每个 `FullMethodName` 都不在 `ClientMethods` 里,且带会话 metadata 调用得到 `PermissionDenied`(照 `go/trade/internal/session/session_test.go:103-112`)。

### 7.4.5 指标(guild 已配 `Prometheus.Host`,用 go-zero `core/metric`)

- `guild_internal_list_applied_total{result}`,result ∈ `ok_empty / ok_rows / invalid / retention / unavailable / error`。
- `guild_internal_list_applied_rows`(histogram,每次调用返回的行数)。

label 不含 `player_id / zone_id / guild_id`。

**启动预置为 0**:result 的六个取值在 `zrpc.MustNewServer` **之后**逐个 `Add(0)`(位置约束见 92-handoff:184-189:放进 `init()` / `NewServiceContext` 会被 go-zero 丢弃)。理由同 §7.9.1。

---

## 7.5 data_service 侧实现

### 7.5.1 接缝

```go
// internal/logic/guild_divergence.go
// GuildDivergenceChecker 回答"这些玩家自各自快照时刻以来,帮会侧有没有已应用的资产操作"。
// 契约:查不成(不可达、超时、任一页失败、保留期拒绝但解析不出下界……)必须返回 error,调用方据此
//       拒绝回档且不可放行;Divergences 已按逐玩家 since 过滤、按 op_id 升序;实现必须自己翻完所有页。
//       Unprovable = since 早于 guild 保留期下界的玩家(升序):对他们只查得到下界之后的行,
//       更早的那段无法证明。它不是 error——调用方按"可放行的拒绝"处理(R2b)。
type GuildDivergenceChecker interface {
    ListDivergences(ctx context.Context, sinceMsByPlayer map[uint64]uint64) (GuildCheckResult, error)
}

type GuildCheckResult struct {
    Divergences         []GuildDivergence
    UnprovablePlayerIDs []uint64
    RetentionCutoffMs   uint64 // 仅当 UnprovablePlayerIDs 非空时有意义;日志用
}
```

- 生产实现包一层 `guildpb.GuildInternalClient`;单测注入 fake。这是"外部系统隔离 + 测试隔离"的真实接缝(AGENTS §11.5-2),不是为抽象而抽象。
- `ServiceContext.GuildDivergence GuildDivergenceChecker`:**nil = 未配置 = 拒绝回档**。⚠️ 这与同文件 `LoginAdminClient` 的先例(`servicecontext.go:186-190`,nil = 跳过)**方向相反**,照抄会做成 fail-open;代码注释必须写明,并由测试 D2 钉住。
- `ROLLBACK_PARTIAL`(`rollback_logic.go:242-251`)同样过闸:今天的字段粒度是 Redis key,无法证明"只恢复的那几个 key 不含资产";将来回档接上 PlayerAllData 单 blob 后,部分恢复更不可能把 currency 与账本拆开(R6)。

### 7.5.2 检查在流程中的位置

```
校验入参 → validateRollbackDependencies → [放行鉴权:accept=true 必须过 authorizeAdmin,在 server 层]
→ Acquire 栅栏 → 写 STARTED 审计
→ 计划:解析每个玩家的快照时刻(只读)
→ 沉降等待 guildSettleDelay(§7.7;持栅栏、零写入)
→ 帮会检查(只读;查不成 / 有分歧或不可证明且未放行 → 写 RESULT 审计(带拒绝码)→ 返回,零玩家数据写入)
→ [放行:逐行 ERROR 日志 + 审计追加一条 ACCEPTED]
→ 逐玩家:安全快照 → 写 Redis        ← 今天的第 3~5 步,原样
→ ── 以下换用脱钩 ctx(§7.5.3-3)──
→ 写后复查(§7.7)
→ 写 RESULT 审计
```

- "任何写之前"的边界定为**任何玩家数据写之前**,回档前安全快照(`:211-238`)也算写,检查在它之前。STARTED 审计在检查**之前**:被拒绝的回档尝试本身需要留痕(谁、何时、想回到哪),RESULT 审计带上拒绝码与分歧条数。
- 检查在栅栏**之内**:栅栏保证目标离线、旧 epoch 存盘已排空(`servicecontext.go:64-72`),之后 scene 侧不会再对这些玩家产生新的应用。

### 7.5.3 计划与分块(RollbackZone / RollbackAll)

1. **取清单与快照时刻,一条 SQL**(新 store 方法,替代循环里 N 次读整份 blob):
   ```sql
   SELECT player_id, MAX(created_at) FROM player_snapshot
    WHERE zone_id = ? AND created_at <= ? AND source = ? GROUP BY player_id;
   ```
   `GetSnapshotPlayerIDsByZone` 保留不动(别的调用点仍在用;本批不夹带重构),新方法名 `GetSnapshotPlayerTimesByZone`,返回 `map[uint64]uint64`。
   - 执行期 `resolveSnapshot` 不带 `zone_id` 条件(`snapshot_store.go:177`),选中的快照只会**等于或晚于**计划值(玩家在别的 zone 另有更新的快照时)。计划值更早 = `since` 更早 = 多查,方向安全。
   - **R5 的落点**:`rollbackSinglePlayer` 解析出 `snap` 之后、安全快照之前,断言 `snap.CreatedAt ≥ plannedCreatedAt[pid]`;不成立(理论上只有快照被并发删除才会发生)→ 该玩家回 `ErrCodeRollbackGuildCheckFailed`,不写。单人回档同一断言(计划值就是自己刚解析的那份,恒成立)。
2. **分块**:按 `player_id` 升序每 **100** 个一块(= guild 的上限)。每块 `since_ms = min(playerSinceMs)`,翻页到 `next_after_op_id == 0`。块与块**串行**:回档是运维操作,并发只会把 guild 的连接池打满,不值得。
   - **2b 保留期钳位(R2b)**。为什么必须处理:快照是稀疏的——`rollback_logic.go:451-456` 自述"快照只在显式触发时才产生";`DeleteOldSnapshots` 全 data_service 无调用者(只有定义 `snapshot_store.go:322`);zone 清单是"所有曾有过快照的玩家"(`snapshot_store.go:249-252`)。开服满 30 天后,清单里出现 `S_p` 早于保留期的玩家是**常态**;不处理的话,一个这样的玩家就让他所在的块、进而整次 zone / 全服回档被拒,且无出口。
     - 某块首页收到 `FailedPrecondition` → 解析 `cutoff_ms`(解析失败 → error → `CheckFailed`)→ 该块内 `playerSinceMs[p] < cutoff_ms` 的玩家记入 `UnprovablePlayerIDs` → 用 `since_ms = cutoff_ms` **重查该块一次**。重查再得 `FailedPrecondition`(不该发生:`cutoff_ms` 随墙钟前移,两次调用间隔远小于 guild 留的 1 小时安全量)→ error。这不是"重试":换了参数,每块至多一次。
     - 重查返回的行照常逐玩家过滤;对不可证明的玩家,其 `playerSinceMs < cutoff_ms`,返回的行全部保留——"保留期内仍可证明的那部分"照列不误。
     - 无论是否带放行开关都走完这一步:拒绝响应里的两个计数才完整。
3. **预算与 ctx(三层,逐层写明)**:
   - **单次 RPC 超时 = `GuildInternalRpc.Timeout`**(yaml 显式写 **3000**)。不另设配置项:zrpc 客户端自带超时拦截器,go-zero 默认 2000ms(`go-zero@v1.10.0/zrpc/config.go:31`),另立一个 `CallTimeoutMs` 只会得到两个旋钮、小的那个悄悄生效。Validate:已配 `GuildInternalRpc` 时 `Timeout ∈ [500, 3500]`(上界 = guild 服务端 `Timeout: 4000`(`guild.yaml:7`)− 500 回包余量)。
   - **检查阶段总预算 `GuildCheckBudgetSeconds`**(默认 120):`context.WithTimeout(入口 ctx, …)`,**只罩检查阶段**(沉降等待之后、第一笔写之前),不罩写阶段。单调时钟。**不重试**:任何一次调用失败即整体拒绝,运维重发即可——重试只会把"guild 正在抖"伪装成"慢"。
   - **写后阶段(复查等待 + 复查 + RESULT 审计)用脱钩 ctx**:`context.WithTimeout(context.WithoutCancel(入口 ctx), guildRecheckDelay + guildRecheckBudget)`,`guildRecheckBudget` = 常量 **120s**。先例 `reconcile.go:549-553` 的 `settleContext`。理由:写阶段可能远超调用方 deadline;调用方超时 / 断开后入口 ctx 已死,沿用它的话 sleep 立即返回、复查必失败 → **每一次这样的回档都被误报成"紧急"的 post-write**,真分歧反而查不出,RESULT 审计(`InsertAuditLog` 走 `ExecContext`,`snapshot_store.go:308`)也写不进。数据已经写了,这一段必须跑完。
   - **zrpc 服务端超时**:`etc/data_service.yaml` 今天没有 `Timeout:` 也没有 `MethodTimeouts`,取 go-zero 默认 **2000ms**(`zrpc/config.go:46`),`UnaryTimeoutInterceptor` 到点取消 handler 的 ctx 并向调用方回 `DeadlineExceeded`。沉降 30s、检查预算 120s 全都大于它——不配的话批量回档恒 `CheckFailed`。本批在 yaml 里为三个 `Rollback*` 配 `MethodTimeouts`(`RpcServerConf` 已支持,`config.go:52`;元素 = `FullMethod` + `Timeout`):
     ```yaml
     MethodTimeouts:
       - FullMethod: /data_service.DataService/RollbackPlayer
         Timeout: 300s     # 沉降 30 + 检查预算 120 + 复查等待 10 + 复查预算 120 = 280,余 20s 给单人写(与 §7.7 末的公式同口径)
       - FullMethod: /data_service.DataService/RollbackZone
         Timeout: 3600s
       - FullMethod: /data_service.DataService/RollbackAll
         Timeout: 14400s
     ```
     Zone / All 的值是**起始值**,没有写耗时的实测数据(回档今天在生产整体停用);落码后按本地 zone 回档实测的写耗时订正,写进手册。**配小了的后果(与 §7.7 末同一口径)**:入口 ctx 到点后,go-zero 的超时拦截器直接向调用方回 `DeadlineExceeded`,调用方**拿不到响应体**;若此时还在写阶段,后续玩家写失败进 `FailedPlayerIDs`。写后阶段靠脱钩 ctx 照常跑完、不会误报,结论以 RESULT 审计与日志为准。所以这三个值是"建议 ≥ 全程之和",不是正确性前提。
4. **任一块任一页失败 = 整体拒绝**,零写入(R2、R4)。
5. **拒绝路径也翻完所有块**:04:1567 要求"返回清单条数",运维要靠这个数决定敢不敢放行。上限见下条。
6. **RollbackAll**:把 `rollbackZoneWithFenceHeld` 拆成 `planZoneRollback`(清单 + 时刻 + 帮会检查,只读)与 `executeZoneRollback`(今天的循环 + 孤儿报告)。`RollbackAll` 先对 `AllZoneIDs()` 全部 plan,**全部通过**才开始 execute 第一个 zone(R4)。理由:逐 zone 边查边写,会出现 zone 1 已回档、zone 2 被拒的半截全服回档。内存:每玩家 16 字节,百万玩家 16 MB,可接受。
   - 每个 zone 的 STARTED / RESULT 审计保持现状(写在 zone 级函数里);plan 阶段被拒的 zone,其 RESULT 审计带拒绝码,未轮到的 zone 不写审计。

### 7.5.4 拒绝与放行

- **上限 `maxDivergenceRows = 10000`**(常量)。过滤后的分歧超过它 → `ErrCodeRollbackGuildCheckFailed`,**放行开关也不管用**,日志提示"缩小范围分批回档"。先例:`BatchRecallItems` 超上限回 `ErrCodeResultTruncated`、零变更(`recall_logic.go:140-157`)。理由:上万行 ERROR 日志既进不全 Loki,也没人能照单补偿。
- **拒绝响应**:`error_code` + `guild_divergence_count` + `guild_unprovable_player_count` + 前 20 条 `guild_divergences`。**样本行的可见性跟 Q2 走**:三个 `Rollback*` 今天零鉴权(`dataserviceserver.go:356-421`),若 Q2 选 ①(只有放行才要 token),任何能拨到 data_service 的进程发一次不带放行的 `Rollback*` 就能读到目标玩家的资产流水样本——所以 ① 之下,**未带合法 `x-admin-token` 的调用只回两个计数,不回样本行**;Q2 选 ②(推荐)则无此分支。handler 现状是"logic 返回 err 时只回 error_code"(`dataserviceserver.go:368-370`),落码时注意拒绝路径 **logic 不返回 err**(规则拒绝不是故障),三个新字段才带得出去。
- **放行条件(三者同时满足)**:`accept_guild_divergence == true`;`authorizeAdmin` 通过;`reason` 与 `operator` 非空(放行时必填;未放行时保持现状不校验,不夹带行为变更)。
- **放行时逐行日志**(ERROR 级,与 assetopfix 的审计同口径"确保进 Loki",05:774;04:1567 写的 INFO 记入偏差 3),一行一条,**在第一笔玩家数据写之前全部写完**:
  ```
  [Rollback][GuildDivergence] accepted scope=<player|zone|server> zone=<id> target_time=<s> operator=<op> caller=<callerIdentity>
      op_id=<> player_id=<> guild_id=<> kind=<> status=<> funds_delta=<> contribution_delta=<> updated_ms=<> snapshot_created_at=<s>
  ```
  另有一条汇总:`… accepted total=<n> unprovable_players=<m> by_kind=donate:<a>,shop:<b>,activity:<c> reason=<reason>`。不含任何 token;`player_id` 只进日志,不进指标 label。
- **不可证明玩家的日志(R2b)**:没有"行"可列,按块一条(每条 ≤100 个 id),同样在第一笔写之前写完:
  ```
  [Rollback][GuildDivergence] unprovable <accepted|rejected> scope=<> zone=<> operator=<op> cutoff_ms=<> oldest_since_ms=<> player_ids=<id,id,…>
  ```
  未放行的拒绝路径也写(`rejected`,WARN 级)——运维要靠它知道是谁挡住了;放行时 ERROR 级。不可证明的玩家数**不设上限**:逐人没有可补偿的清单,上限只会把"老 zone 永远回不了档"的问题搬回来;`maxDivergenceRows` 只管分歧行。
  - 放行"不可证明"意味着什么,手册要写白(§7.9.3-6):保留期之前那段的帮会流水已被清理,**系统给不出补偿单**,放行人自担核帐责任。
- **审计**:放行时在 STARTED 与 RESULT 之间追加一条 `Reason = "GUILD_DIVERGENCE_ACCEPTED count=<n> unprovable_players=<m>: <reason>"`(复用 `AuditLogRow`,**不改表结构**);写不进 → 拒绝(R3)。

### 7.5.5 配置(`internal/config/config.go`,全部 `json:",optional"`)

```go
GuildInternalRpc        zrpc.RpcClientConf `json:",optional"` // 不配 = 回档一律被拒(fail-closed),不是"跳过检查"
GuildClockSkewMarginMs  int64 `json:",default=300000"` // Validate [5000, 3600000];不得为 0,理由见 07 §7.2
GuildCheckBudgetSeconds int64 `json:",default=120"`    // 只罩检查阶段
```

- 单次调用超时**不设独立配置项**,就是 `GuildInternalRpc.Timeout`(§7.5.3-3)。常量(不进配置):`guildSettleDelay = 30s`、`guildRecheckDelay = 10s`、`guildRecheckBudget = 120s`、`maxDivergenceRows = 10000`、`maxDivergenceSample = 20`。

- 客户端用 `zrpc.NewClient`(返回 error)而不是 `MustNewClient`,且 yaml 里 `NonBlock: true`:**guild 不在线不得阻塞 data_service 启动**。依赖方向上 guild 启动要用 data_service(领号段、查名字,`go/guild/etc/guild.yaml:80-89` 已是 NonBlock),这里再反向阻塞就是启动死锁。建客户端失败 → ERROR 日志 + `GuildDivergence = nil` → 回档被拒,服务照常起。
- **data_service 怎么找到 guild(§7.11-U10,需用户拍板)**。前稿写"etcd key 抄 `guild.yaml`"是**错的**:`go/guild/etc/guild.yaml:9-11` 明文"刻意不写 go-zero 的 `Etcd.Key`(port-decisions D-13)……没有任何 Go 服务按 go-zero key 发现 guild;写了反而会被本地 `go_services.ps1 -Zone` 加上 `.z<N>` 后缀,把同一个全局服务切成按 zone 的碎片"。guild 只按 C++ 约定把 protojson `NodeInfo` 注册在 `GuildNodeService.rpc/` 前缀下,zrpc 的 etcd resolver 读不了这种值;今天全仓没有任何 Go 服务用 zrpc 客户端拨 guild。三条路:
  - **(c) 静态直连(推荐)**:dev yaml 写 `Endpoints: [127.0.0.1:50300]`(= `guild.yaml:3` 的 `ListenOn`);k8s 写 guild 的 Service DNS,随 D-14 一起落。`zrpc.RpcClientConf` 原生支持(`config.go:26`、`:93-94` `BuildDirectTarget`),**零额外文件、零新机制**。代价:guild 换端口要同步改这里;多副本时 dev 只连一个(任一副本都读同一个库,答案相同),k8s 由 Service 负载均衡。
  - (a) guild 追加 go-zero `Etcd.Key`:要正面推翻 D-13,并让 `go_services.ps1` 对 guild 豁免 `.z<N>` 后缀;+2 个手改文件(`guild.yaml`、`go_services.ps1`;后者的后缀逻辑本文未读,改法**未能确认**)。
  - (b) data_service 解析 `GuildNodeService.rpc/` 的 NodeInfo 自建 resolver:+2~3 个文件,为一条罕用的运维链路引入一套发现代码,不值(KISS)。
  - 无论哪条,没配 / 拨不通的方向都是拒绝。
- yaml 片段(按 (c)):
  ```yaml
  GuildInternalRpc:
    Endpoints:
      - 127.0.0.1:50300
    Timeout: 3000        # 单次调用上限;Validate [500, 3500]
    NonBlock: true
    Middlewares:
      Breaker: false     # 与 guild.yaml:88-89 对 DataServiceRpc 同取舍:本链路不重试、一次失败即整体拒绝,
                         # 熔断器帮不上忙,只会把"guild 抖了一下"在日志里变成 breaker 错误,误导排障
  ```
- k8s ConfigMap **本批不改**:guild 在 k8s 还没有 manifest(06:1825 D-14 遗留),data_service 在 k8s 里配了也拨不通;不配 = 拒绝 = 安全。

---

## 7.6 鉴权与拒绝语义 **(B5d 前需用户确认,91:52)**

### 7.6.1 现状

- 仓内 Go 服务互调一律**无凭据**:guild → data_service 是裸 `zrpc.MustNewClient`(`go/guild/internal/svc/guild_id_minter.go:30-31`),data_service 的拦截器链里没有会话层(`data_service.go:142-181`)。
- 内部 service 靠四层隔离挡客户端(`proto/trade/trade_admin.proto:13-18`),挡不住集群内进程。全仓没有 NetworkPolicy(`tools/scripts/k8s_deploy.ps1:2739-2742` 的注释自述)。
- 唯一的服务间签名在 login 私有包 `callerauth`,别的服务 import 不了;06:832 已裁决 `MatchInternal` v1 只靠"拒带会话 + NetworkPolicy",v1.1 再把 callerauth 提到 `go/shared`。
- assetop 的 HMAC 密钥**不可复用**:04:1670 明文"不得与其它密钥复用";而且方向相反——那把钥匙是 guild 向 scene 证明自己,交给 data_service 等于把读权限升级成发币权限。

### 7.6.2 推荐:方案 A + 放行口令

- **guild 的 `ListAppliedAssetOpsSince`:不加凭据**(方案 A),与 `TradeAdmin`、`MatchInternal` v1 同级。配套:`player_ids` 必填 ≤100、保留期外直接拒、只读无写面;给 90 G-05 追加一条 NetworkPolicy"guild gRPC 只放路由服、match、data_service",随 D-14(guild 上 k8s)一起落。
- **为什么够**:安全语义全在 data_service 一侧。攻击者能读到的只是资产流水摘要;他**冒充不了 guild 的应答**(data_service 是主动拨出的一方),也就骗不开这道闸。guild 不可达、报错、没配,结果都是拒绝。
- **放行开关必须带 `x-admin-token`**:复用现成的 `authorizeAdmin`(未配 `AdminToken` = 整个放行路径停用,`dataserviceserver.go:163-170`),零新机制。口径照 `ReleasePlayerName` 的"带了就必须验过"(`:575-592`)。

### 7.6.3 请用户回答的选择题

- **Q1. guild 内部 RPC 的鉴权** —— ① 方案 A:无凭据 + 会话拒绝 + 待补 NetworkPolicy(**推荐**,0 个额外文件);② 方案 B:加共享口令 metadata `x-internal-token`(guild 与 data_service 各读一个环境变量,+4 个文件,超 18 上限,且在 guild 身上造出第三种鉴权形态);③ 方案 C:把 `callerauth` 提到 `go/shared` 做 HMAC(最强,但要动 login 与密钥分发,超出 B5d,建议随 `MatchInternal` v1.1 一起做)。
- **Q2. 回档 RPC 自身的鉴权** —— ① 只有 `accept_guild_divergence=true` 时要求 `x-admin-token`(最小集,不改变未放行路径的现状)。**① 的代价**:无凭据的调用方发一次不带放行的 `Rollback*`,(i) 能从拒绝响应里读到目标玩家的资产流水——所以 ① 之下未鉴权调用只回计数、不回样本行(§7.5.4),代码多一个分支;(ii) 能让目标玩家 / 整个 zone 被栅栏挡住登录,最长 = 沉降 30s + 检查预算 120s,还会写一条 STARTED 审计。(ii) 在 ① 之下**无解**。今天这两条都被 `RollbackFence = nil` 挡在前面(`rollback_logic.go:54-56` 先回 NotImplemented),栅栏一接上即暴露。② 三个 `Rollback*` 一律要求 `x-admin-token`(+3 行;没有上述两条代价,也没有样本行的分支;生产回档今天本来就被栅栏关着,无兼容风险;dry-run 试探同样要带 token)。**推荐 ②**——它严格说是"顺手修 B5d 之外的缺口",所以需要你点头;本文其余部分两种选择都成立。
- **Q3. 写后复查发现新分歧时** —— ① 只报错 + 告警 + 留日志,人工按单补偿(**推荐**);② 自动用回档前安全快照把该玩家恢复回去(多一次高风险写,且恢复本身也要过闸,不推荐)。
- **Q4. B5d 是否拆成三个子批合入**(§7.10.1)—— ① 拆三批:B5d-1 账本读取(8 个手改)、B5d-2a guild 侧内部 RPC(7 个手改 + 8 个生成工程登记,含 C++ 编译)、B5d-2b data_service 回档闸(15 个手改,纯 Go);② 不拆,一次 30 + 8 = 38 个文件(按子批累加的上界;`data_service.proto`、`dataserviceserver.go`、`metrics.go` 在 B5d-1 与 B5d-2b 各出现一次,去重后是 27 + 8 = 35;两种算法都**超过** AGENTS §10.2 的 30 停手线)。**推荐 ①**;选 ② 需要你显式授权。

### 7.6.4 拒绝语义一览(data_service 视角)

| guild 侧情况 | data_service 行为 |
|---|---|
| `GuildInternalRpc` 未配 / 建客户端失败 | `ErrCodeRollbackGuildCheckFailed`,日志 `guild check unavailable: not configured` |
| 连接不上 / `DeadlineExceeded` / `Unavailable`(含 zrpc 客户端侧的超时;`Breaker` 已在 yaml 里关掉,不会出现 breaker 短路错误) | 同上,不重试 |
| `Unimplemented`(旧版 guild) | 同上(§7.10.4) |
| `FailedPrecondition` 且解析出 `cutoff_ms` | **不是终局**:记不可证明玩家,钳位重查该块一次(§7.5.3-2b),按重查结果落到下面三行 |
| `FailedPrecondition` 但解析不出 `cutoff_ms` / 钳位重查仍 `FailedPrecondition` | `ErrCodeRollbackGuildCheckFailed`(是 bug,ERROR 日志带原 message) |
| `PermissionDenied` / `InvalidArgument` / 任何其它 code | `ErrCodeRollbackGuildCheckFailed`(是 bug,ERROR 日志带 code) |
| 全部 OK,分歧为空**且**无不可证明玩家 | 通过 |
| 全部 OK,有分歧**或**有不可证明玩家,未放行 | `ErrCodeRollbackGuildDivergence` + 两个计数(+ 样本行,见 §7.5.4) |
| 同上,合法放行,分歧 ≤10000 | 写日志与审计后继续 |
| 分歧 > 10000 | `ErrCodeRollbackGuildCheckFailed`,放行无效 |

**放行能覆盖什么,一句话**:只覆盖"guild 答了,答案是有分歧 / 这部分我证明不了";**不覆盖**"guild 没答"。

---

## 7.7 竞态

**T1 检查通过后、写之前,guild 又终结了一行 APPLIED。** 两条来路:

- (a) 在线路径的答复在途:scene 在栅栏生效**之前**已应用并落盘,答复晚到 guild,Finalize 落在检查之后。
- (b) 离线账本读取(本批自己接的线,§7.8):Loop 在回档写**之前**读到"已落盘 APPLIED",Finalize 落在检查之后;随后回档把玩家侧抹回去——证据是回档前的,结论却活到了回档后。

两条路的共同点有两个,处置各对一个:

1. **"scene 已应用"到"guild 拿到证据"的时间有界,但不短。** (a) 答复在途 ≤ 一个 `OpBudget`(2.5s);答复丢了、或行是 APPLIED 但未 durable(`ActionAwaitDurable`,`reconcile.go:510-512` 重排 500ms)→ 走重投;目标已被栅栏踢下线,重投拿不到位置,要到 `attempts ≥ LedgerReadMinAttempts = 3`(`reconcile.go:280`、`:642`)才读账本。按默认参数(`BaseBackoff = 1s`、抖动上限 ×1.2,`decide.go:126-155`;`Interval = 2s`;`OpBudget = 2.5s`):三次退避 ≤ (1+2+4)×1.2 = 8.4s,四次轮询对齐 ≤ 8s,四次处理 ≤ 10s,合计 **≤ 约 27s**。
2. **"拿到证据"到"Finalize 提交"有硬上界**——投递预算 1800ms + 落库 700ms(04:1511),Finalize 事务子预算 2000ms(90 §2.8)。

前稿只用了第 2 条(写后复查),等于把最常见的时间线——玩家在被踢下线前一两秒捐献——设计成"先复制、事后才告警"。紧急 zone 回档恰恰是玩家活跃到最后一秒的场景,而且 zone 级的写阶段很长,这段时间里 Loop 会把所有"已落盘但 guild 仍 PENDING"的行逐个读账本终结,全变成写后分歧。订正为两段:

- **处置一:沉降等待(事前)。** Acquire 栅栏、解析完快照时刻之后,首次检查之前,等待 `guildSettleDelay = 30s`(常量;> 第 1 条的 27s)。此时零写入。栅栏生效**之前**已应用的操作在这 30s 内被在途答复或离线读账本终结,随后由首次检查在零写入阶段**拒绝**,而不是事后报警。
  - 等待走可注入的 `sleep func(context.Context, time.Duration)`(与复查共用一个钩子),用入口 ctx——此时还没写任何东西,调用方取消就该停。
  - 30s 盖不住的还有一类:**attempts 已高的长期 PENDING 行**(离线玩家的 SHOP / ACTIVITY_REWARD 行按设计长期 PENDING,§7.1.1 注 1)。玩家上线后这类行被投递并应用,若答复丢失,`ActionRetry` 按 `op.Attempts` 算退避,下一次读账本最晚在 `MaxBackoff × 1.2 = 72s` 之后,超过 30s。单人回档对此自洽(读账本晚于回档写,读到的是回退后的账本);`RollbackZone` 写阶段长,这类行可能在"检查之后、该玩家被写之前"被读账本终结,**落到写后复查事后告警**——不是静默漏报。不为这条窄路径把常量抬到 72s:每次回档都多持栅栏 40s,换的只是把"答复丢失 + 恰在回档窗口内"这一种情形从事后告警提前到事前拒绝。
  - 30s 是按 guild 的**默认**退避参数推出来的;`BaseBackoff` 取自配表 `GuildRule.asset_op_retry_base_ms`(`reconcile.go:248`),data_service 读不到。策划把它调大时 30s 会不够——不够的后果是退化回"靠写后复查事后抓",不是漏报;常量旁的注释写明这条依赖,手册同步(§7.9.3-2)。不做成配置项:它跟着另一个服务的配表走,给运维一个旋钮他也不知道该拧到多少。
  - 前提是 B5d-1(离线读账本)已接线。没接线时,掉线玩家的行停在 PENDING 直到玩家再上线;回档后账本一并回退,重投 = 首次应用,自洽(T2),这 30s 只是白等,无害。
  - 代价:每次回档多持栅栏 30s。回档是停机级运维操作,可接受;单人回档同样等(不为省 20s 分出第二个常量)。
- **处置二:写后复查(事后)。** 只负责"首次检查 ~ 写完"之间才拿到证据的行。最后一笔玩家数据写完后,等待 `guildRecheckDelay = 10s`(常量;> 1800 + 700 + 2000,再留一倍余量),用**同一组 `sinceMsByPlayer`** 再查一次,与检查阶段的 `op_id` 集合做差。
  - **复查(含等待)与其后的 RESULT 审计用脱钩 ctx**(§7.5.3-3),不继承调用方的取消;自带 `guildRecheckDelay + guildRecheckBudget` 的截止时间。超出该预算 → 按"复查失败"处理(下)。
  - 不可证明玩家(R2b)在复查里同样钳位处理;差集只比 `op_id`,"不可证明"本身不算新分歧(检查阶段已经放行过)。
  - **复查必须在栅栏释放之前做**(等待的 10s 也持栅栏):栅栏一放,玩家登录后新发生的捐献 / 购买同样满足 `F > S`,会被复查当成"新分歧"误报紧急告警。代价是目标玩家 / zone 多被挡 10s 登录,回档本来就是停机操作,可接受。
  - 差集为空 → 正常结束。
  - 差集非空 → 数据已写、不自动撤销(Q3):每行写 ERROR `[Rollback][GuildDivergence] post-write …`(字段同 §7.5.4),响应 `ErrCodeRollbackGuildDivergedAfterWrite` + 计数 + 前 20 条,RESULT 审计带该码,指标 +1 触发告警。运维按单补偿,或用响应里的 `pre_rollback_snapshot_id` 回滚这次回档。
  - 复查调用本身失败 → 同样回 `ErrCodeRollbackGuildDivergedAfterWrite`(无法证明没有新分歧),日志注明 `recheck failed`。
- **剩余窗口**:回档写**之后**才读账本的 Loop 读到的是回档后的旧账本 → "未见" → 继续等,自洽,不是分歧。证据在写之前、Finalize 却晚于写后 10s 才提交,需要 guild 进程停顿超过 10s 后事务仍然提交成功;而 Finalize 自带 700ms 超时且不继承父 ctx(04:1511-1513),停顿后醒来 ctx 已过期、事务提交不了。剩余概率视为可接受,并由 §7.9 的 `assetop_*` 既有告警间接覆盖。
- **为什么不用"前置条件:无 PENDING 行"**:接口只列已应用行,查 PENDING 要改形状;更要紧的是离线玩家的 SHOP / ACTIVITY_REWARD 行按设计长期 PENDING(04:1536),拿它当前置条件等于 zone 回档永远过不了闸。
- **两段等待的代价**:每次回档 RPC 多等 30s + 10s(zone / server 级各只等一次,不是每玩家一次)。调用方超时与服务端 `MethodTimeouts`(§7.5.3-3)都**建议** ≥ 30s + 检查预算 + 写耗时 + 10s + 复查预算,写进手册。不足不影响正确性:调用方等不及先断开、或服务端入口 ctx 先到点,调用方收到的是 `DeadlineExceeded`、拿不到响应体;写后阶段靠脱钩 ctx 跑完,结论以 RESULT 审计与日志为准。

**T2 PENDING 行在回档后重投。** §7.1.1 注 1 已论证自洽。补一条边界:回档后账本 `max_seq` 变小,guild 的 `next_seq` 不变,差距 ≤ 1024 时新 seq 判 Unseen 正常应用(`go/shared/assetop/classify.go:79-131`);> 1024 判 JumpTooFar → UNKNOWN(04:1187),见 §7.8.1。

**T3 与 B4c 存盘属主围栏的关系。** 回档要真正作用到 scene 读的 blob,必须(a)写 PlayerAllData key,(b)先把属主 SET 为 `rollback:<毫秒>`(04:1414),否则旧 scene 的晚到存盘会把回档结果盖掉——盖掉之后玩家侧其实**没回档**,而 guild 侧的"分歧已放行"日志却说回了,补偿就会补错。这两件事都不在 B5d:B5d 的闸不依赖它们,但**手册写明**:B4c 与"回档写 PlayerAllData"落地之前,放行日志只能当线索,补偿前必须人工核对玩家当前资产(§7.9.3)。

---

## 7.8 账本

### 7.8.1 回档后的账本:要不要抬纪元?**不要。**

- 玩家回档让账本回到旧值,`stream_epoch` 也回到旧值(同一条记录)。guild 侧 `guild_player_op_seq.epoch` 没动,通常两者相等(纪元只在库恢复手册里抬),新操作照常工作。
- 04 §4.31 的"抬纪元手册"解决的是**反方向**的问题:帮会库回退、玩家账本不回退。玩家回档时抬纪元反而有害——旧纪元的 PENDING 行会被判 StaleEpoch → UNKNOWN → 转人工(04:357 C11),把本来自洽的 T2 变成卡死行。
- **唯一需要动手的情形**:某玩家在 `S` 之后的操作数 > 1024,回档后新 seq 命中 JumpTooFar。这在一个回档窗口内对单个玩家几乎不可能(日限购与捐献次数远小于此);万一发生,`assetop_unknown_total` 告警会响(04:1559),届时**对该玩家单独**按 §4.31 抬纪元(用当前毫秒)。不做自动化(YAGNI)。
- 回档抹掉了账本里 `(S, now]` 的"已见"位。guild 对这些 seq 要么已终结(不重投),要么仍 PENDING(重投,自洽)。I2"结局固定"在回档下被人为打破是 D3 接受的代价,闸与放行日志就是为它兜底。

### 7.8.2 `GetPlayerAssetOpLedger`(data_service)

- **读哪个 key**:`fmt.Sprintf("%s:%d", (&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName(), playerID)`——前缀取 message 全名,不手写字面量(先例 `go/login/internal/logic/clientplayerlogin/player_class_backfill.go:31-40`、`go/match/internal/team/presence.go:45-51`)。**不是** `player:{id}:<field>`:那套 key 里没有账本,照 04:1558 的字面"读玩家 blob"去读它会永远 `found=false`。
- 路由:`Router.ClientForPlayer`(`internal/routing/router.go:249`)→ `GET` → `proto.Unmarshal` 成 `PlayerAllData` → `GetPlayerDatabaseData().GetAssetOpLedger()`。
- 语义:

| 情况 | 响应 |
|---|---|
| key 不存在(`redis.Nil`) | `found=false`,无 error。scene 存盘是不带 TTL 的裸 SET,key 缺失只可能是"从未入场"或 Redis 丢数据(C5) |
| key 在,`player_database_data.player_id != 请求 id`,或反序列化失败 | gRPC `Internal`(数据已矛盾,不能当"没有") |
| home_zone 查不到 / Redis 错误 | gRPC `Unavailable` / `Internal`,**不得**回 `found=false` |
| 正常 | `found=true` + 账本(玩家从未有资产操作时为空消息) |

- **不回退读 MySQL**:04:1532 只承认"已在 Redis 即 durable";MySQL 落库是异步 DBTask,可能更旧,会把已记账的 seq 误判为未见。
- 只读、不鉴权(与 `BatchGetPlayerName` 同级,`dataserviceserver.go:598`)。账本是位图 + 少量拒绝码,不含资产数额。

### 7.8.3 `Loop.Ledger` 接线(Y-06)

- **适配器放 `go/shared/assetop/ledger_dataservice.go`**:`type DataServiceLedger struct{ Client dspb.DataServiceClient; Timeout time.Duration }`,实现 `ReadPersistedLedger`(签名以磁盘为准,`reconcile.go:173-175`)。放 shared 是因为 trade 也留着同一个空位(`go/trade/internal/reconcile/pipeline.go:165-186` 只设了 `Manual`),两个服务各写一份迟早分叉。shared 已依赖 `proto` 模块,不引新依赖。
- 映射:`found=false → (nil, nil)`(Loop 计 `absent`,继续退避);任何 gRPC 错误 → `(nil, err)`(Loop 计 `error`,**原样继续 Retry,不终结**,`reconcile.go:646-652`)。
- 超时:调用方传入的 ctx 是投递预算的剩余部分(`ProcessOne` 传 `applyCtx`),适配器再套 `Timeout`(guild 配 300ms)——读账本只是"可能省一次卡死",不值得吃掉 1800ms 里的大头。
- guild 装配:B5b 建 `assetop.Loop` 的那个文件(**落码时以磁盘为准**,B5b 尚未落码;trade 的对应物是 `internal/reconcile/pipeline.go`)里,把 `loop.Ledger = nil` 换成 `&assetop.DataServiceLedger{Client: svcCtx.DataServiceClient, Timeout: 300ms}`。`DataServiceClient` 为 nil(未配 `DataServiceRpc`)时保持 `Ledger = nil` 并打一条 INFO——离线读账本是优化,不是正确性路径。
- `LedgerReadMinAttempts`(默认 3,`reconcile.go:280`):一行至少重投失败 3 次才去读账本,避免每个刚下线的玩家都打一次 data_service。不改。
- trade 的接线**不在本批**(不碰别的会话的服务),只在交付说明里点一句"适配器已在 shared,可直接用"。

---

## 7.9 可观测性

### 7.9.1 data_service 指标(直连 `prometheus/client_golang`,懒注册 `Observe*`;**不能**用 go-zero `core/metric`,`internal/metrics/metrics.go:119-123`)

- `data_service_rollback_guild_check_total{scope, result}`:scope ∈ `player/zone/server`;result ∈ `clean / divergent_rejected / divergent_accepted / unavailable / retention / truncated / budget / post_write_divergent / post_write_recheck_failed`。
- `data_service_rollback_guild_divergence_rows{scope, accepted}`(counter,累加行数;accepted ∈ `true/false`)。
- `data_service_rollback_guild_check_seconds{scope}`(histogram)。
- `data_service_asset_op_ledger_read_total{result}`:result ∈ `found / absent / error`。
- 归类优先级(一次回档只计一个 result):被拒且**有分歧行** → `divergent_rejected`(无论是否同时有不可证明玩家);被拒且**只有**不可证明玩家 → `retention`;合法放行(无论放的是分歧、不可证明还是两者) → `divergent_accepted`。这样"`divergent_rejected` 不告警、`retention` 通知"两条告警口径才不互相吞。
- **启动预置为 0(必须)**:metrics 包提供 `PrimeRollbackGuildCheck()`,对 scope × result 的封闭集合(3 × 9 = 27 条,基数有界)逐个 `WithLabelValues(...).Add(0)`;`data_service.go` 在 `metrics.Start` 之后调用。理由是 92-handoff:184-189 已经踩过的坑:`CounterVec` 的子序列在首次 `WithLabelValues` 之前不存在(本包是懒注册,`metrics.go:187-190` 同款),`increase(...) > 0` 对"从无到 1"的序列在窗口内只有一个样本,**恰好漏掉第一次**;而放行回档与 post-write 分歧都是"进程一辈子可能只发生一次"的事件——最需要告警的那一次正好是哑的。同仓先例:`data_service.go:267-273` 为 `kafka_consumer_up` 预注册 0。测试 M1。

> **前提缺口**:`etc/data_service.yaml` 与 k8s ConfigMap 都没配 `MetricsListenAddr`(`config.go:42`;`data_service.go:110`),data_service 的 `/metrics` 今天是关的。本批在 dev yaml 里补上该项;k8s 侧随 D-14 一起。**在那之前告警以日志关键字为准**(下表第二列)。

### 7.9.2 告警

| 口径 | 指标表达式 | 日志关键字(Loki) | 级别 |
|---|---|---|---|
| 有人放行了分歧回档 | `increase(data_service_rollback_guild_check_total{result="divergent_accepted"}[10m]) > 0` | `[Rollback][GuildDivergence] accepted` | 通知(必须有人认领补偿单) |
| 写后复查发现新分歧 / 复查失败 | `increase(…{result=~"post_write_.*"}[10m]) > 0` | `[Rollback][GuildDivergence] post-write` | **紧急** |
| 回档因 guild 不可达被拒 | `increase(…{result=~"unavailable|budget"}[10m]) > 0` | `guild check unavailable` | 警告 |
| 回档因保留期不可证明被拒 | `increase(…{result="retention"}[10m]) > 0` | `[Rollback][GuildDivergence] unprovable rejected` | 通知 |
| 有人带会话调内部 RPC(客户端探测) | —(见下) | `拒绝客户端调用内部方法` + `GuildInternal`(`session.go:104` 现成的 ERROR 行) | 警告 |

- 上表前四行的 `increase > 0` 依赖 §7.9.1 的启动预置,否则第一次不响。
- 第五行**只用日志关键字**。前稿引用的"grpcstats 上的 PermissionDenied 计数"不存在:`go/shared/grpcstats/collector.go:1-13、31-38` 只统计每方法的次数 / 字节 / 延迟并周期性打日志,没有 gRPC code 维度,也不出 Prometheus 指标。go-zero 自带的 `rpc_server_requests_code_total{method,code}`(guild 已配 Prometheus,`guild.yaml:133-136`)可作补充,指标名**未能确认**(未读 go-zero 的 prometheus 拦截器),落码时核对后再写表达式。
- `divergent_rejected` **不告警**:那是闸在正常工作。

### 7.9.3 运维手册要点(G-07:写进 `docs/design/guild-phase2.md` 的"运维"节,文档不计手改名额)

1. 回档前先**不带**放行开关发一次,看 `guild_divergence_count`、`guild_unprovable_player_count` 与前 20 条(Q2 选 ② 时这一次也要带 `x-admin-token`;选 ① 时不带 token 只看得到两个计数)。看错误码:`…GuildDivergence` = 带放行能过;`…GuildCheckFailed` = 放行也过不了,先修 guild 连通或缩小范围。
2. 决定放行:带 `accept_guild_divergence=true`、`x-admin-token`、非空 `reason / operator`;调用方超时 ≥ 沉降 30s + 检查预算 + 写耗时 + 10s + 复查预算,且不超过 yaml 里该方法的 `MethodTimeouts`。策划若把 `GuildRule.asset_op_retry_base_ms` 调到 1000 以上,通知服务端复核 `guildSettleDelay`(§7.7)。
3. 补偿清单从 Loki 按 `[Rollback][GuildDivergence] accepted` + `operator` 取;每行按 `op_id` 回查 `guild_asset_op`(`payload / ref_id / ref_count`)得到"具体是什么":
   - DONATE:扣回帮会资金 `funds_delta`、该玩家帮贡 `contribution_delta`(帮贡已花掉则记欠账,人工裁定);
   - SHOP:补发物品,或退回帮贡并退限购次数;
   - ACTIVITY_REWARD:补发奖励(delta 为 0,只能看 payload)。
4. **B4c 与"回档写 PlayerAllData"落地前**,补偿前必须人工核对玩家当前资产——回档可能根本没作用到 scene 读的那份 blob(§7.7-T3)。
5. 看到 `post-write`:立即按第 3 步处理差集行,或用 `pre_rollback_snapshot_id` 撤销这次回档(撤销同样过闸)。
6. `guild_unprovable_player_count > 0`(日志 `[Rollback][GuildDivergence] unprovable`,列出是谁):这些玩家的快照比帮会流水保留期(默认 30 天)还老,保留期之前那段流水已被清理,系统无法证明。三条出路,按优先级:① 这些玩家本来就不需要回档 → 改用逐玩家回档,避开他们;② 需要回档且人工核过帐 → 与分歧同一个放行开关(第 2 步),日志记 `unprovable accepted`,**系统给不出补偿单**,放行人自担;③ 都不是 → 不回档。保留期内仍可证明的那段照常出现在分歧清单里。
7. 同步订正两处旧口径:`rollback_logic.go:378-382` 的 "Guild/friend data is NOT rolled back … self-healing" 注释改为指向本文;`docs/design/zone_data_rollback.md:53-62` 追加"帮会资产见 guild-phase2/07"。

---

## 7.10 文件清单、测试、验证、升级

### 7.10.1 手改文件(生成物、文档不计)

**B5d-1 账本读取(8 个;依赖 B5b 的 Loop 已落码)**

| # | 路径 | 改什么 |
|---|---|---|
| 1 | `proto/data_service/data_service.proto` | import 账本 proto;`GetPlayerAssetOpLedger` 及两条消息 |
| 2 | `go/data_service/internal/logic/asset_op_ledger_logic.go`(新) | §7.8.2 |
| 3 | `go/data_service/internal/logic/asset_op_ledger_logic_test.go`(新) | L1–L5 |
| 4 | `go/data_service/internal/server/dataserviceserver.go` | 新 handler(薄包装) |
| 5 | `go/data_service/internal/metrics/metrics.go` | `ObserveAssetOpLedgerRead` |
| 6 | `go/shared/assetop/ledger_dataservice.go`(新) | §7.8.3 适配器 |
| 7 | `go/shared/assetop/ledger_dataservice_test.go`(新) | A1–A3 |
| 8 | guild 装配 Loop 的文件(B5b 产物) | `Ledger` 接线 |

**B5d-2a guild 侧内部 RPC(7 个手改 + 8 个生成工程登记;含 C++ 编译)**

| # | 路径 | 改什么 |
|---|---|---|
| 1 | `proto/guild/guild_internal.proto`(新) | §7.3.1 |
| 2 | `go/guild/internal/data/asset_op_divergence_repo.go`(新) | §7.4.1 SQL |
| 3 | `go/guild/internal/data/asset_op_divergence_repo_test.go`(新) | G1–G6、G5b |
| 4 | `go/guild/internal/server/guild_internal_server.go`(新) | §7.4.2 校验、保留期(含 `cutoff_ms` 格式常量)、指标与预置 |
| 5 | `go/guild/internal/server/guild_internal_server_test.go`(新) | G7–G9 |
| 6 | `go/guild/guild.go` | 注册 `GuildInternal`;`MustNewServer` 之后预置指标 |
| 7 | `go/guild/internal/session/session_test.go` | G10 |

**生成工程登记(8 个,单列、不挤手改名额,口径同 91:29 的"+生成工程登记")。** 前稿写"不改任何 C++"是**错的**:新 proto 文件占消息号,就必然进 `cpp/generated/rpc/service_metadata/rpc_event_registry.cpp` 的注册表(模板 `trade_admin` 在该文件 `:37` include `trade_admin.pb.h`、`:70` include metadata 头、`:251` 前向声明 `trade::SendTradeAdminSeedListing`、`:1463-1467` 写进 `gRpcMethodRegistry`);该注册表已在工程里,它引用的新符号定义在**新生成的** `.cc/.cpp` 里,而 C++ 工程文件是**手工登记**的(`tools/proto_generator/protogen` 下没有任何写 vcxproj / CMakeLists 的代码;先例 `5247d273a proto.vcxproj 补登记 15 个已生成头文件`)。不登记 = 全部 C++ 节点 LNK2019。照 `trade_admin` 的登记位逐条对齐:

| # | 路径 | 加什么(对齐位) |
|---|---|---|
| R1 | `cpp/generated/proto/CMakeLists.txt` | `guild/guild_internal.grpc.pb.cc`、`guild/guild_internal.pb.cc`(紧随 `:119-121` 的 guild 三行,即 `guild/guild_db.pb.cc` 之后) |
| R2 | `cpp/generated/proto/proto.vcxproj` | 两个 `ClCompile`(`:121-123` 之后)+ 两个 `ClInclude`(`:239-241` 之后) |
| R3 | `cpp/generated/proto/proto.vcxproj.filters` | 同上四项(照 `trade_admin` 的 `:306-309`、`:644-647`) |
| R4 | `cpp/generated/grpc_client/CMakeLists.txt` | `guild/guild_internal_grpc_client.cpp`(`:46` 之后) |
| R5 | `cpp/generated/grpc_client/grpc_client.vcxproj` | `.cpp`(`:31` 之后)+ `.h`(`:51` 之后) |
| R6 | `cpp/generated/grpc_client/grpc_client.vcxproj.filters` | 同上两项 |
| R7 | `cpp/generated/rpc/rpc.vcxproj` | `service_metadata\guild_internal_service_metadata.h`(`:46` 之后) |
| R8 | `cpp/generated/rpc/rpc.vcxproj.filters` | 同上(`:117` 之后) |

生成文件的**确切文件名以 proto-gen 的实际产物为准**(上表按 `trade_admin` 的命名规律推定,未跑生成,**未能确认**);先跑生成、`git status` 看到新文件名之后再登记。动手前对这 8 个文件单独 `git status`——`cpp/generated/table/*` 此刻有别的会话在途改动,这三个工程也可能有。

**B5d-2b data_service 回档闸(15 个;纯 Go)**

| # | 路径 | 改什么 |
|---|---|---|
| 1 | `proto/data_service/data_service.proto` | 3 个请求字段、9 个响应字段、`RollbackGuildDivergence` |
| 2 | `go/data_service/internal/logic/guild_divergence.go`(新) | 接缝、分块翻页、保留期钳位、过滤、日志 |
| 3 | `go/data_service/internal/logic/guild_divergence_test.go`(新) | D1–D6、D15–D16 |
| 4 | `go/data_service/internal/logic/rollback_logic.go` | §7.5.2 流程、沉降等待、plan/execute 拆分、R5 断言、脱钩 ctx、写后复查、订正 `:378-382` 注释 |
| 5 | `go/data_service/internal/logic/rollback_recall_test.go` | D7–D14、D17–D18(已有文件,追加) |
| 6 | `go/data_service/internal/server/dataserviceserver.go` | 鉴权(按 Q2)、三个 handler 透传新字段 |
| 7 | `go/data_service/internal/svc/servicecontext.go` | `GuildDivergence` 字段与装配(nil = 拒绝) |
| 8 | `go/data_service/internal/config/config.go` | §7.5.5 三项 + Validate |
| 9 | `go/data_service/internal/config/config_test.go` | C1(已有文件,追加) |
| 10 | `go/data_service/internal/constants/error_codes.go` | 三个码 |
| 11 | `go/data_service/internal/store/snapshot_store.go` | `GetSnapshotPlayerTimesByZone` |
| 12 | `go/data_service/etc/data_service.yaml` | `GuildInternalRpc`(§7.5.5 片段)、三条 `MethodTimeouts`、`MetricsListenAddr` |
| 13 | `go/data_service/data_service.go` | `FaultCodeSet` 加两个码;`metrics.Start` 之后调 `PrimeRollbackGuildCheck()` |
| 14 | `go/data_service/internal/metrics/metrics.go` | 三个回档指标 + `PrimeRollbackGuildCheck` |
| 15 | `go/data_service/internal/metrics/metrics_test.go` | M1(已有文件,追加) |

- **为什么拆成 2a / 2b 而不是硬塞 18**:逐项列清后回档闸是 22 个手改 + 8 个登记,一批放不下。按服务切开恰好对上部署顺序(guild 先、data_service 后,§7.10.4):2a 合入后 guild 多一个没人调的只读 RPC,无害;2b 不碰 C++ 工程(`data_service.proto` 只追加字段,重生的 `.pb.cc` 早已在工程里)。两批各自 ≤18。
- **不改**:`session.go`、`tables.go`、`Tip.xlsx`、`MessageLimiter.xlsx`、`k8s_deploy.ps1`、`guild.yaml`(按 U10 选 (c))、手写的 C++ 源码、客户端仓。
- **Y-14 串行**:`config.go`、`etc/data_service.yaml`、`id_segment_store.go` 此刻有 B5a 的在途改动(`git status`)。B5d 落码前必须等 B5a 提交,动手前再 `git status` + ListAgents 报文件范围;提交只用 `git commit -- <显式路径>`。

### 7.10.2 测试(面向接口;fake 经同一接缝注入;不依赖墙钟——`now` 由参数传入)

**guild**
- G1 只返回 APPLIED / APPLIED_PARTIAL;PENDING / REJECTED / ABORTED 不出现。G2 `next_attempt_ms == since` 不返回,`since+1` 返回(严格大于)。
- G3 翻页:造 1201 行,limit 500 → 三页,`op_id` 严格升序、无重无漏,末页 `next_after_op_id == 0`;恰好 500 行时一页且 next = 0(`limit+1` 判定)。
- G4 zone 过滤:`zone_id=0` 全返回;`zone_id=Z` 时保留本 zone 与**已解散帮会**(LEFT JOIN)的行,滤掉别的 zone。G5 人工终结的 APPLIED 行可被查到。**G5b 经真实 `GuildAssetStore.Finalize`(不是直接插行)终结一行,断言 `next_attempt_ms == 传入的 nowMs`,且 `since = nowMs − 1` 时能被 `ListAppliedAssetOpsSince` 查到**(钉住 §7.4.1 对 B5b 的硬要求)。G6 SQL 字面量守卫:`rg -n "(status|kind|stream)\s*(=|IN)\s*\(?[0-9]" asset_op_divergence_repo.go` 为 0。
- G7 入参表(§7.4.2)逐行。G8 保留期:`since = now − 30d + 59min` 拒、`+61min` 过;拒绝的 message **逐字**等于样例 `since_ms older than terminal retention; cutoff_ms=<n>` 且 `n` = 公式右端。G9 Store 未装配 → `Unavailable`。
- G10 `GuildInternal` 的方法全不在 `ClientMethods`;带会话 metadata → `PermissionDenied`;不带 → 放行。

**data_service**
- D1 分块:250 个玩家 → 3 次首页调用,每次 ≤100 id、`since = 块内最小值`。D2 **`GuildDivergence == nil` → `CheckFailed`,零写入**(钉住"nil 不是跳过")。D3 逐玩家过滤:同块内快照新的玩家,其早于自身 since 的行被滤掉。D4 任一页 error → 整体 error。D5 > 10000 行 → `CheckFailed`,放行也不过。D6 总预算耗尽 → `CheckFailed`。
- D7 有分歧未放行:`ErrCodeRollbackGuildDivergence`、count、≤20 条样本;fake Redis **零写**、**无安全快照**;审计有 STARTED 与带码的 RESULT。D8 放行缺 token → `PermissionDenied`;缺 reason/operator → `InvalidRequest`。D9 合法放行:ACCEPTED 审计在第一笔写之前;写发生。D10 ACCEPTED 审计写失败 → 零写入。
- D11 `RollbackZone`:第 2 块失败 → 第 1 块的玩家也没被写(R4)。D12 `RollbackAll`:zone 2 plan 被拒 → zone 1 零写入。D13 R5:执行期快照早于计划值 → 该玩家失败不写。D14 写后复查:fake 第二次多返回一行 → `DivergedAfterWrite`;复查 error → 同码。沉降与复查的等待共用一个可注入的 `sleep func(context.Context, time.Duration)`,测试里置零或当钩子用。
- D15 保留期钳位(checker 层):块内 3 个玩家、其中 1 个 `since < cutoff`;fake 首次回 `FailedPrecondition`(用 G8 的同一条样例字符串)→ 断言第二次调用 `since_ms == cutoff`、`UnprovablePlayerIDs == [该玩家]`、其余玩家的行照常按各自 since 过滤。D16 message 解析不出 `cutoff_ms` → error;钳位重查仍 `FailedPrecondition` → error。
- D17 不可证明 × 放行两态(logic 层):未放行 → `ErrCodeRollbackGuildDivergence`、`guild_unprovable_player_count == 1`、零写入、有 `unprovable rejected` 日志;合法放行 → `unprovable accepted` 日志先于第一笔写、写发生、ACCEPTED 审计含 `unprovable_players=1`。
- D18 时序与 ctx:(a) **沉降**——fake checker 在 sleep 钩子被调用**之前**返回空、之后返回一行分歧;断言首次检查发生在沉降之后 → 拒绝、零写入。(b) **脱钩 ctx**——入口 ctx 在最后一笔写完后被取消;断言复查仍被调用且拿到未取消的 ctx、RESULT 审计写入成功、结论是 `clean` 而不是 `post_write_recheck_failed`。(c) 复查超出 `guildRecheckBudget` → `DivergedAfterWrite` + `recheck failed`。
- C1 配置:`GuildClockSkewMarginMs` = 0 / 4999 → `Validate` 报错,5000 过;已配 `GuildInternalRpc` 且 `Timeout` = 499 / 3501 → 报错。
- M1 指标预置:`PrimeRollbackGuildCheck()` 之后 `Gather`,能看到 `scope="player",result="divergent_accepted"` 且值为 0 的序列,27 条齐全。
- L1 `redis.Nil` → `found=false`。L2 正常 → 账本逐字段相等。L3 `player_id` 不符 → `Internal`。L4 坏字节 → `Internal`。L5 读的是 PlayerAllData 全名 key(往 `player:{id}:*` 写数据,断言读不到)。

**shared**
- A1 `found=false → (nil,nil)`。A2 gRPC error → `(nil, err)`。A3 超时生效(fake 阻塞 → `DeadlineExceeded`)。

### 7.10.3 给用户 / Codex 的验证序列(Claude 不执行;未编译,待验证)

前置:B5a、B5b 已合入;本机已装 Python(proto-gen 需要);MySQL / Redis 本地可用(repo 测试)。protoc 与 protoc-gen-go 在 PATH。**B5d-2a 涉及 C++ 工程**(新 proto 文件的生成物要登记并编译,§7.10.1);MSBuild 必须串行 `/m:1 /nr:false`,同一时刻只允许一个构建方(90 G-08),开跑前与其它会话确认没人在跑导表 / proto-gen / MSBuild。B5d-1、B5d-2b 只追加字段与方法,不新增 C++ 文件,但重生的 `.pb.cc` 同样要过步 4 的编译。

| 步 | 工作目录 | 命令 | 期望 / 通过标准 | 失败保留 |
|---|---|---|---|---|
| 1 | 仓库根 | `git status --short proto go/guild go/data_service go/shared cpp/generated/proto cpp/generated/grpc_client cpp/generated/rpc` | 只有 B5d 清单内的文件(含 8 个登记文件);别的会话的在途改动逐个认领清楚 | 输出全文 |
| 2a | 仓库根 | `.\dev.bat gen`(导表 + proto-gen;写法同 92-handoff:257) | 退出码 0;新出现 `go/proto/guild/guild_internal.pb.go`、`guild_internal_grpc.pb.go`,以及 `cpp/generated` 下的 `guild_internal.*pb.{h,cc}`、`guild_internal_grpc_client.{h,cpp}`、`guild_internal_service_metadata.h`(确切文件名以 `git status` 为准,**据此回填 §7.10.1 的 R1–R8**)。生成器报 `kMaxRpcMethodCount` 超限即停,**不手改**(G-06) | 生成器完整输出 + `git status --short` |
| 2b | `go\` | `pwsh -NoProfile -ExecutionPolicy Bypass -File .\build.ps1` | 退出码 0。**不用 `build.bat`**:它末尾有 `pause`(`go/build.bat:3`),非交互环境会挂住;且 `build.ps1` 只重生 goctl 包装、**不重生 `.pb.go`**(`go/build.ps1:3-12` 自述),所以 2a 不能省 | 控制台全文 |
| 3 | 仓库根 | `rg -n "ListAppliedAssetOpsSince\|GetPlayerAssetOpLedger" go/client_rpc_router/generated` | 两个方法所在条目 `ClientProtocol: false`(G-01),节点类型 = guild / data_service;记下新的 `kMaxRpcMethodCount` | 命中行 |
| 4 | 仓库根 | 逐工程串行:`msbuild <vcxproj> /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal`,顺序 `cpp/generated/proto/proto.vcxproj` → `cpp/generated/grpc_client/grpc_client.vcxproj` → `cpp/generated/rpc/rpc.vcxproj` → `cpp/nodes/gate/gate.vcxproj`(至少链接一个节点 exe;中间依赖缺了就改跑 `msbuild game.sln` 同参数) | 0 error;**无 C1083(头文件没登记 / 没生成)、无 LNK2019(`.cc/.cpp` 没登记)**。错误落在 `cpp/generated/table/*` 或别的会话的文件上 → 不归本批,原样上报 | 首个 error 前后 30 行 |
| 5 | `go\shared` | `go vet ./assetop/... && go test ./assetop/... -run "Ledger" -count=1` | A1–A3 绿 | 失败用例输出 |
| 6 | `go\guild` | `go vet ./... && go test ./internal/data/... ./internal/server/... ./internal/session/... -count=1` | G1–G10、G5b 绿 | 同上 + 测试库 `SHOW CREATE TABLE guild_asset_op` |
| 7 | MySQL 客户端 | 对 §7.4.1 的 SQL 跑 `EXPLAIN`,两种形状各一次:100 个 id + `since` = 1 小时前;1 个 id + `since` = 29 天前(`zone_id=0`)。表里先灌 ≥ 1 万行终态数据,空表的计划没有参考价值 | `key` ∈ {`idx_guild_asset_op_0`, `idx_guild_asset_op_2`}。选了 `PRIMARY` → 按 §7.4.1 的退路加 `USE INDEX`,重跑本步 | EXPLAIN 全文 |
| 8 | `go\data_service` | `go vet ./... && go test ./internal/... -count=1` | D1–D18、C1、M1、L1–L5 绿;既有回档用例不回归 | 失败用例输出 |
| 9 | 仓库根 | `rg -n "(status\|kind\|stream)\s*(=\|IN)\s*\(?[0-9]" go/guild/internal/data/asset_op_divergence_repo.go` | 0 命中 | 命中行 |
| 10 | 本地联调 | 起 guild + data_service;`grpcurl -plaintext -d '{"player_ids":[1],"since_ms":<now-1h>}' 127.0.0.1:50300 guildpb.GuildInternal/ListAppliedAssetOpsSince` | 返回空 `ops`;`since_ms` 取 40 天前 → `FailedPrecondition`,message 含 `cutoff_ms=`。data_service 启动日志**没有**"GuildInternalRpc 建客户端失败"(验证 U10 的直连配置) | 两端日志尾 200 行 |
| 11 | 本地联调 | `curl -s http://<MetricsListenAddr>/metrics \| rg rollback_guild_check_total` | 启动后未发生任何回档,即可见 27 条值为 0 的序列(§7.9.1 预置) | 输出全文 |
| 12 | 本地联调 | 停掉 guild,调 `RollbackPlayer` | 今天仍是 `ErrCodeNotImplemented`(栅栏为 nil,先于本闸返回)——**这是预期**;本闸的端到端只能靠单测里注入 fake 栅栏验证,直到栅栏有生产实现 | — |

按子批裁剪:B5d-1 跑 1、2a、2b、3、4、5、8(L 系列);B5d-2a 跑 1–4、6、7、9、10(guild 半边);B5d-2b 跑 1–4、8、10–12。

没有 Codex / 用户的运行结果之前,不得写"编译通过""测试绿"。

### 7.10.4 滚动升级与回滚

- **部署顺序:guild 先,data_service 后。** 反过来也安全:新 data_service 拨旧 guild 得到 `Unimplemented` → `CheckFailed` → 拒绝回档(R2)。任何顺序、任何并存窗口,失败方向都是"回档被拒",不是"回档裸奔"。
- **协议兼容**:请求只追加字段且默认 false;响应只追加字段;旧调用方不受影响。`guild_internal.proto` 是新文件新 service,不动任何既有消息号之外的东西(消息号由生成器追加,G-06)。
- **旧 data_service + 新 guild**:无交互,新 RPC 无人调用。
- **`GetPlayerAssetOpLedger` 不可用时**:guild 的适配器拿到 `Unimplemented` → `(nil, err)` → Loop 计 `error` 并照常 Retry,不终结(R7)。副作用是 `assetop_store_errors_total{op="ledger_read"}` 在并存窗口内上升,属预期;窗口过后应归零。
- **回滚**:
  - 回滚 data_service 二进制 → 失去这道闸;但生产栅栏仍为 nil,回档仍整体停用,不裸奔。**栅栏一旦有了生产实现,data_service 就不得回滚到 B5d 之前**,写进手册。
  - 只想停掉离线读账本:guild 配置里把 `DataServiceRpc` 留着、把 `Ledger` 接线用开关关掉不值得加配置(YAGNI)——回滚 guild 二进制即可,B5b 的行为(`Ledger=nil`)本来就是完整可用的。
  - 无库表变更,无数据迁移,无需回填。

---

## 7.11 未决项(接口形状不许改,以下只提最小修补,**等用户拍板,不擅自做**)

- **U1(= §7.6.3 的 Q1–Q4)**:guild 内部 RPC 鉴权方案;回档 RPC 是否一律要 admin token;写后复查发现分歧的处置;B5d 是否拆三批。
- **U2 复合游标**:单个 `after_op_id` 表达不了索引序游标,本文以"按 `op_id` 排 + filesort"绕开(§7.4.1)。若将来回档窗口内的行数大到 filesort 成为问题,最小修补 = 请求追加 `uint64 after_next_attempt_ms`、响应追加 `next_after_next_attempt_ms`(只追加,不破坏现形状)。现在不做。
- **U3 已知误报**:`F > S` 但 scene 的应用早于快照的行会被误判为分歧(§7.2)。最小修补 = `GuildAssetOpBrief` 追加 `seq = 10`、`stream_epoch = 11`,data_service 用**快照里的账本**跑一次 `ClassifyPersisted`,已见者剔除。需要快照里存有 PlayerAllData blob——今天没有(T3),所以现在做不了,先记着。
- **U4 `data_service.proto` 首次 import 仓内 proto**:今天它只 import `google/protobuf/empty.proto`(`:4`)。Go 侧 guild 已经 `import dspb "proto/data_service"`,与 `proto/common/component` 同在 `proto` 模块,**推断**生成链能处理;C++ 侧的 `cpp/generated/data_service`(`tools/proto_generator/protogen/etc/proto_gen.yaml:364-376`)是否需要额外 include 路径**未能确认**(未读生成器代码)。若生成失败,退路 = `bytes ledger = 2`(序列化后的 `PlayerAssetOpLedgerComp`)+ 适配器里 `proto.Unmarshal`;这改变了 04:1558 给定的字段类型,须用户点头。
- **U5 `ResolveManually` 是否同写 `next_attempt_ms`**:04:1547 的原文没写。属 B5b 范围,本文只提要求(§7.4.1 末)并在 G5 钉住。
- **U5b `Store.Finalize` 同写 `next_attempt_ms`**(§7.4.1):05:690 写了,`go/shared/assetop/reconcile.go:136-138` 的契约注释没写。属 B5b / shared/assetop 持有会话的范围;B5d 只提硬要求并由 G5b 钉住。**漏了是 fail-open**,比 U5 严重,B5b 交付前请点名核对。
- **U9 保留期不可证明能否被放行覆盖**(R2b、§7.5.4):本文定为**能**(同一个开关、同样要 token + reason + operator),依据是 90:191 的"**默认**拒绝"与"不给出口就逼出绕闸"。这比前稿放宽了一档,请确认。若你要维持"不可覆盖":最小改法 = §7.6.4 表里"有不可证明玩家"一行改回 `CheckFailed`、放行无效,钳位重查与 `guild_unprovable_player_count` 保留(让运维至少看得见是谁挡的);**不改的风险** = 开服满 30 天后 `RollbackZone / RollbackAll` 基本不可用,只剩逐玩家回档。
- **U10 data_service 如何发现 guild**(§7.5.5):推荐 (c) 静态 `Endpoints`(dev)/ Service DNS(k8s),零额外文件;(a) 给 guild 加 go-zero `Etcd.Key` 要推翻 D-13;(b) 自建 NodeInfo resolver 不值。三条的失败方向都是拒绝。
- **U6 回档实际不作用于 scene 读的 blob**(§结论第二条、T3):不在 B5d,但它决定了"复制资产"何时从理论变成现实,也决定放行日志何时可以直接当补偿单用。需要有人认领一个独立批次(回档写 PlayerAllData + B4c 属主围栏 + `RollbackFence` 生产实现),建议在 PROGRESS 里挂账。
- **U7 data_service 的 `/metrics` 在 k8s 未开**、guild 无 k8s manifest(D-14):B5d 的指标告警在 k8s 环境实际不可用,以日志关键字告警过渡。
- **U8 回档前安全快照的清理**:被本闸拒绝的回档不产生安全快照(检查在它之前),无新增垃圾;放行后写后复查失败的那次,安全快照保留,属预期。

## 7.12 与既有正文的偏差

| # | 既有写法 | 本文 | 原因 |
|---|---|---|---|
| 1 | 90:191 "按 `guild_id` 关联 `guild.zone_id`";04:1566 "zone 维度经 `guild.zone_id` 关联" | 实现为可选过滤(LEFT JOIN,保留已解散帮会),**data_service 恒传 0** | 合服改写 `guild.zone_id`、解散删 `guild` 行,两者都会让过滤静默丢行 = fail-open(§7.4.3) |
| 2 | 90:191 "走 idx_0" | 谓词对 idx_0 可用,但不预先加索引提示;验收 = `EXPLAIN` 的 `key ∈ {idx_0, idx_2}`;选了 PRIMARY 才加 `USE INDEX (idx_0, idx_2)` | 玩家少、窗口长时 idx_2 更优;结果集相同(§7.4.1) |
| 3 | 04:1567 放行清单"以 INFO 逐行写日志" | ERROR 级 | 与 assetopfix 审计同口径"确保进 Loki"(05:774);放行是需要人工跟进的事件,不是例行信息 |
| 4 | 04:1567 "since_ms = 快照时间" | `S_p×1000 − 300000`,每块取最小值,返回后逐玩家过滤 | 秒/毫秒、跨机时钟、批量回档每人快照不同(§7.2) |
| 5 | 04:1558 "读 zone Redis 的玩家 blob" | 明确为 `<PlayerAllData 全名>:{id}`,不是 data_service 自己那套 `player:{id}:<field>` | 后者里没有账本(§7.8.2) |
| 6 | 04:1566–1567 未提 | 新增沉降等待(事前)+ 写后复查(事后)+ 第三个错误码 | 给定接口查不到 PENDING;栅栏前已应用的行靠沉降转为事前拒绝,检查~写之间才拿到证据的靠复查(§7.7) |
| 7 | 04:1567 只说"请求显式带 `accept_guild_divergence`" | 另要求 `x-admin-token` + 非空 reason/operator | 回档 RPC 无鉴权,开关裸放等于没有闸(§7.6) |
| 8 | 91:26 "估 ≤18" | 拆成 8 + (7 + 8 登记) + 15 三个子批 | 逐项列清后放不进 18;新 proto 文件另带 8 个 C++ 生成工程登记;不为凑数合并测试文件或省掉 fail-closed 用例 |
| 9 | 05:953(W15)、06:651(C14)"接受 + 审计 / 不补发" | 作废,指向本文 | Y-13 |
| 10 | 90:191 "无法证明,默认拒绝回档";Y-13 "guild 调用失败也拒绝" | 保留期不可证明 = 可放行的拒绝(R2b);其余"问不到" = 不可放行 | 快照稀疏且永不清理,不给出口则批量回档在开服 30 天后不可用(§7.5.3-2b、U9) |
| 11 | 04:1567 未提余量 | `GuildClockSkewMarginMs` 下界 5000,禁止 0 | 已落码的 `ProcessOne` 把**开始时刻**写成终结时刻,可早于落盘 ≤ 2500ms(§7.2) |

---

## 附录:对抗评审处理记录

# S7 回档 fail-closed(B5d)— 对抗评审处理记录(2026-09-20)

结论:三个视角共 17 条,去重后 14 个独立问题(#1 与 #15 同题、#6 与 #17 同题、#5 与 #7 的 ctx 部分同题)。逐条开证据核实,**全部成立**:采纳 13 条、部分采纳 4 条(#1、#2、#4、#15——问题成立,修法与建议不同,理由见各行)、驳回 0 条。接口形状(`ListAppliedAssetOpsSince`、`GetPlayerAssetOpLedger`、`GuildAssetOpBrief`)未动;新增需用户拍板项 U5b、U9、U10,Q2 的推荐由 ① 改为 ②,Q4 由两批改为三批。

核对依据(本次回修实际打开过的):
- **代码**:`go/shared/assetop/reconcile.go:136-142`(Finalize 契约注释无 `next_attempt_ms`)、`:269-281`(默认参数)、`:484/:509/:569`(`nowMs` 在 `ProcessOne` 入口取值并一路传进 `Store.Finalize`)、`:549-553`(`settleContext` 的 `WithoutCancel` 先例)、`:642`(读账本门槛);`go/shared/assetop/decide.go:126-155`(退避与 ×[0.8,1.2) 抖动);`go/guild/etc/guild.yaml:3、7、9-11、88-89、133-136`;`go/data_service/etc/data_service.yaml`(全文无 `Timeout` / `MethodTimeouts`);`go-zero@v1.10.0/zrpc/config.go:26、31、46、52、93-94` 与 `internal/serverinterceptors/timeoutinterceptor.go:19-22`;`go/data_service/internal/metrics/metrics.go:187-190`、`data_service.go:267-273`;`go/shared/grpcstats/collector.go`(无 prometheus、无 code 维度);`go/guild/internal/session/session.go:104`;`proto/guild/guild.proto:6、396`;`go/build.ps1:3-12`、`go/build.bat:3`。
- **C++ 工程登记**:`cpp/generated/rpc/service_metadata/rpc_event_registry.cpp:37、70、251、1467`;`cpp/generated/proto/CMakeLists.txt:119-125`、`proto.vcxproj:121-127、239-245`、`proto.vcxproj.filters:306-309、644-647`;`grpc_client/CMakeLists.txt:46-48`、`grpc_client.vcxproj:31-33、51-53` 及 `.filters`;`rpc/rpc.vcxproj:46-48`、`.filters:117-123`;`tools/proto_generator/protogen` 下 `*.go` 对 `vcxproj|CMakeLists` 零命中;提交 `5247d273a`。
- **文档**:92-handoff `:184-189`(预置 0 的坑)、`:257、:269`(gen / build.ps1 写法)、`:359-381`(MSBuild 口径);91 `:26、:29`。
- **未核实**:go-zero 的 `rpc_server_requests_code_total` 指标名;`go_services.ps1` 的 `.z<N>` 后缀逻辑;proto-gen 对新文件的确切产物名(未跑生成)。三处在正文里均标了"未能确认"。

| # | 视角 | 级别 | 问题 | 处理 | 落点章节 |
|---|---|---|---|---|---|
| 1 | correctness | major | 保留期拒绝无放行通道 + 按块内最小 since 判定 → 开服 30 天后批量回档不可用;手册 §7.9.3-6 写"放行"却无此机制 | **部分采纳**。问题成立(快照稀疏、`DeleteOldSnapshots` 无调用者均已核)。修法是评审 ①② 的合并而非二选一:guild 在 `FailedPrecondition` 里带回 `cutoff_ms` → data_service 钳位重查该块一次 → 不可证明的玩家**单列**(逐玩家)→ 未放行 = 可放行的拒绝,合法放行 = 按块写 `unprovable accepted` 日志后继续。没采纳 ① 的"标记为 failed、其余照常回档":批量回档里静默跳过一部分玩家会得到半截 zone,且违反 R4 的"先查完再写";没采纳"探测调用":多一轮 RPC 不如直接带回下界。错误码不新增:`…GuildDivergence` 的含义收敛为"带放行能过" | 结论、R2/R2b、§7.3.2、§7.3.3、§7.4.2、§7.5.1、§7.5.3-2b、§7.5.4、§7.6.4、§7.9.3-1/6、D15–D17、U9、偏差 10 |
| 2 | correctness | major | 检查紧跟栅栏,栅栏前一两秒已应用的行此刻仍 PENDING → 被设计成"先复制后告警" | **部分采纳**。采纳沉降等待;**不采纳** ≥ 62.5s 与"单人另取短值":新鲜行走到读账本的上界按默认参数核算 ≈ 27s(三次退避 ≤ 8.4s + 四次轮询 + 四次处理),`MaxBackoff = 60s` 只对早已退避到顶的老行成立。(复核订正:"老行的证据早被读过"说过头了——长期 PENDING 的高 attempts 行在玩家上线后应用、且答复丢失时,同样要等 ≤72s,见 §7.7 处置一新增的那条;其失败方向是写后复查事后告警,不是漏报,所以常量仍取 30s。)取一个常量 30s,不分 scope(KISS)。如实写明它依赖 guild 的配表参数,不够时退化为写后复查,不是漏报 | §7.5.2、§7.7 处置一、§7.5.5 常量、D18(a)、偏差 6 |
| 3 | correctness | major | F 不是提交时刻而是 `ProcessOne` 开始时刻,可早于落盘 ≤ OpBudget;Validate 允许余量 = 0 → fail-open | **采纳**。`reconcile.go:484→509→569` 核实 | §7.1.1、§7.2、§7.5.5、C1、偏差 11 |
| 4 | correctness | minor | `ORDER BY 主键 + LIMIT` 可能被优化器改走 PRIMARY;"type≠ALL"与步 6 口径不一 | **部分采纳**。验收口径统一为 `key ∈ {idx_0, idx_2}`,EXPLAIN 要求灌数据、两种形状各跑一次;退路取 `USE INDEX (idx_0, idx_2)`。不采纳 `op_id + 0`(隐晦,后人会当笔误删掉)与"Go 里排序截断"(取回行数无界)。不预先加提示:无 EXPLAIN 证据 | §7.4.1、§7.10.3 步 7、偏差 2 |
| 5 | correctness | minor | 写后复查沿用入口 ctx,调用方断开 → 假阳性紧急告警且真分歧查不出 | **采纳** | §7.5.2、§7.5.3-3、§7.7 处置二、D18(b)(c) |
| 6 | correctness | minor | proto 注释称 guild.proto 没 import proto_option | **采纳**。`guild.proto:6` 核实 | §7.3.1 |
| 7 | ops-security | major | data_service 服务端 zrpc 默认 2000ms 超时,文档的 3s/120s/10s 全超;120s 是否罩写阶段没说清;复查无独立预算 | **采纳**,两条建议都要(不是二选一):`MethodTimeouts` 解决"检查阶段根本跑不完",脱钩 ctx 解决"调用方先断开";120s 限定只罩检查阶段;复查单列 `guildRecheckBudget`。Zone/All 的值标为起始值(无实测) | §7.5.3-3、§7.5.5、§7.10.1 2b#12、D18 |
| 8 | ops-security | major | "etcd key 抄 guild.yaml"无值可抄,闸的通过分支永远走不到 | **采纳**。推荐 (c) 静态 `Endpoints`,零额外文件;列为 U10 请用户拍板 | §7.5.5、U10、§7.10.3 步 10 |
| 9 | ops-security | major | 三条 `increase > 0` 告警因懒注册漏掉第一次 | **采纳**。data_service `PrimeRollbackGuildCheck`(27 条);guild 侧在 `MustNewServer` 之后预置 | §7.4.5、§7.9.1、§7.9.2、M1、步 11 |
| 10 | ops-security | minor | 告警第 4 行引用了不存在的 grpcstats 指标 | **采纳**。改日志关键字(`session.go:104`);go-zero 指标名标"未能确认" | §7.9.2 |
| 11 | ops-security | minor | 单次超时两个旋钮互相打架;Breaker 未点名 | **采纳**前一种:删 `GuildCheckCallTimeoutMs`,只用 `GuildInternalRpc.Timeout`(Validate [500, 3500]);yaml 关 Breaker 并写注释 | §7.5.3-3、§7.5.5、§7.6.4、C1 |
| 12 | ops-security | minor | Q2 选 ① 时,无凭据调用可读资产流水样本并长时间持栅栏 | **采纳**,并更进一步:两条代价写进 Q2;"持栅栏"在 ① 之下无解,故**推荐改为 ②**;① 之下样本行只对带 token 的调用返回 | §7.5.4、§7.6.3-Q2、§7.9.3-1 |
| 13 | implementability | **blocker** | 新 proto 文件要登记进手工维护的 C++ 工程,文档写"不改任何 C++",照做则全节点 LNK2019 | **采纳**。8 个登记文件单列(R1–R8,照 `trade_admin` 登记位);删两句错话;验证序列加 MSBuild 步;据此把回档闸拆成 2a / 2b | §7.10.1、§7.10.3 前置与步 1/2a/4、Q4、偏差 8 |
| 14 | implementability | major | 步 2 `build.bat` 不重生 `.pb.go`,且末尾 `pause` | **采纳** | §7.10.3 步 2a / 2b |
| 15 | implementability | major | 同 #1(保留期无出口) | **部分采纳**,同 #1。取评审 (A) 的主体(可覆盖 + 钳位重查),并吸收 (B) 的"让运维看见是谁挡的"(`guild_unprovable_player_count` + `unprovable rejected` 日志) | 同 #1 |
| 16 | implementability | minor | `Store.Finalize` 的契约注释没有 `next_attempt_ms`,B5b 照注释落码则漏行 = fail-open | **采纳**。`reconcile.go:136-138` 核实。B5d 不碰该文件(属别的会话),只提硬要求 + G5b + U5b | §7.4.1、G5b、U5b |
| 17 | implementability | minor | 同 #6 | **采纳**,同 #6 | §7.3.1 |

回修引入的复杂度,逐项交代(AGENTS §11.2):新增 1 个响应字段、1 个结构体(`GuildCheckResult`)、1 个 message 解析函数、2 个常量、3 条 `MethodTimeouts`、1 个 `Prime` 函数;删除 1 个配置项(`GuildCheckCallTimeoutMs`)。没有新增服务、依赖、表或错误码。其中唯一不是"补漏"而是"放宽"的是 R2b,已列 U9 请用户确认。
