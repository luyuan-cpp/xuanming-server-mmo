# RPC 热关停：节点 / 服务 / 方法三级（C++ 与 Go 同一套）

> 状态：2026-11-12 落码，**未编译，待 Codex 验证**（命令见 §12）。Java 版待做（§13）。
> 相关：Go 包 [`go/shared/killswitch`](../../go/shared/killswitch/killswitch.go)、C++ 模块 [`cpp/libs/engine/core/kill_switch/`](../../cpp/libs/engine/core/kill_switch/kill_switch.h)、
> 断线重连核对 [infra-reconnect-overview.md](./infra-reconnect-overview.md) §4 R12（新鲜度只在真正读到 etcd 时刷新）。

## 0. 结论速览

- **事实源只有一个**：etcd 前缀 `/mmorpg/killswitch/`。运维经 gateway_node 管理接口 `/admin/killswitch` 写（`X-Admin-Key` 鉴权、审计日志、键格式校验），`etcdctl` 是破窗手段。
- **每个进程自己 watch、自己判定**：C++ 节点（gate / scene / battle）与 Go 服务都在本地派发入口拦截。节点之间不互相通知，任何业务服务都不参与下发。
- **三级**：节点（`type/<节点类型>/*` 整类、`instance/<节点 uuid>/*` 单个进程）、服务（`<服务>/*`）、方法（`<服务>/<方法>`）。范围与目标可以任意组合。
- **被关停的请求在业务执行之前快速失败**：客户端看到带请求 id 的信封 1003（`kServiceUnavailable`，与 Java 版同口径）；节点间 RPC0 回 `KILL_SWITCH_DENIED`，调用方按「确定未执行」处理；gRPC 回规则里的状态码（缺省 UNAVAILABLE）。
- **fail-open**：没有规则、规则写坏、与 etcd 失联超过 60 秒，一律放行。它是止血阀，不是权限、资格或活动开放的判定。
- **只挡新工作，不挡收尾**：连接握手、停机、会话收尾（玩家下线、交接释放、离开游戏、退订观战）、资源收尾（拆场景、拆战斗）、在途事务的补偿与回执是「受保护的方法」，通配规则（`*`、`服务/*`）关不掉它们，只有精确到方法的规则能关。关停期间链路不断，玩家照样能下线并存盘，冻结与预留照样能退回。

## 1. 要解决什么

线上出事时要秒级止血：某个方法把 DB 打爆、某个服务在放大故障、某个节点进程状态异常。改代码、发版、滚动更新都太慢。

现状（2026-11-12 核对）：

- Go 早有 `go/shared/killswitch`（方法 / 服务 / 全局三级，etcd 驱动，fail-open），11 个服务挂在拦截器链上；但没有「只关某一类节点」「只关某一个进程」，`client_rpc_router` 与 `match`（含 TeamNodeService）没挂。
- C++（gate / scene / battle）完全没有，RPC0、muduo gRPC 服务端、gate 客户端入口、scene 玩家方法、battle 直连都关不了。
- Java 版有方法 / 服务 / 全局（Redis 哈希 + 管理接口），没有节点级。

本设计把 C++ 与 Go 统一成一套：同一前缀、同一键格式、同一值格式、同一匹配顺序；补上节点级；补上没挂的服务。

## 2. go-zero 有没有

核对本仓依赖的 go-zero v1.10.0 源码（`zrpc/internal/serverinterceptors/`、`zrpc/config.go`、`zrpc/internal/config.go`）：

| go-zero 自带 | 作用 | 触发方式 |
|---|---|---|
| Breaker 中间件 | 按方法（`info.FullMethod`）的自适应熔断（Google SRE 算法） | 错误率高时**自动**拒绝 |
| Shedding（`CpuThreshold`，缺省 900） | 自适应降载 | CPU 超阈值时**自动**拒绝 |
| Timeout / `MethodTimeouts` | 按方法的超时 | 自动 |
| Auth | app / token 鉴权 | 配置 |
| Stat / Prometheus / Trace / Recover | 观测与兜 panic | — |

