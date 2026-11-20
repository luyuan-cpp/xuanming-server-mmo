# 小游戏客户端经 WebSocket 连接 gate 与 battle(W1–W10)

- **日期**:2026-11-19。
- **状态**:设计稿。改动预计约 60 个文件,超过 AGENTS.md §10.2 的 30 个文件门槛;§9 的文件清单要先得到负责人对这一批的明确同意,才动代码。2026-11-19 负责人已同意 W1(共用端口)、W7(新增地址字段)、W10(客户端改法),并说明 TLS 放在哪按最标准的做法定、Unity 有官方 WebSocket 就用官方的(前提是能上小游戏);W2 / W8(边缘转发)待确认。
- **范围**:让微信、抖音小游戏客户端连上 gate(大厅长连接)和 battle(每局直连、观战直连)。登录侧的小游戏登录已落码(`auth_type` 为 `wechat_minigame` / `douyin_minigame`,见 [第三方登录端到端设计](third-party-login-end-to-end-design.md) §十一),本文只管登录之后的长连接。
- **决策编号**:本文 W1–W10。地址模型沿用 [集群外客户端入口](k8s-client-entry.md) D76(进程自报客户端可达地址)。

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
- `wx.connectSocket` 最多同时 5 条;本设计最多用 3 条(大厅、战斗、观战)。
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
- 握手完成前要发给这条连接的下行帧丢弃并计数。正常不会发生:gate / battle 在客户端验票之前不主动下发业务消息;连接建立时就拒绝的路径直接关连接,WebSocket 客户端看到的是关闭。

### W5 连接生命周期(AGENTS.md §11.7)

- 每条 WebSocket 连接的状态(握手中 / 已打开 / 关闭中、续帧标记、待喂给 codec 的字节)存在 core 的线程局部表里,以连接指针为键,表项只存 `weak_ptr<TcpConnection>` 做身份自校验:锁不住或锁出来不是同一个对象,就当表项不存在并删掉。表不持强引用,连接的所有权仍只在 muduo。
- 断开回调里删表项:gate `main.cpp` 的连接回调、battle `BattleClientEdge` 的连接回调各加一行。漏删时,weak 自校验保证地址复用的新 TCP 连接不会被当成 WebSocket。
- 验票期限(建连后 10 秒,含升级握手)、应用层心跳、空闲超时(验票后静默 30 秒)全部不变,见 [gate 客户端心跳](gate-client-heartbeat.md)。WebSocket Ping 只回 Pong,不算活跃;Pong 忽略。小游戏切到后台时 JS 定时器暂停、心跳停发,gate 照常按空闲超时关闭,回到前台后走现有的断线重连。
- 客户端发 Close:回 Close(回显状态码)后半关。服务端主动关闭(踢线、排空滞留关闭、空闲超时、停机)照旧直接关 TCP,不补发 Close 帧:`forceClose` 会丢弃没写出去的数据,补发也送不到。客户端按断线(1006)处理,和 TCP 断线走同一条恢复路径。

### W6 准入与限流

- 同端口、同 Acceptor:总量上限、新建速率闸、关源天然覆盖 WebSocket 连接([连接准入](gate-connection-admission-control.md) P1-1 ~ P1-3)。ws-edge 给每条客户端连接各开一条上游 TCP,不复用,所以连接数口径不变。
- 经过边缘的连接,`peerAddress()` 是 ws-edge 的 Pod IP。按源 IP 的限制目前本来就没做(同文 P2-2,默认关闭);以后要做时,WebSocket 连接从可信代理的 `X-Forwarded-For` 取源地址,本批不解析这个头。

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
| `RedirectToGateEvent`(Kafka) | `target_gate_ws_url = 8` | scene_manager 跨 zone 重定向 |
| `RedirectToGateInfo` | `target_gate_ws_url = 6` | scene_manager |
| `GateMigrationResponse` | `target_gate_ws_url = 7` | login 排空迁移 |

- login 排队:Redis 里的 `AdmitSlot` 与 `AdmitToken` 加 `ws_url`(JSON `omitempty`),`GateCandidate` 加 `WsURL`。dispatcher 选中 gate 时写入,consume 时带出,和 ip / port 同一条路。
- 本仓 Java gateway(`java/gateway_node`)的 `/api/assign-gate` 与 `/api/queue-status` 用手写 DTO,加 `gateWsUrl`(JSON `gate_ws_url`)并在映射处赋值。
- 不回落、不筛选:`ws_url` 为空表示这台节点不接小游戏,小游戏正式构建拿到空值就报错不连。login 不按客户端传输筛 gate:同一 zone 的 gate 由部署统一注入 env,不存在部分支持。D78 的去重仍按 `client_endpoint`;两个地址属于同一个 Pod,同来同走。

