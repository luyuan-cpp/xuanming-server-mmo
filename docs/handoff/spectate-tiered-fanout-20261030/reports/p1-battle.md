# P1-BATTLE 交付说明(未编译、未运行,待 Codex 验证)

battle 节点的观战中继(L1)与全服广播(L2)出口已按 §15.4 落码。开工时目标文件没有本任务的半截改动;`PumpSqlOutcomes` 未碰。

## 一、改动文件

1. **新增 `cpp/nodes/battle/logic/battle_spectate_relay.{h,cpp}`**:`FrameRecord`(topic / 分区 / key / 字节)、`FrameSink`(可注入出口)、`SendFrameViaKafka`(默认出口)、`SpectateFramePublisher`(`PublishToRelayGate` / `PublishToBroadcast` / `SendFailures`)。发送失败只计数 + 采样日志 `metric=battle_spectate_frame_send_failed channel= kind= failed_total=`,不重试。帧直接用 proto 的 `SpectateFrameEvent`,不写并行 struct。
2. **`cpp/nodes/battle/logic/battle_room_manager.h`**
   - :22、:32-35 include;:66-68 类头出站说明。
   - :190-218 公开区契约与 `HandleAttachRelayGate` / `HandlePromoteSpectateBroadcast` / `SetSpectateFrameSink`。
   - :415-424 房间状态 `relayGates`、`spectateBroadcast`、`HasSpectateAudience()`。
   - :525-527 `BuildSpectatorState`;:543-569 `NotifySpectateEndAndClose` 契约与 `FindSpectateChannelRoom` / `NewSpectateFrame` / `BuildSpectateStateFrame` / `PublishSpectateTurn` / `PublishSpectateEnd`;:599-602 `PushSpectatorFrame` 改为收信封;:752 `spectateFramePublisher_`。
3. **`cpp/nodes/battle/logic/battle_room_manager.cpp`**
   - :85 `ClientEnvelope`(原 `DirectEnvelope` 上移改名,直连单发、L0 编码一次、中继 / 广播共用)。
   - :2025 `BroadcastTurnResult`:观众版信封只做一份,L0 与中继 / 广播共用。
   - :2087 `BuildSpectatorState` 从 `PushSpectateState` 抽出(L0 首帧与 STATE 帧同源)。
   - :2133 `NotifySpectateEndAndClose`:没有 L0 观众也发 END;先发通道,再走原 L0 流程。
   - :2380 `SetClientEdge` 改取 `edge->EffectiveMaxConnections()`。
   - :3399-3556 新增 Attach / Promote 与帧发布实现(文件末尾)。
4. **`cpp/nodes/battle/client/battle_client_edge.{h,cpp}`**:新增 `EffectiveMaxConnections()`(h:99,cpp:113),闸 1(cpp:187)改用它。「0 = 硬上限」只剩这一处(B 批 DRY 修正)。
5. **`cpp/nodes/battle/handler/grpc/battle_node.cpp`** :115、:123:两个守护段各一行委托。
6. **登记**:`cpp/nodes/battle/battle.vcxproj`、`.vcxproj.filters`、`CMakeLists.txt`(后者由 vcxproj 生成,手加的行与生成结果同位置),`cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`(新测试与 relay 源文件)。

## 二、新增测试

`BattleObserverContract`(夹具换捕获出口;结算回合走 `HandleSetAutoBattle`,不等定时器):
- `AttachRelayGateRejectionsFollowContractOrderWithoutSideEffects`、`PromoteRejectionsFollowContractOrderAndDoNotPromote`
- `AttachRelayGateIsIdempotentAndNewInstanceReplacesOldWithOneStateFrame`
- `EveryRoundSendsOneRelayFramePerAttachedGateOverConfiguredRoute`
- `PromoteIsIdempotentAndLaterRoundsOnlyBroadcast`
- `EndFrameReachesEveryChannelOnceWithDirectObserverReason`
- `DirectObserverAndRelayGateShareTheSameSpectatorEnvelope`
- `RefusedSpectateFramesNeitherFailAttachNorBlockRounds`
- `ShutdownSendsNoEndFrameForUndecidedRoomButEndsAbortableRoom`(子进程)
- `RelayStateFrameDuringOutcomeWaitDoesNotLeakTerminalResult`(子进程)
- `ClientEdgeEffectiveMaxConnectionsFallsBackToHardCap`

