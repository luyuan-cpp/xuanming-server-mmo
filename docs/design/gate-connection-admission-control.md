# gate 连接准入控制：muduo 无条件 accept 的业界解法与本仓落地 (2026-09-03)

> **状态：调研 / 设计文档，未落码。** 回答的是
> [`client_message_processor.cpp`](../../cpp/nodes/gate/handler/rpc/client_message_processor.cpp) 第 443-451 行
> 那段注释自己提出的问题——「muduo 的 `TcpServer` 无条件 accept，有没有更好的办法？业界标准做法是什么？」
> 以及 [`cpp/nodes/gate/SECURITY.md`](../../cpp/nodes/gate/SECURITY.md) 第 4 节末段自认的那个洞：
> 「该上限只约束资源总量，不替代握手超时、每来源限速或 L4 防护；**未认证连接仍可长期占满全部槽位**。」
>
> **本文推翻了两条流传中的前提：**
> 1. ~~gate 是服务端先说话（accept 后下发 session_id）~~ → **错**。`HandleConnectionEstablished`
>    全程零 `send`，session_id 只经 gRPC 发给 login/scene，从不写回客户端 socket。
>    gate 是**客户端先说话**，因此 `TCP_DEFER_ACCEPT` 在协议上是可用的（§6.3）。
> 2. ~~容量闸「在限流本身付出代价之前拒掉」~~ → **只对一半成立**。它拒在 `TcpConnection`、
>    两个 1KB Buffer、连接表插入、`epoll_ctl ADD` **全部做完之后**（§4.2）。
>
> 调研方法与证据分级见 §10。

---

## 0. 一句话结论

**muduo 无条件 accept 不是缺陷，是分层的正确位置；业界不存在「更聪明的 accept」，只有四道叠加的闸。**
libuv、Netty、asio、Go `net.Listener` 全都无条件 accept——「多少连接算多」只有业务知道。
真正的问题不是「muduo 该不该帮我挡」，而是**这道闸放在哪一层、用什么判据、拒绝路径有多贵**。

而本仓真正的缺口不在 accept 那一层：**一个纯数量上限没有时间维度，就等于把上限本身交给攻击者当武器。**
所有成熟系统无一例外都给未认证连接配了死线（sshd `LoginGraceTime` 120s、nginx `client_header_timeout` 60s、
Envoy `listener_filters_timeout` 15s、HAProxy `timeout http-request`），gate 一个定时器都没有。

---

## 1. 分层模型：四道闸，各挡各的，谁也替不了谁

| 层 | 手段 | 挡得住 | **挡不住** |
| --- | --- | --- | --- |
| **L3/L4（网络与云）** | Anycast、云清洗、L4 LB、XDP/eBPF | 带宽型（volumetric）洪水 | 完成握手后挂机；且清洗设备常把攻击**转化成**完成握手的形态 |
| **内核 socket 层** | `somaxconn`/backlog、`tcp_syncookies`、`TCP_DEFER_ACCEPT`、nftables `connlimit`/`hashlimit` | 伪造源 IP 的 SYN flood；零字节挂机连接（DEFER_ACCEPT） | 真实 IP 完成握手并发一个字节的连接 |
| **accept 层（进程内，未建应用对象）** | 停止 accept / 准入回调后直接 close | 稳态总量；每次尝试的用户态成本 | 公平性——内核队列是不可区分的 FIFO |
| **握手 / 会话层** | 未认证独立预算 + 死线 + 票据先行 + 每账号并发 | 「占着茅坑不认证」——**本仓唯一真正的洞** | 带宽耗尽 |

**读法**：`tcp_syncookies` 完全挡不住 gate 面对的主要威胁（攻击者用真实 IP 完成三次握手然后什么都不发，
cookie 一点忙不帮）；反过来，应用层再怎么改也吸收不了带宽型攻击。**没有单点银弹**——
凡是把某一层说成「配了就安全了」的建议，都是错的。

---

## 2. 路线之争：「停止 accept」vs「accept 后立刻关」

这是本题最容易站错队的地方。网上（和我最初的判断）常把「停止 accept」说成业界主流，
**这个说法需要限定**。

### 2.1 业界其实不是二选一，而是按「判据需不需要知道对端是谁」分工

- **不需要知道对端是谁**（总量、全局速率）→ 放在 accept **之前**，停止 accept：
  HAProxy 到 `maxconn` 时直接 `l->rx.proto->disable(l)` 摘掉监听 fd；
  Envoy 的 `envoy.overload_actions.stop_accepting_connections`；
  memcached 的 `accept_new_conns(false)`；Go `netutil.LimitListener` 先拿信号量再 `Accept`；
  Apache 靠 worker 忙完自然不 accept。
- **需要知道对端是谁**（per-IP、未认证预算、协议内容）→ **物理上只能**在 accept 之后：
  nginx `limit_conn`/`limit_req`、sshd `drop_connection`、Redis `maxclients`、
  Envoy 的 `connection_limit` network filter。

**分界线是信息可得性，不是偏好。**

### 2.2 对比表

