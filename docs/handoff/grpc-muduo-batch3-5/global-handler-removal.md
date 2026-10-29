# 删除 gRPC 生成客户端的全局应答处理器：给 Codex 的逐文件改动清单

> 2026-10-16 由 Claude 只读调查写成，又按两路对抗核对（完整性、语义）定稿。没有改代码，没有编译，也没有测试。
> 基线：HEAD `a105c3efd`，加上主工作区里还没提交的第 3 批（muduo_grpc 生成客户端）和 RPC0 第 7 批改动。
> 行号取自 10-16 主工作区。定稿复核时又漂了一次：`SendCorrelatedEnterScene` 从 L465 到了 L469，`DispatchEnterSceneReply` 从 L4833 到了 L4854，scene `main.cpp` 的两个 Init 调用从 L139/L142 到了 L137/L140。说明还有会话在改这些文件。**定位一律按函数名或字符串，行号只作参考。**

---

## 0. 目标、理由、前置条件、范围

**目标**：C++ 生成的 gRPC 客户端只留一种完成形式，即每次调用回调 `Send<Svc><Method>(registry, node, req, GrpcDone<Resp> done, const GrpcCallOptions& = {})`。要删的有：
- 全局 `Async<Svc><Method>Handler` / `FailedHandler`；
- 文件级 `Set<File>[IfEmpty][Failed]Handler`，以及总广播 `Set[IfEmpty][Failed]Handler`；
- 三个不带回调的 `Send` 重载；
- 只为上面这些存在的 `GrpcReplyContext` / `GrpcCallFailure`。

**为什么**
1. **和 RPC0 保持一致。** RPC0 第 6 批已经删了全局回包表，回包在 `CallMethod` 的 lambda 里处理，done 为空直接 `LOG_FATAL`（`cpp/libs/engine/core/network/rpc_channel.cpp` 的 `RpcChannel::CallMethod`）。gRPC 是剩下的唯一例外。
2. **不再按回显字段反查。** 全局处理器拿不到请求上下文，调用方只能靠响应回显或 metadata 编号把应答对回请求：EnterScene 用 `correlation_id`，收件箱用 `player_id/domain/correlation_id`，gate 用 `x-session-detail-bin`，号段用 `x-idseg-seq` 加 FIFO。每次调用回调的闭包本身就带着这些值。
3. **同一方法不再有多个调用方抢处理器。** 处理器是进程级单例，一个方法只能装一个。漏装就静默丢应答（f1b110bcc 让换图应答丢了约 5 个月），两个调用方各装一个会互相覆盖。
4. **漏接应答变成编译错误。** 无回调重载删掉之后，忘了接应答就编不过。

**前置条件**（逐条确认，任何一条不满足就停下报告）
- **负责人已按 AGENTS §10.2 授权。** 全清单手改约 60 个文件，另有重生成产物（`cpp/generated/grpc_client/**`、`rpc_event_registry.{h,cpp}`），远超 30 个文件的门槛。所以拆成 4 批分别派发、分别授权：
  - 批 A：步骤 0–4（scene 侧），约 27 个手改文件；
  - 批 B：步骤 5（gate、生成器增量、重生成），约 16 个手改文件加重生成产物；
  - 批 C：步骤 6（模板删除），约 7 个手改文件加重生成产物；
  - 批 D：步骤 7（注释与文档），约 26 个文件。

  派发时负责人必须写明“授权本清单第 X 批”，含重生成产物的批次要把产物点名。没有授权就停下报告，不得自行开工。
- 第 3 批已编译通过。交接 README 记的是“10-16 代码补完，未编译”，拿不到编译结果就不要开始。
- RPC0 第 7 批已收口。它在 `service_register_info.go`、`service_register_info_test.go`、`service_header.tmpl`、`rpc_event_registry.cpp.tmpl` 上有未提交改动，步骤 5 要叠在它上面改。
- **本清单与第 4 批（服务端改 muduo + nghttp2，`grpc-muduo-transport.md` §15）无关，不要混在一起做。** 不碰 `cpp/nodes/*/handler/grpc/**`、muduo_grpc 服务端和 Agones。

**不在范围内**
- 路由模式出站 `cpp/nodes/gate/handler/rpc/router_muduo_forward.cpp` 的 `SendForward`。它是手写的，自带回调，不走生成客户端，也不走全局处理器（只在步骤 5 改注释）。把它改成“按 seq 认领、删掉 rpc_reply_bridge 的回显解析”是另一批。
- Go 侧的响应头回显，以及 proto 的 `correlation_id` 字段。都保留，字段号不动。
- 按 D61 删直连模式、清理部署脚本的 `-GateRouterMode`。只有负责人在 D1 选 A 才做；即使选 A，脚本也另起一批。
- 删 EnterScene 旧版兼容分支（D6）、号段改成按种类并行（D8）。
- `SetGrpcCallDeadline`、`Set<File>CallDeadline`、`grpc_init_client.{h,cpp}`：全部保留。
- Java 版：**本清单自己的改动**（生成器、全局处理器、gate 接线）是服务端内部实现，不涉及。**步骤 1 合入的 blissful 有客户端可见变化**，Java 侧状态引用 blissful 自己的 PARITY 登记，见 §1 第 12 条。
- 不 commit、不 push、不打 tag（AGENTS §9）。

---

## 1. 与任务起点描述不一致的地方（已读代码核实）

1. **已知清单漏了 gate 直连模式的 `Login.Disconnect`。** `client_message_processor.cpp` 的 `HandleConnectionDisconnection` 里，`else` 分支（读取时 L526）调的是 typed `metaKeys/metaValues` 重载，同样依赖 gate main 里的 `SetIfEmptyHandler` / `SetIfEmptyFailedHandler`。
2. **`GrpcReplyContext` 还有一个隐藏用户**：`cpp/libs/engine/session/system/session.{h,cpp}` 的 `GetSessionDetailsByReplyContext(const GrpcReplyContext&)`，唯一调用方是 gate `main.cpp:351`。要删 `GrpcReplyContext`，得先删它。
3. **`RpcMethodMeta.sender` 不在手写头里。**
   - 它由 `tools/proto_generator/protogen/internal/template/service_header.tmpl` 生成到 `cpp/generated/rpc/service_metadata/rpc_event_registry.h`；前向声明和绑定由 `service_register_info.go`（读取时 L535-547）拼好，再经 `rpc_event_registry.cpp.tmpl` 的 L13-15 输出。
   - 共 197 条，包括 etcd 的流式方法 `SendWatchWatch` / `SendLeaseLeaseKeepAlive`。这两条只是靠模板里的“流式拒绝桩”才链接得上。
   - 绑定的是不带回调的 `Message&` 重载。全仓唯一用它的地方是 gate 的 `HandleGrpcNodeMessage`（读取时 L883）。
4. **`cpp/tests/rpc_controller_test/grpc_client_local_reject_test.cpp` 是未跟踪文件，`git grep` 搜不到。** 它是全仓唯一装全局处理器的测试，用例 2 和用例 3 锁的是旧契约。
5. **blissful-morse-969d44 不是“正在改”，而是已经写完。**
   - 未提交、未编译，13 个文件，+488/-173，13 个文件在该 worktree 里都是 ` M`，没有未跟踪的新文件。
   - 基线是 `526780b4e`，落后 HEAD 4 个提交，看不到第 3 批的改动。它用的 `SendSceneManagerCreateScene(…, GrpcDone, GrpcCallOptions)` 签名与主工作区现有生成物一致。
   - 新增 **8 条** `MirrorCreateInFlightEcs.*` 用例。
   - 主工作区 [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) §7 已把它记为“已关闭（未编译）”。
6. **EnterScene 有 5 条发起路径，但只有一个出口。** 换图 C2S、队伍跟随、镜像自动进场、交接、疏散/排空，全都经过 `player_lifecycle.cpp` 的 `SendCorrelatedEnterScene`。所以只改这一处发送点和两个 `Dispatch*`，`player_scene_handler.cpp` 和 `player_team.cpp` 不用动。
7. **生产代码目前没有地方调用每次调用回调重载**，只有上面那个测试和 blissful（未合入）在用。battle 等其他节点也不调用任何生成的 `Send`。
8. **`grpc_init_total.cpp.tmpl` 的实际行号和先前调查不一致。** 实际是：前向声明 L15-19（L19 是 `CallDeadline`），总广播 L23-45，`SetGrpcCallDeadline` L47-54。先前写的 38-41 / 46-68 不对。
9. **`grpc_call_deadline.cpp` 在 `cpp/libs/engine/core/node/system/` 下**，不在 core/network。它直接包含 `grpc_call_tag.h`，所以 `grpc_init_client.h` 去掉对 `grpc_call_tag.h` 的包含不影响它。
10. **HEAD 和工作区不一致，HEAD 本身也不自洽。**
    - HEAD 里的 `cpp/generated/grpc_client/**` 还是旧的 CQ 形状；工作区里是按现行模板重生成的结果（未提交）。
    - HEAD 的 scene `main.cpp` 调了 `InitPlayerInboxReply`，但 HEAD 的 scene 工程没登记 `player_inbox_response_handler.cpp`。登记行是工作区里的未提交改动。
    - **主工作区的 `cpp/tests/cross_zone_test/cross_zone_test.cpp` 是 CRLF**（6143 行，6143 个 `\r`），HEAD 和 blissful 版本都是 LF（`.gitattributes` 设了 `eol=lf`）。
11. **还有几处依赖旧形状，要同步改：**
    - `tools/scripts/tests/k8s_deploy_contract.tests.ps1` L839：正则里有 `Async…Handler`；L845 的正向对照要求手写代码里仍出现 `SendDataServiceAllocateIdSegment`。
    - `docs/ops/cross-zone-failure-test-runbook.md` L293、L956：按 `SceneManager.EnterScene error: player=<P> corr=<c> code=<code> msg=…` 和 `deferred (handoff pending)` 两种日志判演练结果，还写明“先分发应答、后打这一行”。
    - 设计文档 §9.2 的冒烟项依赖 `reply dropped: Async…Handler is not installed` 这条日志。
12. **合入 blissful 本身会改变客户端可见行为。** 旧的 CreateScene 全局处理器在 `error_code != 0` 时只打 `LOG_ERROR`，客户端什么也收不到。blissful 的 `PlayerSceneSystem::HandleMirrorCreateDone` 改成：
    - 建场被拒或 `scene_id == 0`：推 `kEnterSceneFailed`；
    - 建好时玩家在战斗中：推 `kEnterSceneFailed`；
    - 没有可用的 SceneManager、或传输失败：推 `kEnterSceneServerBusy`（3028）；
    - 建场在途期间 `IsSceneChangeBusy` 为真：客户端换图、队伍跟随会被挡回，跨 zone 传送回 3014。

    blissful 的 PROGRESS 增量写明 Java 侧“无改动、原本就对”，已在 Java `PARITY.md` 追加一行，并记了已知差异：建镜像传输失败时 mmorpg 推 3028，Java 推 1003。
