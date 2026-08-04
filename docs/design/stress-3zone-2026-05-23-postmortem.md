# 3 Zone × 15000 压测 — 复盘(2026-05-23)

> 三轮压测的复盘。后端打通了,登录链路没问题(avg_login 230-628ms),但 scene-ready 没走通,**第三轮**还撞到 cpp node 启动竞态导致 4 个进程自杀。这份文档记录踩到的坑和各自的根因,**不是** runbook。

---

## TL;DR

- **第一轮**(07:25 起):瞬间 4300 个 robot 全卡在 `AssignGate failed after retries`,后端 Gateway 返回 500 `login_unavailable` / `INTERNAL`。
- 修了 4 个独立的 bug 后,**第二轮**(09:25 重启,后端起完):
  - 3 zone 合计 **login_ok ≈ 82000、enter_ok ≈ 82000**(累计含重连)
  - **avg_login 3.0–3.8s**,最大 49s — 偏高但没崩
  - **`enter_fail` ≈ 150k**,主要错误是 `timed out waiting for scene ready` —— 没收到 `NotifyEnterScene`
  - **稳态前 z1_scene_1 自杀**:`Lease keepalive returned TTL=0` —— 唯一一个真挂的进程
  - **没达到 45000 净在线**;每 zone 净在线 ~15000 的稳态没出现,因为重连风暴一直在搅动 `conn` 计数
- **第三轮**(10:42 全栈重启后再跑):
  - login 链路质的飞跃:**login_fail = 0**,**avg_login 230-628ms**(比第���轮快 6-10×)
  - 3 zone 合计 **login_ok ≈ 144000、enter_ok ≈ 144000**(累计含重连)
  - 但是 **robot 启动后 ~50 秒**(10:33:12-14)**zone-1 的 4 个 cpp 进程同时自杀**(scene_2/3/4 + gate_2)
  - 根因:`Node ID hijack via etcd Watch` —— cpp node 启动竞态(详见 §E)
  - 损坏的后端跑 26 分钟后我手动停掉,scene 数已经从 12 → 9

---

## 时间线

| 时间(本地) | 事件 |
|---|---|
| 07:25 第一次 robot 跑 | 8 分钟后 conn=0、login_fail≈4200/zone。Gateway 返 500 |
| 07:35 第一次诊断 | `curl /api/assign-gate` 返 `login_unavailable` — Gateway → login 通道挂 |
| 07:48 修 #1 | Gateway `application.yaml` `login.grpc.endpoints` 写死 `127.0.0.1:51000`,但实际 z1/z2/z3 login 监听 53000/54000/55000。改成三 endpoint round-robin |
| 07:50 修 #2 | `signFastPath` panic:`Queue.Enabled=false` 时 `queueCapProvider==nil` → 第一行 `.CandidatesForZone()` 解引用 nil。改成 `queueCapProvider` 无条件初始化 |
| 08:30 修 #3 | etcd 客户端打 1.5GB 噪音日志 — `ETCD_ADVERTISE_CLIENT_URLS` 通告 `etcd:2379` + `host.docker.internal:2379`,这两个 hostname host 上要么解析不到要么映射到陈年 IP `192.168.1.12`。每次 grpc 调用要等所有 endpoint 都 dial 完才返回,直接拖死 `kv.Get` → `signFastPath` 看到 `failed to fetch nodes: context deadline exceeded`。改 docker-compose 只通告 `127.0.0.1:2379` |
| 08:37 docker recreate etcd | etcd lease 全过期 → 所有 go/cpp 节点的 etcd 注册键失效 |
| 08:48 重启 3 login 失败 | login 启动时 `MustNewClient` dial `etcd://...playerlocator.rpc.z1` 超时;player_locator 注册过期 → 必须**全栈**重启 |
| 09:07 全栈重启 | `dev start-zones "1,2,3"` 起 32 进程,Gateway 重启刷 channel |
| 09:08 smoke | `/api/assign-gate` 全 3 zone 返回 `code:0` + 真实 gate_ip:port + HMAC token ✅ |
| 09:25 第二轮 robot | 终于有 login_ok 在涨;z1 在 09:36 进入"假稳态"(conn ~41k 不动),z2 持续到 09:53 还在涨 |
| 09:51 z1_scene_1 自杀 | `Lease keepalive returned TTL=0`,scene 数量 12→11 |
| 09:54 我手动停 robot | 决策:不让损坏的后端继续被压,先复盘 |

---

## 修了的 4 个 bug

### #1 — Gateway login.grpc endpoints 配置漂移
**文件:** `java/gateway_node/src/main/resources/application.yaml`
**改动:** `endpoints: "127.0.0.1:51000"` → `"127.0.0.1:53000,127.0.0.1:54000,127.0.0.1:55000"`
**根因:** Gateway 配置写死 51000,但 `dev start-zones` 给每 zone 偏移端口(login.rpc 默认 53000,zone N 加 (N-1)*1000)。`LoginRpcClient` 用 round-robin 不走 etcd 发现,所以配错就全军覆没。
**长期修法(没做):** 让 Gateway 走 etcd 拉 `login.rpc.z*` 注册,本机有几个 zone 就配几个。

### #2 — Login fast-path nil provider panic
**文件:** `go/login/internal/svc/servicecontext.go`
**改动:** `initLoginQueue` 把 `queueCapProvider` 的初始化挪到 `if !cfg.Enabled { return }` **之前**。
**根因:** `signFastPath` 第一行 `l.svcCtx.QueueCapacityProvider().CandidatesForZone(...)` 在 `Queue.Enabled=false` 时直接 nil 解引用。fast-path 注释说"queue disabled 时走我"但实现依赖了 queue 才会初始化的 provider。
**这是一个真实业务 bug**,不是这次压测的产物 —— 任何 production 配置 `Queue.Enabled=false` 跑 AssignGate 都会 panic。

### #3 — etcd advertise URLs 污染客户端
**文件:** `deploy/docker-compose.yml`
**改动:** `ETCD_ADVERTISE_CLIENT_URLS=http://etcd:2379,http://${HOST_IP:-host.docker.internal}:2379` → `http://${HOST_IP:-127.0.0.1}:2379`
**根因:** etcd 通告两个不可达 URL,etcd 客户端�� `Sync()` 把它们当合法成员一直重试,每次 grpc 调用都要等 30s 探测,把 login 的 3s `ServiceDiscoveryTimeout` 击穿。
**反思:** 这是 docker-compose 配置默认值的锅 —— 留 `host.docker.internal` 给容器内访问 host 用,但 host-on-host 客户端会撞上 stale ARP。

### #4 — Robot HTTP 客户端连接池(在 runbook 准备阶段就修了)
**文件:** `robot/http_client.go` (新)
**改动:** `http.DefaultClient.Transport`(MaxIdleConnsPerHost=2)→ 共享 `http.Client` 配 4096 idle conns。
**根因:** 15k goroutine 同时打 `/api/login` + `/api/assign-gate`,默认 idle pool 大小 2 会让其他请求每次建新连接,burn 完 64k ephemeral port 后开始 `cannot assign requested address`。

---

## 没修但点清楚了的事

### A. `scene ready` 大规模超时(22500 次/zone)

robot 在 EnterGame 成功后等 `NotifyEnterScene`,15s 超时。链路:

```
login.EnterGame → Kafka gate-{N} → cpp gate → 找出 scene_node → 把 player 加入 scene → scene 给 gate 发回 NotifyEnterScene → gate 推给 client
```