| | 停止 accept（内核队列当拒绝引擎） | accept 后立刻 close（gate 现状） |
| --- | --- | --- |
| 服务端用户态成本 | **零**：零 fd、零对象、零 epoll 项、零 TIME_WAIT | 每条约 3.2KB 用户态 + 3 次 `epoll_ctl` + 6~7 次系统调用 |
| 客户端观感 | 三次握手已完成、`connect()` 已成功返回，但**收不到任何响应** → 表现为「卡在连接中」，客户端分不清「排队中」和「服务端挂了」 | 立即失败，客户端可快速重连别的 gate |
| 内核尾账 | 无 | 服务端是主动关闭方 → **60s TIME_WAIT**；稳态数量 = 60 × 拒绝速率 |
| 公平性 | **无**：内核队列是不可区分 FIFO，合法玩家排在 4000 条攻击连接后面就是等 4000 条 | 无（但至少快速失败） |
| 队列满之后 | 由 `tcp_abort_on_overflow` 决定：默认 0 = 丢掉第三次握手的 ACK、服务端重传 SYN-ACK，客户端指数退避 | N/A |
| muduo 实现难度 | `acceptChannel_` 是 private、`TcpServer` 零暴露 → **必须改 third_party** | 现成 |

### 2.3 对**这个 gate** 的取舍

**推荐：维持 accept-then-close 为主路线，不做「停止 accept」。** 理由三条：

1. **形态不同**。HAProxy 能排队是因为它是代理、后端会恢复；游戏网关面对的是玩家 TCP 连接，
   排队 = 玩家挂着等，体验比明确拒绝更差。memcached 后来专门加了 `maxconns_fast`
   （立即回错误再关）就是为了这个——**同一个软件里两条路线都实现了，快速失败是后加的那条**。
2. **本仓客户端有自动重连**。假连接会把「快速失败 + 换节点」退化成「长时间伪连接 + 雪崩」。
3. **停止 accept 不解决公平性**，而 gate 的真实痛点恰恰是公平性（攻击者占满槽位把真玩家挤出去），
   那要靠**分池**解决，不是靠换拒绝方式。

**但要补两件事**，把 accept-then-close 的成本压下来（§6.2 / §6.5）：把闸门前移到 `Acceptor`，
以及拒绝时用 `SO_LINGER(1,0)` 发 RST 消掉 60s TIME_WAIT。

> **反对意见（保留在案）**：若将来 gate 前面上了 L4 且真的遇到 volumetric 场景，
> 「极端过载时停止 accept」仍是有价值的最后一档降级（Envoy 的编排就是
> `shrink_heap → reduce_timeouts → disable_http_keepalive → stop_accepting_connections`，
> 停 accept 排在最后）。若要做，**必须双水位滞回**（如 0.95 停 / 0.85 开），否则每断一条连接就 flap 一次 `epoll_ctl`。

---

## 3. 业界做法速查表

| 系统 | 总量闸 | 时间维度（**关键列**） | 每来源 | 备注 |
| --- | --- | --- | --- | --- |
| **nginx** | `worker_connections`（容量，非闸门） | `client_header_timeout` 60s、`reset_timedout_connection`（`SO_LINGER` 0 发 RST） | `limit_conn` / `limit_req`（leaky bucket + burst + nodelay） | `ngx_accept_disabled` 的 1/8 退让**自 1.11.3 起是死代码**（`accept_mutex` 默认 off），别拿它论证「暂停 accept」 |
| **HAProxy** | 全局 `maxconn` → 停止 poll 监听 socket | `timeout client` / `timeout http-request` | stick-table + `track-sc` 的 `conn_cur`/`conn_rate`；`tcp-request connection reject` | `maxconnrate`/`maxsessrate` 的限速也是用「暂停 accept」实现的，不是丢包 |
| **Envoy** | Overload Manager：`global_downstream_max_connections`，文档明确要求 **≤ 系统 fd 上限的一半** | `listener_filters_timeout` 15s | `connection_limit` filter、`local_ratelimit` | 「资源压力」与「降级动作」解耦成两张表，是这一类里最完整的框架 |
| **sshd** | `MaxStartups start:rate:full`（默认 10:30:100） | `LoginGraceTime` 120s | — | **未认证连接预算的教科书实现**：random early drop，`p = rate + (100-rate)*(n-begin)/(max-begin)`，在 n=full 时才到 100%。网上「到 60 就全拒」的说法是错的 |
| **Redis** | `maxclients`，启动时把 rlimit 抬到 `maxclients+32`，抬不上去就**下调 maxclients** 并打日志 | — | — | 「上限永远不能吃满资源」+「预留内部 fd」 |
| **PostgreSQL / MySQL** | `max_connections` | — | — | 保留管理通道是硬规矩：PG `superuser_reserved_connections`；上游 MySQL 是 `admin_address`/`admin_port`（33062）——`extra_port` 是 MariaDB/Percona 的，别混 |
| **memcached** | `accept_new_conns(false)` **与** `maxconns_fast` 两条路线都实现了 | — | — | 最直接的路线对比证据 |
| **Go** | `netutil.LimitListener`（先拿信号量再 Accept） | **无** → 不配 `ReadHeaderTimeout` 就是完整的 Slowloris 洞 | — | 归还信号量依赖调用方真的 `Close`，漏一次永久泄漏一个槽位 |
| **Netty** | 无内建上限 | — | — | idiom 是关掉 server channel 的 `AUTO_READ` |

**提炼出的三条共识**：
1. **分层**——两条路线都要，按判据分工。
2. **分池**——未认证连接必须有独立的、远小于总量的预算。
3. **必须有时间维度**——**无一例外**。这一列是 gate 唯一全空的一列。

---

## 4. 本仓现状：做对了什么，漏了什么

### 4.1 已经做对的（不要在后续审计里重复报，也不要「优化」掉）

- **拒在分配 `SessionInfo` 之前**，且拒绝路径不回任何应答（`client_message_processor.cpp:468-490`）——
  给被拒连接回一条 tip 是净放大，攻击者花一个 SYN、你花一次 protobuf 序列化 + 一次 write。
