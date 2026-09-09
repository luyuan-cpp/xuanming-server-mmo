package main

// attribute-smoke 场景:单机器人对本地服务端做「角色属性加点」端到端冒烟。
//
// 兑现 docs/design/player-attribute-allocation.md 的服务端契约,逐条断言:
//   0. 预备(让脚本可重复运行):等级归 1、两个池洗点(1 级免费)、切回首个方案、GM 发金币;
//   1. GetAttributePanel 拿到全量面板(池 / 维度 / 方案 / 二级属性都非空);
//   2. GmSetPlayerLevel 1 → 30:属性点总量恰好 +145(AttributePool 行 1:每级 5 点,总量不落库、按等级换算);
//   3. AutoAllocateAttributePoints 只算不落 —— 面板不变,建议里的增量总和 = 剩余点;
//   4. AllocateAttributePoints 按建议提交 → 剩余点归零、二级属性真实变大;
//   5. 幂等:重发同一份"目标值"被判"无变化"(tip 144),不重复扣点;
//   6. 只增不减:提交比已分配更小的值被拒(tip 134);
//   7. 相性池单项上限:提交超过 dimension_cap 被拒(tip 135);
//   8. 未解锁池(仙魔点,60 级解锁)在低等级被拒(tip 131);
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
	"strconv"
	"time"

	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"

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

// 池 id 与表 data/AttributePool.xlsx 对齐(1 属性点 / 2 相性点 / 3 仙魔点)。
const (
	attrPoolPrimary  uint32 = 1
	attrPoolAffinity uint32 = 2
	attrPoolImmortal uint32 = 3
)

// AttributePool 行 1 的 points_per_level(表值;冒烟用它算 1→30 级应得的精确点数)。
const attrPrimaryPointsPerLevel uint32 = 5

// tip id 一律读导表器生成的枚举,**不写字面量**(AGENTS.md §7 不变量 5)。
// 2026-09-09 修正:这组常量原本手抄的是 131/134/... 那套旧号,tip 码轴改成按段发号
// (commit 95b5641d0)之后就全部作废了,负向断言(期待被拒的那几步)因此永远对不上且零报错。
// 改成引用枚举后,再发生一次同类改号会在编译期断,而不是在冒烟里静默变绿/变红。
var (
	tipAttributePoolLocked           = uint32(tiptable.AttributeError_kAttributePoolLocked)
	tipAttributePointsCannotDecrease = uint32(tiptable.AttributeError_kAttributePointsCannotDecrease)
	tipAttributeDimensionCapExceeded = uint32(tiptable.AttributeError_kAttributeDimensionCapExceeded)
	tipAttributeSchemeLimitReached   = uint32(tiptable.AttributeError_kAttributeSchemeLimitReached)
	tipAttributeSchemeSwitchCooldown = uint32(tiptable.AttributeError_kAttributeSchemeSwitchCooldown)
	tipAttributeNothingToChange      = uint32(tiptable.AttributeError_kAttributeNothingToChange)
)