13. **`cpp/nodes/scene/CMakeLists.txt` 是生成物。** 它由 `tools/archived/vcxproj2cmake.py` 从 vcxproj 生成，`build_linux.sh` 第 2 步默认会覆盖它，脚本头写着“never hand-edit them”。但这个文件已被跟踪，以前也手工补登记过（L107 的注释）。本清单仍按字符串删同样两行，保持已跟踪文件一致；Linux 验证用默认参数（会按 vcxproj 重生成），顺带验证 vcxproj。

---

## 2. 需要负责人先拍板

**负责人没有答复时，Codex 按“默认”一列执行。**（批次授权见 §0，不属于可以默认的项。）

| # | 问题 | 选项 | 默认 |
|---|---|---|---|
| D1 | gate 直连模式怎么办（通用转发 `rpcHandlerMeta.sender` 和直连 `Login.Disconnect`） | **A**：先按 D61 删掉直连模式，同批删 `sender` 成员、`Message&` 重载、`SenderDeclarations`（见附录 A）。D61 的硬前置是“K8s 路由模式下跑通 battle-smoke”，还没满足（PROGRESS 记为待定），需要负责人豁免或先补跑。<br>**B**：现在把 `sender` 换成带类型擦除回调的签名，gate 两处都传 done，等 D61 时整体删掉。<br>A、B 都超过 30 个文件，都要批次授权。 | **B**。A 需要负责人豁免硬前置，Codex 无权替负责人决定。如果负责人豁免，推荐 A：直连模式现在已经基本不能用（match 回包全丢，回执不带请求 id，chat / guild / friend 等不可达），B 是一次性投入。 |
| D2 | done 为空怎么处理 | a：在 `Start<Svc><Method>Call` 开头 `LOG_FATAL`（与 RPC0 相同）<br>b：打 `LOG_ERROR` 后丢弃<br>c：再生成一个 `std::nullptr_t` 的 `= delete` 重载 | **a，不加 c**。删掉无回调重载后，编译器已经拦住绝大多数误用；运行期出现空 `std::function` 是调用方 bug。代价是 scene 进程会直接退出，所以每一步都要跑到冒烟。 |
| D3 | 要不要加 fire-and-forget 辅助（`GrpcLogFailureOnly` 之类） | 加 / 不加 | **不加**（YAGNI）。迁完之后没有“不要应答”的调用方，直连 Disconnect 的 done 写成内联 lambda 即可。也不给 `grpc_call_tag.h` 引入 muduo Logging。 |
| D4 | `GrpcReplyContext` / `GrpcCallFailure` 删不删 | 删 / 留 | **删**，在步骤 6 做。“失败 = 结果未知”那段注释挪到 `GrpcCallOutcome` 上。保留：`GrpcCallOutcome`（含 `initialMetadata`、`FindInitialMetadata`）、`GrpcMetadata` / `GrpcSentMetadata`、`FindGrpcMetadata`、`kGrpcStatus*`、`GrpcDone`、`GrpcCallOptions`。 |
| D5 | 生成的调用状态要不要瘦身 | 去掉请求副本和 `sentMetadata` / 保留 | **去掉**。它们只为交还给 FailedHandler；请求在 `Start` 里同步序列化。前提是“同步序列化”，由步骤 6.6 新增的模板断言守住。将来改成异步序列化，要先把副本加回来，这句写进模板注释。 |
| D6 | EnterScene 旧版兼容分支（回显号为 0 的 `kLegacy*`、`reply_uncorrelated`、`DecideReplyClaim` 按 player_id 兜底） | a：本批原样保留<br>b：本批一起删 | **a**。改成闭包捕获后，生产中 requestCorrelationId 恒不为 0，这些分支只剩单测会走（连旧版 SM 不回显时也走不到，见 2.2）。删除牵涉跨 zone 契约文档，另开 PR。 |
| D7 | 收件箱 / EnterScene 的回显校验 | a：只认捕获值<br>b：以捕获值为准；**逐字段**判断，回显非 0 且与捕获值不同就 `LOG_ERROR` 并丢弃<br>c：不一致就 Halt 该域 | **b，两个域都丢弃（fail-closed）**。Go 侧每条返回路径都会回填回显：EnterScene 在 `enterscenelogic.go` 的 defer 里回填，包括去重重放；收件箱在 `store_consume.go:105/302`、`player_inbox_server.go:62/78` 回填。所以不一致只可能是缺陷。旧写法按回显比号，这类应答必然落进 kUnmatched / 找不到在途记录而被丢，丢弃与旧语义一致。回显为 0 不算不一致，按捕获值处理。 |
| D8 | 号段的跨种类单飞和 10s 迟到窗口 | 保留 / 改成按种类并行 | **保留**。只把归属方式从 metadata 回显 / FIFO 换成闭包捕获的 requestSequence。 |
| D9 | `sender` 的生成范围 | 只跳过流式方法 / 再收窄到客户端协议方法 | **只跳过流式方法**（这一步必须做）。 |
| D10 | 类型擦除重载的命名和回调类型 | 与 typed 重载同名 / 另起名 | **另起名**：`Send<Svc><Method>Erased` 加 `GrpcMessageDone`。`rpc_event_registry.cpp` 用函数名初始化 `std::function`，名字一旦对应重载集就编不过；用独立名字一次解决。 |
| D11 | 日志里的方法名从哪来 | 生成 `k<Svc><Method>MethodName` / 调用方写字面量 | **调用方写字面量**。EnterScene 必须保持 runbook 依赖的日志格式（§1 第 11 条）。 |
| D12 | AGENTS.md §8 第 146 行（“`rpc_replies/` 只放 gRPC 异步应答适配…”） | 改 / 不改 | **Codex 不改**。改 AGENTS.md 需要负责人同意。`.github/copilot-instructions.md:191`、`cpp/AGENTS.md:24,39`、`cpp/README.md:48` 跟着 AGENTS 走，也先不改，在 PROGRESS 记“待批”，拟稿见附录 B。另外，这一行在本次改动之前就已过时：玩家回包链第 6 批已删，`On<Domain><Method>Reply` 命名也没人在用。 |
| D13 | blissful-morse 怎么合入 | a：**逐文件三方合并，以主工作区为 ours**（`git merge-file`，步骤 1）<br>b：该会话先 rebase 再交付 | **a**。前提是负责人确认那个会话已经收尾。**不用 `git apply --3way`**：它隐含 `--index`，而主工作区里这些文件都是未暂存修改，会报 `does not match index`、整份打不上；即使能打，ours 侧也取自 index（即 HEAD），会无视别的会话的未暂存改动。 |

---

## 3. 硬约束与通用命令

- **不手改 `cpp/generated/**`。** 唯一例外是 `cpp/generated/grpc_client/grpc_call_tag.h`，文件头写明它是手写例外。其余生成物只能通过 proto-gen 重生成。
- **MSBuild 一律串行 `/m:1`**，并发会报假的 C1041 / LNK1104。
- **不 `git commit`**（除非负责人明确要求），**不 `git push` / `git tag`**。**不 `git add`、不 `git stash`**，不为让 index 一致而暂存别人的改动。
- **不设置 `PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE` / `PROTOGEN_ALLOW_MASS_MESSAGE_TOMBSTONE`** 来强行放行墓碑门禁。生成被门禁拦下就停下报告。
- **proto-gen-run 用 no-unity 配置**，不写同级的 `../mmorpg-client`。
- **主工作区有多个会话并行。** 改任何文件之前先看 `git diff -- <file>`，在别人的未提交改动上叠加，不回退、不顺手整理。工程文件按字符串删行，不按行号删。
- **失败时不连续重试。** 保留第一条 `error C` / `LNK` / `--- FAIL` 及前后 20 行，停下报告。
- **回滚。** Codex 不自行提交。每一步结束时：
  - 执行 `git diff -- <本步文件> > <仓库外草稿目录>/stepN.patch` 留档，回滚用 `git apply -R`；
  - 步骤 1 另按命令里的 `pre/` 副本回滚；
  - 新建或删除的文件在交付说明里逐一列出；
  - 生成物的回滚方式：先回滚模板，再重跑 C-REGEN；
  - 负责人愿意的话，每步之后提交一次最省事。

**通用命令**

```powershell
# C-GO:生成器单测
cd tools/proto_generator/protogen; go test ./internal/... ./cmd -count=1

# C-REGEN:重生成(仓库根目录,两条依次执行)
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml

# C-BUILD:编译全解(仓库根目录)
msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64

# C-TEST <name>:编译并运行单个测试工程
# rpc_controller_test 不在 game.sln 里,只能这样编
pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter <name>

# C-LINUX:Linux 构建(仓库根目录,在 Linux 主机或构建容器里)
# 不加 --skip-generate:CMakeLists.txt 按 vcxproj 重生成,同时验证 vcxproj
bash tools/scripts/build_linux.sh --debug
```

**通过标准**：失败项不比步骤 0 记录的基线多。C-LINUX 没有环境就不跑，交付说明里写“Linux CMake 未验证”。

---

## 4. 分步改动

步骤之间的依赖：
- 步骤 1 必须在步骤 2 之前，两者都改 `scene_manager_response_handler.cpp` 和 `cross_zone_test.cpp`。
- 步骤 2、3、4、5 的代码改动彼此独立。每一步同批完成“迁调用点 + 删对应的处理器 / 安装调用 + 改工程登记”，保证单独可编译。唯一例外：3.4 删 `rpc_replies/` 目录依赖步骤 2 已完成。
- 步骤 6 必须等 1 到 5 全部完成。
- 步骤 7 放在 6 之后。
- 批次：A = 步骤 0–4，B = 步骤 5，C = 步骤 6，D = 步骤 7（§0）。

### 步骤 0：前置核对与基线（批 A）

1. 执行 `git status --short` 和 `git log -5 --oneline`，逐条确认 §0 的前置条件，包括批次授权。
2. 确认以下会话已经收尾：
   - 改 `client_message_processor.cpp`、`client_request_lane.{h,cpp}`、`router_lane_test.cpp` 的 gate 并行会话（10-16 04:30 前后还在改）；
   - blissful-morse 会话；
   - RPC0 第 7 批会话。
3. 跑一遍 C-GO、C-BUILD，以及 C-TEST `cross_zone_test`、`bag_test`、`currency_test`、`rpc_controller_test`，把失败项清单记为基线。C-BUILD 编不过就停下（说明第 3 批没编过）。有 Linux 环境的话，再跑一次 C-LINUX 记基线。

### 步骤 1：CreateScene，合入 blissful-morse（批 A）

**1.1 逐文件三方合并**（Git Bash，仓库根目录；`D` 指向仓库外的草稿目录）

```bash
B=.claude/worktrees/blissful-morse-969d44
D=<仓库外草稿目录>/blissful
EXCL=cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp
mkdir -p "$D"
git -C "$B" diff --name-only | grep -vx "$EXCL" > "$D/files.txt"     # 应为 12 个
while read -r f; do
  for sub in pre base ours theirs; do mkdir -p "$D/$sub/$(dirname "$f")"; done
  cp "$f" "$D/pre/$f"                                     # 回滚用:合并前主工作区原样
  git show 526780b4e:"$f" | tr -d '\r' > "$D/base/$f"
  tr -d '\r' < "$f"    > "$D/ours/$f"                     # 主工作区 cross_zone_test.cpp 是 CRLF,统一成 LF
  tr -d '\r' < "$B/$f" > "$D/theirs/$f"
  opt=; [ "$f" = docs/PROGRESS.md ] && opt=--union
  git merge-file $opt -L main -L base -L blissful "$D/ours/$f" "$D/base/$f" "$D/theirs/$f"
  echo "rc=$? $f"                                          # rc>0 = 冲突块数,rc<0 = 出错
done < "$D/files.txt"
```