- **日志采样纪律**：容量拒绝 1/1024（`:472-483`）、未认证流量拒绝（`:176-186`）、
  未绑定断开（`:571-584`）、accept 采样（`:571`附近）、codec 解析错误（`codec.cpp:118-131`）、
  未知 proto（`main.cpp:139-151`）。每一条都对应一次踩过的坑。
- **拒连路径统一 `setContext(kInvalidSessionId)`** 让断开回调早退（`:321`），
  避免把 TCP 洪峰放大成对 login 的 gRPC 洪峰。
- **fail-closed 三连**：prod 空 `gate_token_secret` 启动即 `LOG_FATAL`（`main.cpp:71-76`）、
  `gate_max_connections` 越界拒启动（`main.cpp:107-121`）、连接层纵深防御（`:495`/`:522`）。
- `codec` 的 `maxMessageLen` 已从 64MB 收到 64KB（`codec.h:64`），token 比较用常数时间（`:842`）。
- **muduo 官方示例反而是错的**：`examples/maxconnection/echo.cc` 在判上限前无条件 `LOG_INFO`，
  且用 `shutdown()` + `forceCloseWithDelay(3.0)` 让每条被拒连接多占 3 秒 fd。
  **本仓现在的直接 `forceClose` 是对的，不要「向官方示例看齐」改回去。**

### 4.2 漏了什么（逐条带文件:行）

| # | 缺口 | 证据 |
| --- | --- | --- |
| G1 | **未认证连接零死线、零收割器** | 全 gate 只有一个定时器：`main.cpp:134` 声明、`main.cpp:315` 每 10s 上报 player_count。`SessionInfo`（`session_info_comp.h`）**整个结构体没有任何时间戳字段**——今天就算想写 sweeper 也没有判据数据。`setKeepAlive` 在 `cpp/` 下零命中 |
| G2 | **闸门位置比注释声称的晚**：拒绝发生时 `TcpConnection`、两个 1KB Buffer、`connections_` map 节点、`epoll_ctl ADD` 已全部完成 | `TcpServer.cc newConnection` 先于我们的回调执行；`Buffer.h:45-46`（`kCheapPrepend=8`+`kInitialSize=1024`） |
| G3 | **只有总量、没有速率**。每秒建 5000 条又立刻关的攻击者永远碰不到 `:468` 那条闸 | `client_message_processor.cpp:468` 是唯一判据 |
| G4 | **`RLIMIT_NOFILE` 全链路无人设**：镜像没设、k8s 没有该 API 字段、进程从不 `setrlimit`。containerd 依赖 systemd v240+ 隐式默认 **soft=1024**——真正生效的上限很可能是 ~1000 而不是 20000，**`GateMaxConnections` 那条分支是够不着的死代码** | `Dockerfile.runtime`/`Dockerfile.cpp` 无 ulimit；`cpp/` 下 `setrlimit` 仅命中 Windows shim |
| G5 | **muduo 两行 per-connection `LOG_INFO` 抵消了全部采样纪律** | `TcpServer.cc:80`（newConnection）+ `:109`（removeConnectionInLoop）**无条件**；`Logging.h:20-29` 里 `INFO==2`；`node.cpp:1092-1098` 把 `log_level` **直接 `static_cast`** 进 `muduo::Logger::LogLevel`；而 `bin/etc/base_deploy_config.yaml:25` 是 `LogLevel: 2`，注释却写「2=WARN」——**注释错了，实际是 INFO，这两行现在就在打**。更糟：proto3 缺键得 0 = **TRACE** |
| G6 | **`sockets::accept` 对 `ENFILE`/`ENOBUFS`/`ENOMEM` 直接 `LOG_FATAL` → abort** | `SocketsOps.cc:125-156`。即便把连接上限调到 1，只要内核在 accept 时返回 `ENOBUFS`（连接洪水的典型产物）gate 照样崩。这条与 gate 自己的上限**完全正交** |
| G7 | **`Acceptor` 的 EMFILE 分支自带日志洪水**：`LOG_SYSERR` 在 `errno==EMFILE` 判断**之外**，LT epoll + EMFILE = 忙循环，每转一圈一行同步日志 | `Acceptor.cc` handleRead 的 else 分支第一行。应用层采样在这一层完全帮不上忙 |
| G8 | **`idleFd_` 不是可靠兜底**：`close(idleFd_)` 与 `::accept` 之间被别的线程抢走名额时 `idleFd_` 会变成 -1 且**没有任何重新装填路径**（重开 `/dev/null` 的返回值根本不检查），对进程剩余生命周期彻底失效 | `Acceptor.cc` EMFILE 分支。gate 进程里 grpc/redis/kafka 客户端都在别的线程开 fd，窗口是存在的 |
| G9 | **零 per-source 限制**，且**做之前有前置条件未满足**：`peerAddress()` 全仓只出现在 5 行日志里；而 `gate-entry` Service 未设 `externalTrafficPolicy` → 默认 `Cluster` → kube-proxy SNAT → `peerAddress()` 拿到的是 **node IP** | `k8s_deploy.ps1:928-947`（Service spec 只有 type/selector/ports） |
| G10 | **pre-auth 路径的按包防护为零**：`DispatchClientRpcMessage` 在 `:736` 的 `verified` 判定**排在** `ValidateClientMessage`（`:707`）之前；`DispatchTokenVerify`（`:780`）从头到尾不调限流器、也不调 `CheckMessageSize` | 未认证连接可反复让 gate 缓冲 64KB（`codec.h:64`）+ adler32 + 反射建 message + parse |
| G11 | **gate 容器 BestEffort**：无 `resources`、无探针、无 `securityContext`、无 PDB。`oom_score_adj=1000`，节点内存压力时最先被驱逐，且连接洪峰打的是整个 node | `k8s_deploy.ps1:560-632`（`New-NodeDeploymentYaml` 全文）。对照 go-svc 的 login/gateway 都配了 limits |
| G12 | **131071 硬上限是借来的**：`network_utils.h:12` 借 `snow_flake.h:26` 的 `kNodeBits=17` 当 seq 位宽。编译期打开 `ENABLE_SNOWFLAKE_TESTING` 会让它**静默变成 511**，并让默认的 `GateMaxConnections: 20000` 在 `main.cpp:116` 直接 `LOG_FATAL` | 两个语义完全无关的常量共用一个名字 |
| G13 | **应用侧零 Prometheus 埋点** | `cpp/nodes/gate` 全仓 grep 不到 prometheus。现在只能靠内核计数器 + 采样日志间接推断 |

