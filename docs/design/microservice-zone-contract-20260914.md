# Go 微服务接入 zone 体系的契约 v1(首个落地:chat)

**Created:** 2026-09-14
**状态:** 契约已定。经 3 名架构师起草、3 名反驳者挑战、3 名裁判收敛。2026-09-14 Codex 已完成聊天/注册包单测、真实 etcd 集成、路由服与 robot 构建及脚本检查；运行验收记录见 §15。
**关联:** [xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md)(D-1 / D-2 / D-9,以及本批新增的 D-11 / D-12 / D-13)、[client-rpc-router.md](./client-rpc-router.md)(D29–D34)、[cross-zone-matchmaking.md](./cross-zone-matchmaking.md)(D11)、[guild-zone-client-access.md](./guild-zone-client-access.md)(同口径的并行批次)、[xuanming-port-feasibility-20260902.md](./xuanming-port-feasibility-20260902.md) §8

> 本文是契约的正式版。之后接入的每个 Go 业务服务(friend / guild / team / mail / leaderboard / …)都照这份做。
> 与任何草案冲突时,以本文为准。

---

## TL;DR

1. **业务微服务一律全局一份、多副本**,无状态或有锁保护。
   - 客户端请求:gate → client_rpc_router → 服务。
   - 推送:player_locator 会话 → `gate-cmd_g<N>`。
   - 服务**看不到**请求来自哪个 zone,业务代码不许按 zone 分支。
2. **注册发现只有一份实现**:新包 `go/shared/noderegistry`。
   - key 形状与 C++ 逐字一致。
   - 失租后先重夺原 id;抢不回时,chat 这类服务换 id 继续,login 这类服务退出(D-11)。
3. **客户端入口只承诺路由服模式**。翻转落在部署层:本地默认开,K8s 默认仍关;C++ 默认值一字不改(D-12)。
4. **全局服务不注册 go-zero 发现键**:yaml 里有 `Etcd` 段时写 `Key: ""`,不能省略这一行(D-13)。
5. **chat v1**:
   - 只开 WORLD 和 PRIVATE 两个频道;历史存 Redis LIST,7 天 / 200 条尽力保留。
   - 不建 MySQL 表、不推送、不改 proto。
   - 验收:robot `chat-smoke` 跨 zone 端到端输出 `CHAT_SMOKE_OK`。

### 相对草案的修订(实现时核源码后改的)

| 草案原文 | 本文口径 | 为什么改 |
|---|---|---|
| §7「`Etcd:` 只写 `Hosts`,不写 Key」 | **`Key: ""` 显式留空** | go-zero v1.10.0 的 `discov.EtcdConf.Key` 不是 optional。`Etcd` 段存在而缺 Key 时,`conf.MustLoad` 会 Fatal,K8s 上 Pod 会 CrashLoop(见 D-13 证据) |
| §2「扫描已占 id 时跳过 `/allocated/` 子树」 | **两棵子树的 key 都算占号** | 两棵子树合起来是超集,只会让可用 id 变少,不会撞号;真正防撞号的是 allocKey 上的 CAS(§2.3)。扫描结果只是跳过已占 id 的优化:少跳过一个 id,最多多一次 CAS 失败再试下一个 |
| §9「幂等:SET NX EX 60」 | **两态幂等键 `pending:<token>` / `done`** | 只有一态时,首发还在写入,重发就会被当成功回,消息实际可能丢了。两态下,读到 `pending` 回限速码让客户端重试(§9.3) |
| §7 第 5 条「`-GateRouterMode` 默认 `$true`」 | **`[string]`,取值 `'1'` / `'0'`,默认 `'1'`** | 根目录 `启动服务器.cmd` 经 `pwsh -File` 透传的是字符串;与 K8s 同口径 |

---

## §0 三个含义的 zone

「zone」在本仓库里指三件不同的事,混用就会出错:

1. **部署单位**:gate / scene / login / player_locator / scene_manager / db 按 zone 部署。本地 `dev-start-zones` 每个 zone 各起一套;K8s 上一个 namespace 就是一个 zone。
   - 证据:`tools/scripts/dev_tools.ps1:962-978`。
2. **数据归属标签 home_zone**:唯一真值是 data_service 的 `player:zone:{player_id}`,读接口是 `GetPlayerHomeZone` / `BatchGetPlayerHomeZone`。
   - 证据:`go/data_service/internal/routing/router.go:65`、`:197`、`:219`。
3. **玩家主数据的物理落点**(TiDB Phase 2 之前):scene → Kafka `db_task_zone_{N}` → `zone_{N}_db`。
   - 证据:`go/db/internal/config/config.go:231`。
   - 规则:新全局服务**不得**直连 `zone_{N}_db`,不得生产或消费 `db_task_zone_{N}`,不得经 data_service 的 DevRedis / Regions 直读 `player:{id}:*`。需要玩家主数据时,只能走 D1 发物入口,或等 Phase 2。
   - 为什么:这条链的写者是 C++ scene,落点按 zone 分库。全局服务一旦旁路读写,就会绕开存盘顺序与合服迁移,出现第二份玩家真相。

**业务微服务的共同形态**(friend / guild / chat / team / mail / leaderboard / …):

- 全局一份、多副本;无状态,或有锁保护。
- 客户端经 gate → 路由服到达;推送经 player_locator 会话 → `gate-cmd_g<N>` 到达。
- **目标服务看不到发起请求的 zone**,业务代码禁止读 `cfg.ZoneId` 做分支。
  - 证据:路由服只在自己挑实例时用 `ForwardRequest.zone_id`(`go/client_rpc_router/internal/logic/forwardlogic.go:148`、`:194-198`),向目标透传的只有 `x-` 前缀的 metadata(`:41`);`SessionDetails` 里没有 zone 字段(`proto/common/base/session.proto:6-12`)。
  - 为什么:全局服务的实例对所有 zone 等价。按 zone 分支,等于把一份全局服务偷偷切成多份,而路由服并不按 zone 挑它,结果就是按运气出错。

---

## §1 两条轴:部署轴与路由轴

| 轴 | 由谁决定 | 新服务的规定 |
|---|---|---|
| 部署轴 | `k8s_deploy.ps1` 的 `$GoSvcCatalogue`,`Global = $true` | 表里有跨 zone 的行,就标 Global(D-2)。已知例外:data_service 每 zone 部署,但发现与存储是全局的。这是 Phase 1 过渡态,不作先例 |
| 路由轴 | 路由服 `ZoneScopedNodeTypes`(默认只有 `LoginNodeService`);C++ `node_util.cpp` 的两个 switch 只管 gate / scene 直连 | 新服务不进 `ZoneScopedNodeTypes`;`node_util.cpp` 一行不改 |

- 今天全局池的路由类型是三类:Match / Battle / ClientRpcRouter。
  - 证据:`cpp/libs/engine/core/node/system/node/node_util.cpp:127-139`;zone-scoped 四类见 `:108-121`。
- gate → 路由服这一跳同样是全局随机。本地双 zone 时,zone 1 的 gate 可能走到 zone 2 的路由服,这是设计内的。
- **新 Go 服务的客户端入口只承诺路由服模式**(D-12)。
  - 直连模式下,任何 zone 的 gate 都到不了 chat:直连白名单里没有 Chat(`cpp/nodes/gate/main.cpp:207-209`)。同 zone 过滤只是第二道墙。
- 「翻转」落在**部署层**。C++ 默认值(`cpp/nodes/gate/gate_router_mode.h:31-39`、`:51-55`,未设即直连)和单测(`cpp/nodes/gate/tests/gate_security_test.cpp:102-105`)一字不改。
  - **本地**:`start_game.ps1 -GateRouterMode`,默认 `'1'`。起 gate 前设 `$env:GATE_CLIENT_RPC_ROUTER`,脚本结束还原旧值(`tools/scripts/start_game.ps1:20`、`:33`、`:397-403`、`:451`)。`dev_tools.ps1 dev-start-zones` / `cpp_nodes.ps1` 由父 shell 设这个 env。
  - **K8s**:`k8s_deploy.ps1 -GateRouterMode`,**默认 `"0"`**,只注入 gate Deployment(`tools/scripts/k8s_deploy.ps1:120-135`、`:806-812`)。要等 K8s 上以路由模式跑通过一次 battle-smoke、且路由服部署链补齐(§14 缺口 1、2)才翻。
  - **路由服部署链**(本批已补的部分):
    - `go_svc_image.ps1` 加 `client-rpc-router`;
    - `$GoSvcCatalogue` 加 `"client-rpc-router"`(Global,端口 50600;`k8s_deploy.ps1:404`);
    - `New-GoSvcConfigMapYaml` 加对应 case,带 `IgnoreContentMethods: [/client_rpc_router.ClientRpcRouter/Forward]`、`ZoneScopedNodeTypes: [LoginNodeService]`、`ZoneId`、`LeaseTTL`、`ForwardTimeoutMs`、`Timeout`、`Etcd.Key: ""`;
    - C++ ConfigMap 的 `service_discovery_prefixes` 加 `ClientRpcRouterNodeService.rpc`(`k8s_deploy.ps1:695-706`)。
    - **还缺**:manifest 本身,以及路由服注册时通告的 IP(§14)。
- **回退直连 = chat 和所有只承诺路由模式的服务同时不可达**。回退前先用 killswitch 关掉这些方法,并发公告。

为什么这样定:每加一个服务就改 gate 白名单、重编 gate、滚动重启踢玩家,正是 client-rpc-router.md §1 要消灭的成本;C++ 默认值则是灰度开关的安全兜底(拼错宁可留在直连)。完整论证见 D-12。

---

## §2 注册与发现:`go/shared/noderegistry`(新包,唯一实现)

### 2.1 边界

