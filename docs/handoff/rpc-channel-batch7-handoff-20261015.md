# 交接:RpcChannel 改造收尾——第 7 批、剩余调用点迁移与玩家级延后完成(2026-10-15)

> **状态(2026-10-16)**:已由接手会话实施,见 [`muduo-rpc-channel.md` §20](../design/muduo-rpc-channel.md)。用户拍板:一次落完;退役范围加 Scene 1 / 40;
> Broadcast 保留;玩家级延后完成暂不实施。下文是当时的任务说明,行号与「未开始」等状态不再更新。

> 交出方:Claude(会话「C++ RPC 异步回调链式调用设计」)。接手方:另一账号的 Agent。
> 基线:`de4960bee`(2026-10-15 21:57,用户快照,已含批 1–6、gRPC 每次调用回调、GM 鉴权拒绝返回失败);其后 `0249bee0a` 提交了玩家延后设计文档。
> 本文行号以该提交为准;工作区有十几个会话并发修改,**动手前逐文件 `git status` / `git diff` 重新核对**,只改自己的 hunk。
> 设计总文档:[`docs/design/muduo-rpc-channel.md`](../design/muduo-rpc-channel.md)(§13 分批、§14 后续、§18–§19 实施记录)。
> 按 AGENTS.md §10.1:本仓 Claude 不编译、不跑测试 / 生成器;用户自己编译。Java 仓 Claude 可以自行编译测试(该仓 AGENTS §4)。

## 0. 现状一览

| 项 | 状态 |
|---|---|
| 批 1–5(RpcChannel 核心、改名、tag、切换) | 已提交,未编译(§18) |
| 批 6 玩家回包死链 | 已提交 `de4960bee`,未编译(§19.1) |
| gRPC 每次调用回调 | 已提交并 regen;`grpc_client_lifecycle_test.cpp` 有 `PerCallDone*` 三条(§19.2、[历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) §11) |
| GM 停机鉴权被拒返回 `RPC_ERROR(HANDLER_FAILED)` | 已提交(§19.3、`cpp/nodes/gate/SECURITY.md`) |
| 镜像 CreateScene 改每次调用回调 | **别的会话做**,在 worktree 分支 `claude/blissful-morse-969d44`(基线 `526780b4e`,未合并);不要碰 `player_scene.{h,cpp}`、`scene_manager_response_handler.cpp`、`player_ownership_comp.h`、`player_lifecycle.{h,cpp}` 的 `IsSceneChangeBusy` |
| Java `MessageIdRegistry` 识别墓碑 | 已提交 Java 仓 `15058ad`,`./mvnw -pl game-proto -am test -Dtest=MessageIdRegistryTest` 10/10 通过 |
| 玩家级延后完成 | **设计已定,建议暂不实施**,见 §4 与 [player-handler-deferred-reply.md](../design/player-handler-deferred-reply.md)(已随 `0249bee0a` 提交) |
| 第 7 批(墓碑 / 删 Route 一族 / 去 `RpcMethodMeta.handler`) | **未开始**,§1–§2 |
| 剩余调用点迁移 (a)–(e) | **未开始**,§3 |

本会话最后未提交的只有文档:本文(新)、`docs/README.md`(登记本文一行)、`docs/design/muduo-rpc-channel.md`(文首状态行与 §14 第 1 项各一句指针)、
`docs/PROGRESS.md`(末尾追加一条)。这几个文件里也可能混有别的会话的未提交改动,提交时按 hunk 核对。

## 1. 第 7 批 A:生成器——message_id 墓碑 + 去掉 `RpcMethodMeta.handler` / 空 `XxxImpl`

### 1.1 背景事实

- `proto/message_id.txt` 是发号唯一事实源(生成器 `pipeline.go:26` 读、`:44` 写;Java `ContractSync.java:153` 原样复制)。
  `generated/proto/{_unified,db,login}/proto/message_id.txt` 是发号前整目录拷贝的副本,无读者。
- 今天**没有墓碑**:`InitMessageId` 只在 `[0, 方法总数)` 里找空号(`service_register_info.go:189-193`),按 Go map 随机顺序回收(`:197-203`)。
  先例:19 号易过主(`docs/PROGRESS.md:5124`)。
