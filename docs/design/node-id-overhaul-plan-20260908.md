# Node ID / 永久 ID 改造方案(2026-09-08 草案,未落码)

> 状态:设计稿,等拍板后按 Phase 0 → 5 落码。本文只描述"改成什么样、为什么、怎么验",不含代码。
> 前置结论见对话记录:**etcd 这一步没选错**,坏在拿到号之后的复用策略、依赖强度、耦合与位预算。

---

## 0. 一页总览

| # | 改什么 | 解决什么 | 成本 | 阶段 |
|---|---|---|---|---|
| 0a | k8s scene-manager ConfigMap 补 `ZoneId` | 所有 zone 的 SceneManager 注册成 zone 1 | 1 行 | Phase 0 |
| 0b | Go 分配器复用分支删 `released` 标记 | 同主机第三个进程能抢活着的 worker id | 1 行 + 1 测试 | Phase 0 |
| 0c | tx_id / snapshot_id Kafka 消费者落地 | 回滚审计链路实际没数据 | 中(5 步) | Phase 0 |
| A | 选号从"最小空闲"改"最久未用 + 隔离期";水位当墓碑;自 fence 改按水位年龄 | 冻结/同秒重启/跨机时钟偏差三类撞号窗口 | Go 中 / C++ 中 | Phase 1(Go)、2(C++) |
| B | C++ 发号 worker 与路由 node_id 解耦,走与 Go 同一套 etcd 协议 | 路由层 TTL/宽限期一动就动 ID 唯一性 | C++ 中 | Phase 2 |
| C | 弱依赖:本地缓存 + etcd 抖动不自杀 | etcd 单点抖一下全员自杀 | Go 小 / C++ 小 | Phase 1、2 |
| D | 17 位 worker 切成 [cluster5][node12] | 第二个 etcd 命名空间出现即撞号;全球同服前置 | 两端各改常量 + 解码 + 一致性向量 | Phase 3 |
| E | 永久身份走号段(player_id / guild_id;item guid 最后) | 永久 ID 不该依赖 lease/时钟/worker | Go 中 / C++ 大 | Phase 4、5 |

不做的:继续调大 TTL 当修复;为拿序号改 StatefulSet(scene 走 Agones 拿不到序号,强删 Pod 同样破坏 at-most-one);搞一个"全球唯一 etcd"。

---

## 1. 先把三个概念讲透

### 1.1 为什么"最小空闲位回收"是最坏的复用策略

时间线(今天的代码就是这样):

```
T0   scene A 拿到 worker=5,正常发号
T1   A 被冻结(GC 停顿 / cgroup freeze / 宿主机 live-migration / 断点)
T1+TTL  etcd 判 A 的 lease 过期,slot 5 的 key 消失
T2   scene B 启动,扫"最小空闲" → 5(因为 1~4 被别人占着,5 刚空出来)
T3   A 解冻,继续发号;它的 last_time_ 和 B 的 now 是同一秒 → 同 (秒, worker=5, step) 逐位相同
```

关键:**最小空闲位算法挑出来的槽,恰好就是"前任最可能还活着"的那个槽**。因为活着的节点都占着低位,唯一空出来的低位一定是刚死(或刚冻)的那一个。Chubby 论文对锁的处理叫 lock-delay:持有者失联后,这把锁一分钟内不许别人拿。美团 Leaf 的回收 PR 也是"某个 workId 超过最大时间没更新时间戳才放回池子"。我们缺的就是这段"等一等"。

改法("最久未用 + 隔离期"):

```
每个槽 i 有一个持久水位 wm[i](持有者每秒写一次,不挂 lease,写的是"我发到哪一秒了")
申领时:
  候选 = 没被占 且 (now - wm[i]) ≥ Q          # Q = 隔离期,默认 4 小时
  选   = 候选里 wm 最小的(最久没人用过的);从没用过的 wm=0 最优先
```

同一条时间线再走一遍:B 启动时 slot 5 的水位是 T1(A 最后一次写),now − T1 < 4h → 跳过 5,去拿一个几小时没人碰过的槽。A 解冻后发的号没人和它撞;A 自己在 2 小时(见 1.2)内必须自我 fence。13 万个槽对几十个节点,隔离几小时不花钱。

### 1.2 什么叫"lease 只做活性,不拿 lease 证明唯一性"

今天的 lease 背着两件事:
1. 告诉别人"我还活着"(服务发现) — 这是它该干的。
2. 证明"这个 worker id 是我的" — 这条**不成立**:etcd leader 切换会把所有 lease 续满 TTL;冻结的进程感知不到自己过期;clientv3 感知恒晚服务端一拍。Jepsen 对 etcd 锁的结论就是"多个客户端可能同时持有"。

