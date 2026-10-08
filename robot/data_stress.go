// Robot-side data-consistency stress driver.
//
// This complements the standalone Kafka driver under
// `go/db/cmd/data_stress`. Where that driver synthesizes DBTask messages
// directly (testing the consumer in isolation), THIS driver exercises the
// FULL pipeline:
//
//   robot client → Gate → Scene (cpp) → Kafka → DB (go) → MySQL + Redis
//
// What it asserts: for every robot session that successfully reached
// "entered game" and then logged out cleanly, the player's persistent state
// must have converged in MySQL by the time the verifier runs. The robot
// publishes its expectations to Redis under the same `verify:enrolled:*`
// and `verify:expected:*` schema the verifier already understands; the
// "expected seq" in this mode is simply the round count for that account.
//
// To enable: set `mode: data-stress` in robot.yaml plus a `data_stress`
// block (see DataStressConfig below).
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"proto/common/base"
	"proto/login"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/ai"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"
)

// runDataStressMode drives a population of robots through repeated
// login → enter → play → logout cycles, recording each successful logout as
// a "save event" in Redis for the verifier.
//
// `cfg.RobotCount` accounts are exercised in parallel; each account performs
// `cfg.DataStress.RoundsPerRobot` rounds. After each round, the robot bumps
// its expected counter (`verify:expected:player_database:<player_id>`) so the
// verifier can detect "fewer rows than expected" or "stale row content".
//
// NOTE on the seq encoding: in robot mode the seq represents the round count,
// not a per-write monotonic version. The cpp Scene controls when
// SavePlayerToRedis fires per round; we cannot easily count individual writes
// from the client. The verifier still checks "MySQL row exists & has the
// player_id we expect"; it ignores the registration_timestamp value in
// data-stress robot mode unless the cpp side is also instrumented to write
// the round number into the seq carrier.
func runDataStressMode(cfg *config.Config, stats *metrics.Stats, stop <-chan struct{}) {
	dsCfg := cfg.DataStress
	if dsCfg.Rounds <= 0 {
		dsCfg.Rounds = 5
	}
	if dsCfg.PlaySeconds <= 0 {
		dsCfg.PlaySeconds = 5
	}
	if dsCfg.VerifyRedisAddr == "" {
		dsCfg.VerifyRedisAddr = "127.0.0.1:6379"
	}

	rc := redis.NewClient(&redis.Options{
		Addr:     dsCfg.VerifyRedisAddr,
		Password: dsCfg.VerifyRedisPassword,
		DB:       dsCfg.VerifyRedisDB,
	})
	defer func() { _ = rc.Close() }()
	if err := rc.Ping(context.Background()).Err(); err != nil {
		zap.L().Fatal("data-stress: cannot reach verifier Redis",
			zap.String("addr", dsCfg.VerifyRedisAddr), zap.Error(err))
	}

	var totalRounds atomic.Int64
	var failedRounds atomic.Int64

	var wg sync.WaitGroup
	for i := 1; i <= cfg.RobotCount; i++ {
		account := fmt.Sprintf(cfg.AccountFmt, i)
		wg.Add(1)
		go func(account string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					zap.L().Error("data-stress robot panic",
						zap.String("account", account), zap.Any("panic", r))
				}
			}()
			runDataStressOneRobot(account, cfg, stats, stop, rc, dsCfg.Rounds, dsCfg.PlaySeconds, &totalRounds, &failedRounds)
		}(account)
		// Stagger to avoid login thundering herd.
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()

	zap.L().Info("data-stress complete",
		zap.Int64("total_rounds", totalRounds.Load()),
		zap.Int64("failed_rounds", failedRounds.Load()),
	)
}

// runDataStressOneRobot performs `rounds` repeated login → play → logout
// cycles for a single account, recording success in Redis.
func runDataStressOneRobot(
	account string,
	cfg *config.Config,
	stats *metrics.Stats,
	stop <-chan struct{},
	rc *redis.Client,
	rounds int,
	playSeconds int,
	totalRounds, failedRounds *atomic.Int64,
) {
	// access_token reconnect cache: first round does full primary auth
	// (password / satoken), every subsequent round short-circuits via
	// auth_type=access_token to keep the /auth/dev-login load constant.
	var cachedAccessToken string
	var cachedAccessExpire int64

	for round := 1; round <= rounds; round++ {
		select {
		case <-stop:
			return
		default:
		}
		ok, playerID, tok, exp := dataStressOneRound(account, cfg, stats, stop, playSeconds, cachedAccessToken, cachedAccessExpire)
		cachedAccessToken, cachedAccessExpire = tok, exp
		totalRounds.Add(1)
		if !ok {
			failedRounds.Add(1)
			zap.L().Warn("data-stress round failed",
				zap.String("account", account), zap.Int("round", round))
			continue
		}

		// Record expected state for this account/player.
		// The verifier accepts any monotonic value in the seq carrier; we
		// publish the round number so a later run with more rounds is
		// detectable as "expected > actual = consumer lag".
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		const msgType = "player_database"
		key := fmt.Sprintf("verify:expected:%s:%d", msgType, playerID)
		_ = rc.Set(ctx, key, round, 24*time.Hour).Err()
		_ = rc.SAdd(ctx, "verify:enrolled:"+msgType, playerID).Err()
		cancel()
	}
}

