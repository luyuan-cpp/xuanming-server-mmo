# 任务 LOCAL-MARK：本地模式观战标记的一致性修复（Go match）

状态（2026-10-30）：已基于 P1-MATCH 落完的版本全部落码，**未编译、未跑测试，待 Codex 按第 6 节验证**。定稿函数签名见第 3 节末尾。

只影响本地模式（`GlobalMatch.Enabled=false`，dev / test）。生产强制走全局路径，全局路径已满足本文的规则。先读 `00-common-rules.md`。

## 1. 缺陷

10-24 的修复让 Add / Remove 带上了账本三字段，但标记的去留还停在「尽力而为」，比全局路径（`cancelGlobalSpectator`）和 Java（`LocalObserverOperation`、`clearPreviousMark`、`SpectateGatherHooks`）都弱：

| 现状（`spectate.go`、`watchbattlelogic.go`） | 后果 |
|---|---|
| `stopWatching`、补偿路径在 Remove 失败后照样删标记 | battle 上的观众还挂着，恢复入口（原 op）却丢了。同场重看时，账本只认旧 op，新 op 一律被拒，而旧 op 已无从取消：只能等直连断开加 30s 宽限，或等该场结束 |
| `deleteSpectateWatching` 是无条件 DEL | 并发时误删别的 WatchBattle 刚抢到的新标记，新观众的恢复入口丢失，gather 也清退不到它 |
| Remove 按观战记录的 node_id 定位（`spectateTarget`） | 记录会被 D82 改写，节点也可能换实例，取消请求依赖一份会变的数据；读记录失败或记录已不在时干脆不发 Remove |
| `addObserverOutcomeUnknown` 只把截止 / 取消 / 不可用算作结果未知 | `Internal`、`Unknown` 等状态也可能发生在 battle 已处理之后，此时直接删标记不安全 |
| 本地路径 `observer_name` 用 `session.Account` | 登录账号是私有信息，会进观众广播；全局路径和 Java 本地路径都用角色公开名 |
| 本地路径没有跨会话栅栏（独立评审发现） | 重连后旧会话上还在途的 WatchBattle 会拆新会话的观战，再用新会话的路由挂旧请求选的场；JoinQueue 和全局路径都有这道复核 |

## 2. 规则（只此一条）

**标记是 battle 上可能挂着的观众唯一的恢复入口。只有下面两种情况才能按值删除，其余一律保留，靠下一次 WatchBattle 沿同一个 op 重试，并由 TTL（战斗最长期限 + 60s）兜底：**

1. 拿到取消证明：RemoveObserver 回包 `cancelled=true`，且带回原 op 和原实例。
2. 这个 op 确定不会登记：Add 被 battle 明确拒绝（`rejectTip != 0`），或请求截止前根本没发出。

标记的值解析不出原件时没有可重试的取消，按值清掉（否则只是把玩家卡到 TTL）。

项目未上线、数据可清空，不写任何旧格式兼容；十进制 battle_id 旧标记就是普通的非法值。

## 3. 改法

### `go/match/internal/discovery/node_watcher.go`（已落码）

`EntryOfInstance(instanceId)`：按实例 uuid 定位，只命中同一实例；查不到不代表实例已死。

### `go/match/internal/logic/spectate.go`

- 新增按值删除，形状照 `queue.go` 的 `ticketDelCasScript` / `deleteTicketIfOwnedCtx`：

  ```go
  const spectateWatchingDelCasScript = `if redis.call("GET", KEYS[1]) ~= ARGV[1] then
  	return 0
  end
  return redis.call("DEL", KEYS[1])`

  func releaseWatchingMark(svcCtx *svc.ServiceContext, playerId uint64, mark string) error // 独立 ctx,spectateRedisOpTimeout
  ```

  删掉 `deleteSpectateWatching`，全部调用点改用它。值不同（返回 0）不算错误：说明已不是这次的标记。
