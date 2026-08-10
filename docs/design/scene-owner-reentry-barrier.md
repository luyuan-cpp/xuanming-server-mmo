# 场景所有权:再入屏障 + owner_epoch(防 etcd 分区期双写/回档)

状态:待评审(本文只定方案供拍板,未落任何强耦合代码)
关联:`docs/design/player-async-save-loss-windows.md`、`go/player_locator/internal/logic/session_cas.go`(已有的会话代际 CAS,是本方案要对齐的范式)

## 0. 一句话

etcd 租约到期时,Go scene_manager 和 C++ scene 节点会**各自独立**对同一个信号做反应,两条时间线之间**没有任何互相等待**——新节点可能在老节点还在存盘 drain 时就 load 了旧状态,造成同一玩家在两个节点上双写、回档。修法是给"新节点接管"加一道**再入屏障**(等老节点一定停笔),再给玩家数据加一个**owner_epoch CAS**(迟到写一律拒),两层叠加。

## 1. 现状时间线(读代码得到,非推测)

角色:
- **C++ scene 节点** = 玩家的权威持有者。存盘走 `SavePlayerToRedis`(`libs/services/scene/player/system/player_lifecycle.cpp`)。
- **Go scene_manager** = 观察者/路由器。不持有玩家,只维护 Redis 里的 `scene:{id}:node`、`player:{id}:location`、负载集,并在判死时改派。

C++ 节点丢租约时(`OnNodeIdConflictShutdown`,`node.cpp:728`),四个触发点:keepalive TTL=0 / 本地租约 deadline / 重注册 CAS 失败 / Watch 发现身份被抢。它做的事**顺序是对的**:

1. `tlsSnowflakeManager.Fence()` —— 立刻停发号。
2. 停健康检查 + 停注册重试。
3. `onConflictShutdownFn_` = `BeginEmergencyRelocateAll()`(`scene/main.cpp:190`):**抄会话 → 存盘 → 存盘落地后才发改派请求**。注释明确:反过来做新节点会读到存盘前旧数据(回档)。
4. `StartConflictDrainWatchdog()`:有界 15s(`kDrainBudget`),存盘全落地或超时才 `Shutdown()`(flush Kafka、释放租约)。

关键配置(`bin/etc/base_deploy_config.yaml`):
- `NodeTTLSeconds: 180` —— C++ 节点 etcd 租约 TTL(2026-05-24 压测把 60s 提到 180s,因为 60s 在 45k 开服浪涌下 keepalive 抖动就会误触 FATAL)。
- `HealthCheckInterval: 1` —— 健康检查每秒一次。
- C++ drain 预算 = 15s(硬编码在 `node.cpp:774`)。

Go 侧(`load_reporter.go` `watchAndRefresh` → `handleWatchEvent`):
- etcd `EventTypeDelete` → 立刻 `removeNodeFromRedis` → `reconcileDeadNodeScenes`(强制销毁孤儿实例场景)。
- `resolveScene`(`enterscenelogic.go:571`):`!IsNodeAlive` → 挑新节点 + `Set(scene:{id}:node)` + `RequestNodeCreateScene`。
- `IsNodeAlive`:节点是否在 Redis 负载 ZSET 里 —— **当前实现把 Redis 报错也当成"已死"**(见 §3.1)。

## 2. 缺口:两条时间线在同一时刻并行,互不等待

网络分区(C++ 节点↔etcd 断,但节点↔Redis 仍通)是最危险的情形:

```
T0            节点与 etcd 失联(节点仍在跑,仍在服务玩家)
T0+180s   etcd 租约到期
          ├─ etcd 发 DELETE → Go 立刻判死、改派、resolveScene 把玩家 P 搬到新节点 N2
          └─ C++ 老节点健康检查(每秒)发现租约过期 → 开始 emergency relocate:
             存盘 P 的最终态 S_final → 15s drain
T0+180s ~ T0+195s   老节点仍在写 S_final;而 N2 可能已在 T0+180s+ε load 了更旧的 S_earlier
                    → 玩家在 N2 上继续玩产出 S_new,老节点的 S_final 与 N2 的 S_new 互相覆盖
```

C++ 侧的"存盘→改派"顺序只保护**它自己驱动的那条改派**;它管不住 Go 因为看到同一个 etcd DELETE 而**独立发起**的改派。这就是双写/回档的根因。

