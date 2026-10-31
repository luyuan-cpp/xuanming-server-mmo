# GO KNOWLEDGE BASE

## OVERVIEW
`go/` 是一组各自独立的 Go module:一个服务一个 module,另有公共库 `shared/`、`schemamigrate/` 与生成的 `proto/`。`battle/`、`team/` 下只有生成的消息号常量,不是服务。

## STRUCTURE
目录结构、服务清单与端口见同目录 [README.md](README.md),这里不重复维护。

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Login bootstrap | `login/login.go` | etcd registration + unary interceptor |
| Scene manager bootstrap | `scene_manager/scene_manager_service.go` | load reporter + zrpc server; optional `MetricsListenAddr` |
| Scene node roles + world rebalance | `scene_manager/internal/logic/load_reporter.go`, `world_rebalance.go`, `orphan_cleanup.go` | Ops: `docs/ops/scene-node-role-split.md`; design: `docs/design/scene-creation-architecture.md` (rebalance section); CI harness: `scene_manager/internal/logic/integration_test.go` (`TestIntegration_*`, bufconn + miniredis) |
| Data service bootstrap | `data_service/data_service.go` | standard go-zero RPC server wiring |
| DB consumer (Kafka → MySQL+Redis) | `db/db.go`, `db/internal/kafka/key_ordered_consumer.go` | Per-partition workers, write coalescing, retry queue, per-key applied-seq guard |
| Data consistency stress harness | `db/cmd/data_stress/`, `db/cmd/verifier/`, `db/internal/stresstest/` | L2 Kafka driver + verifier. Design: `docs/design/data-consistency-stress-testing.md` |
| Config/runtime flags | `*/etc/` + entry file flags | Reflection only in dev/test |
| 生成协议与消息号 | `proto/`、各服务 `generated/` | 共享 `.pb.go` 在 `go/proto/`；只重生成，不手改 |

## CONVENTIONS
- Services follow go-zero / `zrpc.MustNewServer` startup.
- Reflection is enabled only for dev/test mode checks.
- 各服务在自己的启动入口注册节点；排查服务发现时先读目标服务的入口，再追 `shared/noderegistry`。login、data_service、scene_manager 都直接使用注册工具。
- Contract changes should be regenerated through repo tooling, not fixed manually inside generated Go files.
- 协议事实源是仓库根 `proto/`；`go/proto/` 和各服务 `generated/` 都是产物。db/login 的临时服务内 `proto/` 由包装生成脚本清理，不能作为共享绑定的读取入口。

## ANTI-PATTERNS
- Hand-editing generated `*.pb.go` / grpc outputs.
- Copying startup wiring between services without preserving each service's config flag shape.
- Treating `go/` as one module with uniform commands; each service root may have its own `go.mod` and lifecycle.

## COMMANDS
```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml # 仓库根：共享绑定重生成
cd go && build.bat                                    # 仅 db/login 的 goctl 包装、导入修正与临时产物清理
cd go && test.bat                                     # L1 隔离测试子集，范围见 test.ps1 的 L1Suites
cd go && test.bat -Verbose -Race                      # 同一子集，增加详细输出与竞态检测
cd go\login && go run login.go
cd go\login && go test ./...
cd go\login && go mod tidy
cd go\scene_manager && go test ./...
cd go\data_service && go test ./...
cd go\db && go test -v ./internal/kafka/...           # consumer unit tests only
cd go\db && go test -v ./internal/stresstest/...      # ComputeSig golden vectors + probe round-trip
cd go\db && go build ./cmd/data_stress ./cmd/verifier # build L2 stress tools
```

## NOTES
- `build.bat` 仅转调 `build.ps1`；完整 `.pb.go` 重生成由 protogen 完成，流程见根 `AGENTS.md` §4。
- `test.bat` / `test.ps1` 执行不依赖真实 Kafka/MySQL/Redis 的 L1 子集，使用 miniredis 与进程内替身。当前覆盖 db 的 `internal/playerstore`、`internal/logic/pkg/proto_sql`、`internal/config`、`cmd/migrate`，以及 shared 的 `placement`；完整清单以 `test.ps1` 的 `L1Suites` 为准，不代表所有服务测试。
- login、data_service、scene_manager 均采用 go-zero 服务装配并各自管理注册生命周期。
- **Scene manager**: C++ scene nodes register in etcd; LoadReporter mirrors type/load into Redis. World channel moves use `RebalanceWorldChannelsForZone` (planner `PlanWorldChannelRebalance`). Before changing that path, run `cd go\scene_manager && go test ./internal/logic/ -run TestIntegration_ -count=1` and extend `integration_test.go` if you add new invariants.
- **Player store** (`go/db`): 存盘服务,scene 直接提交 TiDB,不经 Redis 和 Kafka。契约见 `docs/design/player-save-direct-commit.md`。
- When `.proto` changes, regenerate first, then rebuild affected Go services. The `PlayerStressTestProbe` sub-message lives in `proto/common/component/player_comp.proto` and is embedded in `player_database` + `player_database_1`.