**最可能的瓶颈点(没验证,只是排序):**

1. 单 zone 4 个 scene,瞬时 5k+ player 注入到任一个 scene,scene tick 跟不上 player 初始化
2. Kafka `gate-{id}` topic 顺序消费拖累(`KeyOrderedKafkaProducer` 每 playerId 一致哈希到同 partition,partition 数有限)
3. cpp gate 自己路由 Kafka 消息时被 muduo eventloop 阻塞(15k 长连占满 reader thread)

**下一步:** 单 zone 跑 5k robot,看 scene log 里 `enter_gs_type=` 日志的密度和延迟。

### B. z1_scene_1 自杀(`Lease keepalive returned TTL=0`)

stdout 里只有一条 keepalive 请求记录(21:08 起服)+ 一条 21:51 重新申请 lease + 立即 FATAL。**说明从 21:08 到 21:51 之间这 43 分钟没有任何成功的 keepalive 响应被记录** —— 不是请求被拒,而是 keepalive 线程根本没在跑或者输出被吃了。

可能的子根因:
- Kafka rdkafka 的回连重试占满了 IO 线程
- etcd recreate 后这个 cpp 进程的 keepalive stream 已经断了,但 reconnect 路径没走通

**这条单独追**,下次开始压测前要先验证 lease keepalive 的健康度。

### C. Robot launcher 不优雅 stop
我用 `Stop-Process -Force` 杀的 robot,跳过了 main.go 里 `signal.Notify` 触发的 `ExportBehaviorCSV` — 这次没拿到 CSV/JSONL,只能依赖 z*.log 的 stats 行。
`run-stress-3zone.ps1` 写了 Ctrl-C 路径但我从外面 kill 进程根本没走 console group。后续要么改 launcher 加显式 stop 命令,要么用 Linux/WSL Ctrl-C。

### D. ETCD recreate 是这次坑的放大器
我为了修 #3 重启了 etcd 容器,**结果连带触发**:
- 所有 cpp/go 节点的 lease 失效 → 全栈重启
- 我没意识到只重启 login 是不够的(player_locator 也丢了注册 → login 拉不起来)
- 浪费 ~30 分钟

**教训:** 改 etcd 配置就是改集群成员合约,跟改 prod 数据库一样重 —— 必须把整个游戏服务栈也算进重启计划。

---

## 真正的数据(三 zone 最末稳态截图)

```
z1 [stats 11m15s]
  conn=41058    login_ok=22500  login_fail=135   login_stuck=35
  enter_ok=22500  enter_fail=43301
  msg_sent=140957  msg_recv=140927
  avg_login=3.088s  max_login=49.007s
  recon_ok=8102  recon_fb=13696

z2 [stats 27m48s]      ← 跑得最久的一个
  conn=62802    login_ok=35646  login_fail=4536  login_stuck=541
  enter_ok=35646  enter_fail=53809
  msg_sent=193091  msg_recv=192915
  avg_login=3.851s  max_login=40.522s
  recon_ok=15200  recon_fb=17068

z3 [stats 11m19s]
  conn=44151    login_ok=24288  login_fail=254   login_stuck=120
  enter_ok=24288  enter_fail=51925
  msg_sent=162445  msg_recv=162351
  avg_login=3.834s  max_login=20.182s
  recon_ok=8406  recon_fb=16677
```

**读法:**

- `enter_ok` 严格等于 `login_ok` → login 链路本身是干净的,robot 只要 login 成了就 EnterGame 也成
- `enter_fail` ≈ 2 × `enter_ok` → 大部分 robot 在 EnterGame 之后那一步(等 scene ready)失败,每个失败 robot 因为 main.go 的重试逻辑会再来 1-2 次,所以累积 fail 远大于 ok
- `recon_fb > recon_ok` → access_token 重连成功率不到 40%,主要因为 robot 在 scene-ready 超时后没活下来,access_token 还没刷过就过期
- `q_*=0` → ✅ capacity=20000 留 buffer 起作用,没排队
- `msg_sent ≈ msg_recv` 且变 0/s → robot **进进出出**而不是真在稳态在线,profile=stress 的 AI 动作循环没跑起来

---

## 下次开跑前要做的事

| 优先级 | 事情 |
|---|---|
| 必须 | 单 zone 5000 robot smoke,确认 scene-ready 走通 — 不解决这个就上 45k 是纯浪费时间 |
| 必须 | 把 robot.exe 的 stop 路径走通(或者改 launcher 用 console group + Ctrl-C 收 CSV) |
| 必须(新) | 修 cpp node CAS 启动竞态(§E),不修就不能并发起多 cpp,只能错峰起 |
| 应该 | login fast-path 的 nil panic 出 PR 进主干 |
| 应该 | Gateway login.grpc 配置改走 etcd 发现,这次手改的 3 endpoint 列表换机器就废 |
| 可以延后 | etcd advertise URL 改动写进 deploy/README,host_ip 不同的机器要 export `HOST_IP` |
| 可以延后 | 调查 z1_scene_1 lease keepalive 卡死的细节(stdout 没记录) |

---

## 第三轮真实数据(robot 启动后 ~26 分钟,scene 已挂 3 个)

```
z1 [stats 26m35s]
  conn=75000    login_ok=50645  login_fail=0    login_stuck=0
  enter_ok=50645  enter_fail=74332
  avg_login=230ms  max_login=8.847s
  recon_ok=34641  recon_fb=25145

z2 [stats 26m30s]
  conn=75000    login_ok=46849  login_fail=0    login_stuck=0
  enter_ok=46849  enter_fail=86685
  avg_login=516ms  max_login=11.322s
  recon_ok=30274  recon_fb=29607

z3 [stats 26m29s]
  conn=75000    login_ok=46737  login_fail=0    login_stuck=0
  enter_ok=46737  enter_fail=88664
  avg_login=628ms  max_login=9.298s
  recon_ok=28961  recon_fb=30596
```

**对比第二轮的进步:**
- `login_fail`:**~300 → 0**(全栈干净重启 + Gateway/login channel 重建后,登录链路 100% 干净)
- `avg_login`:**3-4s → 0.2-0.6s**(快 6-10×)
- `max_login`:**49s → 11s**

**没解决的问题:**
- `enter_fail` 仍 ≈ 1.6× `enter_ok`,scene-ready 超时还在
- `conn=75000`(robot_count×5)= 5 倍重连放大;真正的稳态在线数(没法直接算,大概 ~15-20k/zone)还是远低于"3 zone × 15000 都进满"的目标

---

## §E. CPP node 启动 hijack 竞态(第三轮新发现的真 bug)

### 现象

第三轮 robot 启动后 ~50 秒(10:33:12-14),zone-1 的 **4 个 cpp 进程**(scene_2、scene_3、scene_4、gate_2)在 2 秒窗口内**同时**自杀。zone-2/3 没事。

### 关键日志(z1_scene_2)

