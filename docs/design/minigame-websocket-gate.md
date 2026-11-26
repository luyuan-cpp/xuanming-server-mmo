# 小游戏客户端经 WebSocket 连接 gate 与 battle(W1–W11)

- **日期**:2026-11-19。
- **状态**:代码已全部写完(服务端、部署脚本、客户端、robot),**未生成协议、未编译、未测试、未上集群,待 Gemini 验证**(AGENTS.md §10.1)。本文任何「通过」「生效」都指设计意图,不是运行证据。验证命令在 `docs/PROGRESS.md` 2026-11-19「小游戏 WebSocket 入口」条目,robot 的在同日「robot 加 WebSocket 传输」条目。2026-11-20 的收尾(边缘按来源限速、battle 的 WebSocket 计数、小游戏账号注销的重新认证、robot 去掉回环桥)在 `docs/PROGRESS.md` 2026-11-20「小游戏 WebSocket 收尾」条目。
- **授权**:改动约 60 个文件,超过 AGENTS.md §10.2 的 30 个文件门槛。负责人 2026-11-19 逐项同意 W1(共用端口)、W7(新增地址字段)、W10(客户端改法),说明 TLS 放在哪按最标准的做法定、Unity 有官方 WebSocket 就用官方的(前提是能上小游戏),确认原生 App 仍然直连之后回复「开始改」。
- **范围**:让微信、抖音小游戏客户端连上 gate(大厅长连接)和 battle(每局直连、观战直连)。登录侧的小游戏登录已落码(`auth_type` 为 `wechat_minigame` / `douyin_minigame`,见 [第三方登录端到端设计](third-party-login-end-to-end-design.md) §十一),本文只管登录之后的长连接。
- **决策编号**:本文 W1–W11。地址模型沿用 [集群外客户端入口](k8s-client-entry.md) D76(进程自报客户端可达地址)。

## 1. 为什么必须做 WebSocket

2026-11-13 核对的平台事实:

| 平台 | 长连接接口 | 结论 |
|---|---|---|
| 抖音小游戏 | 官方 API 概览的「网络」分类只有 `tt.request`、`tt.downloadFile`、`tt.uploadFile`、`tt.connectSocket`(WebSocket),没有 TCP / UDP 接口 | 只能走 WebSocket,并且要求 wss 与合法域名白名单 |
| 微信小游戏 | 另有 `wx.createTCPSocket`,可连「配置过的服务器域名」 | 不是必须走 WebSocket,但限制多:端口黑名单(1024 以下及一批常用端口)、每 5 分钟最多建 20 个连接;Unity 转小游戏要求基础库 ≥ 3.1.1,官方适配文档写明 `TCPSocket.OnMessage` 表现与文档不一致,并建议「也可以使用 WebSocket 替代」;社区有多帖反映真机上配了 TCP 合法域名仍报 `connect fail: invalid`;没查到 TCPSocket 支持 TLS 的说明 |

两家都要支持,WebSocket 是唯一的公共选项。微信也走 WebSocket:客户端只维护一套传输,服务端只有一个入口、一套证书和域名。微信 TCP 不做(§8)。

平台对连接地址的规则(微信「网络」使用说明,2026-11-19 读的全文;抖音的 `tt.connectSocket` 文档同样要求 wss 与后台登记的 socket 合法域名):

- 只能连事先在后台登记的域名;只支持 wss;域名不能用 IP 地址;域名必须经过 ICP 备案;「不支持配置父域名,使用子域名」。
- wss 域名不用登记端口,该域名下所有端口都可以连(抖音有没有同样的规则没查到,本设计只用 443,不依赖这一条)。
- 证书必须被系统信任、信任链完整,TLS 支持 1.2 及以上。
- `wx.connectSocket` 最多同时 5 条;本设计同一时刻至多 3 条(大厅,加上战斗、观战或匹配前的地域测速)。
- 开发者工具与开了调试模式的真机不校验域名,可以用不带证书的 ws。

服务端怎么接,两家的 Unity 适配文档给的是同一段话:服务端用 TCP 接入时需要一层 WSS 与 TCP 之间的代理,「使用 Nginx、websockify 做反向代理(推荐)」,或「改造原有 TCP 服务兼容 wss 服务」。本设计两样各取一半:gate / battle 自己说 WebSocket(不要 websockify 那层协议转换),前面的 nginx 只负责 TLS 和转发。

Unity 转小游戏后跑在 WebGL 上,`System.Net.Sockets` 不可用,长连接只能经平台的 JS 接口。客户端现有的 HTTP 已经用 `UnityWebRequest`、流程已经用协程,这两部分在 WebGL 下能跑;要换的只有长连接本身。

## 2. 总体形态

```
小游戏客户端 --wss://ws.<域名>/ws/<PodIP>/<端口>--> ws-edge(nginx,终止 TLS,按路径定向)
                                                      --ws://<PodIP>:<端口>--> gate / battle 的客户端端口
原生客户端 ------------------- TCP <client_endpoint> -------------------------> 同一个端口(照旧)
```

gate 和 battle 的客户端端口同时接受两种连接:首字节是 `G` 的按 HTTP 升级握手走 WebSocket,其余照旧按原始帧解析。WebSocket 只是现有客户端帧下面的一层拆包,验票、心跳、准入、排空迁移、踢线、观战都不分叉。

## 3. 决策

### W1 WebSocket 在 gate / battle 进程内终止,同一个端口按首字节分流

