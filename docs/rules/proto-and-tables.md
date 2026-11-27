# 协议、配表与生成物规范

> 对应根索引 [AGENTS.md §3 / §4](../../AGENTS.md#3-生成代码与可编辑边界)。本文件定义生成物边界、proto 同步与字段类型约束、tip 错误码轴以及配置表权威 schema 规则。

## 1. 生成代码与可编辑边界

- ❌ 不要手改 `generated/` 或各语言 `proto` 输出树下的生成产物。
- 生成式 C++ handler 文件中含 `///<<< BEGIN WRITING YOUR CODE` 守护段的，自定义逻辑只写在守护段内。
- 不要手写与 proto 重复的并行 struct。

## 2. proto 同步流程

1. proto 源契约在 `proto/`。在仓库根目录执行：
   - 生成器源码变化或尚无工具时：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`
   - 常规重生成：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`
   - 共享 Go 绑定写入 `go/proto/`。db / login 的 gRPC 服务端转发层（`internal/server/*/*server.go`）是手写的，goctl 与 `go/build.bat` / `go/build.ps1` 2026-11-21 起退役：给这两个服务加 RPC 时在对应 `*server.go` 手补转发方法，login 漏接由 `go/login` 的 `TestGoctlServerWrappersImplementEveryRPC` 拦下。
   - 默认生成配置会写同级客户端仓；不需要写客户端仓时使用 `-ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
2. **同级客户端同步（改动客户端可见 `.proto`、`Tip.xlsx`、配置表，或默认配置写入了 `../mmorpg-client/Assets/Scripts/Net/Generated/Rpc/` 时必做）**：切到同级 `mmorpg-client` 运行 `.\gen.bat client`（或在客户端直接运行 `.\gen.bat` 一键串联服务端 `export` + `proto-gen` + 客户端同步），依次完成：
   - `pwsh -File tools/sync_tables.ps1`（同步二进制配置表 `Assets/StreamingAssets/Tables/*.pb` 及 `Table/Generated`、`Net/Generated/Rpc` 的 `.meta`）
   - `pwsh -File tools/gen_proto.ps1`（自动扫描 `Rpc/*.cs` + 递归 `import` + 全部 `generated/code/proto/tip/*_tip.proto` + 底层信封种子，并自动探测同级服务端仓与仓库 `protoc`，无需手维护 `.proto` 清单）
   - `pwsh -File tools/gen_messageids.ps1`
   - `pwsh -File tools/client_compile_check.ps1`（若改到公共签名或测试代码，一并确认受影响的 EditMode/PlayMode 测试程序集编译通过，确保 Unity 启动不进 Safe Mode）
3. 重新编译受影响的 C++ / Go / Java 服务。
4. 字段编号上线后**不复用**，只能 deprecate（`reserved N;` + 注释原因）；开发期已删字段可复用，但须重生并完整编译所有启用 module。

## 3. tip 错误码与配置表 Schema

- **tip 错误码不在 proto 里手写**：`generated/code/proto/tip/*.proto` 全部由导表器从 `data/tip/Tip.xlsx` 生成。
  - 加一个码 = 表里加一行（A 列码名，**全局唯一**，建议带域前缀；B 列中文文案；服务端内部故障在 `fault` 列填 `1`，业务拒绝留空）→ 跑导表器 → 在服务的 `constants.go` 里写 `ErrX = uint32(table.XxxError_kXxxX)`。
  - 「哪个码算故障」也由表生成（`go/shared/generated/tip/faults.go`；Java 网关用同源的 `com.game.table.tip.TipFaults`），**不要在服务里再包一层 fault map**。
  - 新开一个域 = 表里加一行组头 `//xxx_error base=<起始号> width=1000`，并把新生成的 `.pb.cc/.pb.h` 加进 `cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`。
  - 详见 [tip-code-axis.md](../design/tip-code-axis.md)。
- **配置表 schema 事实源**：自 2026-09-03 起不在 xlsx 表头里写，事实源是 `data/schema/<sheet>_table.proto`，类型 / owner / 键 / 索引 / 外键 / 位序 / 注释全用 `data/schema/cfg_options.proto` 的自定义 option 表达；xlsx 只出列名与数据。
  - 加列、加表、改字段号的完整流程与全表索引见 [data/AGENTS.md](../../data/AGENTS.md)。
  - 配表字段号与 proto 同规矩：**上线后不复用，删字段写 `reserved N;`**。

## 4. proto 字段类型约束（强制）

- **坐标/Transform 用 `double`**：`Location` / `Rotation` / `Scale` / `Vector3` / `Velocity` / `Acceleration` 的 `x/y/z` 必须 `double`，匹配大世界客户端精度。
- **`BaseAttributesComp` 用 `uint64`**：`strength` / `stamina` / `health` / `mana` / `critchance` / `armor` / `resistance` / `speed` 必须 `uint64`，避免战斗公式 cast 时窄化。
- **64 位 ID 用 `uint64`**：发号器的永久 ID（`player_id` / `item_id` / `tx_id` / `snapshot_id` 等）、SnowFlake 的 `scene_id`、运行时实例 ID（`entity` / `buff_id` / `skill_id`）必须 `uint64`；号源见 [不变量规范](invariants.md#1-永久-id-与-snowflake-隔离)，不能按字段名推断位布局。
- **单调计数 / 代次用 `uint64`**：`session_incarnation` / `gate_call_seq`。
- **Unix 时间戳用 `uint64`/`int64`**：`created_at` / `expires_at` / `castTime` / `previous_time` / `last_time` / `start` 等必须 64-bit。
- **货币用 `uint64`**：`CurrencyComp.values` / `owed` / `paid` / `balance_before/after`。
- **可用 `uint32`**：`session_id` / `session_version` / `attack_power` / `defense_power` / `max_health` / `skill_table_id`（表查 key）/ `current_frame`。
- **可用 `float`**：`ViewRadius.radius`（AOI range）。
- proto3 enum 第一个值必须是 0。
