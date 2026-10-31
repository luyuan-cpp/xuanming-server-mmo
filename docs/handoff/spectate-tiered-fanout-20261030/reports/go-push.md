# GO-PUSH 交付说明（未编译、未测试，待 Codex 验证）

勘查参考 `scratchpad\wf\scout_gate.md` 不存在，改为直接按工作区源码与 `broadcast_target_codec.h` 核对（位序：第 offset/8 字节的第 offset%8 位，低位在前；阈值：去重后 ≥32 且 bytes+6 < n*3）。

## 改动文件
1. `go/shared/kafkautil/gate_push.go`
   - L16-25 `PlayerGateInfo` 注释：PlayerID 必填。
   - L35-59 `GateCommandBuilder`：`BuildPushCommand(gateNodeID, sessionID, playerID uint64, gateInstanceID, messageID, body)`；`BuildBroadcastCommand(gateNodeID, targets []BroadcastTarget, ...)`，契约写明四个目标字段只能经 `EncodeBroadcastTargets` 编码。
   - L61-99 `PushToPlayer`：最前面拒绝 `PlayerID==0`（返回错误，不调 builder / writer），playerID 传给 builder。
   - L101-184 `BroadcastToPlayers`：PlayerID==0 的目标逐个丢弃（Error 日志 + 记首个错误）；分组收 `[]BroadcastTarget`；全无效时不调 builder / writer。
   - L286-350 新增 `BroadcastTarget`、`EncodeBroadcastTargets`（不改入参；稳定升序、同 session 留首个；跨度按 uint64 算）；删除 `EncodeBitmapFields`；`BitmapThreshold` 保留。
2. `go/friend|guild|match/internal/kafka/gate_command_builder.go`（三份函数体逐字相同）：`BuildPushCommand` 的 PushToPlayerEvent 与 GateCommand 信封都填 PlayerId，信封另填 SessionId；`BuildBroadcastCommand` 用 `EncodeBroadcastTargets` 填 session_list / bitmap_base / bitmap / player_list。
3. `go/guild/internal/logic/guild_manage_logic_test.go` L73、L77：fake builder 只同步签名。
4. `go/login/internal/svc/gate_command.go` L159-203 `buildPushTipCommand`：拒绝 playerID==0；PushToPlayerEvent 填 PlayerId；信封注释更新。
5. `go/mail/internal/notify/notify.go` L92-93 `publish`：PushToPlayerEvent 填 PlayerId（入口本就拒 player==0）。
6. `go/mail/internal/notify/notify_integration_test.go` L103、L107：补信封与事件的 `GetPlayerId()` 断言。
7. 新增 `go/shared/kafkautil/testdata/broadcast_target_vectors.json`（7 条，按约定格式，另有顶层 `schema` 说明串）：single_target、few_unsorted_list、duplicate_session_keeps_first、dense_bitmap_64、duplicate_in_bitmap_keeps_first、alternating_bits_bitmap_40（0x55 钉低位在前）、sparse_span_falls_back_to_list_32。session 取 gate 5 段（655360 起），player_id 刻意不随 session 单调。

## 测试用例
- 新增 `go/shared/kafkautil/gate_push_test.go`：`TestEncodeBroadcastTargetsMatchesSharedVectors`（逐字段对 expect，含不改入参与覆盖面守卫）、`TestBroadcastTargetVectorsDecodeToSortedTargets`（按 gate 解码契约自检向量，与 C++ 用例同断言）、`TestEncodeBroadcastTargetsFormBoundaries`（31/32 阈值、去重后计数、span 712/713、uint32 全跨度、空输入）、`TestPushToPlayerRejectsZeroPlayerID`、`TestBroadcastToPlayersDropsZeroPlayerID`（全无效：nil writer、builder 未被调用；混合：只把有效目标交给 builder）。
- 新增 `go/guild/internal/kafka/gate_command_builder_test.go`：`TestBuildPushCommandCarriesTargetPlayer`、`TestBuildBroadcastCommandAlignsPlayerListWithBitmapOrder`（64 个逆序密集目标，按 bitmap 位序逐个核对 player_list）、`TestBuildBroadcastCommandListFormKeepsAlignment`。
- `go/login/internal/svc/gate_command_test.go`：`TestPushTipCommandPayloadLayers` 补 `GetPlayerId()`；新增 `TestPushTipRejectsZeroPlayerID`（helper 与零值 ServiceContext 的导出发送路径都被拒）。
- 新增 `go/mail/internal/notify/notify_test.go`：`TestPublishCarriesTargetPlayerIdentity`（miniredis + 假 Publisher，默认就跑）；真 Kafka 用例 `TestRealKafkaRedisMailReminderUsesGateFenceAndNoAssetAuthority` 补断言（默认 Skip）。

## 给 Codex 的命令（仓库根 `D:\luyuan\xuanming-server-mmo-cpp`）
0. 与其他会话的 proto 改动协调后统一生成（CPP-GATE 改了生成器源码，先 build）：
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
   通过标准：`go/proto/contracts/kafka/gate_event.pb.go` 出现 `PushToPlayerEvent.PlayerId` 与 `BroadcastToPlayersEvent.PlayerList`。
1. 工作目录 `go/shared`：`go test ./kafkautil/... -count=1 -v -run "BroadcastTarget|ZeroPlayerID"`，再 `go test ./kafkautil/... -count=1`、`go vet ./kafkautil/...`。
2. 依次在 `go/friend`、`go/guild`、`go/match`、`go/login`、`go/mail`：`go build ./... && go test ./... -count=1`。重点包：guild `./internal/kafka/...`、`./internal/logic/...`；login `./internal/svc/...`；mail `./internal/notify/...`；friend `./internal/logic/...`；match `./internal/kafka/...`、`./internal/playercontract/...`。
3. `gofmt -l`（只列不写）检查上面 13 个文件，期望无输出。

通过标准：全部绿；mail 真 Kafka 用例未设 `MAIL_PUSH_TEST_KAFKA` 时 Skip 属正常。向量用例失败请保留 `-v` 全文回报：向量是手算的，以机器结果为准，不要直接改表。match 有其他会话的在制改动，与本任务无关的失败请对照基线单列。

## 未完成项、风险、待拍板
1. 三份 builder 逐字重复，按说明本次不合并，建议另立任务收进 shared。
2. 文档未改（不在可改范围）：`docs/design/broadcast-message-size-analysis.md:92` 仍写 `EncodeBitmapFields()`；PROGRESS.md、routing-identity-audit R13 的 Kafka 路径状态需主会话补记。
3. C++ `broadcast_target_codec.h` 的 span 用 uint32 算：min=0、max=UINT32_MAX 时回绕成 0，得到空 bitmap 后越界写（UB）。同一 gate 实例的 session 高位相同，实际到不了；Go 已按 uint64 算，建议 CPP-GATE 同口径。
4. 按说明 C++ 向量用例只解码 expect；建议再加一条：C++ Encode(targets) 对两种消息类型都等于 expect，阈值与去重口径才算两向锁住。
5. login 行为变化：playerID 为 0 的进场失败 tip 不再发出，计 failed 并打 ERROR（正常链路 playerID 恒非 0）。
6. 重复 session 保留首个：若首个来自过期会话记录，当前玩家这条广播会被 gate 栅栏丢弃（广播本就可丢，与 C++ 同语义），已写进注释。
7. Java：gate 推送栅栏已有等价实现（每个目标带 player_id），本任务不改 Java，也不加 PARITY 行。
8. proto-gen 之前，这些模块会因为 `PlayerId` / `PlayerList` 还没生成而编不过，属预期。