- 原始帧前 4 字节是大端长度,codec 上限 64KB(`ProtobufCodec::kDefaultMaxMessageLen`),所以合法 TCP 连接的首字节恒为 `0x00`。`G`(`0x47`)当长度首字节意味着 ≥ 1.19GB,原本就会被判 `kInvalidLength` 断连。按首字节分流不改变任何合法 TCP 连接的行为。
- WebSocket 层放在 core 的 `ProtobufCodec` 下面:`onMessage` 入口分流、拆帧,`send` 出口包帧。gate 和 battle 的客户端面都用这个 codec(battle 的 `BattleClientEdge` 照搬了 gate 的客户端面),所以两边一起得到 WebSocket。
- 不开第二个端口:现有 Acceptor 准入、关源、排空、两跳 `quit()` 的关机顺序全部复用,部署也不加端口和 Service。
- 选型(AGENTS.md §11.2):
  - 不用前置代理把 WebSocket 转成 TCP(websockify 或自写桥):多一跳、多一个要运维和扩容的组件,丢掉消息边界(微信适配文档自己也提醒要处理粘包);websockify 不适合十万级长连接。
  - 不引入现成 WebSocket 库(Boost.Beast、uWebSockets、libwebsockets):它们都自带 I/O 模型(Asio 或自己的事件循环),和 muduo 单 EventLoop、§11.7 的连接生命周期规则冲突。WebSocket 服务端协议本身很小(一次 HTTP 升级握手加帧头),握手要的 SHA-1 与 Base64 用仓里已有的 BoringSSL。用 RFC 6455 的示例向量做单测。

### W2 TLS 在边缘终止;小游戏连接不是 TCP 意义上的直连

- 结论:TLS 在 ws-edge(W8)终止,gate / battle 只说明文 WebSocket。小游戏玩家连的是边缘,边缘再连到地址里指定的那台 gate / battle。原生客户端照旧直连 `client_endpoint`,不经边缘。
- 小游戏为什么不能像原生客户端那样直连每台 gate / battle:
  1. 直连意味着每个实例有一个自己的、事先登记过的域名地址(§1 的规则:不能用 IP、不能登记父域名后用子域名)。
  2. gate 的副本数会变;battle 实例由 Agones 动态创建,对外端口是各节点上的 hostPort,不同节点会用同一个端口号,一个域名分不出是哪个节点。实例地址没法事先登记。
  3. 所以必须有一个固定域名的入口,把连接转给地址里指定的实例。在进程内做 TLS 也省不掉这个入口。
- 这是通行做法:两家平台文档推荐的就是 nginx 反向代理(§1);K8s 上 TLS 通常在入口终止;Agones 上给 WebSocket 游戏服加 TLS,社区的做法(Octops gameserver-ingress-controller)同样是入口按域名或按路径转发到具体实例,其中只有按路径的模式用得了单个域名。
- 不在 gate / battle 进程内做 TLS 的理由:
  - 仓内 C++ 没有任何 TLS 代码(2026-11-19 在 `cpp/libs`、`cpp/nodes` 下 grep `SSL_new`、`SSL_CTX_new`、`SSL_read`、`BIO_new` 无命中),要在 muduo 上新写握手与记录层的状态机。
  - 握手的非对称运算会落在 gate 唯一的 EventLoop 线程上,登录洪峰时挤占消息转发。
  - 证书要分发到每个实例,到期轮换要么写热加载,要么滚动重启所有 gate / battle。
- 代价:
  - 多一跳,在集群网络内。
  - 边缘 Pod 重启时,经过它的玩家断线,走现有的断线重连。边缘至少 2 个副本,逐个滚动。
  - gate / battle 看到的对端地址是边缘,不是玩家(W6)。
- 边缘不解析游戏帧,不持有会话状态。每条玩家连接仍然一对一对应到自己那台 gate / battle 上的一条连接,不复用,任何一个边缘副本都能转任何一条连接,可以随意加副本。它与「battle 不经 gate」([传输选型](battle-transport-decision.md) §8.2)不冲突:那一条防的是战斗流量挤进 gate 这个带会话状态、单线程的进程。
- [集群外客户端入口](k8s-client-entry.md)「明确不做」里的「gate / battle TCP 上加 TLS」不变。

### W3 帧映射:一条二进制消息承载一个现有客户端帧

- 字节与 TCP 上完全相同(长度前缀 + 类型名 + 载荷 + 校验和,`codec.cpp` `fillEmptyBuffer`),客户端编解码不分叉。
- 入站:把每个数据帧的载荷按字节流喂给 codec。codec 本来就处理半帧与多帧,所以分片消息、一条消息里装多个客户端帧都正确;每条连接待解析的字节受 codec 的单帧上限约束。
- 出站:每个客户端帧发成一条 FIN=1 的二进制消息,服务端帧不加掩码。不加掩码的帧对所有连接字节相同,广播的 `EncodedFrame` 同时编码一份 WebSocket 字节,仍然只编码一次(`gate_push_delivery.h`)。
- 拒绝并关闭(关闭码按 RFC 6455 §7.4.1):文本帧 1003;客户端帧没加掩码、RSV 位非 0、保留操作码、控制帧分片或载荷超过 125 字节、续帧没有起始帧、起始帧未完又来新数据帧,1002;单帧载荷超过 codec 上限加帧头余量,1009。
- 不协商扩展(客户端提出 `permessage-deflate` 也不回)和子协议。

### W4 握手

- 首包必须是 `GET <路径> HTTP/1.1`,头里有 `Upgrade: websocket`、`Connection` 含 `upgrade` 令牌(都不区分大小写)、`Sec-WebSocket-Version: 13`、`Sec-WebSocket-Key`(Base64 解出 16 字节)。请求头上限 8KB,超过即关闭。路径不校验,定向由边缘完成。
- 成功回 `101 Switching Protocols` 加 `Sec-WebSocket-Accept`(Key 拼上 RFC 固定 GUID 后取 SHA-1,再 Base64)。失败回 `400 Bad Request`;版本不符回 `426` 并带 `Sec-WebSocket-Version: 13`。之后半关。
- 不查 `Origin`:身份靠帧内的 ClientTokenVerify,不靠 cookie,没有跨站劫持面。
- 握手完成之前这条连接没有任何 WebSocket 状态,此时发给它的下行帧按原始 TCP 写出。正常不会发生:gate / battle 在客户端验票之前不主动下发业务消息,广播只推已绑定玩家的会话;连接建立时就拒绝的路径直接关连接,WebSocket 客户端看到的是握手失败,与连接被关等价。
- 已经在跑原始帧的连接在帧边界发来 `G`,以前因长度非法被断开,现在等于改走握手。只影响它自己:会话、票据都绑在这条连接上,不带来新的权限。