结论：go-zero **没有**「运维按名字立即关掉某个方法 / 服务 / 节点」的开关。本仓 Go 侧的人工关停是自研的 `shared/killswitch`，不是 go-zero 自带。两者互补：熔断、降载兜**自动**过载；热关停给**人**用来止血。C++ 侧对应的自动保护是 gRPC 服务端的在途上限（RESOURCE_EXHAUSTED）、gate 的限流与路由舱壁。

## 3. 谁来通知：控制面

| 角色 | 是谁 | 说明 |
|---|---|---|
| 决策 | 运维值班 / 事故指挥 | 按告警与手册决定关什么、何时恢复；不是任何业务服务 |
| 入口 | gateway_node `GET/PUT/DELETE /admin/killswitch` | 已有 `AdminApiKeyFilter`（`X-Admin-Key`）鉴权；写操作打审计日志；拒绝格式不对的键 |
| 破窗 | `etcdctl put / del` | 管理接口不可用时直接写 etcd，格式见 §4 |
| 事实源 | etcd `/mmorpg/killswitch/` | 唯一事实源；不进 Redis、不进配置文件、不烘进镜像 |
| 下发 | 每个进程自己 watch | C++ 节点用一条独立的 etcd 连接（§9）；Go 服务用各自的 etcd 客户端 |
| 生效 | 进程本地的派发入口 | 不依赖任何别的服务 |

这是配置中心 + 各实例订阅 + 本地拦截的标准形态（Dubbo 服务治理的禁用 / 降级规则、Sentinel、Envoy runtime 同一结构）。理由：

1. **止血阀不能依赖被止血的系统**。如果让 scene_manager、login 之类的业务服务去「通知」各节点，出事的恰好是它们时就没人能关了。
2. **水平扩展**。写一次 etcd，所有实例经 watch 收到；实例数增长只多 watch 连接，写入方的工作不变；新实例启动先拉全量快照，不会漏规则。
3. **没有单点**。etcd 本身多副本；管理接口不可用还有 etcdctl。
4. **可审计**。写入只有一个入口，谁在什么时候关了什么都在审计日志里；各进程的指标（§11）反映「规则在我这里生没生效」。
5. **连接层不做业务**。节点之间不广播、不做重连补发（与「重连 / 连上某类节点不做业务逻辑」的定论一致）。

## 4. 规则键与值

```
/mmorpg/killswitch/<目标>                              全部进程
/mmorpg/killswitch/type/<节点类型>/<目标>               这一类节点的全部实例
/mmorpg/killswitch/instance/<节点 uuid>/<目标>          这一个节点实例（进程）

<目标>      := <包.服务>/<方法> | <服务>/<方法> | <包.服务>/* | <服务>/* | *
<节点类型>  := eNodeType 枚举名（proto/common/base/node.proto），与发现键 "<名>.rpc/..." 同名，
              如 GateNodeService、SceneNodeService、BattleNodeService、LoginNodeService、ChatNodeService
<节点 uuid> := NodeInfo.node_uuid：发现键值里的 nodeUuid，也打在节点启动日志里；进程每次启动新生成
```

- 第一段的 `type`、`instance` 是保留字（proto 服务名是大驼峰，不会冲突）。范围段残缺的键（`type/`、`type/X`、`instance//*`）整条忽略。
- **实例范围用 uuid，不用 node_id**：node_id 在进程重启后可能被另一个健康的进程拿到，按 node_id 写的规则会落到它身上；uuid 只指这一个进程，进程退出后规则自然失效（成为孤儿键，管理接口列表里标出，可以删）。
- 服务名：gRPC 服务带包名（`chatpb.ClientPlayerChat`），全名与短名都认；C++ 节点间 RPC0 服务没有包名（`Scene`、`Gate`），客户端协议服务按 proto 里的服务名（`SceneSkillClientPlayer`、`ClientPlayerChat`）。
- 值（与现有 Go / Java 完全相同）：
  - JSON：`{"deny":true,"reason":"db overload","code":9}`。`deny` 缺省 false；`reason` 回给调用方并进日志；`code` 是 gRPC 状态码，0 或超出 1..16 一律按 UNAVAILABLE（14）。字段类型不对（如 `"deny":"yes"`、`"code":-1`）算写坏。
  - 裸值：`true / 1 / on / yes / deny` = 关停，`false / 0 / off / no / allow` = 放行（豁免），大小写不敏感。
  - 空值 = 放行（常见于先建键占位）；写坏的值整条忽略并打一条日志（fail-open）。

