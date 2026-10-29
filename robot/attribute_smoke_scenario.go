package main

// attribute-smoke 场景:单机器人对本地服务端做「角色属性加点」端到端冒烟。
//
// 兑现 docs/design/player-attribute-allocation.md 的服务端契约,逐条断言:
//   0. 预备(让脚本可重复运行):等级归 1、属性点池洗点(1 级免费)、切回首个方案、GM 发金币;
//   1. GetAttributePanel 拿到全量面板(池 / 维度 / 方案 / 二级属性都非空);
//   2. GmSetPlayerLevel 1 → 30:属性点总量恰好 +145(AttributePool 行 1:每级 5 点,总量不落库、按等级换算);
//   3. AutoAllocateAttributePoints 只算不落 —— 面板不变,建议里的增量总和 = 剩余点;
//   4. AllocateAttributePoints 按建议提交 → 剩余点归零、二级属性真实变大;
//   5. 幂等:重发同一份"目标值"被判"无变化"(kAttributeNothingToChange),不重复扣点;
//   6. 只增不减:提交比已分配更小的值被拒(kAttributePointsCannotDecrease);
//      (原 7 相性池单项上限 / 8 仙魔池未解锁两步 2026-09-14 随两池删除;规则由纯规则单测覆盖)
//   9. CreateAttributeScheme:金币按面板下发的 create_scheme_cost_gold 精确扣减;
//      SwitchAttributeScheme 到新方案 → 新方案是干净的(分配为 0、点数满额);
//      等切换冷却过去后切回 → 原方案的加点原样恢复(方案互不串档);
//  10. 下线重登:等级 / 方案数 / 当前方案 / 已分配 / 二级属性与下线前一致(attribute_component 落库往返 + db 列迁移);
//  11. ResetAttributePoints(30 级收费)→ 金币按 reset_cost_gold 精确扣减、分配清零、点数全额返还。
//
// 结果约定(供外层脚本消费):
//   全过 → 日志一行 `ATTRIBUTE_SMOKE_OK …`,退出码 0;
//   任一步失败 → `ATTRIBUTE_SMOKE_FAIL step=… reason=…`,退出码 1。

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧的 DevPasswordAuth(开发用密码认证)。
// 9101 避开压测区段与 battle-smoke 的 9001/9002。
const attributeSmokeAccount = "robot_9101"

// 单次属性 RPC 的等待预算。属性操作是纯内存 + 表查询,正常 <50ms;
// 给 10s 是覆盖 gate→scene 首次路由建立。
const attributeSmokeRpcTimeout = 10 * time.Second

// 预备阶段发的金币:覆盖一次开方案(1000)+ 一次 30 级洗点(500)再留余量。
const attributeSmokeGoldGrant int64 = 100000

// 池 id 与表 data/AttributePool.xlsx 对齐:角色只剩属性点池(2026-09-14 删相性点 / 仙魔点池)。
const attrPoolPrimary uint32 = 1

// AttributePool 行 1 的 points_per_level(表值;冒烟用它算 1→30 级应得的精确点数)。
const attrPrimaryPointsPerLevel uint32 = 5

// tip id 一律读导表器生成的枚举,**不写字面量**(AGENTS.md §7 不变量 5)。
// 2026-09-09 修正:这组常量原本手抄的是 131/134/... 那套旧号,tip 码轴改成按段发号
// (commit 95b5641d0)之后就全部作废了,负向断言(期待被拒的那几步)因此永远对不上且零报错。
// 改成引用枚举后,再发生一次同类改号会在编译期断,而不是在冒烟里静默变绿/变红。
var (
	tipAttributePointsCannotDecrease = uint32(tiptable.AttributeError_kAttributePointsCannotDecrease)
	tipAttributeSchemeLimitReached   = uint32(tiptable.AttributeError_kAttributeSchemeLimitReached)
	tipAttributeSchemeSwitchCooldown = uint32(tiptable.AttributeError_kAttributeSchemeSwitchCooldown)
	tipAttributeNothingToChange      = uint32(tiptable.AttributeError_kAttributeNothingToChange)
)

