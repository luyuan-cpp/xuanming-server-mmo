# gate 与客户端的连接存活:握手期限、应用层心跳与空闲超时

> 2026-11-06,Claude。负责人 2026-11-05 要求补齐断线重连的应用层,本文是其中「gate 及时发现客户端掉线」这一块。
> C++ gate 与 robot 已落码,**未编译、未测试**,验证步骤见 §11。客户端仓与 Java 版待做,见 §9、§10。
> 同日负责人确认项目未上线、玩家数据可以清空、不考虑旧客户端,所以按标准做法:验签成功即开始判死,未验签连接有握手期限(§6)。

## 0. 一句话

gate 对每条客户端连接设两道期限,到了都只调 `forceClose()`:

- 建连 10 秒内必须验签;
- 验签成功的回包里通告心跳参数,从这一刻起连续 30 秒收不到这条连接的任何一帧就关。

客户端每 10 秒发一条心跳,gate 每条都回。关掉之后由既有的断开回调代发 `ExitGame(DISCONNECTED)`,走断线保留(`player-connection-lost-hold.md`)。

## 1. 现状与缺口(2026-11-05 核实)

- 客户端协议里没有心跳消息。`proto/` 下能搜到的心跳只有节点链路的 RPC0 `HEARTBEAT` 和 etcd 的 KeepAlive。
- gate 对客户端连接没有任何时限。muduo 只开了系统默认的 `SO_KEEPALIVE`,Linux 空闲 2 小时后才开始探测。
- 验签之后:手机断网、NAT 映射过期、基站切换后 IP 变了,这类掉线都不会有 FIN / RST 到达 gate,角色一直按在线处理:
  - gate 有数据要推时,要等 TCP 重传耗尽(Linux `tcp_retries2` 缺省约十几分钟)才报错;
  - 没有数据要推时,要等 keepalive(2 小时以上)。
- 验签之前:建连之后不验签、一帧不发的连接永远占着槽位。`gate-connection-admission-control.md` 把它列为头号缺口(G1),`cpp/nodes/gate/SECURITY.md` §5 也自认这个洞;Java gate 已有 30 秒握手时限。
- 断线之后的处理已经做好:gate 在 `HandleConnectionDisconnection` 代发 `ExitGame(DISCONNECTED)`,scene 把角色原地留 25 秒等重连。缺的只是「gate 及时知道连接已死」这一步。

## 2. 目标与非目标

目标:

- 验签之后客户端静默掉线,gate 在最后一帧之后 30–31 秒内关闭连接;建连 10 秒仍未验签的连接在 10–11 秒内关闭。
- 关闭只调 `forceClose()`。断线通知、断线保留、login 断线租约都走既有的断开回调,不新增任何业务逻辑(负责人 11-05 原则:连接层只做建连、握手、Ready)。
- 参数只在 gate 一处定义;心跳间隔与判死阈值经验签回包通告,客户端照通告执行。
- 每个 gate 只管自己的连接,没有跨进程状态,随 gate 水平扩展。

非目标:

- 客户端怎么发现 gate 已死、怎么自动重连。属于客户端仓,本文只写协议约定(§9)。
- 未认证连接的独立预算、LRU 淘汰、按概率早拒(`gate-connection-admission-control.md` P0-2 的其余部分)。理由见 §12。
- 节点链路的心跳(`muduo-rpc-channel.md` §21,已做完)、战斗直连 battle 的客户端连接(battle 自有 10 秒握手期限)。
- 调系统 TCP keepalive 参数。它保持开启,作为最后一道兜底。

## 3. 协议(客户端可见,两版服务器同批改)

全部在 `proto/common/base/message.proto`,与 `ClientTokenVerifyRequest` / `ClientTokenVerifyResponse` 放在一起:

```proto
message ClientTokenVerifyResponse {
  bool success = 1;
  string error = 2;
  uint32 heartbeat_interval_ms = 3;  // 新增。0 = 这台 gate 不支持心跳
  uint32 heartbeat_timeout_ms = 4;   // 新增
}

message ClientHeartbeatRequest  { uint64 seq = 1; }  // 客户端 → gate
message ClientHeartbeatResponse { uint64 seq = 1; }  // gate → 客户端,回显 seq
```