// dataStressOneRound runs ONE login→enter→play→logout cycle.
// Returns (ok, playerID, newAccessToken, newAccessExpire) — the latter two
// are threaded back so subsequent rounds reuse the access_token reconnect
// path instead of re-issuing against the primary auth provider.
func dataStressOneRound(
	account string,
	cfg *config.Config,
	stats *metrics.Stats,
	stop <-chan struct{},
	playSeconds int,
	prevAccessToken string,
	prevAccessExpire int64,
) (ok bool, playerID uint64, newAccessToken string, newAccessExpire int64) {
	newAccessToken, newAccessExpire = prevAccessToken, prevAccessExpire

	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		zap.L().Warn("resolve gate", zap.String("account", account), zap.Error(err))
		stats.LoginFail()
		return
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)

	gc, err := pkg.NewGameClient(host, port)
	if err != nil {
		stats.LoginFail()
		return
	}
	defer gc.Close()
	stats.Connected()
	defer stats.Disconnected()

	gc.Account = account
	time.Sleep(100 * time.Millisecond)

	if len(tokenPayload) > 0 {
		if err := gc.VerifyGateToken(tokenPayload, tokenSig); err != nil {
			// R17 R2 收尾(Round 19): gate token 过期不算 login_fail,
			// 由 data_stress 上层 retry 逻辑重新拿 token。详见
			// docs/stress/stress-1zone-45k-2026-06-04-round18.md §R2。
			stats.GateTokenRetry()
			return
		}
	}

	loginStart := time.Now()
	if err := loginAndEnterWithAuth(gc, cfg, stats, prevAccessToken, prevAccessExpire); err != nil {
		zap.L().Warn("login", zap.String("account", account), zap.Error(err))
		return
	}
	if access, _, accessExp, _ := gc.SnapshotTokens(); access != "" {
		newAccessToken = access
		newAccessExpire = accessExp
	}
	stats.LoginOK(time.Since(loginStart))
	stats.EnterOK()

	playerID = gc.PlayerId
	player := gameobject.NewPlayer(playerID)
	gameobject.PlayerList.Set(playerID, player)
	defer gameobject.PlayerList.Delete(playerID)

	pkg.Clients.Register(playerID, gc)
	defer pkg.Clients.Unregister(playerID, gc)

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		gc.RecvLoop(func(client *pkg.GameClient, msg *base.MessageContent) {
			stats.MsgRecv()
			handler.MessageBodyHandler(client, msg)
		})
	}()

	go runTokenRefresher(gc, cfg.GatewayAddr, stats, stop, recvDone)

	// Wait for scene-ready so any save-triggering action (e.g. movement,
	// scene switch) is actually delivered to a real scene node.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := player.WaitSceneReady(waitCtx); err != nil {
		waitCancel()
		zap.L().Warn("scene ready timeout", zap.String("account", account), zap.Error(err))
		_ = gc.SendRequest(game.ClientPlayerLoginLeaveGameMessageId, &login.LeaveGameRequest{})
		<-recvDone
		return
	}
	waitCancel()

	// Issue a few stress actions during the play window. We deliberately do
	// NOT use the LLM/profile loop here — we want a deterministic, dense
	// sequence of saves-on-action.
	robotAI := ai.NewRobotAI(gc, stats)
	robotAI.SetPlayer(player)
	if cfg.ActionInterval > 0 {
		robotAI.SetInterval(time.Duration(cfg.ActionInterval) * time.Second)
	} else {
		robotAI.SetInterval(500 * time.Millisecond)
	}
	if profile := cfg.ResolveProfile(); profile != nil {
		robotAI.SetProfile(profile)
	} else if p, found := ai.BuiltinProfiles["stress"]; found {
		robotAI.SetProfile(&p)
	}

	playCtx, playCancel := context.WithTimeout(context.Background(), time.Duration(playSeconds)*time.Second)
	defer playCancel()

	aiStop := make(chan struct{})
	go robotAI.RunLoop(aiStop)

	select {
	case <-playCtx.Done():
	case <-stop:
	}
	close(aiStop)

	// Trigger a forced scene switch right before logout to maximise the
	// chance of an extra SavePlayerToRedis fire on the cpp side.
	_ = gc.SendRequest(game.SceneSceneClientPlayerEnterSceneMessageId, &scene.EnterSceneC2SRequest{
		SceneInfo: &scene.SceneInfoComp{SceneConfigId: player.GetSceneConfigID()},
	})
	time.Sleep(200 * time.Millisecond)

	_ = gc.SendRequest(game.ClientPlayerLoginLeaveGameMessageId, &login.LeaveGameRequest{})
	// Give the gate/scene a moment to flush the snapshot to Kafka.
	time.Sleep(500 * time.Millisecond)

	gc.Close()
	<-recvDone

	if gc.SaToken != "" {
		revokeSaToken(cfg.SaTokenAddr, gc.SaToken)
	}

	// Pick up any in-session token rotation so the next round skips
	// dev-login entirely.
	if access, _, accessExp, _ := gc.SnapshotTokens(); access != "" {
		newAccessToken = access
		newAccessExpire = accessExp
	}

	ok = true
	return
}