- shared **不 import proto 模块**(既有纪律:proto 模块反过来依赖 shared)。所以:
  - 注册器只管 key、租约、CAS、keepalive、重注册;
  - NodeInfo 的 protojson 由调用方回调 `BuildValue(nodeID, nodeUUID) ([]byte, error)` 生成。
  - 证据:`go/shared/noderegistry/registry.go:13-18`。
- 既有 7 份非 login 副本与 login 的异源实现**本轮不动**,chat 是 shared 版的第一个消费者。
  - 7 份是:4 份 `internal/noderegistry`(match / scene_manager / data_service / client_rpc_router)+ friend / guild / player_locator 的 `internal/node`。
  - 为什么先不迁:它们都是 internal 包、各自 import proto,语义也有分歧(friend 换 id、login 退出)。一次迁 8 个服务,改动面和回归面都过大,难以审阅(AGENTS.md §10.2、§11.2 小而完整)。先让新服务用上 shared 版,把行为钉稳后再逐个替换;替换时以 shared 版语义为准,不往副本里补丁。
  - 证据:`registry.go:4-11`。

### 2.2 key 形状(与 C++ 逐字一致,测试钉死)

| key | 形状 | 值 |
|---|---|---|
| rpcPath | `<Prefix>/zone/<z>/node_type/<t>/node_id/<n>` | protojson NodeInfo |
| allocKey | `<Prefix>/allocated/node_type/<t>/node_id/<n>` | nodeUUID |

- `Prefix = base.ENodeType_name[nodeType] + ".rpc"`,由调用方派生,不手写(chat:`go/chat/chat.go:108`)。新枚举名不得与既有名互为子串,否则前缀扫描会串到别的服务。
- 证据:
  - Go 侧:`registry.go:200-207`。
  - C++ 侧:`cpp/libs/engine/core/node/system/etcd/etcd_manager.cpp:55-58`、`:72-75`。
  - 测试:`registry_test.go` 的 `TestKeyFormatsMatchCpp`。
- 为什么必须逐字一致:gate / 路由服 / C++ 节点都按这个形状 watch。差一个斜杠,服务就「注册成功但永远不被发现」,而且零报错。

### 2.3 分配与校验

- **已占 node_id 从 key 路径解析**:用 `<Prefix>/` 加 WithPrefix + KeysOnly 扫一次,统一从 key 尾部 `/node_id/<n>` 取值(`registry.go:450-490`)。
  - 尾部 node_id 为 0 的一律忽略;有效区间是 [1, `snowflake.NodeMask`]。
  - 为什么看路径不看值:值可能是坏 JSON,也可能来自半截写入;路径是 CAS 的对象本身,最可靠。
- **注册前校验回调产物**(fail-closed,不合格就拒注册):用手写镜像 struct 解析,字段名照 `go/client_rpc_router/internal/discovery/node_watcher.go:25-40`(`registry.go:387-448`)。要求:
  - `nodeId == nodeID`,`nodeUuid == nodeUUID`,`zoneId == ZoneId`,`nodeType == NodeType`;
  - `endpoint.port != 0`,`grpcEndpoint.port != 0`;
  - `protocolType` 为 `"PROTOCOL_GRPC"` 或数字 `1`。
  - 其中 nodeUuid 相等、nodeType 相等两条比草案更严,理由见 §14。
  - 为什么:路由服按这个镜像解析。坏值注册进去,会在路由服那边被静默丢掉,表现为「实例在跑、路由报 no_target」。
- **写入是同一租约、同一个 Txn**:`If(Version(allocKey)==0) Then(Put allocKey uuid, Put rpcPath value)`(`registry.go:536-555`)。
  - 为什么:两把 key 分开写,中间崩溃就会留下「有占位、没服务」或「有服务、没占位」,后者会被别人重复分配。

### 2.4 失租(D-11)

- keepalive 由一条 `safego.Go` 循环负责:续租 → 发现失租 → 重注册(`registry.go:611-730`)。不再像既有副本那样递归起 goroutine。
- **重注册先 CAS 重夺原 id**(`registry.go:557-582`)。allocKey 不存在,或值仍是自己的 uuid,都算重夺成功。
  - 为什么「值是自己的」也算:客户端先判定失租时,服务端的旧 key 可能还没过期。若把这种情况当成「被抢」,`ExitProcess` 进程会被误杀(与 snowflakealloc F2 同一个教训)。
- 原 id 已被别人占用时,按 `OnReclaimFailed`:
  - `ReallocateNewID`(默认,chat 用):重建值、换 key,并用 `safego.Run` 包住 `OnNodeIDChanged` 回调。
  - `ExitProcess`(login,以及任何以 NodeId 派生持久身份或 per-node topic 的服务):Revoke 新租约 → `logx.Close` → `os.Exit(1)`。
- **不提供 `Lost()`**:发号器失租只听 `snowflakealloc.Handle.Lost()`(`go/shared/snowflakealloc/allocator.go:682`)。两把租约不合并、不共用 lease。
  - 为什么:合并后发现键一抖就会 fence 发号器(`registry.go:20-26`)。

### 2.5 就绪与注销

- **`RegisterAfterListening(ctx, cli, listenAddr, timeout, spec)`**:先探 TCP 能连上,再注册(`registry.go:246`、`:753-790`)。监听 `0.0.0.0` / `[::]` / `:port` 时改探 `127.0.0.1`。写法照 `go/data_service/data_service.go:431-446`。
  - 为什么:路由服发现即转发。先注册后监听,会有一段「被选中但连不上」的窗口。
- **`Close()`**(`registry.go:291`):
  - 顺序:cancel keepalive → 等 keepalive goroutine 退出(最多 closeTimeout = 5s)→ 一个 Txn 删双 key(条件是 `Value(allocKey) == NodeUUID`)→ Revoke。
  - 幂等(`sync.Once`),并且**必须先于 gRPC Stop 调用**。
  - 为什么先注销再停服:路由服的 `PickRandom` 不看连接状态(`node_watcher.go:252-265`)。先停服后注销,中间的请求全打到死端口。
  - 为什么删除要带条件:失租后 id 可能已被别人接管,无条件删会删掉别人的 key。
- **LeaseTTL 作参数,chat 用 60**(`go/chat/etc/chat.yaml:55`)。它就是崩溃场景下的切换窗口:进程被 kill 后,etcd 最长保留死实例 LeaseTTL 秒,这段时间路由服仍可能选中它。
- **ZoneId 必填,`config.Validate` 拒 0**(`go/chat/internal/config/config.go:104`)。它只影响注册路径;K8s 上全局服务取命令行 `-ZoneId`。

### 2.6 API(照契约原文实现)

```go
package noderegistry // 导入路径 "shared/noderegistry"

type ReclaimPolicy int
const ( ReallocateNewID ReclaimPolicy = iota; ExitProcess )
func (p ReclaimPolicy) String() string // 契约外新增,仅供日志

type Spec struct {
    Prefix string; NodeType uint32; ZoneId uint32; LeaseTTL int64
    BuildValue func(nodeID uint32, nodeUUID string) ([]byte, error)
    OnReclaimFailed ReclaimPolicy
    OnNodeIDChanged func(oldID, newID uint32) // 可选;在 keepalive goroutine 里同步调用,不要阻塞
    LogPrefix string
}
type Registration struct { NodeUUID string /* 其余字段未导出 */ }

func Register(ctx context.Context, cli *clientv3.Client, spec Spec) (*Registration, error)
func RegisterAfterListening(ctx context.Context, cli *clientv3.Client, listenAddr string, timeout time.Duration, spec Spec) (*Registration, error)
func (r *Registration) NodeID() uint32 // 原子读
func (r *Registration) KeepAlive()     // 后台续租 + 按策略重注册
func (r *Registration) Close()         // 等 keepalive 退出 → 条件 Txn 删双 key → Revoke;幂等
func RpcPath(prefix string, zoneId, nodeType, nodeId uint32) string
func AllocationKey(prefix string, nodeType, nodeId uint32) string
```

调用顺序固定为:`RegisterAfterListening` → `KeepAlive()` → 退出时先 `Close()` 再停 gRPC。

---

## §3 客户端入口(C2S)

- **proto 规则**:文件级 `OptionFileDefaultNode`;客户端面的 service 标 `OptionIsClientProtocolService=true`;东西向方法不得与之同 service(D-9)。
  - chat v1 **不改 proto**:消息号 28 / 61 已烧进 gate 与路由表(`go/client_rpc_router/generated/pb/game/route_table.go:39-40`)。
- **改 proto 的通用流程**:改 proto → 重生成 → 重部署路由服。
  - 新消息号还要重编 gate,且两边的生成物必须出自同一次 proto-gen。
  - 发布顺序:路由服先,gate 后。
  - 为什么:gate 认识消息号而路由表还不认识,请求会被路由服当 unknown_message 拒掉;反过来则只是暂时用不上。
- **身份**:
  - 会话从 metadata `x-session-detail-bin` 解码进 ctx(照 `go/match/internal/pkg/ctxkeys/ctxkeys.go`;chat 实现在 `go/chat/internal/session/session.go`)。
  - `sender_player_id` 一律被会话里的 player_id **覆盖**;只有 `target_player_id` 这类「对方是谁」的字段才信请求体。
  - chat 首个提交就同时带上拦截器与逻辑层读会话,不能先交付一个信 `req.player_id` 的版本(D-9)。
  - 无会话时回非故障码 `kInvalidParameter`(`go/chat/internal/logic/chat_logic.go:111`)。**不能用 `kPlayerNotFoundInSession`**:它是 fault 码,会把客户端的非法请求计成服务端故障。
- **拦截器链**(新口径,仓内没有现役样板):
  - 顺序:`grpcstats`(最外)→ `killswitch` → session 解码 → `serverbase`(`TipClassifier = serverbase.TipVerdict`)→ handler。
  - 以 `go/friend/friend.go:105` 的 `buildUnaryInterceptors` 为模板。链写成可测函数(`go/chat/chat.go:174`),配三个测试:`TestBuildUnaryInterceptorsOrder`、`TestKillSwitchWiredIntoUnaryChain`(SetRules 后 handler 不被调)、`TestKillSwitchFailOpenWithoutRules`。
  - 为什么 killswitch 在 session 解码之前:被关停的请求不白解码。为什么要守链测试:「哪次重构顺手删了 killswitch 那行」是零报错的事故。
