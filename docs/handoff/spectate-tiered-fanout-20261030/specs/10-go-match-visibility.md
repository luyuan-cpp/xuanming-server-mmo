# 任务 GO-MATCH-B：Go match 观战可见性闸（B 批）

先读 `00-common-rules.md`（同目录）。勘查参考：`..\wf\scout_visibility.md`（行号已过时，以工作区为准）。
注意：A 批与「本地模式观战按账本发原操作」子任务都改过 `spectate.go` / `watchbattlelogic.go` / `global_spectator.go`
（新增了 `newLocalObserverAdd`、观战标记值改为原件 pb、`spectateRejectVerdict`、`close_tip` 等），动手前完整读最新版本。

## 允许改的文件
- 新增 `go/match/internal/logic/spectate_visibility.go`、`go/match/internal/logic/spectate_visibility_test.go`
- `go/match/internal/logic/gather.go`：**只改两处**（「全服匹配方案讨论」会话已同意，其余段落归它，别碰）：
  1. `runGatherWithOptions` 组 `createReq := &battlepb.CreateBattleRequest{...}` 的字面量里加
     `SpectateVisibility: spectateVisibilityFor(mode, opts.activityContext),`
  2. 第 4.1 步 `record := newSpectateRecord(...)` 实参末尾加 `createReq.GetSpectateVisibility()`
     （取 `proto.Unmarshal(canonical, createReq)` 规范化之后的值；全局模式随 formation 的 create_request 冻结）
- `go/match/internal/logic/spectate.go`、`watchbattlelogic.go`、`global_spectator.go`、`listwatchablebattleslogic.go`
- `go/match/internal/metrics/metrics.go`：只允许在 `match_watch_battle_total` 的 Help / 注释里加一个 outcome `private`
- 测试：`gather_spectate_index_test.go`、`spectate_test.go`、`watchbattle_create_window_test.go`、
  `global_spectator_verdict_test.go`（以及因 `newSpectateRecord` 签名变化必须同步的同包测试夹具）

## 要做的事
1. **单一策略表**（`spectate_visibility.go`）：
   ```go
   // spectateVisibilityFor 是「这局能不能被观战」的唯一权威,建房时调用一次、随 CreateBattleRequest 冻结。
   func spectateVisibilityFor(mode matchpb.MatchMode, activity *battlepb.BattleActivityContext) battlepb.ESpectateVisibility
   ```
   - 活动局（`activity != nil && activity.GetKind() != BATTLE_ACTIVITY_KIND_NONE`，如帮会历练）→ PRIVATE；
   - `PVE_SOLO`、`PVE_TEAM` → PUBLIC；
   - `1V1`、`5V5` → PRIVATE（计分局禁止观战，`docs/design/global-match-hierarchical-pools.md` D7 / §8）；
   - `PVP_CHALLENGE`（点名切磋）→ PRIVATE（无公开依据，默认拒绝）；
   - 其余（3V3 未开放、UNSPECIFIED、未知枚举值）→ PRIVATE。
   用平铺的显式决策表（map 或 switch），不用默认分支偷偷放行。另提供
   `isSpectatePublic(v) bool`（只认 PUBLIC）与 `formationSpectatePublic(createRequest []byte) bool`
   （反序列化 formation 持久的 CreateBattleRequest 读该字段；空或坏字节 → false，fail-closed）。
2. **四处执法**，口径一致：
   - 登记与公开：`newSpectateRecord` 增加 visibility 形参写进记录；`publishSpectateBattle` 对非公开记录只写记录、
     **不 ZADD** `spectate:battles:active`（私密局的记录照写，丢票补签靠它定位 battle 节点）；全局分支
     `loadSpectateBattle`（或等价处）从 create_request 取 `GetSpectateVisibility()`。
   - Redis（dev/test）路径 `watchbattlelogic.go`：指定 battle_id 时，**在任何副作用之前**（例如清退旧观战、写标记之前）
     读记录判可见性；非公开 → 若它在活跃集合里就只 ZREM（不 DEL 记录）→ 回 `ErrBattleNotWatchable`，文案与「不存在」
     相同（「该战斗不存在或已结束」，不泄露存在性），指标 outcome `private`。随机模式挑到非公开成员 → ZREM 后继续挑。
     循环内现有的检查点也要保留同一判定（纵深防御）。
   - 全局（生产）路径 `global_spectator.go`：现有「Formation 状态必须为 4」的判定处同时要求 `formationSpectatePublic`，
     不满足走现有的 `CloseUnpreparedSpectator`（close_tip = `ErrBattleNotWatchable`）出口，结论可重放；随机观战挑到的
     battle 同样复核。
   - 列表 `listwatchablebattleslogic.go`：过滤非公开记录，并只 ZREM 活跃集合成员（新增包级 `removeSpectateActiveMember`，
     现有只 ZREM 的方法改为委托它；不能调会 DEL 记录的 `removeSpectateBattle`）。
3. **测试**（沿用现有夹具 `newGatherSvcCtx`、`stubGatherRPCs`、`stubObserverRPCs`、`setPlayerSessionOnline`、`watchBattle`、`activeIndexSize` 等）：
   - `spectate_visibility_test.go`：`TestSpectateVisibilityPolicy`（表驱动，覆盖每条规则与 activity kind）；
     `TestSpectateVisibilityDecidesEveryMatchMode`（遍历 `matchpb.MatchMode_name`，断言每个枚举值都在显式决策表里，
     新增枚举没进表就红）；`TestFormationSpectatePublic`（公开 / 私密 / 空 / 坏字节）。
   - `gather_spectate_index_test.go`：切磋局记录为 PRIVATE、记录在、活跃集合为 0；PVE_SOLO 局公开入集合；1V1 局私密。
   - `spectate_test.go`：指定观战私密局 → 16018 且无副作用（adds 为空，旧观战标记与记录键都还在）；随机观战跳过私密成员；
     列表隐藏私密局但保留记录。
   - 现有夹具里构造记录 / 建房请求的地方补可见性实参（多数用例是 PVE_SOLO / 1V1：按新策略 1V1 已是私密，
     若某个现有用例依赖「1V1 可观战」，改用 PVE_SOLO 而不是改策略，并在交付说明里点名）。
4. 交付说明里给 Codex 的命令：先 `proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
   （与其他会话的未提交 proto 协调后统一跑），再在 `go/match` 下 `go build ./...`、`go vet ./internal/logic/...`、
   `go test ./internal/logic/... -count=1`。
