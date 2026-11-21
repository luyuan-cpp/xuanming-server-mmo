package main

// LOBBY 档观战冒烟(docs/design/battle-spectate-tiered-fanout.md §6 / CODEX §6 / §8.4):
//   1. 参战方 A 进 PVE_SOLO 对局,建直连后等待观战屏障(不立即开自动战斗);
//   2. direct_fill_count 名观众(默认 20,对应 BattleRoomManager::kMaxObserversPerRoom)先 WatchBattle
//      占满房间直连席;
//   3. 大厅观众 L 调用 WatchBattle,断言回包 delivery == SPECTATE_DELIVERY_LOBBY 且带非空 spectate_ticket;
//   4. L 经大厅连接发 GateClientSpectate.SubscribeSpectate(351),不建 battle 直连,
//      断言大厅连接依次收到 161(NotifySpectateState) → 每回合 158(NotifySpectateTurnResult) → 166(NotifySpectateEnd);
//   5. 可选 manual_hold_25s=true:首回合 A 不开自动战斗,等满 25s 行动窗口(覆盖 10s 通道心跳),
//      断言在最后一回合前 L 不收到 SOURCE_LOST,随后正常打完收到 158 + 166(BATTLE_FINISHED);
//   6. 可选 shutdown_battle_command 非空:L 收到 161 后执行停机命令,断言 L 在 10s 内立刻收到
//      166(SPECTATE_END_SOURCE_LOST),不等 30s 空闲超时。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"proto/battle"
	gatepb "proto/gate"
	"proto/match"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
)

const (
	lobbySpectateDefaultFillCount = 20
	lobbySpectateAccountPrefix    = "robot_91"
	lobbySpectateLoginConcurrency = 4
	lobbySpectateShutdownMaxWait  = 10 * time.Second
)

// battleSmokeWatchBattleResp 经大厅连接发 WatchBattle 并返回完整回包(含 delivery 与 spectate_ticket)。
func battleSmokeWatchBattleResp(bot *battleSmokeBot, stats *metrics.Stats, battleId uint64, requestId string, timeout time.Duration) (*match.WatchBattleResponse, error) {
	if requestId == "" {
		requestId = chatSmokeRequestId()
	}
	request := &match.WatchBattleRequest{
		PlayerId:  bot.gc.PlayerId,
		BattleId:  battleId,
		RequestId: requestId,
	}
	resp := &match.WatchBattleResponse{}
	caller := newSmokeCaller(bot.gc, stats, bot.account, timeout, nil)
	envelopeTip, err := caller.call(game.MatchServiceWatchBattleMessageId, request, resp)
	if err != nil {
		return nil, fmt.Errorf("WatchBattle: %w", err)
	}
	if envelopeTip != 0 {
		return nil, fmt.Errorf("WatchBattle envelope rejected tip=%d", envelopeTip)
	}
	if tip := resp.GetErrorMessage().GetId(); tip != 0 {
		return nil, fmt.Errorf("WatchBattle rejected tip=%d params=%v", tip, resp.GetErrorMessage().GetParameters())
	}
	return resp, nil
}

// battleSmokeSubscribeSpectate 经大厅连接发 GateClientSpectate.SubscribeSpectate(351)。
func battleSmokeSubscribeSpectate(bot *battleSmokeBot, stats *metrics.Stats, battleId uint64, ticket []byte, timeout time.Duration) error {
	req := &gatepb.SubscribeSpectateRequest{
		BattleId:       battleId,
		SpectateTicket: ticket,
	}
	resp := &gatepb.SubscribeSpectateResponse{}
	caller := newSmokeCaller(bot.gc, stats, bot.account, timeout, nil)
	envelopeTip, err := caller.call(game.GateClientSpectateSubscribeSpectateMessageId, req, resp)
	if err != nil {
		return fmt.Errorf("SubscribeSpectate: %w", err)
	}
	if envelopeTip != 0 {
		return fmt.Errorf("SubscribeSpectate envelope rejected tip=%d", envelopeTip)
	}
	if tip := resp.GetErrorMessage().GetId(); tip != 0 {
		return fmt.Errorf("SubscribeSpectate rejected tip=%d params=%v", tip, resp.GetErrorMessage().GetParameters())
	}
	return nil
}