## 5. 匹配顺序

一个进程只认三种范围：**本进程的实例、本进程的节点类型、全部**。按下面的顺序找，**第一条存在的规则说了算**：`deny=true` 关停，`deny=false` 放行。

1. `instance/<本进程 uuid>/` 下：`包.服务/方法` → `服务/方法` → `包.服务/*` → `服务/*` → `*`
2. `type/<本进程类型>/` 下：同上五个
3. 全部（无范围前缀）：同上五个

范围上「实例 > 类型 > 全部」，同一范围里「方法 > 服务 > 全部方法」，所以更具体的规则可以用 `deny=false` 豁免更宽的规则。一个进程以多种节点类型注册时（match 同时是 MatchNodeService 与 TeamNodeService），每种类型、每个 uuid 都算本进程的范围；这些范围之间写了相反的规则时取关停。受保护的方法（§8）每个范围只看前两项（精确到方法的规则）。

| 已有规则 | 请求 | 结果 |
|---|---|---|
| `type/SceneNodeService/*` = true | 任一 scene 上的普通方法 | 关停 |
| `type/SceneNodeService/*` = true，`instance/<A>/*` = false | scene A 上的任何方法 | 放行（实例豁免） |
| `SceneSkillClientPlayer/*` = true，`SceneSkillClientPlayer/ListSkills` = false | ListSkills | 放行（方法豁免） |
| `instance/<A>/*` = true，`SceneSkillClientPlayer/ReleaseSkill` = false | scene A 上的 ReleaseSkill | 关停（实例范围先于全部） |
| `type/GateNodeService/ClientPlayerChat/SendChat` = true | 经 gate 发的 SendChat | gate 入口关停 |
| `type/SceneNodeService/*` = true | scene 上的 ExitGame / ReleasePlayer | 放行（受保护，通配不覆盖） |
| `type/SceneNodeService/SceneNodeGrpc/AssetAbortDebit` = true | scene 上的 AssetAbortDebit | 关停（精确规则能关受保护的方法） |

## 6. 三级怎么写（运维速查）

| 要关的 | 键（省略前缀 `/mmorpg/killswitch/`） |
|---|---|
| 一类节点整体 | `type/SceneNodeService/*` |
| 一个节点进程 | `instance/<uuid>/*` |
| 所有进程上的一个服务 | `ClientPlayerFriend/*` |
| 一类节点上的一个服务 | `type/SceneNodeService/SceneBagClientPlayer/*` |
| 所有进程上的一个方法 | `ClientPlayerChat/SendChat` |
| 一个进程上的一个方法 | `instance/<uuid>/SceneSkillClientPlayer/ReleaseSkill` |

管理接口（推荐）：

```bash
curl -s -H "X-Admin-Key: $KEY" http://<gateway>/admin/killswitch
curl -s -X PUT -H "X-Admin-Key: $KEY" -H "Content-Type: application/json" http://<gateway>/admin/killswitch \
  -d '{"node_type":"SceneNodeService","target":"SceneSkillClientPlayer/ReleaseSkill","deny":true,"reason":"skill exploit"}'
curl -s -X DELETE -H "X-Admin-Key: $KEY" "http://<gateway>/admin/killswitch?key=type/SceneNodeService/SceneSkillClientPlayer/ReleaseSkill"
```

`PUT` 的请求体：`target` 必填；`node_type` 与 `instance` 至多给一个（都不给 = 全部进程）；`deny` 必填；`reason`、`code` 可选。`GET` 列出每条规则的范围、目标、是否有效，实例规则另标「是否指向在线节点」。

破窗（etcdctl；Git Bash 下以 `/` 开头的参数要加 `MSYS_NO_PATHCONV=1`）：

