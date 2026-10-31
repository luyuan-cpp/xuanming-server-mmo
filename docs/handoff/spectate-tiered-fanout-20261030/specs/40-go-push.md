# 任务 GO-PUSH：Go 侧 gate 推送生产者一律填目标玩家（C 批 Go 侧）

先读 `00-common-rules.md`（同目录）。勘查参考：`..\wf\scout_gate.md` §0、§4、§5（行号已过时）。

## 允许改的文件
- `go/shared/kafkautil/gate_push.go`、`go/shared/kafkautil/gate_push_test.go`、
  新增 `go/shared/kafkautil/testdata/broadcast_target_vectors.json`
- friend / guild / match 三份 `internal/kafka/gate_command_builder.go`（及新增同目录测试）
- 用到这些 builder 接口的测试替身（如 `go/guild/.../guild_manage_logic_test.go` 的 fake），只同步签名
- `go/login/internal/svc/gate_command.go` 及其测试、`go/mail/...` 里组装 PushToPlayerEvent 的文件（勘查称 `notify.go`）及其测试
- 用 `grep -rn "PushToPlayerEvent{\|BroadcastToPlayersEvent{\|EncodeBitmapFields\|BuildPushCommand\|BuildBroadcastCommand" go/ --include=*.go`
  （排除 vendor、generated、`*.pb.go`）找到的其他生产者 / 调用点，同样补齐；`go/match/internal/logic/` 下的观战文件
  由另一个任务在改，若那里有调用点只做签名同步、改动最小。

## 要做的事
1. `gate_push.go`：
   - 新增 `type BroadcastTarget struct { SessionID uint32; PlayerID uint64 }` 与
     `EncodeBroadcastTargets(targets []BroadcastTarget) (sessionList []uint32, bitmapBase uint32, bitmap []byte, playerList []uint64)`：
     按 SessionID **稳定**升序排序、同一 session 重复时保留第一个；目标数 ≥ 32 且 `bitmapBytes + 6 < n*3` 时用 bitmap
     （与 C++ `broadcast_target_codec.h` 同阈值同口径），否则用 session_list；player_list 按同一顺序对齐。删掉旧的
     `EncodeBitmapFields`（调用点全部改用新函数）。
   - `PushToPlayer` 拒绝 `PlayerID == 0`（返回错误，不发）；`BroadcastToPlayers` 逐个丢弃 PlayerID 为 0 的目标并记首个错误，
     全部无效时不调用 writer。
   - builder 接口：`BuildPushCommand` 增加 `playerID uint64`，`BuildBroadcastCommand` 改收 `[]BroadcastTarget`。
2. 三份 builder：PushToPlayerEvent 填 `PlayerId`，GateCommand 信封填 `PlayerId`（与 `SessionId`）；广播用
   `EncodeBroadcastTargets` 填 session_list / bitmap / player_list 四个字段。三份逐字相同的重复本次不合并（不扩范围），
   交付说明里记一笔。
3. login `gate_command.go`、mail 的推送组装：PushToPlayerEvent 填 `PlayerId`。
4. 跨语言向量：写 `testdata/broadcast_target_vectors.json`（格式见下），至少包含：单目标、少量乱序目标（list 形态）、
   含重复 session、64 个密集目标（bitmap 形态）、跨度过大而退回 list 的稀疏目标。`gate_push_test.go` 新增
   `TestEncodeBroadcastTargetsMatchesSharedVectors`：对每条向量 `EncodeBroadcastTargets(targets)` 必须逐字段等于 `expect`。
   另加 `TestPushToPlayerRejectsZeroPlayerID`、`TestBroadcastToPlayersDropsZeroPlayerID`（全无效时 writer 可传 nil 不被调用）；
   新建 guild builder 测试断言 player_list 与 bitmap 位序对齐；login / mail 既有推送测试补 `GetPlayerId()` 断言。

## 向量文件格式（C++ 侧任务按此读取）
```json
{
  "vectors": [
    {
      "name": "dense_bitmap_64",
      "targets": [ { "session_id": 1, "player_id": 1001 } ],
      "expect": { "session_list": [1], "session_bitmap_base": 0, "session_bitmap_hex": "", "player_list": [1001] }
    }
  ]
}
```
`session_bitmap_hex` 是 bitmap 字节的小写十六进制；用 bitmap 时 `session_list` 为空数组，反之 `session_bitmap_hex` 为空串。
bitmap 的位序与 C++ 现有解码一致（勘查报告与 `broadcast_target_codec.h` 为准：第 N 位置位表示 session = base + N，
先核对 C++ 的字节内位序再写向量）。

5. 交付说明里给 Codex 的命令：proto-gen 统一跑之后，`go/shared` 下 `go test ./kafkautil/... -count=1`，再对 friend / guild /
   match / login / mail 各模块 `go build ./... && go test ./... -count=1`（列出确切模块目录）。