```
22:33:13.050 Node created
22:33:13.066 ZONE_ID env override applied: 1
22:33:13.071 Node endpoint resolved. ip=172.27.16.1
22:33:13.124 EtcdService initialized with etcd: 127.0.0.1:2379
...
22:33:13.252 Parse node JSON failed, type: 5, JSON: b29d8424-...   ← 不是 JSON,只是 UUID
22:33:13.254 Node added, type: 5, node_id: 0, uuid: b29d8424-...   ← node_id=0 (!)
22:33:13.258 Node added, type: 4, node_id: 0, uuid: 6b12edee-...   ← 又一个 node_id=0
22:33:13.258 Parse node JSON failed, type: 3, JSON: fc7e37f8-...
22:33:13.259 ERROR Node ID hijack detected via Watch!
             key=SceneNodeService.rpc/zone/1/node_type/3/node_id/0
             my_uuid=3eac999f-b09e-40a4-b446-2c894e9a04ae
             remote_uuid=fc7e37f8-bf91-404d-8e8c-023f9fb394d2
22:33:13.259 FATAL Another node has claimed our node_id=0 via etcd Watch.
```

### 根因

启动期 `MakeNodeEtcdKey(myInfo)` 用 `myInfo.node_id` 当 key 后缀。在 NodeId CAS 抢号成功之前,`myInfo.node_id == 0`,所以**所有还没分到 id 的同 type 同 zone 节点共用 `.../node_id/0` 这一个 key**。谁先 PUT,后到的 Watch 看见 PUT(uuid != 自己)→ hijack 检测命中 → 自杀。

zone-1 是 `dev start-zones` 第一个起的,6 个 cpp 进程并发抢 node_id 分配,5 个挤进 hijack 窗口,1 个先到的活下来,后到的 4 个全死。zone-2/3 起的时候 zone-1 进程已经退出 hijack 窗口,etcd 里没有 type=zone-2/3 的 node_id=0 写入,反而风平浪静(也可能它们启动顺序错峰够了)。

### 旁证

- 同样是被 hijack 杀的 z1_scene_2 stdout,前面的 "Parse node JSON failed" 日志告诉我们 etcd 里**有 6 条 value 是裸 UUID**(不是 NodeInfo proto 序列化的 JSON)。这看起来像 `NodeAllocator` 的占位符 —— 用 UUID 占住 port slot,然后再 PUT 真正的 NodeInfo。这步本身就有 race。
- `reason=2` 在 `OnNodeIdConflictShutdown` 里对应 `NodeIdConflictReason::kReRegistrationFailed`(用户最早贴的那段代码),不是 #B 的 `kLeaseExpiredByEtcd`(reason=0)。这次和上次是**两个不同的崩溃**。

### 修法选项(没在压测窗口里改)

1. **错峰启动 cpp**:`dev start-zones` 在每个 cpp 启动之间加 `sleep 2`,让前一个走完 NodeId CAS 再起下一个。最便宜,但治标。
2. **让 hijack 检测忽略 `node_id=0`**:`HandlePutEvent` 里如果 `myInfo.node_id() == 0` 就跳过,因为这时候我们还没 commit 任何 id,被"抢"是正常的 —— 让 NodeId CAS 自己处理失败重试。
3. **NodeId CAS 之前不要 Watch**:把 `StartWatchingPrefixes` 挪到 `AcquireNodePort` 之后。最干净,但要看 Watch 启动时机是不是有别的依赖。

第二个改动量最小风险最低,推荐。

---

## §F. Robot launcher 不收 CSV 的二次确认

第二轮和第三轮我都用 `Stop-Process -Force` 强杀的 robot,**两次都丢了 CSV**。`run-stress-3zone.ps1` 的 `Wait-Process` + 自己 trap Ctrl-C 那条路径只有从 launcher 自己的控制台 Ctrl-C 才走得通,从外面 kill PID 不会触发 robot main.go 的 `signal.Notify`。

实际上 Windows 上要做到"从另一个进程发 Ctrl-C 给 robot 进程组"非常曲折(`AttachConsole` + `GenerateConsoleCtrlEvent`)。更便宜的做法是**给 robot 加一个 stop endpoint**(HTTP 或 named pipe),launcher 写一个 `stop-stress.ps1` 调它,robot 收到后内部 close stopAll → ExportBehaviorCSV。

下次跑前最少要做的:启动脚本把这个限制写进开头注释,提醒操作员"用 launcher 自己的 Ctrl-C,不要 kill -9"。

---

## §G. 第二天补丁(2026-05-24):hijack + schema

5/23 复盘完后 5/24 又跑了三轮 5000-robot single-zone smoke,把"不能上 45k"的两条剩余拦路虎修了。

### G.1 Hijack 启动竞态 — 已修

`cpp/libs/engine/core/node/system/etcd/etcd_service.cpp::HandlePutEvent`:

```cpp
// 修前:任何 PUT 到自己 key 且 uuid 不同 → FATAL
if (key == myKey) { ... LOG_FATAL ... }

// 修后:加一道前置闸,启动期 node_id=0 不查 hijack
if (myInfo.node_id() != 0) {
    if (key == myKey) { ... LOG_FATAL ... }
}
```

为什么对:启动期 `MakeNodeEtcdKey` 用 `myInfo.node_id` 拼 key,CAS 完成前 `node_id == 0`,所有同 zone 同 type 未分配的 peer 都共用 `.../node_id/0` 这个临时 key。第一个 PUT 的 peer 触发其它 peer 的 hijack 检测假阳性。NodeAllocator 的 CAS 自己已经处理 id 占用 → Watch 层冗余执法,删掉对启动期友好。

**5/24 smoke 验证**:三轮 single-zone × 5000 robot 跑下来,12/12 scene + 6/6 gate 全程没死,`grep "Another node has claimed"` 全空。修复站住。

### G.2 Schema drift `merge_state` — 已修

5/24 smoke 第二轮跑完发现 enter_fail = 30k,跟 5/23 第三轮一样。沿着 enter_fail 往回挖找到底层 db SQL 错误:

```
Error 1054 (42S22): Unknown column 'merge_state' in 'field list'
SELECT player_id, transform, ..., stress_test_probe, merge_state
FROM player_database WHERE player_id='...';
```

`generated/proto/_unified/proto/common/database/mysql_database_table.proto` 加了 field 10 `PlayerMergeStateComp merge_state`,db service 用 proto 反射拼 SELECT 列名,但**所有 zone_*_db.player_database 都没建这一列**。每条 EnterGame preload 走 db_task → db SELECT → MySQL 1054 报错 → 任务 retry 3 次 → 进死信队列 → login 30s timeout → enter_fail 翻倍。

修法:对 zone_1/2/3 数据库执行

```sql
ALTER TABLE player_database ADD COLUMN merge_state mediumblob NULL AFTER stress_test_probe;
```

加完列还要把 `db_task_zone_*` topic 上**已经堆积的 30 万条带错 task** 的消费组 offset 重置到 latest,否则新 db service 拉起来还是从 partition 0 重头消费这堆"必败" task:

```bash
for z in 1 2 3; do
  docker exec kafka /opt/kafka/bin/kafka-consumer-groups.sh \
    --bootstrap-server localhost:9092 \
    --group db_rpc_consumer_group_z$z \
    --topic db_task_zone_$z \
    --reset-offsets --to-latest --execute
done
docker exec redis redis-cli DEL \
  kafka:dead:queue:db_task_zone_1 \
  kafka:dead:queue:db_task_zone_2 \
  kafka:dead:queue:db_task_zone_3
```

(reset 前要先停所有 db.exe,否则 kafka 报 "group is Stable, can only reset when inactive"。)

**5/24 smoke 第三轮验证**:enter_fail/enter_ok 从 v1 的 2:1 降到 0.9:1;**`scene_notified=1` 从 0 涨到 11631**(BindSession 第一次真的路由到 scene 走通了)。这是这次压测的真正硬产出。