- event_id 已有墓碑先例 `N=reserved:<KeyName>`(`internal/message_id.go:17-26`、`generator/cpp/event_id.go:427-447`);
  Go 常量生成 `message_id.go:64-67` 已跳过 `reserved:` 行,go/ 与 robot/ 不用改代码。
- `RpcMethodMeta.handler` 全仓无读者(服务端派发走 `RpcChannel` 的 `services_`,玩家消息走 `gPlayerService`)。删掉后
  `rpc_event_registry.cpp` 的 17 个 `class XxxImpl final` 消失,顺带消除它们与 `scene/handler/rpc/player/player_service_interface.cpp`
  同名(不带 final)类的 ODR 冲突。**`player_service_instance.cpp.tmpl` 生成的 Impl 不能删**(生成服务类构造函数 protected,
  `PlayerService::service()` 用它取 descriptor / prototype)。

### 1.2 已替用户拍板的默认决定(接手方可复议,复议要写进 §20 实施记录)

1. 墓碑格式沿用 `N=reserved:<KeyName>`(Go 常量生成、Python 补丁脚本、Java 都已兼容)。
2. message_id.txt 的畸形非空行 **Fatal**(跳过的行会变空号被回收);event_id 现有的 Warn 行为不动。
3. 已删方法以同名重新出现:**发新号,墓碑不动**(同 event_id D67 先例);复活旧号只能人工改行并评审。
4. 行解析与重号校验抽到新文件 `id_file.go`,event_id 改用中性名(机械重命名,逻辑零变化)。
5. 墓碑上限单独设:`maxNewMessageTombstonesPerRun = 4`,超过需 env `PROTOGEN_ALLOW_MASS_MESSAGE_TOMBSTONE=1`。本批恰好 4 个(41/74/78/81),卡线通过。
6. `rpc_event_registry.h` 的 `#include <google/protobuf/service.h>` **保留**(多处靠它传递拿 `Service` / `Closure` / `RpcController`)。
7. `OptionMessagePriority` 定义保留(删完 RoutePlayerStringMsg 后全仓无使用者,另议)。
8. `legacy_id0_reply` 计数保留(§13 批 7「删 id=0 旧规则分支」实际无分支可删:§18.1 已选停服一次切换,只剩这个计数)。
9. 视为开发期(未上线);本设计无论如何都不复用号。

### 1.3 改动清单

