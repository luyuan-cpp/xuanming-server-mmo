package main

// battle-smoke 的 cross_zone 子模式:两个机器人分别登进不同 zone,
// 双双 JoinQueue(1V1) 后断言被匹配进同一场对局,并自动战斗到终局。
//
// 兑现 docs/design/cross-zone-matchmaking.md §8 "robot" / §9 验证清单第 3 条:
//   机器人 A:登 zone_a → JoinQueue(1V1) → NotifyBattleStart → SetAutoBattle → NotifyBattleEnd;
//   机器人 B:登 zone_b → 同上;
//   主流程:断言 A/B 的 gate 地址不同(落在不同 zone 的证据)、battle_id 相同、
//           双方回合数 ≥ 1。
//
// 结果约定(供外层脚本消费):
//   全部通过 → 日志一行 `CROSS_ZONE_MATCH_OK battle_id=… zone_a=… zone_b=… a_turns=… b_turns=…
//                a_direct_turns=… b_direct_turns=…`(跳过直连时后两项为 -1),
//              退出码 0;
//   任一步失败 → 日志一行 `CROSS_ZONE_MATCH_FAIL step=… reason=…`,退出码 1。
//
// 与原 PVE + 观战流程(battle_smoke_scenario.go)共用 battleSmokeLogin /
// battleSmokeBot / 超时常量,但不复用 spectateReady 屏障:1V1 双方都是
// 真实参战者,没有"观战者要先到位"的时序问题。

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"proto/battle"
	"proto/match"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
)

// 与原模式(robot_9001/9002)错开账号:两种冒烟可能被脚本先后甚至并行跑,
// 同账号会互相顶成 ReplaceLogin。
const (
	crossZoneAccountA = "robot_9003"
	crossZoneAccountB = "robot_9004"
)

// crossZoneBattleConfigId 是 1V1 JoinQueue 携带的 battle_config_id。
//
// 合法性依据(禁止只凭 PVE 冒烟的习惯照抄):
//   - go/match joinqueuelogic.go 对 MATCH_MODE_1V1 只取 required=2,不查
//     battle_config_id;gather.go 原样透传给 CreateBattleRequest.battle_config_id;
//   - cpp/libs/services/battle/system/turn_battle_engine.cpp Initialize:
//     FindDungeon(battle_config_id) 查不到只是回落缺省 30 回合(不报错),
//     InitMonsters 仅在 PVE 模式执行 —— 所以 PVP 下任何值都不会让
//     CreateBattle 失败;
//   - 取 1 是 generated/tables/Dungeon.json 的第一行(time_limit=1800s →
//     300 回合上限),与 PVE 冒烟同值,便于对照日志。
const crossZoneBattleConfigId = 1

// crossZoneMatchModeOf 把配置里的模式名映射到协议枚举。
// 配置层已把非 "1v1" 拒掉(config.BattleSmokeConfig.validate),这里只是兜底。
func crossZoneMatchModeOf(mode string) (match.MatchMode, bool) {
	switch mode {
	case "", "1v1":
		return match.MatchMode_MATCH_MODE_1V1, true
	default:
		return match.MatchMode_MATCH_MODE_UNSPECIFIED, false
	}
}

// crossZoneFighterResult 是一侧参战 goroutine 的产出。
type crossZoneFighterResult struct {
	step     string // 失败时所在步骤(成功为空)
	err      error  // 失败原因(成功为 nil)
	battleId uint64
	outcome  int32
	turns    int
	// directTurns 是从战斗直连收到的 NotifyTurnResult 条数;-1 = 本次跳过直连
	directTurns int
}