### W5 连接生命周期(AGENTS.md §11.7)

- 每条已完成升级的连接的状态(续帧标记、关闭标记、待喂给 codec 的字节)存在 core 的线程局部表里(`network/codec/websocket.cpp`),以连接指针为键,表项只存 `weak_ptr<TcpConnection>` 做身份自校验:锁不住或锁出来不是同一个对象,就当表项不存在并删掉。表不持强引用,连接的所有权仍只在 muduo。没有 WebSocket 客户端时,每次收发只多一次判空。
- 线程约束:表是线程局部的,所以收发必须在连接所属的 loop 线程上(换一个线程查到的是空表,会把 WebSocket 连接当成原始 TCP 发出没有帧头的字节)。gate / battle 的客户端连接都在节点主 loop 上,全仓没有 `setThreadNum`。
- 断开回调里删表项(`websocket::Forget`):gate `main.cpp` 的连接回调、battle `BattleClientEdge::OnConnection` 各加一行。漏删时,weak 自校验保证地址复用的新 TCP 连接不会被当成 WebSocket。
- 同一批数据里排在 Close 帧(或协议错误)之前的客户端帧照常交给上层,然后才回 Close 并半关(`websocket::FinishRead`),与 TCP「先处理数据、后处理 FIN」同序:客户端「发完离开请求就关连接」时,服务端看到的仍是正常离开而不是断线。
- 验票期限(建连后 10 秒,含升级握手)、应用层心跳、空闲超时(验票后静默 30 秒)全部不变,见 [gate 客户端心跳](gate-client-heartbeat.md)。WebSocket Ping 只回 Pong,不算活跃;Pong 忽略。小游戏切到后台时 JS 定时器暂停、心跳停发,gate 照常按空闲超时关闭,回到前台后走现有的断线重连。
- 客户端发 Close:回 Close(回显状态码)后半关。服务端主动关闭(踢线、排空滞留关闭、空闲超时、停机)照旧直接关 TCP,不补发 Close 帧:`forceClose` 会丢弃没写出去的数据,补发也送不到。客户端按断线(1006)处理,和 TCP 断线走同一条恢复路径。

### W6 准入与限流

- 同端口、同 Acceptor:总量上限、新建速率闸、关源天然覆盖 WebSocket 连接([连接准入](gate-connection-admission-control.md) P1-1 ~ P1-3)。ws-edge 给每条客户端连接各开一条上游 TCP,不复用,所以连接数口径不变。
- 经过边缘的连接,gate / battle 在 accept 时看到的来源是 ws-edge 的 Pod 地址(内网)。它们自己的按来源限速只管公网源地址(core `accept_admission.h` 的 `IsPublicSourcePrefix`,[连接准入](gate-connection-admission-control.md) P2-2;「battle 直连准入」会话 2026-11-20 落码),所以全体小游戏玩家不会被当成同一个来源一起限。
- 这部分连接的按来源限速在边缘做,只有边缘看得到玩家的真实地址:`infra-up -WsEdgeSourceRatePerSecond <n>`(缺省 0 = 不限)让 nginx 按来源 /24 做 `limit_req`,桶深 = 速率 × 握手期限(10 秒),超出的升级请求回 429。口径与 gate 的那一道相同:按 /24 聚合(CGNAT、校园网出口后面是成片的真玩家)、限新建速率不限总量、不封来源。
- 与 gate 那一道不同的一点:nginx 不知道后端是否承压,开了就一直生效;gate 是开了也只在承压时生效。所以边缘这个参数缺省不开,被打的时候再带上它跑 infra-up。同一个出口后面几百人同时重连时,先放行一个桶深,之后按速率进,其余的收到 429 后由客户端的断线恢复重试。
- 为什么不在 gate 里按 `X-Forwarded-For` 限:请求头要到升级握手才读得到,那时这条上游连接已经过了 accept、占掉了全局速率闸的名额,攻击流量照样和真玩家抢同一个桶。在边缘拒绝,攻击连接到不了 gate。限速排在目标校验之后,目标不合法(403)的请求不消耗名额。
- 前提同 gate 那一道:边缘要看得到真实源地址。ws-edge 的 Service 是 `externalTrafficPolicy: Local`;前面是代理型负载均衡时看到的是负载均衡的地址,这时不要开。

### W7 地址模型:节点自报 `client_ws_url`

- `NodeInfo` 新增 `string client_ws_url = 17;`。值 = `CLIENT_WS_BASE_URL` + `/` + endpoint.ip + `/` + endpoint.port,在 endpoint 端口定下来之后计算(`node_allocator.cpp` 设 gRPC 端口的同一处)。env 不设就不自报。只有 gate / battle 的部署注入这个 env。
- 用 endpoint(Pod IP:RpcPort)而不是 `client_endpoint`:ws-edge 在集群内,直连 Pod 最可靠,不经 NodePort 或云负载均衡回绕。
- 启动校验(fail-closed,同 D79):`CLIENT_WS_BASE_URL` 必须以 `wss://` 或 `ws://` 开头,不带查询串与片段,末尾的 `/` 去掉;设了它而 endpoint.ip 不是 IPv4 字面量时拒绝启动(ws-edge 只认 IPv4,发布一个永远连不通的地址比起不来更难查)。
- 下发字段(只加字段,老客户端忽略):