| 文件 | 改什么 |
|---|---|
| `tools/proto_generator/protogen/internal/generator/cpp/id_file.go`(新) | 从 `event_id.go:60-128` 挪 `eventIdFileEntry` / `parseEventIdLine` / `eventIdFileState` / `newEventIdFileState` / `add`,改名 `idFileEntry` / `parseIdFileLine` / `idFileState` / `newIdFileState` / `add`,行为不变;报错文案去掉 event 字样。规则:同号两次、活号与墓碑同号、活名重复一律报错;墓碑名与活名相同允许共存 |
| `.../generator/cpp/event_id.go`、`event_id_test.go` | 删已挪走的段,引用机械改名(`:52`、`:252`、`:266`、`:300`、`:366`);测试改名或挪进 `id_file_test.go`,用例内容不变 |
| `.../generator/cpp/service_register_info.go` | **墓碑**:(a) 删 `:69` `atomic.AddUint64(&internal.MaxMessageId, 1)` 与 `"sync/atomic"`;(b) 包级 `loadedMessageIdFile = newIdFileState()`、`reservedMessageIdMap map[uint64]string`;(c) 重写 `ReadMessageIdFile`(`:101-146`)用 `parseIdFileLine` + `add`,畸形行 / 非数字号 / add 报错 Fatal;(d) 新纯函数 `assignMessageIds(services, file) (messageIdAssignment{Infos, Reserved, MaxId, NewlyReserved, ReaddedOverTombstone}, error)`,照 `event_id.go:285-356`:活号沿用;文件里有、本轮 proto 没有的活号转墓碑;新方法按 `GlobalRPCServiceList` 顺序先**升序**填 `0..file.MaxId` 中既非活号也非墓碑的空洞,再从 `MaxId+1` 往后发(替换 `:189-203`);KeyName 与墓碑同名 → 发新号;本轮两个活方法 KeyName 相同 → error;(e) `checkMessageIdInputComplete(file, result, allowMass)`:活方法 0 而文件有活号 → 报错;新墓碑 > 4 且 env 未放行 → 报错并列出 `号=名`;(f) 重写 `InitMessageId`(`:169-214`)调 (d)(e),失败 Fatal(此时文件尚未写,`pipeline.go:42-44`);每条新墓碑 / 同名复活打 WARN;结果写 `internal.RpcIdMethodMap`、`reservedMessageIdMap`、`internal.FileMaxMessageId`(含墓碑);(g) `WriteMessageIdFile`(`:148-167`)改纯函数 `renderMessageIdFile(infos, reserved)` + `WriteFileIfChanged`,按号升序输出活行与 `N=reserved:<KeyName>`,删 `:156-163` 死分支。**去 handler**:删 `ServiceInfoCppData.HandlerClasses`(`:246`)、`handlerClasses` / `handlerClassSet`(`:260`、`:269`)、生成 `class %sImpl final` 的块(`:292-298`)、`:391` 赋值;`:333-338` 去掉 `handler := service.Service()+"Impl"`,init 行改 `"    %d, %s};"`;`:351-354` 去掉 `nullptr, `;注释 `:323-328` 改「protocol, targetNodeType [, sender]」;建议抽纯函数 `rpcMethodMetaInitLines` 便于单测。`generateInstanceData`(玩家 Impl)不动 |
| `.../generator/cpp/service_register_info_test.go` | 原 `TestInitMessageIdUpdatesRegistrySizeWhenAssigningUnusedId`(`:12-66`)改测纯函数并更名 `TestAssignMessageIdsAppendsNewMethodAfterFileMax`;新增:墓碑不回收(`0=A,1=B,2=C` + proto `A,C,D` → 1 墓碑、D 得 3)、跳墓碑填普通空洞(`0=A,1=reserved:B,3=C` + `D,E` → 2、4)、升序确定性(循环 50 次一致)、同名复活发新号且墓碑保留、KeyName 重复拒绝、渲染往返逐字节一致、上限 4 过 5 拒 env 放行空输入拒、墓碑在最大号时长度仍覆盖、Route 一族删除恰好 `[41,74,78,81]` 转墓碑、`RpcMethodMeta` init 行不含 `Impl` / `nullptr,`。全部只测纯函数 |
| `.../internal/model.go` | 删 `MessageIdMap` / `MaxMessageId` / `MessageIdFileMaxId`(`:137-139`);`FileMaxMessageId` 与 `MessageIdLen` 注释改「最大号(含墓碑)+1」 |
| `.../internal/message_id.go`、`message_id_test.go` | 注释说明两份 id 文件共用墓碑;新增 `TestGenerateWithSuffixSkipsReservedMessageIds` |
| `.../internal/template/service_header.tmpl` | 删 `:17` `std::unique_ptr<::google::protobuf::Service> handler;`(`service.h` include 保留) |
| `.../internal/template/rpc_event_registry.cpp.tmpl` | 删 `:14-16` `{{range .HandlerClasses}}` |
| 可选 | `proto/README.md:23` 补墓碑说明;`robot/travel_smoke_wire.go:26` 注释改「按号升序填空洞,墓碑号永不复用」 |

## 2. 第 7 批 B:删 Route 一族 proto 方法

### 2.1 事实

- 不属于客户端契约:Gate / Scene 服务没标 `OptionIsClientProtocolService`,41/74/78/81 不在 `IsClientMessageId`;Go / robot / Java 只有生成物或 proto 副本引用。
  但 message_id.txt 格式变了,Java 必须能读墓碑(**已做**,Java `15058ad`)。
- 生成器对已删方法的**方法守护段直接丢弃**(`code_parser.go:136-146`,不报警),**全局守护段逐字保留**(`:110-128`)——
  所以全局守护段里的拒绝函数必须手删,否则 regen 后留下未引用的内部链接函数(C4505 / `-Wunused-function`)。