- **超时预算**:业务服务 zrpc `Timeout ≤ ForwardTimeoutMs − 1000`。
  - 路由服 `ForwardTimeoutMs: 5000`(`go/client_rpc_router/etc/client_rpc_router.yaml:32`),所以 chat `Timeout: 4000`(`go/chat/etc/chat.yaml:9`);`Validate` 拒 0 与 >4000(`config.go:20`、`:112`)。
  - 为什么:上游先于下游超时,会把下游的正常慢响应判成故障,客户端再重试,请求就翻倍。
- **幂等**:有副作用的 C2S 必须带幂等键。SendChat 用 `request_id`(§9.3)。
- **限速两层**:
  - 第一道:gate 每会话、每消息号的 MessageLimiter。28 / 61 不在表里,吃默认档 3 次 / 1 秒窗口(`robot/chat_smoke_scenario.go:72-77` 注释)。
  - 第二道:chat 的 `chat:{rl:<pid>}`,默认 5 次 / 秒,一条 Lua 做 INCR,结果为 1 时 EXPIRE 1。
- **长度两层**:
  - 第一道:gate 对序列化后的请求体超过 1024B 直接拒(`kMessageSizeExceeded`)。
  - 第二道:chat 按**字节**限 `MaxContentBytes`(默认 512,`config.go:48`),并要求 trim 后非空;超限或为空都回 `kMessageSizeExceeded`。

---

## §4 数据归属

- **表的落点**:新全局服务的表落全局库,不落 `zone_{N}_db`;主键只用 player_id / 业务 id,不含 zone。
  - chat v1 **零 MySQL**。首个要建表的全局服务按 [port-decisions D-14](xuanming-port-decisions-20260910.md) 落库与建表:每服务一库 `mmorpg_<svc>`,表以 proto 为源,迁移走 `go/schemamigrate` + 服务 `-migrate` 的 K8s Job(随该服务同批落地)。
- **`zone_id` 列就是 home_zone**:唯一写入路径是服务端调 `BatchGetPlayerHomeZone`;未映射时 fail-closed,拒绝创建。
  - guild 旧的 `req.ZoneId` 直落不作先例。chat v1 没有归属数据,不接 `merge:in_progress` 围栏。
- **Redis 双句柄**:
  - 契约 key(`player:{id}:location` / `player:session:{id}` / `battle:lock:{id}`)只读 SharedRedis。SharedRedis 禁 Cluster 分片,主从 / 哨兵可以。
  - 服务私有 key 用独立句柄 `ChatRedis`(go-zero `redis.RedisConf`,可 `Type: cluster`;yaml 不得写 DB)。缺省时回落共享库,启动打 **WARN** 并打印落点(`go/chat/internal/svc/servicecontext.go:72-78`)。
  - 为什么禁切集群:共享库上有 C++ scene 与多个 Go 服务的跨 slot Lua / MULTI(清单见 cross-zone-matchmaking.md §10.0)。
- **Redis 是某类数据的唯一权威时**,staging / prod 的私有 Redis 必须是独立实例,且 `maxmemory-policy=noeviction`。compose 的共享库是 allkeys-lfu;chat 本批暂用的 `redis-match-cluster` 是 volatile-lru(`k8s_deploy.ps1:1916-1921`)。
- **key 规则**:单 key 操作不需要同 slot;跨 key 原子才收 hash tag;不用 SCAN,不做跨 slot 多 key 操作。
- **发号器**:每类永久身份只能有一个发号器。chat v1 没有发号器,不接 snowflakealloc。

---

## §5 推送(S2C)

- **共享收口** `kafkautil.PushToPlayer` / `BroadcastToPlayers`(`go/shared/kafkautil/gate_push.go:50`、`:87`)只做三件事:组信封、选 topic 与分区、写入。
- **调用方负责查会话**:读 `player:session:{id}` → `proto.Unmarshal(PlayerSession)` → 只推 `State==ONLINE` → 组 `PlayerGateInfo`。
  - 模板:`go/match/internal/logic/push.go:23`、`:70`。每个服务自带 `internal/kafka/gate_command_builder.go`。
- **Writer 必配**:`Balancer=&kafkacmd.CommandPartitionBalancer{Fallback:&kafka.Hash{}}`、`RequiredAcks=RequireOne`、`Async=false`(`go/match/internal/svc/servicecontext.go:142`、`:149`)。
- **投递语义是 at-most-once**,有三种丢失窗口:gate 重启从 OFFSET_END 起消费、不回放;会话 DISCONNECTING 期间不推;跨 zone redirect 窗口。
  - 所以每个业务域都必须有全量拉取 RPC,S2C 推送只用来触发客户端去拉(本契约首次拍板,来源 feasibility D13)。
- **chat v1 不推送**:
  - WORLD:gate 的 `BroadcastToAll` Kafka 路径是空实现(`cpp/nodes/gate/handler/event/gate_event_handler.cpp:369-373`)。
  - PRIVATE:主动选择不推。
- **v1.1 的前置**:
  - `ChatMessage` 追加 `message_id`,`PullChatHistoryRequest` 追加 `since` 游标(append-only);
  - Notify* 类方法按 D-9 二选一:拆成独立 service,或在 handler 里拒绝带 `x-session-detail-bin` 的来源;
  - 先做私聊单播;WORLD 广播单独立项。

---

## §6 Kafka 消费者(chat v1 没有)

- 全局 topic 的消费者,yaml 里不得出现能被 `go_services.ps1` 正则 `^\s*GroupID:\s*"…"` 命中的行(`tools/scripts/go_services.ps1:375`),改用 `<用途>ConsumerGroup` 这类键名。
  - 为什么:`-Zone` 会给命中的 GroupID 加 `_z<N>`。全局 topic 被两个 zone 各自当独立消费组读,同一条消息会被处理两遍。
- 幂等标记的 TTL ≥ topic 保留期。
- 分区数是生产方与消费方共享的契约,不能单方面改。

---

## §7 部署登记(chat 五处 + 路由服链)

| # | 文件 | 登记内容 | 证据 |
|---|---|---|---|
| 1 | `tools/scripts/go_services.ps1` `$ServiceCatalogue` | `chat`:Dir chat / Entry chat.go / Port 50700 / `-f etc/chat.yaml` / AllowMultiInstance / Tier 1;**无 Global 字段** | `go_services.ps1:147` |
| 2 | `tools/scripts/go_svc_image.ps1` `$Catalogue` | `chat` → `mmorpg-chat`;`client-rpc-router` → `mmorpg-client-rpc-router` | 同文件 `$Catalogue` |
| 3 | `tools/scripts/k8s_deploy.ps1` | `$GoSvcCatalogue.chat`(Global)+ `New-GoSvcConfigMapYaml` 的 chat case | `k8s_deploy.ps1:396`、`:1885-1942` |
| 4 | `deploy/k8s/manifests/go-svc/chat.yaml` | 照 match.yaml:Service + Deployment(replicas 2、podAntiAffinity)+ 同文件 PDB;端口 50700,metrics 9210;POD_IP 走 Downward API;不写 namespace;不挂 snowflake-cache 卷 | manifest `:76`(POD_IP) |
| 5 | `tools/scripts/start_game.ps1` | `$services` 与第二批 `Start-LocalGoServices` 加 chat;`-GateRouterMode` | `start_game.ps1:26`、`:419` |

第 3 处 ConfigMap 的取值:

- 契约值(`Timeout` / `LeaseTTL` / `MaxContentBytes` / `RateLimitPerSecond` / `HistoryMaxEntries` / `HistoryTTLSeconds`)用 `Get-AuthoritativeScalar` 从 `go/chat/etc/chat.yaml` 读,不在脚本里抄第二份数。
- 固定值:`ListenOn: 0.0.0.0:50700`;`Etcd.Hosts` 加 `Key: ""`(D-13);`Middlewares.StatConf.IgnoreContentMethods: [/chatpb.ClientPlayerChat/SendChat]`;`ZoneId: ${CurrentZoneId}`;`MetricsListenAddr: ":9210"`。
- `ChatRedis` 指向 `redis-match-cluster` 的六节点串,注释写明风险(§4)。

规则与为什么:

- **端口五处一致(50700)**:go_services 的 Port / 本地 yaml 的 ListenOn / k8s 的 Port / ConfigMap 的 ListenOn / manifest 的 Service、containerPort 与探针。本地写 `127.0.0.1:50700`,K8s 写 `0.0.0.0:50700`。
  - 为什么:任何一处不一致,表现都是「Pod Ready 但探针或路由连不上」。
  - 本地 zone 2 按 `ZonePortShift` 1000 位移到 51700,落进已观测的 Windows 保留区 51573-51872,由 `Resolve-BindablePort` 自动上挪。
- **指标端口分工**:9101 login / 9150 scene_manager / 9160 db / 9170 match / 9180 friend / 9190 player_locator / 9200 路由服 / **9210 chat** / 9220 guild。
  - 必须是顶层单行 `MetricsListenAddr: ":9210"`,因为 `-Zone` 的端口位移正则只认这个形状。
- **启动横幅**:main 打印 `=====` 框住的 `CHAT SERVICE STARTED SUCCESSFULLY`(`go/chat/chat.go:127`)。`go_services.ps1` 靠这行字判就绪(`go_services.ps1:472`),并要求在 TierReadySeconds 内进入 TCP LISTEN。
- **yaml 锚点**:
  - 顶层单行 `ListenOn:`、顶层 `ZoneId:`、顶层 `MetricsListenAddr:`;
  - `Etcd:` 写 `Hosts` 加 `Key: ""`;
  - 不声明任何 `RpcClient` 的 `Etcd.Key`。
  - 为什么:`-Zone` 派生 yaml 靠这几个锚点改写,锚点形状一变就静默不生效。
