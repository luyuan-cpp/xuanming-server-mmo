# 登录排队压测 — 判读清单

> 配合 `robot/main.go` 的 queue-aware 模式 + `etc/login.yaml` 的 `Queue.Enabled=true`
> 使用。robot 的 `[stats ...]` 周期日志会持续输出关键指标。

---

## 起步:一个有意义的压测场景

```yaml
# robot/etc/robot.yaml
robot_count: 5000
gateway_addr: "http://127.0.0.1:8081"
use_http_login: true
zone_id: 1
# 用 1000 robot 当 baseline,5000 robot 触发排队
```

```yaml
# go/login/etc/login.yaml
Queue:
  Enabled: true
  DispatchInterval: 1s
  AdmitTTL: 60s
  DefaultRetryAfterMs: 2000
  SoftCapMultiplier: 1.5
```

区容量不在 login.yaml 里(2026-11-10 起只认网关区服目录 `zone_config.capacity`)。压测前把 zone 1 的容量改小,让 5000 个 robot 一定触发排队:
GET 一次 `/admin/zones/1` 拿整行,把 `capacity` 改成 `1000` 再整行 `PUT /admin/zones/1`(PUT 要给全每个业务字段)。本区网关至多 5 秒写到 etcd,
login 至多再 2 秒读到;login 日志 `[ZonePolicy] zone 1 policy changed: capacity=1000` 出现后再起 robot。压完改回原值。

> **合入门槛(2026-10-15)**:在 `docs/handoff/launch-readiness-20261013/spec_G4b.md` §7「合入门槛」的三项都满足、`docs/PROGRESS.md` 有记录之前,上面的 `Queue.Enabled: true` 只在本机验证时用(清门槛所需的真实 Redis 冒烟即下方「验证清单」第 2、3 步与 `spec_G4b-G.md` §8.4),写在 scratchpad 里的配置副本上,不改 `go/login/etc/login.yaml`。部署环境一律不打开排队:K8s 由 `tools/scripts/k8s_deploy.ps1` 的 `$script:LoginQueueMergeGateCleared` 机械拦截(未清时各档位排队都关、`-LoginQueue on` 拒绝),手改 ConfigMap 或 login.yaml 的 `Queue.Enabled` 同样违规。

先按当前启动脚本或 [K8s 开服手册](k8s-open-server-runbook_zh.md) 启动完整依赖链,确认 login、gate 与 Java gateway 就绪后再起 robot;路由模式还要求先有 client-rpc-router。本页是判读清单,不替代根 AGENTS.md §6 的每轮基线、数据清理与三阶段采样门禁。

---

## 核心健康指标(看 robot 的 `[stats ...]` 日志)

| 指标 | 健康范围 | 红线含义 |
|---|---|---|
| `q_entered` | 4000±200(总数 5000 − 容量 1000) | 远低于 4000 = 容量计算错了/排队判定没触发 |
| `q_admitted` | 单调上升,最终 → q_entered;增速受下方「放行节奏」的上界约束 | `在线 + login_queue_admitted_inflight ≥ capacity` 时不涨是**正常回压**;停滞且 `在线 + 在途 < capacity` 才是 dispatcher 卡死(模式 1) |
| `q_expired` | < 1% of q_entered | > 5% = AdmitTTL 太短 OR 客户端轮询太慢 |
| `q_avg_wait` | < 60s(1000/s admit 速率下;待 L-14 重新标定) | > 5min = dispatcher 严重落后 EnterGame |
| `q_max_rank` | 单调下降到 0 | 长时间不变 = 该 robot 卡住,看 Redis 检查 |
| `login_ok` 增速 | 不超过「放行节奏」的上界(G4b-G 之前写的是「接近 zone capacity / login_avg」,待 L-14 重新标定) | login_ok 不涨但 q_admitted 涨 = gate 链路坏 |
| `recon_fb` | 接近 0 | > 5% = 重连豁免判定有 bug,看 player_locator 日志 |

**放行节奏(G4b-G,2026-10-13 起)**:本节的节奏数字(上表的增速、下面的样例与「关键观察」)写于 G4b-G 之前,没有按修复后的上界标定,**待 L-14 重新标定**。修复后的上界:

- 每次放行都以 `在线 + 在途 ≤ capacity` 为界。排队放行的人取走票后,名额 20s 内不放回(在途占位再留 20s,盖住 gate 人数的上报滞后;2026-10-15 起,此前是 15s);之后他若已在线,名额转给在线数,也不放回。
- 只有拿了票没连 gate、或连上很快就走的那部分名额,每约 22~23s(取走延迟 + 20s)周转一次(15s 时是约 17~18s)。
- 所以 `在线 + login_queue_admitted_inflight ≥ capacity` 时 `q_admitted` 不涨是**正常回压,不是模式 1**。robot 登录后保持在线时(未核实),放满 capacity 之后 `q_admitted` 只随 robot 下线而涨。
- 推导见设计稿 §5「在途占位的生命周期」与 `docs/handoff/launch-readiness-20261013/spec_G4b-G.md` §5.2「放行节奏的上界」。