- `PlayerInfoExternal` 仍被 `message.proto` 的 `PlayerNodeServiceRequest.header` / `PlayerMessageResponse.header` 使用,保留。

### 2.2 改动清单

| 文件 | 改什么 |
|---|---|
| `proto/gate/gate_service.proto` | 删 `:60` `RouteNodeMessage`、`:61` `RoutePlayerMessage` |
| `proto/scene/scene.proto` | 删 `:112` `RouteNodeStringMsg`、`:113-115` `RoutePlayerStringMsg`(含 option 块) |
| `proto/common/base/message.proto` | 删 `RoutingNodeInfo`(`:10-15`)、`RouteMessageRequest` / `Response`(`:110-128`)、`RoutePlayerMessageRequest` / `Response`(`:130-144`);整条 message 删除不涉及字段号复用 |
| `cpp/nodes/gate/handler/rpc/gate_service_handler.cpp` | 全局守护段 `:55-84`(源路由退役说明 + `RejectDeprecatedRouteCall`)与 `:10` `rpc_channel_stats.h` include——**若做 §3(e),改为把它泛化成 `RejectRetiredCall` 留着用**,见 §3(e)。两个 Route 方法体(`:160-181`)由 regen 自动去掉;regen 之前它们仍被生成头声明,**不要先手删方法体**(会缺虚函数定义),如删了 helper 就把这两个方法守护段里的调用一并清空 |
| `cpp/nodes/scene/handler/rpc/scene_handler.cpp` | 手删全局守护段 `:43-72`(注释 + `RejectDeprecatedRouteCall`)与 `:15` `rpc_channel_stats.h` include;两个 Route 方法(`:598-619`)的守护段内容清空(方法体本身由 regen 去掉)。保留 `CheckRoutedPlayerIdentity`、`WriteRejectTip`、`rpc_controller.h` |
| `cpp/tests/proto_field_checker_test/proto_field_checker_test.cpp` | `:10-32` 两个用例从 `RouteMessageRequest` / `add_route_nodes` 改成 `ProtoFieldCheckerTestSubPB`(`message.proto:169-172`)/ `add_items("x")`,第二个用例期望文案改 `"Field 'items' exceeds threshold"`,阈值语义不变 |
| `cpp/tests/routing_identity_test/routing_identity_test.cpp` | 可选:`:342-344` 注释改「Route 一族已在批 7 从 proto 删除(41/74/78/81 留墓碑)」 |
| 生成物(regen 产出,不手改) | `gate_service_handler.h` / `scene_handler.h` 声明消失;两个 metadata 头的常量、宏、`<KeyName>Rpc` tag 消失且其后 `Index` 前移(无手写读者);`rpc_event_registry.cpp` 4 个 Route 项消失、`kMaxRpcMethodCount` 仍 264;`message.pb.*`、`gate_service.pb.*`、`scene.pb.*`;`go/proto/**`;各 `go/*/generated/pb/game/message_id.go` 与 `robot/generated/pb/game/message_id.go` 少 4 个常量 |

### 2.3 顺序与最大风险(必须遵守)

1. **旧生成器会悄悄毁掉墓碑**:旧 `ReadMessageIdFile` 把 `41=reserved:X` 当未知键、把 41 当空号回收、写回时丢掉墓碑行。
   `dev_tools.ps1` 默认优先跑预编译的 `proto-gen.exe` / `pbgen.exe`,所以**每次都先 `proto-gen-build` 再 `proto-gen-run`**;
   评审时确认 `proto/message_id.txt` 的 diff 只有「改行」、没有「删行」。别的会话若用旧 exe regen,同样会抹掉墓碑——提交前通知相关会话。
   (2026-10-16 补:`proto-gen-run` 现会按写入时间判 exe 是否落后于生成器源码——默认路径告警并改走 `go run ./cmd`,`-UseBinary` 直接拒绝;
   `proto-gen-build` 成功后把 exe 写入时间打成构建开始时刻。仍防不住两类:绕过 `dev_tools.ps1` 直接跑 exe;exe 写入时间被刷新却没重建,
   如不保留时间的复制。上面「先 build 再 run」与「核对只改行不删行」照旧要做。详见 `docs/PROGRESS.md` 同日条目。)
