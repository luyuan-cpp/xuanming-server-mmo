package main

// battle-smoke 场景:两个机器人对本地服务端做"回合制战斗 + 观战"端到端冒烟。
//
// 兑现 docs/design/turn-based-battle-server.md §9 预留的 "robot battle 动作":
//   机器人 A(参战):JoinQueue(PVE_SOLO) → NotifyBattleStart → SetAutoBattle
//                   → 等 NotifyBattleEnd;
//   机器人 B(观战):等 A 开战 → WatchBattle(battle_id=0,观战匹配随机对局)
//                   → NotifySpectateState → 断言观战对局 == A 的对局
//                   → 等 NotifySpectateEnd → 断言观战回合数 ≥ 1。
//
// 结果约定(供外层脚本消费):
//   全部断言通过 → 日志一行 `BATTLE_SMOKE_OK battle_id=… a_turns=… b_spectate_turns=…`,
//                  进程退出码 0;
//   任一步失败   → 日志一行 `BATTLE_SMOKE_FAIL step=… reason=…`,进程退出码 1。
//
// 组织方式照 currency_crash_window_scenario.go:纯客户端脚本,复用
// login_test_scenarios.go 的 prepareBehaviorClient(登录 → 进场 → 起 RecvLoop),
// 战斗/观战信号走 gameobject.Player 的 WaitXxx/SignalXxx 通道
// (由 robot/logic/handler 下的 battle_* handler 触发)。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"proto/battle"
	"proto/match"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"
)

// 账号必须带 robot_ 前缀才能走 login 侧的 DevPasswordAuth(密码模式开发认证)。
// 用 9001/9002 高位编号,避开压测常用的 robot_0001..NNNN 区段,防止会话互踢。
const (
	battleSmokeAccountA = "robot_9001" // 参战方
	battleSmokeAccountB = "robot_9002" // 观战方
)

// battle-smoke 各步超时。开战 30s 是给匹配 + battle 节点建局的预算;
// 战斗/观战终局 120s 覆盖"自动战斗打满回合上限"的最坏情况。
const (
	battleSmokeStartTimeout    = 30 * time.Second
	battleSmokeSpectateTimeout = 15 * time.Second
	battleSmokeEndTimeout      = 120 * time.Second
)

// battleSmokeBot 是一个已登录进场的机器人会话。
type battleSmokeBot struct {
	account string
	gc      *pkg.GameClient
	player  *gameobject.Player
}

// spectateReady 是"允许 A 开自动战斗"的屏障:PVE solo 秒杀(玩家 auto + 怪物无
// 属性一回合即结束,实测 ~100ms),战斗会在 B 走完 WatchBattle→match→AddObserver
// 一圈之前就销毁,B 必然扑空(battle 房间已不存在,回 tip_id=5)。故 A 收到
// BattleStart 后先不出招,阻塞在这个屏障上,等主流程确认 B 观战首帧到位再放行。
// 这只是冒烟脚本的时序编排;真实玩家战斗多回合、有决策时间,观战窗口天然充裕。
var spectateReady = make(chan struct{})

// battleSmokeFighterResult 是参战方 goroutine 的产出。
type battleSmokeFighterResult struct {
	step     string // 失败时所在步骤(成功为空)
	err      error  // 失败原因(成功为 nil)
	battleId uint64
	outcome  int32
	turns    int
}