补充:硬崩溃(SIGKILL/OOM/段错误)不走优雅 drain,老节点直接不写了,Go 改派是安全的(只会回档到上次周期存盘,任何系统都躲不掉)。**唯一真正危险的是"老节点还活着还能写 Redis,但 Go 已经判死改派"**,即上面的分区情形。

## 3. 修法(两层,可分阶段上)

### 3.1 前置(P0):IsNodeAlive 三态,unknown 一律 fail-closed —— ✅ 已落码

状态:**已完成**(`load_reporter.go` `IsNodeAlive`),含两条成对回归测试
`TestIsNodeAliveTreatsRedisOutageAsAlive` / `TestIsNodeAliveReportsDeadWhenMemberMissing`
(`logic_test.go`)。已验证前者在修复前必失败、修复后通过;`go build` / `go vet` /
`go test ./...` 全绿。

`IsNodeAlive` 现在 `return err == nil`,把"成员不在集合(确实死)"和"Redis 连不上(状态未知)"折叠成同一个"死"。一次 Redis 抖动 → 活节点被判死 → 触发改派 → 叠加 §2 就是双写。

go-zero v1.10.0 的 `ZscoreCtx` 原样返回 go-redis 错误,且 go-zero 已重导出 `redis.Nil`,不用加依赖。改成:

```go
func IsNodeAlive(svcCtx *svc.ServiceContext, zoneId uint32, nodeId string) bool {
	if isKnownNodeIdentityAmbiguous(zoneId, nodeId) {
		return false
	}
	_, err := svcCtx.Redis.Zscore(nodeLoadKey(zoneId), nodeId)
	if err == nil {
		return true
	}
	if errors.Is(err, redis.Nil) { // 明确不在负载集 = 已死
		return false
	}
	// Redis 状态未知:判死会触发改派+CreateScene,与 C++ 15s drain 叠加会双写。
	// 未知一律按"存活"处理(fail-closed against 改派),让本轮走可重试路径。
	logx.Errorf("[LoadReporter] IsNodeAlive 状态未知按存活处理: node=%s zone=%d err=%v", nodeId, zoneId, err)
	return true
}
```

边界:"未知按存活"对**改派/销毁/rebalance** 调用点是对的方向;对**给新玩家挑节点**的调用点(`world_init`/`createscenelogic`)意味着可能路由到疑似已死节点——那是可重试的瞬时降级,不是数据损坏。要更严就得升三态枚举让各点自判,属后续。

### 3.2 第一层(P0):再入屏障 —— Go 判死后不得立即改派

**核心不变量**:老节点最晚可能写入的时刻 < 新节点最早被允许接管的时刻。

推导屏障值:老节点的 drain 从"租约到期"(= etcd DELETE 时刻)开始,持续最多 15s。所以 Go 在**看到 DELETE 之后,还要再等 `drain(15s) + 时钟余量`** 才能把玩家/场景改派到新节点。180s 那段在信号到达时已经花掉了,增量屏障只是 drain+skew:

```
SceneReentryBarrier = C++ drain 预算(15s) + 时钟/调度余量(建议 5s) = 20s
```

落地(**这两处在 load_reporter.go / enterscenelogic.go,当前有并发编辑者,须协调后再改,见 §4**):

1. `handleWatchEvent` 的 `EventTypeDelete` 分支:记录 `SET node:zone:{z}:{n}:death_at = now_ms`(带 TTL 略大于屏障即可)。
2. `resolveScene` / `RebalanceWorldChannelsForZone` 的 dead-node 分支:改派前先读 `death_at`,若 `now - death_at < SceneReentryBarrier`,**不改派**,返回可重试错误(让上游带 conf id 退避重试),而不是立刻 `Set(scene:{id}:node, newNode)`。
3. 屏障常数放 `internal/constants` 单一入口,并在启动时机械校验 `SceneReentryBarrier >= C++ drain`。最好把 C++ 的 15s drain 也从硬编码提到 `base_deploy_config.yaml`,Go 和 C++ 读**同一个值**,否则两边各改各的又会劈叉(这正是 XuanMing `pkg/placement` 把脑裂常数集中成单一派生入口的原因)。

屏障挡住的是"gameplay 层面两个节点同时认领同一玩家"。

### 3.3 第二层(P1):owner_epoch CAS —— 迟到写一律拒(backstop)

屏障靠时间估算,极端情况(drain 超 15s 预算、时钟余量估低)仍可能擦碰。epoch 是不依赖时间的兜底:每次归属变更单调 +1,老 epoch 的写被原子拒绝。这与 `session_cas.go` 已有的 `session_version` CAS 是同一范式。