// battleSmokeUnsubscribeSpectate 经大厅连接发 GateClientSpectate.UnsubscribeSpectate(352)。
func battleSmokeUnsubscribeSpectate(bot *battleSmokeBot, stats *metrics.Stats, battleId uint64, timeout time.Duration) error {
	req := &gatepb.UnsubscribeSpectateRequest{BattleId: battleId}
	resp := &gatepb.UnsubscribeSpectateResponse{}
	caller := newSmokeCaller(bot.gc, stats, bot.account, timeout, nil)
	envelopeTip, err := caller.call(game.GateClientSpectateUnsubscribeSpectateMessageId, req, resp)
	if err != nil {
		return fmt.Errorf("UnsubscribeSpectate: %w", err)
	}
	if envelopeTip != 0 {
		return fmt.Errorf("UnsubscribeSpectate envelope rejected tip=%d", envelopeTip)
	}
	if tip := resp.GetErrorMessage().GetId(); tip != 0 {
		return fmt.Errorf("UnsubscribeSpectate rejected tip=%d params=%v", tip, resp.GetErrorMessage().GetParameters())
	}
	return nil
}

func runBattleSpectateLobbySmoke(cfg *config.Config) {
	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	fillCount := cfg.BattleSmoke.DirectFillCount
	if fillCount <= 0 {
		fillCount = lobbySpectateDefaultFillCount
	}
	shutdownCmd := strings.TrimSpace(cfg.BattleSmoke.ShutdownBattleCommand)
	manualHold25s := cfg.BattleSmoke.ManualHold25s && shutdownCmd == ""

	var botsMu sync.Mutex
	var allBots []*battleSmokeBot
	cleanup := func() {
		botsMu.Lock()
		list := append([]*battleSmokeBot(nil), allBots...)
		botsMu.Unlock()
		for _, b := range list {
			if b == nil || b.gc == nil {
				continue
			}
			_ = leaveGame(b.gc, stats)
			gameobject.PlayerList.Delete(b.gc.PlayerId)
			b.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("BATTLE_LOBBY_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 步骤 1:登录参战 A、占座观众 1..fillCount、大厅观众 L、退订校验观众 U ----
	totalBots := 1 + fillCount + 2
	accounts := make([]string, totalBots)
	accounts[0] = fmt.Sprintf("%s%02d", lobbySpectateAccountPrefix, 0)
	for i := 0; i < fillCount; i++ {
		accounts[1+i] = fmt.Sprintf("%s%02d", lobbySpectateAccountPrefix, 1+i)
	}
	accounts[1+fillCount] = fmt.Sprintf("%s%02d", lobbySpectateAccountPrefix, 98)
	accounts[2+fillCount] = fmt.Sprintf("%s%02d", lobbySpectateAccountPrefix, 99)

	zap.L().Info("[battle-lobby-smoke] step 1: logging in bots",
		zap.Int("total", totalBots), zap.Int("direct_fill_count", fillCount),
		zap.Bool("manual_hold_25s", manualHold25s), zap.Bool("shutdown_test", shutdownCmd != ""))

	logged := make([]*battleSmokeBot, totalBots)
	errs := make([]error, totalBots)
	sem := make(chan struct{}, lobbySpectateLoginConcurrency)
	var wg sync.WaitGroup
	for i, acc := range accounts {
		wg.Add(1)
		go func(idx int, account string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			bot, err := battleSmokeLogin(cfg, account, stats)
			logged[idx] = bot
			errs[idx] = err
			if bot != nil {
				botsMu.Lock()
				allBots = append(allBots, bot)
				botsMu.Unlock()
			}
		}(i, acc)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			fail("login", "account=%s err=%v", accounts[i], err)
		}
	}
	botA := logged[0]
	fillers := logged[1 : 1+fillCount]
	botL := logged[1+fillCount]
	botU := logged[2+fillCount]

	// ---- 步骤 2:A 开启 PVE_SOLO 并在直连就绪后等待屏障 ----
	releaseAuto := make(chan struct{})
	releaseOnce := sync.Once{}
	release := func() { releaseOnce.Do(func() { close(releaseAuto) }) }
	aDone := make(chan battleSmokeFighterResult, 1)
	aFailed := make(chan struct{})
	go func() {
		res := runLobbySmokeFighter(cfg, botA, stats, releaseAuto, manualHold25s, shutdownCmd != "")
		aDone <- res
		if res.err != nil {
			close(aFailed)
		}
	}()

	aBattleId, err := battleSmokeWaitStart(botA.player, battleSmokeStartTimeout+10*time.Second, aFailed)
	if err != nil {
		select {
		case res := <-aDone:
			if res.err != nil {
				fail(res.step, "%v", res.err)
			}
		default:
		}
		fail("wait-battle-start", "timeout waiting fighter A battle start: %v", err)
	}

	// ---- 步骤 3:占满房间直连席 ----
	directCount := 0
	lobbyFillerCount := 0
	for i, filler := range fillers {
		resp, watchErr := battleSmokeWatchBattleResp(filler, stats, aBattleId, "", battleSmokeSpectateTimeout)
		if watchErr != nil {
			fail("fill-direct-seat", "filler=%d account=%s err=%v", i+1, filler.account, watchErr)
		}
		if resp.GetBattleId() != aBattleId {
			fail("fill-battle-id", "filler=%d picked battle_id=%d expected=%d", i+1, resp.GetBattleId(), aBattleId)
		}
		if resp.GetDelivery() == match.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY {
			lobbyFillerCount++
		} else {
			directCount++
		}
	}
	zap.L().Info("[battle-lobby-smoke] direct seats filled",
		zap.Uint64("battle_id", aBattleId), zap.Int("direct_admitted", directCount), zap.Int("early_lobby", lobbyFillerCount))

	// ---- 步骤 4:观众 L 走 LOBBY 档 WatchBattle + SubscribeSpectate ----
	lResp, err := battleSmokeWatchBattleResp(botL, stats, aBattleId, "", battleSmokeSpectateTimeout)
	if err != nil {
		fail("lobby-watch-battle", "%v", err)
	}
	if lResp.GetBattleId() != aBattleId {
		fail("lobby-watch-battle-id", "got battle_id=%d expected=%d", lResp.GetBattleId(), aBattleId)
	}
	if lResp.GetDelivery() != match.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY {
		fail("lobby-expected-delivery", "got delivery=%s expected=SPECTATE_DELIVERY_LOBBY (direct_admitted=%d)",
			lResp.GetDelivery().String(), directCount)
	}
	if len(lResp.GetSpectateTicket()) == 0 {
		fail("lobby-empty-ticket", "LOBBY delivery returned empty spectate_ticket")
	}
	if err := battleSmokeSubscribeSpectate(botL, stats, aBattleId, lResp.GetSpectateTicket(), battleSmokeSpectateTimeout); err != nil {
		fail("lobby-subscribe", "%v", err)
	}

	// 验证另一名大厅观众 U 订阅后可正常退订
	uResp, err := battleSmokeWatchBattleResp(botU, stats, aBattleId, "", battleSmokeSpectateTimeout)
	if err != nil {
		fail("unsub-watch-battle", "%v", err)
	}
	if uResp.GetDelivery() != match.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY || len(uResp.GetSpectateTicket()) == 0 {
		fail("unsub-delivery", "expected LOBBY delivery with ticket for U, got=%s", uResp.GetDelivery().String())
	}
	if err := battleSmokeSubscribeSpectate(botU, stats, aBattleId, uResp.GetSpectateTicket(), battleSmokeSpectateTimeout); err != nil {
		fail("unsub-subscribe", "%v", err)
	}
	if err := battleSmokeUnsubscribeSpectate(botU, stats, aBattleId, battleSmokeSpectateTimeout); err != nil {
		fail("unsub-unsubscribe", "%v", err)
	}

	// ---- 步骤 5:断言 L 在大厅连接上收到 161 首帧(从未建立战斗直连) ----
	stateCtx, stateCancel := context.WithTimeout(context.Background(), battleSmokeSpectateTimeout)
	specBattleId, err := botL.player.WaitSpectateState(stateCtx)
	stateCancel()
	if err != nil {
		fail("lobby-wait-161", "no NotifySpectateState on lobby connection within %s: %v", battleSmokeSpectateTimeout, err)
	}
	if specBattleId != aBattleId {
		fail("lobby-161-battle-id", "got battle_id=%d expected=%d", specBattleId, aBattleId)
	}
	zap.L().Info("[battle-lobby-smoke] L received 161 on lobby connection",
		zap.Uint64("battle_id", specBattleId), zap.Int("state_count", botL.player.GetSpectateStateCount()))

	// ---- 分支 A:停机 battle 节点,断言中继/广播观众立刻收到 166 SOURCE_LOST(不等 30s) ----
	if shutdownCmd != "" {
		t0 := time.Now()
		if err := runShellCommand(shutdownCmd); err != nil {
			fail("shutdown-trigger", "command=%q err=%v", shutdownCmd, err)
		}
		endCtx, endCancel := context.WithTimeout(context.Background(), lobbySpectateShutdownMaxWait)
		reason, endErr := botL.player.WaitSpectateEnd(endCtx)
		endCancel()
		elapsed := time.Since(t0)
		if endErr != nil {
			fail("shutdown-wait-166", "no NotifySpectateEnd within %s after battle shutdown: %v", lobbySpectateShutdownMaxWait, endErr)
		}
		if battle.ESpectateEndReason(reason) != battle.ESpectateEndReason_SPECTATE_END_SOURCE_LOST {
			fail("shutdown-end-reason", "got reason=%s expected=SPECTATE_END_SOURCE_LOST", battle.ESpectateEndReason(reason).String())
		}
		zap.L().Info(fmt.Sprintf("BATTLE_LOBBY_SHUTDOWN_SMOKE_OK battle_id=%d elapsed_ms=%d reason=%s",
			aBattleId, elapsed.Milliseconds(), battle.ESpectateEndReason(reason).String()))
		cleanup()
		return
	}

	// ---- 分支 B:常规 LOBBY / 25s 手动回合心跳验证 ----
	release()
	if manualHold25s {
		// 等 24s 检查:首回合手动窗口 25s 内不得被 gate 提前合成 SOURCE_LOST(10s 心跳续命)
		time.Sleep(24 * time.Second)
		if reason, ended := botL.player.HasSpectateEnd(); ended {
			fail("manual-hold-premature-end", "spectate ended early during 25s manual window: reason=%s",
				battle.ESpectateEndReason(reason).String())
		}
		zap.L().Info("[battle-lobby-smoke] 24s into manual 25s window: no premature SOURCE_LOST",
			zap.Int("state_count", botL.player.GetSpectateStateCount()),
			zap.Int("turn_count", botL.player.GetSpectateTurnCount()))
	}

	endCtx, endCancel := context.WithTimeout(context.Background(), battleSmokeEndTimeout)
	endReason, err := botL.player.WaitSpectateEnd(endCtx)
	endCancel()
	if err != nil {
		fail("lobby-wait-166", "no NotifySpectateEnd on lobby connection within %s: %v", battleSmokeEndTimeout, err)
	}
	if battle.ESpectateEndReason(endReason) != battle.ESpectateEndReason_SPECTATE_END_BATTLE_FINISHED {
		fail("lobby-166-reason", "got reason=%s expected=SPECTATE_END_BATTLE_FINISHED",
			battle.ESpectateEndReason(endReason).String())
	}

	lTurns := botL.player.GetSpectateTurnCount()
	if lTurns < 1 {
		fail("lobby-158-count", "lobby spectator received %d NotifySpectateTurnResult, expected >= 1", lTurns)
	}
	sSeq, tSeq, eSeq := botL.player.GetSpectateFrameOrder()
	if sSeq == 0 || tSeq == 0 || eSeq == 0 || !(sSeq < tSeq && tSeq < eSeq) {
		fail("lobby-frame-order", "expected 161 < 158 < 166 on lobby connection, got seq state=%d turn=%d end=%d", sSeq, tSeq, eSeq)
	}

	// 等待 A 正常结算收尾
	var aRes battleSmokeFighterResult
	select {
	case aRes = <-aDone:
	case <-time.After(battleSmokeEndTimeout + 10*time.Second):
		fail("fighter-timeout", "fighter A did not finish in time")
	}
	if aRes.err != nil {
		fail(aRes.step, "%v", aRes.err)
	}

	zap.L().Info(fmt.Sprintf("BATTLE_LOBBY_SMOKE_OK battle_id=%d direct_fill=%d l_states=%d l_turns=%d end_reason=%s manual_hold_25s=%t",
		aBattleId, directCount, botL.player.GetSpectateStateCount(), lTurns,
		battle.ESpectateEndReason(endReason).String(), manualHold25s))
	cleanup()
}

func runLobbySmokeFighter(cfg *config.Config, bot *battleSmokeBot, stats *metrics.Stats,
	releaseAuto <-chan struct{}, manualHold25s bool, expectShutdown bool) battleSmokeFighterResult {
	planId, err := ensureNetworkProbe(bot.gc, stats, bot.account)
	if err != nil {
		return battleSmokeFighterResult{step: "a-network-probe", err: err}
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), battleSmokeStartTimeout)
	if err := battleSmokeJoinQueue(bot, stats, &match.JoinQueueRequest{
		PlayerId:       bot.gc.PlayerId,
		Mode:           match.MatchMode_MATCH_MODE_PVE_SOLO,
		BattleConfigId: 1,
		ZoneId:         cfg.ZoneID,
		RequestId:      chatSmokeRequestId(),
		NetworkPlanId:  planId,
	}, battleSmokeStartTimeout); err != nil {
		startCancel()
		return battleSmokeFighterResult{step: "a-join-queue", err: err}
	}
	battleId, err := bot.player.WaitBattleStart(startCtx)
	startCancel()
	if err != nil {
		return battleSmokeFighterResult{step: "a-wait-battle-start", err: fmt.Errorf("no BattleStart: %w", err)}
	}

	direct, err := openBattleDirectConn(bot, stats)
	if err != nil {
		return battleSmokeFighterResult{step: "a-direct-connect", err: err, battleId: battleId}
	}
	defer direct.Close()

	if expectShutdown {
		// 停机测试只维持对局活着直到外部停掉 battle 节点
		<-releaseAuto
		return battleSmokeFighterResult{battleId: battleId}
	}

	select {
	case <-releaseAuto:
	case <-time.After(45 * time.Second):
		zap.L().Warn("[battle-lobby-smoke] fighter A timed out waiting for lobby spectator readiness")
	}

	if manualHold25s {
		// 首回合故意不开自动战斗,等满 25s 手动行动超时(含余量),再开自动战斗打完
		zap.L().Info("[battle-lobby-smoke] holding turn 1 manually for 26s to verify spectate keepalive",
			zap.Uint64("battle_id", battleId))
		time.Sleep(26 * time.Second)
	}

	if err := direct.Send(game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{
		BattleId: battleId,
		Enabled:  true,
	}); err != nil {
		return battleSmokeFighterResult{step: "a-set-auto-battle", err: fmt.Errorf("send SetAutoBattle: %w", err), battleId: battleId}
	}
	stats.MsgSent()

	endCtx, endCancel := context.WithTimeout(context.Background(), battleSmokeEndTimeout)
	outcome, err := bot.player.WaitBattleEnd(endCtx)
	endCancel()
	if err != nil {
		return battleSmokeFighterResult{step: "a-wait-battle-end", err: fmt.Errorf("no BattleEnd: %w", err), battleId: battleId}
	}
	if battle.EBattleOutcome(outcome) == battle.EBattleOutcome_BATTLE_OUTCOME_VOID {
		return battleSmokeFighterResult{step: "a-battle-voided", err: fmt.Errorf("battle_id=%d voided", battleId), battleId: battleId, outcome: outcome}
	}
	return battleSmokeFighterResult{
		battleId:    battleId,
		outcome:     outcome,
		turns:       bot.player.GetTurnCount(),
		directTurns: int(direct.turnResults.Load()),
	}
}

func runShellCommand(command string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("pwsh", "-NoProfile", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