| 消息 | 新字段 | 谁填 |
|---|---|---|
| `AssignGateResponse` | `ws_url = 12` | login 快路径与排队消费 |
| `QueryQueueStatusResponse` | `ws_url = 11` | login 排队放行 |
| `RedirectToGateNotify`(msg 124) | `target_ws_url = 6` | gate,从下面两条原样拷贝 |
| `BattleAssignedS2C` | `ws_url = 8` | battle `BuildAssignment`,补签同路 |
| `NetworkProbeTarget`(地域测速目标) | `ws_url = 5` | match `GetNetworkProbePlan` |
| `RedirectToGateEvent`(Kafka) | `target_gate_ws_url = 8` | scene_manager 跨 zone 重定向 |
| `RedirectToGateInfo` | `target_gate_ws_url = 6` | scene_manager |
| `GateMigrationResponse` | `target_gate_ws_url = 7` | login 排空迁移 |

- login 排队:Redis 里的 `AdmitSlot` 与 `AdmitToken` 加 `ws_url`(JSON `omitempty`),`GateCandidate` 加 `WsURL`。dispatcher 选中 gate 时写入,consume 时带出,和 ip / port 同一条路。
- 本仓 Java gateway(`java/gateway_node`)的 `/api/assign-gate` 与 `/api/queue-status` 用手写 DTO,加 `gateWsUrl`(JSON `gate_ws_url`)并在映射处赋值。
- 不回落、不筛选:`ws_url` 为空表示这台节点不接小游戏,小游戏正式构建拿到空值就报错不连。login 不按客户端传输筛 gate:同一 zone 的 gate 由部署统一注入 env,不存在部分支持。D78 的去重仍按 `client_endpoint`;两个地址属于同一个 Pod,同来同走。
- 匹配前的地域测速也是直连 battle(`GetNetworkProbePlan` 下发的 `NetworkProbeTarget`),同样加 `ws_url = 5`,由 match 取自 battle 自报的 `client_ws_url`(`go/match/internal/discovery/node_watcher.go`、`logic/network_probe.go`)。测速计划缺省是必需的,不带上它小游戏玩家进不了全服匹配队列。
- battle 同理不筛选:match 给玩家选哪台 battle(建房、测速目标)不看它有没有自报 WebSocket 地址。所有 battle 实例来自同一份工作负载模板,要么都带 `CLIENT_WS_BASE_URL`,要么都不带;只有第一次开启边缘的那次滚动期间新旧并存,这段时间小游戏玩家分到没自报的那台会连接失败(测速换下一个机房,战斗直连走现有的补签重试),滚动结束后恢复。开启边缘要放在小游戏对外开放之前做。

### W8 边缘 ws-edge

- 形态:infra namespace 里一个 Deployment(官方 nginx 镜像,钉版本,缺省 2 个副本,逐个替换),Service 端口 443、`externalTrafficPolicy: Local`;证书放预置的 TLS Secret(`tls.crt` 带完整证书链,部署脚本不经手内容,缺了在写操作之前拒绝);`nginx.conf` 放 ConfigMap,指纹写进 Pod 模板注解,配置变了才滚动。四份清单由 `tools/scripts/lib/k8s_client_entry.ps1` 的 `New-WsEdgeYaml` 生成,`infra-up` apply。
- 部署参数(`k8s_deploy.ps1`,`dev_tools.ps1` 与 `k8s_image.ps1` 照样透传):`-WsEdgeHost`(域名,给了才启用,要求 `-ClientEntryMode external`)、`-WsEdgeTlsSecret`、`-WsEdgePodCidrs`(这三项缺一不可)、`-WsEdgeReplicas`、`-WsEdgeServiceType LoadBalancer|NodePort`(托管云用前者,自建集群用后者接在外部四层负载均衡后面)、`-WsEdgeNodePort`、`-WsEdgeImage`。
- 按来源限速(W6):`-WsEdgeSourceRatePerSecond <n>`,缺省 0 = 不限,三条包装链同样透传;只有 infra-up 生成 ws-edge,所以改它要跑 infra-up。关闭时生成的 `nginx.conf` 与没有这个参数时逐字相同(指纹不变,不触发滚动);没给 `-WsEdgeHost` 却给了它,infra-up 在写操作之前拒绝。
- 给了 `-WsEdgeHost`,gate 的 StatefulSet 与 battle 的 Fleet / hostPort Deployment 就带上 `CLIENT_WS_BASE_URL=wss://<域名>/ws`(经 PodCommon,三个生成器同一处)。
- 参数不粘滞:线上的 gate / battle 已经在自报 WebSocket 地址,而本次漏传或换了域名时,`Get-GateAddressDrift` / `Get-BattleEntryDrift` 在任何写操作之前拒绝,确认要改加 `-AllowDisruptiveSwitch`。否则之后重建的节点不再自报,落到它上面的小游戏玩家拿不到入口,且不报任何错。留空不会删已有的 ws-edge。
- 路由:`/ws/<IPv4>/<端口>` 转发到 `http://<IPv4>:<端口>`。IPv4 用 `geo` 限定在 Pod 网段(部署参数),端口用正则限定为 gate 与 battle 的 RpcPort(部署参数,现为 18000 与 20000),不符合的回 403,其余路径 404。`proxy_read_timeout` / `proxy_send_timeout` 120s(大于 gate / battle 自己的空闲超时,谁该断由 gate / battle 说了算),关代理缓冲,带 `X-Forwarded-For`。nginx 收全请求头才连上游,慢速发头的连接停在边缘,到不了 gate。
- 出站限制:scene 的节点口也是 20000,只靠网段加端口,边缘还能把一个 HTTP 请求发到 scene 的内部口(scene 会按非法帧断开,建不成通道)。所以给 ws-edge 加一条只选中它自己的 egress NetworkPolicy:只许连 `app` 为 gate、battle(含金丝雀)的 Pod 的客户端口。网络插件不执行 NetworkPolicy 时这条不生效,剩下 nginx 的网段与端口限定。仓里现有的 gRPC 口准入策略(`-NetworkPolicies on`)对节点口不设来源限制,不挡边缘。
- 为什么按 Pod IP 定向:gate 票据绑节点(`token_gate_node_mismatch`),连接必须到达指定实例;battle 由 Agones 管,实例随时生灭,没有稳定的 Service。Pod IP 对两者都适用,不用为每台建 Service 或路由规则,也不动 Agones 端口。
- 为什么是独立的 nginx 而不是 Ingress:按路径里的变量转发到任意 Pod 需要 snippet 注解,ingress-nginx 新版默认禁用;独立 nginx 的配置就是一个静态文件,审阅即可。
- 安全:边缘只能转到 Pod 网段里 gate / battle 的客户端端口,而这两个端口本来就对玩家开放,所以它不是开放代理。路径里的 Pod IP 会暴露集群内地址,本批接受。
- 平台后台:socket 合法域名登记 `wss://<ws-edge 域名>` 一个;request 合法域名登记 gateway 的 https 域名(D91 的 Ingress)。两个域名都要 ICP 备案,证书由受信 CA 签发。对外只用 443,不受两家平台对端口的不同规则影响。备案、账号、合法域名这几项要排队几周,汇总在 [发布清单](../ops/release-checklist.md) §7。