// attributeSmokeSession 是一个已登录进场的机器人会话。属性请求经 caller 发出、只等本次请求自己的回包
// (按 MessageContent.id 认领),所以不再需要面板序号游标。
type attributeSmokeSession struct {
	gc *pkg.GameClient
	// player 只用作 readGoldBalance / gmAddGold 的兼容参数(它们已不读写它),属性请求不经 Player 状态。
	player *gameobject.Player
	stats  *metrics.Stats
	caller *smokeCaller
}

// newAttributeSmokeSession 包装一个已进场的连接。gc 的 RecvLoop 必须已启动(prepareBehaviorClient 返回前已起),
// 否则 caller 等不到回包。
func newAttributeSmokeSession(gc *pkg.GameClient, player *gameobject.Player, stats *metrics.Stats) *attributeSmokeSession {
	return &attributeSmokeSession{
		gc:     gc,
		player: player,
		stats:  stats,
		caller: newSmokeCaller(gc, stats, attributeSmokeAccount, attributeSmokeRpcTimeout, nil),
	}
}

// RunAttributeSmoke 是 main.go `mode: attribute-smoke` 的入口。
func RunAttributeSmoke(cfg *config.Config) {
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	var session *attributeSmokeSession
	cleanup := func() {
		if session != nil && session.gc != nil {
			_ = leaveGame(session.gc, stats)
			sendDisconnectBestEffort(session.gc)
			gameobject.PlayerList.Delete(session.gc.PlayerId)
			session.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("ATTRIBUTE_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 步骤 1:登录进场 ----
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		fail("resolve-gate", "%v", err)
	}
	port, _ := strconv.Atoi(portStr)
	gc, player, err := prepareBehaviorClient(host, port, attributeSmokeAccount, cfg.Password, stats, tokenPayload, tokenSig)
	if err != nil {
		fail("login", "account=%s err=%v", attributeSmokeAccount, err)
	}
	session = newAttributeSmokeSession(gc, player, stats)
	zap.L().Info("[attribute-smoke] step 1: in scene", zap.Uint64("player", gc.PlayerId))

	// ---- 步骤 0:预备,把账号拉回已知起点(同账号可反复跑) ----
	panel, err := session.call(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: 1}, &scene.GmSetPlayerLevelResponse{})
	if err != nil {
		fail("prep-level", "%v", err)
	}
	// 切回 id 最小的方案(上一轮可能停在别的方案上);冷却中就等它过去
	firstSchemeId := panel.Schemes[0].SchemeId
	for _, s := range panel.Schemes {
		if s.SchemeId < firstSchemeId {
			firstSchemeId = s.SchemeId
		}
	}
	if panel.ActiveSchemeId != firstSchemeId {
		if panel, err = session.switchSchemeWaitingCooldown(panel, firstSchemeId); err != nil {
			fail("prep-switch", "%v", err)
		}
	}
	// 1 级洗点免费(reset_free_below_level=30);没有分配时服务器回"无变化",同样视为干净
	for _, poolId := range []uint32{attrPoolPrimary} {
		p, tip, err := session.callTolerant(game.SceneAttributeClientPlayerResetAttributePointsMessageId,
			&scene.ResetAttributePointsRequest{PoolId: poolId}, &scene.ResetAttributePointsResponse{}, tipAttributeNothingToChange)
		if err != nil {
			fail("prep-reset", "pool=%d %v", poolId, err)
		}
		if p != nil {
			panel = p
		}
		_ = tip
	}
	if _, err := gmAddGold(gc, player, stats, attributeSmokeGoldGrant, attributeSmokeRpcTimeout); err != nil {
		fail("prep-gold", "%v", err)
	}
	goldStart, err := readGoldBalance(gc, player, stats, attributeSmokeRpcTimeout)
	if err != nil {
		fail("prep-gold-read", "%v", err)
	}
	zap.L().Info("[attribute-smoke] step 0: account reset to level 1 / clean pools / funded",
		zap.Uint32("active_scheme", panel.ActiveSchemeId), zap.Int("schemes", len(panel.Schemes)),
		zap.Uint64("gold", goldStart))

	// ---- 步骤 2:面板形状 ----
	panel, err = session.call(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{}, &scene.GetAttributePanelResponse{})
	if err != nil {
		fail("get-panel", "%v", err)
	}
	if len(panel.Pools) == 0 || len(panel.Dimensions) == 0 || len(panel.Schemes) == 0 {
		fail("panel-shape", "pools=%d dimensions=%d schemes=%d (all three must be nonempty)",
			len(panel.Pools), len(panel.Dimensions), len(panel.Schemes))
	}
	if panel.Derived == nil || panel.Derived.MaxHealth == 0 {
		fail("panel-derived", "Derived attributes are missing or max health is 0: %v", panel.Derived)
	}
	if panel.Level != 1 {
		fail("panel-level", "Expected level 1 after setup; panel level=%d", panel.Level)
	}
	before := findPool(panel, attrPoolPrimary)
	if before == nil {
		fail("pool-missing", "Panel has no pool_id=%d", attrPoolPrimary)
	}
	if before.Remaining != before.Total {
		fail("panel-clean", "All attribute points should be available after setup: total=%d remaining=%d", before.Total, before.Remaining)
	}
	zap.L().Info("[attribute-smoke] step 2: panel ok",
		zap.Int("pools", len(panel.Pools)), zap.Int("dimensions", len(panel.Dimensions)),
		zap.Uint32("level", panel.Level), zap.Uint64("max_health", panel.Derived.MaxHealth))

	// ---- 步骤 3:1 → 30 级,点数精确 +145(总量不落库,靠等级换算) ----
	const targetLevel = 30
	panel, err = session.call(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: targetLevel}, &scene.GmSetPlayerLevelResponse{})
	if err != nil {
		fail("gm-set-level", "%v", err)
	}
	if panel.Level != targetLevel {
		fail("level-mismatch", "After setting level %d, panel level=%d", targetLevel, panel.Level)
	}
	primary := findPool(panel, attrPoolPrimary)
	wantGain := attrPrimaryPointsPerLevel * (targetLevel - 1)
	if primary == nil || primary.Total-before.Total != wantGain {
		fail("points-growth", "Attribute points from level 1 to %d should increase by exactly %d: %d -> %v", targetLevel, wantGain, before.Total, primary)
	}
	zap.L().Info("[attribute-smoke] step 3: level up grants points",
		zap.Uint32("level", panel.Level), zap.Uint32("total", primary.Total),
		zap.Uint32("remaining", primary.Remaining))

	// ---- 步骤 4:自动加点(只算不落) ----
	suggested, err := session.autoAllocate(attrPoolPrimary)
	if err != nil {
		fail("auto-allocate", "%v", err)
	}
	var delta uint32
	for dimensionId, want := range suggested {
		delta += want - allocatedOf(panel, dimensionId)
	}
	if delta != primary.Remaining {
		fail("auto-delta", "Suggested delta %d != remaining points %d (automatic allocation should spend all remaining points)", delta, primary.Remaining)
	}
	fresh, err := session.call(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{}, &scene.GetAttributePanelResponse{})
	if err != nil {
		fail("panel-after-auto", "%v", err)
	}
	if findPool(fresh, attrPoolPrimary).Remaining != primary.Remaining {
		fail("auto-mutated", "Automatic allocation changed server state: remaining points %d -> %d",
			primary.Remaining, findPool(fresh, attrPoolPrimary).Remaining)
	}
	zap.L().Info("[attribute-smoke] step 4: auto suggestion is preview-only",
		zap.Uint32("delta", delta), zap.Int("dimensions", len(suggested)))

	// ---- 步骤 5:确认加点 → 剩余点归零、二级属性变大 ----
	derivedBefore := derivedSum(fresh.Derived)
	panel, err = session.call(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolPrimary, Allocated: suggested},
		&scene.AllocateAttributePointsResponse{})
	if err != nil {
		fail("allocate", "%v", err)
	}
	primary = findPool(panel, attrPoolPrimary)
	if primary.Remaining != 0 {
		fail("allocate-remaining", "Remaining points should be 0 after allocation, got %d", primary.Remaining)
	}
	// 只断言"六项二级属性上限总和变大",不绑定哪一项:自动加点方案是表驱动的(2026-09-13 起全投本职业主属性,
	// 通用档全投力量 → 只涨物伤),原先写死"气血上限变大"会随方案调整误报
	if derivedSum(panel.Derived) <= derivedBefore {
		fail("derived-not-growing", "Derived attributes did not increase after allocation: sum %d -> %d", derivedBefore, derivedSum(panel.Derived))
	}
	zap.L().Info("[attribute-smoke] step 5: allocate applied",
		zap.Uint64("max_health", panel.Derived.MaxHealth),
		zap.Uint64("physical_attack", panel.Derived.PhysicalAttack),
		zap.Uint64("magic_attack", panel.Derived.MagicAttack),
		zap.Uint64("defense", panel.Derived.Defense),
		zap.Uint64("speed", panel.Derived.Speed))

	// ---- 步骤 6:幂等 —— 重发同一份目标值被判"无变化" ----
	if err := session.callExpectTip(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolPrimary, Allocated: suggested},
		&scene.AllocateAttributePointsResponse{}, tipAttributeNothingToChange); err != nil {
		fail("idempotent", "Resending the same target values should return tip=%d (unchanged): %v", tipAttributeNothingToChange, err)
	}

	// ---- 步骤 7:只增不减 ----
	var anyDim uint32
	for dimensionId, want := range suggested {
		if want > 0 {
			anyDim = dimensionId
			break
		}
	}
	lower := map[uint32]uint32{anyDim: allocatedOf(panel, anyDim) - 1}
	if err := session.callExpectTip(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolPrimary, Allocated: lower},
		&scene.AllocateAttributePointsResponse{}, tipAttributePointsCannotDecrease); err != nil {
		fail("decrease-guard", "Decreasing points should return tip=%d: %v", tipAttributePointsCannotDecrease, err)
	}

	// 原步骤 8(相性池单项上限)/ 步骤 9(仙魔池未解锁)随两池删除一并移除(2026-09-14);
	// "单项上限"与"未解锁"两条规则宝宝池仍在用,由纯规则单测 attribute_allocation_rules_test 覆盖。
	zap.L().Info("[attribute-smoke] steps 6-7: guards ok (idempotent / increases only)")

	// ---- 步骤 10:方案:开新方案精确扣金币、新方案干净、切回后不串档 ----
	allocatedInScheme1 := allocatedOf(panel, anyDim)
	activeSchemeId := panel.ActiveSchemeId
	createCost := panel.CreateSchemeCostGold
	goldBeforeCreate, err := readGoldBalance(gc, player, stats, attributeSmokeRpcTimeout)
	if err != nil {
		fail("gold-read", "%v", err)
	}
	var otherSchemeId uint32
	created := false
	p, tip, err := session.callTolerant(game.SceneAttributeClientPlayerCreateAttributeSchemeMessageId,
		&scene.CreateAttributeSchemeRequest{}, &scene.CreateAttributeSchemeResponse{}, tipAttributeSchemeLimitReached)
	if err != nil {
		fail("create-scheme", "%v", err)
	}
	if tip == tipAttributeSchemeLimitReached {
		// 上一轮已把方案开满(表上限 3):改用任意一个非当前方案继续验证隔离
		for _, s := range panel.Schemes {
			if s.SchemeId != activeSchemeId {
				otherSchemeId = s.SchemeId
				break
			}
		}
		zap.L().Info("[attribute-smoke] scheme limit reached, reuse existing scheme", zap.Uint32("scheme", otherSchemeId))
	} else {
		panel = p
		created = true
		for _, s := range panel.Schemes {
			if s.SchemeId != activeSchemeId {
				otherSchemeId = s.SchemeId // 新建的 id 最大,循环末尾留下的就是它
			}
		}
		goldAfterCreate, err := readGoldBalance(gc, player, stats, attributeSmokeRpcTimeout)
		if err != nil {
			fail("gold-read", "%v", err)
		}
		if goldBeforeCreate-goldAfterCreate != createCost {
			fail("create-cost", "Creating a scheme should deduct %d gold (panel create_scheme_cost_gold), actually deducted %d",
				createCost, goldBeforeCreate-goldAfterCreate)
		}
	}
	if otherSchemeId == 0 {
		fail("scheme-pick", "No second scheme is available to switch to: %v", panel.Schemes)
	}
	if panel, err = session.switchSchemeWaitingCooldown(panel, otherSchemeId); err != nil {
		fail("switch-scheme", "%v", err)
	}
	if created {
		if allocatedOf(panel, anyDim) != 0 {
			fail("scheme-not-clean", "A new scheme should be clean; dimension=%d already has %d allocated", anyDim, allocatedOf(panel, anyDim))
		}
		if pp := findPool(panel, attrPoolPrimary); pp.Remaining != pp.Total {
			fail("scheme-points", "All points in a new scheme should be available: total=%d remaining=%d", pp.Total, pp.Remaining)
		}
	}
	// 切回原方案(要等切换冷却):原方案的加点必须原样还在
	if panel, err = session.switchSchemeWaitingCooldown(panel, activeSchemeId); err != nil {
		fail("switch-back", "%v", err)
	}
	if got := allocatedOf(panel, anyDim); got != allocatedInScheme1 {
		fail("scheme-isolation", "Allocation leaked across schemes after switching back: expected %d, got %d", allocatedInScheme1, got)
	}
	zap.L().Info("[attribute-smoke] step 10: scheme cost / isolation ok",
		zap.Uint32("scheme_a", activeSchemeId), zap.Uint32("scheme_b", otherSchemeId), zap.Bool("created", created))

	// ---- 步骤 12(洗点之前):下线重登,持久化往返必须原样恢复 ----
	// 覆盖 player_database_loader 的 Marshal/Unmarshal 接线、Kafka 存盘任务、db 服务的 attribute_component 列:
	// 三者任一断裂,重登后方案 / 已分配都会退回默认"方案一 + 0 分配",而单会话冒烟看不见。
	persistAlloc := allocatedOf(panel, anyDim)
	persistSchemes := len(panel.Schemes)
	persistActive := panel.ActiveSchemeId
	persistMaxHealth := panel.Derived.MaxHealth
	persistDerived := derivedSum(panel.Derived)
	reborn, err := session.relogin(cfg)
	if err != nil {
		fail("relogin", "%v", err)
	}
	session, gc, player = reborn, reborn.gc, reborn.player
	panel, err = session.call(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{}, &scene.GetAttributePanelResponse{})
	if err != nil {
		fail("relogin-panel", "%v", err)
	}
	if panel.Level != targetLevel {
		fail("persist-level", "After relogin level=%d, expected %d", panel.Level, targetLevel)
	}
	if len(panel.Schemes) != persistSchemes || panel.ActiveSchemeId != persistActive {
		fail("persist-schemes", "After relogin schemes=%d active=%d, expected %d/%d",
			len(panel.Schemes), panel.ActiveSchemeId, persistSchemes, persistActive)
	}
	if got := allocatedOf(panel, anyDim); got != persistAlloc {
		fail("persist-alloc", "After relogin dimension=%d has %d allocated, expected %d", anyDim, got, persistAlloc)
	}
	if panel.Derived.MaxHealth != persistMaxHealth || derivedSum(panel.Derived) != persistDerived {
		fail("persist-derived", "After relogin max health %d / derived attribute sum %d, expected %d / %d (derived attributes should recompute to the same values from persisted allocations)",
			panel.Derived.MaxHealth, derivedSum(panel.Derived), persistMaxHealth, persistDerived)
	}
	zap.L().Info("[attribute-smoke] step 12: relogin restored persisted state",
		zap.Uint64("player", gc.PlayerId), zap.Int("schemes", len(panel.Schemes)),
		zap.Uint32("allocated", persistAlloc), zap.Uint64("max_health", persistMaxHealth))

	// ---- 步骤 11:30 级洗点收费 → 精确扣金币、分配清零、全额返还 ----
	primary = findPool(panel, attrPoolPrimary)
	resetCost := primary.ResetCostGold
	if resetCost == 0 {
		fail("reset-cost-table", "Resetting points at level 30 should cost gold (reset_free_below_level=30), but panel reset_cost_gold=0")
	}
	goldBeforeReset, err := readGoldBalance(gc, player, stats, attributeSmokeRpcTimeout)
	if err != nil {
		fail("gold-read", "%v", err)
	}
	totalBefore := primary.Total
	panel, err = session.call(game.SceneAttributeClientPlayerResetAttributePointsMessageId,
		&scene.ResetAttributePointsRequest{PoolId: attrPoolPrimary}, &scene.ResetAttributePointsResponse{})
	if err != nil {
		fail("reset", "%v", err)
	}
	primary = findPool(panel, attrPoolPrimary)
	if primary.Remaining != totalBefore {
		fail("reset-refund", "All points should be refunded after reset: total=%d remaining=%d", totalBefore, primary.Remaining)
	}
	if allocatedOf(panel, anyDim) != 0 {
		fail("reset-clear", "After reset dimension=%d still has %d points", anyDim, allocatedOf(panel, anyDim))
	}
	goldAfterReset, err := readGoldBalance(gc, player, stats, attributeSmokeRpcTimeout)
	if err != nil {
		fail("gold-read", "%v", err)
	}
	if goldBeforeReset-goldAfterReset != resetCost {
		fail("reset-cost", "Reset should deduct %d gold (panel reset_cost_gold), actually deducted %d", resetCost, goldBeforeReset-goldAfterReset)
	}
	zap.L().Info("[attribute-smoke] step 11: reset refunded all points, gold charged",
		zap.Uint32("remaining", primary.Remaining), zap.Uint64("cost", resetCost))

	zap.L().Info(fmt.Sprintf(
		"ATTRIBUTE_SMOKE_OK player_id=%d level=%d pools=%d dimensions=%d schemes=%d max_health=%d gold=%d",
		gc.PlayerId, panel.Level, len(panel.Pools), len(panel.Dimensions), len(panel.Schemes),
		panel.Derived.MaxHealth, goldAfterReset))
	cleanup()
	_ = zap.L().Sync()
}