存储用 sidecar key `player:{id}:owner_epoch`(纯整数),与 `player:{id}:location` 同一原子域,不进 `PlayerAllData` blob。

- **铸造(Go,唯一写者)**:`EnterScene` 是唯一的归属变更闸口(见 §6.1),每次把玩家(重新)分配到某节点时 `INCR player:{id}:owner_epoch`,拿到新值 N。
- **传递(必须随路由事件下发,不能让节点自己读 Redis)**:把 N 写进 `RoutePlayerEvent` 的新字段 `owner_epoch`,随 Kafka 路由给目标节点。
  - ⚠️ **为什么不能让 C++ 节点在 load 时自己 `GET owner_epoch`**:两次改派挨得近时(先派 A 再派 B),A 若在 Go 已 `INCR` 到 N+1 之后才去读,就会读到 N+1 并与 B 一样自认为最新 → **双主**。epoch 必须跟着"这一次路由决策"走:Go 给 A 的事件带 N、给 B 的带 N+1,只有最新的 B 的缓存值与 Redis 相等。这需要给 `RoutePlayerEvent` 加一个 proto 字段(见 §6)。
- **校验(C++)**:节点在 `SavePlayerToRedis` 落 `PlayerAllData` 时,用 Lua 原子做 `GET owner_epoch == 我缓存的 epoch` 才写;不等就丢弃并打 `stale_owner_write_rejected`(健康信号,压测期应恒 0),并销毁本地实体(我已被废黜,不能再存也不能再改派)。
- 老节点 emergency relocate 时,它手里是**旧 epoch**;一旦 Go 已经因屏障到期改派并 `INCR`,老节点的 `S_final` 写就被 CAS 拒——即使时间估算失手也不回档。
- Go 的 `updatePlayerLocationWithRaw`(`changesceneutil.go:53`,**此文件当前干净,可改**)写 `player:{id}:location` 时同样走 epoch CAS,别让两条通道各写各的。

⚠️ **半接线警告**:只在 Go 侧写 epoch、C++ 不校验,等于假防护(会给人"已经防住了"的错觉,实际 `PlayerAllData` 仍被老节点覆盖)。第二层**必须 Go 写 + C++ 校验同时上**,否则不如不上。所以它排在屏障之后、作为一个完整的跨语言小项单独做。

## 4. 与并发编辑者的协调

本仓有活跃的并发编辑者(本文写作期间 `player_locator`、`cpp/` 多处、`go/shared/` 均有未提交改动,并新增了 `session_reconciler.go`),按仓库"逐文件确认归属"的纪律,动手前先看 `git status` 确认目标文件没有他人未提交的改动。

§3.1 落码时 `load_reporter.go` / `logic_test.go` 已确认干净(先前观察到的 ` M` 是 `core.autocrlf=true` 造成的 stat-cache 假象,`git diff` 为空),故已安全落码。

§3.2 屏障的落点在 `load_reporter.go`(记 `death_at`)与 `enterscenelogic.go`(改派前检查屏障),动手前需重新确认这两个文件的归属。`changesceneutil.go`(§3.3 的 Go epoch 写路径)当前干净。

## 5. 分阶段落地顺序

1. **P0-a** IsNodeAlive 三态(§3.1)—— 一个函数,挡住"Redis 抖一下就改派"。
2. **P0-b** 再入屏障(§3.2)—— death_at 记录 + 改派前屏障检查 + 屏障常数与 C++ drain 同源。
3. **P1** owner_epoch CAS(§3.3)—— Go 写 + C++ 校验,一次性做完,别只做半边。

前两步就能把 §2 的分区双写窗口关掉;第三步是时间估算失手时的确定性兜底。

## 6. C++ 侧落地(第三层 owner_epoch 的 C++ 半边,精确到 file:line)

读代码确认的三个事实,决定了 C++ 侧比预想的干净:

1. **改派唯一走 `EnterScene`**:老节点紧急疏散时 `DispatchEmergencyRelocate` 最终调 `scene_manager::SendSceneManagerEnterScene`([player_lifecycle.cpp:773](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp)),`scene_id`/`scene_conf_id` 都留 0,让 Go 按世界频道表挑存活节点。也就是说无论"Go 独立判死改派"还是"老节点自己驱动改派",都汇聚到 Go 的 `EnterScene` 这一个闸口——epoch 在那里 `INCR` 一次即可覆盖两条路径。
2. **存盘本来就走 Lua**:`SavePlayerToRedis` 的实际写入是 `tlsRedisSystem.GetPlayerDataRedis()->Save(message, playerId)`([player_lifecycle.cpp:1228](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp)),底层 `MessageAsyncClient::Save` 走 EVALSHA 脚本 `kSaveAndMarkLuaScript`([redis_client.h:18](../../cpp/libs/engine/infra/storage/redis_client/redis_client.h)):`SET KEYS[1] ARGV[1]` + `SADD dirty_keys_set`。**epoch CAS 塞进这段 Lua 就是原子的,没有 TOCTOU。**
3. **组件模式现成**:玩家实体上已有 `PlayerSessionSnapshotComp`、`PlayerLastPersistedSnapshotComp`,新增 `PlayerOwnerEpochComp` 是同一范式。

### 6.1 收 epoch:load 时缓存

`HandlePlayerAsyncLoaded`([player_lifecycle.cpp:179](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp))从 `tlsPendingEnterMap` 取本次 EnterScene 的待入场信息、建玩家实体。在建实体处 `get_or_emplace<PlayerOwnerEpochComp>(player).epoch = enterInfo.owner_epoch()` —— epoch 来自 `RoutePlayerEvent`/`EnterSceneRequest` 里新加的字段(Go 铸造后带下来,§3.3)。新玩家首登 Go 的 `INCR` 建键得 1,一样带下来。

### 6.2 校验 epoch:save 时原子 CAS

两种改法,推荐前者:

- **(a) 原子 Lua(推荐)**:因为 `kSaveAndMarkLuaScript` 是泛型 `MessageAsyncClient<K,V>` 共用的,不要直接改它污染别的类型。给 `Save` 加一个可选重载 `Save(message, key, guardKey, expectedValue)`,内部换用带 epoch 校验的变体脚本:
  ```lua
  -- KEYS[1]=PlayerAllData key, KEYS[2]=owner_epoch key
  -- ARGV[1]=payload, ARGV[2]=expected_epoch
  if redis.call('GET', KEYS[2]) ~= ARGV[2] then return 0 end
  redis.call('SET', KEYS[1], ARGV[1])
  redis.call('SADD', 'dirty_keys_set', KEYS[1])
  return 1
  ```
  返回 0 时走 `save_failed_callback_`,`SavePlayerToRedis` 的调用方据此判定"已被废黜":停止对该玩家的存盘重试、销毁本地实体、**不发 relocate**。
- **(b) GET 比对 + 屏障兜底(次选)**:`SavePlayerToRedis` 顶部同步 `GET owner_epoch` 比对缓存,不等就 bail。有 GET→异步 SET 之间的 TOCTOU,靠屏障(新主延迟接管)+ 一个 ≤ 屏障的周期自检来关。比 (a) 简单但不是原子的。

### 6.3 别漏掉 Kafka→MySQL 这条持久化通道

`SavePlayerToRedis` 除了写 Redis,还给每张分表发 `DBTask` 到 Kafka([player_lifecycle.cpp:1260](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp))→ db 服务 key-ordered consumer 落 MySQL。**只在 Redis 侧做 epoch CAS,被废黜节点的 DBTask 仍可能后到并覆盖 MySQL**。两个办法:①DBTask 带上 `owner_epoch`,db 的 `key_ordered_consumer` 落库前比对(它已有 applied-seq 单调守卫,天然是挂载点);②直接复用现有 applied-seq——只要保证 epoch 变更时 seq 也跳变。这一条不做,等于关了 Redis 的门却留了 MySQL 的窗。

### 6.4 C++ 改动清单(供 Codex 编译验证)

- proto:`RoutePlayerEvent`(及承载它的 `EnterSceneRequest`)加 `uint64 owner_epoch`;按 §5 的 buf 纪律留号不复用。
- 新增 `PlayerOwnerEpochComp`(`libs/services/scene/player/comp/`)。
- `HandlePlayerAsyncLoaded`:建实体处缓存 epoch(§6.1)。
- `MessageAsyncClient::Save` 加带 guard 的重载 + 变体 Lua(§6.2a);`SavePlayerToRedis` 传 `player:{id}:owner_epoch` 与缓存 epoch。
- save 被拒的 callback:停重试 + 销毁实体 + 不 relocate。
- DBTask 带 epoch 或复用 applied-seq(§6.3)。

以上全部依赖 Go 侧先把 epoch 铸出来并带进 `RoutePlayerEvent`(§3.3),否则 C++ 单独上就是校验一个永远不变的 0 —— 假防护。所以 P1 必须 Go+C++ 同一批做。