### W9 战斗服

- `BattleClientEdge` 用同一个 codec,WebSocket 自动生效。它的两处发帧改调能识别 WebSocket 的发送函数,断开回调补清理。参战直连与观战直连都覆盖。
- `BattleAssignedS2C.ws_url` 取自本节点 `NodeInfo.client_ws_url`;补签(IssueBattleTicket)同路。
- 匹配前的地域测速(`RegionProbeClient`)走同一条路:测速目标带 `ws_url`(W7),battle 的测速消息本来就经同一个 codec。经过边缘多一跳,测出的延迟比原生客户端略高,各机房都多这一跳,排序不受影响。

### W10 客户端(`D:\luyuan\mmorpg-client`)

- Unity 没有官方的 WebSocket 接口。Unity 手册「Web networking」写明 Web 平台不支持 `System.Net` 下的任何类,并且「Unity doesn't have a built-in API that allows you to use WebSockets or WebRTC, but you can use a JavaScript plug-in to implement this」。Unity Transport 包的 WebSocket 模式跑的是它自己的连接协议,不是通用的 WebSocket 客户端,gate 不说这个协议。
- 底层直接调两家平台自己的接口:`Assets/Plugins/WebGL/MmorpgMiniGame.jslib` 在抖音宿主里用 `tt.connectSocket`,在微信宿主里用 `wx.connectSocket`,都没有时(普通浏览器)用 `WebSocket`。不依赖两家的 Unity SDK,与客户端仓里已有的支付桥(`MinigamePay.jslib`)同一个做法。
- 没有用开源插件 UnityWebSocket(两家的 Unity 适配文档拿它做客户端示例):它在 WebGL 下调的是运行环境里的 `WebSocket` 全局对象,那是转换工具的适配层补出来的,而平台原生的接口是 `connectSocket`;它经 git 地址引入,打开工程的机器要能访问 GitHub,包解析失败或与 Unity 6000.6 不兼容会让整个客户端工程编不过。自己的 JS 桥约 100 行,换成插件只需要再写一个 `IWebSocketDriver` 实现。
- 客户端已有传输接口 `IFramedConnection`(`Assets/Scripts/Net/IFramedConnection.cs`)。它多一个带 `wsUrl` 的 `Connect` 重载,缺省实现就是原来的两参数 `Connect`,所以 `GateTcpClient` 与测试替身都不用改;另加 `CloseForResume`(回到前台时关掉旧连接),缺省什么都不做。
- 新增 `WebSocketFramedConnection : IFramedConnection`(分帧、排队、心跳、事件派发)与 `IWebSocketDriver`(一条 WebSocket 的收发面)。驱动两个实现在 `WebSocketDrivers.cs`:小游戏 / 浏览器走上面的 JS 桥;其它平台用 `System.Net.WebSockets.ClientWebSocket`,只给编辑器联调用。测试注入假驱动。
  - `Connect` 立即返回;打开前的 `Send` 排队,打开时按序发出;`Connected` 在打开之后才为真;打开失败或 10 秒内没打开,在 `Poll` 里触发 `OnDisconnected`。三个事件仍只在 `Poll` 内触发,线程约定不变(驱动的回调只入队)。
  - 没有后台线程(WebGL 没有线程):心跳在 `Poll` 里发。主线程长时间不 `Poll`、或小游戏切到后台,心跳就停,gate 按空闲超时关掉连接,之后走正常的断线恢复。
