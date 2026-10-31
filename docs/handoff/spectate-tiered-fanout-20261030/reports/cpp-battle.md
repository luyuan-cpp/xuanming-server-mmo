# CPP-BATTLE 交付说明(未编译、未运行,待 Codex 验证)

开工时 5 个目标文件都是干净的(无上一轮半截改动)。只改了任务说明允许的 5 个文件,未碰 outcome / SQL outcome / FinishBattle / AbortAllRooms 段落。

## 一、改动文件

1. `cpp/nodes/battle/logic/battle_room_manager.h`
   - :3 加 `<algorithm>`;:50-51 类头出站说明补「GateCommand / PushToPlayerEvent 都带目标 player_id,0 本侧拒发」。
   - :110-115 `SetClientEdge` 改为只声明(定义挪到 .cpp),契约:借用非空直连面时按有效连接上限注入节点观众席配额;nullptr 只收回借用、不动配额。
   - :162-176 `HandleAddObserver` 契约同步:不可观战=kInvalidParameter(非 PUBLIC,含未填);单房间或节点配额任一满=kMatchSpectateFull;同 op 幂等重放先于可见性闸与满员/配额判定。
   - :281-297 公开区新增 `kObserverConnectionShareDivisor=4`、`constexpr ObserverSeatQuotaFor(有效上限)`=max(kMaxObserversPerRoom, 上限/4)、`SetObserverSeatQuota()`(只约束新准入,调低不摘人)。
   - :331-333 `BattleRoom::spectateVisibility`(缺省 UNSPECIFIED)。
   - :487-490 `RegisteredObserverSeats()`;:492 NotifySpectateEndAndClose 注释;:522-534 `PushSpectatorFrame` / `BattleFrameConnOf` 声明;:676-677 `observerSeatQuota_`(装配前默认 kMaxObserversPerRoom)。
2. `cpp/nodes/battle/logic/battle_room_manager.cpp`
   - `SendGateCommand`(:88-111):入口 player_id==0 → LOG_ERROR 并返回;信封 `set_player_id`。`PushMessageViaGate`(:135-142):`PushToPlayerEvent.set_player_id`。
   - `HandleCreateBattle`(:709、:822):建房时赋值一次 spectateVisibility;成功日志带 spectate_visibility。
   - `HandleAddObserver`(:1152-1182):幂等块之后、席位判定之前加可见性第二道闸(非 PUBLIC → kInvalidParameter + 英文 LOG_WARN 只带 battle_id/observer,墓碑保持已封闭);单房间满员判定之后加节点配额判定(→ kMatchSpectateFull,INFO 日志带 node_observers/quota)。
   - `BroadcastTurnResult`(:2016-2023)观众循环、`NotifySpectateEndAndClose`(:2124-2132)改调 `PushSpectatorFrame`;后者先把终局包推给全部观众再逐个 RetireObserver,关闭仍推迟到本轮 loop 之后,每条直连上终局包先于 FIN。参战者帧未改。
   - `RegisteredObserverSeats`(:2091-2102):遍历房间表求名单人数之和(不设计数器,停机清房等整间删房路径不会漂移)。
   - `SetClientEdge`(:2344-2359):读一次 battle_max_connections(0 → kHardMaxConnections)注入配额,打一行 INFO。
   - 匿名 `DirectEnvelope`(:2385)抽出 MessageContent 信封;`BattleFrameConnOf`(:2401-2417)抽出战斗帧逐人判定与丢弃日志;`PushBattleFrame`(:2419)行为不变;新增 `PushSpectatorFrame`(:2429-2445):编码一次,逐条活直连 `SendEncoded`。
3. `cpp/nodes/battle/client/battle_client_edge.h`(:35、:73-89)/ `.cpp`(:96-111):新增不透明 `EncodedFrame`(私有构造,只能由 `EncodeFrame` 产出,挡住裸序列化字节)、`static EncodeFrame()`(与 Send 同一份 fillEmptyBuffer 分帧)、`SendEncoded()`(`conn->send(StringPiece)`,不对共享 Buffer 调 `send(Buffer*)`)。`Send` 未改。
4. `cpp/nodes/battle/tests/battle_observer_contract_test.cpp`
   - 夹具:`OriginalCreation` 设 PUBLIC;SetUp 注入 `TableBattleDataProvider` 与默认配额,TearDown 复位二者并销毁 `CreateExtraRoom` 建的房间;新增 `Destroy` / `CreateExtraRoom` / `OriginalIn` / `ClientEdgeBinding`(RAII 借用直连面);回环客户端收字节 `ClientReceived()`。
   - **夹具既有故障(静态推断)**:321f5e1ab 起房间引擎改用 `SetDataProvider` 注入的数据源,夹具从未注入,`TurnBattleEngine::Initialize` 对空数据源直接返回 false,原有所有调 Create() 的用例都应是 kInvalidTableData。已在 SetUp 注入生产实现(空表足够开 PVP 1v1)。