```bash
etcdctl put /mmorpg/killswitch/type/SceneNodeService/SceneSkillClientPlayer/ReleaseSkill '{"deny":true,"reason":"skill exploit"}'
etcdctl get --prefix /mmorpg/killswitch/
etcdctl del /mmorpg/killswitch/type/SceneNodeService/SceneSkillClientPlayer/ReleaseSkill
```

## 7. 判定点：谁在拦、回什么

规则范围看的是**处理这个请求的进程**自己的身份：gate 只按 gate 的类型与 uuid 判，scene 只按 scene 的。

| 进程 | 入口 | 判的方法名 | 被关停时 |
|---|---|---|---|
| gate | 客户端上行 `DispatchClientRpcMessage`：GM 闸之后、本地观战与转发之前 | 客户端方法，如 `ClientPlayerChat/SendChat`（gRPC 方法带包名全名） | 回带请求 id 的信封 1003；不计非法包、不断线；限流额度照扣 |
| gate / scene / battle | RPC0 服务端派发 `RpcChannel::CallServiceMethod`：方法解析之后、请求体解析之前 | RPC0 方法，如 `Gate/SendMessageToPlayer` | `RPC_ERROR KILL_SWITCH_DENIED`；调用方 `RpcStatus` 为 `kRemoteRejected`（确定未执行） |
| scene | 玩家方法派发 `ProcessClientPlayerMessage` 内层：与退出闸、GM 闸并列 | 客户端方法真实名，如 `SceneSkillClientPlayer/ReleaseSkill` | 应答的 `error_message` 写 1003；应答是 Empty 的单向方法不回包 |
| scene / battle | muduo gRPC 服务端：收到请求头时（`RouteRequest`） | gRPC 全名，如 `/BattleNode/CreateBattle` | 规则的状态码（缺省 UNAVAILABLE），文案 `killswitch: method <m> is disabled[, reason: ...]`；请求体不收、handler 不执行 |
| battle | 客户端直连：四个白名单方法 | `BattleClientPlayer/<方法>` | 信封 1003；不计非法包 |
| Go 各服务 | gRPC 一元拦截器（链的外层） | gRPC 全名 | 规则的状态码（缺省 UNAVAILABLE） |
| Go 路由服 client_rpc_router | `Forward` 逻辑：查到路由表之后、选实例之前 | 转发的**目标方法**（如 `/chatpb.ClientPlayerChat/SendChat`） | 信封 1003，指标 outcome=`killswitch`；本跳的 `Forward` 是转发信封、受保护 |

- 全部范围的规则对客户端方法会在 gate 先拦住：省一跳，也保护已经过载的下游（与 Java 版 gate 同口径）。节点范围的规则由处理请求的进程自己拦，gate 不按下游节点的身份判。
- `Scene/ProcessClientPlayerMessage` 是 gate→scene 的转发信封，RPC0 层不判它，由 scene 内层按真实方法判（§8）。
- gRPC 的 UNAVAILABLE 在 muduo 服务端的契约里属于「业务可能已执行」那一档：调用方会保守处理（不假定没执行），不会出错；要让调用方不重试，规则里写 `"code":9`（FAILED_PRECONDITION）。

## 8. 受保护的方法：只挡新工作，不挡收尾

热关停的目的是让**新工作**停下来。在途的工作必须收得了尾：玩家能下线存盘、交接能释放旧节点、冻结能解开、资产预留能退回、节点能被优雅停机、链路不断。所以下面这些方法是「受保护的方法」：

- **通配规则（`*`、`服务/*`）关不掉它们**，不论范围是全部、类型还是实例；
- **精确到方法的规则仍然关得掉**（如 `type/SceneNodeService/SceneNodeGrpc/AssetAbortDebit`）：补偿调用本身在打爆下游时，运维还有手段止血。