2. **批内顺序**:先只落 §1 生成器改动 → `proto-gen-build` → 基线 regen(此时 `message_id.txt` 应零 diff)→ 再落 §2 proto 删除 → regen。
   若 proto 先删、生成器没重建就 regen,41/74/78/81 四行直接消失;补救:下次 regen 前手工把四行恢复成 `N=reserved:<KeyName>`。
3. Java:`java tools/ContractSync.java --mmorpg ..\xuanming-server-mmo-cpp` 同步带墓碑的 message_id.txt 与三个 proto 副本(会带上主工作区所有客户端契约改动,含他人未提交的,需人确认);
   Java 仓 `PARITY.md` 登记一行(墓碑格式 + 删 Route 一族;`RpcMethodMeta` 属 C++ 内部不适用)。
4. 独立客户端仓 `../mmorpg-client` 是否直接读 message_id.txt **未核实**(无授权);若读,也需容忍墓碑行。

## 3. 剩余调用点迁移

来源:本会话两轮只读扫描(`muduo-rpc-channel.md` §8 / §14 的候选)。

| 项 | 结论 | 改法 |
|---|---|---|
| (a) `cpp/libs/engine/core/network/rpc_session.h` `FailNotConnected` | **要做**:构造时连接为空则 `loop_` 留空,channel 不在时 `CallMethod` 的 done 被就地丢弃,违反「恰好一次、永不同步」(`rpc_channel.h:112-114`)。全仓 4 个构造点连接都非空(生产 `registration_manager.cpp:229` 已在 `:171-177` 判空;测试 `rpc_client_lifecycle_test.cpp:391/394/395`) | 构造即拒空连接:私有 `static EventLoop& LoopOf(conn, uuid)`,空则 `LOG_FATAL`;`loop_` 改 `std::reference_wrapper<EventLoop>`(可拷贝赋值,entt swap-and-pop 可用;全仓无 `get_or_emplace<RpcSession>`);`FailNotConnected` 只剩一行 `RpcChannel::FailNotSent(loop_.get(), messageId, std::move(done), "session channel gone", node_uuid);`。回归测试追加在 `rpc_client_lifecycle_test.cpp` 末尾:session 构造后清掉 conn context 里的 channel,`CallMethod` 返回时回调 0 次、PumpLoop 后恰好 1 次 `kNotSent` 且 response 为空 |
| (b) `registration_manager.{h,cpp}` 握手 | **要做**:今天用运行期表 `kNodeTypeToMessageId`(`:20-26`)+ 无类型 `CallMethod`,手写 descriptor 校验与 `static_cast`(`:127-132`)。tag 已生成 | 匿名命名空间加 `VisitHandshakeRpc(nodeType, fn)`:`SceneNodeService → SceneNodeHandshakeRpc`、`GateNodeService → GateNodeHandshakeRpc`,`static_assert` 两者 Request/Response 同型;`IsHandshakeNodeType` 改为 `VisitHandshakeRpc(nodeType, [](auto){})`;`SendHandshake` 用 `client->CallMethod<Method>(req, lambda, RpcCallOptions{kNodeHandshakeCallTimeout})`,lambda 仍只捕获 `{nodeType, weakClient, weakConn}`;`OnHandshakeCallDone` 的 response 改 `const std::shared_ptr<NodeHandshakeResponse>&`(头文件同步),删 descriptor 校验与 `static_cast`(typed 调用发送时已校验类型,解析失败得 `kInvalidResponse` 落 default 分支照旧重试)。**Gate 项不能删**:scene 收 gate 握手时 `OnNodeHandshake`(`:182`)靠它放行——顺带更正 `muduo-rpc-channel.md` §10 末条「只剩 Scene 一项」 |
| (c) `cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp:141` | **要做,一行**:`SendRequest(ScenePlayerEnterGameNodeMessageId, req)` → `SendRequest<ScenePlayerEnterGameNodeRpc>(req)`;返回值语义不变,线上字节不变 | — |
| (d) `scene_handler.cpp` `ProcessClientPlayerMessage` 回显 `session_id` | **要做**:gate 已在调用点 lambda 捕获会话,不再读应答的 `session_id`(已核 gate / Go / robot / Java 无读者) | 删守护段内 `:464` `response->set_session_id(sessionId);`;`message_content` 的 `id` / `message_id` 必须保留(客户端协议)。proto 字段暂不 `reserved`:`rpc_channel_loopback_test.cpp` 与 `rpc_chain_test.cpp` 拿它当关联标记,要 reserved 须同批改夹具 |
| (e) `gate_service_handler.cpp` 的 `Gate.PlayerEnterGameNode`(42)与 `Gate.BindSessionToGate`(56) | **建议做**:全仓(含 Go / robot / Java)无发起端,且都是无栅栏写路径(前者改写会话 scene 绑定,权威写入口在 `scene_route_helper.h`;后者按代次 0 写登录绑定) | 全局守护段的 `RejectDeprecatedRouteCall` 泛化为 `RejectRetiredCall(controller, method, reason, occurrences)`(保留 `ShouldLogRpcEvent` 采样与 peer 地址),两个方法守护段改为显式拒绝。proto 方法删除留到下一次带墓碑的收口(否则本批墓碑超过上限 4)。同类无发起端的还有 `Scene.SendMessageToPlayer`(1)、`Scene.InvokePlayerService`(40),待用户决定是否列入退役 |
| (f) `player_message_utils` 的 `BroadcastMessageToScene` / `BroadcastMessageToAll` | 零调用方,建议删;**未做**:要改 `.github/copilot-instructions.md:96`(规范文件须用户同意),且「玩家消息投递与位置管理」会话可能有接入计划,先问 | — |
| (g) 镜像 CreateScene | 别的会话已做(见 §0) | 合并时注意主工作区 `player_scene.cpp` 有一行他人 include(`type_alias/player_session_type_alias.h`),两边都要保留 |
| 不做(需各自 owner) | PlayerEnterGameNode 改 `CallMethod` 拿送达确认(进场欠账 owner 决定,方案见扫描结论:一期只计数 + 采样 WARN);EnterScene correlation / id 号段 / gate lane 的 gRPC 认领改每次调用回调(各自 owner);`SendMessageToClientViaGate` 返回 bool(「玩家消息投递」会话要求签名语义不变) | — |