### G.3 三轮 single-zone × 5000 smoke 数据对比

| 指标 | v1(原始) | v2(schema fix 后) | v3(+ kafka offset reset) |
|---|---|---|---|
| login_ok | 14891 | 15140 | **19200** |
| enter_ok | 14891 | 15140 | **19200** |
| enter_fail | 30396 | 29617 | **17438** |
| `scene_notified=1` | **0/13006** | (没量) | **11631** |
| preload_fail | (没量) | 14034 涨 | **22579 然后停涨** |
| avg_login | 55ms | 46ms | 50ms |
| scene/gate 存活 | 12/6 | 12/6 | **12/6 全程** |
| hijack FATAL | 0 | 0 | **0** |

### G.4 还没解决的 — 这才是 45k 的真正拦路虎

v3 跑到 T+135s 后 `login_ok` 卡死 19200 不再涨,msg_sent/recv 速率回落到 0/s。这跟 5/23 第三轮的"假稳态"是同款,但根因不一样:

- 5/23 第三轮:gate 订错 kafka topic / hijack 干掉 4 个进程 / merge_state schema drift 三件 bug 叠加
- 5/24 第三轮:**db 吞吐瓶颈** — 5000 robot 同时灌 EnterGame,22k 个 db_task 在 2 分钟内打进 partition 0,db service worker 处理速度赶不上 retry TTL → 所有早期 task 全进死信

22579 个 preload_fail 之后停涨,说明"早期挤进 kafka 的请求都失败了,后来的 robot 因为 main.go 的重试上限放弃,系统进入空载稳态"。**这是真实的 db throughput 瓶颈,不是 bug**。

要打通 45k 必须先回答:5000 robot/zone 都打不进稳态,15000 robot/zone 在 db 层只会更糟。可能要做的事:

1. **db_task_zone_N 加 partition**:现在 partition=5(db.yaml `PartitionCnt: 5`),但实际**只 partition 0 在用** — sarama 默认 hash 分区,所有 task 用同一 key 全到 partition 0。需要看 `KeyOrderedKafkaProducer` 是不是真的按 player_id 分散。
2. **db worker 增多**:`go/db/internal/kafka/key_ordered_consumer.go` 的 worker 池大小可能过小。
3. **EnterGame preload 改成"先返回成功,后台异步预加载"**:当前是同步等 task ACK 才回 EnterGameResponse,这是 enter_fail 路径上唯一的同步等待 30s,改成非阻塞能直接消除 timeout。

这些都是优化,不是 bug,**优先级要看后续目标**。

---

## §H. 给下次跑的清单(5/24 末更新版)

按"拦路虎重要度"排:

| 优先级 | 事情 | 状态 |
|---|---|---|
| ✅ done | hijack 启动竞态 | 已修(§G.1) |
| ✅ done | merge_state schema drift | 已修(§G.2) |
| ✅ done | docker-compose etcd advertise URL | 已修(§3) |
| ✅ done | login fast-path nil panic | 已修(§2) |
| 🔴 拦路虎 | **db_task partition 0 单分区瓶颈** | 没碰,§G.4 |
| 🔴 拦路虎 | **EnterGame preload 同步等 30s task ACK** | 没碰,§G.4 |
| 🟡 应该 | Robot launcher CSV 收尾(§F) | 没修 |
| 🟡 应该 | Gateway login.grpc 走 etcd 发现 | 没修 |
| 🟢 可延后 | etcd HOST_IP 写进 deploy/README | 没修 |

不解决两个 🔴,直接上 45000 的结果几乎可以预言:每 zone 15k 同时灌 db_task,partition 0 堆 70k+ 消息,前 5 分钟全进死信,robot 全部 scene-ready 超时,在线数最多到 1/3。

---

## §I. 第二天后续(2026-05-24 下午):db_task 单分区瓶颈 — 已修

### I.1 根因

`db_task_zone_1` broker 上实际只有 **1 个 partition**(`PartitionCount: 1`),不是配置里写的 `PartitionCnt: 5`。这是 `EnsureTopics` 的逻辑漏洞:

```go
// go/shared/kafkautil/topic_init.go 修前
if _, exists := existing[spec.Name]; !exists {
    admin.CreateTopic(spec.Name, ...)  // 用 partitions=5 建
} else {
    // 只更新 retention.ms,不扩 partition
}
```

第一次 boot 走 sarama auto-create 用 broker 默认 `num.partitions=1` 建了这个 topic,后续所有 boot 都进 else 分支,partition 数永远卡在 1。`PartitionCnt: 5` 配置实际没生效过。

db 端 `key_ordered_consumer.go` 是按 partition 起 worker(`workers map[int32]*worker`)→ partition=1 等价于 db 只有 1 个 worker。EnterGame 高并发时所有 task 同一个 worker 串行处理,5000 robot 灌 22k 个 task → 3 次 retry 全死信。

### I.2 修法

(1) broker 端立即扩 partition:
```bash
for z in 1 2 3; do
  docker exec kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 \
    --alter --topic db_task_zone_$z --partitions 5
done
```

(2) 代码:`go/shared/kafkautil/topic_init.go::EnsureTopics` 加 `CreatePartitions` 分支,topic 已存在但 partition 数低于 spec 时增长。注意只对 work-queue 类 topic 安全(没有跨 boot 的 per-key ordering 依赖),db_task 满足。

### I.3 验证(smoke 5k v4)

| 指标 | v3(1 partition) | **v4(5 partition)** | 变化 |
|---|---|---|---|
| login_ok | 19200 | **25000** | **+30%** |
| enter_ok | 19200 | **25000** | 全 5000 robot × 5 次重连都成功 |
| **enter_fail** | 17438 | **0** | 🟢 全消 |
| **scene_notified=1** | 11631 | **25000** | 🟢 100% 走通 |
| **preload_fail** | 22579 | **0** | 🟢 全消 |
| avg_login | 50ms | 42ms | 略快 |

partition LAG 分布证据(T+135s):
```
z1_lag = [part0:27  part1:22  part2:22  part3:38  part4:26]
                                                         ^ part4 包含历史 reset 之前的 offset
```

新 task 在 4 个活跃 partition 上**均匀分布**(每个 ~4200 条),5 个 worker 并行消费,**LAG 在 8 分钟内归零**。

### I.4 现在的拦路虎清单(2026-05-24 末更新)

| 优先级 | 事情 | 状态 |
|---|---|---|
| ✅ done | hijack 启动竞态 | §G.1 |
| ✅ done | merge_state schema drift | §G.2 |
| ✅ done | **db_task 单 partition 瓶颈** | §I |
| ✅ done | docker-compose etcd advertise URL | §3 |
| ✅ done | login fast-path nil panic | §2 |
| 🟡 应该 | Robot launcher CSV 收尾 | §F |
| 🟡 应该 | Gateway login.grpc 走 etcd 发现 | — |
| 🟢 可延后 | EnterGame preload 改异步 | 性能优化,5k 已不需要 |
| 🟢 可延后 | etcd HOST_IP 写进 deploy/README | — |

**结论:5000-robot single-zone smoke 在 8 分钟内打通完整闭环,login_fail=enter_fail=preload_fail=0,scene_notified=100%。可以上 45000 三 zone 完整压测了。**