// RunBattleSmoke 是 main.go `mode: battle-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunBattleSmoke(cfg *config.Config) {
	// prepareBehaviorClient 经 loginAndEnterScenario 读 loginTestCfg 决定认证方式
	// (password / satoken),这里先挂上配置。
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	// bots 收集已登录的会话,fail 时统一优雅收尾(LeaveGame + Disconnect),
	// 避免残留 ONLINE 会话把下一次冒烟顶成 ReplaceLogin(见 main.go 注释)。
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
		zap.L().Error(fmt.Sprintf("BATTLE_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 步骤 1:A / B 并发登录进场 ----
	zap.L().Info("[battle-smoke] step 1: login both robots",
		zap.String("a", battleSmokeAccountA), zap.String("b", battleSmokeAccountB))

	botsArr := make([]*battleSmokeBot, 2)
	errsArr := make([]error, 2)
	var wg sync.WaitGroup
	for i, account := range []string{battleSmokeAccountA, battleSmokeAccountB} {
		wg.Add(1)
		go func(idx int, account string) {
			defer wg.Done()
			botsArr[idx], errsArr[idx] = battleSmokeLogin(cfg, account, stats)
		}(i, account)
	}
	wg.Wait()
	for i, err := range errsArr {
		if botsArr[i] != nil {
			bots = append(bots, botsArr[i])
		}
		if err != nil {
			fail("login", "account=%s err=%v", []string{battleSmokeAccountA, battleSmokeAccountB}[i], err)
		}
	}
	botA, botB := botsArr[0], botsArr[1]
	zap.L().Info("[battle-smoke] both robots in scene",
		zap.Uint64("a_player", botA.gc.PlayerId), zap.Uint64("b_player", botB.gc.PlayerId))

	// ---- 步骤 2:A 排队开战(goroutine 里跑完参战全程) ----
	aDone := make(chan battleSmokeFighterResult, 1)
	go func() { aDone <- runBattleSmokeFighter(cfg, botA, stats) }()

	// ---- 步骤 3:B 等 A 的开战信号后进观战 ----
	// B 等在 A 的 battleStart 通道上(close 即广播,和 A 自己的等待互不抢占)。
	// 比 A 的 30s 预算略宽,保证先看到 A 侧的失败原因而不是 B 这边的超时。
	bStartCtx, bStartCancel := context.WithTimeout(context.Background(), battleSmokeStartTimeout+10*time.Second)
	aBattleId, err := botA.player.WaitBattleStart(bStartCtx)
	bStartCancel()
	if err != nil {
		// A 侧大概率已经带着更具体的原因失败了;把它的结果捞出来优先呈现。
		select {
		case res := <-aDone:
			if res.err != nil {
				fail(res.step, "%v", res.err)
			}
		default:
		}
		fail("b-wait-battle-start", "timeout waiting A's battle start: %v", err)
	}
	zap.L().Info("[battle-smoke] step 3: B watch battle", zap.Uint64("a_battle_id", aBattleId))

	// battle_id=0 走"观战匹配"(服务端随机挑一场可观战对局)。
	// 当前全服只有 A 这一场,所以随后断言观战到的就是 A 的对局。
	if err := botB.gc.SendRequest(game.MatchServiceWatchBattleMessageId, &match.WatchBattleRequest{
		PlayerId: botB.gc.PlayerId,
		BattleId: 0,
	}); err != nil {
		fail("b-watch-battle", "send WatchBattle: %v", err)
	}
	stats.MsgSent()

	specCtx, specCancel := context.WithTimeout(context.Background(), battleSmokeSpectateTimeout)
	specBattleId, err := botB.player.WaitSpectateState(specCtx)
	specCancel()
	if err != nil {
		fail("b-wait-spectate-state", "no SpectateState within %s: %v", battleSmokeSpectateTimeout, err)
	}
	if specBattleId != aBattleId {
		fail("b-spectate-battle-id", "spectating battle_id=%d, expected A's battle_id=%d", specBattleId, aBattleId)
	}
	zap.L().Info("[battle-smoke] B spectating A's battle",
		zap.Uint64("battle_id", specBattleId),
		zap.Uint32("observer_count", botB.player.GetSpectateObserverCount()))

	// B 观战首帧已到位,放行 A 开自动战斗推进对局(见 spectateReady 注释)。
	close(spectateReady)

	// ---- 步骤 4:B 等观战结束(战斗打完观众收 SPECTATE_END_BATTLE_FINISHED) ----
	bEndCtx, bEndCancel := context.WithTimeout(context.Background(), battleSmokeEndTimeout)
	specEndReason, err := botB.player.WaitSpectateEnd(bEndCtx)
	bEndCancel()
	if err != nil {
		fail("b-wait-spectate-end", "no SpectateEnd within %s: %v", battleSmokeEndTimeout, err)
	}
	if specEndReason != int32(battle.ESpectateEndReason_SPECTATE_END_BATTLE_FINISHED) {
		// 非正常收尾(节点停机/观众被清退)不视为冒烟失败主因,但要显式可见。
		zap.L().Warn("[battle-smoke] spectate ended with non-finish reason",
			zap.String("reason", battle.ESpectateEndReason(specEndReason).String()))
	}
	bSpectateTurns := botB.player.GetSpectateTurnCount()
	zap.L().Info("[battle-smoke] step 4: spectate finished",
		zap.String("reason", battle.ESpectateEndReason(specEndReason).String()),
		zap.Int("b_spectate_turns", bSpectateTurns))
	if bSpectateTurns < 1 {
		fail("b-spectate-turn-count", "spectator saw %d turn results, expected >= 1", bSpectateTurns)
	}

	// ---- 步骤 5:收 A 的参战结果 ----
	var aRes battleSmokeFighterResult
	select {
	case aRes = <-aDone:
	case <-time.After(battleSmokeEndTimeout + 10*time.Second):
		fail("a-join", "fighter goroutine did not finish in time")
	}
	if aRes.err != nil {
		fail(aRes.step, "%v", aRes.err)
	}
	if aRes.turns < 1 {
		fail("a-turn-count", "fighter saw %d turn results, expected >= 1", aRes.turns)
	}

	// ---- 全部断言通过 ----
	zap.L().Info(fmt.Sprintf("BATTLE_SMOKE_OK battle_id=%d a_turns=%d b_spectate_turns=%d",
		aRes.battleId, aRes.turns, bSpectateTurns),
		zap.String("a_outcome", battle.EBattleOutcome(aRes.outcome).String()),
		zap.String("b_end_reason", battle.ESpectateEndReason(specEndReason).String()))
	cleanup()
	// 正常返回,main 里该分支 return 后进程退出码为 0。
}

// battleSmokeLogin 为一个账号走完整的 AssignGate → 连接 → 登录 → 进场流程。
// 每个机器人各自取一次 gate token(token 有 TTL 且按连接下发,不能共享)。
func battleSmokeLogin(cfg *config.Config, account string, stats *metrics.Stats) (*battleSmokeBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	port, _ := strconv.Atoi(portStr)

	zap.L().Info("[battle-smoke] logging in",
		zap.String("account", account), zap.String("gate", host+":"+portStr))

	gc, player, err := prepareBehaviorClient(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("prepare client: %w", err)
	}
	return &battleSmokeBot{account: account, gc: gc, player: player}, nil
}

// runBattleSmokeFighter 是参战方(A)的完整流程:
// JoinQueue(PVE_SOLO) → 等 NotifyBattleStart → SetAutoBattle → 等 NotifyBattleEnd。
func runBattleSmokeFighter(cfg *config.Config, bot *battleSmokeBot, stats *metrics.Stats) battleSmokeFighterResult {
	zap.L().Info("[battle-smoke] step 2: A join queue",
		zap.String("account", bot.account),
		zap.String("mode", match.MatchMode_MATCH_MODE_PVE_SOLO.String()))

	// PVE solo 即时开战(伪匹配);battle_config_id=1 用表里第一行的战斗配置。
	if err := bot.gc.SendRequest(game.MatchServiceJoinQueueMessageId, &match.JoinQueueRequest{
		PlayerId:       bot.gc.PlayerId,
		Mode:           match.MatchMode_MATCH_MODE_PVE_SOLO,
		BattleConfigId: 1,
		ZoneId:         cfg.ZoneID,
	}); err != nil {
		return battleSmokeFighterResult{step: "a-join-queue", err: fmt.Errorf("send JoinQueue: %w", err)}
	}
	stats.MsgSent()

	startCtx, startCancel := context.WithTimeout(context.Background(), battleSmokeStartTimeout)
	battleId, err := bot.player.WaitBattleStart(startCtx)
	startCancel()
	if err != nil {
		return battleSmokeFighterResult{step: "a-wait-battle-start",
			err: fmt.Errorf("no BattleStart within %s: %w", battleSmokeStartTimeout, err)}
	}
	// 秒杀防竞态:等主流程确认 B 已观战到位再开 auto 推进战斗(见 spectateReady 注释)。
	// 兜底 30s:即便 B 观战失败,也让 A 把战斗打完,好让主流程拿到 A 侧真实结果/超时。
	select {
	case <-spectateReady:
	case <-time.After(30 * time.Second):
		zap.L().Warn("[battle-smoke] A 等观战就绪超时,仍开自动战斗以推进")
	}

	zap.L().Info("[battle-smoke] A battle started, enabling auto battle",
		zap.Uint64("battle_id", battleId))

	// 开自动战斗,battle 节点每回合替 A 出招,冒烟无需手工提交动作。
	if err := bot.gc.SendRequest(game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{
		BattleId: battleId,
		Enabled:  true,
	}); err != nil {
		return battleSmokeFighterResult{step: "a-set-auto-battle",
			err: fmt.Errorf("send SetAutoBattle: %w", err), battleId: battleId}
	}
	stats.MsgSent()

	endCtx, endCancel := context.WithTimeout(context.Background(), battleSmokeEndTimeout)
	outcome, err := bot.player.WaitBattleEnd(endCtx)
	endCancel()
	if err != nil {
		return battleSmokeFighterResult{step: "a-wait-battle-end",
			err: fmt.Errorf("no BattleEnd within %s: %w", battleSmokeEndTimeout, err), battleId: battleId}
	}

	turns := bot.player.GetTurnCount()
	zap.L().Info("[battle-smoke] A battle finished",
		zap.Uint64("battle_id", battleId),
		zap.String("outcome", battle.EBattleOutcome(outcome).String()),
		zap.Int("a_turns", turns))
	return battleSmokeFighterResult{battleId: battleId, outcome: outcome, turns: turns}
}
