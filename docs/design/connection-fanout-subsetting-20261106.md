# 连接与发现扇出:路由服确定性子集、每类节点只发现自己用到的(2026-11-06)

> 状态:**设计稿,未落码**。对应[服务器代码缺口清单](../handoff/server-gap-audit-20261101.md)第 30 条与第 28 条后半(「连全服全部目标」)。
> 接在[控制面按 zone 化](control-plane-zone-scoping-20261105.md)之后:那份已经把 zone-scoped 类型的发现收窄到本 zone、把兜底 Range 拉长到约 60 秒。本文处理剩下的、随全服规模平方增长的连接与 watch。
> 挑 gate 的规则以「玩家切换zone和gate链接」会话 11-06 的改动为准(`go/shared/gatepick/eligibility.go` 一次分档判定),本文不另定挑 gate 规则。

## 1. 结论

1. **gate → 路由服:按 zone 取确定性子集。** zone z 的路由服集合 = 对 `(z, 路由服 node_uuid)` 做 HRW(rendezvous)打分取前 k 台,k 缺省 3,在同一发布轨道内算。gate 只连、只挑这 k 台。gate 到路由服的连接从 G_全服 × R 降到 G_全服 × k。路由服增减时,只有前 k 名变了的 zone 换路由服,其余 zone 不动。
2. **路由服 → 目标:zone 跟着子集走,全服池再取一次子集。** 稳态下一台路由服只收到分给它的那些 zone 的请求,zone-scoped 目标(login、scene_manager)只会拨这些 zone 的实例。全服池目标(match、chat、friend、guild 等)按 HRW 对 `(路由服 node_uuid, 目标 node_uuid)` 取前 k_t 台,k_t 缺省 4。连接按空闲回收,子集变了之后旧连接自然消失。
3. **gate ↔ scene 的 zone 内全连接保持不变。** 它的上界是 G_z × S_z,随单 zone 规模增长,不随全服 zone 数增长;单 zone 规模由开服容量限定。重新评估的触发条件:单 zone 的 G_z × S_z 超过 10 000 条连接(经验值,待压测),或者做万人同屏的时候(那时整体重做,现在不预留)。
4. **每类 C++ 节点只发现自己用到的类型。** 每类节点显式声明自己的发现集合,与 `CanConnectNodeTypeList` 并列。共享的前缀列表(本机 `bin/etc/base_deploy_config.yaml` 13 个,K8s 模板 11 个)只回答「部署里有哪些前缀」,不再决定每个节点 watch 什么。battle 的集合是 {Match},自身类型只 watch 自己那一条键,不再 watch 全部 battle。
5. **控制面命令 topic 的分区数先不改。** 每个节点只 assign `node_id % P` 那一个分区,读放大等于同分区的节点数。按该 topic 的设计口径(控制面总量每秒几百条),P = 256 时每个节点每秒只解码几条。先加读放大计数,超过阈值再按既有规则换代号、换更大的 P,绝不原地扩分区。
6. **场景亲和先测再定。** 场景广播已经按 gate 分组,每台 gate 一份。亲和只能减少一个场景的玩家分散在几台 gate 上。先在 scene 侧数每次广播涉及几台 gate,压测证明它是瓶颈再做。要做时作为 gatepick 判定的一维(同一档里偏好已有该场景玩家的 gate),不在调用方另加一层过滤。

## 2. 现状(2026-11-06 核对)

| 项 | 位置 | 现状 |
|---|---|---|
| gate 连路由服 | `cpp/nodes/gate/main.cpp:379-382` | 路由模式白名单是 {Scene, ClientRpcRouter};发现到的每台路由服都建连 |
| gate 挑路由服 | `cpp/nodes/gate/handler/rpc/client_session_handler.cpp` 的 `PickRandomNode` | 在全部已连上的路由服里随机;全服池类型按本 gate 的发布轨道过滤 |
| 路由服挑目标 | `go/client_rpc_router/internal/discovery/node_watcher.go` | 11-05 起按 zone、按轨道的只读索引,选点 O(1) |
| 路由服连目标 | 同文件 `DialEndpoint` | 懒拨号,按 endpoint 缓存,只在节点下线时关;路由服不分 zone,任何 zone 的请求都可能到任何一台,长期运行后每台都连上全服全部目标 |
| C++ 发现 | `cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp` 的 `EffectiveDiscoveryPrefixes` | 每个节点对部署配置里同一份前缀列表(本机 13 个,K8s 模板 11 个)Range 并 watch,zone-scoped 类型收窄到本 zone;全服池类型(battle、路由服、match、data_service、player_inbox、team)全量 |
| battle 看 battle | `tools/scripts/k8s_deploy.ps1` 的 base_deploy_config 模板注释、`cpp/nodes/battle/main.cpp` 的 `ObservedAdvertised` | battle 保留 BattleNodeService 前缀只为了看自己的键(劫持检测、注册自检),实际收到全部 battle 的变更 |
| 命令 topic | `cpp/libs/engine/core/node/system/node/node_command_topic.h` | partition = node_id % P,P 缺省 256,可配置,改 P 要换代号 |
| 场景广播 | `cpp/libs/engine/core/network/client_message_sender.cpp` | `BroadcastToPlayers` 按 gate 分组,每台 gate 一份,只带本 gate 上的收件人;`BroadcastToSceneClientsImpl` 给场景玩家所在的每台 gate 各发一份 |