### 4.3 一条要专门澄清的：`sessions().size()` 是**会话配额**，不是连接配额、更不是 fd 配额

被拒连接在 `:486-487`/`:499`/`:528` 只是 `forceClose`（**异步**，`queueInLoop`），
关闭完成前仍占着 fd。任一时刻 `真实 fd 数 = sessions.size() + 在途被拒数`。
另外 **18000 端口同时是节点 RPC 口**（`node.cpp:807-832` 只 bind 一个端口，
`main.cpp:303/307` 整体替换了回调），任何拨到该端口的 TCP 对端——端口扫描、探活、拨错的节点——
都会占一个会话槽。

---

## 5. 攻击者今天仍能做到的事（零凭证）

1. **占满全部槽位并永久持有**：建连后一个字节都不发。`SessionInfo` 在 `:568` 落地后
   **没有任何路径会回收它**。高峰期只需数千条空连接就能锁死所有新登录，
   而真玩家全部撞 `:468` 被静默 forceClose——**拒绝逻辑本身成了 DoS 的执行者**。
2. **connect/close 洪峰，永不触发上限**：总量闸对 churn 完全无效。每循环让 gate 付出
   1 个 fd、`TcpConnection` + 两个 Buffer、一次 `Generate()`、一次 map 插入/删除、
   `:325` 那次**无条件**的 `peerAddress().toIpPort()` 字符串构造，以及一个 60s TIME_WAIT。
3. **pre-auth 就能让 gate 缓冲并解析 64KB**（`codec.h:64`），1KB 的业务上限（`:202`）只对已验证生效。
4. **把 fd 推到 `RLIMIT_NOFILE`**，随后触发 G6/G7/G8 那一串。
5. **已验证会话可无限重发 `ClientTokenVerifyRequest`**（`:813` 直接 `sendReply(true,"")` 返回），
   每次都让 gate 序列化一条响应，既不过限流器也不计非法包。

---

## 6. 落地建议清单

> 排序原则：**先让闸门能生效，再加新闸门**。G4/G5 不修，后面所有工作都是死代码或被自己的日志抵消。

### P0-1 · 先量 fd 上限，并加启动断言（对应 G4）

- `main.cpp` 现有 `ValidateGateConnectionLimitOrDie()` 旁边加一条：`getrlimit(RLIMIT_NOFILE)`，
  若 `soft < GateMaxConnections + 余量`（节点间连接 + 日志 + gRPC）就 `LOG_FATAL` 拒绝启动。
- 顺手 `setrlimit` 把 soft 抬到 hard（通常 524288）——**这是进程自己就能做的，不需要动 k8s**。
  抬不上去时上一条的 FATAL 就是正确行为。参照 Redis 的做法（抬不上去就下调 `maxclients` 并打日志）。
- **不要**走 initContainer + privileged：那是为一个进程自己能改的值给节点提权。
  KEP-5758（per-container ulimits）至今只是提案。
- 验收：`cat /proc/1/limits`。**「我在 yaml 里设了 nofile」这句话通常是错的**。

### P0-2 · 未认证连接：独立预算 + 死线 + LRU 淘汰（对应 G1，堵 SECURITY.md 自认的洞）

这是**投入产出比最高**的一条，约 20 行，**不需要动 third_party**。

1. `SessionInfo` 加 `muduo::Timestamp connectedAt` 与 `muduo::net::TimerId preAuthTimer`。
2. `HandleConnectionEstablished` 在现有总量闸之后，再加一道 `preAuthCount >= kPreAuthMax`
   （取 `GateMaxConnections` 的 5-10%）。
3. `auto tid = conn->getLoop()->runAfter(kPreAuthTimeoutSec, makeWeakCallback(conn, &TcpConnection::forceClose));`
4. token 校验通过时 `loop->cancel(tid)`、`--preAuthCount`、置 `authenticated`。

**四个必须照做的细节，任何一个漏了这道防线就白做：**

- ⚠️ **必须用 `makeWeakCallback`**（`muduo/base/WeakCallback.h`，`forceCloseWithDelay` 内部就是这么用的）。
  直接绑 `TcpConnectionPtr` 会让定时器**强持有连接、续命到超时那一刻**，连对端主动断开都清不掉——正好帮了攻击者。
- ⚠️ **不能用现成的 `conn->forceCloseWithDelay(seconds)`**：它**不返回 TimerId**，认证成功后取消不掉，
  会把每条正常连接也在 N 秒后杀掉。
- ⚠️ **收割判据必须是「距 connect 的时长 + 尚未认证」，绝不能是「距上次活动的时长」**。
  按最后活动时间算，攻击者每 N 秒发 1 个字节就永久续命——这是这道防线被静默绕过的**头号方式**。