**一拍的放行怎么分到 gate(2026-10-15 起;未编译、未测试)**:dispatcher 每个非空拍只读一次 gate 目录(etcd 前缀读 + 排空标记 MGET),算容量与挑 gate 共用这一份;批内按「gate 上报的 `PlayerCount` + 本拍已分给它的人数」挑最少的(同数取 NodeID 最小),一拍的放行先填最闲的 gate,填平之后在各台之间轮转。判读:

- 排队放行期间,各 gate 的在线数应先把最闲的那台补上来,之后大致同步上涨。
- 各台在线数已经相近、一拍放行的人数又多于台数时,放行仍全部落在同一台 gate 上(只有一台的 `PlayerCount` 在涨,其余不动)= 跑的是 10-15 之前的版本(那时每放一个人重读一次目录,只按 `PlayerCount` 挑,而它 10s 才上报一次,一拍之内不变)。某台明显比别的闲时,整批先去填它属正常。
- 快速通道(直接签票)仍是每次单独挑一次,规则相同,不跨请求记账。

### 周期 report 示例(健康状态)

```
[stats 30s] conn=5000 login_ok=920 login_fail=12 ... q_entered=4080 q_admitted=920 q_expired=3 q_avg_wait=28.4s q_max_rank=4078
[stats 60s] conn=5000 login_ok=1980 login_fail=15 ... q_entered=4080 q_admitted=1980 q_expired=5 q_avg_wait=31.2s q_max_rank=4078
[stats 90s] conn=5000 login_ok=3050 login_fail=18 ... q_entered=4080 q_admitted=3050 q_expired=7 q_avg_wait=33.8s q_max_rank=4078
```

**关键观察**:`q_admitted` 的涨速以上面「放行节奏」为准。样例里每 30s 涨约 1000(一个 zone capacity)写于 G4b-G 之前,成立的前提是放行的人不长期占着名额;robot 保持在线时,放满 capacity 之后 `q_admitted` 不涨属正常回压(先核对在线 + 在途是否已到 capacity),这个数字待 L-14 重新标定。`q_avg_wait` 稳定在 30s 附近(等于排队的平均位置 × 1s 出队节奏);`q_max_rank` 在第一波入队后就固定不变(没有新的"插队")。

---

## 异常模式速查

> **本页的 `redis-cli` 连哪个实例(2026-11-01 补)**:排队键在 login 的 `Node.QueueRedis` 指向的实例上:拆出 queue 时是 `redis-queue-g<N>`(`dev_tools.ps1 -Command k8s-redis-split-status` 的 `login.QueueRedis` 一列),没拆时是核心 `redis`。单实例形态 exec 进 `<名>-0` 就在主上。高可用形态 `<名>-0` 不一定是主,从上的读可能滞后、写回 `READONLY`,要写 `-h <名>` 经代理落到当前的主,`xargs` 后面那个 `redis-cli` 也要带:
>
> ```
> kubectl -n <infra> exec statefulset/<名> -c redis -- sh -c 'if [ -n "$REDIS_PASSWORD" ]; then export REDISCLI_AUTH="$REDIS_PASSWORD"; fi; redis-cli -h <名> ZCARD queue:zone:1'
> ```

### 模式 1:dispatcher 卡死
```
[stats 30s] ... q_entered=4080 q_admitted=20 q_avg_wait=22s q_max_rank=4078
[stats 60s] ... q_entered=4080 q_admitted=20 q_avg_wait=22s q_max_rank=4078  ← admitted 不涨
```
**排查**:
- **先看是不是正常回压**:`login_queue_admitted_inflight` 与该 zone 各 gate 上报的 `PlayerCount` 之和是否已到 capacity(看 `login_queue_capacity`:2026-11-10 起它是区服目录的 `zone_config.capacity` 再按在接客的 gate 到水位(90%)的承载封顶之后的值,gate 台数不够时小于目录的值,见 `login_queue_configured_capacity` 与告警 `LoginQueueCapacityLimitedByGates`)。到了就是正常回压(G4b-G 起取走后占位再留一段——2026-10-15 起 20s,此前 15s——之后名额转给在线数,见上方「放行节奏」),不往下查
- `redis-cli ZCARD queue:zone:1` → 应该单调下降。不降 = ZPOPMIN 没在跑
- 查 login 日志 `[loginqueue] elected dispatcher leader zone=<id>` 是否存在 + `[loginqueue] zone=1 admitted=N` 是否持续
- 无 leader 日志 = `dispatcher:lock:login_queue:zone:<id>` 抢锁失败,看 Redis 连接。多 zone 共用 Redis 时每个 zone 各有一把锁(2026-10-12 G1b 起),某个 zone 没有锁键 = 该 zone 没有打开 Queue 的 login 副本
- 有 leader 但无 admitted 日志,且在线 + 在途 < capacity(已按第一条排除正常回压)= `FreeSlots` 永远返回 0,查 `GateWatcher.FetchAllNodes()` 和区服目录的容量(`login_queue_configured_capacity`;为 0 说明读不到区服策略,看 `login_zone_policy_refresh_total` 与本区网关是否在发布)
- 有 leader、日志里持续出现 `[loginqueue] freeSlots zone=<id> queueLen=<n> err=no gates for zone` = 有人在排,但本 zone 一台可用 gate 都没有(G1b 之前这种情况是静默跳过的)
- 有 leader、日志里持续出现 `[loginqueue] queueLen zone=<id> err=...` = 读不出队列键(WRONGTYPE / ACL / 单键异常),锁照常续期但每拍都不出队(2026-10-14 之前这种情况零日志)
- 日志里有 `goroutine_panic` 且 `point=login.queue_dispatcher_term`(指标 `safego_panic_total{point="login.queue_dispatcher_term"}` 在涨)= 出队路径有代码缺陷。该任期已放锁,本副本退避 TTL/3 后再参选,同 zone 备机可以接手;每个副本都 panic 就是每轮当选即 panic,队列照样不出,按栈修代码