## 二、新增用例(夹具改动影响全部 BattleObserverContract.*)
- `NonPublicBattleRejectsObserverAsTerminalInvalidParameter`(PRIVATE、UNSPECIFIED 各一:kInvalidParameter≠kEntityIsNull、ABSENT、补签拒、同 op 迟到不复活、Remove 有取消证明)
- `NodeObserverQuotaSpansRoomsAndReopensAfterReclaim`(注入 3,跨两房间第 4 名 16021,席位重放照常成功,回收一席后新 op 可进,旧 op 终态)
- `ClientEdgeBindingDerivesObserverQuotaFromConnectionCap`(生产注入路径:84 连接 → 21 席)+ 3 条配额公式 static_assert
- `SpectatorFrameEncodedOnceReachesEveryDirectObserverIntact`(作废路径推 SpectateEnd:两名有直连观众收到的字节与独立 fillEmptyBuffer 结果逐字节相等,中间无直连观众被跳过)

## 三、给 Codex 的命令(仓库根目录,PowerShell,按序)
0. 主会话统一 proto-gen:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`(gate 任务改了生成器)→ `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。核对生成物:`battle_data.pb.h` 有 `SPECTATE_VISIBILITY_PUBLIC`,`battle_node.pb.h` 有 `spectate_visibility()`,`gate_event.pb.h` 的 PushToPlayerEvent 有 `set_player_id`。
1. 推荐 `msbuild game.sln /p:Configuration=Debug /p:Platform=x64 /m:1`(工程间无 ProjectReference;最小集合按序:`cpp/generated/proto/proto.vcxproj` → `cpp/libs/services/battle/battle.vcxproj`)。
2. `msbuild cpp/nodes/battle/battle.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`
3. `msbuild cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`(不在 game.sln)
4. `.\build\cpp\tests\rpc_controller_test.exe --gtest_filter=BattleObserverContract.*:BattlePushPolicy*:BattleTicket*:BattleRoomTable* > <证据目录>\gtest.txt 2>&1`(直接运行 exe:v1 用例会以 threadsafe 死亡测试重入本进程)

通过标准:2、3 零错误;4 全部 PASS(4 个新用例 + 原 BattleObserverContract 用例)。失败保留 msbuild 错误段与 gtest.txt 全文;编码一次用例失败时附两侧收到字节长度。

## 四、未完成项 / 风险 / 需主会话拍板
1. **配额注入点**:共用规则不许改 main.cpp,生产注入放在 `SetClientEdge`(main.cpp 装配时调用一次)里读配置。若要 main.cpp 显式调 `SetObserverSeatQuota`,需授权改 main.cpp 一行并删掉 SetClientEdge 里的推导。
2. **DRY 残余**:「battle_max_connections=0 → kHardMaxConnections」在直连面闸 1(`HandleEstablished`)与 `SetClientEdge` 各写一处;直连面按说明只为编码一次接口改动,未收口。建议后续收成 BattleClientEdge 的静态函数。
3. 依赖 match(B 批 Go)在 CreateBattleRequest 填 spectate_visibility;未填 = UNSPECIFIED,battle 拒绝一切观战(设计如此,fail-closed),两边要同批上。
4. `SendGateCommand` 拒发 player 0 没有单测接缝(匿名命名空间 + KafkaProducer 单例),靠 gate 侧栅栏计数 / robot 冒烟覆盖。
5. 回合结果观众帧无专门用例:与 SpectateEnd 共用 `PushSpectatorFrame`。用例刻意走作废路径:驱动引擎出招若打出终局会进 BeginResultCommit,房间删不掉,会把观众带进后续用例的节点配额。
6. 节点配额满的拒绝日志每次一行 INFO,与单房间满员同口径、未采样;配额调低不摘已登记观众。
7. 文档未改(不在允许范围):`battle-spectate-tiered-fanout.md` §10.1 进展(第 3、4、6 条 battle 侧)、`docs/PROGRESS.md`、Java `PARITY.md` 由主会话登记。