改造后唯一性由三样东西保证,**没有一样依赖 lease**:

| 机制 | 谁做 | 保证什么 |
|---|---|---|
| 持久水位 `wm[i]` | 持有者每秒写 `max(墙钟, 发号器逻辑高水位) + 2s` | 继任者知道前任最远发到哪一秒 |
| 隔离期 Q | 申领者跳过 `now − wm[i] < Q` 的槽 | 前任就算还活着,Q 内没人和它共用槽 |
| 自 fence 期限 F = Q/2 | 持有者:距上次水位写成功超过 F 就停发(判定放进 `Generate()` 本身) | 前任在别人可能申领之前就已停发 |

三者合起来的不等式:`前任最晚停发时刻(T_ack + F) < 继任者最早申领时刻(wm + Q)`,中间 2 小时余量吃掉时钟偏差、drain、冻结感知延迟。**TTL 从此是活性参数**,60 还是 180 只影响服务发现多快摘掉死节点,不再影响 ID 正确性。

### 1.3 什么叫"把协调器当弱依赖"

今天:etcd 不通 → C++ 起不来(没 node_id 不启 RPC)、Go 全部 panic;运行期失租 → 全员 fence + 自杀;生产 etcd 还是单副本 emptyDir Deployment。

改造后:
- **发号永远不等 etcd**:水位是异步写,失败只记账。
- **etcd 抖动不自杀**:lease 过期但槽没被别人挂走 → 重新挂 lease,继续;只有"槽被别人挂到了另一个 lease 上"(运维手动清理、或水位机制被绕过)才立即 fence。
- **启动时 etcd 不通可以用本地缓存起**:本地文件存 `(kind, id, incarnation, lastWatermark, lastAckWall)`。若 `now − lastAckWall < F`,这个槽在协议上仍是我的(别人 Q 内不会申领,且 etcd 不通时别人也拿不到新号),直接用缓存 id 起,发号器地板设成 `max(now, lastWatermark)`,后台重试注册。这就是 Leaf 的 workerID 本地文件。
- **诚实的边界**:C++ scene 没有 etcd 就没有服务发现,玩家进不来。弱依赖的价值主要在 Go 服务(login 建号、guild、match 不因 etcd 抖动停摆)和"不自杀"。号段模式(第 6 节)才真正把永久 ID 的发号从协调器上摘下来。
- etcd 自身:改 3 副本 StatefulSet + PVC(独立小任务,不在本文范围内展开,但 Phase 1 前必须做)。

---

## 2. Phase 0:三个独立小修复

### 2.0a scene-manager ConfigMap 缺 `ZoneId`

- 现象:`tools/scripts/k8s_deploy.ps1` 的 scene-manager ConfigMap 模板(约 1350–1373 行)没有 `ZoneId`,而 `go/scene_manager/internal/config/config.go` 的 `ZoneId` 默认 1。login / player-locator / db 的模板都有 `ZoneId: ${CurrentZoneId}`。
- 改法:模板加 `ZoneId: ${CurrentZoneId}` 与 `LeaseTTL`;顺手删掉 `NodeID: "node-1"`(字段已 deprecated,零读取)。
- 验收:`k8s-zone-up -ZoneId 102` 后 `etcdctl get --prefix SceneManagerNodeService.rpc/zone/102/` 有记录,`/zone/1/` 没有多出来的。

### 2.0b Go 分配器复用分支不清 `released` 标记

- 现象:`go/shared/snowflakealloc/allocator.go` 复用 Txn(约 201–207 行)只 Put nodeKey + idKey,不删 `<prefix>/snowflake_nodes/<host>/released`。序列:A 优雅退出写标记(挂在 A 的 lease 上,存活 ≤ TTL)→ A' 同主机重启复用成功但标记还在 → A'' 第二个进程看到标记,跳过 leaseAlive 检查,把活着的 A' 的 id 抢走 → A' 收到 ownership lost,fence + exit。guild / login 用裸 hostname 亲和,本地多 zone 或裸金属会踩。
- 改法:复用 Txn 的 Then 里加 `OpDelete(releasedKey)`;`Close()` 顺序改为 **先 Fence 发号器,再写 released**(否则标记先落、发号器还在发)。
- 测试:`allocator_integration_test.go` 加 "A Close → A' reuse → A'' within TTL must NOT get A's id"(修复前应失败)。

### 2.0c tx_id / snapshot_id 消费者落地(三个里最大的一个)