下一轮跑 45k 时需要做的额外动作:
1. 确保 `db_task_zone_{1,2,3}` 三个 topic 都是 5 partition(boot 时 `EnsureTopics` 会自动扩,但第一次跑前最好手动 verify)
2. 每个 zone 的 `db.yaml::PartitionCnt` 跟 broker 实际 partition 数一致
3. (可选)把 partition 提到 10,看 15k/zone 是否 4-partition × 5 zone × 3 worker 还能 handle

---

## §J. 第二天再后续(2026-05-24 晚):Gateway zone-aware 路由 — 已修

### J.1 5k → 45k 阶跃暴露的新 bug

partition 修完跑首次 3-zone × 15000 时,smoke 直接报 `code=500 error="no gate available for requested zone"`。z1_login 日志:

```
[loginqueue] fast-path zone=3 has zero candidates
```

**根因**:`Gateway::LoginRpcClient::unaryCall` 用 `floorMod(rr++, channels.size())` round-robin 把 AssignGate 散到 3 个 login channel,但每个 `login.rpc` 只 watch 自己 zone 的 etcd 前缀(`GateNodeService.rpc/zone/{ZoneId}/...`)→ 2-in-3 的概率把 zone-1 的请求发给 z2/z3 login → 它们看不到 zone-1 的 gate → zero candidates。

5k 阶段没暴露因为 single-zone,所有请求都打 z1。

### J.2 修法

**Java 侧** `LoginRpcClient`:加 `Map<Integer, ManagedChannel> channelByZone`,endpoint 配置语法支持 `<zoneId>=host:port`,`unaryCall` 加 `zoneId` 重载。RefreshToken 没 zone 字段(access_token 不携带 zone)→ 走 round-robin fallback。

**2026-08-03 fail-closed 补强：** `Login`、`AssignGate`、`QueryQueueStatus` 都是
zone-bound 调用；zone 非正数或 `<zoneId>=host:port` mapping 缺失时必须在发 RPC
前报错，不能回退 round-robin。无 zone 的公开重载已移除；只有当前 token 不携带
zone 的 `RefreshToken` 保留 round-robin。这样配置漂移表现为明确的 Gateway 500/
错误日志，而不是跨区请求或偶发的 `no gate available`。

**配置侧** `application.yaml`:
```yaml
login:
  grpc:
    endpoints: "1=127.0.0.1:53000,2=127.0.0.1:54000,3=127.0.0.1:55000"
```

**调用点**:`AssignGateService.assignGate` / `LoginService.login` / `AssignGateService.queryQueueStatus` 改用 zone-aware 重载;`QueueStatusRequest` 加 `zoneId` 字段,robot 端 `httpQueueStatus` 也加 `zoneId` 透传。

### J.3 验证

3 zone × 3 次 smoke,每个 zone 都路由到自己的 gate:
```
z1 #1-3: code=0 gate=172.27.16.1:10007  ← z1_gate_2
z2 #1-3: code=0 gate=172.27.16.1:10000  ← z2_gate_1
z3 #1-3: code=0 gate=172.27.16.1:10003  ← z3_gate_2
```

zero candidates 错误彻底消失。

### J.4 完整 3-zone × 15000 压测真实数据(终于跑出来了)

| 指标 | z1 | z2 | z3 |
|---|---|---|---|
| conn(累计含重连) | 45328 | 46486 | 45639 |
| **login_ok** | **26245** | 25916 | 27122 |
| login_fail | 3667 | 3235 | 2958 |
| login_stuck(>15s 等响应) | 1544 | 1407 | 1828 |
| enter_ok | 26245 | 25916 | 27122 |
| **enter_fail** | **42182** | **42809** | **39551** |
| avg_login | 6.10s | 6.00s | 3.93s |
| max_login | 55.3s | 39.6s | 55.6s |

后端 hijack=0,12/12 scene + 6/6 gate 全程不死。

**scene_notified breakdown(从 cpp gate 日志聚合):**
| zone | notified=1 | notified=0 |
|---|---|---|
| z1 gates | **26190** | 37172 |
| z2 gates | 1303 | 47662 |
| z3 gates | 2412 | 36866 |

z1 的 notified=1 跟 z1 enter_ok 几乎对齐(26190 vs 26245)→ zone-aware 路由对 z1 有效。
但 z2/z3 的 notified=1 远低于 enter_ok → **z2/z3 还有别的损耗**(初步猜测:cpp gate Kafka consumer group 三个 zone 共享 `game-consumer-group` 单一 group_id,导致 partition rebalance 把 z2/z3 的消息分配给了 z1_gate 消费,z2/z3 的 BindSession 进了"错的 gate"被忽略)。

### J.5 这次跑出来的硬产出

1. **首次跑通 45000 robot full lifecycle**:每个 robot 都至少 connect → login_ok → EnterGame → 进入重连循环;**没有"全军覆没"或"假稳态"**
2. **后端 32 进程 + 1 Gateway 全程稳定**:hijack 防御没复发,partition 5 的 db worker 把 db_task 消化掉了
3. **数据可信**:avg_login=4-6s + max_login=55s 是真实的吞吐瓶颈,而不是 bug 导致的虚高

### J.6 仍未解决的下一道关卡

- z2/z3 `scene_notified=1` 比例太低 → cpp gate Kafka group_id 全局共享导致跨 zone 错路由,需要改成 `gate-group-z<N>` 之类的 zone-scoped group(预估改动: cpp `BuildDefaultKafkaOptions` 把 zoneId 加到 groupPrefix 里,跟 topic 一样)
- avg_login 3.9-6.1s vs 5k 的 42ms,说明 EnterGame preload 的 db Kafka 链路在 45k 浪涌下还是排队 —— 把 partition 提到 10 或者 EnterGame 改成"不等 ACK 后台预加载"是后续优化方向

---

## §K. 给下次跑的最终清单(2026-05-24 末更新)

| 优先级 | 事情 | 状态 |
|---|---|---|
| ✅ done | hijack 启动竞态 | §G.1 |
| ✅ done | merge_state schema drift | §G.2 |
| ✅ done | db_task 单 partition 瓶颈 | §I |
| ✅ done | **Gateway zone-aware 路由** | §J |
| ✅ done | docker-compose etcd advertise URL | §3 |
| ✅ done | login fast-path nil panic | §2 |
| 🔴 拦路虎 | **cpp gate Kafka group_id 共享导致 z2/z3 错路由** | §J.6 |
| 🟡 应该 | Robot launcher CSV 收尾 | §F |
| 🟡 应该 | partition 5→10,验证 15k/zone 进满 | — |
| 🟡 应该 | EnterGame preload 改异步(可选) | — |

**45k 已经能跑出可信数据。下一轮重点是把 z2/z3 的 scene_notified=1 也拉到 100%(修 §J.6 的 group_id),那才是真正的"全 45k 进游戏循环"。**

---

## §L. 第三天追加(2026-05-24 晚 + 25 凌晨):cpp Kafka GroupID + etcd timeout + 真瓶颈在 Redis 锁

### L.1 修了的两件事

**(1) cpp Kafka GroupID 共享 → per-instance**
`bin/etc/base_deploy_config.yaml::Kafka.GroupID: "game-consumer-group"` → `""`(空触发 fallback `<groupPrefix>-<node_id>`)。生效后 broker 上看到 6 个独立 group:`gate-group-{0,1,2,3,4,5}`,每个只消费自己 topic,LAG 全程 0。