### W8 边缘 ws-edge

- 形态:infra namespace 里一个 Deployment(官方 nginx 镜像,钉版本,至少 2 个副本),Service 为 LoadBalancer、443、`externalTrafficPolicy: Local`;证书放 Secret(`tls.crt` / `tls.key`,完整证书链);`nginx.conf` 放 ConfigMap,由 `tools/scripts/lib/k8s_client_entry.ps1` 生成。只在部署参数给了边缘域名时生成。
- 路由:`/ws/<IPv4>/<端口>` 转发到 `http://<IPv4>:<端口>`。IPv4 用 `geo` 限定在 Pod 网段(部署参数),端口用正则限定为 gate 与 battle 的 RpcPort(部署参数,现为 18000 与 20000),不符合的回 403,其余路径 404。`proxy_read_timeout` / `proxy_send_timeout` 120s(大于 gate / battle 自己的空闲超时,谁该断由 gate / battle 说了算),关代理缓冲,带 `X-Forwarded-For`。nginx 收全请求头才连上游,慢速发头的连接停在边缘,到不了 gate。
- 出站限制:scene 的节点口也是 20000,只靠网段加端口,边缘还能把一个 HTTP 请求发到 scene 的内部口(scene 会按非法帧断开,建不成通道)。所以给 ws-edge 加一条只选中它自己的 egress NetworkPolicy:只许连 `app` 为 gate、battle(含金丝雀)的 Pod 的客户端口。网络插件不执行 NetworkPolicy 时这条不生效,剩下 nginx 的网段与端口限定。仓里现有的 gRPC 口准入策略(`-NetworkPolicies on`)对节点口不设来源限制,不挡边缘。
- 为什么按 Pod IP 定向:gate 票据绑节点(`token_gate_node_mismatch`),连接必须到达指定实例;battle 由 Agones 管,实例随时生灭,没有稳定的 Service。Pod IP 对两者都适用,不用为每台建 Service 或路由规则,也不动 Agones 端口。
- 为什么是独立的 nginx 而不是 Ingress:按路径里的变量转发到任意 Pod 需要 snippet 注解,ingress-nginx 新版默认禁用;独立 nginx 的配置就是一个静态文件,审阅即可。
- 安全:边缘只能转到 Pod 网段里 gate / battle 的客户端端口,而这两个端口本来就对玩家开放,所以它不是开放代理。路径里的 Pod IP 会暴露集群内地址,本批接受。
- 平台后台:socket 合法域名登记 `wss://<ws-edge 域名>` 一个;request 合法域名登记 gateway 的 https 域名(D91 的 Ingress)。两个域名都要 ICP 备案,证书由受信 CA 签发。对外只用 443,不受两家平台对端口的不同规则影响。

### W9 战斗服

- `BattleClientEdge` 用同一个 codec,WebSocket 自动生效。它的两处发帧改调能识别 WebSocket 的发送函数,断开回调补清理。参战直连与观战直连都覆盖。
- `BattleAssignedS2C.ws_url` 取自本节点 `NodeInfo.client_ws_url`;补签(IssueBattleTicket)同路。
- 跨地域测速(`NetworkProbeTarget`,`RegionProbeClient`)小游戏端本批不做:WebSocket 客户端不启动测速,等同于测速全部失败,而测速失败本来就是要处理的情形。

### W10 客户端(`D:\luyuan\mmorpg-client`)

- Unity 没有官方的 WebSocket 接口。Unity 手册「Web networking」写明 Web 平台不支持 `System.Net` 下的任何类,并且「Unity doesn't have a built-in API that allows you to use WebSockets or WebRTC, but you can use a JavaScript plug-in to implement this」。Unity Transport 包的 WebSocket 模式跑的是它自己的连接协议,不是通用的 WebSocket 客户端,gate 不说这个协议。
- 用开源插件 UnityWebSocket(`psygames/UnityWebSocket`,MIT,要求 Unity 2018.3 及以上):微信、抖音两家的 Unity 适配文档都拿它做客户端示例,微信文档注明新版本不用再改 jslib,并附 Demo。它在 WebGL 下经 jslib 调运行环境的 `WebSocket`,其余平台有自己的实现,编辑器里可以直接联调。经 `Packages/manifest.json` 的 git URL 引入,钉到具体提交。
- 客户端已有传输接口 `IFramedConnection`(`Assets/Scripts/Net/IFramedConnection.cs`)。`Connect(host, port)` 改成 `Connect(host, port, wsUrl)`,`GateTcpClient` 忽略 `wsUrl`。
- 新增 `WebSocketFramedConnection : IFramedConnection`,只是 UnityWebSocket 外面的一层适配,换插件只改这一个文件:
  - `Connect` 立即返回;打开前的 `Send` 排队,打开时按序发出;`Connected` 在打开之后才为真;打开失败或 10 秒内没打开,在 `Poll` 里触发 `OnDisconnected`。三个事件仍只在 `Poll` 内触发,线程约定不变(插件的回调只入队)。