- **为什么是独立的顶层消息,不走 `ClientRequest`**。心跳是连接层的事,与验签同层:
  - 不占 message_id,不进生成的方法注册表、客户端号白名单和 `MessageLimiter`;
  - 不进路由 lane,不转发给任何后端;
  - gate 本地收、本地回。
  - 帧格式不变,仍是 gate codec 的「类型名 + 载荷 + 校验和」,与验签消息一样按类型名派发。
- **通告即要求**。`success = true` 时 gate 填两个字段,失败回包不填。客户端收到非 0 的间隔,就必须在收到回包后立即发第一条心跳、之后每个间隔一条;gate 从发出这个回包起按 `heartbeat_timeout_ms` 判死。dev 免验签模式下会话建连即已验证,客户端照常发的验签请求仍回 `success = true`,同样带参数。
- **`seq`** 由客户端自选,gate 原样回显。客户端用它把回包对上请求、测往返时延。gate 不解读它。
- **字段号**:项目未上线,3、4 是 `ClientTokenVerifyResponse` 的新号;上线后照 AGENTS §4 不复用。

## 4. 取值(`session/comp/client_liveness.h` 的 `kClientLiveness`)

| 常量 | 值 | 理由 |
|---|---|---|
| `handshakeTimeout` | 10s | 合法客户端建连后立即发验签,毫秒级完成;移动网络上首个数据段丢一两次(初始重传 1s、再 2s)也在几秒内到。与 battle 直连的握手期限(`battle_client_edge.h` `kHandshakeTimeoutSec`)同值。准入控制文档 P0-2 建议的 1–3 秒对移动网络太紧;Java gate 现在是 30 秒,两版要对齐(§10) |
| `interval` | 10s | 客户端发心跳的间隔。2 万连接的 gate 每秒约 2000 条心跳、2000 条回包,每条几十字节,相对玩家流量可以忽略。远短于运营商 NAT 的 TCP 空闲回收时间,也起到保活映射的作用 |
| `idleTimeout` | 30s = 3 × interval | 从收到上一帧算起。正常节奏下,要连着丢两个心跳、第三个也没准时到才会判死。手机短暂断网时,TCP 重传按指数退避(1、2、4、8 秒…),网络恢复后最长再隔十几秒数据才到,30 秒能容下约 15 秒的断网而不误判 |
| `checkPeriod` | 1s | 扫描节拍,精度 ±1s。两道期限都只会晚、不会早 |
| `maxHeartbeatsPerInterval` | 3 | 一个 interval 窗口里最多回 3 条,窗口从窗口里第一条心跳算起。断网恢复后 TCP 会把积压的几条心跳一次送到,这个余量容得下;正常节奏与两倍速都碰不到。超出的不回(§5.2 第 4 步) |

- **比节点链路(5s / 15s)宽**。节点链路在机房内部,抖动小;客户端在公网和移动网络上,15 秒会把一次电梯、隧道里的断网判成掉线,逼客户端重连。判死晚 15 秒的代价只是:真掉线时,角色在原地多停 15 秒。
- **掉线到下线的总时长**:最后一帧之后 30–31 秒判死,再加断线保留的 25 秒,角色最多在原地停约 56 秒。原来要等 TCP 自己报错,十几分钟到两小时以上。
- **与断线保留、login 断线租约的关系**:这两个计时都从 gate 发出断线通知那一刻开始,本文只把这一刻提前,`player-connection-lost-hold.md` §7 的不变量(保留 25s + 扫描 1s + 余量 4s ≤ 租约 30s)不受影响。
- **写成编译期常量**,`static_assert` 锁住 `idleTimeout = 3 × interval` 与参数合法性。客户端照通告执行,所以不存在「只改一端就混跑」的问题;以后要调,只改 gate 这一处,两版服务器取同值。

## 5. gate 行为

### 5.1 一条连接的两道期限