调查结论(全部核实过 file:line):
- C++ 两个生产者是好的:`transaction_log_topic` / `player_snapshot_topic`,全局单 topic,无 zone 字段,消息是裸 proto,key = player_id,tx_id / snapshot_id 为 0 时 fail-closed。
- **全仓没有任何消费者**,两个 topic 也没进 `EnsureTopics`,于是按 broker 默认自动建(1 分区,dev 60s / prod-like 300s 保留期)。今天每一条 tx / snapshot 都在几分钟内被丢掉。
- 落地表在 `go/data_service`(全局、单副本、写 `testdb`):`transaction_log` 有表(手写 DDL,`tx_id` 已是主键)但**没有任何 INSERT**;`player_snapshot` 用 AUTO_INCREMENT `id`,**没有列**放 C++ 的 snapshot_id,所有读路径(`resolveSnapshot` / `GetPlayerSnapshotDiff` / `rollbackSinglePlayer` / `rollback_audit_log.snapshot_id_used`)都按 AUTO_INCREMENT id 走。
- **格式不兼容**:data_service 今天写读的 `data` 列是 JSON `{fields: map[string][]byte}`(Redis 字段图),C++ 发的是 `PlayerSnapshotEntry` 里两个 proto blob。直接落库会让现有 GM 回滚路径 `json.Unmarshal` 失败。
- `snapshot_type` 两套枚举(data_service `SnapshotType` vs C++ `SnapshotTrigger`)同列不同值域。
- 手写 DDL 与 proto 已经漂移:`rollback_audit_log` DDL 多了 `orphans_cleaned`;`transaction_log` DDL 列名 `timestamp_sec` 而 proto 是 `timestamp`;`player_snapshot` 同时存在于 go/db 的 `zone_N_db`(proto2mysql 建)和 data_service 的 `testdb`(手写 DDL)。

改法(按顺序,每步可独立验收):
1. **proto 补 `zone_id`**:`proto/common/rollback/transaction_log.proto` 与 `player_snapshot.proto` 各加 `uint32 zone_id`,C++ 生产者填 `GetZoneId()`。捕获时的 zone 才是 `RollbackZone` 想要的语义,消费者不必回查 Router。
2. **注册 topic**:data_service 启动时 `kafkautil.EnsureTopics`,`transaction_log_topic` 6 分区、`player_snapshot_topic` 3 分区、保留期 30 天。分区数一旦定下不可变(仓库已有 TopicGeneration 契约),现在定够。
3. **表结构走 proto,不再手写 DDL**:`mysql_database_table.proto` 的 `player_snapshot` 加 `snapshot_guid`(可空 UNIQUE)与 `source`(0 = GM/data_service,1 = C++ scene);`rollback_audit_log` 补 `orphans_cleaned`;`rollback_database_table.proto` 的 `transaction_log` 统一列名、补 TiDB 方言选项、`extra` 放宽到 TEXT。把 go/db 的表注册/迁移助手抽到 shared 给 data_service 复用。**明确决策:transaction_log / player_snapshot 只在全局库(data_service)一处**,从 go/db 的表清单里摘掉 `player_snapshot` / `rollback_audit_log`。
4. **transaction_log 消费者**(`go/data_service/internal/kafka/`,照 `go/match/internal/kafka/result_consumer.go` 的形态:kafka-go、FirstOffset、同步 commit):`INSERT IGNORE`(tx_id 主键天然去重),攒批 N 条 / T ms 一次多行插入,DB 失败有界重试后**停下不 commit**(宁可积压不许丢)。`QueryTransactionLog` / `BatchRecallItems` 从此第一次有真数据。
5. **snapshot 消费者最后**,且以 `source=1` 落库,同时 `resolveSnapshot` / `GetLatestSnapshotBefore` / `GetSnapshotPlayerIDsByZone` 先过滤 `source=0`,直到回滚路径学会读 C++ proto blob(把两个 blob 展开成与 `SavePlayerData` 相同的 Redis 字段图)。不这么做会把今天能用的 GM 回滚弄坏。
   - 列契约:`source=1` 行的 `snapshot_type` 存 C++ 的 `SnapshotTrigger` 原值,`data` 存收到的 `PlayerSnapshotEntry` 序列化字节;`source=0` 行保持 data_service 自己的 `SnapshotType` 与 JSON 字段图。统一两套枚举是后续 proto 改动。
   - `snapshot_guid` 是普通 INDEX 不是 UNIQUE(proto2mysql 做不出可空唯一列,GM 行的 0 会互撞),去重靠单实例消费者的 check-then-insert。

不做:让 go/db 的 `KeyOrderedKafkaConsumer` 消费(它绑死 `DBTask` 信封、按 zone 写库,形状不对);把 snapshot_id 改成主键(`rollback_audit_log.snapshot_id_used` 引用的是 AUTO_INCREMENT id,改了就篡改历史审计)。

---

## 3. 改造 A:选号策略 + 水位墓碑 + 自 fence(Go / C++ 同一协议)

### 3.1 etcd key 协议(两种语言共用)

