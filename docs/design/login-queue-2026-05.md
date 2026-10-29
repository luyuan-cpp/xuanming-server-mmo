# Login Queue (AssignGate Real Queue) — 2026-05

> **决策**: ARCH.md §11 #14
> **代码**: `go/login/internal/logic/pkg/loginqueue/`, `java/gateway_node/.../service/AssignGateService.java`
> **运维**: [docs/ops/login-queue-stress-runbook.md](../ops/login-queue-stress-runbook.md)
> **关联**: 2026-05-08 [open-server-rate-limit-design.md](./open-server-rate-limit-design.md)(Bucket4j 限流);本文是其**互补层**,不是替代
> **修订**: 2026-10-12 G1b — 出队 leader 锁改为每 zone 一把(§7.1)
> **修订**: 2026-10-13 G4b-G — 在途占位改为逐个到期的 ZSET,交票后保留 15s(§5「在途占位的生命周期」)
> **修订**: 2026-10-15 — 负责人按「最标准」拍板:交票后宽限 15s → 20s;在途占位键的整键 TTL 改为只延长不缩短;真实 Redis 首跑定为打开排队前的硬门槛(§5「在途占位的生命周期」、§14 第 8 条)。出队每个非空拍只取一次 gate 候选、批内按本拍已分人数轮转(§5 Dispatcher Loop)。均已落码,**未编译、未测试**,待 Codex 验证
> **修订**: 2026-10-18 — 出队改为「`ZRANGE` + 每条一段先判在队的放行脚本」,条目在放行写成之前不离开队列(写满、出错、取消、强杀都不丢人);`Lookup` 没取到放行槽之后的读状态合成一段脚本并续空闲 TTL,`zone` 取自令牌;`QueueEntryTTL` 改为空闲 TTL,排队令牌固定 24h;新增过期原因 `queue_gone`;本版本与从它回滚都必须在 `Queue.Enabled=false` 下滚完(§5 决策树后的 2026-10-18 更正,其余各处的 10-18 注指向它)。`loginqueue` 包已落码,**未编译、未测试**,待 Codex 验证

---

## 1. 为什么再做一层"真排队"

Java Gateway 已实现共享 zone／IP 桶与进程内账号冷却，波次默认关闭，当前契约见[削峰设计](./open-server-rate-limit-design.md)。它限制入口速率，但不提供真实排位；本队列负责有序容量准入，是否可启用仍受合入门禁约束。

实战中两个场景是 Bucket4j 解不了的:

1. **开服当日**:5 万玩家在 09:00:00 同时点"进入服务器",Bucket4j 把请求按 token 桶节流到 2000/s,但玩家拿到的回应是"请重试"——他们不知道何时能进、是不是被踢、要不要换服。截图(用户提供的排队 UI)需要展示 `rank=N / total=M` 这种**有序位次**。
2. **配置容量回压**:DBA 判定本服 gate 集群当下只能稳吃 1 万人,但 Bucket4j 不知道"在线玩家数"——它只看请求 RPS。需要一个组件能把 `online + admitted_inflight` 与 `zoneCapacity` 比对,**动态决定**当前请求是放行还是入队。

Bucket4j 是**无序的速率匙**(token bucket),真排队是**有序的容量阀**(FIFO + 容量计算)。两者必须互补,不是二选一。

---

## 2. 决策记录(为什么放在 go-zero login,不是 Java Gateway)

| 维度 | 放 go-zero login(选定) | 放 Java Gateway(否决) |
|---|---|---|
| 出队需要的状态 | EnterGame 进度、PlayerSession、enter_gs_type 全在本进程 | 要新增 4 个 gRPC 才能查 |
| Leader 选举 | 复用 `pkg/locker.RedisLocker` + `Lock.StartHeartbeat` | 要新引选举库 |
| 重连/顶号豁免 | 直接调 `sessionmanager.GetSession`(同进程) | 要再实现一份判定 |
| 跟现有 Bucket4j 关系 | Bucket4j 在前(过滤洪峰),Queue 在后(保位次) | 两套语义同居,容易污染 |
| 未来扩"进场景排队" | 同一套 Redis ZSET 模板复用 | 要在 Go 侧再写一遍 |