> (2026-10-15 补,已落码,未编译、未测试,待 Codex 验证)「队列在降、admit 不涨、robot 停在 rank=0」这一形状另查下面三条日志(见 `docs/handoff/launch-readiness-20261013/spec_G4b-G.md` §5.8 末尾补注):
> - `[loginqueue] PopAdmit dropped entries after zpopmin zone=<id> reason=write_failed popped=… dropped=… firstErr=…`(ERROR,每批一行)= 已弹出的条目写放行失败,这些玩家停在名次 0 直到 meta 过期(1h)。`firstErr` 含 `OOM` 即 `redis-queue` 写满(noeviction):弹出照常成功、写入全被拒,几拍之内整条队列被弹空,先扩内存或清堆积(见「故障应急」),再排查谁占了内存;其它错误按 Redis 连接 / 延迟排查。停服、丢租约已不会造成这条(弹出之后的写入有 3s 独立预算),滚动重启期间偶发一行可以接受,持续出现不行。
> - `[loginqueue] PopAdmit dropped entries after zpopmin zone=<id> reason=meta_gone dropped=<n>`(INFO)= 队首条目已在队里等满 `QueueEntryTTL`(1h),令牌已失效,这些人查询回 EXPIRED、会重新入队;不写放行槽、不占名额、不计入 `login_queue_admit_total`。持续出现说明排队等待超过 1h。
> - `[loginqueue] PopAdmit zone=<id> err=loginqueue: … ttl must be positive …`(ERROR,每拍一行)= `Queue.AdmitTTL` 或 `Queue.QueueEntryTTL` 被显式配成 0 或负数(配置的默认值只在键缺省时生效),dispatcher 不出队,条目原样留在队列;改配置后滚动重启。起服时的校验还没接上,`QueueEntryTTL` 非正时入队本身也会把整条队列删掉,不要配。
>
> (2026-10-15 再补,更正上面两处,同样未编译、未测试)
> - 第一条括号里的「3s 独立预算」自停服、丢租约时起算:没有取消时弹出之后的写入不设截止,一批写多久都照写;取消之后 3s 内仍没写完的尾部才会丢,记进这行 `reason=write_failed`(`firstErr` 通常是 `context deadline exceeded`)。
> - 第三条「起服时的校验还没接上」已过时:`Queue.Enabled=true` 时起服装配在构造排队之前校验,`AdmitTTL` / `QueueEntryTTL` 非正、或 `AdmitTTL` 不足 1ms,login 直接起不来(panic 信息含 `login queue: loginqueue: … ttl must be positive`)。上线环境看不到第三条的每拍 ERROR,看到的是起服失败;按同样的方法改配置。
>
> (2026-10-18 更正,上面「`ZCARD` 不降 = ZPOPMIN 没在跑」一条与两段补注;原文保留;已落码,未编译、未测试,待 Codex 验证。完整说明见设计稿 §5 决策树后的 2026-10-18 更正)
> - **「不降 = ZPOPMIN 没在跑」改读「不降 = 放行脚本没在跑,或在报错」。** 出队已是 `ZRANGE` + 每条一段 `admitEntryScript`,条目在放行写成之前一直在队列里。不降时先查下面的 ERROR 行。
> - **两行 `PopAdmit dropped entries after zpopmin …` 已不存在**(`reason=write_failed` 的 ERROR、`reason=meta_gone` 的 INFO)。换成下面两行:
>   - **ERROR 行**:`[loginqueue] PopAdmit zone=<id> err=…`。dispatcher 每拍至多一行,停服、丢租约时不打。err 里是以下之一:`admit script queueId=… admitted=… skipped=… untried=…: <原因>`、`zrange: …`、`skipped <n> entries without an admit slot, first queueId=…: …`。本拍已放行的人照样计入 `login_queue_admit_total`。按原因判读:
>     - 含 `OOM`:`redis-queue` 写满(noeviction)。放行停止(fail-closed),**条目留在队列、名次不变、不丢**;排队中的查询与续期照常;新入队回 `StatusError`。先扩内存或清堆积(见「故障应急」),再排查谁占了内存。
>     - 含 `WRONGTYPE`:占位键或队列键被写成了别的类型(人工误操作)。之后各拍通常改由 `freeSlots …` 或 `queueLen …` 的 ERROR 报出;修好键之后自动恢复。若是占位键,出错那一条已写了放行槽却没有占位,至多多放 1 人。
>     - 含 `NOREPLICAS`(2026-11-01 补,只在排队实例是高可用形态时出现):主上没有一个从在线且复制延迟不超过 10 秒(`min-replicas-to-write 1`),所有写命令被拒。放行停止(fail-closed),条目留在队列、名次不变;新入队和排队中的查询也报错(查询先 `GETDEL` 放行槽),客户端按 queue-status 失败的路径退避。查两个从(`kubectl -n <infra> get pod -l app=redis-queue-g<N>`,告警 `RedisReplicaLinkDown`),从接上后自动恢复。不要为此关掉 `min-replicas-to-write`:那会让切主时丢掉已答复成功的写入。
>     - 其它错误:按 Redis 连接 / 延迟排查。
>   - **INFO 行**:`[loginqueue] PopAdmit entries not admitted zone=<id> metaGone=<n> alreadyAdmitted=<n> gone=<n>`。每批至多一行。
>     - `metaGone`:队首条目的 meta 已过期或类型损坏,已就地清掉。meta 现在是空闲 TTL,所以持续出现说明有客户端停止轮询超过 `QueueEntryTTL`(关了客户端,或断网后不带 token 重新入队留下的孤儿),不再表示「排队等待超过 1h」。
>     - `alreadyAdmitted`:清理上一次 `WRONGTYPE` 留下的部分写入。
>     - `gone`:别的 leader 或副本已放行同一条。同一 zone 持续出现时,按模式 3 查双 leader。
> - **「队列在降、admit 不涨、robot 停在 rank=0」这一形状在本版本不再出现。** 不在队列又没放行的条目,查询回 EXPIRED(`queue_gone`,410),robot 丢掉 token 重新排。
> - **第三条的起服 panic 范围变大。** 现在还包括 `QueueEntryTTL` 小于 1s 或大于 24h(`… queue entry ttl must be positive, at least 1s and at most 24h0m0s, got …`)。