// ---------------------------------------------------------------------------
// 会话辅助
// ---------------------------------------------------------------------------

// attributePanelResponse 是回全量面板的角色属性响应的公共形状(error_message + panel):GetAttributePanel /
// AllocateAttributePoints / ResetAttributePoints / CreateAttributeScheme / SwitchAttributeScheme / GmSetPlayerLevel
// 的响应都满足。pet-smoke 借角色属性通道(改等级 / 查角色面板)时也用它。
type attributePanelResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
	GetPanel() *scene.AttributePanelInfo
}

// requestAttributePanel 经 caller 发一个回全量面板的角色属性请求,只等本次请求自己的回包并直接解析
// (按 MessageContent.id 认领,docs/design/client-rpc-callback.md §6)。response 传该请求对应响应类型的空值。
// 结局:受理回 (面板, 0, nil);scene 在响应体里拒绝、且码在 allowedTips 里回 (nil, tip, nil);其余一律返回 error:
// scene 的其它拒绝码(tip 一并返回)、gate 信封拒绝、超时 / 断线 / 没发出、受理却不带面板。
// allowedTips 只对 scene 的业务码生效:信封拒绝说明请求没到 scene 的属性逻辑,现有调用方容忍 / 期待的都是业务码。
//
// 迁移前等的是生成 handler 写进 gameobject.Player 的「面板序号 + 来源消息号」,信封错误则由 prepareBehaviorClient 的
// 收包回调翻译成同一个 tip,与业务拒绝分不清。GmSetPlayerLevel 先推一份 NotifyAttributePanelChanged 再回响应,
// 当时要靠来源消息号才不错位;现在回包按请求号认领,拿到的就是本次的响应。attribute-smoke / pet-smoke 不读
// Player 上那份属性状态,所以本次回包直接解析(交接 §8.1 做法 1),不再经 handler。但 equip / equip_forge 冒烟的
// panelAfter 还靠它识别 NotifyAttributePanelChanged 推送,所以推送必须继续经 prepareBehaviorClient 的
// onMessage → handler.MessageBodyHandler 写 Player,那份状态与对应 handler 写入不能删。
//
// 调用约束:caller 所在连接的 RecvLoop 必须已启动;不得在 RecvLoop goroutine 上调用(同步等回包会自锁)。
func requestAttributePanel(caller *smokeCaller, messageId uint32, request proto.Message, response attributePanelResponse,
	allowedTips ...uint32) (*scene.AttributePanelInfo, uint32, error) {
	envelopeTip, err := caller.call(messageId, request, response)
	if err != nil {
		return nil, 0, err
	}
	return attributePanelOutcome(messageId, envelopeTip, response, allowedTips...)
}