- **go-zero Stat 屏蔽正文**:本地 yaml 和 ConfigMap 都带 `IgnoreContentMethods: [/chatpb.ClientPlayerChat/SendChat]`(`chat.yaml:27`、`k8s_deploy.ps1:1894-1897`)。
  - 为什么:Stat 默认按 INFO 打整包 JSON,SendChat 的请求体就是聊天正文(含私聊),不能进 Pod 日志或 Loki(AGENTS.md §11.3)。
- **本地双 zone**:保持每 zone 一份。多实例只在路由服与 NodeInfo 的视角下等价。`ZonePortShift` 默认 1000 不改;`go_services.ps1 stop` 不看 `-Zone`,会停掉所有 zone 的同名服务。

---

## §8 C++ scene → 全局 Go 服务(东西向;chat v1 没有这条边)

- **路由服不是东西向通道**:它缺会话就回 Unauthenticated。
- 内部调用只有两条路:
  - Kafka(以 db_task 为样板,带幂等键);
  - 直连 gRPC,配一个专用选择器:不比 zone、只挑 READY 的实例(照 `PickReadyDataServiceNode`,不照 `GetSceneManagerEntity`)。同时要补 `nodeTypeNameMap`、两份前缀表、scene 白名单。
- 为什么:给路由服开「无会话放行」等于给所有客户端开了伪造内部调用的口子(D-9 的同一类风险)。

---

## §9 go/chat v1

### 9.1 形态

- `ChatNodeService = 9`、`NODE_CHAT = 14` 已存在;proto 不改。
- 频道:
  - WORLD:全服唯一的世界频道,与 scene_manager 的 `world_channels:zone:{z}` 无关。
  - PRIVATE:私聊。
  - TEAM / SYSTEM / UNSPECIFIED 回 `kFeatureUnavailable`(`constants.go:30`)。
- 启动顺序(`go/chat/chat.go`):MustLoad(含 Validate)→ svcCtx → metrics → killswitch → MustNewServer + 拦截器链 → `go s.Start()` → `RegisterAfterListening`(`ReallocateNewID`)→ `KeepAlive` → 打横幅。
- 退出顺序（2026-09-15 修正）：`nr.Close()` 注销尝试返回 → 真实 `grpc.Server.GracefulStop`（最多5s，超时异步Stop并报告失败）→ 取消killswitch、关闭metrics及etcd → `proc.Shutdown`/等待Start（共2s）→ 刷日志。Linux框架自动停机推迟至24s硬截止，正常预算内由chat串行收尾；启动期退出信号和晚到Server也进入统一清理。Close不返回删除确认，etcd传输失败仍依赖LeaseTTL。见§16。
- 写进 NodeInfo 的 IP:优先 `POD_IP`,其次 ListenOn 的 host,再次 `netx.InternalIp`,最后 `127.0.0.1`(`chat.go:235-240`)。
  - 为什么:容器里 ListenOn 恒为 `0.0.0.0`,把它写进 NodeInfo,别的 Pod 连不上。

### 9.2 存储(ChatRedis,全是单 key 操作,集群安全)

| key | 类型 | 写法 |
|---|---|---|
| `chat:{world}:log` | LIST | 一条 Lua:LPUSH + LTRIM(`HistoryMaxEntries`=200)+ EXPIRE(`HistoryTTLSeconds`=604800) |
| `chat:{p:<小id>:<大id>}:log` | LIST | 同上;双方共用一把 key |
| `chat:{req:<pid>}:<request_id>` | STRING | 两态幂等键,见 9.3;EX = `RequestIdTTLSeconds`(60) |
| `chat:{rl:<pid>}` | STRING | 一条 Lua:INCR 后若为 1 则 EXPIRE 1 |

- 每条值是 `proto.Marshal(ChatMessage)`。服务端盖 `send_time_ms`、覆盖 sender(`chat_logic.go:10-13`、`:39`)。
- 为什么 hash tag 按「对话 / 玩家」分:同一对话的操作永远落同一 slot,不同对话自然分散;没有任何跨 key 原子需求。

### 9.3 SendChat 顺序与幂等

**处理顺序**:会话 → 频道 → 字节长度与 trim 后非空 → `request_id` ≤64 字节 → 盖 `send_time_ms` 并覆盖 sender → 幂等占位 → 限速 Lua → 写入 Lua → 幂等键改 `done`(`chat_logic.go:144-249`)。

**两态幂等键**(相对草案的修订):

- 占位:`SET NX EX` 写入 `pending:<随机 token>`；每次占用使用独立 token。
- 写入成功后,用一条 Lua 比较完整 `pending:<token>`，仅本次仍持有占用时改 `done` 并重置 EX；旧请求不能完结新请求的在途状态。
- 重发时读键:
  - 读到 `done`:回成功,不重写(outcome=duplicate)。
  - 读到 `pending` 或空:回 `kRateLimitExceeded`(业务拒绝码,不是 fault 码;outcome=in_flight),让客户端稍后重试。
  - GET 出错:回 `kServiceUnavailable`。
- 占住幂等键之后,任何一步失败(限速、Redis 故障)都用 Lua 比较完整 token 后释放自己的占用，客户端可以用同一 `request_id` 重试；旧请求不能删除新请求的 pending/done。
- `request_id` 为空时不做幂等。
- 为什么:只有一态时,首发写入还在路上,重发就被当成功回;首发一旦失败,消息静默丢失,客户端却以为发出去了。

**取舍与限制**:

- 写入 Lua 超时但实际已落库时,重试会多出一条重复消息。偶发重复比静默丢失更可接受。
- `request_id` ≤64 字节:它是客户端可控的字符串,直接拼进 Redis key,必须限长。

### 9.4 PullChatHistory

- 快照式读取:`limit` 钳到 ≤50,0 取默认 20 → LRANGE → 反序列化(坏条目跳过)→ 按 `send_time_ms` 稳定倒序,新的在前(`chat_logic.go:256`)。
- PRIVATE 必须给 `peer_player_id`;为 0 回 `kInvalidParameter`。
- 历史是 7 天 / 200 条的尽力而为窗口,不是权威账本;Redis 重启即清空。

### 9.5 错误码

只用 common 段既有码,常量一律 `uint32(table.CommonError_kX)`(`go/chat/internal/constants/constants.go:19-35`):

| 常量 | 码 | 场景 | fault |
|---|---|---|---|
| `ErrInvalidParameter` | kInvalidParameter | 无会话 / 参数错 / 频道未知 | 否 |
| `ErrMessageTooLong` | kMessageSizeExceeded | 超长 / trim 后为空 | 否 |
| `ErrRateLimited` | kRateLimitExceeded | 限速;幂等键 in_flight | 否 |
| `ErrChannelUnavailable` | kFeatureUnavailable | TEAM / SYSTEM / UNSPECIFIED | 否 |
| `ErrStorage` | kServiceUnavailable | Redis 错误 | **是**(只用于真故障) |

- `TestNoHandWrittenTipCodes` 用 go/parser 扫 constants.go 守住「不许手写数字」;`TestTipCodesVerdicts` 断言只有 `ErrStorage` 判为 fault。
- chat 私有码段等 Tip.xlsx 开段后再替换。

### 9.6 指标(`:9210/metrics`,低基数,不含 player_id)

- `chat_send_total{channel,outcome}`、`chat_pull_total{channel,outcome}`。
- channel 取值:world / private / team / system / unspecified / unknown。
- outcome 取值:ok / duplicate / in_flight / no_session / bad_request / channel_unavailable / too_long / rate_limited / storage_error。

### 9.7 冒烟(robot `mode: chat-smoke`)

- 配置 `robot/etc/chat_smoke.yaml`,账号 robot_9005(A)/ robot_9006(B)。
- 流程:
  1. A 登 zone_a,B 登 zone_b。`cross_zone: true` 时两 zone 必须不同,且断言两个 gate 地址不同;`false` 时同 zone,弱验收。
  2. A 发 WORLD `"world <nonce>"` → B 拉 WORLD(limit 20),看到 nonce 且 sender == A。
  3. A 私聊 B → B 拉 PRIVATE(peer = A),看到这条。
  4. A 发 600 字节 ASCII → 期望 chat 侧回 `kMessageSizeExceeded`。600 字节超过 chat 的 512 上限,但整包仍小于 gate 的 1024B,保证拒绝来自 chat 而不是 gate。
  5. A 用同一 `request_id` 重发 → 期望 tip 0,且历史里该 nonce 只有一条。
- 输出:`CHAT_SMOKE_OK player_a= player_b= gate_a= gate_b=`(退出码 0),或 `CHAT_SMOKE_FAIL step= reason=`(退出码 1)(`robot/chat_smoke_scenario.go:141`、`:294`)。
- 实现要点:
  - 同一机器人相邻两个请求至少间隔 1.1s(`:77`),否则 gate 默认限速会让第 5 步的重发根本到不了 chat,测试假绿。
  - 登录用自己的 `chatSmokeLogin`,不用 `prepareBehaviorClient`。后者的接收回调只拦截属性消息的 gate 信封错误;被 gate 或路由服拒绝的聊天请求会以空 body 交给 handler,被记成 tip 0,同样假绿。

---

## §10 记账

- 推翻或补充既有决策的三条,已追加到 `xuanming-port-decisions-20260910.md`:
  - **D-11** 失租口径:推翻 feasibility D9 纠正层②。
  - **D-12** 客户端入口只承诺路由服模式,翻转在部署层:补充 client-rpc-router D34。
  - **D-13** 全局服务 yaml 不注册 go-zero 发现键(`Key: ""`):修正 goctl 模板口径。
- 不动:`node_util.cpp`、既有 8 份注册实现、gate C++。
- 待拍板项见 §13。

---

## §11 本批交付物

