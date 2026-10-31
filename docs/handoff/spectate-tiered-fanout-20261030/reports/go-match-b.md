# GO-MATCH-B 交付说明（Go match 观战可见性闸，未编译，待 Codex 验证）

勘查参考 `wf/scout_visibility.md` 不存在，已直接按工作区最新代码落码（A 批的 newLocalObserverAdd / spectateRejectVerdict / close_tip 已在提交里，在其基础上改）。

## 1. 改动文件
路径前缀 `go/match/internal/`。
- **新增 `logic/spectate_visibility.go`**：`spectateVisibilityFor`（活动局→PRIVATE，其余查表，表外→PRIVATE）、`spectateVisibilityOfMode`（switch 平铺决策表，每个 MatchMode 一行一理由，`decided=false` 只给表外值）、`isSpectatePublic`（只认 PUBLIC）、`formationSpectatePublic`（空/坏字节→false）。
- **`logic/gather.go`（只动两处）**：L345-356 的 `createReq` 字面量加 `SpectateVisibility`（新键最长，gofmt 要求整列对齐，该字面量其余 9 行只改了空白）；L403-404 的 `newSpectateRecord` 加 `createReq.GetSpectateVisibility()`（取在规范化之后）。文件里其它 diff 是「全服匹配」会话正在改的内容，不是本任务的。
- **`logic/spectate.go`**：文件头注释；`newSpectateRecord`（L108）加 visibility 形参；`publishSpectateBattle` 注释；`addSpectateActive`（L200，活跃集合唯一的 ZADD 点）拒收非公开记录；`loadSpectateBattle` 全局分支取 `create.GetSpectateVisibility()`；新增 `removeSpectateActiveMember`（L284），`removeSpectateBattle` 改为委托它。
- **`logic/watchbattlelogic.go`**：WatchBattle 注释；L101 起，在互斥检查之后、读观战标记和清退之前加可见性闸 `selectSpecifiedBattle`；循环里指定场直接用闸里选定的 `watchTarget`，随机模式挑场后按同一判定 `unwatchableOutcome` 复核（替换原 `record==nil` 分支）；新增 `watchTarget` / `selectSpecifiedBattle` / `unwatchableOutcome`（L336-378）；`removeActiveMember` 改为委托；`spectateBattlePublished` 日志里的 "private" 改成 "unpublished"，免得和可见性混淆。
- **`logic/global_spectator.go`**：`prepareGlobalWatch` L193 原来的 `State != 4` 判定改为 `!globalFormationWatchable(formation)`，不满足时仍走 `CloseUnpreparedSpectator(16018)`；随机选中的场也经过这里。新增 `globalFormationWatchable`（L244）。
- **`logic/listwatchablebattleslogic.go`**：过滤非公开记录，只 `removeSpectateActiveMember`，不删记录。
- **`metrics/metrics.go`**：`match_watch_battle_total` 的 Help 和注释加上 outcome `private`。

## 2. 测试
- **新增**：
  - `spectate_visibility_test.go`：`TestSpectateVisibilityPolicy`、`TestSpectateVisibilityDecidesEveryMatchMode`、`TestFormationSpectatePublic`；
  - `gather_spectate_index_test.go`：`TestGatherFreezesSpectateVisibility`，覆盖切磋、1V1、帮会历练三种私密局（记录在、集合 0），以及单人 PVE、整队 PVE 两种公开局（入集合）；新夹具 `startPveSolo`；
  - `spectate_test.go`：`TestWatchBattleSpecifiedPrivateBattleRejectedBeforeSideEffects`（回 16018、回包与「不存在」逐字相同、adds/removes 为空、旧标记与记录都在、残留成员被摘）、`TestWatchBattleRandomModeSkipsPrivateMember`、`TestListWatchableBattlesHidesPrivateButKeepsRecord`；新夹具 `registerPrivateBattle`；
  - `global_spectator_verdict_test.go`：`TestGlobalFormationWatchable`。