// attributePanelOutcome 是 requestAttributePanel 的结局判定(纯函数,单测见 equip_smoke_scenario_test.go)。
// envelopeTip != 0 时 response 未填充。attribute / pet / equip 三个冒烟的属性请求都经它判定,
// 改判据只改这一处。
func attributePanelOutcome(messageId, envelopeTip uint32, response attributePanelResponse,
	allowedTips ...uint32) (*scene.AttributePanelInfo, uint32, error) {
	if envelopeTip != 0 {
		return nil, 0, sceneRequestEnvelopeRejection(messageId, envelopeTip)
	}
	if tip := response.GetErrorMessage().GetId(); tip != 0 {
		if slices.Contains(allowedTips, tip) {
			return nil, tip, nil
		}
		return nil, tip, fmt.Errorf("rejected by scene: message_id=%d tip=%d", messageId, tip)
	}
	// 迁移前受理却不带面板的回包不推进序号,表现为等满超时;现在回包就是本次的,不会再有下一份,直接判失败
	panel := response.GetPanel()
	if panel == nil {
		return nil, 0, fmt.Errorf("message_id=%d was accepted but the response carries no panel", messageId)
	}
	return panel, 0, nil
}

// sceneRequestEnvelopeRejection 把 gate 信封拒绝(MessageContent.error_message 非空、响应体为空)报成 error,
// 与 scene 的业务拒绝分开:不能当成 scene 业务逻辑给的码去断言(1003 也可能是结局未知,见 sceneRequestEnvelopeHint)。
// attribute / pet / equip 冒烟共用这一份,保证同一次运行里信封拒绝的日志格式只有一种。
func sceneRequestEnvelopeRejection(messageId, tip uint32) error {
	return fmt.Errorf("rejected by gate envelope: message_id=%d tip=%d%s", messageId, tip, sceneRequestEnvelopeHint(tip))
}

