# scene 按 (gate 实例, session_id) 认会话(e 方案 C)全线:验证交接(2026-11-26,交 Gemini)

## 0. 现状

- 要解决的问题:gate 会话号是 `node_id<<17 | 序号`。同一 node_id 短暂有两个 gate 进程(新进程接走号、老进程还没关停)时,同一名玩家可能
  在两边拿到同一个会话号。改动前老进程迟到的 ExitGame 会让新会话进入断线保留、25s 后退出要重登(不丢数据);迟到的入场、旧客户端消息、
  回推投错进程、location 与托管落点按会话号认是同一类问题的其余几面。
- **代码、测试、文档都已写完,Gemini 只负责生成、编译、测试**。本线四块:

  | 块 | 内容 | 设计稿 | 代码在哪 |
  |---|---|---|---|
  | A(11-20) | RPC0 入站握手把对端 uuid 记在连接上;scene 入场栅栏、客户端消息(含 ExitGame)、被拒提示按实例认会话;login 跨实例同号改回踢线 | §14 | HEAD |
  | B(11-20) | scene 回推与自发请求按会话所在的 gate 进程找 gate | §14.7 | HEAD |
  | C(11-26) | location 记 gate 实例(`PlayerLocation.gate_instance_id = 10`)、托管落点事件带实例(`PlayerHostedPlacementEvent.gate_instance_id = 8`) | §14.8 | HEAD,**proto 改了,要先生成** |
  | D(11-26) | 源节点上托管角色的改派待确认条目也记实例 | §14.8 末条 | 代码在 HEAD;两条用例与设计稿在工作区未提交(`cross_zone_test.cpp`、`scene-enter-chain-residuals.md`) |

- **一条都没验证过**,没有生成、编译或测试记录。Claude 只做过 `test_cpp_no_raw_pointer_member.py` 预检(通过)与静态核对。
  唯一沾边的是 11-21 一次 Docker Linux 全量构建(`build/verification/docker-rebuild-all-20261121T040528.237014Z`):它在 core 库停在别的线的
  `table_bundle_manager.cpp`(把 `string_view` 写进 muduo 日志流,现已改成 `std::string(...)`),停之前本线的 `rpc_channel.cpp`、`network_utils.cpp`、
  `client_message_sender.cpp` 用 GCC 13 编过,scene 库与节点都没编到。只当参考,不算本线的编译证据。
- 设计:[scene-enter-chain-residuals.md](../design/scene-enter-chain-residuals.md) §14~§14.9(两处不变量措辞的定论在 §14.1,变异表在 §14.4、§14.7、§14.8,
  修后主流程、方案取舍与水平扩展分析在 §14.9);
  [scene-enter-session-fence.md](../design/scene-enter-session-fence.md) §5.2~§5.7;[muduo-rpc-channel.md](../design/muduo-rpc-channel.md) §8.3。
- PROGRESS 里四条(按标题开头 grep):「Claude:scene 按 gate 实例认会话」(11-20)、「Claude:scene → gate 的回推与自发 EnterScene」(11-20)、
  「Claude:location 记 gate 实例、托管落点带实例」(11-26)、「Claude:托管角色的改派待确认条目也记 gate 实例」(11-26)。
  旧条目写的是「待 Codex」、变异编号有一处作废(§14.4 原 M3),**命令以本单 §3 为准**。
- 不用再拍板,都已定案:同一会话 = 同一 (gate 实例, session_id),任一边实例未知按会话号认;不加栅栏之前的连接级判定;login 跨实例同号照踢;
  回推找不到会话所在的进程时不退回按 node_id 投给另一个进程;代次都为 0 的情形生产上走不到(§14.6)。

## 1. 规矩

- 读 `AGENTS.md` 全文;只在 main 上做,不开分支,不 push,不打 tag;**只有负责人明确要求才 commit**,只提交本线文件,不要 `git add -A`;
  不手改生成产物;日志文案英文,文档与注释中文。
- 分工(负责人 11-26 定):**逻辑代码由 Claude 改,Gemini 只负责生成、编译、测试、冒烟**,用低档模型 `Carbon Medium` 跑。
- 失败时:保留第一条失败原文(编译错误写到 文件:行),停下,交回 Claude 改;不注释断言、不跳测试、不连续自动重试;
  失败落在 §2 以外的文件,是别的线的,用 `git diff` 分清后只记录。
- 游戏未上线,scene_manager 的 location 等数据可以清空;两个改动的 proto 都不是客户端契约,不写客户端仓。
- 结果:在 `docs/PROGRESS.md` 文末追加「Gemini:scene 按 gate 实例认会话全线验证」,写清每步命令、实跑条数、平台与失败原文摘要;
  日志放 `build/verification/scene-gate-instance-20261126/`。
- Java 不写代码,Java 仓 `PARITY.md` 已登记三条待做。

## 2. 本线文件

- RPC0:`cpp/libs/engine/core/network/{rpc_channel.h,rpc_channel.cpp,rpc_controller.h,network_utils.h,network_utils.cpp,client_message_sender.h,client_message_sender.cpp}`、
  `cpp/libs/engine/core/node/system/registration/registration_manager.cpp`、`cpp/libs/engine/core/type_define/player_login_identity_comp.h`。
