# FIX-CPP 交付说明(未编译、未运行,待 Codex 验证)

96-fix-cpp.md 的 8 条逐条核实后都还成立,全部落码,没有跳过的。

## 改动文件

- `cpp/nodes/gate/spectate/gate_spectate_hub.{h,cpp}`
  - 严重-1:`OnEndFrame`(:301)与 `CloseAsSourceLost`(:309)只记 `finalFrame`,经新增的 `StartFinalFanoutUnlessBusy`(:326)开始扇出;在途时由 `FinishFanout`(:479)接着发。`DeliverToTarget`(:454)的必达判据改成 `fanoutFrame == finalFrame`;缓存推迟到 166 开始扇出才清。
  - 一般-1:新增 `NextTurnTargets`(:77)与 `Entry::turnFanoutStart`,`OnTurnFrame` 改用它;`RebindToInstance` 复位。
  - `ContinueFanout`(:438)加判据:没有在途的一轮时不再走 `FinishFanout`。
  - 一般-5:`.h` 文件头写清两条水位,并同步终局与合并规则。
- `cpp/nodes/gate/spectate/gate_spectate_client.h`:`OnSessionClosed` 注释补回选角语义。
- `cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp`(:17、:393):`OnClientLeaveGame` 摘订阅。hub 这条路径只动订阅表,不发帧、不碰连接,TCP 不断时语义正确,之后可重新订阅。
- `cpp/nodes/battle/logic/battle_room_manager.cpp`(:3542):`PublishSpectateEnd` 已升档只发广播。`.h` 的 :202、:209、:417、:543、:567 注释同步。`battle_spectate_relay.h` 只改注释。
- `cpp/libs/engine/core/node/system/node/node_kafka_command_filter.h`:`KafkaCommandTargetFilter::staleInstanceDropCounter`(非空 = 采样,默认空),WARN 行多一个 `sampled_total`(0 = 未采样)。`node_kafka_command_handler.h` 只改注释。
- `cpp/nodes/gate/main.cpp`(:275):中继 options 打开采样,广播 options 由它复制,共用计数。
- `cpp/nodes/gate/handler/gate_push_delivery.{h,cpp}`:最小接缝,`DeliveryCounters` 移到头文件,加 `Counters(Path)` 只读口,逻辑不变;注释补 Bind 前提示。
- 只改注释 / 文档:`proto/contracts/kafka/gate_event.proto`、`client_message_sender.h`、`session_identity_fence.h`(含撤销「删 `kDeliverUnfenced`」的计划)、`docs/design/scene-enter-chain-residuals.md` §2.5。
- `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`:登记 `gate_push_delivery.cpp` 与新测试(只 Debug|x64)。

## 测试

新增文件 `cpp/nodes/gate/tests/gate_push_delivery_test.cpp`,回环夹具,用哨兵帧判定「没有写」,不靠等待:

- `GatePushDelivery.`:`UnicastDeliversOnMatchDropsOnMismatchAndPassesZeroUnfenced`、`UnboundSessionOnlyAcceptsTheExplicitZeroTarget`、`MissingSessionAndReleasedConnectionAreOnlyCounted`、`MissingMessageContentIsRejectedForEveryKindOfPush`、`TargetedBroadcastFencesEveryTargetOnBothEntries`、`MisalignedPlayerListRejectsTheWholeBroadcast`、`SceneBroadcastReachesOnlyBoundSessionsOfThatScene`、`SceneZeroIsRejectedEvenThoughSessionsOutsideAnySceneHaveSceneZero`、`BroadcastToAllSkipsSessionsNotBoundToAPlayer`

`gate_spectate_hub_test.cpp` 新增:

- `SpectateHub.`:`EndWaitsForTheLastTurnSoEverySessionSeesTurnThenEnd`(spec 指定用例)、`TailSessionWithoutStateStillGetsAStateFrameBeforeEnd`、`LastTurnIsStillBestEffortWhileEndIsPending`、`SubscribeWhileEndIsPendingGetsOnlyTheEnd`、`SourceLostAlsoWaitsForTheInFlightFanout`、`OverloadedFanoutTakesTurnsInsteadOfStarvingTheTail`、`TurnFanoutStartStaysInRangeWhenSubscribersLeave`、`SessionLeavingTheGameStopsReceivingAndCanSubscribeAgain`
- `SpectateFrameTargetFilter.`:`StaleInstanceWarnIsNotSampledByDefault`、`StaleInstanceWarnIsSampledWhenTheFilterAsksForIt`、`OnlyStaleInstanceDropsAdvanceTheSampleCounter`

修改:`BattleObserverContract.PromoteIsIdempotentAndLaterRoundsOnlyBroadcast` 的 END 断言改成只有 1 条广播。