### 模式 2:over-issue(超发 gate token)
```
[stats 30s] ... q_admitted=2000  (期望 920)
gate 日志:VerifyGateToken 突然出现大量 token 过期 / 玩家被踢
```
**根因**:在途占位没有覆盖「票已交出、gate 还没上报」这一段:这个人既不在在途里,也还不在 gate 的在线数里,下一拍又放一批。G4b-G 之前取走即撤占位,就是这一段(设计稿 §5「在途占位的生命周期」Q2)。
**判读**:G4b-G 起在途占位是 `admitted:zset:zone:{id}` ZSET,成员分数是到期 Unix 毫秒。排队领票后仍保留 `handoffGrace`(当前 20s,2026-10-15 起;此前 15s),覆盖客户端连 gate 到在线人数上报的窗口;不再在领票时 SREM。旧键 `admitted:zone:{id}` 不再读写,不能用它判断当前容量。
**排查**:
- 看成员:`redis-cli ZRANGE admitted:zset:zone:1 0 -1 WITHSCORES`。分数是占位到期的 Unix 毫秒,`fast:` 开头的是快速通道占位,其余是排队放行的 queueId。
- 只读统计未到期成员:先用 `redis-cli TIME` 取 Redis 当前 Unix 毫秒,再 `redis-cli ZCOUNT admitted:zset:zone:1 '(<当前毫秒>' +inf`。裸 `ZCARD` 可能包含尚未被下一次容量查询清掉的过期成员。
- 对照 `login_queue_admitted_inflight`、gate 在线数与 `login_queue_capacity`;在途数还包含 fast-path 的 `fast:<uuid>` 成员,没有固定的 `<50` 正常阈值:稳态约等于交票速率 × 20s(加上等轮询的放行数;2026-10-15 之前是 × 15s)。长时间顶在 `capacity − online` 而 gate 人数不涨 = 客户端拿了放行没去连 gate。
- 超发时检查占位键是否被删 / 淘汰、过早到期、login 与 Redis 的时钟偏差以及旧二进制是否仍在运行。占位偏高通常会压低放行量,不能直接归因为超发。

**反方向(少放:在途偏高、放不出人)**:
- `ZRANGE … WITHSCORES` 里有分数比当前毫秒时间戳大出 `AdmitTTL`(默认 60s)以上的成员 = 某个 login 副本的钟快过。合法分数最多是写入时刻 + `AdmitTTL`;钟快的副本写下的占位,在别的副本看来要到「真实时间 + 偏差」才到期,它恢复后这些占位还要留满偏差量才到期。只要该 zone 还有写入,整键 TTL 一直被续上、不兜底(`docs/handoff/launch-readiness-20261013/spec_G4b-G.md` §4.2)。同时查各 login 节点的时钟同步。
- 止血:`redis-cli DEL admitted:zset:zone:<z>`,代价是在途清零,下一拍最多多放一批 `capacity − online`。能停准入时,按文末「清空堆积」末段的做法先停该 zone 的新准入再删。

### 模式 3:dispatcher 双 leader(同一 zone 的双副本同时出队)
```
同一 zone 的两台 login 都打印 [loginqueue] elected dispatcher leader zone=<同一个 id>
admitted 短时间内突增超过 freeSlots 上限
```
G1b(2026-10-12)之后每个 zone 各有一个 leader,**不同 zone 各打印一次是正常的**,只有同一个 zone id 出现两次才是双 leader。