```
/snowflake/<kind>/c<cluster>/slots/<id>        = <holder uuid>     挂 lease(只表活性)
/snowflake/<kind>/c<cluster>/affinity/<host>   = <id>              挂 lease(Go 保留;同主机优雅重启复用)
/snowflake/<kind>/c<cluster>/released/<host>   = "released"        挂 lease
/snowflake/<kind>/c<cluster>/watermark/<id>    = <epochSec>        **不挂 lease**,既是 guard 又是墓碑
```

- `kind`:`scene-item`(C++ scene 的 item/tx/snapshot)、`login-player`(bwmarrin 毫秒布局)、`guild`、`scene-manager`、`match`。
- `cluster`:第 5 节的集群号,默认 0。Phase 1/2 先按 c0 落,Phase 3 只是把常量接上。
- 迁移:Go 现有 `<prefix>/snowflake_guard/<id>` 已经是"无 lease 持久水位",语义完全一样。Phase 1 保留旧 prefix 名(`/guild` 等)只改算法;Phase 3 统一搬到新 prefix 时,读水位取新旧两处的 max。
- C++ 新增一个 `SnowflakeSlotClient`(与 `EtcdService` 的注册状态机分开),用同一套 key。

### 3.2 申领算法(替换"最小空闲")

```
used     = slots/* 下有 lease 的 id
wm[i]    = watermark/i(缺省 0)
now      = 本机墙钟秒(snowflake epoch 口径)
Q        = 4h(可配,下界见 3.4)
候选      = { i ∉ used, i ≤ maxID, now − wm[i] ≥ Q }     # wm=0 视为无穷久
选择      = 候选中 wm 最小者;并列取最小 i
CAS      = If CreateRev(slots/<i>)==0 Then Put slots/<i>=uuid (lease), Put affinity/<host>=i (lease)
失败      = 重扫重试
候选为空  = fail-closed 报错(13 万槽不可能;login 1024 槽也不可能)
```

同主机亲和复用(Go 保留):**只在 `released/<host>` 存在时允许**(前任已优雅退出并已 fence),复用 Txn 同时删 released。lease 死了但没 released 标记(崩溃)→ 不复用,走正常申领(受隔离期约束),重启拿到不同 id,日志靠 incarnation 关联(3.6)。

**滚动升级过渡规则(落码时发现的缺口,保留一个版本后删):** 灰度期间旧二进制仍在用"路由 node_id = worker"发号,而新命名空间里对应槽位的水位是 0,会被当成"从没用过"优先选中,与旧进程同 worker 位撞号。所以过渡期申领时:
- C++:把 `<SceneNodeService>.rpc/allocated/node_type/<T>/node_id/<N>` 被任何节点持有视为槽 N 已占用;能连上分 zone Redis 时把旧 `snowflake_guard:<node_type>:<N>` 读作额外水位取 max。
- Go:cluster 0 读旧 `<oldprefix>/snowflake_guard/<slot>`(login 另读 `/login/guard_ms/<slot>`)与新水位取 max,隔离期自然把旧进程刚用过的槽挡住。

### 3.3 水位契约(Go 已有,C++ 对齐)

- 值 = `max(墙钟秒, 发号器逻辑高水位) + lead(2s)`,每 1s 写一次,无 lease,单调不回退(Go 已有 guardMu 保序)。
- 申领成功后**先同步写一次水位再激活发号器**;启动地板 `SetGuardTime(max(now, wm))` 并**等待真实时钟越过**(不借位,这是跨重启屏障的一部分,两侧代码注释已经写明原因)。
- **C++ 改动点**:`EtcdManager::WriteSnowFlakeGuard` 从 Redis `SETEX 墙钟` 改为 etcd Put `watermark/<id>` = 逻辑高水位 + 2(`SnowFlake` 加 `high_water()` 只读访问器);`ActivateSnowFlakeAfterGuard` 改从 etcd 读。分 zone Redis 不再参与,跨 zone 回收读不到水位的缺口随之消失。
- 优雅退出:先 `Fence()`,再同步写最终水位,再写 released。

### 3.4 自 fence 改按"水位年龄"

- 持有者维护 `lastAck`(最近一次水位 Put 成功的**单调时钟**读数 + 同刻墙钟)。
- `Generate()` 内部先判:`mono_now − lastAck > F` 或 `wall_now − lastAckWall > F` → 返回 fenced(两个时钟任一触发,防 VM 挂起时单调钟停走)。判定放进发号器本身而不是只靠后台 ticker,堵住"解冻后请求线程先于 ticker 发出第一个号"的竞争。
- `F = Q/2 = 2h`。下界推导:`Q ≥ 2 × (TTL_max 180s + 最大时钟偏差 + drain 预算 15s + 冻结感知延迟)`,4h 对所有项都是两个数量级余量。
- lease 过期但 slots 没被别人挂走 → 重新 `Put slots/<i>`(If Value==uuid 或 CreateRev==0),**不 fence**。
- slots 被挂到别的 lease / 被删 → 立即 fence(现有 watch 逻辑保留)。
- 排空预算从 TTL/3 = 20s 变成 F − 已过时间,不再紧张。