- **原先依赖「1V1 / 5V5 可观战」、已改用 PVE_SOLO 的用例**：
  - `registerBattle` 夹具（1V1 改 PVE_SOLO）；
  - `TestSpectateRegisterLoadRemoveRoundTrip`（5V5 改 PVE_SOLO）；
  - `TestGatherEntryEvictsSpectatorAndRegistersBattle`；
  - `TestGatherFailureLeavesNoSpectateIndexAndKeepsObserverEvicted`：顺带把原来无意义的 `spectateBattleKey(members[0])` 断言改成实际的 battle_id；
  - `TestGatherSpectateIndexTransientFailureStillStartsBattle`；
  - `TestGatherPublishRestoresRecordEvictedDuringCreate`；
  - `TestGatherSucceedsWhenActiveZaddFails`；
  - `TestRegisterSpectateBattleZaddFailureIsBestEffort`。

  这几个 gather 用例都改成单人 PVE_SOLO。
- **签名同步**：`registerSpectateBattle` 签名不变，内部按策略算可见性；`TestWriteSpectateRecord*` 传 PRIVATE；`watchbattle_create_window_test.go` 的 `preWriteRecord` 和 D82 改写用例传 PUBLIC。

## 3. 给 Codex 的命令（按顺序）
1. 仓库根目录：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。先和其他会话未提交的 proto 协调，再统一跑。生成后 `go/proto/battle` 应有 `ESpectateVisibility` 和 `CreateBattleRequest.SpectateVisibility`，`go/proto/match` 应有 `SpectateBattleRecord.Visibility`。
2. 在 `go/match` 下：`go build ./...`
3. `go vet ./internal/logic/...`
4. `go test ./internal/logic/... -count=1`

建议加跑只读检查 `gofmt -l internal/logic internal/metrics`，应无输出。

通过标准：全部 exit 0；上面列出的新增和改动用例，以及 spectate / gather / watchbattle / requestbattleticket / global_spectator_verdict 的既有用例全绿。失败时保留 build/vet 原文，以及失败用例 `-run <名> -v` 的输出。

注意：go/match 目前还有「全服匹配」等会话的在制改动（gather.go、global_match.go、catalog 等）。build 若失败，先确认失败点是不是本任务的文件。

## 4. 未完成项、风险与待拍板
1. **行为变化，比说明多走了一步**：Redis 路径指定的场「记录不存在」时，现在也在副作用之前回 not_found，不再先清退旧观战。原因是，如果只有私密局在清退前拒绝、而不存在的场在清退后拒绝，「旧观战有没有被拆」就成了探测私密局是否存在的旁路。全局路径本来就是先占请求头、清退旧观战再判定，私密局和不存在的场处理方式相同，没有这条旁路，未改。
2. **与说明的偏离**：指定场的记录只在闸里读一次，循环里不再重读。可见性随建房冻结，重读拿不到新信息；这样也保住了既有断言「指定场只查一次活跃集合」（`TestWatchBattleMissingRoomActiveLookupFailureKeepsRecord`）。随机模式在循环内仍按同一判定复核；挑到私密成员的那一次会占用一次挑选机会（共 2 次）。
3. **PVP_TEAM（真人人数档 PVP）**：说明里没列，按默认拒绝判为 PRIVATE，请主会话确认。
4. **全局提交点**：`BeginSpectator` 在提交时不复核可见性（authority 不在本任务范围）。理由：create_request 在成局 1→2 时写入一次后不可变，state 4 时必已落库，预检的结论到提交点不会变。如果要求提交点也复核，需要改 `authority/spectator.go`。
5. **全局路径的私密局计为 not_found**：closeTip 只存 16018，所以不出 private，与 metrics 注释一致。全局随机挑到私密成员时不换场，按 16018 关闭，这是说明的要求。正常情况下私密成员进不了集合。
6. **未改的文档**：未改设计文档 §10.1 和 `docs/PROGRESS.md`（不在允许列表），需要主会话登记 B 批进展。Java 版由 JAVA 任务同批对齐，本任务未动 Java 仓。
7. **验证状态**：未编译、未测试，待 Codex 验证。