```
建连 ──ExpectHandshake──▶ 握手检查点:建连 + 10s
  │                         到期时仍未验签 → forceClose(handshake_timeouts)
  ▼
验签成功的回包 ──OnTokenVerified──▶ 通告 10000 / 30000,开始跟踪;空闲检查点:回包时刻 + 30s
  │                                  到期时距上一帧不足 30s → 按「上一帧 + 30s」重新登记
  │                                  已满 30s → forceClose(idle_closed)
  ▼
每一帧(验签、ClientRequest、心跳)──NoteInboundFrame──▶ 刷新「上一帧」
```

- **握手期限按「距建连的时长 + 仍未验签」判**,期间收到过字节也不续期。按「距上一次活动」算的话,对端每隔几秒发一个字节就能永远占着槽位,这是这道防线被静默绕过的头号方式(准入控制文档 P0-2)。登记点在 `HandleConnectionEstablished` 插入会话之后;dev 免验签的会话建连即已验证,不登记。
- **空闲超时从验签成功的回包起算**。三条成功路径都调 `OnTokenVerified`:正常验签、已验证会话重发验签、dev 免验签。重复调用只填参数,不重复登记。
- dev 免验签模式下,客户端如果没发验签请求就直接发心跳,第一条心跳开始跟踪(§5.2 第 5 步);既不验签也不发心跳的连接,gate 从没通告过参数,不跟踪。生产模式(prod 空密钥拒启)没有这条路径。

### 5.2 收到心跳

`gate_client_heartbeat::OnHeartbeatRequest`(`cpp/nodes/gate/handler/rpc/client_heartbeat.cpp`),按顺序:

1. 按连接上下文的 session_id 找会话。找不到:`forceClose`。与 `ClientRequest` 找不到会话的处置相同。
2. 记一次「收到一帧」(§5.3)。
3. 会话未验签:`forceClose`,计 `before_verify`。与验签前发 `ClientRequest` 的处置相同。
4. 本窗口已回满 3 条:不回,计一次非法包,计 `excess`;非法包到阈值(`GATE_ILLEGAL_PACKET_THRESHOLD`,缺省 50)时 `forceClose`。
5. 其余:回 `ClientHeartbeatResponse{seq}`,计 `answered`。这条连接如果还没在跟踪(只有 §5.1 末段那条 dev 路径),从这一条起跟踪。

### 5.3 任何一帧都算活着

三个收包入口都在找到会话之后记一次「收到一帧」:`DispatchTokenVerify`、`DispatchClientRpcMessage`、心跳。被拒绝的帧也算,因为它同样证明对端活着;被拒的帧另有非法包计数管着。

- **按完整的帧算,不按字节算**。节点链路按字节算(§21.5),因为对端是自己人。公网端口按字节算的话,对端每隔几秒发一个字节、永远凑不成一帧,就能让连接一直不被判死。完整的帧要过 codec 的长度、校验和与类型检查。客户端合法帧都很小(`ClientRequest` 不超过 1KB),慢网络上也是瞬间收完。
- **还没开始跟踪的连接(验签之前)不读时钟**。跟踪中的连接每帧读一次 `steady_clock`(约 20ns),相对每帧的解码与转发开销可以忽略。

### 5.4 扫描与判定

主 loop 上一个 1 秒的定时器(`GateRuntimeContext::clientLivenessSweepTimer`)驱动 `SweepLiveness(now)`。

- **数据结构**:一个按到期时刻排序的小根堆,检查点是 `(到期时刻, session_id, 会话代次, 种类)`。一条连接至多同时有两个:握手检查点(验签之后作废)与空闲检查点(跟踪之后恰好一个)。
  - 到期时取出,核对会话还在、代次相同;否则丢弃。会话关闭时不用摘检查点,到期时自然丢掉;session_id 被新连接复用时,代次对不上,也丢掉。
  - 握手检查点:已验签就丢弃;仍未验签就关。
  - 空闲检查点:距上一帧不足 30 秒,活着,按「上一帧 + 30 秒」重新登记;已满 30 秒,关。