- `rc>0` 的文件在 `$D/ours/<f>` 里手工解决冲突。处理原则：
  - 文档两边都保留；
  - `player_lifecycle.{h,cpp}` 附近别的会话的改动（存盘序号围栏、`SavePlayerForAssetCommit`）一律保留。
- 写回之前逐个确认 `cmp "$f" "$D/pre/$f"` 相同，即合并期间没有别人又改过这个文件。不同就对该文件重做 1.1。
- 确认后执行 `cp "$D/ours/$f" "$f"`。写回是 LF；`.gitattributes` 规定 `eol=lf`，git 视角无差异。
- 回滚：`cp "$D/pre/$f" "$f"`。

**补丁内容**
- `cpp/libs/services/scene/player/system/player_scene.{h,cpp}`：`RequestEnterMirrorScene` 先过 `IsSceneChangeBusy`，再创建 `PlayerMirrorCreateInFlightComp{token,…}`，然后用 GrpcDone 重载发出。lambda 只按值捕获 `playerId` 和 `token`，转给新增的 `PlayerSceneSystem::HandleMirrorCreateDone`。
- `player_ownership_comp.h`：新增组件定义（文件本身已存在）。
- `player_lifecycle.{h,cpp}`：`IsSceneChangeBusy` 也判断这个新组件。
- `player_scene_handler.cpp`、`player_feature_snapshot.cpp`：只改注释。
- `cross_zone_test.cpp`：新增 8 条用例：`BlocksSceneChangesUntilTtl`、`ZoneTravelIsRefusedWhileMirrorCreateIsInFlight`、`RequestRefusedWhileAnotherSceneChangeIsInFlight`、`DoneClaimsOnlyItsOwnToken`、`RejectedCreateReleasesTheSlot`、`CreatedWhileInBattleDoesNotAutoEnter`、`CreatedWithoutSceneManagerReleasesTheSlot`、`UncorrelatedEnterSceneReplyLeavesMirrorCreateAlone`。
- 4 份文档：`docs/PROGRESS.md`、`client-request-ordering.md`、`ecs-component-access-rules.md`、`scene-creation-architecture.md`。

**1.2 手工部分**
- 在主工作区的 `cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp` 里，删掉从 `// CreateScene response handler.` 注释开始、到 `AsyncSceneManagerCreateSceneFailedHandler = …;` 语句结束的整段（读取时 L77-202）。
- 保留 EnterScene 的两段和 `DescribeGrpcFailure`。只给 CreateScene 用的 include 按编译结果删。
- 可选：把 blissful 测试里的 `outcome.code = 4` 改成 `kGrpcStatusDeadlineExceeded`。

**验证**
- C-BUILD。
- C-TEST `cross_zone_test`：新增的 8 条 `MirrorCreateInFlightEcs.*` 全部通过，其余与基线一致。如果 `CreatedWithoutSceneManagerReleasesTheSlot` 的前置断言失败，说明前面的用例泄漏了假 SceneManager 节点，单独报告。

### 步骤 2：EnterScene（批 A）

**2.1 `cpp/libs/services/scene/player/system/player_lifecycle.cpp`，`SendCorrelatedEnterScene`（读取时 L469-487）**

- 现状：调三参 `SendSceneManagerEnterScene(smRegistry, smEntity, req)`，应答交给全局处理器。
- 改成：

```cpp
req.set_correlation_id(correlationId);   // 保留:Go 侧照常回显、写日志
auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req,
    [playerId = req.player_id(), correlationId](const GrpcCallOutcome &outcome,
                                                const ::scene_manager::EnterSceneResponse *reply)
    {
        if (outcome.ok() && reply != nullptr)
        {
            PlayerLifecycleSystem::DispatchEnterSceneReply(playerId, correlationId, *reply);
            LogEnterSceneReply(playerId, correlationId, *reply);   // 先分发、后打日志(runbook 依赖这个顺序)
            return;
        }
        const uint32_t code = outcome.ok() ? kGrpcStatusInternal : outcome.code;
        PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(
            playerId, correlationId,
            "SceneManager.EnterScene code=" + std::to_string(code) + " msg=" + outcome.message);
    });
```

- 新增 `LogEnterSceneReply(Guid playerId, uint64_t correlationId, const ::scene_manager::EnterSceneResponse&)`，放在同一个匿名命名空间里。内容逐字照搬 `scene_manager_response_handler.cpp` 里 EnterScene 应答处理器的日志段：
  - 错误码 18（`kSmErrHandoffPending`）打 INFO：`SceneManager.EnterScene deferred (handoff pending)`；
  - 其他非 0 错误码打 ERROR：`SceneManager.EnterScene error: player= corr= code= msg=`；
  - 带 redirect 的打 INFO。
  - 日志里的 player / corr 用捕获值。
- **注意**
  - `correlationId` 必须在函数前面“0 修正成新号”之后再捕获。
  - 不要捕获 `smEntity`、玩家实体、`req` 的引用或任何组件指针。疏散 / 排空发完请求后实体会立刻销毁；节点摘除时，done 在实体销毁之后才执行。
  - 函数上方的两段注释要改写：
    - L446 一带的“应答只回到发出它的那条线程的完成队列”，改成“完成回调在发出它的 loop 线程上执行，闭包里带着本次发送的号”；
    - L461-468 的“DispatchEnterSceneReply 才能把应答号为 0 读成旧版 SM”，改成“关联号由闭包捕获交给 DispatchEnterSceneReply，回显只作核对”。

**2.2 同一文件，`PlayerLifecycleSystem::DispatchEnterSceneReply`（读取时 L4854）**

- 签名改为 `(Guid playerId, uint64_t requestCorrelationId, const ::scene_manager::EnterSceneResponse& resp)`。原来读 `resp.player_id()` / `resp.correlation_id()` 的地方改成读参数。
- `playerId == 0` 时打 `LOG_ERROR`（调用方 bug），然后返回。替换原来那条“更老的 SM 不回显 player_id”的 `LOG_DEBUG`。
- **新增回显核对（D7 b，fail-closed）**：在查实体之前判断。
  - 条件：`resp.player_id()` 非 0 且不等于 `playerId`，或 `resp.correlation_id()` 非 0 且不等于 `requestCorrelationId`。
  - 满足时：打 `LOG_ERROR "[EnterSceneReply] echo mismatch player=<捕获> corr=<捕获> echoed_player=<> echoed_corr=<> error_code=<>"`，执行 `travel_handoff_stats::Inc(travel_handoff_stats::Get().replyUnmatched)` 和 `EnsureTravelHandoffStatsTimer()`，然后**返回**。
  - 普通换图靠在途 TTL 收敛，交接靠应答看门狗和冻结上限收敛，结果与旧写法丢弃时相同。
- 回显为 0 不算不一致。这样连旧版 SM（不回显）的应答也按捕获号精确路由，不再落 legacy 分支。生产中 `reply_uncorrelated` 计数和那条每线程一次的 WARN 不会再出现。这是改进，不是回归，要写进 PROGRESS。
- 以下全部原样保留：`requestCorrelationId == 0` 分支（D6，只剩单测会走）、`Classify`、等待者号比对、先抄后摘、`ClaimRelocateEnterSceneReply(playerId, requestCorrelationId, resp)`、`IsSuspiciousUnmatched` 和全部日志。
- **号比对不能删**：TTL 过期后被顶替的旧请求、上一代交接的请求，它们的 done 照样会到，必须靠 kUnmatched 丢掉。

**2.3 同一文件，`DispatchEnterSceneTransportFailure`（读取时 L4971）**

- 签名改为 `(Guid playerId, uint64_t requestCorrelationId, const std::string& reason)`。
- 只把读 `req` 的地方换成读参数，其余不动。

**2.4 `player_lifecycle.h`**

- 两个声明（读取时 L1212-1241）改成新签名。
- 注释改为：“由 `SendCorrelatedEnterScene` 的每次调用回调调用（单测直接调）；requestCorrelationId 是发送时捕获的关联号，生产中恒不为 0；回显非 0 且与捕获值不同即丢弃”。删掉“req 是生成的客户端保存的发出请求”。L1221 的“更老的 scene_manager 连 player_id 都不回显 → LOG_DEBUG”改为“player_id == 0 是调用方 bug → LOG_ERROR”。
- L182-186 的 `reply_uncorrelated` 说明改为：“生产走不到（关联号来自闭包捕获），只剩单测和绕过统一出口的调用”。
- L824-843 的 `enter_scene_reply` 头注释补一句：“关联号的来源已从回显改为闭包捕获；号比对保留，用来防迟到或被顶替的完成；回显只作核对，不一致即丢弃”。

**2.5 `cpp/tests/cross_zone_test/cross_zone_test.cpp`**

在匿名命名空间加两个辅助函数：

```cpp
void DeliverEnterSceneReply(const ::scene_manager::EnterSceneResponse &r)
{ PlayerLifecycleSystem::DispatchEnterSceneReply(r.player_id(), r.correlation_id(), r); }
void DeliverEnterSceneTransportFailure(Guid playerId, uint64_t tag, const std::string &reason)
{ PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(playerId, tag, reason); }
```

- 所有 `DispatchEnterSceneReply(MakeReply(...))` 改成 `DeliverEnterSceneReply(MakeReply(...))`。
- 所有 `DispatchEnterSceneTransportFailure(MakeFailedRequest(p, c), reason)` 改成 `DeliverEnterSceneTransportFailure(p, c, reason)`。
- 删掉 `MakeFailedRequest`。
- 约 22 + 8 处，含 blissful 新增的 `UncorrelatedEnterSceneReplyLeavesMirrorCreateAlone`，以编译报错为准。现有断言不改。
- **新增 3 条用例（必做）**，放进现有 EnterScene 应答相关的 `*Ecs` 套件，夹具照抄同套件里“正常换图应答”的用例：
  1. 发起一次换图（号 T），调 `PlayerLifecycleSystem::DispatchEnterSceneReply(playerId, T, resp)`，其中 `resp.correlation_id = T + 1`。断言在途换图记录仍在，且 `travel_handoff_stats::Read().replyUnmatched` 加 1。
  2. 同上，但 `resp.player_id` 是另一个非 0 值。断言同上。
  3. 同上，但 `resp.player_id = 0`、`resp.correlation_id = 0`（模拟旧版 SM 不回显），调用时 requestCorrelationId 传 T。断言按 T 正常收尾（在途记录被摘），`replyUncorrelated` 不变。

**2.6 删除处理器文件并改工程登记**