// attributeSmokeSession 是一个已登录进场的机器人会话 + 面板序号游标。
type attributeSmokeSession struct {
	gc     *pkg.GameClient
	player *gameobject.Player
	stats  *metrics.Stats
	seq    uint64 // 已消费到的面板序号,保证每次等的是"这次请求的新面板"
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
	session = &attributeSmokeSession{gc: gc, player: player, stats: stats}
	zap.L().Info("[attribute-smoke] step 1: in scene", zap.Uint64("player", gc.PlayerId))

	// ---- 步骤 0:预备,把账号拉回已知起点(同账号可反复跑) ----
	panel, err := session.call(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: 1})
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
	for _, poolId := range []uint32{attrPoolPrimary, attrPoolAffinity} {
		p, tip, err := session.callTolerant(game.SceneAttributeClientPlayerResetAttributePointsMessageId,
			&scene.ResetAttributePointsRequest{PoolId: poolId}, tipAttributeNothingToChange)
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
		&scene.GetAttributePanelRequest{})
	if err != nil {
		fail("get-panel", "%v", err)
	}
	if len(panel.Pools) == 0 || len(panel.Dimensions) == 0 || len(panel.Schemes) == 0 {
		fail("panel-shape", "pools=%d dimensions=%d schemes=%d(三者都必须非空)",
			len(panel.Pools), len(panel.Dimensions), len(panel.Schemes))
	}
	if panel.Derived == nil || panel.Derived.MaxHealth == 0 {
		fail("panel-derived", "二级属性缺失或气血上限为 0:%v", panel.Derived)
	}
	if panel.Level != 1 {
		fail("panel-level", "预备后应为 1 级,面板 level=%d", panel.Level)
	}
	before := findPool(panel, attrPoolPrimary)
	if before == nil {
		fail("pool-missing", "面板里没有 pool_id=%d", attrPoolPrimary)
	}
	if before.Remaining != before.Total {
		fail("panel-clean", "预备后属性点应全额可用:total=%d remaining=%d", before.Total, before.Remaining)
	}
	zap.L().Info("[attribute-smoke] step 2: panel ok",
		zap.Int("pools", len(panel.Pools)), zap.Int("dimensions", len(panel.Dimensions)),
		zap.Uint32("level", panel.Level), zap.Uint64("max_health", panel.Derived.MaxHealth))

	// ---- 步骤 3:1 → 30 级,点数精确 +145(总量不落库,靠等级换算) ----
	const targetLevel = 30
	panel, err = session.call(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: targetLevel})
	if err != nil {
		fail("gm-set-level", "%v", err)
	}
	if panel.Level != targetLevel {
		fail("level-mismatch", "设 %d 级后面板 level=%d", targetLevel, panel.Level)
	}
	primary := findPool(panel, attrPoolPrimary)
	wantGain := attrPrimaryPointsPerLevel * (targetLevel - 1)
	if primary == nil || primary.Total-before.Total != wantGain {
		fail("points-growth", "1→%d 级属性点应恰好 +%d:%d -> %v", targetLevel, wantGain, before.Total, primary)
	}
	zap.L().Info("[attribute-smoke] step 3: level up grants points",
		zap.Uint32("level", panel.Level), zap.Uint32("total", primary.Total),
		zap.Uint32("remaining", primary.Remaining))

	// ---- 步骤 4:自动加点(只算不落) ----
	if err := session.send(game.SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId,
		&scene.AutoAllocateAttributePointsRequest{PoolId: attrPoolPrimary}); err != nil {
		fail("auto-send", "%v", err)
	}
	suggested, err := session.waitSuggestion(attrPoolPrimary)
	if err != nil {
		fail("auto-allocate", "%v", err)
	}
	var delta uint32
	for dimensionId, want := range suggested {
		delta += want - allocatedOf(panel, dimensionId)
	}
	if delta != primary.Remaining {
		fail("auto-delta", "建议增量 %d != 剩余点 %d(自动加点应把剩余点分完)", delta, primary.Remaining)
	}
	fresh, err := session.call(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{})
	if err != nil {
		fail("panel-after-auto", "%v", err)
	}
	if findPool(fresh, attrPoolPrimary).Remaining != primary.Remaining {
		fail("auto-mutated", "自动加点动了服务器状态:剩余点 %d -> %d",
			primary.Remaining, findPool(fresh, attrPoolPrimary).Remaining)
	}
	zap.L().Info("[attribute-smoke] step 4: auto suggestion is preview-only",
		zap.Uint32("delta", delta), zap.Int("dimensions", len(suggested)))

	// ---- 步骤 5:确认加点 → 剩余点归零、二级属性变大 ----
	healthBefore := fresh.Derived.MaxHealth
	panel, err = session.call(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolPrimary, Allocated: suggested})
	if err != nil {
		fail("allocate", "%v", err)
	}
	primary = findPool(panel, attrPoolPrimary)
	if primary.Remaining != 0 {
		fail("allocate-remaining", "分完后剩余点应为 0,实为 %d", primary.Remaining)
	}
	if panel.Derived.MaxHealth <= healthBefore {
		fail("derived-not-growing", "加点后气血上限没变大:%d -> %d", healthBefore, panel.Derived.MaxHealth)
	}
	zap.L().Info("[attribute-smoke] step 5: allocate applied",
		zap.Uint64("max_health", panel.Derived.MaxHealth),
		zap.Uint64("physical_attack", panel.Derived.PhysicalAttack),
		zap.Uint64("magic_attack", panel.Derived.MagicAttack),
		zap.Uint64("defense", panel.Derived.Defense),
		zap.Uint64("speed", panel.Derived.Speed))

	// ---- 步骤 6:幂等 —— 重发同一份目标值被判"无变化" ----
	if tip := session.callExpectTip(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolPrimary, Allocated: suggested}); tip != tipAttributeNothingToChange {
		fail("idempotent", "重发同一份目标值应回 tip=%d(无变化),实得 %d", tipAttributeNothingToChange, tip)
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
	if tip := session.callExpectTip(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolPrimary, Allocated: lower}); tip != tipAttributePointsCannotDecrease {
		fail("decrease-guard", "减点应回 tip=%d,实得 %d", tipAttributePointsCannotDecrease, tip)
	}

	// ---- 步骤 8:相性单项上限 ----
	affinity := findPool(panel, attrPoolAffinity)
	if affinity == nil || affinity.DimensionCap == 0 {
		fail("affinity-cap", "相性池缺失或没有单项上限:%v", affinity)
	}
	affinityDim := firstDimensionOfPool(panel, attrPoolAffinity)
	over := map[uint32]uint32{affinityDim: affinity.DimensionCap + 1}
	if tip := session.callExpectTip(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolAffinity, Allocated: over}); tip != tipAttributeDimensionCapExceeded {
		fail("cap-guard", "超相性上限(%d)应回 tip=%d,实得 %d",
			affinity.DimensionCap, tipAttributeDimensionCapExceeded, tip)
	}

	// ---- 步骤 9:未解锁池 ----
	immortal := findPool(panel, attrPoolImmortal)
	if immortal == nil {
		fail("immortal-missing", "面板里没有仙魔池")
	}
	if immortal.Unlocked {
		fail("immortal-unlocked", "%d 级不该解锁仙魔池(unlock_level=%d)", panel.Level, immortal.UnlockLevel)
	}
	immortalDim := firstDimensionOfPool(panel, attrPoolImmortal)
	if tip := session.callExpectTip(game.SceneAttributeClientPlayerAllocateAttributePointsMessageId,
		&scene.AllocateAttributePointsRequest{PoolId: attrPoolImmortal,
			Allocated: map[uint32]uint32{immortalDim: 1}}); tip != tipAttributePoolLocked {
		fail("locked-guard", "未解锁池加点应回 tip=%d,实得 %d", tipAttributePoolLocked, tip)
	}
	zap.L().Info("[attribute-smoke] steps 6-9: guards ok(幂等 / 只增不减 / 上限 / 未解锁)")

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
		&scene.CreateAttributeSchemeRequest{}, tipAttributeSchemeLimitReached)
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
			fail("create-cost", "开新方案应扣 %d 金(面板 create_scheme_cost_gold),实扣 %d",
				createCost, goldBeforeCreate-goldAfterCreate)
		}
	}
	if otherSchemeId == 0 {
		fail("scheme-pick", "没有可切换的第二个方案:%v", panel.Schemes)
	}
	if panel, err = session.switchSchemeWaitingCooldown(panel, otherSchemeId); err != nil {
		fail("switch-scheme", "%v", err)
	}
	if created {
		if allocatedOf(panel, anyDim) != 0 {
			fail("scheme-not-clean", "新方案应是干净的,dimension=%d 已分配 %d", anyDim, allocatedOf(panel, anyDim))
		}
		if pp := findPool(panel, attrPoolPrimary); pp.Remaining != pp.Total {
			fail("scheme-points", "新方案的点数应全额可用:total=%d remaining=%d", pp.Total, pp.Remaining)
		}
	}
	// 切回原方案(要等切换冷却):原方案的加点必须原样还在
	if panel, err = session.switchSchemeWaitingCooldown(panel, activeSchemeId); err != nil {
		fail("switch-back", "%v", err)
	}
	if got := allocatedOf(panel, anyDim); got != allocatedInScheme1 {
		fail("scheme-isolation", "切回原方案后加点串档:期望 %d,实得 %d", allocatedInScheme1, got)
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
	reborn, err := session.relogin(cfg)
	if err != nil {
		fail("relogin", "%v", err)
	}
	session, gc, player = reborn, reborn.gc, reborn.player
	panel, err = session.call(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{})
	if err != nil {
		fail("relogin-panel", "%v", err)
	}
	if panel.Level != targetLevel {
		fail("persist-level", "重登后 level=%d,期望 %d", panel.Level, targetLevel)
	}
	if len(panel.Schemes) != persistSchemes || panel.ActiveSchemeId != persistActive {
		fail("persist-schemes", "重登后 schemes=%d active=%d,期望 %d/%d",
			len(panel.Schemes), panel.ActiveSchemeId, persistSchemes, persistActive)
	}
	if got := allocatedOf(panel, anyDim); got != persistAlloc {
		fail("persist-alloc", "重登后 dimension=%d 已分配 %d,期望 %d", anyDim, got, persistAlloc)
	}
	if panel.Derived.MaxHealth != persistMaxHealth {
		fail("persist-derived", "重登后气血上限 %d,期望 %d(二级属性应由持久化的分配重算出同样的值)",
			panel.Derived.MaxHealth, persistMaxHealth)
	}
	zap.L().Info("[attribute-smoke] step 12: relogin restored persisted state",
		zap.Uint64("player", gc.PlayerId), zap.Int("schemes", len(panel.Schemes)),
		zap.Uint32("allocated", persistAlloc), zap.Uint64("max_health", persistMaxHealth))

	// ---- 步骤 11:30 级洗点收费 → 精确扣金币、分配清零、全额返还 ----
	primary = findPool(panel, attrPoolPrimary)
	resetCost := primary.ResetCostGold
	if resetCost == 0 {
		fail("reset-cost-table", "30 级洗点应收费(reset_free_below_level=30),面板 reset_cost_gold=0")
	}
	goldBeforeReset, err := readGoldBalance(gc, player, stats, attributeSmokeRpcTimeout)
	if err != nil {
		fail("gold-read", "%v", err)
	}
	totalBefore := primary.Total
	panel, err = session.call(game.SceneAttributeClientPlayerResetAttributePointsMessageId,
		&scene.ResetAttributePointsRequest{PoolId: attrPoolPrimary})
	if err != nil {
		fail("reset", "%v", err)
	}
	primary = findPool(panel, attrPoolPrimary)
	if primary.Remaining != totalBefore {
		fail("reset-refund", "洗点后应全额返还:total=%d remaining=%d", totalBefore, primary.Remaining)
	}
	if allocatedOf(panel, anyDim) != 0 {
		fail("reset-clear", "洗点后 dimension=%d 仍有 %d 点", anyDim, allocatedOf(panel, anyDim))
	}
	goldAfterReset, err := readGoldBalance(gc, player, stats, attributeSmokeRpcTimeout)
	if err != nil {
		fail("gold-read", "%v", err)
	}
	if goldBeforeReset-goldAfterReset != resetCost {
		fail("reset-cost", "洗点应扣 %d 金(面板 reset_cost_gold),实扣 %d", resetCost, goldBeforeReset-goldAfterReset)
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

// call 发一个属性 RPC 并等它带回的新面板(成功路径);tip != 0 视为失败。
func (s *attributeSmokeSession) call(messageId uint32, request proto.Message) (*scene.AttributePanelInfo, error) {
	panel, tip, err := s.callTolerant(messageId, request)
	if err != nil {
		return nil, err
	}
	if tip != 0 {
		return nil, fmt.Errorf("服务器拒绝: tip=%d", tip)
	}
	return panel, nil
}

// callTolerant 发一个属性 RPC:成功回 (面板, 0, nil);被拒且 tip 在 allowedTips 里回 (nil, tip, nil);
// 其它拒绝回 error。用于"可能已经是目标状态"的预备步骤(洗过点的池再洗 → 无变化)。
// 只认"来源 = 本次请求消息号"的面板:GmSetPlayerLevel 先推一份(170)再回响应(175),不看来源会错位消费,
// 之后每一步都拿到上一步的旧面板。
func (s *attributeSmokeSession) callTolerant(messageId uint32, request proto.Message, allowedTips ...uint32) (*scene.AttributePanelInfo, uint32, error) {
	if err := s.send(messageId, request); err != nil {
		return nil, 0, err
	}
	deadline := time.Now().Add(attributeSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		if tip := s.player.GetAttributeLastTip(); tip != 0 {
			for _, allowed := range allowedTips {
				if tip == allowed {
					return nil, tip, nil
				}
			}
			return nil, tip, fmt.Errorf("服务器拒绝: tip=%d", tip)
		}
		p, seq := s.player.GetAttributePanel()
		if p != nil && seq > s.seq && s.player.GetAttributePanelSource() == messageId {
			s.seq = seq
			return p, 0, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, 0, fmt.Errorf("等响应超时 message_id=%d", messageId)
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
	return &attributeSmokeSession{gc: gc, player: player, stats: s.stats}, nil
}

// callExpectTip 发一个预期会被拒绝的请求,返回服务器给的 tip id(0 = 没被拒或超时,均视为断言失败)。
func (s *attributeSmokeSession) callExpectTip(messageId uint32, request proto.Message) uint32 {
	_, tip, _ := s.callTolerant(messageId, request, 0xFFFFFFFF)
	return tip
}

// switchSchemeWaitingCooldown 切方案;撞上切换冷却(tip 138)就按面板的 switch_cooldown_until 等到期再切一次。
func (s *attributeSmokeSession) switchSchemeWaitingCooldown(panel *scene.AttributePanelInfo, schemeId uint32) (*scene.AttributePanelInfo, error) {
	for attempt := 0; attempt < 2; attempt++ {
		p, tip, err := s.callTolerant(game.SceneAttributeClientPlayerSwitchAttributeSchemeMessageId,
			&scene.SwitchAttributeSchemeRequest{SchemeId: schemeId}, tipAttributeSchemeSwitchCooldown)
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

func (s *attributeSmokeSession) send(messageId uint32, request proto.Message) error {
	// 每次发请求前清 tip、把游标同步到当前面板序号:上一步没被消费的面板(推送 / 迟到响应)不能算作本次结果
	s.player.SetAttributePanel(nil, 0, 0)
	_, cur := s.player.GetAttributePanel()
	s.seq = cur
	if err := s.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	s.stats.MsgSent()
	return nil
}

func (s *attributeSmokeSession) waitSuggestion(poolId uint32) (map[uint32]uint32, error) {
	deadline := time.Now().Add(attributeSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		gotPool, suggested := s.player.GetAttributeSuggestion()
		if gotPool == poolId && len(suggested) > 0 {
			return suggested, nil
		}
		if tip := s.player.GetAttributeLastTip(); tip != 0 {
			return nil, fmt.Errorf("服务器拒绝自动加点: tip=%d", tip)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("等自动加点建议超时")
}

// ---------------------------------------------------------------------------
// 面板取值
// ---------------------------------------------------------------------------

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

func firstDimensionOfPool(panel *scene.AttributePanelInfo, poolId uint32) uint32 {
	if panel == nil {
		return 0
	}
	for _, dimension := range panel.Dimensions {
		if dimension.PoolId == poolId {
			return dimension.DimensionId
		}
	}
	return 0
}