**排查**:
- `redis-cli GET dispatcher:lock:login_queue:zone:1` 应该只有一个 owner UUID。新旧版本混跑窗口里 zone 键和全服旧键 `dispatcher:lock:login_queue` 可能同时存在,属于预期(设计稿 §7.1)
- 同一 zone 的副本配了不同的 `DispatcherLockKey` 也会长期双 leader:`redis-cli --scan --pattern "*:zone:1"` 能看到两把锁
- 看 heartbeat 日志:lost leadership 后是不是没有真停掉 drainLoop

### 模式 4:queue token 过期风暴
```
[stats 60s] ... q_expired=400 (期望 < 50)
```
**根因**:`AdmitTTL=60s` 但客户端轮询间隔 + 网络抖动 > 60s
**排查**:
- 客户端日志看实际轮询间隔
- 调大 `AdmitTTL` 到 120s,看 q_expired 是否回落
- 不调 AdmitTTL,看是不是网络层 RTT 异常

**调 `AdmitTTL`(2026-10-15 改写)**:排队开着时可以直接滚动调整 `AdmitTTL`,不必先关排队。在途占位键 `admitted:zset:zone:<z>` 的整键 TTL 只延长不缩短(各副本写占位时,剩余 TTL 不足自己的 `max(AdmitTTL, 20s) + 1min` 才续),新旧值并存时小值副本不会把整键 TTL 压到大值副本写的占位之前。调小之后,已续上的较大整键 TTL 要自然走完才收敛(`redis-cli PTTL admitted:zset:zone:<z>` 一段时间内仍大于新值 + 1min 属正常),只影响 zone 下线后键多留多久。
- 10-15 上午这里写过「新旧值之差不要超过 1min,更大的先 `-LoginQueue off` 再滚」,那是整键 TTL 还在无条件续期时的约束,已作废。
- 例外:10-15 之前的 G4b-G 镜像(无条件续期)还在跑、又要在排队开着时改 `AdmitTTL` 时,旧约束仍对那部分副本成立;先按「故障应急」的升级顺序把所有副本升到新版本(排队关着滚)。依据见 `docs/handoff/launch-readiness-20261013/spec_G4b-G.md` §4.1、§5.5。

### 模式 5:重连玩家被排队(豁免失效)
```
[stats 30s] ... q_entered=4500 (期望 4080)
玩家手感:断网重连也要排队 → 投诉
```
**排查**:
- AssignGate 进来的请求带 `account` 字段了吗?Java Gateway 透传了吗?
- `player_locator.GetSession()` 返回 GateID 非空才豁免;手动查一个重连账号的 PlayerSession.State

### 模式 6:Redis QPS 打爆
```
Redis 监控:queries/sec > 50k,p99 命令延迟 > 50ms
```
**检查对象**:每个 zone 的 dispatcher tick、客户端轮询,以及 fast-path 预占。当前容量查询经 Lua 执行 `ZREMRANGEBYSCORE + ZCARD`,占位写入是一段 Lua `ZADD + PTTL`(剩余不足才 `PEXPIRE`,2026-10-15 起;放行时与 `SET` 放行槽同在一个 MULTI 里,以 `EVAL` 发出),不再是旧 SET 的 SCARD / SADD。dispatcher 每个非空拍只读一次 gate 目录(etcd),不再每放一个人读一次。不同路径的 Redis 命令数不同,用实测 QPS、延迟和调用占比定位,不沿用旧版固定命令数估算。
**排查**:redis MONITOR 抽样 100 个命令,看哪类命令占比最大,反推哪层在打

> (2026-10-18 补注,本模式;已落码,未编译、未测试,待 Codex 验证)
> - **查询。** 排队中一次查询现在是 `GET` + `GETDEL` + 一次 `EVALSHA`(读状态脚本 `lookupStateScript`),此前是 `GET` + `GETDEL` + `HMGET` + `ZRANK` + `ZCARD`。查询从纯读变为偶尔带写:条目剩余 TTL 不足 `QueueEntryTTL` 时,把 meta、token 索引、队列键续到 1.25 倍。
> - **续期写量(估算,未压测)。** 每条约每 `QueueEntryTTL`/4 续一次。4.5 万人排队、2s 轮询、`QueueEntryTTL`=1h 时,稳态约 100 条 `PEXPIRE`/s;入队洪峰期间另加约 2 × 入队速率。
> - **出队。** 出队是一次 `ZRANGE` 加每条一次 `EVALSHA`(放行脚本 `admitEntryScript`)。
> - **判读。** 抽样里 `EVALSHA` 占比高属正常。每次 `EVALSHA` 的 Redis CPU 未测,R16 复测时要看。

### 模式 7:gate ephemeral port 耗尽回归
```
gate 机器 sysctl net.ipv4.ip_local_port_range 内 TIME_WAIT 占满
```
**这是 CLAUDE.md §7 提的老问题,排队层不应该引入新风险**,但要确认:
- queue-aware robot 是否在 admit 后立刻 close → 重连?(我的实现:admit 后建 TCP 连接,close 在 robot 完整 session 结束才发生,所以这条不应该被排队恶化)
- 看 `ss -s` 的 TIME_WAIT 计数,对比开排队前后

---

## 验证清单(每次改 loginqueue 都跑一遍)