> 本节列的是原实现范围；Codex 的实际编译、测试和运行证据见 §15。

### 11.1 文件清单

**共享库(`go/shared`)**

| 文件 | 新 / 改 | 一句话 |
|---|---|---|
| `go/shared/noderegistry/registry.go` | 新 | 唯一注册实现:CAS 分配、双 key 同 Txn、keepalive + 两档失租策略、`RegisterAfterListening`、条件删除的幂等 `Close` |
| `go/shared/noderegistry/registry_test.go` | 新 | 纯单测(不连 etcd):key 格式对齐 C++、node_id 区间、从 key 解析、回调产物校验、Spec 校验、监听地址换算、等端口 |
| `go/shared/noderegistry/registry_integration_test.go` | 新 | `//go:build integration`,连 127.0.0.1:2379,13 个用例覆盖分配、并发、Close、失租重夺 / 换号 / 退出、Close 后提交、等端口 |
| `go/shared/go.mod` | 改 | `github.com/google/uuid v1.6.0` 从 indirect 挪进直接 require,其余不动 |

**chat 服务(`go/chat`)**

| 文件 | 新 / 改 | 一句话 |
|---|---|---|
| `go/chat/go.mod` / `go.sum` | 新 | module chat,go 1.24.5,go-zero v1.10.0,replace proto / shared;go.sum 从 match 复制,待 `go mod tidy` 修剪 |
| `go/chat/chat.go` | 新 | main:启动与退出顺序、可测拦截器链 `buildUnaryInterceptors`、NodeInfo 通告地址、就绪横幅 |
| `go/chat/chat_test.go` | 新 | 链顺序、killswitch 接入与 fail-open、会话拦截器 fail-open、tip 码守卫、NodeInfo 值符合注册器契约 |
| `go/chat/etc/chat.yaml` | 新 | 本地配置:50700、Timeout 4000、`Etcd.Key: ""`、IgnoreContentMethods、共享 Redis + ChatRedis 集群、ZoneId、LeaseTTL 60、`:9210`、killswitch 用法注释 |
| `go/chat/internal/config/config.go` | 新 | 配置结构与 `Validate`(拒 ZoneId=0、Timeout 越界、非空 Etcd.Key、上限非正等) |
| `go/chat/internal/constants/constants.go` | 新 | 5 个错误码常量(只引用生成枚举)与 `TipClassifier` |
| `go/chat/internal/session/session.go` | 新 | `x-session-detail-bin` 解码拦截器与 ctx 读写;坏头放行但打日志 |
| `go/chat/internal/svc/servicecontext.go` | 新 | etcd 客户端、Redis 双句柄与回落 WARN、chat 指标 |
| `go/chat/internal/server/chatserver.go` | 新 | gRPC 薄包装,委托给 logic |
| `go/chat/internal/logic/chat_logic.go` | 新 | SendChat(两态幂等 / 限速 / 写入 Lua)与 PullChatHistory |
| `go/chat/internal/logic/chat_logic_test.go` | 新 | miniredis 覆盖:无会话、WORLD / PRIVATE 可见性、长度、限速、幂等(含 in_flight)、未开放频道、limit 钳制、排序、Redis 故障 |

**部署与脚本**

| 文件 | 新 / 改 | 一句话 |
|---|---|---|
| `deploy/k8s/manifests/go-svc/chat.yaml` | 新 | Service + Deployment(2 副本、反亲和、grpc 探针、preStop、POD_IP)+ PDB |
| `tools/scripts/go_services.ps1` | 改 | `$ServiceCatalogue` 加 chat 及注释 |
| `tools/scripts/go_svc_image.ps1` | 改 | `$Catalogue` 加 chat 与 client-rpc-router |
| `tools/scripts/k8s_deploy.ps1` | 改 | `-GateRouterMode`;chat 与 client-rpc-router 的目录与 ConfigMap case;`service_discovery_prefixes` 加路由服;manifest 缺失时整条跳过(连 ConfigMap 也不 apply) |
| `tools/scripts/start_game.ps1` | 改 | 本地拉起 chat;`-GateRouterMode`(默认 `'1'`,起 gate 前设 env,结束还原) |

**robot**

| 文件 | 新 / 改 | 一句话 |
|---|---|---|
| `robot/etc/chat_smoke.yaml` | 新 | chat-smoke 配置,文件头写明前置条件 |
| `robot/chat_smoke_scenario.go` | 新 | `RunChatSmoke`:双 zone 登录 → 五步断言 → 清理;自带登录与信封错误拦截 |
| `robot/config/config.go` | 改 | `ChatSmokeConfig` 与 `chat-smoke` 模式校验 |
| `robot/main.go` | 改 | 加 chat-smoke 分支 |
| `robot/logic/handler/client_player_chat_send_chat.go` | 改 | 按玩家记录 SendChat 回包 tip(每人最多 64 条) |
| `robot/logic/handler/client_player_chat_pull_chat_history.go` | 改 | 按玩家记录拉取回包 |

**文档**

| 文件 | 新 / 改 | 一句话 |
|---|---|---|
| `docs/design/microservice-zone-contract-20260914.md` | 新 | 本文 |
| `docs/design/xuanming-port-decisions-20260910.md` | 改(只追加) | D-11 / D-12 / D-13 |
| `PROGRESS.md` | 改(只追加) | 2026-09-14 本批条目 |

路由服 manifest 已落到 `deploy/k8s/manifests/go-svc/client-rpc-router.yaml`,路由服同时改为按 `POD_IP` 通告(`go/client_rpc_router/client_rpc_router_service.go` `advertisedHost`);均为 2026-09-14 复核时补,见 §14.1 缺口 1、2。

### 11.2 D-1 四条验收如何核

| # | D-1 判据 | 静态证据(已具备) | 运行核对(§12 步骤) |
|---|---|---|---|
| ① | 进目录,一键栈拉得起 | `go_services.ps1:147`;`start_game.ps1:26`、`:419`;横幅 `chat.go:127` | ④ build 产出 chat.exe;⑥ `go_services.ps1 -Command status` 中 chat 为 RUNNING、横幅出现、50700 LISTEN、etcd 有 `ChatNodeService.rpc/zone/...` |
| ② | 路由表 `ClientProtocol=true`,且路由模式下 gate 不拒 | `route_table.go:39-40`;gate 启动日志打出口模式(`main.cpp:225`) | ⑥ gate 日志「出口模式=router」;⑦ 路由服 `client_rpc_router_forward_total` 中 SendChat / PullChatHistory 的 `outcome="ok"`。区分:`no_target` = 没发现 chat 实例(部署问题);outcome 为 ok 但 tip 非 0 = chat 的业务拒绝。Unity handler 已由生成器产出;UI 属客户端任务,未授权,由 robot 代替验收 |
| ③ | 有一条端到端冒烟 | `robot/chat_smoke_scenario.go` | ⑦ 连续两次 `CHAT_SMOKE_OK`;⑧ 杀掉一个实例后仍通过 |
| ④ | killswitch 挂上 + 守链测试 | `chat.go:77-80`、`:174` | ③ `TestKillSwitchWiredIntoUnaryChain` / `TestKillSwitchFailOpenWithoutRules` / `TestBuildUnaryInterceptorsOrder` PASS;可选 ⑫ 活体关停 |

---

## §12 Codex 验证清单(按序执行)

> 约定:
> - 仓库根 = `E:\work\xuanming-server-mmo`;命令在 PowerShell 7 下执行。
> - 本机 `CGO_ENABLED=0`,不加 `-race`。
> - 依赖下载失败时设 `$env:GOPROXY='https://goproxy.cn,direct'` 再试,不要改 go.mod 版本。
> - 任何一步失败,先按 ⑨ 保留现场,再停下报告;不连续重试。

### ① noderegistry:格式、静态检查、单测(不需要 etcd)

- 目录:`go/shared`
- 命令:
  ```powershell
  gofmt -l noderegistry
  go build ./...
  go vet ./noderegistry/...
  go vet -tags=integration ./noderegistry/...
  go test ./noderegistry/... -count=1 -v
  ```
- 通过标准:
  - `gofmt -l` 输出为空。若列出文件,执行 `gofmt -w noderegistry` 并回报 diff。
  - 其余命令退出码都是 0。
  - 13 个单测全部 PASS:`TestKeyFormatsMatchCpp`、`TestNodeIDRangeMatchesSnowflake`、`TestNodeIDFromKey`、`TestUsedNodeIDsFromKeys`、`TestValidateValue`、`TestSpecValidate`、`TestReclaimPolicyDefaults`、`TestRegisterRejectsBadInputsBeforeAnyIO`、`TestDialAddrForListen`,以及 4 个 `TestWaitForListening_*`。等端口相关用例偶发 SKIP(端口被抢)可接受,FAIL 不可接受。
- 附加:`go mod tidy -diff` 的输出里不应出现 `github.com/google/uuid` 相关的行。只记录,不写盘。

### ② noderegistry 集成测试(需要本地 etcd)

- 前置:仓库根执行 `pwsh tools/scripts/dev_tools.ps1 -Command etcd-up`,确认 127.0.0.1:2379 可达。
- 目录:`go/shared`
- 命令:`go test -tags=integration ./noderegistry/... -count=1 -v -timeout 300s`
- 通过标准:13 个集成用例全部 PASS,**不能出现 SKIP**(SKIP 说明没连上 etcd,不算通过):
  - `TestRegister_WritesBothKeysOnTheSameLease`、`TestRegister_SkipsIDsOccupiedInKeyPaths`、`TestRegister_ConcurrentAllocationsAreUnique`、`TestRegister_InvalidValueWritesNothing`
  - `TestClose_DeletesBothKeysRevokesLeaseAndIsIdempotent`、`TestClose_LeavesKeysOfTheInstanceThatTookOverTheID`、`TestClose_WaitsForKeepAliveGoroutineToExit`
  - `TestKeepAlive_ReclaimsOriginalIDAfterLeaseRevoked`、`TestKeepAlive_ReallocatesNewIDWhenOriginalTaken`、`TestKeepAlive_ExitProcessPolicyExitsWhenOriginalTaken`
  - `TestReRegister_ReclaimsWhileOldKeysAreStillLive`、`TestCommitAfterCloseRevokesTheNewLease`、`TestRegisterAfterListening_WaitsForThePort`