// call 发一个属性 RPC 并取它带回的新面板(成功路径);任何拒绝都视为失败。
func (s *attributeSmokeSession) call(messageId uint32, request proto.Message, response attributePanelResponse) (*scene.AttributePanelInfo, error) {
	panel, _, err := requestAttributePanel(s.caller, messageId, request, response)
	return panel, err
}

// callTolerant 发一个属性 RPC:成功回 (面板, 0, nil);scene 以 allowedTips 里的码拒绝回 (nil, tip, nil);
// 其它结局回 error(见 requestAttributePanel)。用于"可能已经是目标状态"的步骤(洗过点的池再洗 → 无变化)。
func (s *attributeSmokeSession) callTolerant(messageId uint32, request proto.Message, response attributePanelResponse,
	allowedTips ...uint32) (*scene.AttributePanelInfo, uint32, error) {
	return requestAttributePanel(s.caller, messageId, request, response, allowedTips...)
}

// relogin 下线(LeaveGame + Disconnect)后用同一账号重新登录进场,返回新会话。
// 中间等 2s 让 scene 的异步存盘落到 db,否则重登可能读到旧档(HandlePlayerAsyncSaved 会把
// 过快的重登判成"重连取代登出")。
func (s *attributeSmokeSession) relogin(cfg *config.Config) (*attributeSmokeSession, error) {
	_ = leaveGame(s.gc, s.stats)
	sendDisconnectBestEffort(s.gc)
	gameobject.PlayerList.Delete(s.gc.PlayerId)
	s.gc.Close()
	time.Sleep(2 * time.Second)

	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate: %w", err)
	}
	port, _ := strconv.Atoi(portStr)
	gc, player, err := prepareBehaviorClient(host, port, attributeSmokeAccount, cfg.Password, s.stats, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("relogin: %w", err)
	}
	return newAttributeSmokeSession(gc, player, s.stats), nil
}