- `removeObserver` 改为 `cancelObserver(svcCtx, original, reason) error`：`EntryOfInstance(original.target_instance_id)` 定位，再调 `removeObserverFn`，字段全取自原件。返回 nil 才是取消证明。不再读观战记录，也不再判「节点换了实例就当已没了」（`EntryOfInstance` 查不到 = 没有证明）。
- `stopWatching(svcCtx, playerId, raw, reason) error`：decode 失败 → 记 Error、按值删；否则 `cancelObserver`，失败就返回错误且标记不动；成功再 `releaseWatchingMark`。**这是唯一的「取消后释放」路径**，WatchBattle 的补偿也调它（传自己的 mark）。不再调用 `loadSpectateBattle`。
- `stopWatchingIfAny`：`stopWatching` 出错只记日志（不阻断开局）。注释改写：标记保留；客户端收到参战分配会关掉观战直连，旧场席位由 battle 的断线宽限回收。
- `addObserver`：`addObserverFn` 的任何错误都包成 `errAddObserverOutcomeUnknown`（没拿到业务回包就不能断定没落地），删掉 `addObserverOutcomeUnknown` 和不再使用的 `codes` / `status` 导入。请求截止前没发出的那条（`ctx.Err()`）保持「确定没发出」，不算未知。
- 取消证明的判据抽成 `observerCancelProven(req, reply)`，生产实现与表驱动用例共用（桩替换 `removeObserverFn` 后判据本身测不到）。
- `decodeWatchingMark` 注释去掉旧格式字样。`spectateTarget` 签名和语义不变（Add 与 P1 的 Attach 都用它）。

### `go/match/internal/logic/watchbattlelogic.go`

- `admitDirectSeat` 去掉 `observerName` 参数，在里面用 `challengePublicName(l.ctx, l.svcCtx, playerId)` 组原件，不再用 `session.Account`。只有 L0 才查，L1 / L2 观众不多一次 data_service 调用。
- 读会话、判在线、组路由整段提到清退旧观战之前，并加跨会话栅栏：`session_id` 或 `gate_instance_id` 与 SessionDetails 不一致 → 回 `ErrSpectateOffline`，计 `offline`。没有会话 metadata 的调用（内部调用、测试）不做这道复核，与 `callerSessionStale` 同口径。离线或旧会话的请求因此没有任何副作用。
- 清退分支：`stopWatching` 出错 → `internalErr("清退旧观战", err)`（16004，不登记新操作）。旧观众可能还挂着，再挂新观众就是同时观战两场。
- 复查自我清退：`stopWatching(l.svcCtx, playerId, mark, "concurrent_queue")`，出错只记日志，仍回「匹配中无法观战」。
- Add 失败，结果未知：`stopWatching(..., mark, "watch_outcome_unknown")`，出错只记日志，结论不变（RPC 失败不换场、不转 L1）。
- Add 失败，明确拒绝或请求没发出：`releaseWatchingMark`。失败 → `internalWatchResponse("回滚观战标记", ...)`（16004），不转 L1、不换场，否则换场的 SETNX 会得到误导的「已在观战」。残留标记由下一次 WatchBattle 清掉。
- 满员转 L1：`admitDirectSeat` 返回时这个 op 的标记已按值删除，`spectateDirectFullToRelay` 与 `admitSpectateLobby` 都在它之后。

### 定稿函数签名

```go
// logic/spectate.go
func stopWatchingIfAny(svcCtx *svc.ServiceContext, playerId uint64, reason string)
func stopWatching(svcCtx *svc.ServiceContext, playerId uint64, raw, reason string) error
func cancelObserver(svcCtx *svc.ServiceContext, original *battlepb.AddObserverRequest, reason string) error
func releaseWatchingMark(svcCtx *svc.ServiceContext, playerId uint64, mark string) error
func observerCancelProven(req *battlepb.RemoveObserverRequest, reply *battlepb.RemoveObserverResponse) bool
func addObserver(ctx context.Context, endpoint string, original *battlepb.AddObserverRequest) (rejectTip uint32, err error)
func spectateTarget(ctx context.Context, s *svc.ServiceContext, node uint32, battle uint64) (endpoint, instance string, err error) // 未变

// logic/watchbattlelogic.go
func (l *WatchBattleLogic) admitDirectSeat(target watchTarget, playerId uint64, routing *battlepb.BattleRouting, randomMode bool) (done *matchpb.WatchBattleResponse, rejectTip uint32, err error)

// discovery/node_watcher.go
func (w *NodeWatcher) EntryOfInstance(instanceId string) (NodeEntry, error)
```

已删除：`removeObserver`、`deleteSpectateWatching`、`addObserverOutcomeUnknown`。

## 4. 测试（`spectate_test.go`、`request_deadline_fence_test.go`，miniredis + 桩 battle）