新文件 `cpp/nodes/battle/tests/battle_spectate_relay_test.cpp`(`BattleSpectateRelay`):
`RelayRecordAddressesTargetGateOverConfiguredRoute`、`UnconfiguredRouteFallsBackToCommandTopicDefaults`、`BroadcastRecordClearsTargetsAndPartitionsByBattle`、`ReusedFrameCarriesEachGatesOwnTarget`、`RefusedSendIsCountedWithoutRetry`。

## 三、给 Codex 的命令(仓库根目录,PowerShell,按序)

0. 本任务没改 proto,10-30 03:18 的生成物已含所需符号;其他任务改了 proto 或生成器时按 README §3 统一重跑。依赖库过期时先按序编 `cpp/generated/proto/proto.vcxproj` → `cpp/libs/services/battle/battle.vcxproj`。
1. `msbuild cpp/nodes/battle/battle.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`
2. `msbuild cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`(不在 game.sln)
3. `.\build\cpp\tests\rpc_controller_test.exe --gtest_filter=BattleObserverContract.*:BattleSpectateRelay.*:BattlePushPolicy*:BattleTicket*:BattleRoomTable* > <证据目录>\gtest.txt 2>&1`(直接运行 exe:子进程用例以 threadsafe 死亡测试重入本进程)

通过标准:1、2 零错误;3 全部 PASS(上面 16 个新用例 + 原有用例)。失败保留 msbuild 错误段与 gtest.txt 全文。

## 四、风险与需主会话拍板

1. **必须修(gate 侧,不在本任务范围)**:`cpp/nodes/gate/spectate/gate_spectate_hub.cpp:239` 的 `OnStateFrame` 把 `entry.lastRound` 推进到 STATE 的回合。引擎的快照回合 r 是**还没结算**的回合,紧接着的 TURN 也带 r(`TurnResultS2C.round_index` 取结算前的 `roundIndex`,结算后才 +1)。于是 `OnTurnFrame` 的 `round <= lastRound` 会丢掉 TURN(r):每次 Attach 那台 gate、每次 Promote 全部 gate 都少播一回合,若发生在最后一回合,观众看不到终局那一回合。
   §15.5 写的是 STATE「只更新缓存」,建议删掉这一行并补用例「STATE(r) 之后 TURN(r) 被接收」。battle 按 §15.4 字面取值,`EveryRoundSends...` 已钉住 STATE(r) → TURN(r);Java 同语义时注意同一个坑。备选是 STATE 改填 r-1,但要改 §15.4 和 proto 注释,不推荐。gate 文件我没动;gate 代理落码时还在写,行号以最新为准。
2. **相对说明的细化**:STATE 帧的快照走 `BuildSpectatorState`,等结果回执期间取冻结的 `pendingVisibleState`,不用裸 `BuildStateSnapshot`,否则会提前泄露终局(L0 首帧本来就是这样,有回归用例)。
3. **幂等口径**:同 (node, instance) 重复 Attach、重复 Promote 都只回包、不再发 STATE。首帧万一没发出去,靠下一回合 TURN 自愈。
4. **后到的旧实例 Attach 会覆盖新实例**(UUID 无序,只能后写赢):需要旧 RPC 延迟超过 gate 重启时间才发生。发生后新实例在本场收不到中继帧,其观众按 `T_idle` 收 SOURCE_LOST。概率低,未处理。
5. END 的 `round_index` 取引擎当前回合:正常结束就是最后结算的回合;中途作废是还没结算的那一回合。gate 对 END 不看回合,不影响行为。引擎没有回合访问器,这里用一次 `BuildStateSnapshot`,只在有中继 / 广播通道时才算。
6. `SendFailures()` 只数没进生产者队列的帧;已入队后投递失败的归 `kafka_producer_stats`。manager 不对外暴露这个计数,线上只看 metric 日志。
7. **未改的文档 / 对账(不在允许范围,由主会话处理)**:`docs/PROGRESS.md`、设计文档 §15.8 状态、`battle_push_policy.h` 注释(战斗帧分类仍只讲直连出口,可补一句 L1 / L2 走 D94 例外)。**Java 版**:P1 / P2 battle 部分待做,按 README 排在 C++ / Go 落稳之后,`PARITY.md` 届时登记。