// runCrossZoneMatchSmoke 是 cross_zone=true 时 RunBattleSmoke 的实际流程。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func runCrossZoneMatchSmoke(cfg *config.Config) {
	zoneA, zoneB := cfg.BattleSmoke.ZoneA, cfg.BattleSmoke.ZoneB
	mode, ok := crossZoneMatchModeOf(cfg.BattleSmoke.Mode)

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	// bots 收集已登录的会话,fail 时统一优雅收尾(同原模式,避免残留 ONLINE
	// 会话把下一次冒烟顶成 ReplaceLogin)。
	var bots []*battleSmokeBot
	cleanup := func() {
		for _, b := range bots {
			if b == nil || b.gc == nil {
				continue
			}
			_ = leaveGame(b.gc, stats)
			sendDisconnectBestEffort(b.gc)
			gameobject.PlayerList.Delete(b.gc.PlayerId)
			b.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("CROSS_ZONE_MATCH_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	if !ok {
		fail("config", "unsupported battle_smoke.mode=%q", cfg.BattleSmoke.Mode)
	}

	// ---- 步骤 1:A 登 zone_a、B 登 zone_b(并发) ----
	// 复制一份 cfg 只改 ZoneID:resolveGateViaHTTPLocal 用 cfg.ZoneID 向
	// /api/assign-gate 要该 zone 的 gate,登录进场后玩家位置
	// (player:{id}:location.zone_id)就落在这个 zone —— 这正是 match
	// JoinQueue(D6)/gather 读取 zone 的来源。
	zap.L().Info("[cross-zone] step 1: login both robots into different zones",
		zap.String("a", crossZoneAccountA), zap.Uint32("zone_a", zoneA),
		zap.String("b", crossZoneAccountB), zap.Uint32("zone_b", zoneB))

	type loginSpec struct {
		account string
		zone    uint32
	}
	specs := []loginSpec{{crossZoneAccountA, zoneA}, {crossZoneAccountB, zoneB}}
	botsArr := make([]*battleSmokeBot, 2)
	errsArr := make([]error, 2)
	var wg sync.WaitGroup
	for i, s := range specs {
		wg.Add(1)
		go func(idx int, s loginSpec) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = s.zone
			botsArr[idx], errsArr[idx] = battleSmokeLogin(&zoneCfg, s.account, stats)
		}(i, s)
	}
	wg.Wait()
	for i, err := range errsArr {
		if botsArr[i] != nil {
			bots = append(bots, botsArr[i])
		}
		if err != nil {
			fail("login", "account=%s zone=%d err=%v", specs[i].account, specs[i].zone, err)
		}
	}
	botA, botB := botsArr[0], botsArr[1]

	// 落区证据:assign-gate 按 zone 分配 gate,本地部署里各 zone 的 gate 端口
	// 不同(gate_1 / gate_2)。两个地址相同意味着两人其实进了同一个 zone,
	// 后面即便匹配成功也不能叫"跨 zone",直接判失败。
	zap.L().Info("[cross-zone] both robots in scene",
		zap.Uint64("a_player", botA.gc.PlayerId), zap.Uint32("zone_a", zoneA), zap.String("a_gate", botA.gate),
		zap.Uint64("b_player", botB.gc.PlayerId), zap.Uint32("zone_b", zoneB), zap.String("b_gate", botB.gate))
	if botA.gate == botB.gate {
		fail("zone-placement", "both robots assigned the same gate %s (zone_a=%d zone_b=%d); not a cross-zone run",
			botA.gate, zoneA, zoneB)
	}

	// ---- 步骤 2:双双入队,各自在 goroutine 里跑完参战全程 ----
	aDone := make(chan crossZoneFighterResult, 1)
	bDone := make(chan crossZoneFighterResult, 1)
	go func() { aDone <- runCrossZoneFighter("A", botA, zoneA, mode, stats) }()
	go func() { bDone <- runCrossZoneFighter("B", botB, zoneB, mode, stats) }()

	// ---- 步骤 3:主流程等双方开战,断言同一场对局 ----
	// battleStart 通道是 close 广播,主流程与参战 goroutine 可同时等待。
	// 比 goroutine 内的 30s 预算略宽,保证先看到参战侧更具体的失败原因。
	waitStart := func(side string, bot *battleSmokeBot, done chan crossZoneFighterResult) uint64 {
		ctx, cancel := context.WithTimeout(context.Background(), battleSmokeStartTimeout+5*time.Second)
		defer cancel()
		battleId, err := bot.player.WaitBattleStart(ctx)
		if err != nil {
			select {
			case res := <-done:
				if res.err != nil {
					fail(res.step, "%v", res.err)
				}
			default:
			}
			fail(side+"-wait-battle-start", "no BattleStart within %s: %v", battleSmokeStartTimeout, err)
		}
		return battleId
	}
	aBattleId := waitStart("a", botA, aDone)
	bBattleId := waitStart("b", botB, bDone)
	if aBattleId == 0 || aBattleId != bBattleId {
		fail("battle-id-mismatch", "A battle_id=%d B battle_id=%d (expected the same non-zero battle)",
			aBattleId, bBattleId)
	}
	zap.L().Info("[cross-zone] step 3: both robots matched into the same battle",
		zap.Uint64("battle_id", aBattleId))

	// ---- 步骤 4:收双方终局结果 ----
	collect := func(side string, done chan crossZoneFighterResult) crossZoneFighterResult {
		select {
		case res := <-done:
			if res.err != nil {
				fail(res.step, "%v", res.err)
			}
			if res.turns < 1 {
				fail(side+"-turn-count", "fighter saw %d turn results, expected >= 1", res.turns)
			}
			return res
		case <-time.After(battleSmokeEndTimeout + 10*time.Second):
			fail(side+"-join", "fighter goroutine did not finish in time")
		}
		return crossZoneFighterResult{}
	}
	aRes := collect("a", aDone)
	bRes := collect("b", bDone)

	// ---- 全部断言通过 ----
	zap.L().Info(fmt.Sprintf("CROSS_ZONE_MATCH_OK battle_id=%d zone_a=%d zone_b=%d a_turns=%d b_turns=%d a_direct_turns=%d b_direct_turns=%d",
		aRes.battleId, zoneA, zoneB, aRes.turns, bRes.turns, aRes.directTurns, bRes.directTurns),
		zap.String("a_gate", botA.gate), zap.String("b_gate", botB.gate),
		zap.String("a_outcome", battle.EBattleOutcome(aRes.outcome).String()),
		zap.String("b_outcome", battle.EBattleOutcome(bRes.outcome).String()))
	cleanup()
	// 正常返回,main 里该分支 return 后进程退出码为 0。
}