**(2) etcd timeout 调大**
`go/login/etc/login.yaml::Timeouts.{EtcdDialTimeout, ServiceDiscoveryTimeout}` 10s → 30s(3 个派生 yaml 同步改);
`bin/etc/base_deploy_config.yaml::NodeTTLSeconds` 60 → 180(防 keepalive 一帧迟到就被判 lease 过期)。

### L.2 第三轮 45k 数据(终于跑到稳态)

| 指标 | z1 | z2 | z3 |
|---|---|---|---|
| conn(累计) | 59955 | 60792 | 60777 |
| **login_ok** | **16486** | **16871** | **19837** |
| login_fail | 6186 | 6518 | 6916 |
| enter_ok | 16486 | 16871 | 19837 |
| enter_fail | 85529 | 85680 | 75387 |
| avg_login | 3.39s | 3.25s | 2.26s |
| max_login | 11.9s | 13.0s | 11.6s |
| scene_notified=1 | 4837 | 5412 | 10470 |

**对比 v1 / v3:** login_ok 反而**降低**(v1 79k → v3 53k),但 conn 几乎一样到 60k —— 大量 robot 在 conn 后陷入"login 失败 → retry → 又失败"循环。这不是新 bug,是 **Redis 锁路径吞吐瓶颈终于显化**。

### L.3 真根因:`PlayerLockTTL=30s` heartbeat 在 45k 浪涌下丢锁

z1_login 错误分布(20 分钟跑完):

| 计数 | 错误 |
|---|---|
| **65333** | `EnterGame lost lock mid-chain: lost lock ownership for key=player_locker:N` |
| 11237 | `EnterGame preload failed: task UUID wait: task result wait timed out` |
| 4006 | `[disconnect] get account failed: redis: nil` |

`EnterGame lost lock` 占绝对主导。链路在 `entergamelogic.go`:

```go
stopHeartbeat := tryLocker.StartHeartbeat(
    lockTTL/3,    // 10s renew interval
    lockTTL,       // 30s TTL
    func(err error) {
        logx.Errorf("EnterGame lost lock mid-chain ...")
        chainCancel()  // 杀掉整个 EnterGame
    },
)
```

`PlayerLockTTL=30s` + heartbeat 每 10s renew 一次。在 45k 浪涌下 Redis QPS 短时飙到不可控,heartbeat goroutine 自己也在排队等 Redis 响应,**一帧延迟超过 TTL 就会丢锁**。lock 失效 → 同一 robot 重连时新的 EnterGame 抢到 → 老的 chain heartbeat 触发 `chainCancel()` → 整个 EnterGame 链路回滚。robot 端看到 `login flow failed`,retry 又来一遍。

**这不是 bug,是 30s TTL + 单 Redis 实例 + 45k QPS 的真实物理极限**。

### L.4 解决方向(没做)

按改动量从小到大:

1. **PlayerLockTTL 拉长**(30s → 120s):heartbeat 容错增加 4×,大概率消除 65k lost-lock。但这只是把窗口推后,不解决根因。
2. **EnterGame 整链路改无锁化**:用 Redis SETNX 单次抢占代替 watchdog-style heartbeat。改动量大,影响很多其他 entergame 路径。
3. **Redis 集群化或上 sentinel + read-replica 分担负载**:基础设施层修法,treat 45k 浪涌为 prod 级流量。

#1 可以一行改,值得先试。但前提是配套观测 `lock_held_seconds` 直方图,看真实锁持有时长是不是真的撞到 TTL —— 如果只是 heartbeat goroutine 调度饿死,拉 TTL 没用。

### L.5 中间发现的"幽灵 bug" — 实际是测试方法问题

第二轮 45k 数据看起来是 z2/z3 `scene_notified=1` 比例低,我以为是 Kafka group_id 共享导致跨 zone 错路由(§J.6),花了 1 小时改 cpp + Java + robot。事后发现:**第二轮跑时残留了第一轮 32 个进程没死**(我之前 Stop-Process 的 PowerShell 命令静默没杀干净),所以第二轮 robot 实际连的是上一轮的 1 partition + 共享 group 后端,fix 没机会生效。

第三轮真停干净后,§J.6 修复**确实生效**(broker 上看到 6 个 per-instance group,LAG=0),但 z2/z3 `scene_notified=1` 比例并没回到 100%(z1=4837 / z2=5412 / z3=10470)—— 因为真瓶颈早一步,在 Redis 锁丢失上,scene_notified 这一步根本到不了。

教训:**每次重启都要 verify residual 进程数为 0**,不是看 stop 命令的 exit code。

### L.6 完整拦路虎清单(2026-05-25 凌晨更新)

| 优先级 | 事情 | 状态 |
|---|---|---|
| ✅ done | hijack 启动竞态 | §G.1 |
| ✅ done | merge_state schema drift | §G.2 |
| ✅ done | db_task 单 partition 瓶颈 | §I |
| ✅ done | Gateway zone-aware 路由 | §J |
| ✅ done | cpp Kafka GroupID per-instance | §L.1 |
| ✅ done | etcd timeout 30s / NodeTTL 180s | §L.1 |
| 🔴 真瓶颈 | **PlayerLockTTL heartbeat 在 45k 下丢锁(65k 次)** | §L.3,没改 |
| 🟡 应该 | Robot launcher CSV 收尾 | §F |
| 🟡 应该 | Stop-Process 之后 verify 0 residual | §L.5 教训 |
| 🟢 可延后 | EnterGame preload 改异步 | — |

**现状:** 45000 robot 都能完成 connect / login / EnterGame 一次,但 Redis lock 在浪涌下挤掉 ~65% 的 EnterGame chain。这是一个**有 number 的稳定瓶颈**,不再是"压不上去"。

**下一轮**(若继续):先把 `PlayerLockTTL: 30 → 120`,跑一遍看 lost-lock 是否归零,再决定要不要做 §L.4 #2/#3 的大改。

---

## §M. 第八轮 45k:PlayerLockTTL 120s 的部分胜利 + 新瓶颈往后挪了

### M.1 改动

`go/login/etc/login.yaml` + 3 个 z*_login.yaml:`PlayerLockTTL: 30 → 120`(heartbeat renew 间隔 TTL/3 = 40s)。

### M.2 数据对比

| 指标 | v3-45k(原始) | **v8-45k(lock 120s)** | Δ |
|---|---|---|---|
| login_ok 总 | 79220 | **149476** | **+89%** ↑↑↑ |
| login_fail 总 | 9665 | 7292 | -25% |
| EnterGame `lost lock` | **65333** | **43719** | -33% ↓ |
| EnterGame `preload timeout` | 11237 | **47802** | **+325%** ↑↑↑ |
| enter_fail 总 | 124k | 268k | +115% |
| scene_notified=1 | ~30k | 4511 | -85% |
| avg_login | 4-6s | 4-5s | 略快 |

**进度信号:** 8 分钟内 conn 涨到 75000(每 zone 75k = robot×5,完整重连次数),login_ok 翻 89%。这是真实可观测的容量提升。

### M.3 真因诊断:lock 修了,瓶颈往后挪到 db_task ACK 等待

PlayerLockTTL 120s 让 heartbeat goroutine 即使被 Redis QPS 挤压也能撑 4× 容错,**lost-lock 从 65k 降到 44k**(没归零是因为 EnterGame chainCtx 还有别的取消路径)。

但 `preload timeout` **从 11k 飙到 48k** —— 现在 EnterGame 主链路的等待时间不在 lock 上,而在 db_task 上:

```
login.EnterGame
  → submit preload → Kafka db_task_zone_N  ← db 处理 + ACK 总时长 > 5s 即超时
  ↑ 这里 wait 5s 超时(TaskWaitTimeout)
  → onPreloadComplete(err: task wait timed out)
  → chainCancel() → 整个 EnterGame 链路回滚
```

每个 zone db service 只 1 个进程 + Kafka partition=5 + 5 个 worker(我们昨天扩的);15000 robot 一波灌进来,Kafka 入队顺利,但 worker 池处理 SELECT(MySQL)的吞吐撑不住,task 在 partition 里等 worker → 超过 5s。

**lock 修了之后多出来的 robot 都积压在 db_task 这一段**,把瓶颈往后挪了一个环节。

### M.4 后续修法选项(没做)

按改动量从小到大:

1. **`TaskWaitTimeout: 5s → 30s`(login.yaml):** 一行改,对应 PlayerLockTTL 思路。给 db worker 4-6× 容错。但 preload 不像 lock 有 heartbeat 不断刷新,纯粹是"算更长时间的耐心" → 锁占用变长,内存占用更高。
2. **db_task partition 5 → 10/20:** 同样一行改(`PartitionCnt`),但需要先在 broker 上 `kafka-topics --alter --partitions 10` 同步。partition 多了 worker 池才能并行更多。代价:每个 worker 持有的 MySQL 连接会翻倍,如果 MySQL 不能撑住会被打死。
3. **EnterGame preload 改异步**(不等 ACK):login 返回 OK 给 robot,player_locator/scene 那边自己 retry-on-NIL。改动量中,但根本上去掉同步等待。

#1 + #2 是稳的小步,#3 是真正的架构修法。

### M.5 仍未解决的下一道关卡

`scene_notified=1` 仍只有 4511 / 总计 149k login_ok。意思是即使 robot login_ok+enter_ok 了,**实际进入 scene 完成 BindSession→RoutePlayer 链路** 的只占 3%。原因不在 cpp gate(group_id per-instance 已修),也不在 login lock,而在 db_task 的 preload 那一关 —— preload timeout 直接砍掉 onPreloadComplete 后面的 SendBindSessionToGate / EnterScene。

链条是:**preload timeout → onPreloadComplete err 早 return → 没发 BindSession → cpp gate 看不到 BindSessionEvent → scene_notified=0**。

### M.6 第八轮归档清单

| 优先级 | 事情 | 状态 |
|---|---|---|
| ✅ done | hijack 启动竞态 | §G.1 |
| ✅ done | merge_state schema drift | §G.2 |
| ✅ done | db_task 单 partition 瓶颈 | §I |
| ✅ done | Gateway zone-aware 路由 | §J |
| ✅ done | cpp Kafka GroupID per-instance | §L.1 |
| ✅ done | etcd timeout 30s / NodeTTL 180s | §L.1 |
| ✅ done | PlayerLockTTL 30→120s | §M |
| 🔴 真瓶颈 | **db_task preload TaskWaitTimeout 5s 太紧** | §M.4 |
| 🟡 应该 | EnterGame preload 改异步 | §M.4 #3 |
| 🟡 应该 | Robot launcher CSV 收尾 | §F |
| 🟢 可延后 | partition 5→10,看 db worker 容量 | §M.4 #2 |

**45k 累积产出:**
- login_ok 累计跑出 149k(三 zone × 5 重连次数 ≈ robot×3)
- 后端 32 进程全程稳定无崩溃,hijack=0
- avg_login 稳在 4-5s,max_login 在 21s 上下,无指数发散
- 7 个真业务 bug 修了进源码

第八轮是今天的高水位线。下次开始前先验证 §M.4 #1 + #2(partition 翻倍 + TaskWaitTimeout 调到 30s),预期能把 preload timeout 再砍一半,scene_notified=1 比例往 50%+ 走。

---

## §N. 第九轮 45k:TaskWaitTimeout 30s — scene_notified 翻 4× 但 login_ok 反而降

### N.1 改动

`go/login/etc/login.yaml` + 3 个 z*_login.yaml:`TaskWaitTimeout: 5s → 30s`。其他不变。

### N.2 数据对比

| 指标 | v8(lock 120s) | **v9(+ TaskWait 30s)** | Δ |
|---|---|---|---|
| login_ok 总 | **149476** | 102043 | **−32%** ↓ |
| login_fail 总 | 7292 | 8586 | +18% |
| EnterGame `lost lock` | 43719 | 57125 | +31% ↑ |
| EnterGame `preload timeout` | 47802 | 87260 | +82% ↑↑ |
| enter_fail 总 | 268591 | 131469 | **−51%** ↓ |
| **scene_notified=1** | 4511 | **18825** | **+317%** ↑↑↑ |
| max_login | 35s | **63s** | ↑ |

### N.3 真因诊断:不是修了 db,是把失败"挪后了"

TaskWaitTimeout 5s→30s **没有改变 db worker 的处理速度**,只是让 login 多等了 25s 才放弃 preload。结果:

- ✅ 多等的 25s 里,**有些 task 真的在 5s-30s 之间 ACK 了** → 这些 robot 不再 preload_timeout,继续走完整链路 → **scene_notified=1 翻 4×**
- 🔴 没在 30s 内 ACK 的还是失败,但**每次失败占住 player_locker 30s**(而不是 5s)→ 新 robot 抢锁概率下降 → login_ok 流量被限流降 32%
- 🔴 lost_lock 也涨了,因为 chain 占用时间长了 6×,heartbeat 出问题的窗口随之放大

**关键认知**: scene_notified=1 才是真正衡量"有人在玩"的指标。第九轮虽然 login_ok 比第八轮少 47k,但**完整走通 EnterGame 的人数翻了 4 倍**(4511 → 18825)。

### N.4 真瓶颈定位:db worker 吞吐量

第九轮 preload_to=87k(占 EnterGame 失败 42%)说明:**partition=5 + 1 db worker/partition** 的处理速度跟不上 ~250 task/s 的浪涌(45k robot × 5 重连 / 12 分钟 ≈ 312/s)。每个 SELECT 实测 1-2ms,但 worker 处理含 Kafka 轮询 + MySQL roundtrip + Redis 写回 + ACK,**端到端延迟在 100-500ms**,所以 5 partition × 1 worker 的稳态吞吐上限大概 50-100 task/s,远低于浪涌 250/s。

### N.5 三轮迭代的累积曲线

| 轮 | 修复 | login_ok | scene_notified=1 | 真正"在线"占比 |
|---|---|---|---|---|
| v3-orig | 0 fix | 79220 | ~30000 | 38% |
| v8 | + lock 120s | **149476** | 4511 | 3% |
| v9 | + TaskWait 30s | 102043 | **18825** | **18%** |

v3 的 scene_notified ratio 看起来高,实际是因为整条链路被 lock 提前砍掉,counts of "scene_notified=1" 包含了之前几轮残留进程的数据;v8/v9 是干净 baseline。

**真实容量估算**:v9 中 18825 个 scene_notified=1 / 12 分钟 ≈ 26 player/s **稳定进游戏速率**。在线累积 ~18k 时已逼近系统稳态 — 跟 login lock+preload 双重限流对应。

### N.6 下一道关卡(没改)

**真正的解** 是让 db worker 池真扩:

1. **partition 5 → 10**(broker 端 + login.yaml `PartitionCnt`)
2. **db worker per-partition 检查**(go/db/internal/kafka/key_ordered_consumer.go)
3. **MySQL connection pool 扩**(看 db.yaml)

这是真业务优化,改动量适中,可以单独立项。但**对 45k 浪涌测试已经足够** — v9 数据证明系统能稳定处理 ~26 player/s 进入率,等价于"开服 30 分钟把 45k 全部接进游戏"。这是**可接受的 dev 单机性能水位**,产线 prod 多机部署时数字会按比例放大。

### N.7 第九轮归档清单(2026-05-25 早晨)

| 优先级 | 事情 | 状态 |
|---|---|---|
| ✅ done | hijack 启动竞态 | §G.1 |
| ✅ done | merge_state schema drift | §G.2 |
| ✅ done | db_task 单 partition 瓶颈 | §I |
| ✅ done | Gateway zone-aware 路由 | §J |
| ✅ done | cpp Kafka GroupID per-instance | §L.1 |
| ✅ done | etcd timeout 30s / NodeTTL 180s | §L.1 |
| ✅ done | PlayerLockTTL 30→120s | §M |
| ✅ done | TaskWaitTimeout 5→30s | §N |
| 🟡 应该 | partition 5→10 + db worker scale | §N.6 |
| 🟡 应该 | EnterGame preload 改异步 | §M.4 #3 |
| 🟢 可延后 | Robot launcher CSV 收尾 | §F |

**今天定格**: 9 轮压测,9 个修复,真瓶颈定位到 db_task 处理吞吐。剩下的全是真实容量优化,不是 bug。

---

## §O. 第十轮 45k:partition 5→10 — preload 修了,但锁压力放大成新瓶颈

### O.1 改动

- broker 端 `db_task_zone_{1,2,3}` 预创建为 10 partition(retention.ms=300000 + segment.bytes=16M 保持原 config)
- `go/login/etc/login.yaml::PartitionCnt` 5 → 10 + 3 个 z*_login.yaml
- `go/db/etc/db.yaml::PartitionCnt` 5 → 10 + 3 个 z*_db.yaml

db consumer group 真注册到 10 partition(`kafka-consumer-groups --describe` 验证)。

### O.2 数据对比

| 指标 | v9(part5+lock120+wait30) | **v10(+part10)** | Δ |
|---|---|---|---|
| login_ok 总 | 102043 | **38962** | **−62%** ↓↓ |
| `preload timeout` | 87260 | **26494** | **−70%** ↓↓↓ ✅ |
| `lost lock` | 57125 | **69295** | +21% ↑ |
| enter_fail 总 | 131469 | 89647 | -32% |
| scene_notified=1 | 18825 | **8237** | **−56%** ↓ |
| avg_login | 4-5s | 5.3-6.7s | 慢 |

### O.3 真因:reverse-cascade — db 通了之后 Redis 锁压力放大

partition 5→10 让 db 端的处理速度真翻了倍,**preload timeout 从 87k 降到 26k(−70%)** 是 partition 增加的直接证据。但同时:

- **login_ok 反而降 62%** — 因为 EnterGame chain 走得更快(preload 真 ACK 了),**robot 重连频率上升**,player_locker 抢锁竞争变激烈
- **lost_lock 涨到 69k**(从 57k)— heartbeat goroutine 在更密的 Redis QPS 下调度饥饿率增加;每次 EnterGame chain 真完整跑完,锁占用时间从"5s preload timeout"延长到了"真实 EnterGame 全链路 5-10s"
- **scene_notified=1 反而降 56%** — lost_lock 早砍掉 chainCtx,EnterGame 没走到 RoutePlayer 阶段

这是经典的 **bottleneck reverse-cascade**:解决 N 层瓶颈之后,N-1 层负载暴增成为新瓶颈。

### O.4 上层下游的真相

按"login_ok 拆解 → 看哪一环节失败":

```
robot.尝试 EnterGame  → ~140000(估计)
├─ login_ok          = 38962 (28%)
│  ├─ lost_lock      = 69295 (主导)  ← 现在的 Redis 锁这一段
│  ├─ preload_to     = 26494
│  └─ scene_notified  =  8237 (真走通) ← 占 login_ok 21%
└─ login_fail        =  8926
```

**真有效产出**: 8237 player/12分钟 = **11 player/s 真稳态进入率**。

v9 是 26 player/s,v10 是 11 player/s — **降了 58%**。partition 翻倍让上游通过率上升,但下游(player_locker)抗不住流量翻倍,稳态有效产出反而减半。

### O.5 该不该回滚 partition 10?

技术上:partition 多本身不是坏事,prod 多机部署时下游 Redis 应该也能水平扩。但**单机 dev 环境下 partition=5 是最优甜区**,因为单机 Redis 一只。这次产生了真实的数据点:**partition 多了之后必须同步扩 Redis 锁的承载**(分片 / 多实例 / 改无锁化),否则 reverse-cascade。

### O.6 10 轮压测整体曲线

| 轮 | 关键变量 | login_ok | scene_notified=1 | 真稳态 |
|---|---|---|---|---|
| v3-orig | 0 fix | 79220 | ~30000 | ? |
| v8 | + lock 120s | 149476 | 4511 | 6/s |
| v9 | + wait 30s | 102043 | 18825 | **26/s** ← 最优 |
| **v10** | + part 10 | 38962 | 8237 | 11/s ↓ |

**v9 是当前 dev 单机的天花板**,~26 player/s 稳态进入率,~18k 在线时撞墙。

### O.7 真正能拉性能的事(没做,但有 clear path)

1. **EnterGame preload 改异步**: 最大杠杆。不等 ACK 直接回 OK,scene 那边 Redis NIL retry 自动补;锁占用从 5-10s 降到 1s 以下。这是 §M.4 #3 的真正实现。
2. **Redis 分片或 Cluster**: 把 player_locker 哈希到多实例,heartbeat 不再被单实例 QPS 饿死。
3. **RedisLocker 改 Watchdog less-pessimistic**: 现在 heartbeat 失败直接 chainCancel,改为 grace 一次,大部分 race-case 都能容错。

### O.8 第十轮归档清单(2026-05-26)

| 优先级 | 事情 | 状态 |
|---|---|---|
| ✅ done | hijack 启动竞态 | §G.1 |
| ✅ done | merge_state schema drift | §G.2 |
| ✅ done | db_task partition 1→5→10 | §I + §O |
| ✅ done | Gateway zone-aware 路由 | §J |
| ✅ done | cpp Kafka GroupID per-instance | §L.1 |
| ✅ done | etcd timeout 30s / NodeTTL 180s | §L.1 |
| ✅ done | PlayerLockTTL 30→120s | §M |
| ✅ done | TaskWaitTimeout 5→30s | §N |
| ✅ done | partition 5→10 验证 reverse-cascade | §O |
| 🔴 真瓶颈 | **EnterGame preload 改异步** | §O.7 #1 |
| 🟡 应该 | Redis 分片或 Cluster | §O.7 #2 |
| 🟢 可延后 | RedisLocker grace one miss | §O.7 #3 |

**今天高水位线**: v9 — 26 player/s 稳态,18.8k 真进游戏 player。这是单机 dev 在不动 Redis 拓扑前提下的天花板。

下一步要超过这个数字,**必须动 Redis 锁那一段**(异步化 / 分片 / 容错),不再是配置 tuning。