- 传输选择(`ClientTransport`):WebGL 构建用 WebSocket,编辑器与原生用 TCP;编辑器加编译符号 `MMORPG_FORCE_WS` 可以拿 WebSocket 直连本机 gate 联调。`ws_url` 为空时,开发构建回落 `ws://<ip>:<port>/`(本机没有边缘),正式构建按连接失败处理。
- 重定向(msg 124)换连接前的 TCP 探测(`GameClient.ProbeGate`)在 WebSocket 模式下不做,改为等新连接真正打开,期限相同;打开之前不关旧连接,保留「不通就在还连着老 gate 时失败出去」的性质。
- 战斗直连与地域测速原来把会阻塞的 `Connect` 放进线程池;WebSocket 的 `Connect` 不阻塞,而 WebGL 没有线程池,所以改为就地执行(`ClientTransport.ConnectRunner`)。
- 登录:`MiniGameLogin`(同一个 jslib)向宿主要一次性的登录 code(`tt.login` / `wx.login`),运行时看在哪个宿主里决定 `auth_type`。登录界面点「进入」时,在小游戏里直接走它,用 `LoginCredential.ForThirdParty(LoginAuthTypes.WeChatMiniGame / DouyinMiniGame, code)` 走 `/api/login`,与 Apple / Google 登录共用 `BeginChannelSignIn`。`GatewayHttpClient` 的 `AssignGateResult` 加 `gate_ws_url`。
- 账号注销的重新认证(2026-11-20 补):小游戏账号没有口令,注销确认之后与 Apple / Google 一样重新拉起渠道——向宿主重新要一次登录 code(`MiniGameLogin.Request`),连同 `auth_type` 交给注销接口;服务端的重新认证本来就走与登录相同的认证器,不用改。之前会落到口令面板,小游戏账号永远注销不了。
- 不在本批:
  - 把客户端真正转成微信 / 抖音小游戏包(装两家的转换工具、构建配置、资源适配),以及真机联调。负责人 2026-11-20 定:上线前再转;之后打算升到 Unity 7,所以走「Unity 7 + 两家的转换工具」,不换团结引擎(团结是 Unity 中国自己的分支,手册以 2022.3 为基线)。2026-11-20 查过两家转换工具对 Unity 6000.6 的支持,没有结论(微信官方的 GitHub 仓库被 GitHub 停用、文档站 404);转包前先核实它们对 Unity 7 的支持,新引擎大版本出来后转换工具通常要过一段时间才跟上。
  - 转包时要复查与引擎 WebGL 构建绑定的两处:`Assets/Plugins/WebGL/MmorpgMiniGame.jslib`(Emscripten 的 jslib 写法),以及 `WebSocketDrivers.cs`、`MiniGameLogin.cs` 里 `UNITY_WEBGL && !UNITY_EDITOR` 的分支(`[MonoPInvokeCallback]` 回调)。它们至今没编译过:本机只装了 Windows 构建支持,客户端编译检查只编编辑器脚本。其余传输代码是普通 C#,在编辑器里有单测,与引擎版本无关。
  - 推迟的前提:客户端开发期间就按小游戏(WebGL)的限制写,否则转包时是大返工。完整清单(单线程、会卡死的调用、网络、文件、图形、字体、内存与包体)在客户端仓 `AGENTS.md`「小游戏(WebGL)兼容约束」,依据 Unity 6.6 手册的 Web 平台限制;2026-11-20 核对过小游戏会走到的路径上没有违例。

### W11 robot(冒烟 / 压测客户端)的 WebSocket 模式

负责人 2026-11-19 在前十项交付后要求补上,用来自动冒烟这条链路。

- 配置 `transport.kind: websocket`,对全进程生效,缺省 TCP。登录、跟随重定向(msg 124)、战斗直连、地域测速、空闲超时探测的裸连接都按它连(`robot/pkg/transport.go`,用法见 `robot/README.md`「传输方式」)。
- 地址:服务端下发了就用下发的;没下发时直连 `ws://<ip>:<端口>/`,本机栈没有边缘、节点也不自报地址,靠这条回落就能跑。`require_advertised` 打开后没下发就失败,经边缘验证时用,否则服务端漏下发会被直连掩盖。`dial_addr`(实际拨的地址,握手的 Host 与 SNI 不变)和 `insecure_skip_verify` 给边缘域名在本机解析不到、证书自签的测试环境(kind)。后三项写了却没选 websocket 时拒绝启动。
- 地址怎么传到建连处:robot 里几十个函数按 `(host, port)` 传 gate 地址,逐层加参数改动面太大。改用地址簿:解出服务端下发地址的五处(两个 assign-gate 解码点、msg 124、战斗分配、测速目标)登记 `host:port → WebSocket 地址`,建连时查。同一台节点的两种地址总是一起下发,连接又总在下发之后,查到的就是这一次的;空的下发清掉旧记录。
- 协议实现用 `golang.org/x/net/websocket`。robot 本来就依赖 `golang.org/x/net`(之前是间接依赖),没有新增模块;它把一条 WebSocket 呈现成字节流连接,写出的每段字节是一条二进制消息,正好对上 W3。全仓的 Go 代码此前没有用过任何 WebSocket 库。
- 连接:`GameClient` 原来拿着 vendored `muduoclient` 的 `*muduo.Client`,它自己 `net.Dial("tcp", …)`,没有注入拨号函数的口子,接不了 WebSocket。把它的连接管理搬进仓内(`robot/pkg/wire_conn.go` 的 `wireConn`),只换掉一处:怎么拨号由调用方给;帧的编解码仍用原库的 `TcpCodec`。对 `GameClient` 可见的行为逐条不变(后台拨号、拨不通每秒重试、发送队列满了就丢、断线后下一次发送失败才重拨、`Close` 不唤醒 `Recv`),robot 里关于「vendored muduo 的连接」的注释说的就是它。
- WebSocket 模式下 `wireConn` 直接在 WebSocket 上收发:每个客户端帧写成一条二进制消息,与 Unity 客户端相同;没有额外的回环跳转,冒烟与高并发压测都可以用。2026-11-19 的第一版是进程内回环桥(每条连接多一个回环监听和一跳回环 TCP,只适合小规模),2026-11-20 换成现在的做法,桥已删除。
- 两处与原库不同、不影响上述行为:读不设 30 秒期限(原库只用它定期醒来看 ctx,`Close` 本来就会关连接把读唤醒;WebSocket 下读到一半帧头超时会丢掉已读的字节);接收缓冲每条连接各用一个新的。
- 关闭时直接关 TCP、不发 Close 帧,与原生客户端断线一致(x/net 的 Close 要先拿写锁发 Close 帧,对端不读时会和阻塞中的写互相卡住)。服务端回显 Close 的路径由 C++ 单测覆盖。

## 4. 可观测性