> **打开排队前的硬门槛(2026-10-15 负责人定)**:G4b-G 起的在途占位脚本(含 10-15 的条件续期)只有 miniredis 上的证据。G4b-2 打开排队之前,下面第 2、3 步必须在真实 Redis 7.x 上跑通一次,同时按 `docs/handoff/launch-readiness-20261013/spec_G4b-G.md` §8.4 核对 `admitted:zset:zone:1`(类型、分数、约 20s 消失、`PTTL` 在 (0, 120000] 内且不是 −1、手工调大的整键 TTL 不被后续写入压回),结果记进 `docs/PROGRESS.md`。没有这份记录,任何环境都不打开排队。
> (2026-10-15 补)部署侧的机械拦截是 `tools/scripts/k8s_deploy.ps1` 的 `$script:LoginQueueMergeGateCleared`:三项门槛(见 `spec_G4b.md` §7「合入门槛」)都满足、PROGRESS 有记录之后,由负责人把它改为 `$true`,K8s 上才会按档位打开排队。
> (2026-10-18 补)放行脚本与读状态脚本都已改写(设计稿 §5 决策树后的 2026-10-18 更正)。真实 Redis 冒烟因此扩项,同样记进 `docs/PROGRESS.md`,同样是打开排队前的硬门槛;新增的项见 `spec_G4b-G.md` §8.4 的 2026-10-18 补注:
> - 写满时条目留在队列;
> - 续期区间;
> - 入队的 NX/GT;
> - 毒丸 meta;
> - RESP3 下的各查询分支。

### 1. 单元 + 集成测试都过
```bash
cd go/login && go test ./internal/logic/pkg/loginqueue/... -count=1
# 期望:全部 PASS,耗时数秒(主备切换类用例要等 1s 的备机重试周期)。
#       个数不写死:2026-10-14 为 59 个顶层用例,以后增删用例以 -v 实际列出的为准。
#       G1b 的验收命令与通过标准见 docs/handoff/launch-readiness-20261013/spec_G1b.md §7.2
```

### 2. 单 robot 冒烟(zone 不饱和)
```bash
# RobotCount=5, zone_config 里 zone 1 的 capacity = 100(改法见上文「区容量不在 login.yaml 里」)
# 期望:5 个 robot 全部 fast-path 直接进,q_entered=0
```

### 3. 多 robot 冒烟(zone 饱和)
```bash
# RobotCount=200, zone_config 里 zone 1 的 capacity = 100
# 期望:前 100 fast-path,后 100 入队(q_entered=100)。q_admitted 的涨幅 ≤ 容量 − 在线:
#       robot 保持在线时不涨属正常(先核对在线 + 在途是否已到 capacity),robot 下线后每腾出一个名额放行一个。
#       G4b-G(2026-10-13)起不再写死「100s 内涨到 100」,见上方「放行节奏」
```

### 4. dispatcher 杀掉再起(故障演练)
```bash
# 跑 5000 robot,排队中途 kill -9 该 zone 当前的 leader login pod
# 期望:约 20~40s 内同 zone 的备份接管(锁剩余 TTL 在 20~30s 之间,心跳每 TTL/3=10s 续到 30s;
#       再加备机重试周期 TTL/3=10s),q_admitted 短暂停顿后继续上涨,q_avg_wait 略升但无饿死;
#       只有这个 zone 的放行暂停(≤ TTL + TTL/3 ≈ 40s),其它 zone 不受影响
```

### 5. Redis 重启(灾难演练)
```bash
# 跑 5000 robot,中途 redis-cli SHUTDOWN NOSAVE
# 期望:
#   - dispatcher 失 leader → 自动退出
#   - 客户端 QueryQueueStatus 报错 → robot fall through 到 outer retry
#   - Redis 起来后,所有数据丢失(预期),robot 触发新一轮 AssignGate 重建队列
#   (2026-10-16 更正,上面第 2 条原文保留)按 2026-10-15 客户端契约,queue-status 报 500 / 网络错误时
#   robot 先带 token 重调一次 assign-gate 作恢复;重入也失败才返回错误、走外层重试。Redis 宕机期间
#   重入多半回 500 或 410,所以仍会落到外层重试或丢 token 重排;queue-status 连续失败时轮询间隔按
#   2s 起翻倍退避、封顶 30s(open-server-rate-limit-design.md §11.5 的 2026-10-15 补注)。
```

---

## Grafana / Prometheus 指标(已接入)

login 进程通过 go-zero 内置 `/metrics` endpoint(`login.yaml` 第 119 行 `Prometheus` 配置块)暴露下列 8 个指标(后两个 2026-11-10 起):