### 3.5 本地缓存(弱依赖)

- 文件:`<数据目录>/snowflake-<kind>.json` = `{cluster, id, uuid, incarnation, lastWatermark, lastAckWall}`,每次水位写成功后原子更新(写临时文件 + rename)。
- 启动:etcd 可达 → 正常申领(缓存只做日志关联);etcd 不可达 且 `now − lastAckWall < F` → 用缓存 id 起,`SetGuardTime(max(now, lastWatermark + lead))`,后台每 2s 重试注册,注册成功前 lastAck 不推进(所以最多再撑 F);超过 F 仍未注册 → fence。
- etcd 不可达 且缓存过期 → 等 etcd(今天的行为)。

### 3.5.1 落码注记(2026-09-08,与上文设计的差异)

- **Go**(`go/shared/snowflakealloc`):键前缀 `/snowflake/<kind>/c<cluster>/{slots,affinity,released,watermark,watermark_ms}`;kind = `login-player`(3/10 位)、`guild`、`match`、`scene-manager`;水位 Put 是 `If Value(slots/<slot>)==uuid` 的 txn,写成功即所有权证明并推进 FenceClock;按水位年龄的自 fence 是**可恢复**的(`ErrWatermarkStale`,下一次水位写成功即恢复),永久 `Fence()` 只在确认被接管时;本地缓存文件 `snowflake-<kind>-<affinity>.json`,新增配置键 `SnowflakeCacheDir`(默认 `../../run/snowflake`,k8s 里由 ConfigMap 指到 emptyDir);cluster 0 兼容读旧 `snowflake_guard`/`guard_ms` 并把旧 `snowflake_ids`/`snowflake_nodes` 算作占用。
- **C++**(`cpp/libs/engine/core/node/system/snowflake/`):不写 affinity/released;申领 txn = `If Create(slots)==0 Then [Put slots(lease), Put watermark]`(第一笔水位与申领原子);**没有单独的 watch**,每个 tick 的水位 txn 自带 `Value==uuid` 比较,失败即按"被删/被抢"分支处理(重挂或永久 fence),等价且少一条流;`CLUSTER_ID` 环境变量覆盖;`SNOWFLAKE_QUARANTINE_SEC` 仅供故障注入;缓存文件 `<SNOWFLAKE_CACHE_DIR or ./data>/snowflake-scene-item.json`,与 Go 不共享;顺手修了 LeaseGrant RPC 失败会永久卡住注册的老问题(10s 超时重试)。
- 两端共用 `go/shared/snowflakealloc/testdata/selection_vectors.json`(15 例)做选号一致性测试,C++ 侧以 YAML 1.2 flow 方式读同一文件。

### 3.6 观测:三字段

每条日志前缀带 `node=<hostname/pod>`、`worker=<cluster:id>`、`inc=<slots key 的 mod_revision>`。id 会随重启变,`inc` 全局单调唯一,dashboard 按 node 聚合,排查撞号按 inc。

### 3.7 验收(故障注入,真集群跑)

1. **冻结**:`kill -STOP` 一个 scene,等 TTL+10s,起新 scene → 新 scene 拿到的 id ≠ 旧 id(隔离期生效);`kill -CONT` 旧 scene → 日志出现 fenced 且无新 item guid。
2. **同秒重启**:脚本在 1s 内 kill + 拉起 guild → 首个 guild_id 时间字段 > 前任最后水位。
3. **跨机偏差**:把新节点墙钟拨慢 30s 再申领刚释放的槽 → 被隔离期跳过。
4. **etcd 抖动**:`kubectl delete pod etcd-x`(3 副本)→ 无任何节点 fence;单副本环境把 etcd 停 30s → Go 服务继续发号,恢复后水位续写。
5. **本地缓存起**:停 etcd,重启 login → login 起来并能建号;起 etcd 后 `slots/<id>` 重新出现。
6. **一致性向量**:C++ / Go 各自用同一组 `(候选集, wm 表, now, Q)` 输入,选出的 id 必须一致(表驱动测试,两端共用 JSON 用例)。

---

## 4. 改造 B:C++ 解耦