| 类别 | 方法 | 挡住会怎样 |
|---|---|---|
| 连接握手 | `Gate/NodeHandshake`、`Scene/NodeHandshake` | 节点间链路建不起来，解除关停后还要重连 |
| 停机 | `Gate/GmGracefulShutdown`、`SceneScene/GmGracefulShutdown` | 被整体关停的节点停不了机（§10 的处置是先止血、再停机） |
| 转发信封 | `Scene/ProcessClientPlayerMessage`（RPC0 层）、`client_rpc_router.ClientRpcRouter/Forward`（路由服本跳） | `*` 会连 ExitGame / Disconnect 一起挡掉；真实方法由 scene 内层、路由服按目标方法判 |
| 会话收尾 | `ScenePlayer/ExitGame`、`scene_node.SceneNodeGrpc/ReleasePlayer`、`ClientPlayerLogin/LeaveGame`、`BattleClientPlayer/StopWatchBattle`、`GateClientSpectate/UnsubscribeSpectate`、`BattleNode/RemoveObserver` | 玩家卡在旧节点上、下线存不了盘、重登被拒；观战推送停不下来 |
| 资源收尾 | `Scene/DestroyScene`、`scene_node.SceneNodeGrpc/DestroyScene`、`BattleNode/DestroyBattle` | 已结束的场景 / 房间一直占着节点 |
| 补偿与回执 | `Scene/CancelBattlePrepare`、`scene_node.SceneNodeGrpc/CancelBattlePrepare`、`scene_node.SceneNodeGrpc/AssetAbortDebit`、`scene_node.SceneNodeGrpc/AssetAckReceipt` | 参战冻结解不开、资产预留退不回、场景收据一直悬着 |
| Go：会话收尾 | `ClientPlayerLogin/Disconnect`、`ClientPlayerLogin/LeaveGame`（login 与路由服都按它保护）、`SceneManager/LeaveScene` | 登录会话不释放、重登被拒；gate 合成的断线通知（58 号）经路由服 Forward 到 login.Disconnect，路由服设计早已定为不受热关停约束（[client-access-band-routing.md](./client-access-band-routing.md) §13.3） |
| Go：补偿与回执 | `DataService/ReleasePlayerName`（建角回滚）、`DataService/AckRoleTradeDrain`、`PlayerInbox/AckItems`、match 的 `MatchInternal/{CancelActivityBattle, CommitBattleOutcome, ReleaseTrialOccupancy, GetBattleCompletion, GetBattleTerminalDecision}` | 建角失败后名字一直被占、交易排空证明提交不了、收件箱条目重复投递、参战冻结解不开、帮会历练占用放不掉（match 这五个与它排空时放行的收尾方法同一份） |

名单只有两处：C++ 在 `kill_switch.cpp` 的 `kProtectedMessageIds`（用消息号常量，方法改名或删除时编译失败），Go 在各服务装配的 `killswitch.Config.Protected`（用生成的 `*_FullMethodName` 常量）。新增一项必须写明它挡的是哪种收不了尾的情形。业务上的「取消」（撤单、退出帮会等）是玩家发起的新工作，不在名单里；需要时用 `deny=false` 的精确规则单独放行。

不受保护的方法照常关停：它们的调用方本来就必须处理「对端不可用」（节点随时可能崩溃或失联），关停只是让这种情况按运维的意图发生，不引入新的失败模式。

## 9. 失效模式与新鲜度（fail-open）

与 Go 包注释的铁律一致：

- 没配 etcd、etcd 连不上、前缀下没有键、值写坏、没有规则匹配 → 放行。
- 与 etcd 失联超过 `StaleAfter`（缺省 60 秒）→ 本地快照作废、整体放行；失联不到 60 秒保持原状，避免网络抖一下就把止血阀抖开。
- **新鲜度只在真正读到 etcd 时刷新**：全量 Range 成功、或 watch 收到事件。C++ 每 20 秒（`StaleAfter` 的三分之一）全量 Range 一次复核；watch 通道开着不代表还能读到 etcd（R12 教训）。全量 Range 同时是对账：漏掉的事件在下一次复核时补齐。
- C++ 用**一条独立的 etcd 连接**（与注册 / 发现分开，复用同一个传输实现 `etcd_transport::Create`）：热关停的 watch 出问题不影响节点注册与发现，反过来也一样；与 Go data_service / db 为 killswitch 单独建一把 etcd 客户端同一思路。代价是每个 C++ 节点对 etcd 多一条 HTTP/2 连接。
- 进程启动到第一份快照到达之间（毫秒级）全部放行，与 Go 的 `New` 之后立即可用、规则为空即放行一致。