- ⚠️ **`preAuthCount` 必须用 RAII guard，不能手写 `++`/`--`**。现有代码已有 capacity 拒、
  `node_id=0` 拒、prod 空密钥拒等多个早退分支，漏一次就永久泄漏一个槽位且无超时兜底
  （Go `netutil.LimitListener` 的经典坑）。
- 超时值：gate 的 token 校验是同步的，**1-3 秒足够，别配到分钟级**。
- 满池时不要一刀切：按 sshd `MaxStartups` 的 random early drop 按概率拒（§6.7），
  否则攻击者只要稳定占住阈值，合法玩家成功率就是 0。
- 再叠一层 **LRU 淘汰最老的未认证连接**——把「新玩家被挡在外面」换成「攻击者的老连接先死」。

### P0-3 · 修 LogLevel（对应 G5）

- 改 `bin/etc/base_deploy_config.yaml:25` 的注释（`0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL`），
  并把生产值定到 **3(WARN)**。
- `Node::InitLogSystem`（`node.cpp:1092-1098`）加一条越界/缺键校验：proto3 缺键得 0 = TRACE
  是典型的「配置缺失静默降级成最坏值」。
- 否则 `TcpServer.cc:80` / `:109` 两行 per-connection INFO 会让**连接风暴照样是日志风暴**，
  handler 里 1/1024 的克制被完全抵消。

### P0-4 · 降级 muduo 的三条必崩路径（对应 G6，3 行改动）

`SocketsOps.cc` 的 errno switch 里，把 `ENFILE`/`ENOBUFS`/`ENOMEM` 从 `LOG_FATAL`
降级成 `LOG_ERROR` + 返回 -1（和 `EMFILE` 同路）。风险极低，收益是把一类**必崩**变成可观测的降级。

> **关于「不改 third_party」**：这在本仓不是真约束——`cpp/libs/engine/muduo_windows/`
> 已经是一份被本地魔改过的 muduo（整套 WIN32 分支）。但注意 `third_party/muduo`（Windows）
> 与 `third_party/muduo-linux` 是**两份独立副本，必须同改**，否则 Windows 编过、Linux 的 clang `-Werror` 红。

### P1-1 · 把容量闸前移到 `Acceptor`（对应 G2）

`TcpServer` 把 `acceptor_` 放在 private 且注释写着 "avoid revealing Acceptor"，
`newConnection` 是 private 且非 virtual——**继承路线不通，别试**。可行的最小侵入约 20 行：

```cpp
// Acceptor.h
typedef std::function<bool(int, const InetAddress&)> AdmissionCallback;
void setAdmissionCallback(AdmissionCallback cb);
// Acceptor::handleRead()，connfd >= 0 之后、newConnectionCallback_ 之前：
if (admissionCallback_ && !admissionCallback_(connfd, peerAddr)) {
    // 设 SO_LINGER(1,0) 后直接 close，连 TcpConnection 都不构造
    sockets::close(connfd);
    return;
}
```

判据与采样日志原样搬过来，省掉每条被拒连接的 `TcpConnection` 构造、2KB 缓冲、
连接表 insert/erase 和 3 次 `epoll_ctl`。

- ⚠️ **不要顺手改掉 `Acceptor.cc` 里那句 `//FIXME loop until no more`**。
  每次可读事件只 accept 一条，在这个场景下等价于 Envoy 的
  `max_connections_to_accept_per_socket_event=1`，是**免费的保护**：
  accept 风暴不会在单次事件里饿死同一 event loop 上已有连接的读写。改成 while 循环会直接放大伤害。

### P1-2 · 拒绝时发 RST 而不是 FIN（消掉 60s TIME_WAIT）

muduo 全库零 `SO_LINGER`，`TcpConnection` 也不暴露 fd，所以必须加 8 行：
`Socket::setLinger(bool, int)` + `TcpConnection::setLingerAbort()`。
若走了 P1-1，则连 `TcpConnection` 都不存在，直接对裸 `connfd` `setsockopt` 再 close 即可。

- 参照 nginx 的 `reset_timedout_connection`——**这就是它存在的理由**。
- ⚠️ 副作用：客户端拿到 `ECONNRESET` 而不是干净的 FIN。**只在被限流路径用 RST，正常关闭仍走 FIN**，
  并确认 UE 客户端能区分二者，否则会触发更激进的立即重连、把限流放大成重连风暴。
- ⚠️ RST 会丢弃未读数据，与任何「服务器已满」提示互斥；中间设备也可能过滤 RST。

### P1-3 · 全局新建连接速率闸（对应 G3）

**限速率、不限总量**，且**全局、不按 IP**（per-IP 的前置条件见 §7）。参照 HAProxy `maxconnrate`。
单 IO 线程下就是一个普通令牌桶，不需要锁。阈值必须能覆盖**正常洪峰**（§6.8）。

### P1-4 · 埋点（对应 G13）——**先埋点，再定阈值，顺序反了会同时误杀玩家和放过攻击**

必须的低基数 SLI：
- `gate_connections_current`
- `gate_unauthenticated_connections`（**未认证池占用率**，决定 `kPreAuthMax`）
- `gate_accept_rate`
- `gate_rejected_total{reason}`（reason 是有限枚举）
- `gate_handshake_duration`（**P99.9 决定死线值**）
- `gate_preauth_reaped_total`

🚩 **高基数红线**：`peer_ip` / `session_id` / `account` **绝不能进 metrics label**——
标签基数由攻击者控制，会把连接层 DoS 转成监控系统 DoS，而且监控挂了你就瞎了。
排障维度进日志/trace，不进 label。

### P1-5 · 部署清单补齐（对应 G9/G11）