详细对比见对话记录 / [plan 文件](#)。本期**只做选服登录排队**,进场景排队等真有"攻城战 / 世界 Boss / 跨服竞技场"玩法时按同一套模板升级,不重复造轮子。

---

## 3. 总体拓扑

```
                   ┌─────────────────────── Java Gateway ──────────────────────┐
                   │                                                            │
Client ── POST ───►│ /api/assign-gate                                           │
                   │   ├─ ① Bucket4j 限流(zone/IP/account)──── 挡洪峰        │
                   │   │      └ 拿不到令牌 → code=100 queueSource="ratelimit"  │
                   │   └─ ② gRPC LoginPreGate.AssignGate ─────────────────────┐│
                   │                                                          ││
                   │ /api/queue-status                                        ││
                   │   └─ gRPC LoginPreGate.QueryQueueStatus ─────────────────┤│
                   └──────────────────────────────────────────────────────────┘│
                                                                                │
                   ┌─────────────────────── go-zero login ────────────────────┐│
                   │                                                          ││
                   │ AssignGate handler:                                      ◄┘
                   │   1. queue_token 非空 → 验签 + Lookup(Redis)            │
                   │   2. account 非空 + 有 PlayerSession → 重连豁免         │
                   │   3. Queue.Enabled=false → 直接签 gate token             │
                   │   4. FreeSlots>0 且队列空 → 直接签 gate token            │
                   │   5. 其它 → Enqueue + 返回 queue_token+rank              │
                   │                                                          │
                   │ QueryQueueStatus handler:                                │
                   │   验签 + Lookup → ADMITTED 或 QUEUEING                   │
                   │                                                          │
                   │ Dispatcher goroutine(每 zone 一个 leader,1s 一拍):       │
                   │   只出本 zone(Node.ZoneId):                              │
                   │     free = capacity - online - admitted_inflight         │
                   │     ZPOPMIN free 个 → 签 gate token → SET admit:{queueId}│
                   └──────────────────────────────────────────────────────────┘

                   Redis (共享):
                   ─ queue:zone:{zoneId}      ZSET, score=入队 ms, member=queueId
                   ─ admit:{queueId}          STRING JSON{ip,port,payload,sig,deadline}, TTL=60s
                   ─ admitted:zset:zone:{zoneId}  ZSET member=queueId | fast:<uuid>, score=占位到期毫秒(逐个到期;整键 TTL 只做垃圾回收)
                   ─ queue:meta:{queueId}     HASH {account,device,zoneId,createdAt}, TTL=1h
                   ─ queue:token:{token}      STRING queueId, TTL=1h(token→queueId 反查)
                   ─ dispatcher:lock:login_queue:zone:{zoneId}  STRING uuid, TTL=30s(本 zone leader 锁)
```

> **(2026-10-15 更正,上图原文保留)** 上图键表里 `admit:{queueId}` 一行的值格式早已过时。R17 R1 修复(2026-06-04)起,放行槽只存 `AdmitSlot`:JSON `{"node_id","ip","port","zone_id"}`,即 dispatcher 挑中的 gate 端点及其 zone,**不含** `token_payload`、签名与 deadline(`go/login/internal/logic/pkg/loginqueue/queue.go:77-98`)。放行时写入:`SET admit:{queueId} <AdmitSlot JSON>`,TTL 仍为 `AdmitTTL`(默认 60s),与在途占位的 `EVAL admittedHoldScript` 排进同一个 MULTI(`queue.go:465-473`)。gate 票在客户端取走时才签:`Lookup` → `consumeAdmitAndSign` 先 `GETDEL admit:{queueId}`,再用调用方传入的 `SignAdmitFn` 现签(`queue.go:331-361`;签票实现是 `gatetoken.go` 的 `SignGateToken`),所以 gate 票的有效期从取走那一刻起算。同图 Dispatcher 框里「签 gate token → SET admit:{queueId}」也按此读:出队只挑 gate、不签票(§5 Dispatcher Loop 已是现行写法)。另:`queue:meta:{queueId}` 的字段名是 `zone_id`,不是 `zoneId`(`queue.go:207-212`)。

> **(2026-10-15 更正,上图 AssignGate 第 4 步;原文保留)** 第 4 步「FreeSlots>0 且队列空 → 直接签 gate token」漏了占位:签票之前先调 `TryReserveFastPathSlot`(`go/login/internal/logic/loginpregate/assigngatelogic.go:110-129`;脚本与函数见 `go/login/internal/logic/pkg/loginqueue/queue.go:563-593`),一段 Lua 先清掉 `admitted:zset:zone:{z}` 里已到期的成员,未到期成员数 < `budget`(= `capacity − online`,注意不是 `free`)才写入成员 `fast:<uuid>`、分数 = 现在 + 交票宽限 20s(`defaultHandoffGrace`),并按「只增不减」续整键 TTL;返回 1 才签票。占不到(名额被并发请求占满)或脚本出错(Redis 抖动,fail-closed)都改走第 5 步入队,不签票。这个占位不撤,20s 后按分数自然到期,期间与排队路径的在途占位一起计入 `FreeSlots` 的 `admitted_inflight`,所以快速通道同样受容量约束,不是旁路。第 3 步(`Queue.Enabled=false`)与第 2 步(重连豁免)才是真正「直接签票」,不读不写任何排队键。占位的生命周期见 §5「在途占位的生命周期」表的「快速通道占位」一行。

> **(2026-10-18 更正,上图 Dispatcher 框、AssignGate 第 1 步与键表;原文保留;已落码,未编译、未测试,待 Codex 验证)**
> - Dispatcher 框「ZPOPMIN free 个 → 签 gate token → SET admit:{queueId}」改读:`ZRANGE` 读出前 `batch` 个候选(只读,条目仍在队列)→ 逐条挑 gate → 逐条 `EVALSHA admitEntryScript`。脚本先判成员仍在队列,再在同一段里写 meta 的 `admitted_at`、放行槽与在途占位,最后 `ZREM` 成员。gate 票仍在取走时才签。
> - AssignGate 第 1 步与 QueryQueueStatus 的「验签 + Lookup」:`Lookup` 用验签得到的令牌里的 zone 定位队列键,并与 meta 的 `zone_id` 对账。
> - 键表 `queue:meta:{queueId}` 与 `queue:token:{token}` 的「TTL=1h」改读**空闲 TTL**:入队时设 `QueueEntryTTL`;之后每次排队中的查询,剩余 TTL 不足 `QueueEntryTTL` 时续到 1.25 倍。所以条目空闲(自最后一次成功的排队中查询或入队起算)至少 `QueueEntryTTL`、至多 1.25 倍才过期;放行之后不再续期。
> - `queue:zone:{zoneId}` 同样会被续期;入队对它改为 `EXPIRE NX` + `EXPIRE GT`,TTL 只增不减。
> - `queue:meta` 另有字段 `admitted_at`(10-15 起,放行时刻 unix 毫秒)。
> - 排队令牌的签名过期固定为 24h(`loginqueue.queueTokenMaxAge`),不再等于 `QueueEntryTTL`(§6)。
> - 推导与残余风险见 §5 决策树后的 2026-10-18 更正。

---

## 4. 协议(proto/login/login.proto)

新增字段全部加在末尾(7-11 / 1-10),保持 wire-format 向前兼容:

```protobuf
message AssignGateRequest {
  uint32 zone_id     = 1;
  string queue_token = 2;   // 已在排队的客户端用此重入,跳过容量检查
  string account     = 3;   // 用于重连豁免判定
  string device_id   = 4;
}

message AssignGateResponse {
  // 既有字段 1-6 保持不变 (ip / port / token_payload / token_signature / token_deadline / error)
  uint32 status         = 7;   // 0=ADMITTED, 1=QUEUEING, 2=ERROR, 3=EXPIRED
  string queue_token    = 8;   // status=1 时返回
  uint32 queue_rank     = 9;   // 0-based,0 = "下一个就是你"
  uint32 queue_total    = 10;  // 整个 zone 队列总人数
  uint32 retry_after_ms = 11;  // status=1 时建议轮询间隔
}

message QueryQueueStatusRequest  { string queue_token = 1; }
message QueryQueueStatusResponse {
  uint32 status         = 1;   // 0=ADMITTED, 1=QUEUEING, 3=EXPIRED
  // status=0 时 (gate token bundle):
  string ip = 2; uint32 port = 3;
  bytes  token_payload = 4; bytes token_signature = 5; int64 token_deadline = 6;
  // status=1 时:
  uint32 queue_rank = 7; uint32 queue_total = 8; uint32 retry_after_ms = 9;
  string error = 10;
}

service LoginPreGate {
  rpc AssignGate       (AssignGateRequest)       returns (AssignGateResponse);
  rpc QueryQueueStatus (QueryQueueStatusRequest) returns (QueryQueueStatusResponse);
}
```

**Java Gateway HTTP 层**保留双语义 `code=100`,用 `queueSource` 区分来源:

| HTTP code | queueSource | 含义 | 客户端动作 |
|---|---|---|---|
| 0   | —          | 已放行,gate token 已附 | 立刻 TCP 连 gate |
| 100 | "ratelimit" | Bucket4j 限流(无位次) | sleep `retryAfterMs` 重调 `/api/assign-gate` |
| 100 | "login"    | 真排队,有 `queueRank/queueTotal/queueToken` | sleep `retryAfterMs` 调 `/api/queue-status` |
| 410 | —          | queue_token 过期 | 丢 token,重调 `/api/assign-gate` |
| 429 | —          | 限流硬拒(IP 风暴 / 账号冷却) | 退避后重试或显示错误 |

> **(2026-10-15 更正,上表「客户端动作」列原文保留)** 负责人 2026-10-15 定了持有 `queue_token` 时的客户端契约,以 `docs/design/open-server-rate-limit-design.md` §11.5 的 2026-10-15 更正与补注为准。上表按此改读:
> - `100 "login"`:拿到 token 后一律轮询 `/api/queue-status`,不再是「sleep `retryAfterMs`」。每次等待 = max(`retryAfterMs`, 2s) + [0, 20%] 均匀抖动;queue-status 连续失败时下限从 2s 起翻倍,封顶 30s,成功一次即复位。
> - `100 "ratelimit"` 与 `429`:上表动作只适用于**不持** `queue_token` 的请求;429 的「退避」指 30/60/120s。
>   持 token 的客户端只在 queue-status 本身失败(网络错误 / HTTP 非 200 / `code=500`)时带 token 重调一次 assign-gate 作恢复。
>   重入回这两种时保留 token、回到 queue-status 轮询,**不**退避重调 assign-gate,否则已被放行的玩家会错过 `AdmitTTL`(60s)放行窗口。
> - 表里未列的 `503`(`zone_maintenance` / `zone_closed` / `zone_not_open`)与 `404 zone_not_found`,由 gateway 区服准入在 assign-gate 与 queue-status 上都可能返回。持 token 时是终态:不重入,按错误结束。
> - `410` 一行不变。

---

## 5. 入队/出队判定

### AssignGate 决策树(top-to-bottom,first match wins)

```
1. queue_token 非空
     ├─ ParseAndVerifyQueueToken 失败 → status=EXPIRED
     ├─ Lookup 命中 admit → status=ADMITTED + gate token(consumeAdmit 一次性消费)
     └─ 未命中 → status=QUEUEING + 当前 rank

2. account 非空
     └─ sessionmanager.GetSession() 返回非空 + GateID 非空
         → 重连/顶号:跳过排队,fast path 签 token
       否则 fall through

3. !Queue.Enabled
     → fast path 签 token(legacy 行为,kill switch)

4. FreeSlots > 0 且 ZCARD queue:zone:{id} == 0
     → fast path 签 token

5. else
     → Enqueue:ZADD queue:zone:{id} + 写 meta + token 反查索引
     → 返回 status=QUEUEING + queue_token + rank
```

> **(2026-10-15 更正,上图第 1 步「未命中 → status=QUEUEING + 当前 rank」;原文保留;已落码,未编译、未测试,待 Codex 验证)** 负责人 2026-10-15 按「最标准」定:放行之后客户端拿到确定的终态,不再停在名次 0。此前「未命中」一律回 QUEUEING,放行槽没人来取、按 `AdmitTTL` 过期之后再查,或已经取走过一次之后再查(回包丢失后重试、go-redis 超时重发 `GETDEL`),ZRANK 都落空,回「排队中、名次 0」,客户端一直轮询到 meta 按 `QueueEntryTTL`(1h)过期(§14 第 6 条)。现行判定(`go/login/internal/logic/pkg/loginqueue/queue.go` 的 `Lookup`,`:282` 起):
>
> - dispatcher 放行时在写放行槽的同一个 MULTI 里给 meta 记 `admitted_at`(放行时刻 unix 毫秒,`markAdmittedScript`,`queue.go:603`;只在 meta 还在时写,不凭空建出没有 TTL 的 meta;`HSET` 不改 meta 的 TTL)。MULTI 里各命令对其它客户端同时可见,所以「看得到 `admitted_at`」⇒「放行槽也已写过」。
> - `Lookup` 先 `GETDEL` 放行槽,取到即签票回 ADMITTED(不变)。取不到时一次 `HMGET meta zone_id admitted_at`(替换原来的 `HGET zone_id`,往返数不变)。
> - 有 `admitted_at`:再 `GETDEL` 一次(复用 `consumeAdmitAndSign`,覆盖「第一次 `GETDEL` 早于放行的 MULTI」的竞态),取到照常签票回 ADMITTED;仍取不到 = 放行槽已被取走或已过期,回 **EXPIRED**,`login_queue_expired_total` 记新原因 `admit_window_passed`(带 zone 标签)。gateway 对 assign-gate 重入与 queue-status 的 EXPIRED 都回 410,客户端丢掉 token 重走 `/api/assign-gate`(§4 表)。
> - 没有 `admitted_at`:维持原逻辑。ZRANK 命中回名次;落空是在途窗口(已 `ZPOPMIN`、写放行槽的 MULTI 还没落地,亚秒级),仍回 QUEUEING、名次 0,下一次轮询就能取到。
> - token 索引或 meta 不在、meta 损坏:EXPIRED(`meta_gone`),不变。
>
> 仍会停在名次 0 的只剩 `PopAdmit` 在写放行槽之前丢掉的条目(`pickFn` 返回 `(nil, nil)`、Marshal 失败、写放行槽的 MULTI 失败,§14 第 7 条),这类条目没有 `admitted_at`,与在途窗口无法区分。回包丢失时那张 gate 票仍找不回来,客户端收到 410 后从队尾重新排;Java 版对取走后再查同样回 410,另给取走加了请求号,让 go-redis 超时重发能拿回同一张票(`spec_G4b-G.md` §5.8 第 4 条),本仓库没有做。`queue:meta:{queueId}` 的字段因此多一个 `admitted_at`(§3 键表未列)。
>
> **(2026-10-15 补注,上面第 1 条与末段;原文保留;已落码,未编译、未测试,待 Codex 验证)** 同日复查后,`PopAdmit` 写放行的方式又改了一次:
>
> - 放行槽、`admitted_at`、在途占位改由一段脚本 `admitEntryScript` 写(`Script.Run`,即 `EVALSHA`,遇到 `NOSCRIPT` 回退 `EVAL`),不再是 MULTI{`SET`, `markAdmittedScript`, `admittedHoldScript`};`markAdmittedScript` 已删除。三者对其它客户端仍同时可见,「看得到 `admitted_at`」⇒「放行槽也已写过」照旧成立,上面 `Lookup` 的判定顺序不变。
> - 脚本先做两道只读判定:**meta 不在**(条目在队里待满 `QueueEntryTTL`,token 索引与排队令牌同时失效,再没人取得到)→ 整条丢弃,不写放行槽、不占名额、不计入放行数,每批一行 INFO(`reason=meta_gone`);**`admitted_at` 已在** → 什么都不写、计入放行数。第二道是改成单条命令的必要配套:go-redis 对单条命令在读超时 / 连接断开后会自动重发,原来的 MULTI 读失败不重发;不加这道,重发会写出第二个放行槽、签出第二张 gate 票。
> - `ZPOPMIN` 之后的写入不再随调用方 ctx(dispatcher 的 drainCtx)取消,改用 `context.WithoutCancel`:没有取消时不设截止,开区大批次不会被截断;看到停服或丢租约的取消之后,剩下的写入再给 3s 预算(`popAdmitWriteBudget`,自看到取消时起算)。停服、丢租约因此不再整批丢掉已弹出的条目。写入出错不回填队列,每批汇总一行 ERROR(`reason=write_failed`)。
> - `QueueEntryTTL` 必须为正、`AdmitTTL` 不小于 1ms(`loginqueue.ValidateTTLs`;放行脚本的 `SET PX` 取整到毫秒,亚毫秒会成为 `PX 0` 被拒):起服时 `initLoginQueue` 校验,`PopAdmit` 弹出之前也校验。
>
> 末段「仍会停在名次 0」的路径因此变为:取消之后写入超出 3s 预算、Redis 出错(含 noeviction 写满)、进程被强杀、`pickFn` 返回 `(nil, nil)`、Marshal 失败;「写放行槽的 MULTI 失败」一项改读「放行脚本执行失败」。详见 §14 第 7、8 条补注。

> **(2026-10-18 更正,上图第 1 步与上面两条 10-15 注;原文保留;`loginqueue` 包已落码,未编译、未测试,待 Codex 验证)** 本条是 10-18 改造的完整说明,文中其余 10-18 注都指向这里。方向是评审后定稿的规格,不改 proto、不新增 tip 码、不新增配置键,客户端契约不变:排队令牌照旧不透明,EXPIRED 照旧映射为 410。
>
> 本次修掉两个缺陷:
> - **D1:写满或出错就丢人。** noeviction 写满时 `ZPOPMIN` 照常弹出,放行脚本却被拒,几拍之内整条队列被弹空,所有人停在名次 0,直到 meta 过期(§14 第 7 条)。脚本出错、进程被强杀、`pickFn` 回 `(nil, nil)`、Marshal 失败,也都会丢条目。
> - **D2:等满 1h 被弹回队尾。** meta、token 索引和令牌只在入队时设一次 1h,持续轮询的人等满 1h 也会被弹回队尾(§14 第 9 条)。
>
> 现行实现在 `go/login/internal/logic/pkg/loginqueue/queue.go`。
>
> **一、出队(`PopAdmit`)**
>
> - **怎么出队。** 不再 `ZPOPMIN`。先 `ZRANGE queue:zone:{z} 0 n-1` 读出候选(只读,条目仍在队列),逐条挑 gate,再逐条跑 `admitEntryScript`(`EVALSHA`,遇 `NOSCRIPT` 回退 `EVAL`)。脚本的 KEYS 在放行槽、meta、在途占位之外多了队列键,只返回整数:
>   - GONE(1):成员已不在队列(另一副本或重叠 leader 已放行或已清理),什么都不写,不计数。
>   - META_GONE(2):meta 不在或不是 hash,`ZREM` 成员,不写放行、不占名额、不计数。判据从「不存在」扩为「不是 hash」,类型损坏的 meta 不再是卡死整个 zone 的毒丸。
>   - ALREADY(3):成员仍在队列,但 meta 已有 `admitted_at`。只可能是上一次非写满原因造成的部分写入,`ZREM`,不计数。
>   - WRITTEN(4):依次 `HSET admitted_at`、`SET admit:{queueId} PX AdmitTTL`、`ZADD` 在途占位(剩余 `PTTL` < W 才 `PEXPIRE W`)、`ZREM` 成员,计数。
>   - REPLAYED(5):成员已不在队列,且 `admitted_at` 恰等于本次的放行时刻,说明 go-redis 重发了已执行成功的同一条命令。什么都不写,计数。
> - **停批规则。** 只有与条目无关的错误才让本批在第一个出错处停下,剩余条目原样留在队列:脚本出错(写满、连接 / 超时、占位键或队列键 `WRONGTYPE`),以及调用方 ctx 取消(在下一次跑脚本之前检查)。以下情况只跳过该条:挑 gate 失败、`pickFn` 回 `(nil, nil)`、Marshal 失败;条目从未离开队列,分数不变,下一拍再试。meta 不在或类型损坏、空 member,由脚本就地 `ZREM`,不停批。
> - **返回契约与日志。** `PopAdmit` 返回 (已放行数, err),err 非空时已放行数照样有效。dispatcher 先 `recordAdmit` 再判错误,ctx 未取消时每拍至多打一行 `ERROR [loginqueue] PopAdmit zone=<z> err=…`;err 里带 `queueId`、`admitted`、`skipped`、`untried`,或 `zrange: …`。另有每批至多一行 `INFO [loginqueue] PopAdmit entries not admitted zone=<z> metaGone=<n> alreadyAdmitted=<n> gone=<n>`。10-15 版的两行 `PopAdmit dropped entries after zpopmin …`(`reason=write_failed` 的 ERROR 与 `reason=meta_gone` 的 INFO)已删除。
> - **不丢条目。** 任何 Redis 错误、写满、取消、进程退出之后,条目要么仍在队列,要么已放行可取。上面 10-15 补注末段列出的「仍会停在名次 0」的路径全部消除。`popAdmitWriteBudget`(`WithoutCancel` 写入预算),以及 `pickFn` 出错时以分数 0 回填的 `ZADD`,随之删除。代价是 Stop 最多多等一条在途命令的 `ReadTimeout`:客户端没开 `ContextTimeoutEnabled`,而取消之后 go-redis 的重试退避直接返回,不再重发。
> - **结果不确定时。** go-redis 重试用尽的读超时之后,条目要么已写成并移出(客户端可取),要么仍在队列,下一拍不会重复放行。写成的那条不计数,只影响指标。
> - **重叠 leader / 多副本。** 两边可能 `ZRANGE` 到同一批,脚本第一步 `ZSCORE` 保证每条只写一次,后到的拿到 GONE。在途上界仍 ≤ 2×(capacity − online)。
> - **快速通道插队窗口消失。** 在本批每条都原子地「移出 + 写占位」之前,快速通道看到的队列不会变空(§14 第 7 条)。
> - **写满时 fail-closed(maxmemory + noeviction)。**
>   - Redis 6/7 兼容模式(脚本无 shebang)下,`EVALSHA` 本身不带 DENYOOM,写满时脚本照常开始执行。
>   - 只有「脚本开跑前已判定 OOM,且尚未执行过任何写命令」时,Redis 才拒绝脚本里的那条 DENYOOM 写(`scriptVerifyOOM`)。一旦执行过任何写(包括没删到东西的 `ZREM`、`PEXPIRE`),后续命令就不再受 maxmemory 约束。
>   - 所以写段的第一条写必须是 `HSET`(DENYOOM),它之前只有只读命令。写满时整段被拒,什么都没写,条目留在队列、名次不变。不得把 `ZREM`、`PEXPIRE` 等非 DENYOOM 写挪到 `HSET` 前面,否则写满时会静默写穿 maxmemory。
>   - META_GONE / ALREADY 分支的 `ZREM` 不是 DENYOOM 命令,写满时照常清理,这是有意的。不用 `#!lua` 的理由见 §14 第 7 条的 10-18 补注。
>   - 写满期间:dispatcher 停止放行,每拍一行含 `OOM` 的 ERROR;排队中的轮询与续期照常(读状态脚本里没有 DENYOOM 命令);新入队整体失败(`StatusError`)。所以需要 redis-queue 的内存水位告警。
>   - 这段语义的证据只有 miniredis 单测(用钩子模拟 OOM 回包)与对 Redis 源码判定条件的推理。真实 Redis 上的证据待 `spec_G4b-G.md` §8.4 的 2026-10-18 补注第 1 项。
>
> **二、查询(`Lookup`)**
>
> - **签名。** 改为 `Lookup(ctx, token, zoneID, signFn)`。`zoneID` 必须取自已验签的排队令牌(`ParseAndVerifyQueueTokenMulti` 的第二个返回值),0 视为损坏。仓内的调用方只有 assign-gate 重入、queue-status 两个 handler 与测试。
> - **判定顺序。**
>   1. TTL 不合法(`ValidateTTLs`):回错误,不碰 Redis。这是纵深防御,挡住绕过起服装配直接 `New` 的调用方,免得续期写出 `PEXPIRE 0` 当场删键。
>   2. zone 为 0:EXPIRED(`meta_gone`),不碰 Redis。
>   3. `GET queue:token:{token}` 不在:EXPIRED(`meta_gone`)。空闲超过 `QueueEntryTTL` 的条目走这里。
>   4. `GETDEL admit:{queueId}` 取到:现签 gate 票,ADMITTED(不变)。
>   5. 没取到:跑一次 `lookupStateScript`,KEYS 是 meta、`queue:zone:{令牌里的 zone}` 与 token 索引。
>      - 仍在队列:QUEUEING,带名次与总数,按需续空闲 TTL。
>      - meta 有 `admitted_at`:再 `GETDEL` 一次,保留对「第一次 `GETDEL` 落空之后放行才落地」的覆盖。取到回 ADMITTED,否则 EXPIRED(`admit_window_passed`)。
>      - meta 在、没有 `admitted_at`、又不在队列:EXPIRED(**`queue_gone`**,新原因)。
>      - token 索引恰在两步之间到期、meta 不在或不是 hash、meta 的 `zone_id` 与令牌不符:EXPIRED(`meta_gone`)。
> - **在途窗口消失。** 「出队」与「写 `admitted_at`」在放行脚本的同一段里,读状态脚本与放行脚本互斥原子。所以旧的在途窗口(已 `ZPOPMIN`、还没写放行,回名次 0)不复存在,「HMGET 时还没放行、ZRANK 时已放行」的交错也随之消失。上面 10-15 更正中「没有 `admitted_at`:ZRANK 落空回名次 0」一条改读 `queue_gone`。
> - **`queue_gone` 的来源。** 纯新版下只有三个:
>   1. 整个 zone 空闲 ≥ `QueueEntryTTL` 之后,队列键先于某条 meta 过期。该条目空闲 ∈ [E, 1.25E],本来就该 EXPIRED,只是原因标签不同。
>   2. 人工 `ZREM` 成员或 `DEL queue:zone:<z>`(§15)。
>   3. 排队开着时混跑旧版 leader:旧版已 `ZPOPMIN`、还没写放行的条目。发布约束禁止这种情况,见下文第四部分。
> - **往返数。** 排队中一次轮询 = `GET` + `GETDEL` + 一次 `EVALSHA`,此前是 `GET` + `GETDEL` + `HMGET` + `ZRANK` + `ZCARD`(5 → 3)。进程内首次调用会多一次 `NOSCRIPT` 回退。轮询从纯读变为「偶尔带写」(见下文第三部分的续期)。
> - **zone 标签。** `login_queue_expired_total` 的 zone 标签一律取令牌里的 zone;此前 token 索引不在时记 `unknown`。
>
> **三、空闲 TTL 与令牌**(下文记 E = `QueueEntryTTL`)
>
> - **续期规则。** 由读状态脚本执行,且只在条目仍在队列时执行。meta、token 索引、队列键三把键各自判 `PTTL`:只有不是 −2、且 < E 时,才 `PEXPIRE` 到 1.25E。
>   - 效果:空闲不足 E 的条目不会过期,空闲 TTL 落在 [E, 1.25E]。每个条目约每 E/4 才续一次,而不是每次轮询都写。新值 > E > 触发时的 PTTL,天然只增不减。
>   - `PTTL` 为 −1(没有 TTL)时顺手设上 TTL;不存在的键不碰,`PEXPIRE` 也不会建键。
>   - 放行之后(成员已移出,或 `admitted_at` 已在)不再续期。
>   - 不用「PTTL < E/2 才续到 E」:那会让空闲 30min 的条目也可能过期,违背「`QueueEntryTTL` 是最短空闲时长」的语义。
> - **入队对队列键只增不减。** `Enqueue` 改为 `EXPIRE NX` + `EXPIRE GT`。此前无条件 `EXPIRE E`,会把续上的 1.25E,或 `QueueEntryTTL` 较大的副本写的 TTL,压回本副本的 E。NX 与 GT 缺一不可:只用 GT 的话,没有 TTL 的键永远设不上 TTL(陷阱同 §5「在途占位的生命周期」的「为什么不用 `PEXPIRE … GT`」)。meta 与 token 索引是新键,照旧无条件设 E。**版本下限**:EXPIRE 的 NX / GT 需要 Redis ≥ 7.0(此前排队路径只要求 6.2 的 GETDEL);低于 7.0 时整段 MULTI 回 EXECABORT,入队全部失败。生产与 docker-compose 均为 7.2。
> - **排队令牌固定 24h。** 令牌的 HMAC 过期改为 24h(`loginqueue.queueTokenMaxAge`,代码常量,不是配置),此前等于 `QueueEntryTTL`。令牌只回答「这张票还能不能用」,空闲过期由上面的 Redis TTL 管。令牌离开 Redis 条目就没用,实际可用期 = min(24h, 条目存活),所以延长它不扩大被截获令牌的危害面(§6)。
> - **`QueueEntryTTL` 取值收紧为 [1s, 24h]**(`ValidateTTLs`,起服装配 `initLoginQueue` 沿用),区间外起服失败。1s 下界与 `Enqueue` 的秒级 `EXPIRE` 对齐,也保证续期的毫秒值为正;上界是因为令牌 24h 先到期,再大的空闲 TTL 没有意义。
> - **写放大估算(未压测)。**
>   - 每个排队条目只在首次轮询与之后约每 E/4 产生一次写(meta 与 token 索引各一条 `PEXPIRE`),队列键每 zone 约每 E/4 一次。
>   - 排队 4.5 万人、2s 轮询、E=1h 时,稳态续期约 100 条 `PEXPIRE`/s;入队洪峰期间另加约 2 × 入队速率(新条目的首次轮询都会触发续期)。
>   - 没触发续期的执行是纯读,不进复制流与 AOF。触发时,复制 / AOF 里是 MULTI 加 `PEXPIREAT`,并产生 expire 键事件(部署配了 `notify-keyspace-events Eg`)。
>   - 每次 `EVALSHA` 的 Redis CPU 未测,R16 复测时要看。
>
> **四、发布约束**
>
> 本版本以及从它回滚,都必须在 `Queue.Enabled=false` 下滚完。
> - 排队开着混跑时,旧版 leader 已 `ZPOPMIN`、尚未写完放行的条目,会被新版判成 `queue_gone`、打回队尾。所以下文「新旧版本混跑与部署顺序」里的「不丢人」在本版本的升级窗口里不成立。
> - 不写代码兼容分支:拿不到区分新旧条目的信号。
> - K8s 上依赖 `tools/scripts/k8s_deploy.ps1` 的 `$script:LoginQueueMergeGateCleared` 拦截(门槛未清时各档位排队都关)。手工打开排队的人,照 `login.yaml` 上线清单第 0 步的 10-18 补注做。
>
> **五、残余风险**
>
> 1. 非写满原因的部分写入。实际可达的只有一种:占位键被改成别的类型,`ZADD` 报 `WRONGTYPE`。
>    - 此时 `HSET` 与 `SET` 已写,成员仍在队列。客户端仍可取走放行槽,或在 `AdmitTTL` 后得到 `admit_window_passed`,不会双签。
>    - 同一个键也让 `FreeSlots` 报错,所以修复之前不再放行;修好之后的下一拍走 ALREADY,清掉成员。
>    - 影响:至多多放 1 人、持续一个 `AdmitTTL`。
> 2. 写满时放行停止(fail-closed),见第一部分;需要内存水位告警。
> 3. 结果不确定的脚本只影响计数,见第一部分。
> 4. REPLAYED 遇上另一副本恰在同一毫秒放行同一条目时,会重复计数,只影响指标。
> 5. 混跑约束,见第四部分。
> 6. 空闲 TTL ∈ [E, 1.25E]。整个 zone 空闲时,`queue_gone` 与 `meta_gone` 都可能出现,都是 EXPIRED,客户端照常重排。
> 7. 客户端空闲接近 E 时恰好被放行:meta 或 token 索引先到期,回 `meta_gone`,放行槽白占至多一个 `AdmitTTL`。
> 8. 持续轮询超过 24h:令牌按 `ttl` 拒绝,客户端重排;他原来的成员与 meta 仍可能再活到 1.25E 并被放行,浪费一个 `AdmitTTL`。不改代码:实际到不了;要改得给 `ParseAndVerifyQueueTokenMulti` 加返回值,续期也得改成 `PEXPIREAT min(...)`,代价与收益不成比例。
> 9. 哨兵切主时异步复制可能丢掉放行效果,条目可能再次放行、签出第二张票。既有问题,不在本次范围。
> 10. `pickFn` 对特定条目确定性失败时,该条会永久占一个批位,让排在它后面的人名次多 1。生产 `pickFn` 与条目无关,走不到。
> 11. zone 满(`free == 0`)时 dispatcher 不调 `PopAdmit`,队首的 META_GONE 孤儿要等有空位的那一拍才清理(既有)。
> 12. R16 负载未测,见第三部分的写放大估算。
>
> **六、后续可选项**:评审 #11 / #20 提议把第一次 `GETDEL` 并进读状态脚本,往返 3 → 2,同时可退役测试注入点 `afterAdmitMiss`。这是等价简化,不是缺陷修复;本次保留「`GETDEL` → 注入点 → 读状态脚本 → 必要时二次 `GETDEL`」的结构。
>
> **七、用例**(待 Codex 执行,`queue_test.go` / `dispatcher_test.go`)
> - 写满与出错:`TestPopAdmit_OOMOnAdmitScript_EntriesStayQueued`、`TestPopAdmit_ScriptErrorStopsBatch_PartialWriteCleanedAsAlready`、`TestPopAdmit_PickFailure_EntryNeverLeavesQueue`、`TestPopAdmit_CtxCanceledMidBatch_RestStayQueued`、`TestDispatchOnce_PartialBatchError_CountsAdmittedAndLogsOnce`、`TestDispatchOnce_CanceledDuringPopAdmit_NoErrorLog`。
> - 只写一次与毒丸:`TestPopAdmit_OverlappingLeaders_SameEntryAdmittedOnce`、`TestPopAdmit_MetaNotHash_CleanedWithoutStallingZone`、`TestPopAdmit_EmptyMemberAtHead_Removed`。
> - 空闲 TTL:`TestLookup_ContinuousPollingOutlivesEntryTTL`、`TestLookup_RenewalIsLazy_OnlyBelowEntryTTL`、`TestLookup_IdleBeyondIdleTTL_Expires`、`TestLookup_NoRenewalAfterAdmit`。
> - 判定分支:`TestLookup_MetaPresentNotQueuedNotAdmitted_ReturnsQueueGone`、`TestLookup_ZoneMismatchOrZero_ReturnsMetaGone`、`TestLookup_InvalidEntryTTL_FailsWithoutTouchingRedis`。
> - 入队与令牌:`TestEnqueue_QueueTokenExpiresAtMaxAge`、`TestEnqueue_QueueKeyTTLNeverShortened`。
> - 删除了 `TestPopAdmit_NoCancel_LongBatchNotCutByWriteBudget` 与 `TestPopAdmit_CtxCanceledMidBatch_WriteBudgetExhausted_DropsRest`,它们只验证已删除的写入预算。
>
> **八、Java 版**:待做,由主会话在 Java 仓库 `PARITY.md` 登记。

### FreeSlots 公式

```
online       = Σ(gate.PlayerCount for gate in GateWatcher.FetchAllNodes() if gate.zoneId == z)
capacity     = Queue.ZoneCapacityOverride[z] || ceil(max(online, 1) * SoftCapMultiplier)
admitted_inflight = 清掉过期成员后的 ZCARD admitted:zset:zone:{z}
free         = max(0, capacity - online - admitted_inflight)
```

**关键安全属性 — admitted_inflight 必须减**:dispatcher 写出 admit 后,该玩家从 ZSET 中 popped(不在 ZCARD)、从 gate.PlayerCount 中也未出现(他还没真连上),如果 FreeSlots 不算这部分,下一拍会再放出一批,**一秒内超发数倍 admit token**,gate 那边玩家 token 验证全失败。取走后占位再留 20s(2026-10-13 起为 15s,10-15 改 20s),覆盖「票已交出、gate 还没把人数报上来」这一段(G4b-G;此前取走即撤占位,这一段漏计,见下一小节)。

> **(2026-10-18 更正,上段「从 ZSET 中 popped」;原文保留;未编译、未测试)** 改读:放行脚本在写放行槽与在途占位的同一段里,把成员 `ZREM` 出队列,所以写出 admit 之后它同样不在 ZCARD 里。安全属性与「admitted_inflight 必须减」的结论不变;差别只在出队与写占位现在是原子的(§5 决策树后的 2026-10-18 更正)。

### 在途占位的生命周期(G4b-G,2026-10-13)

2026-10-13 之前在途占位是 `admitted:zone:{z}`(SET,整键滑动 TTL),排队一打开就有两条缺陷:

- **Q1**:快速通道的 `fast:<uuid>` 成员从不单独删除,每次写入又把整键 TTL 续上。已连上 gate 的快速通道玩家同时算进 `online` 和在途,持续有人登录时余量被重复扣到 0,「放一批、停最长 60s」。
- **Q2**:取走即 SREM,而 gate 的人数要等下一次上报才涨。开区时每拍都按「在线 0、在途 0」再放一满批,能放进几倍容量。

G4b-G 换成新键 `admitted:zset:zone:{z}`(ZSET,分数 = 占位到期的 Unix 毫秒),每个成员按自己的分数到期。完整推导、测试与评审记录见 [spec_G4b-G.md](../handoff/launch-readiness-20261013/spec_G4b-G.md)。

一个占位成员的一生(`G` = 交票后宽限 20s,2026-10-15 起;10-13 至 10-15 为 15s。`now` = login 进程时钟的 Unix 毫秒):

| 路径 | 成员 | 写入时的分数(到期毫秒) | 之后 | 何时消失 |
|---|---|---|---|---|
| dispatcher 放行 | `queueId` | `now + AdmitTTL` | 客户端轮询取走时改成 `now + G` | 没人来取:放行后 `AdmitTTL` 到点;取走:取走后 `G` 到点 |
| 快速通道占位 | `fast:<uuid>` | `now + G` | 不再改 | 占位后 `G` 到点 |

- 到点的成员在**下一次计数或占位**时被 `ZREMRANGEBYSCORE key -inf now` 清掉(分数 ≤ now 即过期),没有后台清理任务。计数 = 清掉过期成员之后的 `ZCARD`,清理与计数在同一段 Lua 里,所以计数是一条写命令。
- 取走只发生一次(`GETDEL` 只成功一次)。重复轮询在改分数之前就返回,不会反复续期。`signFn` 失败、meta 读不到 zone 时不改分数,占位保持放行时的分数,最迟 `AdmitTTL` 到点。
- 整键 TTL 只做垃圾回收,成员是否有效只看分数。zone 下线或被合走后没人再来读,整键 TTL 负责把残留收走。**只延长不缩短**(2026-10-15 起):三条写路径(放行、取走、快速通道)都是先 `ZADD`,再看剩余 `PTTL`,小于 `W = max(AdmitTTL, G) + 1min`(默认 2 分钟)才 `PEXPIRE W`。
  - 为什么:`W` 由各副本按自己的 `AdmitTTL` 算,排队开着时滚动调整 `AdmitTTL` 就有新旧两种 `W` 并存。10-13 版每次写入无条件 `PEXPIRE W`,小 `W` 副本的写入会把整键 TTL 压到大 `W` 副本写的占位之前,该 zone 随后一段时间没有写入(例如满员停放)时,整键连同未到期的占位一起消失,下一拍最多多放一批。只延长不缩短之后整键不会先于任何成员到期,`AdmitTTL` 可以在排队开着时滚动调整(此前「调整幅度不超过 1min」的约束作废)。代价:调小之后,已续上的较大整键 TTL 要自然走完才收敛,只影响 zone 下线后键多留多久。做法与 Java 版的「不足才续」一致。
  - 为什么不用 `PEXPIRE … GT`:`GT` 把没有 TTL 的键当作 TTL 无限大,刚由 `ZADD` 建出的键就永远设不上过期(miniredis v2.35.0 已对源码核实;真实 Redis 7 凭记忆,未核实)。也必须先 `ZADD` 再读 `PTTL`:键不存在时 `PTTL` 回 −2,`PEXPIRE` 打空,随后 `ZADD` 建出的键没有 TTL。
  - 放行与取走共用一段脚本(`queue.go` 的 `admittedHoldScript`):放行在原来的 MULTI 里 `SET` 放行槽后 `EVAL` 它,取走直接 `EVAL`;快速通道脚本里是同样的两行。只有 10-15 之前的 G4b-G 镜像(无条件续期)与本版在排队开着、`AdmitTTL` 不同时混跑,旧镜像的副本才仍会缩短整键 TTL;按下文「排队关着滚」的部署顺序不会出现。
- 旧键 `admitted:zone:{z}` 新版本不读、不写、不删。最后一个旧版本停写后,它在 60s 内整键过期。

**时钟:login 进程时钟,经 ARGV 传进脚本**。分数和「现在」都由 Go 用 login 的墙钟算好,不用 Redis `TIME`。理由:测试直接拨假时钟,脚本里没有时间运算;与 Java 版、本包既有做法(排队令牌、gate 票的过期都用 login 墙钟)一致。代价是多副本时钟偏差 δ 直接进占位寿命(± δ)。同一 zone 的副本间偏差按毫秒到几十毫秒估(假设,未测),达到宽限量级(≥ 20s)才有可见后果。2026-10-15 负责人确认维持 login 时钟:系统本来就依赖各节点时钟同步(gate 票与排队令牌的过期同样用 login 墙钟),**login 节点做 NTP 时钟同步是部署要求**;也不在脚本里加上界清理(时钟故障按运维故障处置,见下面的止血):

- **故障副本还在跑时**:它读的时候按自己的钟清理,别人写的、其实还有效的占位被早清,多放。
- **故障副本恢复(NTP 校回)或被摘掉之后,不会立刻自愈**:它最后 20s / 60s 内写的占位,分数停在「故障钟 + 20s / 60s」,还要再过 δ 才到期。这段时间该 zone 的有效容量少掉这些占位(≤ 当时的 `capacity − online`)。清理只有 `-inf … now` 一个方向,而每次写占位都保证整键 TTL 至少剩 `W`,所以只要 zone 还有写入,整键 TTL 不兜底;整个 zone 完全停放时,最长约 `AdmitTTL` + 整键 TTL(默认 3 分钟)后自愈。旧实现没有「占位按某个副本的钟到期」这回事,这是新增的失败模式。
- **判断与止血**:`redis-cli ZRANGE admitted:zset:zone:<z> 0 -1 WITHSCORES` 里有分数比当前毫秒时间戳大出 `max(AdmitTTL, 20s)` 以上的成员,就是这种残留。止血 `redis-cli DEL admitted:zset:zone:<z>`,代价是在途清零,最多多放一批 `capacity − online`(写在 runbook 模式 2)。
- **退路**:偏差不可忽略时改用 Redis `TIME`。键、成员、分数含义都不变,新旧两种写法可以混跑。

**20s 从哪来**:票交出去之后,这个人要「连上 gate(耗时 `c`)→ gate 下一次上报人数(C++ gate 每 10s 一次,`cpp/nodes/gate/main.cpp` 的 `playerCountReportTimer`)→ login 读 etcd(不缓存)」才出现在 `online` 里,最坏滞后 `c + 10s`。20s = 两个上报周期 = 一个周期的上报滞后 + 10s 建连与传播余量。Java 版是 15s,它的 gate 每 5s 发布一次,余量同为 10s。2026-10-13 本仓库照搬 15s,余量只有 5s;10-15 负责人按「最标准」改为 20s,两版余量对齐,宽限值本身不再相同。

- 20s 是代码常量 `loginqueue.defaultHandoffGrace`,不是配置。它与 gate 的上报周期没有机械关联,改上报周期时要同步核对它。数值待 L-14 测「交票 → gate 会话建立」的分布后确认。
- 快速通道当场交票,等同排队路径的「已取走」,所以也取同一个宽限(20s,代码里是同一个字段),不取 `AdmitTTL`(Java 版取 60s,这一点两版有意不同)。

**算式**:记 `C` 容量,`O` gate 上报的在线数,`A` 未到期的占位数,`free = max(0, C − O − A)`;`v = c + φ` 是交票后这个人出现在 `O` 里的时刻,`φ ∈ [0, 10s)` 是距下一次上报的相位。以下按 `G` = 20s 算(10-13 版按 15s 算的数字见 spec_G4b-G.md §5.2 的括注)。

- `v < G` 时,`[v, G)` 这段被算两遍(占位 + `O`),少放。`c` 取 1s 时双计 9~19s、平均约 14s;瞬时少放的名额 ≈ 交票速率 × 14s,最多 × 20s。
- `v > G` 时,`[G, v)` 这段一遍都没算,多放。只在 `c + φ > 20s` 时出现,建连在 10s 以内时为 0。gate 迟迟不上报或建连普遍超过 10s 时,多放的节奏从修复前的约每 2~3s 一批降到约每 22~23s 一批,每批 ≤ `C − O`:有界改善,不是消除。
- gate 票有效期 10 分钟,远大于宽限。拿了票超过 20s 才连上的人不在任何占位里,连上就是多 1 人(既有性质;gate 侧另有连接上限)。

**放行节奏的上界**(判读压测与 runbook 用):每次放行都以 `O + A ≤ C` 为界。一个名额被排队放行的人拿走后,最早要到「取走 + 20s」才可能放回,而且前提是这个人没有进 `O`。

- 放进去的人留在线上(正常玩家):名额从占位转给在线数,不放回。排队的长期放行速率 = 在线玩家的离开速率;`O + A` 顶到 `C` 之后 `q_admitted` 不涨是**正常回压**,不是 dispatcher 卡死。
- 拿了票不连 gate,或连上很快就走:名额每约 22~23s(取走延迟 ≤ 轮询 2s + 一拍 1s,再加 20s)周转一次,任意这样一个窗口内经排队放行的人数 ≤ 窗口起点的 `C − O`。修复前这个周期是 2~3s(Q2)。
- §11 与 runbook 里写于 2026-05 的节奏数字没有按这个上界标定过,待 L-14 重新标定。

**新旧版本混跑与部署顺序**:旧 Pod 读写 `admitted:zone:{z}`(SET),新 Pod 读写 `admitted:zset:zone:{z}`(ZSET)。键不同,不会 `WRONGTYPE`;但两边互相看不见对方的占位。

- **要求的做法:排队关着滚**。`Queue.Enabled=false` 时不构造 `Queue`,新旧两种键都没人碰,滚动对占位记账是空操作(当前所有环境都是这个状态)。先在排队关闭下把所有 zone 的 login 滚到含 G4b-G 的版本,再打开排队。这条要求落在三处:`login.yaml` 上线清单第 0 步(手工打开排队的人照它做);K8s 上的合入顺序 G1b → G4b-G → G4b-2(只有 G4b-2 才写 `Queue` 段,它的契约用例 15 断言 `queue.go` 已是新键);本节与 runbook「故障应急」。
- **排队开着滚**(只在本机或裸机有人手工打开过时可能),按谁经手逐格记账:
  - 旧 leader 放行、旧 Pod 取走:与 G4b-G 之前完全相同,取走即 SREM,Q2 照旧;
  - 旧 Pod 的快速通道:照旧写旧键的 `fast:<uuid>`,Q1 照旧;
  - 旧 leader 放行、新 Pod 取走:旧 SET 里的成员没人删,按旧版 Q1 的方式残留(旧 leader 少放);新键里补一个 20s 的占位;
  - 新 leader 放行、旧 Pod 取走:新键里的占位不被改分数,留到放行后 60s(少放,有界);
  - 新 leader 放行、新 Pod 取走,以及新 Pod 的快速通道:G4b-G 的行为;
  - 两种键互不可见带来的额外量:新旧两边的快速通道各自按自己的键判余量,任一时刻在途占位合计最多 2 ×(`C − O`);
  - 不丢人、不重复放行:`ZPOPMIN` 与 `GETDEL` 的原子性与占位键无关。
- **结论**:相对各自版本单跑,混跑额外多放 ≤ 一个 `C − O`、少放有界,窗口等于滚动时长;**leader 或经手的副本是旧版期间 Q1 / Q2 仍在,不能把混跑窗口当作 G4b-G 已生效**。G4b-G 的两条修复只在放行方与取走方都是新版时才成立。可以接受,但不推荐。
- **回滚(新 → 旧)**:窗口与上面对称,新键在最后一次写入后 2 分钟内整键过期(有副本配过更大的 `AdmitTTL` 时,按其中最大的 `W`)。排队开着时不要用 `kubectl rollout undo` 退镜像:先用 `-LoginQueue off` 关排队,再退(§15)。

> **(2026-10-18 更正,上面「不丢人、不重复放行」一条;原文保留;未编译、未测试)**
> - **不重复放行。** 新版靠放行脚本第一步 `ZSCORE` 判成员仍在队列(每条只写一次),不再靠 `ZPOPMIN`;`GETDEL` 的部分不变。
> - **不丢人在升级窗口里不成立。** 排队开着混跑 2026-10-18 前后两版时,旧版 leader 已 `ZPOPMIN`、尚未写完放行的条目,会被新版判成 `queue_gone`(EXPIRED,410)、打回队尾。
> - **结论。** 本版本以及从它回滚,都只能排队关着滚,上面的「排队开着滚」逐格记账不适用于这一版。见 §5 决策树后的 2026-10-18 更正「四、发布约束」。

### Dispatcher Loop

```
zoneId = Node.ZoneId  // 只出本 zone;队列空直接跳过
if ZCARD queue:zone:{zoneId} == 0: return   // 先判空,空闲 zone 不碰 gate 目录
candidates = CandidatesForZone(zoneId)      // 每个非空拍只取一次 gate 候选(2026-10-15 起)
free = FreeSlots(candidates)                // 再算容量,与挑 gate 共用这一份候选
if free <= 0: return
batch = min(free, ZCARD)
members = ZPOPMIN queue:zone:{zoneId} batch
picker = 本拍的挑 gate 器(candidates)        // 负载 = PlayerCount + 本拍已分给它的人数
for each queueId in members:
    gate = picker.pick()                    // 取负载最少、同负载 NodeID 最小,选中后本拍计数 +1
    MULTI
      SET admit:{queueId} <AdmitSlot JSON:选好的 gate> EX AdmitTTL   // gate 票在取走时才签
      EVAL admittedHoldScript:              // 一段脚本(2026-10-15 起)
        ZADD admitted:zset:zone:{zoneId} <now_ms + AdmitTTL> queueId   // 占位按自己的分数到期
        if PTTL < W: PEXPIRE W              // W = max(AdmitTTL, 20s) + 1min,整键 TTL 只延长不缩短
    EXEC
```

> **(2026-10-18 更正,上面的伪代码;原文保留;未编译、未测试)** 上面是 10-15 之前的写法。10-15 起写放行已是一段 `admitEntryScript`;10-18 起出队不再 `ZPOPMIN`。现行写法:
>
> ```
> zoneId = Node.ZoneId
> if ZCARD queue:zone:{zoneId} == 0: return
> candidates = CandidatesForZone(zoneId)
> free = FreeSlots(candidates)
> if free <= 0: return                        // zone 满时不出队,队首孤儿也要等有空位那一拍才清
> batch = min(free, ZCARD)
> members = ZRANGE queue:zone:{zoneId} 0 batch-1   // 只读,条目仍在队列
> picker = 本拍的挑 gate 器(candidates)
> for each queueId in members:
>     slot = picker.pick()                    // 失败或无 gate:跳过该条,它从未离开队列、分数不变
>     if ctx 已取消: return (已放行数, ctx.Err())    // 剩余条目原样留在队列
>     EVALSHA admitEntryScript:               // 一段脚本,只返回整数
>       if queueId 不在 queue:zone:{zoneId}:
>           admitted_at == 本次放行时刻 ? REPLAYED(计数) : GONE
>       if TYPE meta != hash: ZREM → META_GONE
>       if meta 已有 admitted_at: ZREM → ALREADY
>       HSET meta admitted_at <now_ms>        // 写段第一条必须是 DENYOOM 的 HSET:写满时整段被拒
>       SET admit:{queueId} <AdmitSlot JSON> PX AdmitTTL
>       ZADD admitted:zset:zone:{zoneId} <now_ms + AdmitTTL> queueId
>       if PTTL < W: PEXPIRE W
>       ZREM queue:zone:{zoneId} queueId → WRITTEN(计数)
>     脚本出错: return (已放行数, err)       // 停批,剩余条目原样留在队列
> 有跳过的条目: return (已放行数, 首个跳过原因)
> // dispatchOnce:先 recordAdmit(已放行数),err 非空且 ctx 未取消时打一行 ERROR
> ```

**一拍的 gate 分配(2026-10-15 起;未编译、未测试)**:每个非空拍只调一次 `CandidatesForZone`(生产实现是一次 etcd 前缀读加一次排空标记 MGET,不缓存),算容量和批内挑 gate 共用这一份。批内按「gate 上报的 `PlayerCount` + 本拍已分给它的人数」挑最少的,同数取 NodeID 最小,选中后本拍计数 +1:一拍的放行先填最闲的 gate,填平之后在各台之间轮转。账只记在本拍,下一拍重新取候选、从 gate 上报的人数重新算起。

- 改之前:每放一个人重取一次候选(一拍放 N 人就是 N 次串行目录读),而 `PlayerCount` 每 10s 才上报一次、一拍之内不变,只按它挑会让整批都落到同一台 gate。这批条目已被 `ZPOPMIN` 弹出、既不在队列也不在占位里,拍越长,进程中途退出时丢的越多。
- 快速通道(assign-gate 当场签票)仍是一次 `FreeSlots` 加一次签票,各读一次目录,单次挑选规则不变(`PickGate`,与批内挑选共用同一份规则)。
  - (2026-10-18 更正,已落码、未编译未测试)两处改了:① 读目录改读 login 进程内的 gate 目录 list-watch 镜像,镜像不新鲜时才回落 etcd 前缀读,上文「不缓存」不再成立;② 快速通道的 `PickGate` 改为随机抽两台取负载少的那台,不再与批内挑选同一规则,批内仍是确定性记账。详见 [gate-load-balancing-design.md](./gate-load-balancing-design.md)「gate 目录怎么读、怎么挑」。
- 本拍没有候选时 `FreeSlots` 先报 `no gates for zone`,整拍不出队、条目原样留在队列(日志口径不变,见 runbook 模式 1)。
- 与 Java 版「一轮按段挑 gate、把这一轮已分给它的人数算进负载」对齐(Java 仓库 `PARITY.md`「登录排队」行 ③)。

> **(2026-10-18 更正,上面第一条「这批条目已被 `ZPOPMIN` 弹出……丢的越多」;原文保留;未编译、未测试)** 这句已不成立:`PopAdmit` 先 `ZRANGE` 读候选,条目在放行脚本写成之前一直在队列里,进程中途退出不丢人。一拍只取一次候选的理由,剩下省往返数与批内均衡两条。第三条「本拍没有候选」:`FreeSlots` 先报错的口径不变;万一走到挑 gate 时才没有候选,条目只被跳过、不离开队列、分数不变(此前以分数 0 放回队首)。

2026-10-12(G1b)以前是 `for each zoneId in activeZones()`,且先 FreeSlots 后 ZCARD。`activeZones` 实际只含本 zone(见 §7.1),所以改成单 zone 不丢功能。改成先判空的代价:队列非空、本 zone 却一台 gate 都没有时,每拍一条 `[loginqueue] freeSlots zone=… queueLen=… err=…` ERROR(以前被静默跳过)。

**signFn 失败回滚**:如果选 gate 或签 token 失败(罕见,gate 全部下线),把 queueId 以 score=0 重新 ZADD,保证下一拍最优先重试,不丢人。见 `queue.go:PopAdmit`。

> **(2026-10-18 更正,上一段;原文保留;未编译、未测试)** 不再有「回滚」:出队之前条目从未离开队列。挑 gate 失败、回 `(nil, nil)` 或 Marshal 失败时,`PopAdmit` 只跳过该条,分数不变,下一拍按原位次重试;首个跳过原因随返回值交给 dispatcher,打进当拍的那一行 ERROR。以分数 0 回填的 `ZADD` 已删除,它在队列被弹空后建出没有 TTL 的队列键的边角也随之消失。签 gate 票本来就在取走时才做(R17 R1),不在出队路径上。

---

## 6. Queue Token 安全模型

`queue_token` 是服务端签名的 opaque blob:

```
queue_token = base64url( JSON{kind:"queue", q:queueId, z:zoneId, e:expireTs} | "." | hex(HMAC-SHA256) )
```

- **HMAC 用什么 secret**:复用 `GateTokenSecret`。理由:queue token 和 gate token 的 wire-format 完全不同(JSON+HMAC vs proto-marshalled GateTokenPayload),不可能互相 confuse;一个 secret 旋转就同时换两套,运维更简。
- **(2026-10-14 事实更正,上一条已过时、原文保留)**:代码早已按用途拆出独立的 `Secrets.QueueToken`,**不再复用 `GateTokenSecret`**(`go/login/internal/config/secrets.go` 包注释与 `SecretsConf.QueueToken`;拆开的理由是「任何一侧泄露,另一侧同时失守」)。规则以 `ResolveSecrets` 为准:
  - 生产模式(`Mode` 不是 dev / test,空串也按生产)下 `Queue.Enabled=true` 就必须配 QueueToken,主密钥与附加密钥都要 ≥32 字节、不是占位串,且主密钥不得与 GateToken、InternalAuth 相同,否则 login 拒绝启动;
  - 只有宽松档(dev / test)在没配 QueueToken 时回落 GateToken 的密钥,并打一条 WARN;
  - 每把密钥是「主密钥 + 只验不签的 `Additional`」,支持三段式不停服轮换(新密钥先进 Additional → 提升为主密钥 → 摘掉旧密钥)。
  - K8s 上由环境变量 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET` 注入,见 §10「K8s 上的取值」。
- **过期检查**:`ParseAndVerifyQueueToken` 看 `e` 字段,过期立返 EXPIRED,不查 Redis。
- **伪造防御**:HMAC + 1h TTL。即使攻击者拿到队列容量信息,也无法伪造 queueId 跳过排队(签名匙在服务端)。
- **(2026-10-18 更正,上面「过期检查」与「HMAC + 1h TTL」;原文保留;未编译、未测试)**
  - **令牌过期改为 24h。** 令牌里的 `e` 现在是入队时刻 + 24h(`loginqueue.queueTokenMaxAge`,代码常量,不是配置),不再等于 `QueueEntryTTL`。超过它时 handler 在查 Redis 之前回 EXPIRED(`ttl`)。
  - **空闲过期改由 Redis 管。** 由 meta、token 索引与队列键的空闲 TTL 判定(至少 `QueueEntryTTL`、至多 1.25 倍,§5 决策树后的 2026-10-18 更正)。
  - **伪造防御不变。** 令牌离开 Redis 里的条目就没用,实际可用期 = min(24h, 条目存活),所以延长令牌过期不扩大被截获令牌的危害面。
  - **zone 来自令牌。** `Lookup` 用验签得到的令牌里的 zone 定位队列键,并与 meta 的 `zone_id` 对账,不符回 EXPIRED(`meta_gone`)。
  - **要同步的地方。** 改 `queueTokenMaxAge` 时,要同步改 `ValidateTTLs` 的上界与本节。
  - **密钥轮换的第 ③ 步要多等(2026-10-18 补注)。** 令牌只在入队时签一次,排队期间不换签。此前第 ③ 步(摘掉旧密钥)在第 ② 步(提升新密钥开始签)全量生效后等一个 `QueueEntryTTL` 即可;现在持续轮询的人能拿着旧密钥签的令牌排到 24h,第 ③ 步须在第 ② 步全量生效后至少等 `queueTokenMaxAge`(24h)。要提前,须确认各 zone 队首的入队时刻(`ZRANGE queue:zone:<z> 0 0 WITHSCORES`,分数 = 入队毫秒)都晚于第 ② 步全量生效时刻,且已过一个 `AdmitTTL`(已放行、未取走放行槽的人也持旧令牌)。否则在排玩家会按 `bad_signature` 回 EXPIRED(410)、丢掉排位,指标上记在 zone=unknown 下,与伪造流量混在一起。K8s 档位是单步轮换(只写 `Value`),本来就会让在排玩家重新入队(`open-server-rate-limit-design.md` §11.3 第 7 条),不受此条影响。`config/secrets.go` 包注释、`login.yaml` 的 Secrets 段与 `ParseAndVerifyQueueTokenMulti` 注释里的轮换说明尚未同步这一句,登记为后续项。

---

## 7. Dispatcher Leader 选举

复用现有 `pkg/locker.RedisLocker`:

```go
// 锁键 = <Queue.DispatcherLockKey>:zone:<Node.ZoneId>,每个 zone 一把(§7.1)
lock, _ := rl.TryLock(ctx, ZoneDispatcherLockKey(cfg.DispatcherLockKey, zoneID), 30*time.Second)
if lock.IsLocked() {
    stopHeartbeat := lock.StartHeartbeat(10*time.Second, 30*time.Second, onLost)
    drainLoop(ctx)  // 1s 一拍出队
    stopHeartbeat()
}
```

- 同 zone 的非 leader 副本每 10s `TryLock` 重试,被动备份
- Lost lease(Redis 抖动 / 手动 kill)→ heartbeat 触发 `drainCancel`,goroutine 退出
- 新 leader 在 ≤ TTL + TTL/3(约 40s)内接管:leader 被 kill -9 时锁剩余 TTL 在 20~30s 之间(心跳每 10s 续到 30s),键过期后备机最多再等一个 10s 重试周期。期间 admit 暂停,客户端继续轮询不报错(只是 rank 不动)
- 任期内 panic(出队路径的代码缺陷,2026-10-14 起):一个任期(`leadOneTerm`)的收尾——停心跳、leader 指标归 0、放锁——全走 defer,panic 展开时照样执行;safego 按任期接住(点位 `login.queue_dispatcher_term`)后本副本退避 10s 再参选,同 zone 备机可以接手。此前收尾是直线代码,panic 后心跳 goroutine 还在续期,副本成了「持锁不出队」的僵尸 leader,该 zone 队列只进不出
- leader 指标按电平重发:leader 每拍重发 1,备机每轮抢锁失败重发 0。login 起服时 dispatcher 早于 go-zero 指标开关打开就已当选,那时写的样本被丢弃,只在边沿写一次的话冷启动后整个任期没有这条序列

**为什么不用 etcd 选举**:RedisLocker 已在项目里跑了几个月没翻车,加 etcd lease 是重复造轮子。RedisLocker 唯一弱点是 Redis 主从切换时短暂双 leader,但本场景下双 leader 的最坏情况是 admit 多发一批(玩家更早进游戏),不是数据损坏。

### 7.1 为什么按 zone 分锁(G1b,2026-10-12)

**缺陷**:锁的范围和出队范围原来由两个独立的输入决定,两者对不上。

- 锁键取配置 `Queue.DispatcherLockKey`,默认 `dispatcher:lock:login_queue`,全服只有一把。
- 出队范围是 `activeZones`。它的注释说取「有 gate 的 zone ∪ 非空队列的 zone」,实现却只遍历 GateWatcher 的节点;而 GateWatcher 的 etcd 前缀带本进程 `Node.ZoneId`,所以结果只有本 zone。
- 就算 leader 去遍历别的 zone 也出不了队:候选 gate 按 zone 过滤,本进程只看得见本 zone 的 gate,`FreeSlots` 对外 zone 报 `no gates for zone`。
- 网关按 zone 把 AssignGate 路由到该 zone 的 login,`queue:zone:Z` 只会由 zone Z 的 login 写入。

多个 zone 共用一个 Redis 时(K8s 下所有 zone 的 login 都连同一个 Redis),只有抢到锁的那个 zone 出队,其它 zone 的 login 每 10s 重试一次、永远不出队。快速通道要求队列为空,所以非 leader zone 只要有一个人入队,之后进来的人全部入队,而每次 ZADD 都刷新 ZSET 的 TTL,队列永不过期——整个 zone 被堵死。

**修法**:`Dispatcher` 只认一个 `zoneID`(本 login 的 `Node.ZoneId`)。出队范围用它,锁键也从它派生:`<DispatcherLockKey>:zone:<zoneID>`(`loginqueue.ZoneDispatcherLockKey`)。每个 zone 一个 leader,两种范围在结构上不会再对不上。配置键名和默认值不变(`DispatcherLockKey` 的语义从「锁键」变为「锁键前缀」);`Queue.Enabled=true` 时 `Node.ZoneId` 为 0 或前缀为空,login 起服即失败(开了排队却出不了队 = 队列只进不出)。客户端契约不变。

**为什么不选「全局 leader + activeZones 覆盖所有 zone」**:要让 leader 出得了别的 zone 的队,它得同时具备 (a) 跨 zone 的 gate 视图(另起一个不带 zone 的 etcd watch,并和 `buildGateCandidates` 的过滤、去重保持一致);(b) 所有 zone 的 `ZoneCapacityOverride`,而这个配置是按 zone 分开下发的;(c) 接受「一台 login 挂了,全服排队停摆约 30s」的跨 zone 耦合。按 zone 拆锁三样都不需要,改动也最小。Java 版的全局 leader 遍历全部 OPEN 区、gate 来自共享 Redis 目录,锁范围与出队范围本来一致,不受此缺陷影响。

**同一 zone 的副本必须配同一个 `DispatcherLockKey`**:配得不一样就是两把互不相干的锁,该 zone 长期双 leader(修复前后都如此)。不同 zone 可以同值,也可以不同。

**滚动升级(新旧锁键并存窗口)**:老版本 leader 持全服旧键、出它所在的 zone Zo;Zo 里已升级的新副本抢 `…:zone:Zo`。两把锁不互斥,Zo 会短暂出现两个 leader;其它 zone 的新副本从升级那一刻起就恢复出队。两个 leader 同时出同一个 zone:

1. **不丢人、不重复放行**:`ZPOPMIN` 原子,每个 queueId 只会被一个 leader 弹出;挑 gate 失败时放回的只是自己弹出的那些。这里的「不丢人」是指双 leader 不比单 leader 多丢——`PopAdmit` 既有的几条丢条目路径(`pickFn` 返回 `(nil, nil)`、`json.Marshal` 或写 admit 的事务失败)单 leader 下同样存在。
2. **超发有界**:每个 leader 都按自己读到的在途占位数算余量,自己上一拍写入的占位在下一拍读数之前已完成,所以每个 leader 的在途数 ≤ cap − online,两个合计 ≤ 2×(cap − online)。与上文「双 leader 最坏是 admit 多发一批」的判断一致。
3. **消除窗口的办法(推荐)**:当前所有环境的排队都是关的。**在 `Enabled=false` 下把本修复滚完,再打开排队**,根本不会出现并存窗口(此时也不构造 dispatcher,`login_queue_dispatcher_is_leader` 一条序列都没有,新旧标签集不会并存)。偏要在 `Enabled=true` 下滚动时,旧 pod 的序列在 `sum by (zone)` 里归到 `zone=""` 一组,这组触发告警属于预期。老版本不再续期后,全服旧键 30s 内自然过期,不需要人工清理。

由 `TestIntegration_DualLeadersSameZone_NoLossBoundedOverAdmit` 钉住 1、2 两条。

> **(2026-10-18 更正,上面第 1 条;原文保留;未编译、未测试)** 「`ZPOPMIN` 原子,每个 queueId 只会被一个 leader 弹出;挑 gate 失败时放回的只是自己弹出的那些」改读如下(§5 决策树后的 2026-10-18 更正)。
> - **只写一次。** 两个 leader 可能 `ZRANGE` 到同一批。放行脚本先 `ZSCORE` 判成员仍在队列,并在同一段里 `ZREM`,所以每个 queueId 只被写一次,后到的拿到 GONE。
> - **挑 gate 失败。** 条目只被跳过、从未离开队列,不再有「放回」。
> - **不丢人。** 后半句列出的「既有丢条目路径」已全部消除,所以双 leader 与单 leader 一样不丢人。
> - **第 2 条不变。** 超发上界仍是 ≤ 2×(cap − online)。
> - **用例。** `TestIntegration_DualLeadersSameZone_NoLossBoundedOverAdmit` 的快照改用 `TxPipelined` 原子地读两个 `ZRange`:条目现在原子地从队列移进占位,非原子快照会看到同一条出现两次。另有 `TestPopAdmit_OverlappingLeaders_SameEntryAdmittedOnce` 钉住「只写一次」。
> - **混跑。** 本版本与旧版混跑(排队开着)时「不丢人」不成立,只能排队关着滚,见 §5「新旧版本混跑与部署顺序」后的 10-18 更正。

**前提:锁与它保护的队列在同一个 Redis 实例上**。以后按用途拆 Redis 时,leader 锁必须跟着排队键走;拆开后 Redis 抖动会出现「锁还在、队列读不到」或反过来的组合,上面的有界分析就不成立。按 zone 分锁本身与「每 zone 一个 Redis」「全服共享一个 Redis」两种拓扑都兼容。

**zone-down 与合服后旧 zone 的队列**:本修复不产生新的残留。源 zone S 的 login 下线后不再有人 ZADD,`queue:zone:S` 在最后一次入队后 `QueueEntryTTL`(1h)内过期,`admit:*` 在 `AdmitTTL`(60s)内过期,`admitted:zset:zone:S` 在最后一次写入后 `AdmitTTL + 1min`(默认 2 分钟)内整键过期,锁键 30s 内过期。目标 zone T 的 leader 只出 `queue:zone:T`;S 的排队玩家重新走 AssignGate 时按角色列表给出的归属 zone 进 T 的队,排队位次不迁移(§14 第 4 条)。合服回退把 S 拉起来后,S 的新 leader 自然接手。要立即清理见 §15。

> **(2026-10-18 更正,上段 `queue:zone:S` 的过期时长;原文保留;未编译、未测试)** 队列键、meta 与 token 索引现在是空闲 TTL:排队中的查询会把它们续到 1.25×`QueueEntryTTL`,入队对队列键只增不减。所以「在最后一次入队后 `QueueEntryTTL`(1h)内过期」改读:在最后一次入队或排队中查询之后,至多约 1.25×`QueueEntryTTL` 内过期(§15 的 10-18 补注)。其余键的时长不变。

**停机空窗(未核实,既有行为)**:SIGTERM 路径能否走到 `Dispatcher.Stop()` 主动释放锁尚未核实(login 与 scene_manager 的注释说法冲突)。最坏情况是锁按 TTL 过期,滚动升级时该 zone 最长约 40s(TTL + 备机重试周期 TTL/3)没有 leader,期间不放行、不丢人。修复前(全服锁)也是这样,不是 G1b 引入的回归。

---

## 8. 重连/顶号豁免

`enter_gs_type` 表(参见 [player_login_flow.md](./player_login_flow.md))规定:`REPLACE`(顶号)/`RECONNECT`(30s 重连)的玩家 **不应排队**——他本来就在游戏里,只是断线重连一下。

实现:`AssignGateLogic.lookupSessionByAccount(account)`
1. 从 Redis `account_data:{account}` 拿到 player_id 列表
2. 调 `sessionmanager.GetSession(playerId)` 查 player_locator
3. 若 `existing.GateID != ""` → 跳过排队,fast path

**已知妥协**:目前账号→player_id 取列表第一项。多角色账号需要扩展(查 `playerId` 列表里哪个有 ONLINE/DISCONNECTING session),不影响本期发布,留 TODO。

---

## 9. 客户端协议

```
AssignGate(account, deviceId, zoneId)
   ├─ status=0:连 gate(payload, signature)
   ├─ status=1 + queueSource="login":每 retryAfterMs 调 QueryQueueStatus(queueToken)
   │     ├─ status=0:连 gate
   │     ├─ status=1:UI 更新 rank/total
   │     └─ status=3:丢 queueToken,重调 AssignGate
   ├─ status=1 + queueSource="ratelimit":sleep retryAfterMs,重调 AssignGate(无 queueToken)
   └─ status=2/3:UI 显示错误或重新走 AssignGate
```

robot 的 queue-aware 实现见 `robot/main.go::assignGateHTTPLocal` + `robot/http_assign_gate.go::AssignGateWithQueue`。

> **(2026-10-15 更正,上面协议树原文保留)** 上树是排队初版的口径,与负责人 2026-10-15 定的客户端契约有三处出入,以 `docs/design/open-server-rate-limit-design.md` §11.5 的 2026-10-15 更正与补注为准:
> - 「每 retryAfterMs 调 QueryQueueStatus」改为:每次等待 max(retryAfterMs, 2s) + [0, 20%] 抖动;QueryQueueStatus 连续失败时下限从 2s 起翻倍,封顶 30s,成功一次即复位。
> - 持 queueToken 时,只有 QueryQueueStatus 本身失败(网络错误 / HTTP 非 200 / gateway `code=500`)才带同一 token 重调一次 AssignGate 作恢复。重入回 `queueSource="ratelimit"` 或 429 时保留 token、回到轮询,不退避重调。
>   上树 `queueSource="ratelimit"` 一支(重调 AssignGate、无 queueToken)只适用于还没拿到 token 的请求。
> - gateway 区服准入回 503 zone_* 或 404 zone_not_found(上树未列)是终态:不重入,按错误结束。status=3(gateway 回 410)仍是丢 queueToken、不带 token 重调 AssignGate,同上树。
>
> 上一行 robot 的实现由另一名代理同批改为此口径;本注写下时未核对,未编译、未运行。
> (2026-10-16 核对补注)主会话已静态核对 robot 实现与上述口径一致(见设计稿 §11.5 补注的「落地状态」),仍未编译、未运行。

---

## 10. 配置(login.yaml `Queue` 段)

```yaml
Queue:
  Enabled: false                    # kill switch,默认关
  DispatchInterval: 1s              # dispatcher 出队节奏
  AdmitTTL: 60s                     # admit token 客户端要在此 TTL 内 GETDEL,也是没人来取时在途占位的寿命;
                                    # 取走后占位只再留 20s(代码常量 loginqueue.defaultHandoffGrace,不是配置;10-15 前为 15s);
                                    # 排队开着时也可以滚动调整(整键 TTL 只延长不缩短,§5)
  QueueEntryTTL: 1h                 # ZSET / meta / token-index 的 TTL 上限
  SoftCapMultiplier: 1.5            # 没配 ZoneCapacityOverride 时,用 max(online,1) * mult
  DefaultRetryAfterMs: 2000         # 客户端轮询间隔建议
  DispatcherLockTTL: 30s            # leader 锁 TTL
  DispatcherLockKey: "dispatcher:lock:login_queue"  # 锁键前缀;实际 = <前缀>:zone:<Node.ZoneId>,每 zone 一个 leader
  ZoneCapacityOverride:             # 推荐生产配死,不靠 SoftCapMultiplier 估算
    "1": 5000
    "2": 5000
```

> **(2026-10-18 更正,上面 `QueueEntryTTL` 一行的注释;原文保留;未编译、未测试)** 「ZSET / meta / token-index 的 TTL 上限」改读如下。
> - **空闲 TTL。** meta、token 索引与队列键自最后一次成功的排队中查询(或入队)起算,至少保留 `QueueEntryTTL`、至多 1.25 倍。持续轮询的人不会因为等满 1h 被弹回队尾。
> - **取值。** 须在 [1s, 24h],否则起服失败。
> - **与令牌脱钩。** 它不再决定排队令牌的过期,令牌固定 24h(`loginqueue.queueTokenMaxAge`,§6 的 10-18 更正)。
> - **发布约束。** 本版本以及从它回滚,都必须在 `Enabled=false` 下滚完。`go/login/etc/login.yaml` 的 `Queue` 段与 `config.go` 的 `QueueConf` 注释已写同样的补注。
> - **K8s。** 镜像原样带过 `QueueEntryTTL` 的值,不受影响。

**K8s 上的取值(G4b,2026-10-14;已落码,未编译、未测试,待 Codex 验证)**:`tools/scripts/k8s_deploy.ps1` 按 `-LoginQueue auto|on|off` 决定 login ConfigMap 写不写本段。
缺省 `auto` 跟随 `-ReleaseProfile`:dev 不写(go-zero 零值 `Enabled=false`,产物与 G4b 之前逐字相同),staging / prod 写。写的时候:

- `Enabled: true`;`DispatchInterval` / `AdmitTTL` / `QueueEntryTTL` / `SoftCapMultiplier` / `DefaultRetryAfterMs` / `DispatcherLockTTL` 从 `go/login/etc/login.yaml` 原样镜像。
- `DispatcherLockKey: "dispatcher:lock:login_queue:zone:<本 zone>"`:写进 ConfigMap 的前缀已经带本 zone,login 再按 §7.1 拼一次 `:zone:<Node.ZoneId>`,
  所以 **K8s 上本 zone 的实际锁键是 `dispatcher:lock:login_queue:zone:<id>:zone:<id>`**,选主日志 `[loginqueue] elected dispatcher leader zone=<id> key=…` 里打的也是它。
  每个 zone 仍恰好一把锁;§3、§15 与 `docs/ops/login-queue-stress-runbook.md` 写的 `dispatcher:lock:login_queue:zone:<id>` 是本地 / compose 默认配置下的键名,在 K8s 上排查时按带两段 zone 的键查。
  显式带 zone 是纵深防御:`kubectl rollout undo` 把 login 退回 §7.1 之前的镜像、而 ConfigMap 仍是开排队的这一份时,旧镜像原样使用配置值,照样按 zone 隔离,
  不会退回「全集群一个 leader、其它 zone 只进不出」。login 若改成「一个 leader 覆盖全部 zone」,这一行必须删掉,否则 N 个 leader 同时出队。
- `ZoneCapacityOverride` 只写本 zone 一项,开服档起步值 `5000`(**待压测确认**;单一来源是 `k8s_deploy.ps1` 的 `$script:PeakShavingLaunchTier.LoginQueueZoneCapacity`)。
  每个 zone 的 login 只出队本 zone,用不到别的 zone 的值。必须配:不配会落到 `max(online, 1) × SoftCapMultiplier` 的回落公式,代码里是向下取整
  (`loginqueue/capacity.go`),在线 0 / 1 人时容量只有 1,队列非空后可能长期放不出人。它是**同时在线上限**,不是速率:对已在运行的 zone 首次打开排队前,
  先核对该 zone 当前在线(各 gate 自报的 `player_count` 之和)低于这个值,否则新登录全部入队,直到在线降下来。
- `Secrets.QueueToken.Value` = 环境变量 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`(staging / prod 必填:≥32、非占位、与 `MMORPG_GATE_TOKEN_SECRET` / `MMORPG_INTERNAL_AUTH_SECRET` 不同,
  不满足在部署的任何写操作之前拒绝;dev 可不设,此时不写、login 回落 GateToken,见 §6 更正)。写明文 `Value` 而不是 `secrets.go` 建议的 `Env`,与兄弟项 `InternalAuth` 一致。
- 开关、`Queue` 段与排队密钥都进 login Pod 模板的 `mmorpg.io/peak-shaving-hash` 注解,任一变化 login 自动滚动。参数与运维要点见 `deploy/k8s/README.md` Optional Flags「削峰(G4b)」。

> **(2026-10-15 更正,本块原文保留)** 上面「staging / prod 写」只在登录排队的合入门槛清掉之后成立。`k8s_deploy.ps1` 用脚本常量 `$script:LoginQueueMergeGateCleared`(当前 `$false`)机械拦截:门槛未清时 `-LoginQueue auto` 在 dev / staging / prod 都解析为关,不写本段与 `Secrets.QueueToken`,也不要求 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`(非 dev 档部署 login 时打 `[PeakShaving]LoginQueueGate=closed` 警告);`-LoginQueue on` 在参数解析后、任何 kubectl 之前拒绝。三项门槛(G1b 多 zone 用例跑绿、G4b-G 跑绿、`spec_G4b-G.md` §8.4 真实 Redis 冒烟记进 `docs/PROGRESS.md`)见 `docs/handoff/launch-readiness-20261013/spec_G4b.md` §7「合入门槛」与 `deploy/k8s/README.md`「登录排队的合入门槛」;§14 第 8 条的「任何环境都不打开排队」就是由这道门禁执行的。

---

## 11. 观测与压测

### robot 周期 stats(`[stats ...]` 日志)

新增 5 个字段:
```
q_entered=4080 q_admitted=920 q_expired=3 q_avg_wait=28s q_max_rank=4078
```

### 健康范围速查

| 指标 | 健康 | 红线 |
|---|---|---|
| q_admitted 增速 | 上界是在线玩家的离开速率,加上拿了票没留在线上的那部分名额每约 22~23s 周转一次(§5「放行节奏的上界」;交票后宽限 20s,10-15 前按 15s 是约 17s;数字待 L-14 标定) | `在线 + login_queue_admitted_inflight ≥ capacity` 时不涨是正常回压;停滞且同时满足 `在线 + 在途 < capacity` 才判 dispatcher 卡死 |
| q_expired / q_entered | < 1% | > 5% → AdmitTTL 太短或客户端轮询慢 |
| q_avg_wait | < 60s(待 L-14 标定:放行速率受上一行的上界约束,不是固定的 admit 速率) | > 5min → dispatcher 落后 EnterGame |
| q_max_rank | 早期升高,后期单调下降到 0 | 不下降 → 该玩家卡住 |

详细判读见 [docs/ops/login-queue-stress-runbook.md](../ops/login-queue-stress-runbook.md)。

### Prometheus 指标(2026-05-14 已接入)

通过 go-zero 内置 `/metrics` endpoint 暴露,详细告警/看板 PromQL 见 [login-queue-stress-runbook.md](../ops/login-queue-stress-runbook.md) §"Grafana / Prometheus 指标"。

| 指标 | 类型 | 标签 |
|---|---|---|
| `login_queue_size` | Gauge | zone |
| `login_queue_admitted_inflight` | Gauge | zone |
| `login_queue_wait_seconds` | Histogram | zone |
| `login_queue_admit_total` | Counter | zone |
| `login_queue_expired_total` | Counter | zone, reason |
| `login_queue_dispatcher_is_leader` | Gauge | pod, zone |

> **(2026-10-18 补注,`login_queue_expired_total` 与 `login_queue_admit_total`;未编译、未测试)** 不新增指标,只改原因的语义,详见 runbook 指标表后的 10-18 补注。
> - **新增原因 `queue_gone`。** meta 在、没有放行、又不在队列,来源见 §5 决策树后的 2026-10-18 更正。持续非零,说明有人在排队开着时混跑新旧版,或手工动了队列键。与其它原因一样不预置标签(go-zero 指标开关打开前写的样本会被丢弃),面板用 `or vector(0)`。
> - **`meta_gone` 变宽。** 现在包含空闲超过 `QueueEntryTTL` 的条目(此前这类通常记 `ttl`,因为令牌与条目同为 1h),也包含 meta 类型损坏、meta 与令牌的 zone 不符。
> - **zone 标签。** 一律取自令牌(此前 token 索引不在时记 `unknown`)。
> - **`ttl` 变窄。** 只表示排队令牌超过 24h 的绝对过期。
> - **`login_queue_admit_total`。** `PopAdmit` 中途出错时,已放行的也计入;go-redis 重发、第一次已写成的放行(REPLAYED)也计入。

---

## 12. 实施回顾

按 plan 分了 4 个 PR,逐个独立可回滚:

| PR | 内容 | 测试 |
|---|---|---|
| 1 | proto 字段 + Java DTO 添加 | 仅字段添加,proto-gen.exe 后所有调用方自动可见 |
| 2 | go-zero `loginqueue` 包 + handler + dispatcher | 8 单元 + 3 集成,共 11 测试,< 1s |
| 3 | Java Gateway 改 gRPC client + 删本地 HMAC + QueueStatusController | mvn test 36/36 |
| 4 | robot queue-aware + Stats 5 个新指标 + 压测 runbook | go test PASS |

**前置工作**:`go/login/internal/logic/pregate/getgatelistlogic.go` 已删除——它是 2026-04 留下的孤儿(grep 零调用方),与 `loginpregate/assigngatelogic.go` 同时存在,功能重复。本次顺手收敛。

---

## 13. 与既有决策的关系

- **决策 #2(Java Gateway 选型)**:不变。Java 仍是 HTTP 入口,只是 AssignGate 的"决策"职责下沉到 go-zero,Java 退化为转发器
- **决策 #9(2026-05-08:Login 上移到 HTTP)**:本期延续——AssignGate 也从 Java 自签 HMAC 改成调 go-zero gRPC,**消除两份签名实现并存**(Java 之前自己实现了一份选 gate + 签 HMAC,本次删除)
- **决策 #13(Bucket4j 限流)**:**保留**,本期作为 Queue 的前置闸,不重叠
- **缺口 #10(开服削峰)**:升级状态从"Bucket4j 已落地"→"Bucket4j + 真排队都已落地"

---

## 14. 已知不足 / 后续工作

1. **多角色账号重连**:`lookupSessionByAccount` 仅取第一个 player_id,极少数多角色账号在罕见情况下可能被错排。修复成本低,等真有玩家投诉再补。
2. **Prometheus 接入**:列在 §11,本期未做,等首次生产灰度后再加。
3. **进场景排队**:本期不做,等真有"攻城战"等玩法时按同一套 ZSET 模板升级。详见对话记录中 §"为什么本期不做"。
4. **Queue token 转移**:玩家换设备重登拿到新 token,丢失原位置。目前可接受。
5. ~~**多 zone 公平性**:dispatcher 按 `activeZones` 顺序遍历,极端情况下前面 zone 抢光 admit 批次。~~ 已由 §7.1 消解:每个 zone 有独立的 leader,不存在 zone 之间互相抢批次。
6. **取走后再轮询回「排队中、名次 0」**(2026-10-13 登记,G4b-G 不含):同一令牌取走之后再轮询,meta 与 token 索引还在、ZRANK 落空,回 QUEUEING、rank 0,直到 1h 过期;回包丢失时这张 gate 票找不回来。Java 版回 410。这是 `PARITY.md`「登录排队」行 ③ 的另一句(`spec_G4b-G.md` §5.8 第 1 条)。
   - **(2026-10-15 更正:已修;已落码,未编译、未测试,待 Codex 验证)** 取走之后再查,以及放行槽没人来取、按 `AdmitTTL` 过期之后再查,都回 EXPIRED(gateway 410,原因 `admit_window_passed`),判据是 meta 里的 `admitted_at`,见 §5 决策树后的更正。回包丢失时 gate 票仍找不回来,客户端从队尾重新排。PopAdmit 写放行槽之前丢掉的条目仍停在名次 0 直到 meta 过期(第 7 条)。
7. **`PopAdmit` 不是单段 Lua**(2026-10-13 登记,既有):「FreeSlots 读占位数 → ZPOPMIN → 逐条写占位」是多次往返,一批把队列弹空之后、占位逐条写完之前,快速通道看到「队列空、占位还没写全」可以再占进来,任一时刻在途 ≤ 2 ×(capacity − online)。`pickFn` 返回 `(nil, nil)`、Marshal 失败、MULTI 失败时条目丢失(G1b 规格 §4.5 第 1 条)。Java 版把弹出、算空位、写占位放在一段 Lua 里(`spec_G4b-G.md` §5.4、§5.8 第 2 条)。
8. **在途占位脚本只有 miniredis 上的证据**(2026-10-15 登记):G4b-G 的计数、快速通道与 `admittedHoldScript` 三段脚本(含 10-15 的条件续期、MULTI 里的 `EVAL`)只有 miniredis 单测覆盖(单测本身也还待 Codex 执行),没有任何真实 Redis 上的运行证据。负责人 2026-10-15 定为**硬门槛**:G4b-2 打开排队之前,`spec_G4b-G.md` §8.4 的最小冒烟必须在真实 Redis 7.x 上跑通并记进 `docs/PROGRESS.md`,否则任何环境都不打开排队(`spec_G4b.md` §7 合入门槛)。
   - **(2026-10-15 补注,第 7、8 条;已落码,未编译、未测试)** 第 7 条:写放行改为单段脚本 `admitEntryScript`(见 §5 决策树后的补注),弹出之后的写入用 `WithoutCancel`,看到取消之后再给 3s 预算(没有取消时不截断),停服或丢租约不再丢整批;写入出错每批一行 ERROR。弹出本身仍不在这段脚本里,本条「不是单段 Lua」的其余结论(快速通道可再占进来、在途 ≤ 2×)不变。仍丢条目的路径:取消之后超出预算、Redis 出错、强杀、`pickFn` 回 `(nil, nil)`、Marshal 失败。其中 **noeviction 写满**会让每拍弹出的整批全部写失败、几拍之内把队列弹空、所有人停在名次 0 约 1h:`ZPOPMIN` 不带 DENYOOM 照常成功,写入脚本被拒。根治要把弹出与写入合成一段 Lua、用 `#!lua` 声明 flags 且不带 `allow-oom`(Redis ≥ 7,miniredis 不支持,只能在真实 Redis 上验证),**待负责人定**;上线前至少给 redis-queue 配内存水位告警。另:锁续期单次失败不会触发丢租约(`onLost` 只在超过 fence 窗口或属主已换人时调用)。第 8 条:真实 Redis 冒烟改为覆盖 `admitEntryScript`(直接 `EVALSHA` / `EVAL`,不再是 MULTI 里的 `EVAL`)。
   - **(2026-10-18 更正,第 6、7、8 条及上一条补注;原文保留;已落码,未编译、未测试,待 Codex 验证)** 完整说明见 §5 决策树后的 2026-10-18 更正。
     - **第 7 条已修。** 出队改为「`ZRANGE` + 每条一段先判在队的 `admitEntryScript`」,条目在放行写成之前不离开队列。本条与上一条补注列出的丢条目路径全部消除:`pickFn` 回 `(nil, nil)`、Marshal 失败、脚本出错、取消之后超出预算、写满、强杀。第 6 条更正末句「PopAdmit 写放行槽之前丢掉的条目仍停在名次 0」随之作废。
     - **「根治要用 `#!lua` 声明 flags」的前提作废。** 兼容模式(脚本无 shebang)下,写满时脚本照常开跑。只有当第一条写是 DENYOOM 命令、且之前没执行过任何写时,脚本才在那条写上被拒;一旦执行过任何写,后续命令不再受 maxmemory 约束。
       - 放行脚本据此把 `HSET`(DENYOOM)排成写段第一条,它之前只有只读命令。写满时整段被拒、什么都没写、条目留在队列,本身就是 fail-closed,不需要把弹出与写入合成「一段 Lua 出整批」。
       - 带 shebang 不带 flags 时,Redis 会在写满时整段拒绝,连 META_GONE / ALREADY 分支的清理也做不了。miniredis 也不解析 shebang。所以不用 `#!lua`。
     - **写满期间。** 放行停,dispatcher 每拍一行含 `OOM` 的 ERROR;查询照常(读状态脚本没有 DENYOOM 命令,轮询与续期不受影响);新入队整体失败(`StatusError`)。仍需给 redis-queue 配内存水位告警。
     - **快速通道插队窗口消失。** 快速通道看到的队列,在本批每条都原子地「移出 + 写占位」之前不会变空。本条「在途 ≤ 2×(capacity − online)」的上界只剩双 leader / 多副本重叠出队一个来源(§7.1 的 10-18 更正)。
     - **第 8 条。** 真实 Redis 冒烟扩为同时覆盖新的放行脚本与读状态脚本,并补以下几项,见 `spec_G4b-G.md` §8.4 的 2026-10-18 补注。它仍是打开排队前的硬门槛。
       - 写满:maxmemory 取小值 + noeviction 下,放行失败、条目仍在队列、名次不变、轮询续期仍生效、新入队失败。
       - 续期区间。
       - 入队的 NX/GT。
       - 毒丸 meta。
       - RESP3 下的各 `Lookup` 分支。
9. **排队超过 `QueueEntryTTL` 会被弹回队尾**(2026-10-15 登记,既有,**待负责人定**):meta、token 索引、排队令牌只在 `Enqueue` 时设一次 `QueueEntryTTL`(1h),等待中不续期。等待超过 1h 的玩家下一次查询回 EXPIRED(handler 在 `Lookup` 之前先校验排队令牌的 HMAC 过期时刻,令牌与 meta 同时到期,所以原因通常是 `ttl`,只有令牌按秒取整多出的不到 1s 内才是 `meta_gone`;监控看 `login_queue_expired_total{reason="ttl"}`),丢 token 从队尾重排;他原来的队列成员留在 ZSET 里,轮到时被 `admitEntryScript` 按 meta 不在丢弃(不再白占名额)。开服首日若等待可能超过 1h,要么调大 `QueueEntryTTL`,要么在查询时续期 meta / token 索引 / 令牌(需同步改令牌过期语义,Java 版对齐),由负责人决定。
   - **(2026-10-18 更正:已修,空闲 TTL + 24h 令牌;原文保留;已落码,未编译、未测试,待 Codex 验证)** 做法见 §5 决策树后的 2026-10-18 更正「三、空闲 TTL 与令牌」。
     - **怎么修的。** 排队中的查询在剩余 TTL 不足 `QueueEntryTTL` 时,把 meta、token 索引与队列键续到 1.25 倍。排队令牌的签名过期与 `QueueEntryTTL` 脱钩,固定 24h。
     - **效果。** 持续轮询的人不会因为等满 `QueueEntryTTL` 被弹回队尾(`TestLookup_ContinuousPollingOutlivesEntryTTL`:轮询 2h 仍在排队并被放行)。空闲超过 `QueueEntryTTL`(至多 1.25 倍)的条目回 EXPIRED,原因记 `meta_gone`(此前通常记 `ttl`);`ttl` 只在令牌超过 24h 时出现。
     - **孤儿条目。** 不再轮询的条目,轮到时由放行脚本的 META_GONE 分支 `ZREM`。
     - **Java 版。** 上文「需同步改令牌过期语义,Java 版对齐」:本仓库已改,Java 版待做(由主会话在 `PARITY.md` 登记)。

---

## 15. 故障应急

**关闭排队(秒级回滚)**:`login.yaml` 改 `Queue.Enabled: false`,所有玩家立刻走 fast path。

**K8s 上关闭排队(G4b 止血,2026-10-14;已落码,未编译、未测试,待 Codex 验证)**:不要手改 ConfigMap(进程只在启动时读,Pod 模板不变就不会滚动),也不要 `kubectl rollout undo`
(它只退 Pod 模板、不退 ConfigMap,排队照旧开着)。直接调 `k8s_deploy.ps1` 重跑该 zone 的部署,其余参数与上一次部署一致,加上 `-LoginQueue off`:

```powershell
# 先加 -DryRun 核对 "PeakShaving:" 那一行是 login_queue=off,再去掉 -DryRun 执行;完整参数说明见 deploy/k8s/README.md Optional Flags「削峰(G4b)」
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName <zone> -ZoneId <id> -ReleaseProfile prod `
  <该 zone 上一次部署的其余参数照传(镜像 tag 显式给线上那一版)> -LoginQueue off
```

- `dev_tools.ps1`、`k8s_image.ps1`、`k8s_zone_rollback.ps1` **不透传** `-LoginQueue`,包装入口上找不到这个参数。
- 关闭后 login Pod 模板上的 `mmorpg.io/peak-shaving-hash` 注解消失,login 自动滚动;所以 K8s 上的「秒级」是一次 login 滚动的时间。新 Pod 起来后,在排玩家轮询
  `/api/queue-status` 拿到 EXPIRED(gateway 回 410),丢掉 token 重走 `/api/assign-gate` 快速通道。关排队时部署不要求 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`。
- 参数不粘滞:下一次部署(含回档 Step 6、合服后的 zone-up)不传 `-LoginQueue off`,staging / prod 就按档位恢复为开。原因排除前每次部署都要照传。
  - **(2026-10-15 更正,上一条原文保留)** 「按档位恢复为开」只在登录排队的合入门槛清掉之后成立:门槛未清时(`k8s_deploy.ps1` 的 `$script:LoginQueueMergeGateCleared` 为 `$false`),不传 `-LoginQueue` 在任何档位都是关,`-LoginQueue on` 被拒,本节的止血场景也就不会出现。见 §10 末尾的更正与 `docs/handoff/launch-readiness-20261013/spec_G4b.md` §7「合入门槛」。
- 遗留的 `queue:*` / `admit*` 键按 TTL 自然过期,要立即清理见下面「清空堆积」。K8s 上锁键是 `dispatcher:lock:login_queue:zone:<id>:zone:<id>`(§10),
  下面命令里的锁键在 K8s 上要换成它。

**清空堆积**:

> 多 zone 共用 Redis 时,下面后三行(`admit:*` / `queue:meta:*` / `queue:token:*`)不分 zone,会清掉**全部 zone** 的放行槽和排队条目;只想清一个 zone 时只执行第一行,其余键按 TTL(60s / 1h)自然过期。

```
redis-cli DEL queue:zone:1 admitted:zset:zone:1
redis-cli --scan --pattern 'admit:*'        | xargs -r redis-cli DEL
redis-cli --scan --pattern 'queue:meta:*'   | xargs -r redis-cli DEL
redis-cli --scan --pattern 'queue:token:*'  | xargs -r redis-cli DEL
# leader 锁一般不用删(30s 自然过期);确需强制换主:
redis-cli DEL dispatcher:lock:login_queue:zone:1
# 合服后源 zone S 的队列残留会在 QueueEntryTTL(1h)内自行过期;要立即清:
redis-cli DEL queue:zone:<S> admitted:zset:zone:<S> dispatcher:lock:login_queue:zone:<S>
# 旧键 admitted:zone:* 是 2026-10-13(G4b-G)之前版本的残留,最后一个旧版本停写后 60s 内自行过期,不用删
```

玩家自动重试 `/api/assign-gate` → 重新入队(若 Enabled=true)或直接进游戏(若 Enabled=false)。

> **(2026-10-18 补注,本节;原文保留;未编译、未测试)** 队列键、meta 与 token 索引已是空闲 TTL(§5 决策树后的 2026-10-18 更正)。
> - **自然过期的时长。** 不手工清理时,这三类键在最后一次入队或排队中查询之后,至多约 1.25×`QueueEntryTTL` 过期。上文「清空堆积」提示里的「60s / 1h」与代码块里「合服后源 zone S 的队列残留会在 QueueEntryTTL(1h)内自行过期」相应改读「60s / 至多约 1.25h」「至多约 1.25×QueueEntryTTL 内」。
> - **`DEL queue:zone:<z>` 之后的客户端。** 仍在轮询的客户端,下一次查询立即收到 EXPIRED(`queue_gone`,gateway 410),丢掉 token 重走 `/api/assign-gate`。此前回「名次 0」,一直等到 meta 过期。`login_queue_expired_total{reason="queue_gone"}` 随之出现一个尖峰,属预期。
> - **只删队列键、不清 meta 时。** 不在队列的条目不再续期,meta 与 token 索引按剩余 TTL 自然过期。