为什么 fail-open 而不是 fail-closed：止血阀自身故障时宁可放行，不能拖垮业务。所以它**不能**拿来判定权限、资格、活动开放或发奖（那些必须 fail-closed，见 [configurable-templates.md](./configurable-templates.md)）。

## 10. 与排空、熔断、限流的分工

- 热关停 = 让请求在业务执行之前快速失败。它**不**把节点从发现里摘掉、**不**迁移玩家、**不**挡新连接建立与 token 校验、**不**停 Kafka 命令消费（异步命令不是 RPC 方法）。
- 要让流量绕开一个节点，用已有的排空：scene 计划排空、gate 排空迁移、battle 准入关闭。
- 典型处置：scene A 异常 → 先写 `instance/<A 的 uuid>/*` 止血（新请求快速失败，玩家照样能下线存盘）→ 按手册排空或重启 → 删规则。
- 熔断、降载、限流是自动的、按负载；热关停是人工的、按名字。

## 11. 可观测性

C++（各节点 `/metrics`，采集器名 `kill_switch`）：

| 指标 | 类型 | 含义 |
|---|---|---|
| `killswitch_rules` | gauge | 本进程适用的有效规则条数。正常为 0，非 0 说明有止血阀开着 |
| `killswitch_blocked_total{method}` | counter | 被关停的请求数；`method` 是 `服务/方法`，取值有界 |
| `killswitch_synced` | gauge | 1 = 手上有未过期的快照；0 = 从没同步成功或已与 etcd 失联超过 `StaleAfter`（此时全部放行） |

Go：已有 `killswitch_rules`（改为只计本进程适用的规则：别的类型、别的进程的规则不计，否则一条孤儿实例规则会让全网都显示「有止血阀开着」）、`killswitch_blocked_total{method}`（路由服按目标方法拒绝的也计在这里）、`killswitch_etcd_sync_fail_total`；路由服另有 `client_rpc_router_forward_total{outcome="killswitch"}`。

日志（英文）：规则增删与快照条数打 INFO；值写坏打 ERROR；被关停的请求按采样打 WARN（逐条明细只打 DEBUG，避免止血阀本身成为日志放大面）。

## 12. 落码清单与验证（2026-11-12）

| 位置 | 改动 |
|---|---|
| `proto/common/base/rpc_message.proto` | `GameErrorCode` 加 `KILL_SWITCH_DENIED = 9` |
| `cpp/libs/engine/core/kill_switch/kill_switch.{h,cpp}`（新） | 规则值 / 键解析、匹配顺序、进程级快照、受保护方法名单、指标 |
| `cpp/libs/engine/core/node/system/etcd/kill_switch_etcd_sync.{h,cpp}`（新） | 独立 etcd 连接：Range → watch → 每 20 秒复核；流重建、watch 取消、压缩都回到全量 Range |
| `cpp/libs/engine/core/network/rpc_channel.cpp`、`rpc_status.h` | RPC0 派发前判定；新错误码按「派发前拒绝」归类 |
| `cpp/libs/engine/muduo_grpc/server.{h,cpp}` | `ServerOptions::admission`：头部阶段准入，返回状态即拒绝 |
| `cpp/libs/engine/core/node/system/node/node.{h,cpp}` | 启动时设身份并开同步；gRPC 服务端装准入；关机时停同步 |
| `cpp/nodes/gate/handler/rpc/client_session_handler.cpp` | 客户端入口判定 |
| `cpp/nodes/scene/handler/rpc/scene_handler.cpp` | 玩家方法内层判定 |
| `cpp/nodes/battle/client/battle_client_edge.cpp` | 直连入口判定 |
| `go/shared/killswitch` | `type/`、`instance/` 范围；`Config.NodeTypes`、`AddInstance`、`Config.Protected`、`Deny`；规则数只计适用的；拒绝文案改英文（与 C++ 同格式）；watch 上写坏的值与全量同步同口径（等于没有规则） |
| Go 各服务装配 | chat、client_rpc_router、data_service、db、friend、guild、login、mail / rank（domainhost）、match、player_inbox、player_locator、scene_manager、trade：填节点类型、注册后登记 uuid；client_rpc_router 与 match 新挂热关停，路由服按目标方法判 |
| `java/gateway_node` | `/admin/killswitch` 管理接口与 etcd 存储；`AdminApiKeyFilter` 改常量时间比较、按规范化路径判管理路径、未配置口令时拒绝 |