- 删除 `cpp/nodes/scene/rpc_replies/scene_manager_response_handler.{h,cpp}`。
- `cpp/nodes/scene/main.cpp`：删 `#include "rpc_replies/scene_manager_response_handler.h"`（L9），删 L124-137 的说明块和 `InitSceneManagerReply();`。
- `cpp/nodes/scene/scene.vcxproj`：删 `<ClCompile Include="rpc_replies\scene_manager_response_handler.cpp" />` 和 `<ClInclude Include="rpc_replies\scene_manager_response_handler.h" />`。
- `cpp/nodes/scene/scene.vcxproj.filters`：删同样两行。
- `cpp/nodes/scene/CMakeLists.txt`：删 `rpc_replies/scene_manager_response_handler.cpp`（§1 第 13 条）。
- **注意**：不能先于 2.1 删，否则应答会落进生成代码的“reply dropped”分支被丢掉。

**验证**
- C-BUILD。
- C-TEST `cross_zone_test`：`EnterScene*Ecs`、`EnterSceneTransportFailureEcs.*`、`MirrorCreateInFlightEcs.*`、改派相关用例与基线一致；新增的 3 条通过。
- `git grep -n -E 'InitSceneManagerReply|scene_manager_response_handler' -- cpp` 应为空。

### 步骤 3：收件箱 Fetch / Ack（批 A）

**3.1 `cpp/libs/services/scene/player/system/player_inbox_system.cpp`，`DefaultSendFetch` / `DefaultSendAck`（读取时 L84-106）**

改调 GrpcDone 重载：

```cpp
player_inbox::SendPlayerInboxFetchItems(tlsNodeContextManager.GetRegistry(common::base::PlayerInboxNodeService), node, request,
    [playerId = request.player_id(), domain = static_cast<uint32_t>(request.domain()),
     correlationId = request.correlation_id()](const GrpcCallOutcome &outcome,
                                               const player_inbox::FetchPlayerInboxResponse *reply)
    {
        if (outcome.ok() && reply != nullptr)
        {
            PlayerInboxSystem::OnFetchReply(playerId, domain, correlationId, *reply);
            return;
        }
        PlayerInboxSystem::OnFetchFailed(playerId, domain, correlationId,
                                         outcome.ok() ? kGrpcStatusInternal : outcome.code, outcome.message);
    });
```

- Ack 写法相同。
- `SendFetchFn` / `SendAckFn` 的签名 `bool(*)(const Req&)` 不变，测试注入点照旧可用。
- done 在 loop 线程上执行，与 Tick 串行，不需要加锁。

**3.2 同一文件，`OnFetchReply` / `OnFetchFailed` / `OnAckReply` / `OnAckFailed`（读取时 L1070 / L1150 / L1188 / L1287）**

- 新签名：

```cpp
static void OnFetchReply(uint64_t playerId, uint32_t domain, uint64_t correlationId, const player_inbox::FetchPlayerInboxResponse &response);
static void OnFetchFailed(uint64_t playerId, uint32_t domain, uint64_t correlationId, uint32_t grpcCode, const std::string &detail);
static void OnAckReply(uint64_t playerId, uint32_t domain, uint64_t correlationId, const player_inbox::AckPlayerInboxResponse &response);
static void OnAckFailed(uint64_t playerId, uint32_t domain, uint64_t correlationId, uint32_t grpcCode, const std::string &detail);
```

- `FindLiveState` 和号比对改用参数。原来的 `response.correlation_id() == 0` 判断改成 `correlationId == 0`。
- 保留 phase 判断和 `state->correlationId == correlationId`：在途超时（InflightTimeoutMs）之后 Tick 会重发，实体也可能重建，旧调用的 done 必须丢掉。
- 两个应答函数在找实体之前加**逐字段**回显核对（D7 b）：

```cpp
if ((response.player_id() != 0 && response.player_id() != playerId) ||
    (response.domain() != 0 && static_cast<uint32_t>(response.domain()) != domain) ||
    (response.correlation_id() != 0 && response.correlation_id() != correlationId))
{
    LOG_ERROR << "[PlayerInbox] Fetch reply echo mismatch …";   // Ack 同理;带上捕获值与回显值
    return;   // 不动在途状态,由 Tick 的在途超时收敛
}
```

- **行为变化（有意为之）**：不带回显（三项为 0）的应答，以前一律丢弃，现在按捕获的调用身份处理。
- fence、tip、退避、Halt 分支一行不动。

**3.3 `player_inbox_system.h`**

- L20-31 契约：“回调只认 player_id”改为“回调按值携带 player_id / domain / correlation_id，不捕获实体句柄；找到实体后仍核对号和 phase；回显非 0 且与捕获值不同即丢弃”。删掉 L31（回包适配在 rpc_replies…）。
- L62-69：四个声明改成新签名，注释改为“由默认传输的每次调用回调转进来（单测可直接调）”，“GrpcCallFailure 的语义”改为“非 OK 即结果未知”。
- L83-86 补一句：“默认传输自带完成回调”。

**3.4 删除处理器文件并改工程登记**

- 删除 `cpp/nodes/scene/rpc_replies/player_inbox_response_handler.{h,cpp}`。
- `main.cpp`：删 L10 的 include 和 L140 的 `InitPlayerInboxReply();`；上方注释改成只描述 `PlayerInboxSystem::Initialize()`。
- `scene.vcxproj`、`.filters`、`CMakeLists.txt`：按字符串删对应的行。
- **只有在步骤 2 已完成时**（目录下四个文件都已删除），才删除已经空了的 `cpp/nodes/scene/rpc_replies/` 目录。生成器已不再往这个目录写文件。

**3.5 新增收件箱应答测试（必做）**

- 新建 `cpp/tests/currency_test/player_inbox_reply_test.cpp`，在 `currency_test.vcxproj` 里紧挨 `player_inbox_rules_test.cpp` 加一行 `<ClCompile Include="player_inbox_reply_test.cpp" />`。该工程没有 `.filters` 文件。`currency_test` 已链接 `scene.lib`，`player_inbox_system.cpp` 就在这个库里。
- 夹具：
  - `SetTransportForTest` 注入假的 sendFetch / sendAck，记录请求并返回 true；
  - `SetNowFnForTest` 注入可控时钟；
  - `SetPersistFnForTest` 注入假存盘；
  - `SetConsumerConfigForTest(true, true, …)`；
  - 玩家实体经 `OnPlayerLanded` 落地（按需组件唯一的创建入口）；
  - 调 `Tick()` 发出 Fetch，从记录的请求里取 correlationId。
  - 前置闸（设计 §7.5）需要的条件在夹具里满足；搭不起来就停下，报告具体缺什么，不要跳过。
- 至少覆盖：
  1. 回显 player_id、domain、correlation_id 各有一项与捕获值不同时：丢弃，phase 仍是 kFetching，correlationId 不变；
  2. 捕获的 correlationId 已被在途超时重发顶替：丢弃；
  3. 回显三项全为 0：按捕获值正常处理（phase 回到 kIdle），锁住有意的行为变化；
  4. `OnFetchFailed` / `OnAckFailed`：释放在途状态并进入退避（推进时钟前不再发送）。

**验证**
- C-BUILD。
- C-TEST `currency_test`：与基线一致，新增用例通过。
- 有 Linux 环境时跑 C-LINUX（步骤 2、3 删了 CMake 登记行）。

### 步骤 4：号段 AllocateIdSegment（批 A）

**4.1 `cpp/nodes/scene/id_segment_bootstrap.cpp`**

`IdSegmentTransport::Send`（读取时 L112-167）选完节点、组好请求后，先登记再发送：

```cpp
const uint64_t requestSequence = ++nextSeq_;
inflight_ = OutstandingRequest{requestSequence, kind, step, now};
data_service::SendDataServiceAllocateIdSegment(tlsNodeContextManager.GetRegistry(DataServiceNodeService), target, request,
    [requestSequence](const GrpcCallOutcome &outcome, const ::data_service::AllocateIdSegmentResponse *reply)
    { tlsIdSegmentTransport.OnCompleted(requestSequence, outcome, reply); });
```

- 不捕获 `this`：`tlsIdSegmentTransport` 是 thread_local，done 在同一个 loop 线程上执行。
- 必须保留字面量 `data_service::SendDataServiceAllocateIdSegment(`，k8s 契约测试 L845 的正向对照要用它。

新增 `void OnCompleted(uint64_t requestSequence, const GrpcCallOutcome&, const ::data_service::AllocateIdSegmentResponse*)`：
1. 先认领：
   - `requestSequence` 等于 `inflight_->seq`：记下 kind，`reset` 在途记录；
   - 等于 `abandoned_->seq`：记下 kind，标为“已放弃”，`reset`；
   - 都对不上：`++orphanResponses_`，打 WARN 后返回。
2. 成功（ok 且 reply 非空）：调 `tlsGuidSegmentRegistry.Get(kind).OnResponse(reply->error_code(), reply->lo(), reply->hi())`。已放弃的那次也交付，与现状一致。
3. 失败：先拼 `why = "gRPC DataService.AllocateIdSegment code=" + code + " msg=" + message`。
   - 已放弃的那次：只打 WARN，表示释放了共享路径；
   - 否则调 `Get(kind).OnTransportFailure(why)`。

要删除的：
- `kSeqMetadataKey` 及其注释；
- `SeqMatches`、旧的 `OnResponse` / `OnFailure`（L68-69 的声明和 L180、L231 的定义）；
- `#include "utils/encode/base64.h"`；`std::ostringstream` 不再使用的话，连 `<sstream>` 一起删；
- `InitDataServiceReply()` 的定义（读取时 L265-273），以及 `ConfigureGuidSegmentClients` 里对它的调用和注释（L277-280）。

要保留的：
- `inflight_` / `abandoned_` / seq，用来防串号。fetchTimeout 先到时客户端会重试，旧调用的完成随后才到。没有 seq 比对的话，旧调用的失败会把新请求判成失败，旧调用迟到的应答还会撞上 `lo ≥ highWater` 校验，误计 rangeViolations。
- `kLateResponseWindowSec`、单飞。

注释改写（L28-40、L58-63）：单飞不再是为了归属，只是保证同一 biz_tag 的迟到应答先于新请求到达；归属现在按闭包捕获的 requestSequence 精确认领。

**4.2 `cpp/nodes/scene/id_segment_bootstrap.h`**

- 删 `InitDataServiceReply` 的声明（L25）和 L21-24 的注释。
- 文件头 L10-11 改为：“响应 = 每次调用回调带回本次请求编号，分发给发出它的那一种实例”。

**验证**
- C-BUILD。
- C-TEST `bag_test`：`GuidSegmentClientTest.*` 与基线一致（客户端本体没改）。
- 联机时，scene 启动日志里能看到号段首段 Warm 成功。

### 步骤 5：gate（D1 = B）、生成器增量，然后重生成（批 B）

5.1 到 5.4 加上 5.9 必须同批完成。sender 类型、Go 侧前向声明串、模板里的擦除重载、gate 调用点，这四处只要有一处不同步，就会编译失败或报 LNK2019。

**5.1 `cpp/generated/grpc_client/grpc_call_tag.h`（手写例外文件）**：在 `GrpcDone`（L69-70）之后加：

