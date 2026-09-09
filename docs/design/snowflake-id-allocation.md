# SnowFlake ID Allocation

> **2026-09-08 重写。** 旧版描述的 `mustAllocNodeID()` / `snowflake_counter` / "按 (zone, node_type) 分配" 早已不存在;
> 设计与决策见 [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md),本文只做现状速查。

## Layout

C++ `cpp/libs/engine/core/utils/id/snow_flake.h` 与 Go `go/shared/snowflake/snowflake.go` 逐位一致:

- `[time:32][cluster:5][slot:12][step:15]`,epoch 1773446400(2026-03-14 UTC),秒级。
- worker(17 位)= `(cluster << 12) | slot`;cluster 是部署级常量(`ClusterId` / 环境变量 `CLUSTER_ID`,默认 0),slot ∈ [0, 4095]。
- login 的 player_id 用 bwmarrin 毫秒布局 `[time:41ms][cluster:3][slot:10][step:9]`,epoch 1721473263000;布局不能换(换了作废存量),cluster/slot 切分同上。
- 老 ID 的 17 位 worker 等于新布局 cluster=0 的 slot,时间字段只增不减,所以**不作废存量**。

## 槽位(worker)号源:etcd,两种语言同一协议

```
/snowflake/<kind>/c<cluster>/slots/<slot>       = holder uuid   挂 lease(只表活性)
/snowflake/<kind>/c<cluster>/watermark/<slot>   = epoch 秒       无 lease(持久;既是 guard 地板又是隔离期墓碑)
/snowflake/<kind>/c<cluster>/affinity/<host>    = slot           挂 lease(仅 Go;同主机优雅重启复用)
/snowflake/<kind>/c<cluster>/released/<host>    = "released"     挂 lease(仅 Go)
/snowflake/<kind>/c<cluster>/watermark_ms/<slot>= unix 毫秒      无 lease(仅 login-player)
```

| kind | 铸什么 | 实现 |
|---|---|---|
| `scene-item` | C++ scene 的 item guid(过渡期)/ tx_id / snapshot_id | `cpp/libs/engine/core/node/system/snowflake/snowflake_slot_client.*` |
| `login-player` | player_id(仅作号段的回退与解析旧号) | `go/shared/snowflakealloc` + `go/login/internal/svc/player_id_gen.go` |
| `guild` | guild_id(仅作号段的回退) | `go/shared/snowflakealloc` |
| `scene-manager` | scene_id | 同上 |
| `match` | battle_id / challenge_id | 同上 |

规则(两端共用 `go/shared/snowflakealloc/testdata/selection_vectors.json` 做一致性测试):

1. **选号**:候选 = 未被占 且 `now − watermark ≥ Q`(Q = 4h);取 watermark 最小者(从没用过的优先),并列取最小 slot。不再"最小空闲位"。
2. **水位**:持有者每秒写 `max(墙钟, 逻辑高水位) + 2s`,单调不回退;申领时第一笔与 slots key 同一 txn 落地;启动地板 `SetGuardTime(max(now, watermark))` 并等真实时钟越过。
3. **自 fence**:距上次水位写成功超过 F = 2h(单调钟或墙钟任一)→ `Generate()` 拒发;水位再写成功即恢复(Go)/ 永久 fence 仅在 `slots/<slot>` 被别的 uuid 持有时。
4. **lease 只做活性**:lease 过期但槽没被别人挂走 → 重挂,不 fence、不退出。
5. **弱依赖**:Go 服务 etcd 不可达且本地缓存(`SnowflakeCacheDir`)未过 F 时可用缓存槽起;C++ 只用缓存抬地板与关联日志(发现仍需 etcd)。

路由用的 `NodeInfo.node_id`(`<Svc>.rpc/allocated/node_type/T/node_id/N` + `/zone/Z/...`)与发号槽位**无关**,只做 Kafka topic 名、gate 会话反查、`SessionDetails.gate_node_id`;gate session_id 与 scene buff/skill 的临时复合 id 仍用它。

## 永久身份:号段(Leaf-segment)

player_id / guild_id / item guid 从 `data_service.AllocateIdSegment(biz_tag, step)` 领 `[lo, hi)`,表 `id_segment`(全局库,`biz_tag VARCHAR(64)` 预建,其余列 proto 驱动),号从 1 起、上限 2^55,与存量 snowflake 号(≥ 6.7e16)值域不相交,新旧共存不迁数据。客户端双 buffer、剩 10% 预取:`go/shared/idsegment`(login/guild)、C++ `ItemGuidSegmentClient`(scene)。回退 snowflake 默认关(`FallbackToSnowflake`),开了也安全。

## 过渡期兼容(一个版本后删)

- Go cluster 0 读旧 `<oldprefix>/snowflake_guard/<slot>`、`/login/guard_ms/<slot>` 取 max,并把旧 `snowflake_ids`/`snowflake_nodes` 算作占用。
- C++ 把 `SceneNodeService.rpc/allocated/node_type/<T>/node_id/<N>` 被持有视为 slot N 占用,并读旧 Redis `snowflake_guard:<type>:<N>` 取 max。

## Cross-ID-kind isolation

不同 kind 的槽池相互独立,所以 guild_id / scene_id / battle_id 可能数值相同;一种 ID 只能由一种 kind 铸,不能混进同一个键空间。跨 kind 全局唯一的需求今天不存在。