- **路由 node_id 保持现状**(`allocated` key + `zone` key + lease),只做路由:Kafka topic `scene-N`、gate 会话反查、`SessionDetails.gate_node_id`。
- **发号 worker 独立**:scene 节点起一个 `SnowflakeSlotClient(kind="scene-item")`(3.1–3.5 协议),`SnowFlakeManager::OnNodeStart` 改吃它给的 `(cluster, slot)`。gate / battle **不需要**(`GenerateItemGuid` 在两者中零调用点),它们不再激活 `tlsSnowflakeManager`。
- **启动顺序**:路由 node_id 拿到 → `StartRpcServer`,**不再等发号器**;发号器激活独立进行,scene 的 `DependencyGate` 探针加一项 "snowflake ready",玩家进入前必须满足。发号器未就绪时 `GenerateItemGuid` 仍返回 `kInvalidGuid`(fail-closed,现状)。
- **session_id / buff / skill** 继续用路由 node_id(它们是路由/临时语义,不要求跨进程永久唯一)。顺手修:`World::InitializeSystemBeforeConnect` 在 node_id 分配前就 `SetNodeId(0)`,把 `tlsIdGeneratorManager.SetNodeId` 挪到 node_id 分配成功回调。
- **Fence 语义不变**:失去发号槽 → `SnowFlakeManager::Fence()`;失去**路由** node_id(被抢)→ 仍走 `OnNodeIdConflictShutdown`(存盘、改派、退出),但那条路径不再是 ID 正确性的一部分。
- 顺手清掉:重注册路径对 alloc/service key 仍用 `VERSION==0` CAS、旧 lease 活着必自杀的问题(端口 key 那次修法照抄到这两把 key)。
- 否决的备选:让 Go 做唯一分配器经 gRPC 发给 C++。理由:水位与自 fence 必须由持有者自己驱动,经 Go 中转多一跳、多一个故障域(Go 服务重启会让所有 C++ 节点同时失去心跳)。两端各实现一份**同一协议**,靠 3.7 第 6 条的共用向量测试保证一致。

---

## 5. 改造 C:17 位切成 [cluster5][node12]

- **分层原则(用户 2026-09-08 定):snowflake 发号器本身不改、不知道集群的存在。** 它仍然只接受一个 17 位的 worker 值(`set_node_id(worker)`),布局仍是 `[time32][node17][step15]`。"5 位集群 + 12 位槽"只是**分配器层**(Go `snowflakealloc`、C++ `snowflake_slot_client`)拼这个 worker 值时的内部约定,拼好直接设置;拆解只用于日志标签。业务代码与策划都碰不到。
- 逻辑布局:`[time32][cluster5][node12][step15]`;login 的 bwmarrin 13 位按 `[cluster3][node10]` 拼。
- **不作废存量,论证**:旧 id 中段 = node17(实际值 ≤ 几百)= 新布局 cluster=0 的 node;时间字段只增不减;只要 cluster 0 的 node 空间 ≤ 4096(login ≤ 1024)就与旧 id 逐位不撞。解码函数 `ParseGuid` / Go `NodeOf` 改成拆两段即可。
- `cluster` 来源:部署级常量 `CLUSTER_ID`(ConfigMap / env),`k8s_deploy.ps1` 加 `-ClusterId`,默认 0。**运维一次性设定,策划不碰**。C++ 进 `BaseDeployConfig.cluster_id`,Go 进各服务 config `ClusterId`。
- etcd prefix 带 `c<cluster>`(3.1),两个集群各自的 etcd 天然不撞;同一 etcd 里不同 cluster 也不撞。
- 全球同服的落法(与王者 / Wild Rift 一致):全球账号层(PUUID 类,由 login 的 `login-player` 号源全球唯一)+ 区域集群各自 cluster 号 + 区域内 zone 只是部署单元(TiDB 决策 D1 的 home_zone 语义不变)。
- 一致性:`ParseGuid`、`ComposeID`、Go `snowflake.go` 常量、`maxWorkerID=4095/1023`、`kMaxNodeId` 上界一起改;3.7 第 6 条向量测试覆盖 `(cluster, node, step, t) → id → 解码` 往返。

---

## 6. 改造 D:永久身份走号段(Leaf-segment)

### 6.1 为什么

player_id / guild_id / item guid 是永久身份:它们躺在 TiDB 主键、好友表、公会表、玩家 blob 里几年。永久身份需要的只是"全局唯一 + 不重发",不需要"编码铸号时间和铸号节点"。号段模式没有 lease、没有时钟、没有 worker:一个持有者用一次 CAS 从表里领走 `[lo, hi)`,冻结/重启/时钟回拨都不影响唯一性,协调器只在续段时碰一次。这是美团 Leaf-segment、微信 seqsvr、GitLab Cells 的做法。

### 6.2 机制