- **开销**:收包路径只写一个时刻,不碰堆;每条跟踪中的连接每 30 秒左右被检查一次,外加建连时一个握手检查点。2 万条连接每秒约 700 次查表。不按秒全量遍历会话表:2 万个会话结构遍历一次是毫秒级,gate 唯一的 loop 每秒卡一下不划算。
- **本端卡顿不怪对端**,同节点链路 §21.5。本次扫描距上次超过 2 秒,说明 gate 的 loop 卡过,客户端的帧(包括验签请求)可能还在接收缓冲里没读。这一轮两道期限都不关,改约 1 秒后的下一轮再判(`deferred`)。
- **关闭只做一件事**:`forceClose()`。
  - 之后由 muduo 的断开回调进 `HandleConnectionDisconnection`:向 scene 代发 `ExitGame(DISCONNECTED)`(断线保留),向 login 发断线通知(断线租约),清路由 lane 与观战订阅,删会话。没验签的会话没有角色、没向 login 发过登录,断开回调只删会话。
  - 不重放、不补发、不遍历玩家。
  - 关闭后检查点不再登记,同一条连接不会被关第二次。

### 5.5 与现有防线的关系

| 防线 | 关系 |
|---|---|
| 非法包计数 `illegalPacketCount` | 超频心跳每条计 1,与超限、坏号、限流同一个计数和阈值。正常客户端不会碰到 |
| `MessageLimiter`(按 message_id 限流) | 心跳不是 `ClientRequest`,不经过它;频率由 §5.2 第 4 步自己管 |
| 高水位强关(输出缓冲 2MB) | 互不相干。客户端不读数据时,回包与推送一起堆积,照旧强关 |
| 连接数上限 `GateMaxConnections` | 互不相干。两道期限关掉的连接腾出名额;未验签的连接每条最多占 10 秒 |
| 不认识的消息类型(派发器缺省回调强关) | 心跳类型已注册,新 gate 不会因此强关。旧 gate 会,所以客户端只在收到通告后才发(§6) |
| 验签前的 `ClientRequest` 强关 | 验签前的心跳同样强关 |
| 断线租约到期关残留连接(`exitSceneImmediatelyOnClose`) | 互不相干。哪条路先关,断开回调就按哪条路的原因走 |
| 停机 / 维护关源(`sourceClosed`) | 关源之后收包回调直接丢弃并强关;会话全关之后扫描是空操作 |

### 5.6 代码结构

- 纯逻辑 `cpp/libs/services/gate/session/comp/client_liveness.h`:参数 `ClientLivenessPolicy` 与 `static_assert`、每会话状态 `ClientLiveness`、心跳准入 `AdmitHeartbeat`、判死 `JudgeIdle`、检查点堆与扫描 `LivenessSweeper`。只依赖 STL,不读时钟,时刻由调用方传入;验签与否取自 `SessionInfo::verified`,不另存一份。
- 装配 `cpp/nodes/gate/handler/rpc/client_heartbeat.{h,cpp}`:`ExpectHandshake`、`OnTokenVerified`、收心跳、`SweepLiveness`、计数与日志。状态是 gate loop 线程上的 thread_local,与路由 lane 同一做法;定时器回调不捕获任何对象(AGENTS §11.7)。
- 接线:
  - `SessionInfo::liveness`;
  - `GateClientSessionHandler` 构造时注册心跳回调;
  - `HandleConnectionEstablished` 登记握手期限;
  - `DispatchTokenVerify` / `DispatchClientRpcMessage` 记「收到一帧」,验签成功回包调 `OnTokenVerified`;
  - `main.cpp` 装 1 秒扫描定时器,维护停中继与拆除时取消。

## 6. 兼容与默认开关

**默认开,没有开关。** 项目未上线,负责人 2026-11-06 确认玩家数据可以清空、不考虑旧客户端,所以不做「只对发过心跳的连接判死」这类兼容。

| gate | 客户端 | 行为 |
|---|---|---|
| 新 | 新 | 验签后发心跳;掉线 30 秒内关;不验签的连接 10 秒内关 |
| 新 | 旧(不发心跳) | 验签后 30 秒一帧不发就被关:挂机不动的玩家会掉线。**旧客户端不能连新 gate,客户端要与服务器同批升级** |
| 旧 / Java 版未实现 | 新 | 回包里没有参数(0),客户端不发心跳。旧 gate 不认识心跳类型,发了会被强关,所以这条是硬约定 |