```cpp
namespace google::protobuf { class Message; }
// 类型擦除的完成回调,只给 gRpcMethodRegistry.sender(Send<Svc><Method>Erased)用;契约同 GrpcDone,
// reply 的动态类型是该方法的应答类型。
using GrpcMessageDone = std::function<void(const GrpcCallOutcome&, const google::protobuf::Message* reply)>;
```

**5.2 `tools/proto_generator/protogen/internal/template/grpc_async_client.h.tmpl`**：在 unary 分支里、每次调用声明（L44）之后加一行，不给默认参数：

```
void Send{{ $svc.Service }}{{ $method.Method }}Erased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
```

**5.3 `grpc_async_client.cpp.tmpl`**：在每次调用 `Send` 的定义（L150-153）之后加：

```
void Send{{S}}{{M}}Erased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message,
                          GrpcMessageDone done, const GrpcCallOptions& options) {
    if (!done) {
        LOG_FATAL << "gRPC {{S}}.{{M}} requires a done callback";
    }
    if (message.GetDescriptor() != {{Req}}::descriptor()) {
        LOG_ERROR << "gRPC {{S}}.{{M}}: request type " << message.GetDescriptor()->full_name() << " does not match; not sent";
        QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(kGrpcStatusInternal, "request type mismatch")] {
            done(outcome, nullptr);
        });
        return;
    }
    Start{{S}}{{M}}Call(registry, nodeEntity, static_cast<const {{Req}}&>(message), options.metadata, options.deadline,
        [done = std::move(done)](const GrpcCallOutcome& outcome, const {{Resp}}* reply) { done(outcome, reply); });
}
```

- `{{S}} {{M}} {{Req}} {{Resp}}` 分别对应模板里的 `$svc.Service`、`$method.Method`、`$method.CppRequest`、`$method.CppResponse`。`MakeLocalFailureOutcome`、`QueueGrpcLocalCompletion` 在文件开头 L19-28 的匿名命名空间里，定义点之后可见。
- 旧的 `Message&` 重载（L170-173，不做类型校验的 `static_cast`）本步暂时保留。

**5.4 `service_header.tmpl` L20-26**，改为：

```cpp
std::function<void (
    entt::registry& registry,
    entt::entity nodeEntity,
    const google::protobuf::Message& request,
    GrpcMessageDone done,
    const GrpcCallOptions& options
)> sender;
```

同时加 `#include "grpc_client/grpc_call_tag.h"`（与 `rpc/service_metadata/...` 同一个包含根）。改这个头会触发几乎全量重编。

**5.5 `tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go`**

- L535 的条件改为 `!method.CcGenericServices() && !method.ClientStreaming() && !method.ServerStreaming()`。
- L539-540 的声明串改为 `"void Send" + Svc + Method + "Erased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );"`。
- L546 的 `SenderName` 加 `"Erased"` 后缀。
- L435 的注释改为：“空 = 不带 sender（cc_generic_services 的 muduo 方法和流式方法）”。
- 把“是否生成 sender、前向声明串、绑定名”抽成纯函数，例如 `grpcSenderFor(method, serviceName) (decl, name string, ok bool)`，供单测用。

**5.6 `service_register_info_test.go`**：给 `grpcSenderFor` 加表驱动用例：

| 方法 | 期望 |
|---|---|
| cc_generic | 不生成 sender |
| ServerStreaming / ClientStreaming | 不生成 sender，也不生成前向声明 |
| 有 package 的 unary | 声明 `namespace pkg{void SendSvcMErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}`，绑定名 `pkg::SendSvcMErased` |
| 无 package 的 unary | 全局声明，绑定名 `::SendSvcMErased` |

现有的 `"grpc method with sender"` 用例不用动。

**5.7 `grpc_process_test.go`，`TestGrpcAsyncClientTemplatesUseMuduoChannelAndRejectStreaming`**：加三条正向断言：
- header 包含 `void SendSceneManagerEnterSceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);`；
- header 不包含 `SendSceneManagerWatchErased`；
- source 包含 `GetDescriptor() != ::scene_manager::EnterSceneRequest::descriptor()`。

**5.8 生成**：先跑 C-GO，再跑 C-REGEN。生成后检查：
- `rpc_event_registry.cpp` 的前向声明已换成新签名，`SendWatchWatch` / `SendLeaseLeaseKeepAlive` 不再出现；
- 每个 `*_grpc_client.h` 里的 unary 方法都有 `Erased`。

**5.9 `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`**

- include 区（`client_request_lane.h` 旁边）加 `#include "rpc_reply_bridge.h"`。文件目前没有包含它，只在 L1199 的注释里提到过。

a) `HandleGrpcNodeMessage` 中的 sender 调用（读取时 L883-887）改为：

```cpp
std::string serializedSessionDetails;
GrpcCallOptions options;
if (sessionDetails.SerializeToString(&serializedSessionDetails))
{
    options.metadata.emplace_back(kSessionBinMetaKey, serializedSessionDetails);   // 与现状一致:序列化失败就不带
}
rpcHandlerMeta.sender(tlsNodeContextManager.GetRegistry(rpcHandlerMeta.targetNodeType), *node, *rpcHandlerMeta.requestProto,
    [messageId = request->message_id(), serializedSessionDetails](const GrpcCallOutcome &outcome, const google::protobuf::Message *reply)
    { ForwardDirectGrpcCallResult(messageId, serializedSessionDetails, outcome, reply); },
    options);
```

在匿名命名空间里、`ForwardSceneClientCallResult` 旁边新增静态函数 `ForwardDirectGrpcCallResult`：
- 失败（非 OK，或 reply 为空）：方法名用 `gRpcMethodRegistry[messageId]` 的 `serviceName + "." + methodName` 拼出，调 `gate_rpc_reply_bridge::NotifyCallFailure(rpcMethodName.c_str(), code, outcome.message, serializedSessionDetails.empty() ? nullptr : &serializedSessionDetails)`。
- 成功：`SessionDetails requestSessionDetails; requestSessionDetails.ParseFromString(serializedSessionDetails)`，然后 `DeliverReply(requestSessionDetails, *reply)`。`serializedSessionDetails` 为空（序列化失败）时 `session_id` 为 0，由 `DeliverReply` 现有的“会话已不在就静默丢弃”处理。
- 不捕获 `TcpConnectionPtr`（AGENTS §11.7）。
- 共享原型 `*rpcHandlerMeta.requestProto` 在 Start 里同步序列化，下一条消息覆盖它不影响本次发送（D5，由 6.6 的断言守住）。

b) `HandleConnectionDisconnection` 的直连 `else` 分支（读取时 L522-527）改为：

```cpp
GrpcCallOptions options;
std::string bin;
if (sessionDetails.SerializeToString(&bin)) { options.metadata.emplace_back(kSessionBinMetaKey, std::move(bin)); }
loginpb::SendClientPlayerLoginDisconnect(tlsNodeContextManager.GetRegistry(eNodeType::LoginNodeService), *loginNode, request,
    [sessionId](const GrpcCallOutcome &outcome, const loginpb::LoginEmptyResponse *)
    {
        if (outcome.ok()) return;   // 会话随后就 erase,没有客户端可回
        // 每 1024 次失败采样一条 WARN:session_id / code / msg
    },
    options);
```

**直连模式下客户端可见的行为变化**（写进交付说明）：
- 成功应答改为按“发出时的会话”定位，不再按回显。`DeliverReply` 只用 `session_id` 和 `gate_call_seq`，login 回显的本来就是同一份会话，所以 login 的应答效果不变。match 不回显会话头，它的回包第一次真正送到客户端（原来全丢）。
- Disconnect 失败不会再给“复用了同一 session_id 的新会话”推 tip。
- 其余形状不变：仍不带请求 id，仍按反查表包一层，失败仍回 23 号 tip。

**5.10 gate `main.cpp`**

- 删 L342-367：两段 `SetIfEmpty*` 及其注释。
- 删以下 include，编译报缺哪个就只加回那一个：L18 `handler/rpc/rpc_reply_bridge.h`、L24 `grpc_client/grpc_init_client.h`、L25 `session/system/session.h`。
- **保留 L27 `network/network_utils.h`**：L106 的 `SessionIdGenerator::kSeqMask` 来自这个头。

**5.11 `cpp/libs/engine/session/system/session.{h,cpp}`**

- 删除 `GetSessionDetailsByReplyContext`（h 的 L11-12，cpp 的 L12-20）。
- 删除 cpp L8 的 `kSessionBinMetaKey`（不再使用；`network_utils.h` 里那份保留）。
- 删除 h 里的 `#include "grpc_client/grpc_call_tag.h"`。
- h L14-15 的注释改为：“gate 路由转发的完成回调用它解析响应头里回显的会话”。

**5.12 `cpp/libs/engine/core/network/network_utils.{h,cpp}`**

- 删除 `SerializeSessionDetails`（h 的 L19，cpp 的 L75-84）。5.9 之后已经没有调用方。
- `kSessionBinMetaKey` 保留。
- 删完检查 `network_utils.{h,cpp}` 是否还需要 `session.pb.h`，不需要就去掉，编译报缺再加回。

**5.13 gate 注释同步**（从步骤 7 提前到这里，否则步骤 6 的预检 grep 会命中）

- `cpp/nodes/gate/handler/rpc/rpc_reply_bridge.h`：
  - L4-9 的文件头改为：“直连模式的每次调用回调（`client_message_processor.cpp` 的 `ForwardDirectGrpcCallResult`）和路由模式的 Forward 回调都只调这里”。
  - L25-28 的 `DeliverReply` 契约改为：“sessionDetails：路由模式是路由服在响应头里原值回显的会话；直连模式是发出时捕获的那份（`ForwardDirectGrpcCallResult`）。本函数只用 session_id 与 gate_call_seq。”
- `cpp/nodes/gate/handler/rpc/router_muduo_forward.cpp`：
  - L33 改为“与生成客户端日志里的 `<Service>.<Method>` 同形”；
  - L39-40 删掉“全局应答处理器（GetSessionDetailsByReplyContext 返回空）在这里静默丢包”这一句，只留“缺了说明对端不是预期的路由服版本，这里留一行采样证据”；
  - L84 改为“按**响应头回显**的会话交成功桥（不是本地保存的那份）”。
- `router_muduo_forward.h` L4-10：删掉“它不经全局处理器”；“成功 → 按响应初始 metadata 里回显的会话”后面补一句“直连模式按发出时的会话；DeliverReply 只用 session_id 与 gate_call_seq，两者效果相同”。

**验证**
- C-GO：`TestGrpc*`、`TestRpcMethodMetaInitLines*` 和新增的 sender 用例全部 PASS。
- C-REGEN：没有 panic 或 Fatal。
- C-BUILD。有 Linux 环境时跑 C-LINUX。
- C-TEST `rpc_controller_test`：旧的 local_reject 测试仍能编译，因为全局处理器还在。
- 冒烟：
  - 先 `pwsh tools/scripts/start_game.ps1`（路由模式），在 `robot/` 下运行 `.\robot.exe -c etc/battle_smoke.yaml` 和 `-c etc/ordering_smoke.yaml`；
  - 再 `pwsh tools/scripts/start_game.ps1 -GateRouterMode 0`（直连模式），跑 `battle_smoke`；
  - 日志里不得出现 `requires a done callback`，也不得出现 `reply dropped`。