// callExpectTip 发一个预期被 scene 以 want 拒绝的请求:响应体 tip == want 返回 nil;受理、别的拒绝码、gate 信封拒绝
// 与超时都返回描述实际结局的 error。迁移前这几种结局挤在一个 tip 返回值里(受理与超时都是 0,信封码与业务码不分)。
func (s *attributeSmokeSession) callExpectTip(messageId uint32, request proto.Message, response attributePanelResponse, want uint32) error {
	_, tip, err := s.callTolerant(messageId, request, response, want)
	if err != nil {
		return err
	}
	if tip != want {
		return fmt.Errorf("message_id=%d was accepted", messageId)
	}
	return nil
}

// switchSchemeWaitingCooldown 切方案;撞上切换冷却(kAttributeSchemeSwitchCooldown)就按面板的 switch_cooldown_until
// 等到期再切一次。冷却拒绝的响应不带面板,所以到期时刻取调用方给的上一份面板。
func (s *attributeSmokeSession) switchSchemeWaitingCooldown(panel *scene.AttributePanelInfo, schemeId uint32) (*scene.AttributePanelInfo, error) {
	for attempt := 0; attempt < 2; attempt++ {
		p, tip, err := s.callTolerant(game.SceneAttributeClientPlayerSwitchAttributeSchemeMessageId,
			&scene.SwitchAttributeSchemeRequest{SchemeId: schemeId}, &scene.SwitchAttributeSchemeResponse{},
			tipAttributeSchemeSwitchCooldown)
		if err != nil {
			return nil, err
		}
		if tip == 0 {
			return p, nil
		}
		wait := time.Duration(int64(panel.SwitchCooldownUntil)-time.Now().Unix()+1) * time.Second
		if wait < time.Second {
			wait = time.Second
		}
		if wait > 90*time.Second {
			return nil, fmt.Errorf("切换冷却过长: %s", wait)
		}
		zap.L().Info("[attribute-smoke] switch cooldown, waiting", zap.Duration("wait", wait))
		time.Sleep(wait)
	}
	return nil, fmt.Errorf("切换方案 %d 两次都撞冷却", schemeId)
}