- scene:`cpp/libs/services/scene/player/system/{enter_fence.h,player_lifecycle.h,player_lifecycle.cpp,role_admission_system.cpp,relocate_confirm.h,player_scene.cpp,player_team.cpp}`、
  `cpp/libs/services/scene/battle/system/player_battle.cpp`、`cpp/libs/services/scene/core/system/redis.cpp`;
  节点守护段 `cpp/nodes/scene/handler/rpc/scene_handler.cpp`、`cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp`、`cpp/nodes/scene/handler/event/player_placement_event_handler.cpp`。
- 测试:`cpp/tests/cross_zone_test/{cross_zone_test.cpp,player_team_follow_test.cpp,role_admission_test.cpp}`、`cpp/tests/rpc_controller_test/rpc_channel_loopback_test.cpp`。
- proto:`proto/scene_manager/storage.proto`、`proto/common/event/player_placement_event.proto`。
- Go:`go/login/internal/logic/clientplayerlogin/{entergamelogic.go,metrics.go,entergame_kick_test.go}`;
  `go/scene_manager/internal/logic/` 下 `placement_identity.go`、`changesceneutil.go`、`enter_hosted_placement.go`、`enterscenelogic.go`、`enter_holder_release.go` 及同名 `_test.go`、`enterscene_identity_test.go`、`leave_release_test.go`。

## 3. 步骤(仓库根,串行)

1. 协议生成:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
   通过:Go / C++ 生成物里 `PlayerLocation` 多 `GateInstanceId`、`PlayerHostedPlacementEvent` 多 `GateInstanceId`,本线没有别的生成差异。
2. Go,`go/login`:`go test ./internal/logic/clientplayerlogin/ -count=1 -v -run "TestEnterGame_"`(含 `TestEnterGame_SameSessionDifferentInstanceKicks`、
   `TestEnterGame_SameSessionSameInstanceDoesNotKick`),再 `go vet ./...` 与 `go test ./... -count=1`。
3. Go,`go/scene_manager`:`go test ./internal/logic/ -count=1 -run "TestDecidePlacementIdentity|TestDecideLeaveIdentity|TestEnterScene_SameSessionIdFromAnotherGateProcessIsStale|TestEnterScene_HostedPlacement|TestIdentityIsHostedLogin|TestIdentityStrictlyNewer|TestEnterScene_SamePlacement|TestEnterScene_LoginBehindUnreleasedHolderRequestsReleaseOnce|TestEnterScene_HostedResume|TestEnterScene_LoginWithoutStrictlyNewerIdentity"`,
   再 `go vet ./...` 与 `go test ./... -count=1`。读 location 的 `go/player_locator`、`go/guild`、`go/match`、`go/player_inbox`、`go/shared` 各跑 `go build ./...`。
4. C++ 整解:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。改了 core 与 scene 两个库,`run_cpp_tests -Build` 不重编库,必须先做。0 error。
5. `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/scene-gate-instance-20261126/cpp-tests`:
   先单跑 `build\cpp\tests\cross_zone_test.exe --gtest_filter=GateInstanceFence*:GateInstanceRouteEcs.*:EnterFence*:ActingLoginGeneration*:RoleAdmission*:HostedPlacementEcs.*:HostedPlacementReentry.*:RelocateConfirmEcs.*:PlayerTeamFollowTest.*`,
   新用例(§14.4、§14.7、§14.8 三张表)都在且全绿;再跑全量。
6. `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test -LogDir build/verification/scene-gate-instance-20261126/cpp-tests`:
   先单跑 `build\cpp\tests\rpc_controller_test.exe --gtest_filter=RpcChannelLoopback.*:RpcClient*:NodeHandshake*`(含 `HandshakePeerRecordedOnTheInboundChannelReachesEveryLaterDispatch`),再跑全量。
7. 回归:`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -LogDir build/verification/scene-gate-instance-20261126/cpp-tests`(全部工程;`RpcController` 多了引用成员、
   `PlayerLoginIdentityComp` 换了头并多了 `std::string`,链接 core / scene 的工程都要编得过)。别的线的既有失败点名即可。
8. 变异核对:每个单独做、跑完逐字节恢复;期望变红的用例见设计稿对应表。
   - §14.4:M1 `enter_fence::SameGateInstance` 恒 `return true;`;M2 `RecordLoginIdentity` 里实例改成会话有效就照写;M4 覆盖待入场上下文的擦除条件去掉
     `previousSessionId != sessionId`;M5 `RpcChannel::CallServiceMethod` 改回 `RpcController controller(connection);`;M6 `kickReplacedSession` 恢复「会话号相同、实例不同 → 不踢」(跑第 2 步)。
   - §14.7:M3 `BoundGateInstanceId`(`network_utils.cpp`)删掉取身份组件实例那一支;M7 `ResolveSessionGateEntity` 恒 `return ResolveLocalZoneGateEntity(sessionId);`。
   - §14.8:M8~M10、M12 跑第 3 步的 Go 用例;M11、M13 跑第 5 步。
9. `python tools/scripts/check_log_text_english.py` 无输出;Linux `build_linux.sh` 编过(新头只登记了 vcxproj,没有新增 `.cpp`)。

- **通过标准**:第 1 步只多两个字段;第 2~7 步全绿;第 8 步红绿与设计稿一致;第 9 步通过。
- 没有行为测试、只靠静态阅读的接线列在设计稿 §14.4 与 §14.7 末尾;双 gate 同号的端到端冒烟没有现成场景,不要求。