### 步骤 6：从模板删除全局处理器和无回调重载，重生成，改测试（批 C）

**预检**：下面两条 grep 应该只命中 `grpc_client_local_reject_test.cpp`，否则先回去补迁漏掉的调用方或注释。

```bash
git grep -n -E 'Async[A-Za-z0-9]+(Failed)?Handler|SetIfEmpty|GrpcReplyContext|GrpcCallFailure|FindSentMetadata' -- cpp ':!cpp/generated'
grep -rnE 'Async[A-Za-z0-9]+(Failed)?Handler|SetIfEmpty|GrpcReplyContext|GrpcCallFailure' cpp/tests cpp/nodes cpp/libs --include=*.cpp --include=*.h
```

**6.1 `grpc_async_client.h.tmpl`**：每个方法 region 的最终形状：

```
#pragma region {{ $svc.Service }}{{ $method.Method }}
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view k…GrpcPath = "…";
{{- if or $method.ClientStreaming $method.ServerStreaming }}
// 流式方法不生成任何 Send,也不进 gRpcMethodRegistry.sender;全仓唯一的流式服务 etcd 由 MuduoEtcdTransport 手写。
{{- else }}
// 每次调用回调(契约见 grpc_call_tag.h 的 GrpcDone):done 必填,恰好调用一次、从不在本函数栈内同步调用;为空 LOG_FATAL。
void Send…(entt::registry& registry, entt::entity nodeEntity, const Req& request, GrpcDone<Resp> done, const GrpcCallOptions& options = {});
// 类型擦除版,只供 gRpcMethodRegistry.sender 绑定;message 的动态类型必须是 Req,不符时以 INTERNAL 异步完成、不发出。
void Send…Erased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
{{- end }}
#pragma endregion
```

- 删：L27-35（处理器的 using / extern）、L38-40（三个无回调声明）、L23-25（旧的流式注释）、L53-56（四个 `Set<File>*Handler`）。
- 留：L57-58 的 `Set<File>CallDeadline`。
- include 里的 `<functional> <string> <vector>` 可以删（`grpc_call_tag.h` 已带入）。

**6.2 `grpc_async_client.cpp.tmpl`**

- 删 L5 的 `<algorithm>`；`<memory>` 不再使用的话一起删。
- L20-21 的注释去掉“与应答处理器一样”。
- 删 L41-54 的流式桩：整个方法体用 `{{- if not (or $method.ClientStreaming $method.ServerStreaming) }}` 包起来，流式方法什么都不产出。
- 删 L55-56 的处理器定义。
- 把 L58-101 整段（`…Call` 结构，`Complete…` 及其中的 `reportedMissingHandler` 绊线和无处理器时的 LOG_ERROR）换成：

```
namespace {
void QueueLocal{{S}}{{M}}Failure(GrpcDone<{{Resp}}> done, uint32_t code, std::string message) {
    QueueGrpcLocalCompletion([done = std::move(done), outcome = MakeLocalFailureOutcome(code, std::move(message))] {
        done(outcome, nullptr);
    });
}
}
```

- L103-148 的 `Start{{S}}{{M}}Call`：
  - 签名改为 `(entt::registry& registry, entt::entity nodeEntity, const {{Req}}& request, const GrpcCallOptions& options, GrpcDone<{{Resp}}> done)`；
  - 第一句写 `if (!done) { LOG_FATAL << "gRPC {{S}}.{{M}} requires a done callback (grpc_call_tag.h GrpcDone)"; }`，放在 `FindGrpcChannel` 之前；
  - 直接 `request.SerializeToString(&unaryRequest.message)`，放在任何 `QueueLocal…Failure` / `StartUnaryCall` 闭包之前，并加注释：“请求在这里同步序列化，调用方随后改写（gate 的共享原型）不影响本次；将来改成异步序列化，须先拷贝请求”；
  - metadata 取自 `options.metadata`，逐项 Base64；
  - deadline 用 `options.deadline`，`<= 0` 时用 `DefaultCallDeadline()`；
  - 完成 lambda 改为 `channel->StartUnaryCall(std::move(unaryRequest), [done = std::move(done)](muduo_grpc::UnaryCallResult&& result) {…})`，不捕获 request。非 OK 或解析失败时 `done(outcome, nullptr)`，否则 `done(outcome, &reply)`。
- L150-153 的每次调用 `Send` 改为 `Start…(registry, nodeEntity, request, options, std::move(done));`。
- 步骤 5 加的 `Erased` 改调新的 `Start…(registry, nodeEntity, static_cast<const Req&>(message), options, [done = std::move(done)](…){ done(outcome, reply); });`。
- 删 L155-173 的三个无回调重载，连同 L156 的二义性注释。
- 删 L179-225 的四个 `Set<File>*Handler` 定义，保留 L227-229。

**6.3 `grpc_init_total.h.tmpl`**

- 删 L12-19 的四个总广播。
- 删 L5-7 的 `<functional>`、`<google/protobuf/message.h>`、`grpc_call_tag.h`，保留 `<chrono> <cstdint>`。
- L9-10 的注释改为：“只负责按目标节点类型设置 deadline；应答一律走每次调用回调”。

**6.4 `grpc_init_total.cpp.tmpl`**

- 删 L15-18 的四个处理器前向声明，保留 L19 的 `Set<File>CallDeadline`。
- 删 L23-45 的四个总广播定义。
- 删 L5-6 的 `<functional>` 和 `<google/protobuf/message.h>`。
- L47-54 的 `SetGrpcCallDeadline` 不动（多个互相独立的 `if` 保持原样）。

**6.5 `grpc_call_tag.h`**

- 删 L58-59 的 `GrpcReplyContext` 和 L79-99 的 `GrpcCallFailure`。
- L44 的注释改为：“非 OK 即结果未知：DEADLINE_EXCEEDED / UNAVAILABLE / CANCELLED 都不代表服务端没执行；只做不依赖对端是否执行的事（释放本地在途、提示客户端、按退避重试幂等请求）”，内容取自原 L82-84。
- L54 的注释改为：“按 key 找响应头里的 metadata；没有返回 nullptr。”
- L64 改为：“done 必须非空；为空时生成代码 LOG_FATAL（与 RpcChannel::CallMethod 一致）。没有全局处理器，done 是唯一的完成通道。”
- L72-73 删去“失败时仍按原值交还（与 metaKeys/metaValues 重载一致）”。
- 5.1 加的 `GrpcMessageDone` 保留。

**6.6 `tools/proto_generator/protogen/internal/generator/cpp/grpc_process_test.go`**

`TestGrpcInitTemplateSupportsMultipleServicesForSameNodeType`：
- 删 L126-135。
- L120-121 的 forbidden 列表加上 `"Handler("`、`"FailedHandler"`、`"GrpcReplyContext"`、`"GrpcCallFailure"`、`"google/protobuf/message.h"`。

`TestGrpcAsyncClientTemplatesUseMuduoChannelAndRejectStreaming`：
- L193-195 改成对 header 和 source 都检查的禁用表：`"HandlerFunctionType"`、`"FailedHandler"`、`"IfEmpty"`、`"AsyncSceneManager"`、`"SetSceneManagerServiceHandler"`、`"GrpcReplyContext"`、`"GrpcCallFailure"`、`"metaKeys"`、`"reportedMissingHandler"`、`"reply dropped"`、`"SendSceneManagerWatch"`、`"is a streaming method"`。
- 删 L196-198（原来断言流式方法保留 `Message&` 重载）。
- 删 L209-211。
- 新增正向断言：
  - header 包含 `void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request, GrpcDone<::scene_manager::EnterSceneResponse> done, const GrpcCallOptions& options = {});`；
  - header 不包含三参签名的结尾 `const ::scene_manager::EnterSceneRequest& request);`；
  - source 包含 `requires a done callback`。
- **D5 守卫（新增）**：在生成出的 `StartSceneManagerEnterSceneCall` 函数体里，`request.SerializeToString(&unaryRequest.message)` 必须出现，且位置早于 `StartUnaryCall(` 和第一个 `QueueGrpcLocalCompletion(`（用 `strings.Index` 比较）。source 必须包含 `StartUnaryCall(std::move(unaryRequest), [done = std::move(done)]`，不得包含 `call->request`。

**6.7 `cpp/tests/rpc_controller_test/grpc_client_local_reject_test.cpp`**（未跟踪文件，必须和本步一起交付）