// autoAllocate 发 AutoAllocateAttributePoints 并直接解析本次回包,返回 poolId 的建议目标已分配值(只算不落)。
// 响应不带面板,所以不走 requestAttributePanel。迁移前等的是生成 handler 写进 Player 的建议,全包只有这里读,
// 改为直接解析(交接 §8.1 做法 1)。scene 拒绝、gate 信封拒绝、超时,以及建议不属于该池或为空,都返回 error。
func (s *attributeSmokeSession) autoAllocate(poolId uint32) (map[uint32]uint32, error) {
	messageId := uint32(game.SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId)
	response := &scene.AutoAllocateAttributePointsResponse{}
	envelopeTip, err := s.caller.call(messageId, &scene.AutoAllocateAttributePointsRequest{PoolId: poolId}, response)
	if err != nil {
		return nil, err
	}
	if envelopeTip != 0 {
		return nil, sceneRequestEnvelopeRejection(messageId, envelopeTip)
	}
	if tip := response.GetErrorMessage().GetId(); tip != 0 {
		return nil, fmt.Errorf("auto allocation rejected by scene: tip=%d", tip)
	}
	// 迁移前这两种回包不满足等待条件,表现为等满超时;现在回包就是本次的,不会再有下一份,直接判失败
	if response.GetPoolId() != poolId || len(response.GetSuggested()) == 0 {
		return nil, fmt.Errorf("auto allocation returned pool_id=%d with %d suggested dimensions, expected pool_id=%d with a nonempty suggestion",
			response.GetPoolId(), len(response.GetSuggested()), poolId)
	}
	return response.GetSuggested(), nil
}

// ---------------------------------------------------------------------------
// 面板取值
// ---------------------------------------------------------------------------

// derivedSum 是六项二级属性**上限**之和(不含当前气血 / 法力):用来断言"加点确实生效",
// 而不绑定具体涨了哪一项 —— 涨哪一项由自动加点方案表决定。
func derivedSum(d *scene.DerivedAttributeInfo) uint64 {
	if d == nil {
		return 0
	}
	return d.MaxHealth + d.MaxMana + d.PhysicalAttack + d.MagicAttack + d.Speed + d.Defense
}

func findPool(panel *scene.AttributePanelInfo, poolId uint32) *scene.AttributePoolInfo {
	if panel == nil {
		return nil
	}
	for _, pool := range panel.Pools {
		if pool.PoolId == poolId {
			return pool
		}
	}
	return nil
}

func allocatedOf(panel *scene.AttributePanelInfo, dimensionId uint32) uint32 {
	if panel == nil {
		return 0
	}
	for _, dimension := range panel.Dimensions {
		if dimension.DimensionId == dimensionId {
			return dimension.Allocated
		}
	}
	return 0
}