新增：
1. 换场和同场重看（表驱动）：Remove 未获证明 → 回 16004、不发新 Add、标记不变；恢复后重试，Remove 带同一个 op，接入成功、标记换成新原件。
2. 结果未知且 Remove 未获证明 → 回 16018、标记保留；下一次 WatchBattle 沿同一个 op 取消后接入。
3. 复查自我清退且 Remove 未获证明 → 回「匹配中无法观战」、标记保留。
4. `stopWatchingIfAny`：Remove 未获证明 → 标记保留；实例不在注册表 → 不发 Remove、标记保留。
5. 按值删除：Add 被拒、Remove 成功时，把标记换成另一份新原件（用 `onAdd` / `onRemove` 钩子模拟并发），结束后新原件必须还在。
6. 回滚标记失败（前置钩子按脚本注入 Redis 错误，注意 go-zero 熔断，单个 svcCtx 失败不超过 5 次）→ 回 16004：随机模式不换场，满员不转 L1、不升档；恢复后下一次 WatchBattle 能接入。另加一条：满员转 L1 时，Attach 发出之前标记已回滚。
7. 观众名：有 `challengeNameClient` 时用角色公开名；data_service 不可用时为空串，绝不回退到登录账号。
8. 账本桩：同一个旧 op 的 Remove 重放不能摘掉新 op 的观众。
9. 取消拿到证明但删标记失败 → 回 16004、不发新 Add；下一次沿同一个 op 再取消后接入（`TestWatchBattleRewatchMarkReleaseFailureRegistersNothing`）。
10. 取消证明的判据（`TestObserverCancelProven`）；请求截止前没发出不算结果未知（`TestAddObserverBeforeSendDeadlineIsNotOutcomeUnknown`）。
11. 跨会话栅栏：旧会话或换了 gate 实例的请求回 16019，不发 Remove / Add、标记不动，当前会话照常通过（`TestWatchBattleFromSupersededSessionHasNoSideEffects`）；离线玩家带旧标记同样没有副作用（`TestWatchBattleOfflineObserverKeepsPreviousMark`）。

修改：
- `TestStopWatchingIfAnyPaths`：记录已不在时也发 Remove（靠证明释放）；实例被替换 → 不发 Remove、标记保留；删掉十进制旧标记那一段。
- `TestStopWatchingIfAnyWithCorruptRecordStillClearsMark` 改名 `TestStopWatchingIfAnyIgnoresCorruptRecord`：清退只看原件，记录损坏也照发 Remove，拿到证明后删标记。
- `TestWatchBattleBindsObserverWithGateOnlyRouting`：观众名断言改为公开名。
- `TestWatchBattleRpcFailureKeepsIndexRollsBackMark`：断言多一次 Remove（结果未知）。
- `TestWatchBattleUsesSessionPlayerIdOverRequestBody`：会话 metadata 补上当前会话号和 gate 实例。
- `TestWatchBattleAddObserverOutcomeUnknownRemovesObserver`：加一行 `Internal`。

## 5. 收尾

- 文档：`battle-spectate-tiered-fanout.md` §10.1 本地模式条目；`turn-based-battle-server.md` §10.4 标记一行；`battle-transport-decision.md` 里「尽力 RemoveObserver」「失败只记日志」两句；`docs/PROGRESS.md` 只追加。
- 残余（已写进 §10.1，都只在本地模式）：① battle 实例崩溃且不再回来时，挂在上面的玩家，以及崩溃后、租约到期前试图观战的玩家，WatchBattle 回 16004 直到标记 TTL 到期。Remove 这半边与 Java 同口径；Add 连接失败 Java 能判「确定未送达」并回滚，Go 的 gRPC 分不清，按结果未知保留。② 切磋没有票据：gather 读完标记之后、Prepare 写战斗锁之前到达的 WatchBattle 复查全过，玩家会短暂既是观众又是参战者，标记还在，可恢复。本地路径的 Redis TTL 和尽力清退不等于全局路径的事务互斥。
- Java：本地原操作已由 `add334cb` 落码；待做项（robot 走 dev 口、本地 163 跨会话栅栏）登记在 `PARITY.md`。

## 6. 给 Codex 的验证（工作目录 `go/match`，按顺序）

1. `gofmt -l internal/logic internal/discovery`，期望没有输出。
2. `go vet ./internal/logic/... ./internal/discovery/...`
3. `go test ./internal/discovery/... ./internal/logic/... -count=1`
4. `go build ./...`
5. 可选，本地 robot 观战冒烟：先按 AGENTS §6.2 清空 Redis，确认 `GlobalMatch.Enabled=false`，在 `robot` 目录跑 `./robot.exe -c etc/battle_smoke.yaml`，期望 `BATTLE_SMOKE_OK`。

通过标准：全部 exit 0。失败时保留 `go test -v -run <失败用例名>` 的完整输出。