```sql
CREATE TABLE id_segment (
  biz_tag  VARCHAR(64) NOT NULL,
  max_id   BIGINT UNSIGNED NOT NULL,      -- 已分配到(不含)
  step     INT UNSIGNED NOT NULL,
  version  BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (biz_tag),
  CHECK (max_id < 36028797018963968)      -- 2^55,见 6.3
);
-- 领段(一次往返):
UPDATE id_segment SET max_id = max_id + step, version = version + 1
 WHERE biz_tag = ? AND version = ?;
SELECT max_id, step FROM id_segment WHERE biz_tag = ?;
```

- **落码注记**:proto2mysql v0.1.0 把 `string` 主键渲染成 MEDIUMTEXT(MySQL 1170 建表失败),所以 `id_segment` 是全仓唯一允许用专门 bootstrap 语句预建的表(`biz_tag VARCHAR(64)`),与 go/db 预建 `user_accounts` 同一模式;其余列仍由 proto 驱动。`id_segment` 消息放在 `rollback_database_table.proto`,不能放 `mysql_database_table.proto`(zone 库表清单是从那个文件的消息成员派生的,放进去会在每个 zone 库都建一张)。
- 客户端 `go/shared/idsegment`:双 buffer,当前段用到 10% 时后台预取下一段;`step` 按"10 分钟峰值发号量"配(player 100、guild 100、item 1,000,000)。
- 表放全局库(data_service 的 `SnapshotMySQL`,将来 TiDB)。单调号在 TiDB 上同样用 D3 的 NONCLUSTERED + SHARD_ROW_ID_BITS,不新增热点。
- C++ 不直连 MySQL(仓库里没有 C++ MySQL 客户端):`DataService` 加 `AllocateIdSegment(biz_tag, step) → (lo, hi)`,scene 通过已有 gRPC 通道领段。

### 6.3 与存量 snowflake 号不撞(不迁数据)

| ID 种类 | 存量最小值(布局决定) | 号段值域 |
|---|---|---|
| player_id(bwmarrin,epoch 2024-07,毫秒<<22) | ≈ 2.8 × 10^17 | [1, 2^55 ≈ 3.6 × 10^16) |
| guild_id / item guid(epoch 2026-03,秒<<32) | ≈ 6.7 × 10^16 | 同上 |

号段从 1 起,上限 2^55 由 CHECK 守住;两个区间永不相交,所以**新旧两套号可以在同一张表里共存,不需要改一条存量数据**。2^55 个号:每天 10 亿件物品也够用 1 万年。

### 6.4 接线

- **player_id**(Phase 4):login `CreatePlayer` 改用 `idsegment(biz="player")`;`PlayerIDGen` 与 `/login-player` 槽保留一个版本做回滚开关,之后删。
- **guild_id**(Phase 4):`CreateGuild` 同理。
- **item guid**(Phase 5,最后):`ItemStore::MintGuid` 改从 `SegmentClient(biz="item")` 取;段耗尽且续段失败 → 返回 `kInvalidGuid`(与今天 fence 语义一致,bag 已 fail-closed)。可选:续段失败时回退 snowflake 发号——因为 6.3 的值域不相交,这个回退是**安全**的,代价是 snowflake 机器永远留着。默认不回退,只告警,由你选。
- **继续用 snowflake 的**:snapshot_id、tx_id(需要时间序,且已经是 Kafka 去重键)、scene_id、battle_id / challenge_id、session_id。它们走第 3 节的协议。

### 6.5 号段的弱依赖

双 buffer 让每个持有者在库不可用时还能发完手里两段(按 step 配 10~20 分钟);库恢复后自动续。**这才是"永久 ID 发号不依赖协调器"的真正落点**,3.5 的本地缓存只是给时序 ID 用的补丁。

---

## 7. 阶段顺序、依赖与验收

| Phase | 内容 | 依赖 | 验收 |
|---|---|---|---|
| 0 | 2.0a / 2.0b / 2.0c(1~4 步;第 5 步可延后) | 无 | 各自的验收项 |
| 0.5 | etcd 改 3 副本 StatefulSet + PVC | 无 | 删任一 etcd pod,节点无 fence |
| 1 | Go:3.2 选号 + 3.4 自 fence + 3.5 缓存 + 3.6 日志;`allocator.go` 单测 + 真 etcd 集成测试 | 0b | 3.7 的 2 / 4 / 5 |
| 2 | C++:4 解耦 + `SnowflakeSlotClient` + 3.3 水位到 etcd;共用向量测试 | 1 | 3.7 的 1 / 3 / 6 |
| 3 | 5 位切分:两端常量 + 解码 + `-ClusterId` + prefix 带 c<cluster> | 2 | 往返向量;cluster=1 的第二套 dev 环境与 cluster=0 同时发号不撞 |
| 4 | 号段:表 + `idsegment` 客户端 + player_id / guild_id 接线 | 0c 第 3 步(表走 proto) | 建号压测;停库 10 分钟仍能建号 |
| 5 | item guid 号段 + `AllocateIdSegment` RPC | 4 | bag 全量用例;停 data_service 10 分钟仍能拾取 |