⚠️ **改法与直觉不同**：C++ gate **没有静态 YAML**，它的 Deployment/Service 由
`tools/scripts/k8s_deploy.ps1` 的 `New-NodeDeploymentYaml`（:560）/ `New-GateServiceYaml`（:928）
在 apply 时 here-string 现拼。`deploy/k8s/manifests/go-svc/gateway.yaml` 和
`java-svc/gateway.yaml` **都不是它**（是 Go/Java 的 HTTP 网关，ClusterIP:8080/8081）。
改动必须落在那两个 PowerShell 函数里，并同步补一条
`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的契约断言（那里已经有对 `GateMaxConnections` 的断言先例）。

- `resources.requests/limits`——把 QoS 从 BestEffort 抬到 Burstable（`oom_score_adj` 立刻从 1000 降下来）。
  ⚠️ limit 一旦配上，`GateMaxConnections` 必须和它对齐，否则又是「真正生效的闸门不是你写的那个」。
- `readinessProbe` / `terminationGracePeriodSeconds` / PDB。
- `externalTrafficPolicy: Local`（或 LB 开 PROXY protocol）——**这是 per-source 限制从「不可能正确」
  变成「可能正确」的前提**，应排在写限速代码之前。代价：没有 gate pod 的 node 会拒绝该 NodePort 流量，
  外部 L4 LB 的健康检查必须能正确摘掉空 node。
- **safe sysctl**（pod 里 `securityContext.sysctls` 直接写，不用动节点）：
  `net.ipv4.tcp_keepalive_time`/`_intvl`/`_probes`（v1.29+）——这是部署层对应的死连接回收阀门；
  `net.ipv4.tcp_rmem`/`tcp_wmem`（v1.32+）调小能直接抬高同一内存预算下的连接天花板。
  ⚠️ keepalive 只杀**真死**的对端，活着不说话的攻击者杀不掉——那必须靠 P0-2。
- NetworkPolicy default-deny：现在全仓**零 NetworkPolicy**，集群里任意 pod 都能直连
  gate:18000 / scene:20000 / etcd:2379 / mysql:3306 / redis:6379 / kafka:9092。

### P2-1 · `TCP_DEFER_ACCEPT`（**前提已核实可用**）

**这条是 §0 那个更正的直接收益**：gate 是客户端先说话，所以内核可以在收到第一个数据包之前
不把连接交给应用——精准命中「握手完成但零字节挂机」这类连接，而且**零应用层成本**。

⚠️ 上线前必须实测 UE 客户端行为：`TCP_DEFER_ACCEPT` 超时后不是「放行」而是
**重传 SYN-ACK 直到 `tcp_synack_retries` 用完然后丢弃**。如果客户端是 connect 成功后先显示界面、
等玩家输入才发包，这些连接会被**无声丢掉**。

### P2-2 · per-source 限制（默认关闭，告警后开启）

前置条件（G9）解决后才有意义。而且**在游戏里这是高危操作**，见 §7。

### P2-3 · 「上限满了正常玩家进不来」的二阶问题

这是纯数量上限的**根本病**，不能靠调大数值解决（**线性抬高攻击者成本的同时线性抬高你的爆炸半径，比值永远不变**）。
可用的手段，按性价比：

1. 未认证池独立 + LRU 淘汰（P0-2）——**主力**。
2. Random early drop（sshd 公式）——避免悬崖。
3. **为重连预留配额**：分级降级的原则是「保住已在局内的玩家 > 接纳新玩家」，
   而断线重连比全新登录优先。参照 Redis/PG「保留管理通道」的同一条硬规矩。
4. 排队服——⚠️ 只是把「连接耗尽」换成「队列耗尽」，队列本身需要独立上限、超时和防刷
   （排队资格绑账号不绑连接）。不是容量问题的答案。

### P2-4 · 票据先行（时序改造，改动最大）

gate 的票据零件**全都有**（HMAC-SHA256 + expire + gate_node_id 绑定 + 常数时间比较），
问题纯粹在**时序**：它跑在分配 session id 和 `SessionInfo` **之后**。
⚠️ **换更强的算法不会带来任何改善**——票据的价值 100% 在「服务器分配任何 per-client 状态之前完成校验」。
这是 Valve SDR 的 `SteamDatagramRelayAuthTicket`、netcode.io 的 ConnectToken + challenge token
（slot 只在 challenge 成功后才分配）的共同形状。

---

## 7. 明确不推荐做的事

| 不要做 | 为什么 |
| --- | --- |
| **per-IP 硬并发上限** | CGNAT / 网吧 / 校园网 / 手游基站出口后面可能有几百上千真玩家（Cloudflare 数据：CGNAT IP 被限速的频率是非 CGNAT 的三倍）。要限就**限速率不限总量**，按 `/24`（IPv4）与 `/64`（IPv6，按 `/128` 等于没做——单主机在自己的 /64 里能生成 2^64 个源地址）聚合，默认关闭、告警后自动开启。有了票据先行之后，**per-账号并发才是正确的维度** |
| **自适应 / 动态限流**（Vegas、Gradient2、AIMD、CoDel） | 三条前提 gate 一条都不满足：长连接寿命小时级（调低上限对已有连接零作用）、延迟信号被玩家行为噪声淹没、误判 → 拒真实玩家 → 重连风暴 → 看起来更过载 → 继续调低，**是正反馈自杀环**。硬上限 + 压测标定在工程上严格更优 |
| **`setThreadNum` 来「扛更多连接」** | accept 永远在 base loop 单线程（`TcpServer::newConnection` 第一行就是 `assertInLoopThread`），线程池只分摊已建立连接的读写。而 `tlsSessionManager` 是 `thread_local`——一开线程池，20000 上限**静默变成 20000×N**，`rejectedCount` 变成数据竞争，`SessionIdGenerator` 劈开会让两个玩家拿到同一个 session_id（**会话劫持形状的 bug**，不只是资源 bug）。`:455-460` 那段警告注释必须保留，并应升级成启动断言 |
| **`SO_REUSEPORT`** | 同上会劈开会话表；且滚动升级时监听 socket 一关，它队列里的半连接与未 accept 的子 socket **全被内核丢弃**；分流是纯哈希静态的，不看队列深度 |
| **`net.ipv4.tcp_tw_recycle = 1`** | **Linux 4.12 已删除**，且删除前会打死所有 NAT 后面的客户端。这条建议至今遍布调优博客，**错了两次** |
| **`net.ipv4.tcp_tw_reuse = 1`** 治 TIME_WAIT | 只影响**出方向** `connect()`，对服务端 accept-then-close 产生的入方向 TIME_WAIT **完全是 no-op** |
| **`tcp_abort_on_overflow = 1`** | 把突发变成 RST → 一个 RST 直接变成登录失败弹窗，而默认的丢包重传能让开服洪峰自愈。`man tcp(7)` 明确警告「can harm the clients of your server」。且开了之后攻击者立刻知道什么时候把你打满了 |
| **为「安全/省带宽」关掉 `net.ipv4.tcp_timestamps`** | Linux 把窗口缩放/SACK/ECN 编码进时间戳选项低位，关掉后 syncookie 路径建立的连接**彻底丢失窗口缩放和 SACK**（窗口钉死 64KB、丢包只能 Go-Back-N）。两条各自「标准」的加固项互相抵消 |
| **调大 `somaxconn` 来「扛过载」** | 更大的 accept 队列只是让更多客户端在黑洞里多卡一会儿再失败，backlog 越大过载时 connect 的 p99 越差。backlog 应按吸收**微突发**定。另外 ⚠️ `::listen(fd, SOMAXCONN)` 里的 `SOMAXCONN` 是 **glibc 头文件宏**（老版本=128），不是 sysctl 值，实际队列 = `min(编译期宏, net.core.somaxconn)`——必须在**构建镜像上**实测 |
| **为设 `net.core.somaxconn` 给 kubelet 加 `--allowed-unsafe-sysctls`** | 它在 k8s 的 unsafe 列表，要每个节点改 kubelet 启动参数——为一个不紧的约束换一个节点级的口子 |
| **tarpit / silent-drop** | 游戏客户端有自动重连，被吊住会表现为「连上了但没反应」直到 TCP 超时，把一次明确失败变成一次长时间伪连接。HAProxy 官方也警告不要对合法用户用 |
| **给被拒连接回一条 tip / 写一行日志** | 净放大。客户端侧的指数退避才是解决「玩家不知道为什么连不上」的地方 |
| **`connlimit`/`hashlimit` 而不管 conntrack** | 这两个 match 强制启用 conntrack，`nf_conntrack: table full` 时丢的是**所有人的包**，包括同节点其他 Pod 已建立的连接。在 iptables 模式 kube-proxy 的节点上，每条被拒连接的 TIME_WAIT 占 conntrack 条目 **120 秒**（是内核 TIME_WAIT 的两倍）。**你被打，顺手把邻居一起打掉** |
| **「前面挂个 L4 清洗就行了」** | 对 TCP 游戏流量，清洗设备做 SYN-proxy，会把每一次攻击都转换成**完成握手**的攻击——正是你的上限唯一挡不住的那一种；同时毁掉源 IP。**必要但绝不充分，而且它改变了威胁的形态** |
| **上了 L4 却仍把源站真实地址下发给客户端** | 前置的全部价值建立在「真实 IP 从不泄露」上。需要检查 AssignGate 链路下发的到底是什么地址 |
| **「改用 UDP/QUIC 就没有握手成本了」** | 没有 cookie/Retry 的 UDP 监听器是**反射放大器**；QUIC 的 Retry token 就是同一道准入控制题搬进用户态，实现还归你维护 |
| **「拒绝 N 次就封 IP」** | 把攻击者可控的 IP 写进持久封禁表 = 状态炸弹；CGNAT / 代理后面会封掉真玩家，甚至封掉 LB 自己 |

---

## 8. 正常洪峰 ≠ 攻击：两者需要的处置不同

这是最容易混为一谈、也最容易在生产上误伤的一处。

| | **开服 / 维护后重连风暴 / 跨天重登** | **攻击** |
| --- | --- | --- |
| 形状 | 高 λ（新建速率），**每条都会很快完成认证**，握手耗时分布正常 | 高 L（存量占用）**或** 高 λ 但**永不认证** |
| 关键观测量 | 未认证池**占用率低但周转极快** | 未认证池**占用率高且不周转** |
| 正确处置 | 排队 / 削峰 / 提高 accept 吞吐；**不能**靠拒绝 | 收割 + 淘汰 + 拒绝 |
| 会误伤的手段 | 速率闸阈值订低、per-IP、random early drop | — |

**推论**：`gate_unauthenticated_connections` 与 `gate_handshake_duration` 这两个指标不只是用来定阈值，
它们是**区分洪峰与攻击的唯一判据**。没有它们，任何自动处置都在赌。

⚠️ **最坏 case 是冷启动 + 全量重连**：gate 重启后 0 连接，全服客户端同时重连，几秒内打满上限，
而这批连接**全部**需要做 HMAC token 校验，CPU 峰值远高于稳态。
**只测稳态满载会给出一个在真实故障恢复时必然失效的容量数。**

---

## 9. 验收与压测

### 9.1 先分清证据在哪一层（整份验收的核心）

内核计数器（`TcpExtListenOverflows` / `ListenDrops` / `SyncookiesSent` / `TCPReqQFullDoCookies`）
反映的是 **L4 洪水打没打进来、accept 有没有跟上**，
**不反映** `GateMaxConnections` 在不在起作用——后者放行了握手、`accept()` 照样返回，
只是随后 forceClose。**盯错指标会得出「限连没用」的假结论。**

### 9.2 命令

```bash
# 内核:accept/SYN 队列
nstat -az | grep -Ei 'ListenOverflows|ListenDrops|SyncookiesSent|TCPReqQFullDoCookies'
ss -lnti 'sport = :18000'   # LISTEN 行:Recv-Q=当前 accept 队列长度, Send-Q=生效的 backlog
grep -r SOMAXCONN /usr/include/   # 在构建镜像上实测编译期宏