## 4. 玩家级延后完成:等用户拍板

用户原话要求做完它,但评审结论是**暂不实施**(三个独立方案一致,详见 [player-handler-deferred-reply.md](../design/player-handler-deferred-reply.md) §1、§7):
没有合理的首个调用方(`GmQueryTransactionLog` 等 P1-B 桩所在路径按设计关闭;`EnterScene` / `TravelToZone` 是「受理 + 推送」契约,不该延后),
按 AGENTS §11.2 YAGNI 不写无调用方的机制。**接手方先问用户**:
- 同意暂不做 → 只需在 `muduo-rpc-channel.md` §14 第 1 项追加指向该设计文档的一句,PROGRESS 记一笔;
- 坚持现在做 → 按设计文档 §3、§6 实施(注意 §8:`player_service_gen.go` 另有会话改动,先等其提交再 regen)。

## 5. 文档收尾(随实现同批)

- `muduo-rpc-channel.md`:新增 §20「批 7 实施记录」(墓碑格式、自检、复活规则、Route 删除、handler / Impl 删除、§3 各项);
  改写 §9.3 标题与末段(「暂不删」→「已删,墓碑见 §20」,顺带更正「Unity 从 message_id.txt 生成」——实际用内存里的 `method.Id`,`unity_client_handler.go:99`);
  §11 的 R3 行、§13 批 7 行、§17 第 5 项标为已决;文首状态行同步;§14 第 1 项见 §4。
- `docs/PROGRESS.md` 末尾追加一条(append-only,merge=union),注明 Java 版号(Java `15058ad` + ContractSync 之后的提交)。
- Java 仓 `PARITY.md` 登记一行。
- 可选:`docs/design/client-access-band-routing.md` §11.2 补一句——D54 的 `message_id_state.json` 首次 bootstrap 必须导入 `N=reserved:` 行。