- 传输选择:WebGL 构建用 WebSocket,编辑器与原生用 TCP;编辑器可用编译符号 `MMORPG_FORCE_WS` 强制 WebSocket 联调。`ws_url` 为空时,开发构建回落 `ws://<ip>:<port>/`(本机没有边缘),正式构建报错。
- 重定向(msg 124)换连接前的 TCP 探测(`GameClient.ProbeGate`)在 WebSocket 模式下改为「新连接打开」,超时相同,打开之前不摘旧连接,保留「不通就在还连着老 gate 时失败出去」的性质。
- 登录:WebGL 构建经一个几十行的 jslib 调 `wx.login` / `tt.login` 拿 code(运行时看哪个全局对象存在,决定 `auth_type`),用 `LoginCredential.ForThirdParty(LoginAuthTypes.WeChatMiniGame / DouyinMiniGame, code)` 走 `/api/login`。`GatewayHttpClient` 解析 `gate_ws_url`。不用两家 SDK 的 C# 接口(`WX.Login` / `TT.Login`):客户端仓还没装这两个 SDK,引用它们的代码现在编不过;以后接入转换工具时要换,只改这一处。
- 不在本批:把客户端真正转成微信 / 抖音小游戏包(装两家的转换 SDK、构建配置、资源适配)。客户端现在是 Unity 6000.6,两家转换工具支不支持这个版本没有核实。

## 4. 可观测性

- core 计数(进程累计,线程局部,不带玩家或连接维度):握手成功、握手拒绝、协议错误关闭、握手前丢弃的下行帧。gate 挂到现有 `/metrics` 出口,和连接准入指标同一处注册。
- 握手拒绝与协议错误打采样 WARN(第一条必打,之后每 1024 条一行),带关闭码,不带对端地址以外的标识。

## 5. 安全

- 握手解析有界:请求头 8KB、验票期限 10 秒(含升级)共用现有未验签预算;单帧载荷上限同 codec。
- 只接受加掩码的客户端帧(RFC 6455 §5.1),拒绝文本帧与未协商的扩展位。
- 鉴权不变:WebSocket 连接同样必须先过 ClientTokenVerify,票据绑节点。
- 边缘不是开放代理(W8)。

## 6. 数据一致性与兼容

- 不碰任何存储与提交点。新字段全部是追加的可选字段;项目未上线,`AdmitSlot` 的 Redis 数据可清空,`omitempty` 也让新旧形状互相可读。
- 客户端可见契约有变化(4 条消息各加一个字段)。AGENTS.md §12 要求两版同批改;本批按任务约定只在 Java 仓 `PARITY.md` 登记待做,Java 版 gate 在补齐之前不接 WebSocket,新字段对它的客户端无影响。

## 7. 验证(交 Codex,AGENTS.md §10.1)

具体命令在 `docs/PROGRESS.md` 的同名条目里。要点:

1. 协议生成:proto 改动后按 AGENTS.md §4 用默认配置生成(这一批有客户端任务授权,要写客户端仓),再跑 `go/build.ps1` 修 login 包装。
2. C++:先编 core 库再编测试工程(`run_cpp_tests -Build` 不重编库),串行 `/m:1`。新增单测覆盖 RFC 6455 示例向量(握手 Accept 值、§5.7 的单帧 / 分片 / Ping / 256 字节 / 64KiB 帧)、各类拒绝与关闭码、回环端到端(真 `TcpServer` 加原始 socket 客户端完成握手、收发一个客户端帧、Ping/Pong、Close),以及地址拼接与启动校验。
3. Go:login 排队与快路径带出 `ws_url`、scene_manager 重定向带出 `target_gate_ws_url` 的单测。
4. Java gateway:`mvn test`。
5. 部署契约测试:`k8s_client_entry_contract.tests.ps1` 覆盖 env 注入、ws-edge 配置的路由与限定、未给域名时不生成。
6. 联调:本机 gate 加 Unity 编辑器 `MMORPG_FORCE_WS`。微信、抖音开发者工具与真机联调需要开发者账号,不在本批。

## 8. 明确不做

- 微信 `wx.createTCPSocket` 直连(§1)。
- gate / battle 进程内 TLS、`permessage-deflate` 压缩、`Origin` 校验、按源 IP 限流、IPv6 Pod 网络。
- robot 的 WebSocket 模式:vendored `github.com/luyuancpp/muduoclient` 只会自己拨 TCP,没有从现成连接构造客户端的入口;要做先给它加这个入口。本批端到端靠 C++ 回环测试与编辑器联调覆盖。
- 小游戏端跨地域测速(W9)、QQ 小游戏。