// runCrossZoneFighter 是一侧参战方的完整流程:
// JoinQueue(mode) → 等 NotifyBattleStart → SetAutoBattle → 等 NotifyBattleEnd。
// side 只用于日志与失败步骤名("A"/"B")。
func runCrossZoneFighter(side string, bot *battleSmokeBot, zone uint32, mode match.MatchMode,
	stats *metrics.Stats,
) crossZoneFighterResult {
	stepPrefix := map[string]string{"A": "a", "B": "b"}[side]
	zap.L().Info("[cross-zone] step 2: join queue",
		zap.String("side", side), zap.String("account", bot.account),
		zap.Uint32("zone", zone), zap.String("mode", mode.String()))

	// zone_id 只是可观测性字段:match 以玩家位置里的 zone 为准(设计 D6),
	// 这里如实填本侧登录的 zone。
	if err := bot.gc.SendRequest(game.MatchServiceJoinQueueMessageId, &match.JoinQueueRequest{
		PlayerId:       bot.gc.PlayerId,
		Mode:           mode,
		BattleConfigId: crossZoneBattleConfigId,
		ZoneId:         zone,
	}); err != nil {
		return crossZoneFighterResult{step: stepPrefix + "-join-queue", err: fmt.Errorf("send JoinQueue: %w", err)}
	}
	stats.MsgSent()

	startCtx, startCancel := context.WithTimeout(context.Background(), battleSmokeStartTimeout)
	battleId, err := bot.player.WaitBattleStart(startCtx)
	startCancel()
	if err != nil {
		return crossZoneFighterResult{step: stepPrefix + "-wait-battle-start",
			err: fmt.Errorf("no BattleStart within %s: %w", battleSmokeStartTimeout, err)}
	}
	zap.L().Info("[cross-zone] battle started, enabling auto battle",
		zap.String("side", side), zap.Uint64("battle_id", battleId))

	// 战斗直连(§18):两侧各凭自己的票据直连同一个 battle 节点(battle 是全局池,
	// 跨 zone 对局两人连的是同一进程)。跳过时退回 gate 中继(D23 回落路径)。
	var direct *battleDirectConn
	sendBattle := bot.gc.SendRequest
	if loginTestCfg == nil || !loginTestCfg.BattleSmoke.SkipDirectConnect {
		direct, err = openBattleDirectConn(bot, stats)
		if err != nil {
			return crossZoneFighterResult{step: stepPrefix + "-direct-connect", err: err, battleId: battleId}
		}
		defer direct.Close()
		if direct.battleId != battleId {
			return crossZoneFighterResult{step: stepPrefix + "-direct-battle-id",
				err:      fmt.Errorf("direct handshake battle_id=%d != started battle_id=%d", direct.battleId, battleId),
				battleId: battleId}
		}
		sendBattle = direct.Send
	}

	// 双方都开自动战斗,battle 节点每回合替双方出招,冒烟无需手工提交动作。
	if err := sendBattle(game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{
		BattleId: battleId,
		Enabled:  true,
	}); err != nil {
		return crossZoneFighterResult{step: stepPrefix + "-set-auto-battle",
			err: fmt.Errorf("send SetAutoBattle: %w", err), battleId: battleId}
	}
	stats.MsgSent()

	endCtx, endCancel := context.WithTimeout(context.Background(), battleSmokeEndTimeout)
	outcome, err := bot.player.WaitBattleEnd(endCtx)
	endCancel()
	if err != nil {
		return crossZoneFighterResult{step: stepPrefix + "-wait-battle-end",
			err: fmt.Errorf("no BattleEnd within %s: %w", battleSmokeEndTimeout, err), battleId: battleId}
	}

	turns := bot.player.GetTurnCount()
	directTurns := -1
	if direct != nil {
		directTurns = int(direct.turnResults.Load())
		zap.L().Info("[cross-zone] direct-connect delivery",
			zap.String("side", side), zap.String("counts", direct.summary()))
		if directTurns < 1 || direct.battleEnds.Load() < 1 {
			return crossZoneFighterResult{step: stepPrefix + "-direct-delivery",
				err:      fmt.Errorf("direct connection saw %s, expected turn_results>=1 and battle_ends>=1", direct.summary()),
				battleId: battleId, outcome: outcome, turns: turns}
		}
	}
	zap.L().Info("[cross-zone] battle finished",
		zap.String("side", side), zap.Uint64("battle_id", battleId),
		zap.String("outcome", battle.EBattleOutcome(outcome).String()),
		zap.Int("turns", turns), zap.Int("direct_turns", directTurns))
	return crossZoneFighterResult{battleId: battleId, outcome: outcome, turns: turns, directTurns: directTurns}
}