## 3. 设计

### 3.1 HRW 子集

- 打分:`score = FNV-1a-64(key + "/" + member_uuid)`,取分数最高的前 k 个;分数相同按 uuid 字典序。不用 `std::hash`:它的结果随编译器和平台变,Windows 与 Linux 构建的 gate 会算出不同的子集。这不影响正确性,但会让问题难查。
- 只有算子集的一方需要这个函数:gate 算 zone → 路由服,路由服算自己 → 全服池目标。两边各写各的,不需要跨语言对拍。路由服无状态,同一 zone 的 gate 在成员变化的几秒内看法不一致也没有正确性问题。
- 成员来自发现:路由服与全服池目标的实例数都在百台以内,全量 watch 的代价是线性的(控制面按 zone 化 §6 第 6 条),不用改。重算是 O(成员数),只在成员变化时做一次。
- 发布轨道:先按轨道分组再取子集。canary 会话优先 canary 组,组里没有可用实例才退 stable;stable 永远不进 canary。规则与 `releasetrack.Select` 相同。
- 实例数不足 k 时取全部。

### 3.2 gate 侧

- 引擎今天对白名单类型「发现即连」。给连接建立加一个按类型的准入钩子 `ShouldConnect(nodeType, NodeInfo) -> bool`,gate 给 ClientRpcRouterNodeService 装上「在本 zone 子集里」的判定。
- 成员变化时重算子集:新进子集的立即建连;掉出子集的立即不再被挑选,在途请求按 gate 的路由服调用期限(`GrpcClient.CallDeadlineMs.ClientRpcRouterNodeService`)加 1 秒之后关连接。
- `PickRandomNode` 对路由服的候选改成「已连上 ∩ 子集」。DependencyGate 对路由服的要求改成子集里至少一台已连上。
- 配置键 `ClientRpcRouterSubsetSize`,缺省 3,最小 2:一台路由服重启时,本 zone 至少还有一台可用。
- 子集里的路由服都连不上(登记还在、进程不响应):照旧回 `kServiceUnavailable`。不临时扩大子集。登记失效(租约过期)后它自然掉出子集,下一名补上。

### 3.3 路由服侧

- zone-scoped 目标的挑选不变。gate 只把本 zone 的流量送给本 zone 的 k 台路由服,每台路由服只会拨约 Z × k / R 个 zone 的实例。
- 全服池目标:`PickRandomForTrack` 的候选改成本路由服在该类型、该轨道里的 HRW 前 k_t 台。每个目标实例约有 R × k_t / T 台路由服连它,R 台路由服负载相近时请求仍然均匀。配置键 `GlobalTargetSubsetSize`,缺省 4。
- 连接缓存加最后使用时间,后台每分钟关掉空闲超过 10 分钟的连接。子集或 zone 分配变了之后,不再被用的连接靠它回收,不会只增不减。节点下线时的立即关闭保持不变。

### 3.4 C++ 每类节点的发现集合

- `Node` 构造时多给一个发现集合 `DiscoveryNodeTypeList`。`EffectiveDiscoveryPrefixes()` 只返回配置前缀里属于这个集合的那些(zone-scoped 照旧收窄到本 zone)。
- 自身类型:在集合里就按前缀 watch;不在集合里只 watch 自己那一条发现键,劫持检测与注册自检照常工作。battle 就是后一种。
- 集合必须覆盖这类节点代码里每一处 `GetRegistry(type)` 和 `NodesOfType(type)`。测试构建里对不在集合里的类型调用这两个函数直接断言失败;不要只靠评审记忆。
- 初始集合(落码时逐处核对,以代码为准):
  - scene:SceneManager、Gate、PlayerStore(以上按本 zone),DataService、PlayerInbox、Team、Match。scene 连 gate 的会话在 gate 主动连进来时建立,但广播用 `ResolveLocalZoneGateEntity` 查 gate 登记,所以要 Gate。
  - gate:Scene(本 zone);路由模式加 ClientRpcRouter,直连模式加 Login、SceneManager(本 zone)与 Match。
  - battle:Match;自身只看自己的键。