## 6. 验证(交用户执行;Claude 一条都没跑)

工作目录仓库根,PowerShell。证据放仓库外(如 `$env:TEMP\rpc-batch7-<ts>`)。

1. 前置:`git status --short` 存档;`Get-Process MSBuild,cl` 为空;`pwsh -File tools/scripts/dev_tools.ps1 -Command dev-stop`。
2. **只落 §1 生成器改动** → `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`(失败就停,绝不在旧 exe 上 run)。
3. protogen 自检:`cd tools/proto_generator/protogen; $env:GOTOOLCHAIN='local'`;`gofmt -l ./internal ./cmd` 无输出;`go vet ./internal/... ./cmd`;`go test -mod=readonly ./internal/... ./cmd`。
4. 基线 regen:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
   通过:`proto/message_id.txt` 零 diff;`rpc_event_registry.cpp` 无 `Impl final`、init 行无 `nullptr,`;`RpcMethodMeta` 无 `handler`;`kMaxRpcMethodCount` 仍 264(或当时最大号 +1)。
5. 落 §2 proto 删除与守护段手删、§3 → 同命令再 regen。通过:恰好 4 条墓碑 WARN;`git diff proto/message_id.txt` 只有 41/74/78/81 四行变 `N=reserved:<KeyName>`、无删行;
   Route 相关内容从 handler / metadata / registry 消失;各 `message_id.go` 少 4 个常量;`git -C ..\mmorpg-client status` 不变。
6. 再 regen 一次:只同步 `generated/proto/*/proto/message_id.txt` 三份拷贝;第三次零 diff。
7. Go:`cd go/proto; go build ./...`;各服务模块 `go build ./...`;`cd robot; go mod vendor; go build -mod=vendor ./...`(vendor 会带上 go/proto 下他人未提交改动)。不要跑 `go/build.bat`。
8. C++:严格串行 `/m:1 /p:Configuration=Debug /p:Platform=x64`,proto → rpc → … → core → services → gate、scene、battle 节点;battle 的 `boost/any.hpp` C1083 是既有阻塞单列。
   `RpcMethodMeta` 布局变了,所有库用同一份头重编,不混用旧 .lib。测试工程:`proto_field_checker_test`、`rpc_controller_test`(Rebuild)、`routing_identity_test`、`bag_test`。
9. 运行:`rpc_controller_test.exe`(全量 + `--gtest_filter=InflightIndex*:RpcChannel*:RpcChain*:GrpcClientLifecycle* --gtest_shuffle --gtest_repeat=50`)、`proto_field_checker_test.exe`;退出码 0、无 `[  FAILED  ]`。
10. 冒烟:`robot -c etc/travel_smoke.yaml` 与 robot_smoke;握手成功;`[RpcChannel] summary` 的 timeout / late_reply / legacy_id0_reply 为 0;客户端请求类消息有回包。
11. Java:ContractSync(人确认差异后)→ `./mvnw -B clean install -DskipTests` → `./mvnw -B -pl game-proto -am test`。

## 7. 协作提醒

- 并发会话(2026-10-15 晚):「补镜像建场景在途标记与整理背包的冻结判断」(CreateScene,见 §0)、「修 gRPC CQ 轮询遇 !ok 跳过剩余完成事件」(在改 `grpc_async_client.*.tmpl` 的流式分支)、
  「C++ gRPC 迁移至 muduo HTTP/2」(muduo_grpc 传输;它要求:调用方不得按 code 区分「停机」与「传输失败」,非 OK 一律当结果未知)、
  「玩家消息投递与位置管理」(要求 `SendMessageToClientViaGate` 签名语义不变)、「Route 水平扩展消息乱序问题」(gate client_request_lane)、「玩家收件箱服务设计」(在 `message_id.txt` / `proto_gen*.yaml` 加东西)。
- 子代理只读时禁止 `git show` 重定向到 %TEMP% 留副本;不 `checkout` / `stash` / `reset`;只改自己的 hunk。
- 交付说明一律写「未编译,待用户验证」,两版状态写明(AGENTS §12)。
