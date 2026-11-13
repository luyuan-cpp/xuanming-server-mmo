# SnowFlake ID Allocation

> **2026-09-08 重写。** 旧版描述的 `mustAllocNodeID()` / `snowflake_counter` / "按 (zone, node_type) 分配" 早已不存在;
> 设计与决策见 [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md),本文只做现状速查。

## Layout

C++ `cpp/libs/engine/core/utils/id/snow_flake.h` 与 Go `go/shared/snowflake/snowflake.go` 逐位一致:

- `[time:32][cluster:5][slot:12][step:15]`,epoch 1773446400(2026-03-14 UTC),秒级。
- worker(17 位)= `(cluster << 12) | slot`;cluster 是部署级常量(`ClusterId` / 环境变量 `CLUSTER_ID`,默认 0),slot ∈ [0, 4095]。
- login 原来的 bwmarrin 毫秒布局 player_id 2026-11-12 已随申领代码删除,player_id 只由发号器发(见下文「永久身份」),全系统不再有第三种布局。
- 老 ID 的 17 位 worker 等于新布局 cluster=0 的 slot,时间字段只增不减,所以**不作废存量**。

## 槽位(worker)号源:etcd,现在只有 scene_manager 申领

```
/snowflake/<kind>/c<cluster>/slots/<slot>       = holder uuid   挂 lease(只表活性)
/snowflake/<kind>/c<cluster>/watermark/<slot>   = epoch 秒       无 lease(持久;既是 guard 地板又是隔离期墓碑)
/snowflake/<kind>/c<cluster>/affinity/<host>    = slot           挂 lease(同主机优雅重启复用)
/snowflake/<kind>/c<cluster>/released/<host>    = "released"     挂 lease
/snowflake/<kind>/c<cluster>/watermark_ms/<slot>= unix 毫秒      无 lease(只有已删的 login-player 写过,现在没有读写者)
```

| kind | 铸什么 | 实现 |
|---|---|---|
| `scene-manager` | scene_id | `go/shared/snowflakealloc` + `go/scene_manager/internal/svc/servicecontext.go` |

已退役的 kind,etcd 里这些前缀下的旧键没有读者,项目未上线,清 etcd 时一并清掉:

- `scene-item`:C++ scene 的 item guid / tx_id / snapshot_id 全部改走号段,C++ 槽位客户端已删(`bin/etc/base_deploy_config.yaml` 号段一节)。
- `match`:battle_id / challenge_id / team_id 改走号段(biz_tag = battle,[发号器设计](./player-data-tidb-scaling-plan.md) §6.4)。
- `login-player`、`guild`:player_id、guild_id 早就只由号段发,2026-11-12 删掉了还在跑的槽位申领、水位与失槽 fence。

规则(`go/shared/snowflakealloc/testdata/selection_vectors.json` 钉住选号;C++ 侧不再申领,不再对拍):

1. **选号**:候选 = 未被占 且 `now − watermark ≥ Q`(Q = 4h);取 watermark 最小者(从没用过的优先),并列取最小 slot。不再"最小空闲位"。
2. **水位**:持有者每秒写 `max(墙钟, 逻辑高水位) + 2s`,单调不回退;申领时第一笔与 slots key 同一 txn 落地;启动地板 `SetGuardTime(max(now, watermark))` 并等真实时钟越过。
3. **自 fence**:距上次水位写成功超过 F = 2h(单调钟或墙钟任一;墙钟按去掉单调读数的时刻相减,否则 VM 挂起后不触发)→ `Generate()` 拒发;水位再写成功即恢复(Go)/ 永久 fence 仅在 `slots/<slot>` 被别的 uuid 持有,或槽 key 过期期间水位被别人写过时(见第 4 条)。
4. **lease 只做活性**:lease 过期但槽没被别人挂走 → 重挂,不 fence、不退出。槽 key 已过期时,只有两把水位 key 的 ModRevision 仍是自己最后一次观测的值才重新 Create(与 `CreateRev==0` 同一笔 Txn);对不上说明失联期间有人用过这个槽,永久 fence,重启后走受隔离期保护的申领。细节见设计稿 §3.4。
5. **弱依赖**:scene_manager 在 etcd 不可达且本地缓存(`SnowflakeCacheDir`)未过 F 时可用缓存槽起。

路由用的 `NodeInfo.node_id`(`<Svc>.rpc/allocated/node_type/T/node_id/N` + `/zone/Z/...`)与发号槽位**无关**,只做 Kafka topic 名、gate 会话反查、`SessionDetails.gate_node_id`;gate session_id 与 scene buff/skill 的临时复合 id 仍用它。

## 永久身份:号段(Leaf-segment)

player_id、guild_id、物品 guid、流水号、battle_id 等永久 ID 从 `data_service.AllocateIdSegment(biz_tag, step)` 领 `[lo, hi)`。发号器的预留记录写在两套本地 etcd 与全局号源登记处(`id_segment` 表已删),发出的是打散过的计数器值:63 位以内、不单调、没有位布局,不能按 snowflake 解析,也不能靠大小判断先后。客户端双 buffer 预取:`go/shared/idsegment`(login、guild 等 Go 服务)、C++ `GuidSegmentClient`(scene)。**没有回退号源**,发号失败就是操作失败。设计见 [player-data-tidb-scaling-plan.md](./player-data-tidb-scaling-plan.md) §6,运维见 [发号器运维手册](../ops/id-issuer-runbook.md)。

## 过渡期兼容(一个版本后删)

- scene_manager(`LegacyPrefix` = `/scene_manager`)在 cluster 0 读旧 `<oldprefix>/snowflake_guard/<slot>` 取 max,并把旧 `snowflake_ids`/`snowflake_nodes` 算作占用。login、guild 的 `/login`、`/guild` 旧前缀随申领代码一起不再读。
- C++ 那一条随 C++ 槽位客户端删除。

## Cross-ID-kind isolation

现在只剩 `scene-manager` 一种 kind,规则不变:一种 ID 只能由一种 kind 铸;不同 kind 的槽池相互独立,铸出的值可能相同,不能混进同一个键空间。发号器发的永久 ID 与 scene_id 是两种号源,同样不能混用。跨 kind 全局唯一的需求今天不存在。


## Scene 领段预取的单次协程（2026-11-05）

本节只涉及已迁移到 DataService 号段的永久 ID 客户端，不改变本文其余 ID 位布局规则。`IdSegmentTransport` 将一次 AllocateIdSegment 请求放入原 Scene EventLoop 的任务作用域，回复复制为自有值。`GuidSegmentClient` 的双缓冲、预取阈值、退避与超时继续负责分配策略；请求认领表保留跨种类单飞和原 10 秒迟到窗口，旧序号不能被新种类的请求认领。

普通停机先 `CloseGuidSegmentTransport` 禁止新增，再关闭原客户端预取；已发请求仍可在退出存盘期间交付。最终拆除调用 `StopGuidSegmentTransport`，取消本地续跑并清空认领表，晚回复不再交付。`SceneRuntimeContext` 析构覆盖启动异常时的停止，保证任务所有者先于局部 EventLoop 释放。没有新增线程、阻塞等待或分配重试层。Java 发号契约和可观察行为不变。