| 指标 | 类型 | 标签 | 用途 |
|---|---|---|---|
| `login_queue_size` | Gauge | zone | ZCARD 实时长度,dispatcher 每 tick 刷 |
| `login_queue_admitted_inflight` | Gauge | zone | 在途占位数:容量查询清除过期成员后的 admitted ZSET 长度(ZCARD),即已放行未取 + 交票后 20s 之内(2026-10-15 起;此前 15s),含排队与 fast-path 两种占位。口径 2026-10-13(G4b-G)起变宽,前后曲线不可比;10-15 宽限加长后,稳态里「交票后」那部分约变为原来的 4/3 |
| `login_queue_wait_seconds` | Histogram | zone | 入队到 admit 的端到端等待(buckets: 0.5/1/2/5/10/20/30/60/120/300/600s)|
| `login_queue_admit_total` | Counter | zone | dispatcher 累计 admit 数,`rate(...[1m])` = drain QPS |
| `login_queue_expired_total` | Counter | zone, reason | reason ∈ `bad_signature` / `ttl` / `meta_gone` / `queue_disabled` / `admit_window_passed`(2026-10-15 起:放行槽已被取走或已按 AdmitTTL 过期后再查,回 EXPIRED;此前这类查询回名次 0、最长卡 1h。稳态里它主要来自客户端取票后的重复查询,偏高说明放行后没人来取) |
| `login_queue_dispatcher_is_leader` | Gauge | pod, zone | 1=leader,0=standby;`sum by (zone) (...) == 1` 是稳态不变量(每 zone 一个 leader)。leader 每拍重发 1、备机每轮抢锁失败重发 0(2026-10-14 起),指标开关打开前写丢的样本下一拍就补上。**序列缺失不等于 0**:整 zone 没有副本上报时 `sum by (zone)` 根本不出这一组,发现不了整 zone 缺席,见下方 `absent` 告警 |
| `login_queue_capacity` | Gauge | zone | `FreeSlots` 实际用的区容量:区服目录的 `zone_config.capacity`(读不到区服策略时按软上限)再按在接客的 gate 到自报 `max_connections` 90% 的承载封顶。每次算容量时写(快速通道每次 AssignGate、dispatcher 每个非空拍),排队关着时没有序列 |
| `login_queue_configured_capacity` | Gauge | zone | 区服目录给本 zone 的容量,0 = 读不到区服策略。大于 `login_queue_capacity` = gate 台数装不下目录的容量,排队比配置的早开始拦人,持续 10 分钟报 `LoginQueueCapacityLimitedByGates`(`deploy/k8s/login-alerts.yaml`) |

> (2026-10-18 补注,`login_queue_expired_total` 与 `login_queue_admit_total` 两行;已落码,未编译、未测试,待 Codex 验证)
> - **新增 reason `queue_gone`。** 含义:meta 在、没有放行、又不在队列。来源有三个:整个 zone 空闲超过 `QueueEntryTTL` 后队列键先到期;人工 `ZREM` / `DEL` 队列键;排队开着时混跑新旧版。
>   - 持续非零,说明有人在排队开着时混跑新旧版,或手工动了队列键。
>   - 执行「清空堆积」第一行之后出现一个尖峰,属预期。
>   - 与其它原因一样不预置标签,面板用 `or vector(0)`。
> - **`ttl` 变窄。** 只表示排队令牌超过 24h 的绝对过期。此前令牌与条目同为 1h,等满 1h 的人记 `ttl`。
> - **`meta_gone` 变宽。** 包含空闲超过 `QueueEntryTTL` 的条目(token 索引或 meta 已按空闲 TTL 过期),也包含 meta 类型损坏、meta 与令牌的 zone 不符。zone 标签一律取令牌里的 zone,此前 token 索引不在时记 `unknown`。
> - **`login_queue_admit_total`。** `PopAdmit` 中途出错时,已放行的也计入;go-redis 重发、第一次已写成的放行也计入。
> - 上表 reason 列表相应补上 `queue_gone`。

### 推荐告警(PromQL)

```promql
# 某个 zone 双 leader,或有副本在上报但没有 leader(Redis flap / 同 zone 锁键配得不一样)。
# 建规则时 for: 1m,盖过 leader 交接窗口:kill -9 时锁剩余 TTL ≤ 30s + 备机重试周期 10s ≈ 40s,
# 优雅停机最长 10s;不带 for 的话每次滚动重启 / 切主都会响。
# 「该 zone 全副本挂」这条发现不了:没有序列时 sum by (zone) 不出这一组,!= 1 永远不命中。
sum by (zone) (login_queue_dispatcher_is_leader) != 1

# 某个开了排队的 zone 一条序列都没有(全副本挂 / 没开排队 / 没抓到),只有这条能报。for: 2m。
# absent 不能按标签自动分组,必须按开了排队的 zone 清单逐个写(下面是 zone 1)。
# 也可以与上一条合成「sum by (zone) (...) > 1」加「absent(login_queue_dispatcher_is_leader{zone="<id>"} == 1)」
# (后者同时覆盖全 0 与无序列),两种写法二选一。都依赖 leader / 备机按拍重发,否则冷启动后会一直报。
absent(login_queue_dispatcher_is_leader{zone="1"})

# 队列在涨但 admit 不涨(dispatcher 卡死)。
# zone 满员(在线 + 在途 ≥ capacity)时这条同样成立,属正常回压(G4b-G 起取走后占位再留 20s(10-15 前 15s),之后名额转给在线数);
# 判卡死要再加「在途 + 在线 < capacity」。上面 6 个排队指标里没有在线数与 capacity,这个条件先人工核对
# gate PlayerCount 与 login_queue_capacity。
deriv(login_queue_size[5m]) > 0 and rate(login_queue_admit_total[5m]) == 0

# p99 等待超过 5 分钟
histogram_quantile(0.99, rate(login_queue_wait_seconds_bucket[5m])) > 300

# 签名失败激增(可能有人在压测/攻击)
rate(login_queue_expired_total{reason="bad_signature"}[5m]) > 10
```

### Grafana 看板布局建议