每个 Phase 独立可回滚:1 不改 key 布局,2 只加不减,3 是常量,4/5 靠值域不相交与旧号共存。

---

## 7.5 2026-09-08 晚间修订(用户拍板,覆盖上文冲突之处)

按"10 万台 scene、每天重启、灰度金丝雀"的目标规模重新审视后,决定:

1. **scene 彻底退出 snowflake 槽位协议。** item guid、tx_id、snapshot_id 三种全部走号段(biz_tag `item` / `txlog` / `snapshot`);tx_id 与 snapshot_id 的用途是去重键与主键,不需要时间序,时间在 `timestamp_sec` / `created_at` 列里。理由:10 万节点每秒 10 万次水位写 + 10 万次 lease 续约超出单个 etcd 一个数量级,任何"每节点一个 etcd 槽"的设计在这个规模都不成立;而号段对节点数不敏感。由此 C++ 侧的 `SnowflakeSlotClient`、`snowflake_slot_selection`、`snow_flake.h` 今日新增的集群/槽位常量与期限 fence、缓存文件、灰度双向兼容、`released` 标记**全部作废删除**,`snow_flake.h` / `snow_flake_manager.h` 回到改造前形态。`ItemIdSegment.FallbackToSnowflake` 随之删除(没有槽位就没有可回退的发号器,只保留 fail-closed)。
2. **snowflake 槽位协议只服务 Go 的四种**(login-player、guild、scene-manager、match,合计几十个实例)。4096 槽对它们没有容量压力,现有"同主机优雅重启复用自己的槽"够用,**不再**追加按槽位的通用 released 标记。
3. **一种 GUID 一个号段客户端实例。** C++ 改成通用 `GuidSegmentClient` + 按类型的注册表,启动时按配置实例化 item / txlog / snapshot,以后 pet、guild 等只加一行注册;各实例独立缓冲、预取、退避、指标,共享一条到 data_service 的 gRPC 连接,响应按请求编号分发。Go 侧 `idsegment.Client` 本来就是通用的,login/guild 各持一个实例。
4. **动态 step(Leaf 口径)。** 客户端记录每段消耗时长:不到 15 分钟用完则下次 step 翻倍,超过 30 分钟则减半,上下限各钉一个(item 默认 [1000, 1,000,000],初值 20,000)。每天重启一次时浪费 ≤ 2×step,2^55 在 10 万台每天重启下仍可撑两万年以上;此项只为把浪费压到最小,不是正确性需求。
5. **不做的**:优雅关服把没用完的尾巴还给数据库(会破坏"每段必比上一段大"这条让客户端能整段拒收服务端错误的不变量);本地 json 缓存改 Redis(Redis 是另一个协调器,崩了数据就没了;缓存的唯一意义是 etcd 不通时还能起,只能靠本机磁盘);Go 侧缓存改为**故障期才写**(etcd 水位写失败时才把本进程高水位落盘,稳态零写盘)。
6. **号段客户端不持久化游标**:一段在 `AllocateIdSegment` 提交时就已被数据库判定为用掉,进程崩溃只产生空洞不产生重号,这正是号段优于 snowflake 之处。
7. **`id_segment` 行被重置/从备份恢复是 ID 安全事件**:生产环境不自动补种行(`AllowAutoSeed` 仅 dev),行必须由迁移显式创建;运维手册记录"恢复全局库前须先核对 max_id ≥ 消费表最大号"。
8. **合服与账号角色列表的缺口(已确认未修,待拍板)**:`AccountSimplePlayer.zone_id` 是建角快照,`RemapHomeZoneForMerge` 只改 data_service 映射不改账号 blob;登录取角色列表时应经 `BatchGetPlayerHomeZone` 解析当前归属,并在 EnterGame/SceneManager 校验 `home_zone == 当前区`。player_id 全局唯一,合服不改号,不搬主数据。

## 8. 需要你拍板的点

1. Q = 4h、F = 2h 是否接受(影响:一个槽被释放后 4 小时内不会被复用;login 1024 槽 × 4h 足够,除非一小时内重启超过 1000 次)。
2. item guid 是否真的走号段(Phase 5),还是只做 player_id / guild_id。
3. 号段续段失败时是否允许回退 snowflake(6.4)。
4. `transaction_log` / `player_snapshot` 定在全局库一处(2.0c 第 3 步),从 go/db 的 zone 库表清单里摘掉。
5. etcd 改 3 副本 StatefulSet 是否本轮一起做。