- robot 已经带上心跳(本仓 `robot/pkg/heartbeat.go`),压测与冒烟不受影响。
- 客户端要在验签成功后立即发第一条心跳,不要等满一个间隔(§9)。
- 生产模式下没有不验签的路径;dev 免验签时,既不验签也不发心跳的客户端不受空闲超时约束(§5.1 末段)。

## 7. 可观测性

- **空闲关闭**:每次一行 INFO。绑定了角色的连接逐条打;未绑定角色的连接每 1024 次打一条,理由同断开日志。客户端掉线是正常事件,不是服务端故障,所以不用 WARN。
  `[ClientHeartbeat] idle timeout, forcing close: session_id=<n>, player_id=<n>, silent_ms=<n>, heartbeat_timeout_ms=30000, idle_closed_total=<n>`
  随后照常有一行 `Client disconnected, ... exit_reason=EXIT_GAME_REASON_DISCONNECTED`。
- **握手超时、验签前心跳、超频心跳**:采样 WARN,每 1024 次一条,口径同其他未认证流量的拒绝日志:
  `[ClientHeartbeat] connection rejected (sampled), reason=handshake_timeout|heartbeat_before_token_verify|heartbeat_over_rate, latest_session_id=<n>, rejected_total=<n>`
- **推迟判定**:gate 的 loop 卡过 2 秒以上才会出现,本身是该看的服务端问题。一次卡顿一行 WARN,不按连接打:
  `[ClientHeartbeat] liveness verdicts deferred, local loop stalled: check_gap_ms=<n>, deferred=<n>, deferred_total=<n>`
- **汇总行**:每 30 秒检查一次,累计计数有变化才打:
  `[ClientHeartbeat] summary answered=<n> excess=<n> before_verify=<n> tracked=<n> idle_closed=<n> handshake_timeouts=<n> deferred=<n> pending_checks=<n>`
  - `tracked` 是开始按空闲超时判死的连接数,正常等于验签成功的连接数;
  - `pending_checks` 是检查点堆的大小,含已关闭会话、已验签连接还没到期的检查点,只作观测;
  - 指标只有进程级计数,没有按玩家或会话的维度。
- `stress_summarize.ps1` 的「Gate client heartbeat」段读每个 gate 日志的最后一行汇总。
- 正常运行时 `excess`、`before_verify`、`deferred` 应为 0。`idle_closed` 随客户端真实掉线增长;`handshake_timeouts` 正常接近 0,持续增长说明有人建连不验签(扫描或攻击)。

## 8. 测试

- **纯逻辑 `ClientLiveness*`**(`cpp/nodes/gate/tests/client_liveness_test.cpp`,`routing_identity_test` 工程,合成时刻,22 个):
  - 参数合法性;开始跟踪只一次,未跟踪不记时刻;
  - 心跳准入:验签前拒绝;已跟踪的只回答;没跟踪的第一条开始跟踪;窗口内第 4 条超频,窗口满 interval 后重置;正常节奏、两倍速、积压 3 条都不超频;
  - 判死边界:差 1ns 不满 30 秒为活着,恰好 30 秒为判死,迟到的扫描推迟;
  - 握手期限:恰好 10 秒关、只关一次;期限前验签则作废并转入空闲检查;迟到的扫描推迟,恢复后读到验签请求不关;
  - 空闲扫描:到期才检查;活着的按上一帧重新登记、判死只回调一次且不再登记;会话不在 / 代次不符 / 未跟踪的检查点丢弃;迟到扫描推迟、下一次准时扫描才判;首轮扫描不算迟到。
- **装配 `GateClientHeartbeat.*`**(`cpp/nodes/gate/tests/client_heartbeat_test.cpp`,13 个):会话的连接是真实回环 TCP,结果在客户端侧收字节比对;「没有写」用哨兵帧证明。
  - 验签回包通告 10000 / 30000 并开始跟踪,重发验签不重复登记;
  - 心跳回带同一 seq 的回包;没验签请求就来的心跳开始跟踪;验签前的心跳关掉连接;超频的第 4 条不回、非法包计数加 1;
  - 验签后一帧不发,30 秒关且只关一次;中途收到一帧推迟;按节奏发心跳不关;dev 免验签且不验签不心跳的不跟踪;旧会话的检查点不关同号的新连接;loop 卡顿时推迟;
  - 建连 10 秒未验签关;期限前验签不关。
