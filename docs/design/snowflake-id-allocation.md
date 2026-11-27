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
```

毫秒水位 `watermark_ms/<slot>` 只有已删的 login-player 写过,2026-11-12 从分配器里删掉:不再读写,reclaim 也不再比较它的 revision(原来那条比较在 login 第一笔毫秒水位写失败、又碰上 key 过期时会误 fence)。etcd 里的旧键没有读者,清 etcd 时一并清掉。

| kind | 铸什么 | 实现 |
|---|---|---|
| `scene-manager` | scene_id | `go/shared/snowflakealloc` + `go/scene_manager/internal/svc/servicecontext.go` |

已退役的 kind,etcd 里这些前缀下的旧键没有读者,项目未上线,清 etcd 时一并清掉:

- `scene-item`:C++ scene 的 item guid / tx_id / snapshot_id 全部改走号段,C++ 槽位客户端已删(`bin/etc/base_deploy_config.yaml` 号段一节)。
- `match`:battle_id / challenge_id / team_id 改走号段(biz_tag = battle,[发号器设计](./player-data-tidb-scaling-plan.md) §6.4)。
- `login-player`、`guild`:player_id、guild_id 早就只由号段发,2026-11-12 删掉了还在跑的槽位申领、水位与失槽 fence。

规则(`go/shared/snowflakealloc/testdata/selection_vectors.json` 钉住选号;C++ 侧不再申领,不再对拍):

1. **选号**:候选 = 未被占 且 `now − watermark ≥ Q`(Q = 4h);取 watermark 最小者(从没用过的优先),并列取最小 slot。不再"最小空闲位"。
2. **水位**:持有者每秒写 `max(墙钟, 逻辑高水位) + 2s`,单调不回退;申领 Txn 只写 slots / affinity,第一笔水位在申领之后单独写(Txn 以 slots key 仍是自己的 uuid 为条件,成功才 Ack 闸,`advanceGuard`;已删的 C++ 客户端当年是同一 txn 落地);启动地板 `SetGuardTime(max(now, watermark))` 并等真实时钟越过。
3. **自 fence**:距上次水位写成功超过 F = 2h(单调钟或墙钟任一;墙钟按去掉单调读数的时刻相减,否则 VM 挂起后不触发)→ `Generate()` 拒发;水位再写成功即恢复(Go)/ 永久 fence 仅在 `slots/<slot>` 被别的 uuid 持有,或槽 key 过期期间水位被别人写过时(见第 4 条)。
4. **lease 只做活性**:lease 过期但槽没被别人挂走 → 重挂,不 fence、不退出。槽 key 已过期时,只有水位 key `watermark/<slot>` 的 ModRevision 仍是自己最后一次观测的值才重新 Create(与 `CreateRev==0` 同一笔 Txn);对不上说明失联期间有人用过这个槽,永久 fence,重启后走受隔离期保护的申领。细节见设计稿 §3.4。
5. **弱依赖**:scene_manager 在 etcd 不可达且本地缓存(`SnowflakeCacheDir`)未过 F 时可用缓存槽起。

路由用的 `NodeInfo.node_id`(`<Svc>.rpc/allocated/node_type/T/node_id/N` + `/zone/Z/...`)与发号槽位**无关**,只做 Kafka topic 名、gate 会话反查、`SessionDetails.gate_node_id`;gate session_id 与 scene buff/skill 的临时复合 id 仍用它。

## 永久身份:号段(Leaf-segment)

player_id、guild_id、物品 guid、流水号、battle_id 等永久 ID 从 `data_service.AllocateIdSegment(biz_tag, step)` 领 `[lo, hi)`。发号器的预留记录写在两套本地 etcd 与全局号源登记处(`id_segment` 表已删),发出的是打散过的计数器值:63 位以内、不单调、没有位布局,不能按 snowflake 解析,也不能靠大小判断先后。客户端双 buffer 预取:`go/shared/idsegment`(login、guild 等 Go 服务)、C++ `GuidSegmentClient`(scene)。**没有回退号源**,发号失败就是操作失败。设计见 [player-data-tidb-scaling-plan.md](./player-data-tidb-scaling-plan.md) §6,运维见 [发号器运维手册](../ops/id-issuer-runbook.md)。

## 改造前旧布局的兼容层(2026-11-26 已删)

**删了什么。** `go/shared/snowflakealloc` 原有一层只为「改造前旧二进制与新二进制滚动升级并存」写的过渡代码,由 `Options.LegacyPrefix` 打开(只有 scene_manager 传 `/scene_manager`,只在 cluster 0 生效),两个方向都留脚印:

- 读:申领时把旧 `<prefix>/snowflake_ids/<id>`、`<prefix>/snowflake_nodes/<host>=<id>` 算作已占,旧 `<prefix>/snowflake_guard/<slot>` 与新水位按槽取 max 参与隔离期(`mergeWatermarks`);读地板时同样与旧水位取 max。
- 写:申领 Txn 多一条 `CreateRevision(<prefix>/snowflake_ids/<slot>)==0` 比较,并把这把键挂到自己的 lease 上;复用与 reclaim 把它改挂到新 lease;每拍水位写在同一笔 Txn 里镜像到旧 `snowflake_guard/<slot>`。目的是让只认旧布局的旧二进制看得见新二进制占着的槽、接手时有地板。

这些读写、`Options.LegacyPrefix`、`legacyMirror` 与五个旧 key 函数、`mergeWatermarks`、选号向量里带 `legacy_watermarks` 的 3 例(剩 12 例)、两条只测这层的真 etcd 用例与模拟旧二进制申领的辅助函数、scene_manager 的实参,2026-11-26 一起删掉(负责人定)。新布局的条件一条没动:申领仍是 `If CreateRev(slots/<slot>)==0 Then Put slots + affinity`,复用仍是三元组同 lease 的六条比较,reclaim 与水位写的条件不变。login、guild 的 `/login`、`/guild` 旧前缀早已随申领代码一起不再读;C++ 那一条随 C++ 槽位客户端删除。

- **问题**:这层只在「改造前的二进制与新二进制连着同一个 etcd」时才有用。项目未上线,改造前的二进制从没部署过,以后也不会有。留着有实际成本:cluster 0 每次申领多三次前缀扫描,每拍水位写多一个 Put,读地板多一次 etcd 读;申领、复用、reclaim、水位写、读地板各多一条分支,读代码的人要先弄清它何时生效;用例要模拟一个不存在的对端。代码注释自己写着「发布一版之后删」。
- **方案**:整批删,不留开关。现在申领、复用、reclaim、水位写与读地板只认 `/snowflake/<kind>/c<cluster>/` 下的四类键。旧前缀下的残留键(本地开发 etcd 里可能有)没有读者,清 etcd 时一并清掉。
- **为什么现在删**:兼容要不要保留取决于上线状态(负责人 2026-10-18 确认未上线)。上线前删只是删代码;上线后再删,就得按真实存量安排灰度窗口。
- **被否决的做法**:
  1. 留着等「发布一版」:没有可发布的旧版本,这个条件永远不会自然满足,只会一直背着上面的成本。
  2. 只删 scene_manager 的实参、保留 `Options.LegacyPrefix`(只关开关):过渡代码仍留在五条主路径里,照样要维护和测试;谁传一个非空前缀就会重新打开对旧布局的读写。没有使用者的开关就是死代码。
  3. 改成启动时检查旧前缀下有没有键、有就拒启:为一个不存在的部署再加一道启动门禁,同样是没人用的代码。
- **代价与边界**:从此不能与改造前(2026-09-08 之前)的二进制混跑。旧二进制只认旧布局,看不见新二进制占着的槽,混跑会把同一个 worker id 发两次;想回到那种二进制,只能先停掉现版本、按全新环境起(它不读新水位,前任的地板与隔离期对它都不存在)。与 09-08 之后、仍带这层代码的版本混跑或回退不受影响:双方都以新布局为准,那些版本多写的旧前缀键挂在它们自己的 lease 上(水位镜像键不挂 lease,但只有那些版本读,值也不高于同笔写入的新水位),新版本不读。以后若真要换 etcd 键布局,得按当时的线上状态重新设计迁移。

## Cross-ID-kind isolation

现在只剩 `scene-manager` 一种 kind,规则不变:一种 ID 只能由一种 kind 铸;不同 kind 的槽池相互独立,铸出的值可能相同,不能混进同一个键空间。发号器发的永久 ID 与 scene_id 是两种号源,同样不能混用。跨 kind 全局唯一的需求今天不存在。


## Scene 领段预取的单次协程（2026-11-05）

本节只涉及已迁移到 DataService 号段的永久 ID 客户端，不改变本文其余 ID 位布局规则。`IdSegmentTransport` 将一次 AllocateIdSegment 请求放入原 Scene EventLoop 的任务作用域，回复复制为自有值。`GuidSegmentClient` 的双缓冲、预取阈值、退避与超时继续负责分配策略；请求认领表保留跨种类单飞和原 10 秒迟到窗口，旧序号不能被新种类的请求认领。

普通停机先 `CloseGuidSegmentTransport` 禁止新增，再关闭原客户端预取；已发请求仍可在退出存盘期间交付。最终拆除调用 `StopGuidSegmentTransport`，取消本地续跑并清空认领表，晚回复不再交付。`SceneRuntimeContext` 析构覆盖启动异常时的停止，保证任务所有者先于局部 EventLoop 释放。没有新增线程、阻塞等待或分配重试层。Java 发号契约和可观察行为不变。