- core 计数(`websocket::GetCounters()`,进程累计,线程局部,不带玩家或连接维度):握手成功、握手被拒、协议错误关闭。gate 在连接准入的采集函数里一并导出:`gate_websocket_handshakes_total{result="accepted|rejected"}`、`gate_websocket_protocol_closed_total`。WebSocket 连接数已经算在 `gate_connections_current` 里。battle 同样导出(2026-11-20 补):`battle_websocket_handshakes_total{result}`、`battle_websocket_protocol_closed_total`,写法与 gate 的两族同义,在 battle 连接准入的采集函数末尾。
- 握手被拒与协议错误打采样 WARN(第一条必打,之后每 1024 条一行),带关闭码与对端地址。

## 5. 安全

- 握手解析有界:请求头 8KB、验票期限 10 秒(含升级)共用现有未验签预算;单帧载荷上限同 codec。
- 只接受加掩码的客户端帧(RFC 6455 §5.1),拒绝文本帧与未协商的扩展位。
- 鉴权不变:WebSocket 连接同样必须先过 ClientTokenVerify,票据绑节点。
- 边缘不是开放代理(W8)。

## 6. 数据一致性与兼容

- 不碰任何存储与提交点。新字段全部是追加的可选字段;项目未上线,`AdmitSlot` 的 Redis 数据可清空,`omitempty` 也让新旧形状互相可读。
- 客户端可见契约有变化:`/api/assign-gate` 与 `/api/queue-status` 的应答、msg 124、战斗分配、地域测速目标各加一个可选的 WebSocket 地址;gate / battle 的客户端端口多接一种连接。AGENTS.md §12 要求两版同批改;本批按任务约定只在 Java 仓 `PARITY.md` 登记待做,Java 版在补齐之前不接小游戏,新字段对它现有的客户端没有影响。

## 7. 验证(交 Gemini,AGENTS.md §10.1)

具体命令在 `docs/PROGRESS.md` 的同名条目里。要点:

1. 协议生成:proto 改动后按 AGENTS.md §4 用默认配置生成(这一批有客户端任务授权,要写客户端仓),再跑 `go/build.ps1` 修 login 包装。没生成之前 C++、Go、客户端都编不过。
2. C++:先编 core 库再编测试工程(`run_cpp_tests -Build` 不重编库),串行 `/m:1`。
   - `cpp/nodes/gate/tests/websocket_entry_test.cpp`(编进 `routing_identity_test`):RFC 6455 的握手示例(Accept 值)、升级请求的接受与各种拒绝(含版本不符回 426)、帧头三种长度形式;回环连接上的收发、握手与帧被切在任意位置、分片中间夹 Ping、一条消息装多个客户端帧、Close 之前的数据先交付、八种违规帧各自的关闭码、不是升级请求的 `GET` 回 400、原始 TCP 不受影响。
   - `cpp/tests/client_endpoint_test`:基地址的解析与拒绝、IPv4 判定、地址拼接。
3. Go:login 候选与排队放行槽带出 `ws_url`(含过一遍 Redis 的 JSON)、scene_manager 重定向取挑中那台 gate 的地址、match 解析 battle 自报的地址。
4. Java gateway:`mvn test`(`AssignGateServiceAuthTest` 加了一条透传用例)。
5. 部署契约测试:`k8s_client_entry_contract.tests.ps1` 覆盖预检的六种组合、三个生成器的 env 注入、ws-edge 四份清单的内容与参数拒绝、zone-up / infra-up 的 DryRun、三条包装链的透传。
6. 客户端:EditMode 用例 `WebSocketFramedConnectionTests`(假驱动、可拨时钟);编辑器加 `MMORPG_FORCE_WS` 连本机 gate 联调。微信、抖音开发者工具与真机联调需要开发者账号和转换工具,不在本批。
7. robot(W11):先 `go mod vendor`(把 `golang.org/x/net/websocket` 与新生成的协议带进 `robot/vendor`),再跑 `robot/pkg` 的用例:传输层(假的回显 WebSocket 服务端:地址选择、一帧一条二进制消息、经 `dial_addr` 的 TLS、必须下发时本地失败、TCP 缺省不变),以及原有的 Call / 心跳 / 重定向用例(它们现在跑在仓内的 `wireConn` 上)。之后拿现有冒烟配置加 `transport.kind: websocket` 各跑一遍,这是本文唯一一条端到端的自动化路径;kind 上再加 `require_advertised` 走边缘。
8. 边缘配置的静态检查:把 `-DryRun` 渲染出来的 `nginx.conf`(不带与带 `-WsEdgeSourceRatePerSecond` 各一份)放进钉住的 nginx 镜像里跑 `nginx -t`,证书用临时自签的。`geo` 以路径里捕获的地址为键、`map` 取 /24 这两处写法没有实测过,这一步十秒钟就能定下来,不用等 kind。

## 8. 明确不做

- 微信 `wx.createTCPSocket` 直连(§1)。
- gate / battle 进程内 TLS、`permessage-deflate` 压缩、`Origin` 校验、IPv6 Pod 网络;gate / battle 按 `X-Forwarded-For` 做按来源限速(W6:在边缘做)。
- QQ 小游戏。

## 9. 改动的文件

协议(7):`proto/common/base/common.proto`、`proto/login/login.proto`、`proto/contracts/kafka/gate_event.proto`、`proto/scene_manager/scene_manager_service.proto`、`proto/scene/client_player_common.proto`、`proto/battle/player_battle.proto`、`proto/match/match_network.proto`。

