# PROTO KNOWLEDGE BASE

## OVERVIEW
`proto/` is the contract source of truth. Cross-language interface changes should start here, then flow into generated outputs and consuming services.

## STRUCTURE
目录结构与客户端可见契约的范围见同目录 [README.md](README.md),这里不重复维护。

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Message IDs | `message_id.txt` | Shared message-id registry |
| Event IDs | `event_id.txt` | Shared event-id registry |
| Kafka command contracts | `contracts/kafka/` | Gate/scene command payloads |
| Scene APIs | `scene/` | Player/scene/game proto files |
| Login APIs | `login/login.proto` | Login contract root |
| Scene manager APIs | `scene_manager/` | Manager-side contracts |

## CONVENTIONS
- Edit contracts here first; generated outputs elsewhere are downstream artifacts.
- Keep naming aligned with current runtime terminology, especially **Scene Node** rather than legacy/game-node phrasing.
- When routing behavior is Kafka-based, make sure contract updates match topic/consumer expectations in C++ and Go consumers.
- After proto changes, regenerate outputs and rebuild all affected languages.

## FIELD TYPE CONSTRAINTS
- **double (NOT float)**: `Location`, `Rotation`, `Scale`, `Vector3`, `Velocity`, `Acceleration` x/y/z — must match UE4 client double precision.
- **uint64 (NOT uint32)**: `BaseAttributesComp.strength/stamina/health/mana/critchance/armor/resistance` — combat formulas cast to double; uint64 keeps headroom.
- **uint64 (mandatory)**: All SnowFlake GUIDs (`player_id`, `entity`, `scene_id`, `item_id`, `buff_id`, `skill_id` instance, `tx_id`, `snapshot_id`); all Unix-ms timestamps; currency values.
- **uint32 (safe)**: `session_id`, `session_version`, `attack_power`, `defense_power`, `max_health`, `skill_table_id`, `current_frame`.
- **float (safe)**: `ViewRadius.radius`.

## ANTI-PATTERNS
- Patching generated `.pb.go`, `.pb.h`, `.pb.cc`, or checked-in generated proto trees instead of updating source `.proto` files.
- Making service/event ID changes without updating the registry files.
- Treating a contract change as language-local; this repo has multiple consumers.
- Shrinking coordinate doubles to float, BaseAttributesComp uint64 to uint32, or any SnowFlake GUID / Unix-ms timestamp field — see FIELD TYPE CONSTRAINTS above.

## COMMANDS
```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
cd go && build.bat # 仅在需要重生 db/login 的 goctl 包装时执行
```

## NOTES
- 上述命令从仓库根执行。完整绑定由 protogen 生成；`go/build.bat` 不能代替它。默认 `proto_gen.yaml` 会写同级客户端仓，有客户端任务授权时才使用；只改服务端使用上面的 no-unity 配置，详见根 `AGENTS.md` §4。
- `generated/proto/` is checked in, so proto edits usually create visible downstream diffs.
- `proto/scene/` and `proto/login/` are high-churn service areas; inspect them first for gameplay/auth contract work.
- **`PlayerStressTestProbe`** (`common/component/player_comp.proto`): a test-only sub-message (`test_seq` uint64, `test_sig` bytes) embedded in `player_database` (field 9) in `common/database/mysql_database_table.proto`(2026-11-05 起 `player_database_1` 已删除,只剩这一处,见 `docs/design/player-save-direct-commit.md` §1.2)。 Used by the data-consistency stress test harness. If you renumber/remove it, update both the Go verifier (`go/db/internal/stresstest/`) and the cpp stamper (`cpp/libs/services/scene/player/system/stress_test_probe.{h,cpp}`). Design: `docs/design/data-consistency-stress-testing.md`.