- 清理检查:`docker exec etcd etcdctl get NoderegistryTest --prefix --keys-only` 输出为空。

### ③ go/chat:依赖、静态检查、单测

- 目录:`go/chat`
- 命令:
  ```powershell
  go mod tidy
  go vet ./...
  go test ./... -count=1 -v 2>&1 | Tee-Object chat_test.log
  ```
- 通过标准:
  - `go mod tidy` 退出码 0。require 块仍含 go-zero v1.10.0、etcd client v3.5.15、grpc v1.79.3、protobuf v1.36.11、miniredis v2.37.0、testify v1.11.1、prometheus client_golang v1.23.2、proto v0.0.0、shared v0.0.0。多余的 indirect 行被删属于预期。
  - `go vet` 无输出。
  - 根包 7 个测试全部 PASS:`TestBuildUnaryInterceptorsOrder`、`TestKillSwitchWiredIntoUnaryChain`、`TestKillSwitchFailOpenWithoutRules`、`TestSessionInterceptorFailOpen`、`TestNoHandWrittenTipCodes`、`TestTipCodesVerdicts`、`TestNodeInfoValueMatchesRegistryContract`。
  - `internal/logic` 11 个测试全部 PASS:`TestNoSessionRejected`、`TestWorldSendThenPullVisibleWithSenderOverridden`、`TestPrivateBothDirectionsShareOneKey`、`TestContentLengthLimits`、`TestRateLimitSixthRejected`、`TestSameRequestIdStoredOnce`、`TestSameRequestIdWhileFirstInFlightNotReportedAsSuccess`、`TestNonOpenChannelsRejected`、`TestPullLimitClamped`、`TestPullSortedBySendTimeDesc`、`TestRedisFailureMapsToServiceUnavailable`。最后一个因 go-redis 重试可能耗时 1-3s,属正常。
- 消费方回归(shared 的 go.mod 改了):分别在 `go/client_rpc_router`、`go/match` 目录执行 `go build ./...`,都应退出码 0。

### ④ 构建 chat.exe

- 目录:仓库根
- 命令:
  ```powershell
  pwsh -NoProfile -File tools/scripts/go_services.ps1 -Command build -Services chat
  pwsh -NoProfile -File tools/scripts/go_services.ps1 -Command list
  ```
  build 等价于在 `go/chat` 下执行 `go build -o ..\..\bin\go_services\chat.exe ./chat.go`(`go_services.ps1:795-833`)。
- 期望产物:`bin/go_services/chat.exe`。
- 通过标准:输出 `[ok]    chat`;list 中有 `chat  50700  Chat (全局聊天,路由服可达)`。
- 若 `bin/go_services/client_rpc_router.exe` 比 `go/client_rpc_router` 源码旧,一并执行 `-Services client_rpc_router` 重编。

### ⑤ robot 重编

- 目录:`robot`
- 命令:
  ```powershell
  go build -mod=vendor -o robot.exe .
  go vet -mod=vendor . ./config/... ./logic/handler/...
  ```
- 说明:本批没有改 `go/proto`,不需要 `go mod vendor`。vendor 里已有 `proto/chat` 与 `shared/generated/pb/table`(`robot/vendor/modules.txt:181`、`:192`),其中含 `CommonError_kMessageSizeExceeded`。
  - **例外**:同日并行的 guild 批次改了 proto,而 vendor 里没有 `proto/guild`。若编译报 `proto/guild` 找不到,先执行 `go mod vendor` 再编,并在报告里注明原因是 guild 批次。
- 通过标准:退出码 0;新增文件(`chat_smoke_scenario.go`、`config/config.go`、`main.go`、`logic/handler/client_player_chat_*.go`)没有 vet 警告。
- 可选负例:复制 `etc/chat_smoke.yaml` 到临时目录,把 `zone_b` 改成 1 后运行,应立即非 0 退出,日志含 `chat_smoke: zone_a and zone_b must differ`。

### ⑥ 起双 zone 栈(路由服模式)

**前置**:

1. 本地基础设施已起(Docker Desktop、etcd / Redis / Kafka / MySQL、Java 网关 `127.0.0.1:8081`,见本地开机 runbook)。
2. `redis-cluster` profile 在跑:chat.yaml 的 ChatRedis 指向 7000-7005。
   - 不起集群也行:把 `go/chat/etc/chat.yaml` 的 ChatRedis 段整段删掉,回落共享库,启动会打 WARN。
3. `zone_config` 插入 zone 2 行(没有这一行,B 永远分不到 zone 2 的 gate):
   ```powershell
   "INSERT IGNORE INTO zone_config (zone_id,name,manual_status,capacity,recommended,sort_order) VALUES (2,'zone-2',0,5000,0,2);" |
     docker exec -i mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" mmorpg'
   ```
   表结构见 `deploy/mysql-init/gateway_tables.sql:6-14`、`:38-39`。若网关已在跑且 server-list 仍只有 zone 1,重启网关。
4. 已在跑的 gate 不会换模式:它启动时读一次 env 并缓存。先执行 `dev_tools.ps1 -Command dev-stop`。

**起栈**(同一个 PowerShell 会话里,用 `&` 调用,不要用 `pwsh -File`,否则数组参数会被压成单个字符串):

```powershell
$env:GATE_CLIENT_RPC_ROUTER = '1'
& .\tools\scripts\dev_tools.ps1 -Command dev-start-zones -Zones 1,2
```

- 已知坑(2026-09-02 实测):
  - redis-cluster 在跑时,zone 2 的 db 按默认位移到 7000 会撞集群端口。改传 `-ZonePortShift 2000`。
  - zone 2 的 gate / scene 可能卡在预设端口重试死循环。此时按 `$env:RPC_PORT` 逐个显式起。
- 若 `go_services.ps1 -Command status` 里没有 chat / z2_chat,补起:`& .\tools\scripts\go_services.ps1 -Command start-exe -Services chat -Zone <1|2>`。
- 只起单 zone 的替代方案:`启动服务器.cmd`(默认 `-GateRouterMode 1`)。它只起 zone 1(`start_game.ps1:405-413`),只能配 `chat_smoke.cross_zone: false` 做弱验收,不能替代本步。

**核对**:

- 两个 zone 的 gate 启动日志(`bin/log/` 下)都含 `gate 客户端消息出口模式=router`。
- `& .\tools\scripts\go_services.ps1 -Command status`:client_rpc_router / z2_client_rpc_router / chat / z2_chat 均为 RUNNING;chat 日志(`run/logs/go_services/chat*.log`)含 `CHAT SERVICE STARTED SUCCESSFULLY`,node_id 非 0。
- `docker exec etcd etcdctl get --prefix ChatNodeService.rpc/ --keys-only`:能看到 `ChatNodeService.rpc/zone/1/node_type/9/node_id/<n>` 和 `.../zone/2/...` 两把服务键,以及对应的 `ChatNodeService.rpc/allocated/node_type/9/node_id/<n>`。
- `docker exec etcd etcdctl get --prefix chat.rpc --keys-only`:**输出为空**(D-13)。
- `curl.exe -s -o NUL -w "%{http_code}" http://127.0.0.1:9210/metrics` 返回 `200`。

### ⑦ 跨 zone 冒烟

- 目录:`robot`
- 命令:
  ```powershell
  .\robot.exe -c etc/chat_smoke.yaml 2>&1 | Tee-Object logs\chat_smoke_run1.log
  ```
- 通过标准:
  - 退出码 0。
  - 日志恰有一行 `CHAT_SMOKE_OK player_a=... player_b=... gate_a=... gate_b=...`,且 `gate_a != gate_b`。
  - `[chat-smoke]` 各步日志按序出现。
- 旁证(记录即可):
  - `curl.exe -s http://127.0.0.1:9200/metrics | Select-String client_rpc_router_forward_total`:SendChat / PullChatHistory 的 `outcome="ok"` 计数增长,没有 `no_target`。
  - `curl.exe -s http://127.0.0.1:9210/metrics | Select-String chat_send_total`:有 `channel="world",outcome="ok"`、`outcome="too_long"`、`outcome="duplicate"`。
- 可重复性:立刻再跑一遍(输出到 `chat_smoke_run2.log`),仍应退出码 0。每轮用新的 nonce 与 request_id,单轮约 15-30s。

### ⑧ 容错:杀掉一个 chat 实例再跑

- 按端口精确杀 zone 1 的 chat。**不要用** `go_services.ps1 stop`,它会连同所有 zone 的 chat 一起停:
  ```powershell
  $p = (Get-NetTCPConnection -LocalPort 50700 -State Listen).OwningProcess
  Stop-Process -Id $p -Force
  ```
- 设计内窗口:这是崩溃式退出,etcd 最长保留死实例的键 LeaseTTL = 60s,期间路由服仍可能选中它(`PickRandom` 不看连接状态)。此时跑冒烟若失败(路由服 outcome 为 `dial_error` / `upstream_error`),**记录即可,不算失败**。
- 等待 ≥70s 后:
  - `docker exec etcd etcdctl get --prefix ChatNodeService.rpc/zone --keys-only` 只剩存活实例(zone 2)的键。
  - 执行 `.\robot.exe -c etc/chat_smoke.yaml 2>&1 | Tee-Object logs\chat_smoke_failover.log`。
- 通过标准:退出码 0,输出 `CHAT_SMOKE_OK`。历史存在 Redis,不受实例重启影响。

### ⑨ 失败时保留的现场