C++(22):
- core:`network/codec/websocket.{h,cpp}`(新)、`network/codec/codec.{h,cpp}`、`core.vcxproj`、`core.vcxproj.filters`、`node/system/node/client_endpoint.{h,cpp}`(基地址解析与地址拼接)、`node/system/node/node.cpp`(启动校验)、`node/system/node/node_allocator.cpp`(端口定下后写 `client_ws_url`)。
- gate:`main.cpp`(断开清理)、`handler/gate_push_delivery.h`(`EncodedFrame` 的 WebSocket 字节)、`handler/rpc/connection_admission.cpp`(指标)、`handler/event/gate_event_handler.cpp`、`handler/rpc/drain_migration.cpp`(124 带 ws 地址)。
- battle:`client/battle_client_edge.{h,cpp}`、`logic/battle_room_manager.cpp`、`client/battle_connection_admission.{h,cpp}`(battle 的 WebSocket 两族计数)。
- 测试:`cpp/nodes/gate/tests/websocket_entry_test.cpp`(新)、`cpp/tests/routing_identity_test/routing_identity_test.vcxproj`、`cpp/tests/client_endpoint_test/client_endpoint_test.cpp`。

Go(15):
- login:`loginqueue/{gatetoken,queue,dispatcher,admit_recheck}.go`、`svc/servicecontext.go`、`svc/gate_migration.go`、`loginpregate/{assigngatelogic,queryqueuestatuslogic}.go`;测试 `loginqueue/admit_recheck_test.go`、`svc/candidates_client_endpoint_test.go`。
- scene_manager:`logic/{gate_redirect,enterscenelogic}.go`;测试 `logic/gate_redirect_client_endpoint_test.go`。
- match:`discovery/node_watcher.go`、`logic/network_probe.go`;测试 `discovery/node_watcher_parse_test.go`。

本仓 Java gateway(4):`grpc/LoginRpcClient.java`(手工解码两条应答的新字段)、`dto/AssignGateResponse.java`、`service/AssignGateService.java`、测试 `service/AssignGateServiceAuthTest.java`。

部署(6):`tools/scripts/lib/k8s_client_entry.ps1`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/dev_tools.ps1`、`tools/scripts/k8s_image.ps1`、`tools/scripts/tests/k8s_client_entry_contract.tests.ps1`。gate 指标的告警契约测试 `gate_connection_alerts_contract.tests.ps1` 由「gate 准入 P0-2」会话在同一天一并补上这两个指标族。battle 的 `battle_connection_alerts_contract.tests.ps1` 2026-11-20 由本会话加上 battle 的两族与 `result` 标签。

Unity 客户端(14,不含生成的 C# 协议):
- 新增:`Assets/Scripts/Net/{ClientTransport,WebSocketFramedConnection,WebSocketDrivers,MiniGameLogin}.cs`、`Assets/Plugins/WebGL/MmorpgMiniGame.jslib`、`Assets/Tests/EditMode/Net/WebSocketFramedConnectionTests.cs`。
- 修改:`Assets/Scripts/Net/{IFramedConnection,BattleDirectLink,RegionProbeClient,GatewayHttpClient}.cs`、`Assets/Scripts/Game/{GameClient,GameClient.Reconnect}.cs`、`Assets/Scripts/UI/Ugui/QdaoServerSelectView.cs`、`Assets/Scripts/UI/Ugui/QdaoServerSelectView.LoginUtilities.cs`(注销的重新认证)。
- 新文件的 `.meta` 由 Unity 打开工程时生成。

robot(20,W11):
- 新增:`robot/pkg/transport.go`、`robot/pkg/wire_conn.go`、`robot/pkg/transport_test.go`、`robot/http_assign_gate_ws_url_test.go`。
- 修改:`robot/pkg/{client,redirect,heartbeat}.go`、测试 `robot/pkg/{client_test,heartbeat_test}.go`、`robot/{gate,http_assign_gate,battle_direct_conn,network_probe,heartbeat_idle_smoke_scenario,main}.go`、`robot/logic/handler/scene_client_player_common_redirect_to_gate.go`、`robot/config/config.go`、`robot/go.mod`(`golang.org/x/net` 改为直接依赖,版本不变)、`robot/README.md`、`robot/etc/robot.yaml`(注释掉的示例)。
- `robot/vendor` 由 Gemini 跑 `go mod vendor` 更新,不手改。

文档:本文、`docs/README.md`、`docs/PROGRESS.md`、`docs/design/k8s-client-entry.md`(互链)、`docs/design/third-party-login-end-to-end-design.md` §11.3、Java 仓 `PARITY.md`。

## 10. 参考

- 抖音小游戏 API 概览:<https://developer.open-douyin.com/docs/resource/zh-CN/mini-game/develop/api/javascript-api/overview>
- 微信小游戏 `wx.createTCPSocket`:<https://developers.weixin.qq.com/minigame/dev/api/network/tcp/wx.createTCPSocket.html>
- 微信 Unity WebGL 网络通信适配:<https://developers.weixin.qq.com/minigame/dev/guide/game-engine/unity-webgl-transform/Design/UsingNetworking>
- 微信小游戏「网络」使用说明(域名、证书、并发规则):<https://developers.weixin.qq.com/minigame/dev/guide/base-ability/network.html>
- 抖音 Unity WebGL 网络通信适配:<https://partner.open-douyin.com/docs/resource/zh-CN/mini-game/develop/guide/game-engine/rd-to-SCgame/open-capacity/capability-adaptation/sc_webgl_network>
- 抖音小游戏 `tt.connectSocket`:<https://developer.open-douyin.com/docs/resource/zh-CN/mini-game/develop/api/network/websocket/tt-connect-socket>
- Unity 手册 Web networking:<https://docs.unity3d.com/Manual/webgl-networking.html>
- UnityWebSocket(两家文档的客户端示例,本批没有采用,理由见 W10):<https://github.com/psygames/UnityWebSocket>
- Octops gameserver-ingress-controller(Agones 游戏服的入口转发,域名与路径两种模式):<https://github.com/Octops/gameserver-ingress-controller>
- RFC 6455 The WebSocket Protocol:<https://www.rfc-editor.org/rfc/rfc6455>