- 不覆盖:`GateClientSessionHandler` 注册回调、两个收包入口调 `NoteInboundFrame`、`HandleConnectionEstablished` 登记握手期限、验签回包调 `OnTokenVerified`、`main.cpp` 装定时器这几行接线。那几个文件拖着整套节点依赖,进不了这个工程,靠核对调用点;这几行由下面 robot 的判死冒烟在真实进程里覆盖。
- **robot 客户端**(`robot/pkg/heartbeat_test.go`,8 个):发送与节拍用测试替身,不睡眠。
  - 验签成功当场发第一条,之后按通告的间隔每拍一条;
  - 不通告就不发、不起 goroutine;
  - 重定向换上的新连接验签之前不发,验签后当场补一条;重定向到不支持心跳的 gate 停发;
  - 暂停后不发;Close 之后不发、不重起;
  - `RecvOne` 在登录握手里跳过心跳回包(假 gate 是真实 TCP,按 gate codec 写帧)。
- **端到端判死冒烟** `heartbeat-idle-smoke`(`robot/heartbeat_idle_smoke_scenario.go`,约 45 秒):
  1. 登录后核对通告值与第一条心跳的回包;
  2. 停发心跳、连接保持打开;
  3. 另开一条裸 TCP,量 gate 在最后一条心跳之后多久关连接,要在 29.5–35 秒之间;
  4. 静默满 34 秒后同账号重登,要落回原场景、自己的 actor 号不变(断线保留接回同一个实体);
  5. 旧连接上没收到顶号踢线 34,证明它在重登之前已被 gate 关掉,login 走的是短重连。

## 9. 客户端仓要做的(`../mmorpg-client`,交 Codex;不做的话挂机 30 秒就掉线)

1. 同步 `message.proto`,重新生成协议代码。
2. 建连后立即发验签请求(现在就是这样);gate 10 秒内收不到会关连接。
3. 验签成功且 `heartbeat_interval_ms > 0` 时:
   - 立即发第一条 `ClientHeartbeatRequest`;
   - 之后每 `heartbeat_interval_ms` 发一条,不看别的流量;
   - 用单调时钟计时,在网络线程上发,不受渲染帧率和场景加载卡顿影响。
4. `heartbeat_interval_ms == 0`:不发。这是旧 gate 或尚未实现的那一版服务器。
5. 收到 `ClientHeartbeatResponse` 只用来判活与测往返时延,不交给业务处理。
6. 判 gate 已死:连续 `heartbeat_timeout_ms` 收不到 gate 的任何字节(回包或推送都算),就关掉连接、走重连。gate 对每条心跳都回包,健康连接上每 10 秒至少收到一条。重连时用断线租约走短重连,才能在 25 秒保留窗口内接回原角色。
7. 切到后台被系统挂起时,心跳会停,30 秒后 gate 判死。回前台时按掉线重连处理,不要假设连接还在。
8. `seq` 可以从 1 递增。回包里的 `seq` 不影响存活判定。

robot(本仓 `robot/pkg/heartbeat.go`)按第 1–5、7、8 条实现了发送一侧,可作参考:心跳只发往验签成功的那条连接,跟随 gate 重定向时新连接验签之前不发;`RecvOne` / `RecvLoop` 认领心跳回包、不交给业务分发。第 6 条(判 gate 已死)robot 没做,见 §12。

## 10. Java 版(待做,已在 Java 仓 `PARITY.md` 登记)

两版共享的是客户端契约,行为要一致。Java 按 Netty 惯用方式实现,不照抄 C++:

- ContractSync 同步 `message.proto`。
- `ClientDispatcher` 的验签成功回包(现在是常量 `VERIFY_OK`)填同样的两个参数,取同值 10000 / 30000,并从这一刻起按读空闲 30 秒判死(例如往 pipeline 里补一个 `IdleStateHandler`)。判死只关连接,断线处理走既有路径。
- 收 `ClientHeartbeatRequest`:验签前断开;每 10 秒窗口最多回 3 条,超出的不回、计入 `illegalPacketThreshold`;其余回显 seq。
- 握手期限:Java 已有 `handshake-timeout`,现为 30 秒,对齐到 10 秒。
- Java robot 同样在验签成功后按通告发心跳、跳过回包,判死冒烟可照本仓 `heartbeat_idle_smoke_scenario.go`。

## 11. 验证(交 Codex)

见 `docs/PROGRESS.md` 2026-11-06 三条:「gate 与客户端应用层心跳」(协议生成 → 串行编译 → 单测 → 冒烟)、「robot 客户端心跳与判死冒烟」(robot 编译、单测、`heartbeat-idle-smoke`)与「验签即判死与握手期限」(本轮改动与更正后的用例数、冒烟口径)。

## 12. 残余

- **未认证连接的独立预算、LRU 淘汰、按概率早拒没做**(准入控制文档 P0-2 的其余部分)。有了 10 秒握手期限,占槽的未验签连接每条最多活 10 秒,但持续高速建连仍能把槽位占满。预算的大小要按重连风暴实测:gate 重启或网络抖动后,成千上万个客户端同时重连,它们都短暂处于未验签状态,预算定小了会把正在握手的合法客户端淘汰掉,而断线保留恰恰依赖这些客户端能马上重连回来。所以留给准入控制那一项,带压测一起做。
- 判死之前 30 秒内推给死连接的数据照样丢失,由断线保留、重连后的完整入场画面和各业务有主协议兜底(`player-connection-lost-hold.md` §2 非目标)。
- robot 只做了发送一侧,不判 gate 已死:vendored muduo 断线后静默重拨、`Recv` 不报错,拿不到可靠的断线信号。robot 的会话失败仍靠各请求自己的超时;要做,得先改 muduo 客户端(Close 时关 incoming、断线不自动重拨),另起一项。
- gate 卡顿恢复之后,如果 1 秒内还没读完积压的输入(积压的连接极多),仍可能误判一次。同节点链路 §21.9。

## 13. 改动清单

- proto:`proto/common/base/message.proto`。只改源文件,生成交 Codex。
- gate 新增:
  - `cpp/libs/services/gate/session/comp/client_liveness.h`;
  - `cpp/nodes/gate/handler/rpc/client_heartbeat.h`、`client_heartbeat.cpp`;
  - `cpp/nodes/gate/tests/client_liveness_test.cpp`、`client_heartbeat_test.cpp`。
- gate 修改:
  - `cpp/libs/services/gate/session/comp/session_info_comp.h`(`liveness` 字段);
  - `cpp/libs/services/gate/gate.vcxproj` / `.filters`(登记头文件);
  - `cpp/nodes/gate/handler/rpc/client_session_handler.cpp`(注册回调、建连登记握手期限、两处记帧、验签成功回包 `OnTokenVerified`);
  - `cpp/nodes/gate/main.cpp`(扫描定时器);
  - `cpp/nodes/gate/gate.vcxproj` / `.filters`(登记新源文件);
  - `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`(两个测试文件与被测源文件);
  - `tools/scripts/stress_summarize.ps1`(汇总段);
  - `cpp/nodes/gate/SECURITY.md` §5、`docs/design/gate-connection-admission-control.md` P0-2(握手期限已落地的说明)。
- robot:
  - 新增 `robot/pkg/heartbeat.go`、`robot/pkg/heartbeat_test.go`、`robot/heartbeat_idle_smoke_scenario.go`、`robot/etc/heartbeat_idle_smoke.yaml`;
  - 修改 `robot/pkg/client.go`(验签成功起心跳、`VerifyGateToken` / `RecvOne` / `RecvLoop` 认领回包、`Close` 停心跳)、`robot/main.go`(分发)、`robot/config/config.go`(模式白名单)、`robot/README.md`(模式表)。
- 文档:本文;`docs/README.md` 登记;`docs/PROGRESS.md` 三条;Java 仓 `PARITY.md` 末尾一条。