# 进程
cat /proc/1/limits                 # 唯一可信的 nofile 验证
ls /proc/1/fd | wc -l
cat /sys/fs/cgroup/memory.current
ss -s                              # TIME_WAIT 计数
conntrack -C; cat /proc/sys/net/netfilter/nf_conntrack_max
```

### 9.3 复现

- **空连接洪水（最贴合本威胁）**：python asyncio 裸 `open_connection` 循环，建完就挂着不发包。
  ⚠️ **不要**直接用 `robot/` + `robot_stress.ps1`：它们过完整登录 + token 校验，
  连接是**已认证**的，测不到真正的威胁，且 50ms stagger 的速率远不够洪水量级。要旁路写裸 dial 循环。
- **SYN flood**：`hping3`，需 root/`CAP_NET_RAW`，⚠️ **必须在 network namespace(veth 对) 里做**——
  `--rand-source` 的伪造洪水会溢出到真实网络和上游设备。
- ⚠️ **不要在 Windows 本机跑真规模**：asyncio 无 `SO_REUSEADDR`、源端口 ~64K 上限，
  单机打不满 2 万还会误报端口耗尽。真规模实验放 Linux。

### 9.4 判据

攻击进行中：**已登录玩家 p99 延迟不变**、**新玩家登录成功率 > 阈值**、**进程 RSS 不涨**。
（注意第二条——这正是纯数量上限今天做不到的那一条。）

### 9.5 一条未认证连接的真实内存必须**实测**

⚠️ 不要用 muduo 的 2MB 高水位常量或 Buffer 的 1KB 初始值去估：前者是「越过就断」的**触发器**
不是分配上限（往 `outputBuffer_` 追加是无界的），后者漏算了 socket/对象/内核结构。
**建 N 条空连接，量 RSS 差。** 估算量级供对照：用户态约 3.2KB（其中两个各 1KB 的 Buffer 占 65%，
而一条从不发数据的连接永远用不到它们）+ 内核态约 3KB ≈ **每条 6KB**。

⚠️ 定容时 `GateMaxConnections` 与每连接高水位是**相乘的**：20000 × 2MB = 40GB 理论最坏
output buffer。单独看每个都合理，乘起来超过任何合理预算。

---

## 10. 调研方法与证据分级

本文由 9 个并行调研 agent 的产出合成（3 个一手代码测绘：muduo 源码 / gate 会话链路 / 部署暴露面；
6 个业界调研：内核 L4 / 主流服务端 / 游戏网关 / 过载理论 / 反面挑刺 / 验收方法），
原始结构化产出保留在会话 scratchpad 的 `research/*.json`。

**agent 之间出现过两处直接冲突，均已由我逐条读码裁决**：

| 冲突 | 裁决 | 依据 |
| --- | --- | --- |
| gate 是服务端先说话 vs 客户端先说话 | **客户端先说话** | `HandleConnectionEstablished`（`:439-579`）全程零 `send`；我自己读了 `:544-580` 确认 |
| 「停止 accept」是不是业界主流 | **按判据分工，且本仓不该做**（§2） | memcached 同时实现两条路线且后加 `maxconns_fast`；nginx 的 `ngx_accept_disabled` 自 1.11.3 起是死代码 |

**我亲自核实过的**（可直接引用）：`Logging.h:20-29` 的 `INFO==2`；
`node.cpp:1092-1098` 的直接 `static_cast`；`base_deploy_config.yaml:25` 注释错误；
`TcpServer.cc:80/:109` 的无条件 `LOG_INFO`；`Acceptor.cc` handleRead 全文与 EMFILE 分支；
`k8s_deploy.ps1` 的 gate 清单生成器与 `:363-369` 的暴露警告；
`HandleConnectionEstablished` 零 `send`。

**⚠️ 仍标注为「model-knowledge（未联网核实）」、落码前需复核的关键论断**：

1. `TCP_TIMEWAIT_LEN = 60s` 不可 sysctl 调。
2. 内核侧 per-socket 内存开销约 3KB（§9.5 要求实测，不要引用这个数）。
3. kube-proxy 在 `externalTrafficPolicy: Cluster` 下 SNAT 源 IP（G9 的整条推论依赖它）。
4. glibc 现代版本 `SOMAXCONN = 4096`（§7 已要求在构建镜像上实测）。
5. nginx `client_header_timeout` / HAProxy `timeout http-request` / gRPC `MAX_CONNECTION_AGE` 的确切语义。
6. `nf_conntrack_max` 默认值算法在 5.15 前后不同（照抄老文档可能差 4 倍）。
7. Cloudflare 关于 CGNAT IP 被限速频率的具体倍数。

**未做的事**：没有跑过任何真实压测；本文所有容量与开销数字都是静态推算，
按 §9 的口径实测之前不能写进部署配置。