**测不到的接线与替代验证**:`GateHandler` / `GateEventHandler` 转调 `gate_push::Deliver*` 的那一行、`OnClientLeaveGame` 摘订阅的那一行。两边拖着节点注册表、登录与场景入口,进不了测试工程。替代:下面第 4 步核对调用点,robot 冒烟补正向断言(目标玩家不符时 `dropped_fence` 增加)。

## 给 Codex(工作目录仓库根,按序;不需要额外环境变量)

1. 生成器 06:08 改过,磁盘上的路由还是 03:18 的旧渲染:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(生成器没编过先 `-Command proto-gen-build`)。通过:`gate_kafka_command_router.cpp` 信封分支 9 个 case 全是 `RejectEmptyPayload`。`gate_event.proto` 本次只改注释。
2. MSBuild,每个都带 `/p:Configuration=Debug /p:Platform=x64 /m:1`,串行:`cpp/generated/proto/proto.vcxproj` → `cpp/libs/engine/core/core.vcxproj` → `cpp/libs/services/gate/gate.vcxproj` → `cpp/nodes/gate/gate.vcxproj` → `cpp/nodes/scene/scene.vcxproj` → `cpp/nodes/battle/battle.vcxproj` → `cpp/tests/routing_identity_test/routing_identity_test.vcxproj` → `cpp/tests/kafka_command_test/kafka_command_test.vcxproj` → `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`。通过:0 error。注意 `routing_identity_test` 这回真正用到 `gate.lib` 的 `tlsSessionManager`,链接报缺符号时先看第三项是否编过。
3. 依次运行,全绿:
   - `build/cpp/tests/routing_identity_test.exe --gtest_filter=SpectateHub.*:SpectateFrameTargetFilter.*:GatePush*`
   - `build/cpp/tests/rpc_controller_test.exe --gtest_filter=BattleObserverContract.*:BattleSpectateRelay.*`
   - `build/cpp/tests/kafka_command_test.exe --gtest_filter=ValidateCommandTarget.*`
   - `pwsh tools/scripts/run_cpp_tests.ps1 -Filter routing_identity -LogDir build/verification/cpp-tests`,`-Filter rpc_controller`、`-Filter kafka_command` 同样。

   失败保留 gtest 全文,不要重试。`GatePushDelivery.*` 用真实回环,Windows 上 poll 粒度 100 ms,预计总共十几秒(估算),属正常。
4. 接线核对:`rg -n "gate_push::Deliver" cpp/nodes/gate/handler` 应有 8 处,`gate_service_handler.cpp` 4 处都是 `Path::kRpc`,`gate_event_handler.cpp` 4 处都是 `Path::kKafka`;`rg -n "gate_spectate::OnSessionClosed" cpp/nodes/gate` 应有 3 处。
5. 红绿证据(可选,改完还原):去掉 `StartFinalFanoutUnlessBusy` 的提前返回,`EndWaitsForTheLastTurn...` 应失败;`NextTurnTargets` 改成不累加(只用 `fanoutNext`),`OverloadedFanoutTakes...` 应失败。

## 未完成项、风险与需要拍板的点

1. **评审原文的修法照抄有缺陷,已按累计起点实现**:新名单每轮都回到哈希序,只按 `fanoutNext % size` 转,第三轮起又回到第二轮的起点,表尾照样饿死。现在累计起点,仍是 `std::rotate` + 取模;`OverloadedFanoutTakes...` 钉住。
2. **比 spec 多做两处**:源站失联也等在途的一轮发完(同一条规则,否则同样丢帧);`ContinueFanout` 的判据。
3. **Bind 前提示依赖另一个任务**:注释写的是目标契约(login 显式发 0),但当前 `gate_command.go` 还拒发 0。Go 那边落地之前,`review-c-batch` 严重-1 仍然存在。
4. **严重-2 的代价**:广播 END 丢了要等 `T_idle`;走维护控制启动、没有 L2 消费者的 gate 也收不到 END。评审一般-2 仍待拍板,本次未动。
5. **回选角残余**:LeaveGame 之后、新角色 Bind 之前又发 SubscribeSpectate,订阅会带到新角色(公开脱敏帧,不碰状态)。要堵得在 `ApplyLoginBinding` 换角色时再摘一次,不在本次范围。
6. **不在允许范围,未改,建议主会话顺手**:`proto/battle/battle_node.proto:178` 只写了每回合走广播,没提 END;`battle_spectate_relay_test.cpp:91` 注释还说「同一帧刚发过中继」,用例本身仍有效;`docs/PROGRESS.md` 未写。
7. **Java 版**(只读核对,未改):严重-1 / 一般-1 不适用,`SpectateHub.fanOut` 同步整批、不分批;严重-2 不适用,两档合成一个频道;一般-4 不适用;两条水位已有。一般-3 已有等价(`unbindScene` → `spectate.drop`),`leaveScene` 在 `sceneNodeId == 0` 时提前返回,残余与第 5 条同类。结论是不适用或已有等价,不往 `PARITY.md` 加行。gate_push 的同类测试 Java 侧未核。