## 9. 文件清单(待负责人同意)

协议(6):`proto/common/base/common.proto`、`proto/login/login.proto`、`proto/contracts/kafka/gate_event.proto`、`proto/scene_manager/scene_manager_service.proto`、`proto/scene/client_player_common.proto`、`proto/battle/player_battle.proto`。

C++(约 19):
- core:`network/codec/websocket.{h,cpp}`(新)、`network/codec/codec.{h,cpp}`、`core.vcxproj`、`core.vcxproj.filters`、client_endpoint 头文件(地址拼接与校验)、`node/system/node/node.cpp`(启动校验)、`node/system/node/node_allocator.cpp`(端口定下后拼地址)。
- gate:`main.cpp`(断开清理、注册指标)、`handler/gate_push_delivery.h`(`EncodedFrame` 的 WebSocket 字节)、`handler/event/gate_event_handler.cpp`、`handler/rpc/drain_migration.cpp`(124 拷贝 ws 地址)。
- battle:`client/battle_client_edge.cpp`、`logic/battle_room_manager.cpp`。
- 测试:`cpp/tests/rpc_controller_test/websocket_codec_test.cpp`(新)及其 vcxproj / filters、`cpp/tests/client_endpoint_test` 补用例。

Go(约 12):login 的 `loginqueue/{gatetoken,queue,dispatcher,admit_recheck}.go` 与排队消费处、`svc/servicecontext.go`、`svc/gate_migration.go`、`loginpregate/{assigngatelogic,queryqueuestatuslogic}.go`;scene_manager 的 `logic/{gate_redirect,enterscenelogic}.go`;两到三个测试文件。

本仓 Java gateway(2~3):`dto/AssignGateResponse.java`、`service/AssignGateService.java`,队列状态映射处。

部署(3):`tools/scripts/lib/k8s_client_entry.ps1`(env 注入、ws-edge 的 Deployment / Service / ConfigMap / NetworkPolicy 生成器)、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_client_entry_contract.tests.ps1`。

Unity 客户端(约 13):`Packages/manifest.json`(引入 UnityWebSocket)、`Net/IFramedConnection.cs`、`Net/GateTcpClient.cs`、`Net/WebSocketFramedConnection.cs`(新)、`Plugins/WebGL/MiniGameLogin.jslib`(新)、`Net/MiniGameLogin.cs`(新)、`Game/GameClient.cs`、`Net/BattleDirectLink.cs`、`Net/RegionProbeClient.cs`、`Net/GatewayHttpClient.cs`、登录界面的凭据入口、`Tests/EditMode/Battle/FakeFramedConnection.cs`、一个新的 EditMode 测试;生成的 C# 协议由 protogen 产出,不手改;`Packages/packages-lock.json` 由 Unity 打开工程时更新。

文档(6):本文、`docs/README.md`、`docs/PROGRESS.md`、`docs/design/k8s-client-entry.md`(互链)、`docs/design/third-party-login-end-to-end-design.md` §11.3、Java 仓 `PARITY.md`。

## 10. 参考

- 抖音小游戏 API 概览:<https://developer.open-douyin.com/docs/resource/zh-CN/mini-game/develop/api/javascript-api/overview>
- 微信小游戏 `wx.createTCPSocket`:<https://developers.weixin.qq.com/minigame/dev/api/network/tcp/wx.createTCPSocket.html>
- 微信 Unity WebGL 网络通信适配:<https://developers.weixin.qq.com/minigame/dev/guide/game-engine/unity-webgl-transform/Design/UsingNetworking>
- 微信小游戏「网络」使用说明(域名、证书、并发规则):<https://developers.weixin.qq.com/minigame/dev/guide/base-ability/network.html>
- 抖音 Unity WebGL 网络通信适配:<https://partner.open-douyin.com/docs/resource/zh-CN/mini-game/develop/guide/game-engine/rd-to-SCgame/open-capacity/capability-adaptation/sc_webgl_network>
- 抖音小游戏 `tt.connectSocket`:<https://developer.open-douyin.com/docs/resource/zh-CN/mini-game/develop/api/network/websocket/tt-connect-socket>
- Unity 手册 Web networking:<https://docs.unity3d.com/Manual/webgl-networking.html>
- UnityWebSocket:<https://github.com/psygames/UnityWebSocket>
- Octops gameserver-ingress-controller(Agones 游戏服的入口转发,域名与路径两种模式):<https://github.com/Octops/gameserver-ingress-controller>
- RFC 6455 The WebSocket Protocol:<https://www.rfc-editor.org/rfc/rfc6455>