- 文件头 L7-8 的契约改为：“done 是唯一的完成通道（全局处理器已删，见 [grpc-muduo-transport.md §16.1](../../design/grpc-muduo-transport.md#grpc-client-callback)）”。
- 删 `SetUp` / `TearDown`，以及 L75-78 的四个成员（`globalReplyCalls_`、`globalFailedCalls_`、`savedReplyHandler_`、`savedFailedHandler_`）。夹具只留 `loop_`、`registry_` 和 `RunLoopUntilQuiet`。
- 用例 1：删 L135-136；L81 的注释去掉“不触发全局处理器”。
- 用例 2（`WithoutDoneFailedHandlerGetsUnavailableWithSentCopies`）改名为 `PerCallDoneOnEveryChannellessShapeCompletesOnceWithUnavailable`：
  - 三种目标（没挂组件、通道为空指针、已销毁）都改用 `SendDataServiceLoadPlayerData(registry, target, request, done, options)`；
  - `options` 写成 `GrpcCallOptions options; options.metadata = {{"x-test", "snapshot"}};`，不用指定初始化器；
  - 每个目标断言：恰好调用 1 次、`code == kGrpcStatusUnavailable`、`reply == nullptr`、Send 栈内计数为 0、跑完后已排空；
  - 删掉关于 messageId、method、请求副本、sentMetadata 的断言。“调用方事后改写请求不影响发出字节”改由 6.6 的模板断言守住。
- 用例 3（`EmptyPerCallDoneFallsBackToGlobalFailedHandler`）整条删除。理由：D2 = a；Windows 上的死亡测试要起子进程，EventLoop 又是 thread_local，所以不补死亡测试。
- 用例 4：删 L263-264。
- `rpc_controller_test.vcxproj:171` 不用动。

**验证**
- 依次跑 C-GO、C-REGEN、C-BUILD；有 Linux 环境时跑 C-LINUX。
- C-TEST：`rpc_controller_test`（`GrpcClientLocalRejectTest.*` 3 条通过）、`cross_zone_test`、`bag_test`、`currency_test`。
- 编译报错指向“已删除的重载或处理器”的地方，就是漏迁的调用方。按步骤 2 到 5 的写法迁移，**不要把重载加回来**。

### 步骤 7：注释、文档、PROGRESS（批 D）

**代码注释**（只改注释）
- `cpp/libs/engine/core/network/node_utils.cpp` L47、L80、L85：把“进失败处理器 / 走失败处理器”改为“以 UNAVAILABLE（或 DEADLINE_EXCEEDED）完成每次调用回调”；L85 改为指向 `PlayerLifecycleSystem::DispatchEnterSceneTransportFailure`。这个文件里别的会话还有 84 行未提交改动，只动这几行。
- `cpp/libs/services/scene/player/system/travel_freeze_cap.h` L54-58：改为“EnterScene 的每次调用回调以非 OK 完成”。
- `cpp/tests/bag_test/bag_test.cpp` L3505、`cpp/tests/routing_identity_test/scene_manager_selector_test.cpp` L194、L260：同样的改法。
- `tools/proto_generator/protogen/internal/generator/cpp/handlers.go` L31：“rpc_replies 下只剩手写的 gRPC 应答适配与 gate 的空 InitServiceHandler”改为“rpc_replies 下只剩 gate 生成的空 InitServiceHandler（battle 的是手写空壳）”。这是 Go 注释，不影响生成物。
- 提到“回显号为 0 就退回按 player_id”的注释，各补一句：“关联号已改为闭包捕获，生产中 requestCorrelationId 恒不为 0（旧版 SM 不回显时也按捕获号路由）；本分支只剩单测和绕过统一出口的调用”。位置：
  - `relocate_confirm.h` L40-41、L580-582；
  - `player_lifecycle.h` L831-834、L1187、L1209；
  - `player_lifecycle.cpp` L267、L322、L451-452（`tlsWarnedUncorrelatedReply`）、L1856、L1873、L4500、L4869（requestCorrelationId == 0 分支）；
  - `player_ownership_comp.h` L185-196。

**脚本**（可选）
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1` L839：正则收窄为 `'\b(?:Send' + $name + '(?:Erased)?|' + $name + 'MessageId)\b'`，L835 的注释同步。
- `tools/scripts/k8s_deploy.ps1` L953-954 的注释去掉 `` `Async<服务><方法>…` ``。
- 改完跑 `pwsh tools/scripts/tests/k8s_deploy_contract.tests.ps1`。

**文档**（按 AGENTS §5 加带日期的标注，不重写历史段落）

| 文档 | 改什么 |
|---|---|
| [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) | 状态行追加一句。§1 D3 行尾注“已由 §12 在结构上消除”。§3 开头加横幅：3.1 / 3.2 是历史选型，3.3“结果未知”仍然适用。§5 表格后加一行，指向 §12.2。§6 写结论“已关闭”。§9.2 的冒烟项把 `reply dropped…` 换成“不得出现 `requires a done callback`”。§10 第 2、6 条标为作废。§11.2 / §11.3 / §11.4 加勘误。新增 §12，内容见本表之后。 |
| `docs/design/grpc-muduo-transport.md` §14.1 / §14.3 | 以下几段之后各加一条日期注：“全局处理器保留”“流式方法仍生成三个旧 Send 重载”“session 改为 GetSessionDetailsByReplyContext”“gate main 全局处理器换新签名”“号段、收件箱、scene_manager 只迁签名”“测试不触发全局处理器”。 |
| `docs/design/muduo-rpc-channel.md` | §8.4 段末注“已过时”。§9.2 注“gRPC 应答适配也已退场”。§14 第 2 条划掉。§19.2 注“done 是唯一完成通道”。 |
| `docs/design/client-request-ordering.md` §4.5 | 认领表中“成功回包 / gRPC 失败”两行改为：直连指向 `ForwardDirectGrpcCallResult`，路由指向 `router_muduo_forward.cpp` 的 `SendForward` 完成回调。可选：顺手改 L204 的“CQ 轮询”和 L212 的过时说法。 |
| `docs/design/player-inbox-service.md` §7.11、L1262、L1269 | 改成每次调用回调的写法：闭包捕获三项，回显逐字段核对，不一致 ERROR 并丢弃；回显为 0 按捕获值处理。 |
| `docs/design/cross-zone-scene-travel.md` L268、§12.5.1、L893 | 各加一行日期注。 |
| `docs/design/team-system.md` L2163、L2172 | 加日期勘误：`scene_manager_response_handler.cpp` 已删；应答按闭包捕获的 player_id + correlation_id 认领，“只能靠 resp.player_id()”的残余窗口已消除。 |
| `docs/design/enter-scene-zone-routing.md` L27 | 加日期注：调用方表里的 `scene_manager_response_handler.cpp` 已由 `PlayerSceneSystem::HandleMirrorCreateDone` 取代。 |
| `docs/design/player-storage-placement.md` L259 | 加日期注：EnterSceneRequest 构造点里的 `scene_manager_response_handler.cpp` 改为 `player_scene.cpp`（`HandleMirrorCreateDone`），总数不变。 |
| `docs/design/scene-enter-chain-residuals.md` L885 | 未跟踪文件，属于另一会话正在执行的计划。**先问负责人或那个会话**，同意后加注“镜像自动进场已迁到 `PlayerSceneSystem::HandleMirrorCreateDone`”。没得到答复就不改，在 §5 冲突表里登记。 |
| `docs/design/turn-based-battle-server.md` L637、L1249 | 加日期勘误：通用出口现在是 `gate_rpc_reply_bridge::DeliverReply`。 |
| `docs/ops/cross-zone-failure-test-runbook.md` L275、L293、L956 | “失败处理器 / 应答处理器”改为“每次调用回调”。日志格式和“先分发、后打日志”的顺序不变。 |
| `docs/design/scene-creation-architecture.md` | blissful 合入后，把“不再经过 AsyncSceneManagerCreateSceneHandler”改成“全局处理器已删除”。 |
| `docs/design/third-party-login-end-to-end-design.md` L233-234 | 可选。 |

[历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) 新增 §12，内容：
- 动机；
- 删除清单；
- 保留清单；
- §12.2 调用点落点表：EnterScene、CreateScene、AllocateIdSegment、收件箱 Fetch / Ack、gate 直连通用转发与 Disconnect、路由 Forward（本来就自带回调）；
- 回显核对语义：捕获值为准，逐字段核对，不一致丢弃；
- 日志格式约束；
- 测试改动，含 D5 的模板守卫；
- Java 状态：本清单的改动不涉及；CreateScene 的客户端可见变化引用 blissful 的 PARITY 行。

**PROGRESS 与索引**
- `docs/PROGRESS.md` 末尾追加一条（只追加，不改旧条目），内容：
  - 改动范围和实际执行到的批次；
  - D1 到 D13 的实际取值；
  - 直连模式的行为变化（5.9）；
  - **CreateScene（blissful 合入）的客户端可见变化**（§1 第 12 条逐项列出）；
  - EnterScene 改按捕获号路由后，旧版 SM 不回显也能精确认领，`reply_uncorrelated` 在生产中不再计数；
  - “未编译 / 待验证”，或 Codex 的实际结果；
  - AGENTS §8 待批；
  - Java 版：本清单自己的改动不涉及（服务端内部实现，不碰客户端契约）；CreateScene 的变化见 blissful 的 PARITY 登记（Java 0.1.0-SNAPSHOT，已知差异：建镜像传输失败 mmorpg 推 3028、Java 推 1003）。
- 在 `docs/handoff/grpc-muduo-batch3-5/README.md` 和 `docs/README.md` 里各登记一行，指向本文。
- 负责人批准 D12 后，按附录 B 改 AGENTS.md，再同步 `.github/copilot-instructions.md:191`、`cpp/AGENTS.md:24,39`、`cpp/README.md:48`。

---

## 5. 冲突与协作

| 位置 | 谁在改 | 怎么协调 |
|---|---|---|
| `cpp/generated/grpc_client/**`、`grpc_init_client.{h,cpp}`（含未跟踪的 `mail/`、`rank/`） | 第 3 批的重生成结果，未提交 | 只通过重生成覆盖。重生成会把别人在途的 proto 改动一起刷进来，交付说明里注明“差异无法单独归因”。 |
| `service_register_info.go` / `_test.go`、`service_header.tmpl`、`rpc_event_registry.cpp.tmpl`、`rpc_event_registry.{h,cpp}` | RPC0 第 7 批，未提交（约 +600 行） | 等它收口后在上面改，不回退。 |
| `client_message_processor.cpp`、`client_request_lane.{h,cpp}`、`router_lane_test.cpp` | 主工作区的 gate 并行会话（10-16 读取时行号还在变）；worktree practical-bouman-1beb0d、optimistic-bartik-99a599、fervent-northcutt-6c0c12（基线 526780b4e） | 步骤 5 要等主工作区会话收尾。那几个 worktree 只改 lane 和 `BuildSessionDetails`，不碰 sender 和处理器；后落地的一方按补丁重放，只会有文本冲突。 |
| `player_lifecycle.{h,cpp}`、`cross_zone_test.cpp`（主工作区为 CRLF）、`node_utils.cpp`、scene 工程三件套 | 主工作区的其他 scene 会话（存盘围栏、`SavePlayerForAssetCommit`、SM 选点分级、收件箱登记行）；blissful-morse；relaxed-cerf-d5da4d（`player_lifecycle.cpp` 一行注释） | 先按步骤 1 的逐文件三方合并合入 blissful，再做步骤 2。工程文件按字符串删行，不按行号删。 |
| `session.{h,cpp}`、`id_segment_bootstrap.cpp`、`player_inbox_system.cpp`、`router_muduo_forward.*`、`rpc_reply_bridge.h`、gate `main.cpp` | 第 3 批未提交改动 | 在上面叠加。 |
| `grpc_client_local_reject_test.cpp` | 第 3 批新增的未跟踪文件 | `git diff` 看不到它，交付时单独列出，不要漏交或误删。 |
| `docs/design/scene-enter-chain-residuals.md` | 另一会话的未跟踪计划文档 | 不擅自改；按步骤 7 的表先问。 |
| [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md)、`client-request-ordering.md` | 主工作区（M）；sleepy-dhawan-974040 有描述已删除 CQ 代码的过时修改；多个 worktree 在别的段落追加 | sleepy-dhawan 的文档修改和它未跟踪的 `grpc_completion_ok_test.go` 都**不合入**（后者守的是已删的 CQ 模板，会让 go test 失败）。动 §1 / §10 之前请负责人确认那份作废。 |
| `docs/PROGRESS.md` | 多处在末尾追加 | `merge=union`，只追加；步骤 1 用 `git merge-file --union`。 |

---

## 6. 最终验证

**1. 静态 grep**（期望全部为空）

```bash
# 已跟踪文件
git grep -n -E 'Async[A-Za-z0-9]+(Failed)?Handler(FunctionType)?\b|Set[A-Za-z]*IfEmpty(Failed)?Handler|\bSet[A-Za-z]+FailedHandler\b|\bSetHandler\(|GrpcReplyContext|GrpcCallFailure|FindSentMetadata|GetSessionDetailsByReplyContext|InitSceneManagerReply|InitPlayerInboxReply|InitDataServiceReply|x-idseg-seq|reply dropped: Async|SerializeSessionDetails|is a streaming method|metaKeys' -- cpp tools/proto_generator ':!cpp/tests/muduo_grpc_test' ':!tools/proto_generator/protogen/internal/generator/cpp/grpc_process_test.go'

# 含未跟踪文件(mail/、rank/、local_reject 测试)
grep -rnE 'Async[A-Za-z0-9]+(Failed)?Handler|SetIfEmpty|GrpcReplyContext|GrpcCallFailure|FindSentMetadata|reply dropped: Async|metaKeys' cpp tools/proto_generator --include=*.cpp --include=*.h --include=*.go --include=*.tmpl | grep -vE '^(cpp/third_party/|cpp/tests/muduo_grpc_test/|tools/proto_generator/protogen/internal/generator/cpp/grpc_process_test\.go)'

# rpc_replies 目录与处理器文件已删
ls cpp/nodes/scene/rpc_replies 2>/dev/null

# 生成物形状
grep -nE 'SendWatchWatch|SendLeaseLeaseKeepAlive|const std::vector<std::string>& , const std::vector<std::string>& \)' cpp/generated/rpc/service_metadata/rpc_event_registry.cpp
grep -c 'GrpcMessageDone , const GrpcCallOptions& );' cpp/generated/rpc/service_metadata/rpc_event_registry.cpp
```

最后一条计数应约为 195（10-16 时是 197 减去 2 个流式方法；期间有 proto 增删的话，按实际 unary 方法数核对）。

**2. 构建**
- `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。
- 有 Linux 环境时：`bash tools/scripts/build_linux.sh --debug`。没有环境就在交付说明里写“Linux CMake 未验证”。

**3. 单测**
- C-GO：`go test ./internal/... ./cmd -count=1`。
- C-TEST：`rpc_controller_test`、`cross_zone_test`（含步骤 1 的 8 条和步骤 2.5 新增的 3 条）、`bag_test`、`currency_test`（含步骤 3.5 新增的收件箱应答用例）。
- 改过契约测试的话，再跑 `pwsh tools/scripts/tests/k8s_deploy_contract.tests.ps1`。

**4. 联机冒烟**（有本地栈时）

路由模式：`pwsh tools/scripts/start_game.ps1`，然后在 `robot/` 下跑 `.\robot.exe -c etc/battle_smoke.yaml` 和 `-c etc/ordering_smoke.yaml`。逐项确认：
- scene 日志里号段首段 Warm 成功；
- 同节点换图、跨节点换图（第一跳 18，然后交接）各成功一次；
- 镜像进场一次；
- 收件箱 Fetch / Ack 各一轮；
- 停掉 scene_manager 后发一次换图：在 deadline（默认 10s）内收到 `kEnterSceneServerBusy`（3028，“服务器繁忙”）提示，**不是** `kEnterSceneFailed`；再发一次不会被“换图进行中”挡住。

直连模式（D1 = B）：`pwsh tools/scripts/start_game.ps1 -GateRouterMode 0`，跑 `battle_smoke`。逐项确认：
- 登录正常；
- match 的 JoinQueue 回包能送到客户端（新行为）；
- 断开连接时进程不崩。

全程不得出现以下日志：`requires a done callback`、`reply dropped`、`[EnterSceneReply] echo mismatch`、收件箱的 `echo mismatch` ERROR。

**5. 交付说明必须写明**：
- 实际执行了哪些命令、结果如何；
- 没执行的项（含 Linux CMake）；
- 新建和删除的文件清单（含未跟踪的 local_reject 测试和新增的 `player_inbox_reply_test.cpp`）；
- 直连模式的行为变化；
- CreateScene（blissful 合入）的客户端可见变化，以及 Java 侧引用的 PARITY 行；
- AGENTS §8 待批；
- 本清单自己的改动 Java 版不涉及。

没有运行结果之前，不能写“编译通过”或“测试通过”。

---

## 附录 A：D1 选 A（先删直连模式）时的替代做法

这时步骤 5 换成下面的做法，不再生成 `Erased` 和 `GrpcMessageDone`。批 B 的文件数同样超过 30，需要授权，另外还要豁免 D61 的硬前置。

- **gate**
  - `client_message_processor.cpp`：
    - 删 `HandleGrpcNodeMessage`、`ParseMessageFromRequestBody`、`ResolveSessionTargetNode`（同步改 `scene_route_helper.h` 的注释）、L871 的 `[diag]` 日志；
    - 按模式分派的地方只留 `HandleRouterForward`；
    - Disconnect 去掉 `IsRouterModeEnabled()` 判断和 `else` 分支，删 `#include "grpc_client/login/login_grpc_client.h"`。
  - `main.cpp`：
    - `connectTo` 固定为 `{SceneNodeService, ClientRpcRouterNodeService}`；
    - 关机排空和依赖门不再按模式分支；
    - 删 L20 和 L30 的 include；
    - 读一次 `GATE_CLIENT_RPC_ROUTER`：未设置或值为 1 / true / on 时静默，其他值打一次 WARN“已忽略”，做法照 `GATE_ROUTER_TRANSPORT` 的先例。
  - 删除 `gate_router_mode.h`，并从 `gate.vcxproj` / `.filters` 中摘掉。
  - `gate/tests/gate_security_test.cpp`：删 6 个 `GateRouterMode` 用例和 `SetEnvForTest`。
- **生成器**：删 `RpcMethodMeta.sender`、`SenderDeclarations`、`rpcMethodMetaInit.SenderName`、`rpc_event_registry.cpp.tmpl` L13-15，以及 `Message&` 重载；`service_register_info_test.go` 的 `"grpc method with sender"` 改为不带 sender。
- 5.10 到 5.13（gate main、session、network_utils、gate 注释）照做。5.13 里与直连相关的措辞改为“直连模式已删除”。
- **文档**：`client-access-band-routing.md` D61 阶段 1 标为已实施，并订正其中过时的清单；`client-rpc-router.md` D34 和 `xuanming-port-decisions-20260910.md` D-12 按 D61 修订。
- **脚本、部署文档、robot 文案**（`-GateRouterMode` 等）另起一批，超过 30 个文件，先报告并取得授权。
- 额外验证：按 `gate_security_test.cpp` 文件头的 g++ 命令单独编译并运行该测试。

## 附录 B：AGENTS.md §8 拟稿（负责人批准后再改）

把第 145、146 两行替换为：

> - 节点间 muduo RPC（RPC0）和出站 gRPC（生成客户端）的回包都在调用点处理。RPC0 用 `CallMethod` 的每次调用 lambda（`RpcStatus` + 响应）。gRPC 用 `Send<Svc><Method>(registry, node, req, GrpcDone<Resp>, GrpcCallOptions)` 的每次调用回调：恰好调用一次，从不同步调用，done 必填。不再有按 message_id 的全局回包表，也不再有生成客户端的全局 `Async<Svc><Method>Handler` / `Set*Handler`。回调只按值捕获上下文，委托给 `*System` 的具名函数；应答里的回显字段只作核对，不作路由依据。服务端要等下游返回后再回复时，用 `RpcDoneCallback::Adopt(done)`。见 `docs/design/muduo-rpc-channel.md`、[grpc-muduo-transport.md §16.1](../../design/grpc-muduo-transport.md#grpc-client-callback)。
> - `rpc_replies/` 只剩 gate 生成的空 `InitServiceHandler`（battle 的 `register_handler.cpp` 是手写空壳），不要在这里新增回包适配。

`.github/copilot-instructions.md:190-191`、`cpp/AGENTS.md:24,39`、`cpp/README.md:48` 按同样的意思同步修改。

---

## 核对记录

**采纳（都已读代码确认）**
1. 完整性-高：`git apply --3way` 隐含 `--index`。已确认 `git diff --cached` 为空、补丁涉及的文件全是未暂存修改，主工作区 `cross_zone_test.cpp` 是 CRLF（6143/6143）。步骤 1 和 D13 改成逐文件 `git merge-file`、以主工作区为 ours，先规整成 LF，留 `pre/` 副本回滚，禁止 `git add` / `stash`。
2. 完整性-中：默认方案 B 同样超过 30 个文件。§0 加批次授权前置，拆成 A / B / C / D 四批，D1 的论据改成对 A、B 都适用。
3. 完整性-中：`currency_test` 里没有收件箱用例，但它已链接 `scene.lib`。补测试改成必做（3.5）；EnterScene 回显不一致加 3 条用例（2.5）。断言按语义-1 改为“丢弃”，不用核对意见里的“按 9 路由”。
4. 完整性-低：步骤 6 的预检会命中 `router_muduo_forward.cpp:33`。router 注释改动挪进 5.13；3.4 删目录写明依赖步骤 2。
5. 完整性-低：include 调整。`client_message_processor.cpp` 补 `rpc_reply_bridge.h`；gate `main.cpp` 保留 `network_utils.h`（L106 的 `SessionIdGenerator`）；5.12 检查 `session.pb.h`。
6. 完整性-低：遗漏的文档和注释。补入 team-system.md、enter-scene-zone-routing.md、player-storage-placement.md、scene-enter-chain-residuals.md（先协调），`handlers.go:31`，以及各处“回显号为 0 就退回”的注释。
7. 完整性-低：附录 B 说 battle 的 `register_handler.cpp` 是生成的，不对，它是手写的（`proto_gen.yaml` 只配了 `gate_node_replied`）。已改。
8. 完整性-低：验证里没有 Linux CMake。加 C-LINUX，没环境就写“未验证”；顺带写明 CMakeLists.txt 是 `vcxproj2cmake.py` 的产物（§1 第 13 条）。
9. 语义-中：EnterScene 回显不一致时“继续按捕获值路由”是 fail-open。改成丢弃，计 replyUnmatched，D7 两个域统一。依据：Go 的 defer 对每条返回路径都回填回显，旧写法比号会落进 kUnmatched。
10. 语义-低：收件箱回显核对改成逐字段判断，不再整体挂在 `correlation_id != 0` 上。
11. 语义-低：blissful 新增的是 8 条用例，不是 7 条。已改，并列出用例名。
12. 语义-低：blissful 有客户端可见变化。已核对 `HandleMirrorCreateDone` 推的 tip 和 blissful 的 PARITY 登记；交付说明、PROGRESS、Java 一栏都改写；冒烟口径改成 `kEnterSceneServerBusy`（3028）。
13. 语义-低：`rpc_reply_bridge.h` L25 的 `DeliverReply` 契约注释同步改（5.13）。
14. 语义-低：D5 去掉请求副本后缺守卫。采用核对意见里的选项 (a)，在 6.6 加模板断言：序列化先于任何闭包，闭包不捕获 request。

**定稿时自行补充**
- 行号又漂了一次（`SendCorrelatedEnterScene` L469、`DispatchEnterSceneReply` L4854、`TransportFailure` L4971、scene `main.cpp` L137/L140），开头和各步已更新。
- 改按捕获号路由之后，旧版 SM 不回显的应答也能精确认领，`reply_uncorrelated` 在生产中不再计数。写进 2.2、2.4 和 PROGRESS。

**未采纳 / 部分采纳**
- 完整性-中（补测试）里“登记进 currency_test.vcxproj 和 .filters”：该工程没有 `.filters` 文件，只登记 vcxproj。
- 语义-低（D5）的选项 (b)（rpc_controller_test 挂 muduo_grpc 回环服务端）：不采纳。需要新建测试服务端夹具，改动面大；选项 (a) 已经能守住“同步序列化”这个前提。
