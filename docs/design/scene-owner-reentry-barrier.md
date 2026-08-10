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

### 3.1 前置(P0,已就绪的小修):IsNodeAlive 三态,unknown 一律 fail-closed

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

设计(**不改 proto,用 sidecar key 避免动 Go/C++/Java 三端生成码**):

- 新增 `player:{id}:owner_epoch`(纯整数),与 `player:{id}:location` 同一原子域,用 Lua 写:
  - Go 改派玩家到新节点时,`INCR owner_epoch` 并把新值随改派请求下发给新的 C++ 节点。
  - C++ 节点在 `SavePlayerToRedis` 落 `PlayerAllData` 前,用 Lua 做 `expect_epoch == owner_epoch` 才写;不等就丢弃并打 `stale_owner_write_rejected`(这条日志是健康信号,压测期应恒 0)。
  - Go 的 `updatePlayerLocationWithRaw`(`changesceneutil.go:53`,**此文件当前干净,可改**)同样走 epoch CAS。
- 老节点 emergency relocate 时,它手里是**旧 epoch**;一旦 Go 已经因屏障到期改派并 `INCR`,老节点的 `S_final` 写就被 CAS 拒——即使时间估算失手也不回档。

⚠️ **半接线警告**:只在 Go 侧写 epoch、C++ 不校验,等于假防护(会给人"已经防住了"的错觉,实际 `PlayerAllData` 仍被老节点覆盖)。第二层**必须 Go 写 + C++ 校验同时上**,否则不如不上。所以它排在屏障之后、作为一个完整的跨语言小项单独做。

## 4. 与并发编辑者的协调

本次会话发现 `load_reporter.go` / `enterscenelogic.go` / `logic_test.go` 有未提交的并发改动(gate_instance_id 改 fail-closed、死节点计数残留清理),不是本人所改。§3.1 的 IsNodeAlive 与 §3.2 的屏障落点都在这几个文件里,**按仓库"逐文件确认归属"的协作纪律,须等这些改动落定或与作者协调后再动**,不要盲改造成冲突。

`changesceneutil.go`(§3.3 的 Go epoch 写路径)当前干净,可独立推进。

## 5. 分阶段落地顺序

1. **P0-a** IsNodeAlive 三态(§3.1)—— 一个函数,挡住"Redis 抖一下就改派"。
2. **P0-b** 再入屏障(§3.2)—— death_at 记录 + 改派前屏障检查 + 屏障常数与 C++ drain 同源。
3. **P1** owner_epoch CAS(§3.3)—— Go 写 + C++ 校验,一次性做完,别只做半边。

前两步就能把 §2 的分区双写窗口关掉;第三步是时间估算失手时的确定性兜底。