- 效果:battle 之间的 B² 项消失;scene 不再 watch Battle 与 ClientRpcRouter;gate 不再 watch DataService、PlayerInbox、Team、Battle。

### 3.5 命令 topic 的读放大

- 消费端加计数 `node_command_consumed_total{node_type, target}`,target 取 own(目标是本节点)或 foreign(同分区的别的节点),标签只有这两个值。
- 告警条件(经验值,待压测):单节点 foreign 每秒超过 50 条,且 foreign 与 own 之比超过 50,持续 10 分钟。
- 处理办法沿用 `node_command_topic.h` 的契约:换代号、换更大的 P(2 的幂),生产端与消费端按同一配置一起切换,不在同名 topic 上扩分区。

### 3.6 场景亲和(先测)

- scene 侧加直方图 `scene_broadcast_gate_fanout`(每次场景广播涉及几台 gate)。按场景类型分桶,不按场景号打标签。
- 压测中如果它长期接近 G_z,而且 gate 的入站广播是瓶颈,再做亲和:作为 gatepick 判定的一维,在同一档里偏好已经有该玩家目标场景玩家的 gate。前提是 login 在 AssignGate 时知道玩家要去的场景(落点或上次位置),那时另写设计。

## 4. 失败与一致性

- 子集变化只会让路由服开始收到新 zone 的请求,第一批请求多付一次建连(毫秒级)。路由服不持有会话状态,请求之间的顺序由 gate 的会话 lane 保证(`client-request-ordering.md`),与挑哪台路由服无关。
- 滚动重启路由服:每台重启使成员变两次。HRW 只移动前 k 名变动的 zone,其余 zone 的连接不动。
- 全服池子集:目标实例重启或增减,只影响把它排在前 k_t 的那些路由服。
- 发现集合漏了某个类型:测试构建断言失败;生产构建里,对应的 registry 为空,调用方照旧报「Node not found」。所以落码时必须逐处核对(§3.4)。

## 5. 被否决的做法

- **按 zone 部署路由服**(路由服进 zone namespace):Pod 数随 zone 数增长,小 zone 也要至少 2 台。HRW 在共享池上得到同样的局部性,一台挂了自动由下一名接手。
- **带虚拟节点的一致性哈希环**:k 小、成员在百台以内时,HRW 更简单,O(成员数)重算,不用维护环。
- **etcd gRPC proxy 合并 watch**:发现集合收窄之后用不上;进程数到 10⁵ 量级时再评估。
- **gate 不 watch 路由服、改由配置给地址**:失去自动扩缩与故障摘除。

## 6. 分批与文件

| 批 | 内容 | 文件 | 依赖 |
|---|---|---|---|
| 1 | 路由服全服池子集与空闲连接回收 | `go/client_rpc_router/internal/discovery/node_watcher.go` 及测试、配置 | 无 |
| 2 | gate 的路由服子集:HRW、连接准入钩子、掉出子集后的延后关闭 | `cpp/libs/engine/core/node/` 的连接建立处(新增钩子)、`cpp/nodes/gate/`、gtest | 无 |
| 3 | 每类节点的发现集合;battle 只看自己的键 | `service_discovery_manager.cpp`、`node.h/.cpp`、三类节点的 `main.cpp`、`etcd_service` 的自检、gtest | 改引擎公共代码,所有 C++ 节点受影响 |
| 4 | 计数:命令 topic 读放大、场景广播 gate 扇出 | 命令消费处、`client_message_sender.cpp`、Prometheus 规则 | 无 |

- `node_command_topic.h` 另有会话的未提交改动(观战 topic),本设计不改这个文件。
- 新增 C++ 源文件只登记 `.vcxproj` 与 `.filters`,CMakeLists 由它生成。

## 7. 验证(交 Codex)

- 批 1:`cd go/client_rpc_router && go build ./... && go vet ./... && go test -race ./...`。HRW 用例覆盖:分布均匀(1 000 个 key、R = 10、k = 3,每台承担的 key 数在均值 ±20% 内);增减一台时,只有前 k 名变了的 key 移动;空闲回收用注入时钟。
- 批 2、3:`msbuild game.sln /m:1`;gtest 覆盖子集计算、准入钩子、延后关闭、发现集合断言。
- 冒烟:本机 3 个 zone、3 台路由服、k = 2,核对每台 gate 只连 2 台路由服,每台路由服只拨分给它的 zone 的 login 与 scene_manager。
- 压测按 AGENTS §6,对比连接数、etcd watch 事件量与登录延迟,不预先宣称结论。

## 8. Java 版

未核对 Java 版 game-gate 与路由层的连接方式。落码时核对,有同类问题再登记 Java `PARITY.md`;不适用或已有等价就不登记。