- `run/logs/go_services/chat*.log`、`z2_chat*.log`、`client_rpc_router*.log`、`z2_client_rpc_router*.log`。
- 两个 zone 的 gate 日志(`bin/log/`),以及同一时间段的路由服与 chat 指标快照(`:9200/metrics`、`:9210/metrics`)。
- `robot/logs/chat_smoke_*.log`,尤其是 `CHAT_SMOKE_FAIL step=/reason=` 行,以及 `[chat-smoke] gate envelope error` / `gate tip` 行。
- `docker exec etcd etcdctl get --prefix ChatNodeService.rpc/ --keys-only` 与 `... get --prefix ClientRpcRouterNodeService.rpc/ --keys-only` 的输出。
- ①-③ 失败时保留完整 `-v` 输出(`chat_test.log` 等)。

### 附加(可选,不阻塞 ①-⑧)

- **⑩ 脚本解析体检**(仓库根):
  ```powershell
  pwsh -NoProfile -Command "foreach(`$f in 'tools/scripts/go_services.ps1','tools/scripts/go_svc_image.ps1','tools/scripts/k8s_deploy.ps1','tools/scripts/start_game.ps1'){ `$e=`$null; [void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path `$f).Path,[ref]`$null,[ref]`$e); if(`$e){ `$f; `$e; exit 1 } }; 'PARSE_OK'"
  ```
  期望输出 `PARSE_OK`。
- **⑪ K8s dry-run**:
  - 命令:`pwsh -NoProfile -File tools/scripts/k8s_deploy.ps1 -Command infra-up -DryRun -SkipPreflight -GoSvcRegistry ghcr.io/verify -GoSvcTag t1 -NodeImage ghcr.io/verify/node:t1 *> run/verify-chat-infra-dryrun.log`
  - 期望 `go-svc-chat-config` 段中:
    - Etcd 下有 `Hosts` 和 `Key: ""`;
    - 含 `- /chatpb.ClientPlayerChat/SendChat`;
    - ChatRedis 为六个 `redis-match-cluster-N` 地址且 `Type: cluster`;
    - `MetricsListenAddr: ":9210"`。
  - chat Deployment 含 `POD_IP`;client-rpc-router 的 ConfigMap、Deployment(含 `POD_IP`)与 PDB 都已生成,日志里不再出现 `skipping client-rpc-router`。
  - zone-up 另跑三次:`-GateRouterMode 0` 时 gate env 值为 `"0"`,`1` 时为 `"1"`,`2` 时参数绑定失败。
- **⑫ killswitch 活体关停**(⑦ 通过后):
  1. `docker exec etcd etcdctl put /mmorpg/killswitch/chatpb.ClientPlayerChat/SendChat true`(值 `true` 即 deny,见 `go/shared/killswitch/rule.go:62-63`)。
  2. 跑冒烟,期望 `CHAT_SMOKE_FAIL`,且失败步是 `world-send` 或 `world-send-tip`,不是 `login`;chat 指标 `killswitch_blocked_total` 增长。
  3. `docker exec etcd etcdctl del /mmorpg/killswitch/chatpb.ClientPlayerChat/SendChat`,再跑冒烟应恢复 `CHAT_SMOKE_OK`。

---

## §13 明确不做 / 待拍板

### 13.1 本批明确不做

- **不改**:proto(28 / 61 已烧进 gate 与路由表)、`node_util.cpp`、gate C++、C++ 路由模式默认值、既有 8 份注册实现。
- **chat v1 不做**:推送(§5)、MySQL、发号器、Kafka 消费、merge 围栏、TEAM / SYSTEM 频道、敏感词过滤(见 [chat-sensitive-word-filter.md](./chat-sensitive-word-filter.md),v1 未接)、`since` 增量拉取。
- **不写** Unity 聊天 UI(客户端任务未授权,由 robot 代替验收)。
- **K8s 路由模式不翻**:`-GateRouterMode` 默认 `"0"`。

### 13.2 待拍板

| 项 | 为什么现在不拍 | 触发点 |
|---|---|---|
| ~~D4 全局库归属 + 迁移器~~ **已拍:port-decisions D-14** | chat 零 MySQL | 首个建表的全局服务同批落 `go/schemamigrate` 与 migrate Job |
| killswitch 是否加 zone 维度 | 全局服务本就看不到 zone(§0),需求未出现 | 出现「只关某个 zone 的某个方法」的运维需求时 |
| 推送缓冲(#286 / `shared/pushbuffer`) | chat v1 不推送 | chat v1.1 私聊单播立项时 |
| K8s 路由模式默认翻转时间 | 隔离K8s的router→chat已实跑通过(§16)，含gate的battle-smoke尚未验证 | K8s 上以路由模式跑通过一次 battle-smoke 之后 |
| C++ → Go 东西向调用的身份 | chat v1 没有这条边(§8) | 首个 scene → 全局 Go 服务的调用出现时 |
| D34 何时删直连旧路径 | 仍有依赖直连的环境(K8s) | K8s 翻转且稳定之后 |
| chat 私有 tip 段 | Tip.xlsx 未开 chat 段 | 导表器开段后,把 common 码替换成 chat 段码 |
| MessageLimiter 给 28 / 61 配档位 | 表未改;现在 gate 默认 3 次 / 窗口比 chat 的 5 次 / 秒更严 | 客户端聊天 UI 接入前 |
| 路由服本地 yaml 的 `Etcd.Key` | 本地仍是 `client_rpc_router.rpc`,K8s 已留空 | 路由服迁 shared 版 noderegistry 时 |

---

## §14 已知缺口与实现偏离(需架构确认)

### 14.1 缺口

1. ~~路由服 K8s manifest 不在仓库里~~ **已补(2026-09-14 复核)**:`deploy/k8s/manifests/go-svc/client-rpc-router.yaml`。2026-09-15已在本地kind隔离环境实跑router→chat(§16)；`-GateRouterMode` 默认仍为 `"0"`，默认部署下玩家仍不可达，翻转门禁仍是含gate的battle-smoke。
2. ~~路由服在 K8s 上会通告 `0.0.0.0`~~ **已修(2026-09-14 复核)**:`client_rpc_router_service.go` 改为经 `advertisedHost` 优先 `POD_IP`(与 chat / data_service 同口径);本地 `ListenOn=127.0.0.1` 行为不变。Codex 已通过路由服 vet、单测和 build，并补充 Pod IP 优先及通配地址回退回归测试。
3. **gate MessageLimiter 表里没有 28 / 61**:默认档比 chat 自己的限速更严,真实客户端快速发言会先被 gate 拒。
4. **ChatRedis 与 match 共用 `redis-match-cluster`**(512mb,volatile-lru):聊天历史可能被提前淘汰,并与 match 争内存。chat 数据一旦成为唯一权威,必须换独立的 noeviction 实例。
5. **正常停机顺序已修复并活体验证（2026-09-15）**：chat持有真实gRPC Server，在注销尝试返回后主动排空，再关闭依赖和框架；Linux默认1s自动停止推迟到24s硬截止，Windows使用同一主动清理路径。真实Linux SIGTERM、慢注销超过1.5s、在途/永久阻塞请求及启动期信号均验证通过；最终K8s容器收到SIGTERM后exit=0，旧注册身份已清理并恢复Ready。硬截止/SIGKILL不保证在途业务完成，注销传输失败仍依赖租约TTL；详细边界见§16。
6. **go-zero 版本不一致**:shared 模块实际依赖 go-zero v1.9.2,chat 是 v1.10.0。chat 模块内经 MVS 统一到 v1.10.0;noderegistry 用到的 logx API 两个版本都有。
7. **`start_game.ps1` 的 chat / guild 均为可选服务**:缺 exe 时告警并跳过。Codex 实跑修复了警告内弯引号导致的 PowerShell 参数绑定失败，修复后缺 chat.exe 的 CheckOnly 已通过。
8. **`dev_tools.ps1` 的 `k8s-*` 包装不透传 `-GateRouterMode`**,只能直接调 `k8s_deploy.ps1`。
9. **服务清单文档未同步**:`tools/scripts/README.md`、`deploy/k8s/AGENTS.md`、`deploy/k8s/README.md` 不在本批范围。
10. **`OnNodeIDChanged` 回调里调 `Close()` 不会死锁**,但会等满 closeTimeout(5s)。

### 14.2 实现比契约更严或不同(理由都写在代码注释里)

- **noderegistry**:
  - 回调产物额外要求 `nodeUuid == 入参 uuid`、`nodeType == spec.NodeType`。
  - `Close` 删除带条件 `Value(allocKey)==NodeUUID`,防止失租后删掉别人的 key。
  - 重夺用嵌套 Txn:allocKey 不存在或值是自己的都算成功。
  - 分配时 Txn 出传输错误就直接返回,不像副本那样继续逐个试 id。
  - 重注册成功后 Revoke 旧租约。
  - 扫描已占 id 时两棵子树都算(见文首修订表)。
- **chat**:
  - 两态幂等键(§9.3)。
  - `request_id` ≤64 字节。
  - `Validate` 也拒 `Timeout=0`:0 = 不限时,同样违反超时预算。
  - `conf.MustLoad` 已经调用 `Validate`,所以校验失败的日志前缀是 go-zero 的 `error: config file <path>, ...`,不是自定义前缀。
- **robot**:用 `chatSmokeLogin` 代替 `prepareBehaviorClient`(§9.7)。若要集中修,应在 `login_test_scenarios.go` 的接收回调里加入聊天消息号。
- **部署脚本**:
  - `-GateRouterMode` 是 `[string]` 类型,取 `'1'` / `'0'`。
  - ConfigMap 里 `HistoryDefaultLimit` / `HistoryMaxLimit` / `RequestIdTTLSeconds` 为可选读取,yaml 里没写就不生成对应行。
  - chat 的契约值只在生成 chat 的 ConfigMap 时求值,避免 chat.yaml 缺失时拖垮所有 Go 服务的 ConfigMap 生成。

## §15 2026-09-14 Codex 验证记录

- 使用本机已有 Go 1.26.5（模块缓存内工具链），CGO_ENABLED=0；未安装工具。详细日志：`run/verify-chat-20260914/`。
- `shared/noderegistry`：gofmt、vet、13 个单元测试通过；真实本地 etcd 的 13 个集成测试通过，没有 SKIP。覆盖并发分配、双 key 同租约、失租重夺/换号/退出和条件清理。
- `chat`：`go mod tidy`、vet、20 个顶层测试通过（入口 7 + logic 13）。两个新增请求过期交错用例先红后绿，证明旧请求不能释放成功重试或提前完结新 pending。保持原 Redis key 和跨频道 request_id 语义；token 不把跨 slot 写历史与 claim 更新变成原子事务，迟到写/提交结果不确定时仍可能偶发重复。
- 修正 `.gitignore` 的旧 `go/chat/` 规则，聊天源码已可纳入版本控制，仅忽略重复生成编号目录。
- 路由服：vet、全包测试和 build 通过；新增 Pod IP/具体地址/通配监听的注册回归测试通过。match build 通过。
- robot：首次 vendor 构建因并行 guild 批次缺少 `proto/guild` 失败；该批次完成 vendor 同步后，重新构建和定向 vet 通过。本批独立冒烟程序为 `run/verify-chat-20260914/robot.exe`，从 `robot/` 工作目录运行。
- 项目脚本已构建 `bin/go_services/chat.exe`；两个实际实例 z1_chat:50700 / z2_chat:52700 启动成功（ZonePortShift=2000）。etcd 有两个服务 key 及两个分配 key，无 `chat.rpc` 额外注册；指标 9210/11210 均 HTTP 200。
- 4 个部署/启动脚本解析通过；infra-up 和 zone-up 的模式 0/1 DryRun 通过、模式 2 按预期拒绝，渲染 YAML 结构检查通过。未 apply 到 K8s；K8s 默认仍为直连模式 0。
- 启动器缺 chat.exe 的 CheckOnly 首次失败于中文弯引号；改成「服务不可用」后复验 exit=0，保留首败与复验日志。
- 经 gate 的双 zone chat-smoke 已连续两次通过（chat-smoke-run2.log / run3.log，均 exit=0、各一行 CHAT_SMOKE_OK）。A/B 分别为玩家603/702，gate 127.0.0.1:10000 / 127.0.0.1:11010，两区 gate 均为 router；世界/私聊可见、sender覆盖、600字节拒绝、同request_id只存一条均通过。首轮 run1 在二区登录预加载阶段失败，原因是 zone_2_db.player_database 缺 pet/bag/mission 三列；并行帮会任务按正式迁移补齐并核验后才重跑，首败日志保留。
- Linux/amd64 交叉编译通过；机器人配置负例（cross_zone=true 且 zone_a=zone_b）按预期在连接前 exit=1，错误包含 zone_a and zone_b must differ。
- 单实例故障切换通过：只终止本任务创建的 z1_chat，等待超过70秒，核验 etcd 服务记录仅剩 zone2/node_id2，然后原始 chat-smoke 再次 exit=0 / CHAT_SMOKE_OK（chat-smoke-failover.log）。测试后已恢复 z1_chat；两份注册和 metrics9210/11210 HTTP200 均复核正常。
- 验收结论：本地跨区聊天与故障切换已通过；K8s仅做渲染检查和Linux构建，不等于集群实跑；Linux停机顺序与Redis尽力幂等限制仍见§14.1/§9.3。

## §16 2026-09-15 停机修复与隔离K8s补验

- **代码范围**：`go/chat/chat.go`、`internal/lifecycle/`、`internal/svc/servicecontext.go`及生命周期/metrics回归测试。保留zrpc和既有中间件，不修改协议、依赖版本或聊天存储语义。官方`chat.go`单文件构建入口通过；已用`go_services.ps1 -Command build -Services chat`更新本地`bin/go_services/chat.exe`，日志与SHA256保存在本次K8s验证目录。设计与命令证据见`run/verify-chat-20260914/chat-lifecycle-verification.md`。
- **停机机制**：正常预算内先注销尝试，再5s排空RPC，关闭metrics/killswitch/etcd，最后2s框架收尾和日志。忽略取消的handler可能使GracefulStop/Stop阻塞，因此强制Stop与框架收尾异步发起并有限等待，超时明确报错。24s总硬截止兼容K8s30s宽限中5s preStop；启动期信号通过proc.Done桥接，晚到Server通过ServerSlot交接，避免继续注册或漏关监听。
- **回归结果**：Windows全包27个顶层PASS（含1个子进程辅助入口）、1个Linux专属SKIP、0FAIL；vet与单文件build通过。Linux隔离Alpine容器内7个顶层PASS（含1辅助入口），真实SIGTERM覆盖慢注销超过1.5s仍能接RPC、在途请求完成、永久阻塞handler有限退出、并发Stop、启动前信号和Server交接。metrics关闭后同端口可重新监听。两项`go -overlay`故障注入分别恢复默认1s预算、移除启动期取消防护，均按预期失败；它们不是完整旧版本重建。
- **K8s环境**：本地`kind-mmorpg`（Kubernetes v1.37.0），新建`chat-verify-20260915`；全新emptyDir etcd、node Redis、三主Redis Cluster（16384 slots、noeviction）及专用probe Pod。使用官方`go_svc_image.ps1`/`Dockerfile.go-svc`构建镜像，官方配置函数生成CM，原chat/router Deployment保留双副本、POD_IP、gRPC探针、PDB与指标。测试夹具只替换命名空间、缓存镜像和三主seeds。未使用现存游戏数据；三主Redis仅验证Cluster命令兼容，不代表存储HA验收。
- **初轮链路**：`probe-full.log` exit=0 / `K8S_CHAT_PROBE_OK`，123断言。不同来源zone 101/202经两个router到chat，验证WORLD、双向PRIVATE、session发送者覆盖、服务端时间、600字节拒绝、缺会话拒绝、即时幂等；逐个router与chat读回同一历史。
- **发现/指标**：`discovery-metrics-report.json`的23项断言通过。chat/router各2条NodeInfo，两种endpoint IP均等于对应PodIP，无额外`chat.rpc`/`client_rpc_router.rpc`键。四个实例metrics均HTTP200（各服务Pod本机wget），两个PDB均minAvailable=1/disruptionsAllowed=1；服务日志未出现测试聊天nonce。
- **单幸存者故障切换**：先孤立本次chat Deployment/ReplicaSet以阻止自动补副本，再强制删除一个已按UID核验的chat Pod。运行时已无该task；132s后etcd只剩原幸存Pod，UID不变、restartCount=0。`probe-failover.log` exit=0，145断言；先只读故障前历史，再用新nonce验证新WORLD/PRIVATE及即时重试。没有把已经超过60s幂等TTL的旧request_id当成去重保证。
- **最终版本复验**：聊天最终镜像`local/mmorpg-chat:chat-verify-20260915-final`（manifest index `sha256:58102cf2d455450f4923b3b552e3eaf64f11c96d2512a182c3176ad273551b48`）；路由服`local/mmorpg-client-rpc-router:chat-verify-20260915`。恢复最终双副本并正常关闭旧孤立Pod后，向最终容器发送真实SIGTERM：K8s记录exitCode=0/Completed，日志先收到信号再完成registry.Close，旧nodeUuid消失，容器恢复Ready。随后`probe-final.log`再次exit=0、123断言；源码hash仍与测试版本一致。
- **清理与复现**：隔离namespace已删除并等待确认，原`mmorpg-infra`/`mmorpg-zone-yesterday`仍在。夹具、探针、镜像构建日志、完整结果与清理证据位于`run/verify-chat-k8s-20260915/`，入口`prepare-fixture.ps1`、`fixture-usage.txt`、`verification-summary.json`。Docker首次启动的遗留socket问题仅按现有流程备份通信目录恢复，未重置数据卷。
- **验收边界**：此次K8s链路为probe→router→chat→Redis，不包含C++ gate/login/battle；含gate的本地双zone chat-smoke证据见§15。K8s的GateRouterMode默认翻转仍须另跑battle-smoke。24s硬截止本身未主动触发，Windows用stdin取消模拟退出context而非真实控制台Ctrl+C；未运行race detector。etcd删除失败仍靠租约清理，跨Redis slot的历史写入/claim提交仍是既有尽力幂等语义。

## §17 2026-09-16 完整K8s Battle验收计划

- 继续项：在本机kind新建`chat-full-verify-20260916`隔离命名空间，运行真实gateway→gate→login/scene/match→battle直连的原始robot battle-smoke；必须得到`BATTLE_SMOKE_OK`及玩家/观战直连回合断言，不用gRPC探针替代TCP客户端。
- 当前阻塞：已有`local/mmorpg-node:410b5283d822-dirty`实际指向Alpine占位镜像，没有C++程序；Linux构建清单、镜像/runtime staging及K8s装配均遗漏battle。以`.vcxproj`为源补齐生成器/构建入口/镜像产物，CMake仅在隔离构建环境自动生成。
- 部署范围：battle是全局池，只在infra装配；保留GateRouterMode默认值。Kafka命令topic代数与分区数从现有权威配置读取；全新DB使用正式migrate入口建表。测试数据全部使用本次emptyDir存储，不清理或复用现有游戏数据库。
- 构建资源：Docker有32 CPU、约15.2GiB内存，给C++构建增加可配置并行度，验证时限制并发，避免按CPU数启动32个高内存编译器。gRPC/protobuf使用仓库已固定的v1.83.0/v35.1，保持源码版本一致。
- 验证与清理：先检查构建/部署契约，再导入本地镜像、启动独立基础设施并迁移、运行原robot；保留首败及修复复验日志、源码/镜像标识、注册/直连证据。完成后仅删除本次命名空间。本节当前为实施计划，实际结果另追加，不视为已通过。