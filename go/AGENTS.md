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
| Generated RPC stubs | `*/proto/` and `generated/` | Review only; regenerate instead of patching |

## CONVENTIONS
- Services follow go-zero / `zrpc.MustNewServer` startup.
- Reflection is enabled only for dev/test mode checks.
- `login` owns etcd-backed node registration behavior; trace service-discovery issues from `login.go` first.
- Contract changes should be regenerated through repo tooling, not fixed manually inside generated Go files.
- Service-local `proto/` directories and top-level `generated/` are outputs, not primary authoring targets.

## ANTI-PATTERNS
- Hand-editing generated `*.pb.go` / grpc outputs.
- Copying startup wiring between services without preserving each service's config flag shape.
- Treating `go/` as one module with uniform commands; each service root may have its own `go.mod` and lifecycle.

## COMMANDS
```bash
cd go && build.bat                                    # goctl regen + import fixup
cd go && test.bat                                     # L1 unit tests (db consumer + stresstest golden vectors)
cd go && test.bat -Verbose -Race                      # same with verbose output + race detector
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
- `build.bat` mixes `goctl rpc protoc` and raw `protoc`; it is the canonical regeneration path found in-repo.
- `test.bat` / `test.ps1` runs the L1 hermetic unit-test subset (no live Kafka/MySQL/Redis needed — uses miniredis + in-process mocks). It is designed to be cheap enough for every CI build. The two packages it covers:
  - `db/internal/kafka` — `processTaskBatch` coalescing (TC1–TC4), retry-queue write-back (TC5a), per-key applied-seq guard (TC5b), concurrent-worker convergence (TC6), retry payload wire-format (4 cases).
  - `db/internal/stresstest` — `ComputeSig` golden vectors (cross-language parity with `cpp/libs/services/scene/player/system/stress_test_probe.cpp`), `VerifyProbe` round-trip, and tampering detection.
- `data_service` and `scene_manager` follow the same high-level go-zero server shape as login, but only login handles node registration directly.
- **Scene manager**: C++ scene nodes register in etcd; LoadReporter mirrors type/load into Redis. World channel moves use `RebalanceWorldChannelsForZone` (planner `PlanWorldChannelRebalance`). Before changing that path, run `cd go\scene_manager && go test ./internal/logic/ -run TestIntegration_ -count=1` and extend `integration_test.go` if you add new invariants.
- **DB consumer** (`go/db`): the data consistency contract is "per-key monotonic last-write-wins". Three production bugs were found and fixed by the stress test suite — see `docs/design/data-consistency-stress-testing.md` §4 for details. Key mechanisms in `key_ordered_consumer.go`:
  - **Reverse-pass coalescing** in `processTaskBatch`: reads act as barriers so writes before a read are never coalesced away.
  - **`wrapRetryPayload` / `unwrapRetryPayload`**: carries the original Kafka offset inside the retry-queue payload so the per-key guard survives retries. Legacy payloads (bare DBTask bytes) drain as `seq=0` (fail-open).
  - **`shouldApplyBySeq` / `markAppliedSeq`**: Redis key `consumer:applied:<topic>:<key>:<msgType>` prevents stale retries from overwriting newer state.
- When `.proto` changes, regenerate first, then rebuild affected Go services. The `PlayerStressTestProbe` sub-message lives in `proto/common/component/player_comp.proto` and is embedded in `player_database` + `player_database_1`.