- **Top row** — `login_queue_size` 按 zone 折线 + `login_queue_dispatcher_is_leader` 按 zone、pod 的 stat panel
- **Mid row** — `rate(login_queue_admit_total[1m])` 按 zone + `login_queue_admitted_inflight` 按 zone
- **Bottom row** — `histogram_quantile(p50/p95/p99, login_queue_wait_seconds_bucket)` 按 zone + `rate(login_queue_expired_total[5m])` 按 reason 堆叠

> robot 的 `[stats] q_*` 日志仍然有用——它是**单 robot 视角**,Prometheus 是**服务端视角**。两者互补:robot 看"我等了多久",Prometheus 看"系统整体在做什么"。

---

## 已知不足(本期不修)

1. **排队 token 不能转移**:玩家换设备重登会拿到新 queueToken,丢掉位置。生产可接受,因为玩家通常不会排队中换设备。
2. ~~**多 zone 公平性**:dispatcher 按 `activeZones` 顺序遍历,理论上前面的 zone 抢先用完 admit 批次。~~ 2026-10-12 G1b 起每 zone 一个 leader,不存在 zone 之间抢批次(设计稿 §7.1、§14 第 5 条)。
3. **客户端没有显示 ETA**:只有 `queue_rank` + `queue_total`,前端可以用 `rank / (capacity/sec)` 自己算 ETA。

---

## 故障应急

**立即关闭排队**:`login.yaml` 改 `Queue.Enabled: false`,重启 login pod(或者 hot-reload 如果 go-zero config watcher 已接好)。**所有玩家立刻走 fast path,等同于排队没存在过。**

**升级顺序(G4b-G,2026-10-13)**:打开 `Queue.Enabled` 之前,所有 zone 的 login 先在排队关闭下升到含 G4b-G(含 G1b)的版本。排队开着混跑时,新旧两种在途占位键互不可见,旧版副本放行 / 取走的部分仍按旧版超发;不丢人、不重复放行。这与 `go/login/etc/login.yaml` 上线清单第 0 步是同一条要求,逐格推导见设计稿 §5「在途占位的生命周期」。

> (2026-10-18 更正,上段「不丢人、不重复放行」;原文保留)
> - **升级与回滚都要排队关着滚。** 升级到 2026-10-18 这一版(放行脚本先判在队、空闲 TTL),以及从它回滚,都必须在 `Queue.Enabled=false` 下滚完。
> - **为什么。** 排队开着混跑时,旧版 leader 已 `ZPOPMIN`、尚未写完放行的条目,会被新版判成 `queue_gone`(410)、打回队尾,上段的「不丢人」在这一版的升级窗口里不成立。
> - **怎么拦。** K8s 由 `tools/scripts/k8s_deploy.ps1` 的 `$script:LoginQueueMergeGateCleared` 拦截;手工打开过排队的环境,按 `login.yaml` 上线清单第 0 步的 10-18 补注先关排队再滚。

**清空堆积**(与设计稿 §15 一致):

> 不要用 `KEYS`:它会阻塞全服共用的那个 Redis 主。多 zone 共用 Redis 时,下面后三行(`admit:*` / `queue:meta:*` / `queue:token:*`)不分 zone,会清掉**全部 zone** 的放行槽和排队条目;只想清一个 zone 时只执行第一行,其余键按 TTL(60s / 1h)自然过期。连哪个实例见「异常模式速查」开头。

```
redis-cli DEL queue:zone:1 admitted:zset:zone:1
redis-cli --scan --pattern 'admit:*'        | xargs -r redis-cli DEL
redis-cli --scan --pattern 'queue:meta:*'   | xargs -r redis-cli DEL
redis-cli --scan --pattern 'queue:token:*'  | xargs -r redis-cli DEL
```
玩家会自己重试 `/api/assign-gate` 重新入队(如果排队还开着)或直接进游戏(如果 Enabled=false)。

删除占位会让尚未连上的有效 gate 票据暂时不计入容量,下一拍可能再次放行。应先停止该 zone 的新准入并等待有效票据窗口结束,再执行清理并恢复准入;不能在正常放行中把删键当成日常降队列手段。新旧二进制混跑时应先完成版本收口,不要靠同时删新旧键修复容量语义。旧键 `admitted:zone:*` 是 2026-10-13(G4b-G)之前版本的残留,最后一个旧版本停写后 60s 内自行过期,不用删。

> (2026-10-18 补注,本节;已落码,未编译、未测试,待 Codex 验证)
> - **自然过期的时长。** `queue:zone:*`、`queue:meta:*`、`queue:token:*` 现在是空闲 TTL,不清时在最后一次入队或排队中查询之后至多约 1.25×`QueueEntryTTL` 过期。上面提示里「60s / 1h」的 1h 改读「至多约 1.25h」。
> - **`DEL queue:zone:<z>` 之后的客户端。** 仍在轮询的客户端下一次查询立即收到 EXPIRED(`queue_gone`,410),丢掉 token 重新走 `/api/assign-gate`,此前是停在名次 0 等 meta 过期。`login_queue_expired_total{reason="queue_gone"}` 随之出现尖峰,属预期。
> - **只删队列键、不清 meta 时。** 不在队列的条目不再续期,meta 与 token 索引按剩余 TTL 自然过期。