Codex 验证顺序（全部在仓库根目录；C++ MSBuild 串行 `/m:1`）：

1. 协议生成：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。期望 `cpp/generated/proto/common/base/rpc_message.pb.{h,cc}` 与 `go/proto/common/base/rpc_message.pb.go` 出现 `KILL_SWITCH_DENIED`。
2. C++：先编库（core、muduo_grpc 所在工程）再编 gate / scene / battle 与测试工程（`run_cpp_tests -Build` 不重编库，必须先单独编库）；跑 `rpc_controller_test`（新增 `kill_switch_test.cpp` 全部 `KillSwitch*` 用例）与 `muduo_grpc_test`（新增 `ServerAdmission*` 用例）。
3. Go：`cd go/shared && go vet ./killswitch/... ./domainhost/... && go test -count=1 ./killswitch/...`（新增 `scope_test.go`；`watch_test.go` 里「坏值」用例改为期望原规则失效）；再在各自目录对改了装配的模块跑 `go build ./... && go vet ./...`：chat、client_rpc_router、data_service、db、friend、guild、login、mail、match、player_inbox、player_locator、rank、scene_manager、trade；client_rpc_router、match 新依赖 `shared/killswitch`，报 `missing go.sum entry` 时先 `go mod tidy`。之后各模块 `go test -count=1 ./...`（含各服务的守链测试 `TestKillSwitchWiredIntoUnaryChain` 等与路由服 `internal/logic`）。
4. Java：`cd java/gateway_node && mvn -q test -Dtest='AdminKillSwitchControllerTest,AdminApiKeyFilterTest'`。
5. 冒烟（本机起服后）：`etcdctl put /mmorpg/killswitch/ClientPlayerChat/SendChat true` → robot chat 冒烟在 world-send 步失败、gate 与 chat 的 `killswitch_blocked_total` 增长；`etcdctl del` 同键后恢复。再写 `type/SceneNodeService/SceneSkillClientPlayer/*` 验 scene 内层；写 `instance/<gate uuid>/*` 验整节点（客户端请求全回 1003，断线后玩家正常存盘下线）。

## 13. 兼容、Java 版与不做的事

- 未升级的 Go 进程把 `type/...`、`instance/...` 当成一条含多段斜杠的目标，永远匹配不上，等于忽略，不会误关。
- 新错误码 `KILL_SWITCH_DENIED = 9`：没升级的 C++ 调用方按 `RpcStatus::FromRemoteError` 的默认分支归为 `kRemoteRejected`（确定未执行），语义正确。
- 项目未上线，etcd 里没有存量规则；上线前按 AGENTS.md §6.2 清 etcd 时一并清掉。
- **Java 版**：已有方法 / 服务 / 全局（Redis 哈希 `game:killswitch`、`game-data` 的 `/admin/killswitch`）；待做 `type` / `instance` 两级、与本文同口径的豁免与指标，已在 Java 仓 `PARITY.md` 登记待做。
- **不做**：
  - zone 维度：zone 级关闭业务用 zone 维护 / 开服接口（`/admin/zones/{id}/maintenance`）；全局服务没有 zone 身份，按 zone 判会误关；
  - 按调用方、按玩家、按比例的规则：那是限流与灰度；
  - 把被关停的节点从发现里摘掉：那是排空（§10）；
  - Kafka 命令消费：异步命令不是 RPC 方法，停消费另有手段。
