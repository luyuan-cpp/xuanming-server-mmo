package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"

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

	// Blank-import proto packages so they register with protoregistry.
	_ "proto/chat"
)

// robotStatsRef is a process-wide handle to the metrics aggregator. It's
// populated in main() and read by lower-level helpers that don't (yet) take
// a Stats pointer in their signature — notably the queue-aware AssignGate
// loop in assignGateHTTPLocal, which is reached from resolveGateAddrLocal
// at multiple call sites across login modes (login-test, data-stress, stress).
//
// Plumbing Stats through every caller would touch ~10 functions for marginal
// benefit. The trade-off is "one global, set once at startup" vs "always-
// explicit-arg, never-nil". We pick the global because (a) Stats is a
// singleton by construction, (b) the queue helper has graceful nil-handling
// for unit tests, and (c) the alternative was littering signatures with a
// parameter that almost no caller cares about.
var robotStatsRef *metrics.Stats

func main() {
	cfgPath := flag.String("c", "etc/robot.yaml", "config file path")
	flag.Parse()

	initLogger()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		zap.L().Fatal("load config", zap.Error(err))
	}

	// Load tables (Skill, Class) and auto-resolve skill_ids if not configured.
	cfg.LoadTables()

	stats := metrics.NewStats()
	robotStatsRef = stats

	// Login-test mode: run the scenario suite and exit.
	if cfg.Mode == "login-test" {
		host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
		if err != nil {
			zap.L().Fatal("resolve gate address", zap.Error(err))
		}
		port, _ := strconv.Atoi(portStr)
		runLoginTests(host, port, cfg, stats, tokenPayload, tokenSig)
		_ = stats.ExportBehaviorCSV("behavior_test_results.csv")
		_ = stats.ExportBehaviorJSONL("behavior_test_results.jsonl")
		return
	}

	// Data-stress mode: drive repeated login → play → logout cycles per robot
	// and publish per-player expectations to Redis for the db-side verifier
	// (see go/db/cmd/verifier).
	if cfg.Mode == "data-stress" {
		stop := make(chan struct{})
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			zap.L().Info("data-stress shutdown signal received")
			close(stop)
		}()
		runDataStressMode(cfg, stats, stop)
		_ = stats.ExportBehaviorCSV("behavior_test_results.csv")
		_ = stats.ExportBehaviorJSONL("behavior_test_results.jsonl")
		return
	}

	// Currency-crash-snapshot mode: single login → optional GmAddCurrency →
	// readback → write snapshot JSON → exit. Driven by an external script
	// that runs the robot twice per case (pre/post kill) and diffs the JSON.
	// See docs/notes/currency-crash-window-verification.md.
	if cfg.Mode == "currency-crash-snapshot" {
		host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
		if err != nil {
			zap.L().Fatal("resolve gate address", zap.Error(err))
		}
		port, _ := strconv.Atoi(portStr)
		runCurrencyCrashSnapshotMode(host, port, cfg, stats, tokenPayload, tokenSig)
		return
	}

	// Battle-smoke 模式:两个机器人做"回合制战斗 + 观战"端到端冒烟
	// (A 排队 PVE_SOLO 自动战斗,B 观战匹配跟看),断言全过退出码 0,
	// 任一步失败退出码 1。见 battle_smoke_scenario.go 与
	// docs/design/turn-based-battle-server.md §9 的 robot battle 动作预留。
	if cfg.Mode == "battle-smoke" {
		RunBattleSmoke(cfg)
		return
	}

	// Attribute-smoke 模式:单机器人做「角色属性加点」端到端冒烟
	// (拉面板 → 提级发点 → 自动加点预览 → 确认 → 各类拒绝守卫 → 方案隔离 → 洗点返还)。
	// 见 attribute_smoke_scenario.go 与 docs/design/player-attribute-allocation.md。
	if cfg.Mode == "attribute-smoke" {
		RunAttributeSmoke(cfg)
		return
	}

	stopReport := make(chan struct{})
	reportInterval := time.Duration(cfg.ReportInterval) * time.Second
	if reportInterval <= 0 {
		reportInterval = 5 * time.Second
	}
	stats.StartReporter(reportInterval, stopReport)

	zap.L().Info("starting robots",
		zap.String("gateway", cfg.GatewayAddr),
		zap.Int("count", cfg.RobotCount),
	)

	stopAll := make(chan struct{})

	var wg sync.WaitGroup
	for i := 1; i <= cfg.RobotCount; i++ {
		account := fmt.Sprintf(cfg.AccountFmt, i)
		wg.Add(1)
		go func(account string) {
			defer func() {
				if r := recover(); r != nil {
					zap.L().Error("robot panic", zap.String("account", account), zap.Any("panic", r))
				}
				wg.Done()
			}()
			runRobot(account, cfg, stats, stopAll)
		}(account)
		time.Sleep(50 * time.Millisecond) // stagger to avoid thundering herd
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	zap.L().Info("shutting down...")
	close(stopAll)
	close(stopReport)
	wg.Wait()
	_ = stats.ExportBehaviorCSV("behavior_test_results.csv")
	_ = stats.ExportBehaviorJSONL("behavior_test_results.jsonl")
	zap.L().Info("all robots stopped")
}

// runRobot is the full lifecycle for one robot (one goroutine).
func runRobot(account string, cfg *config.Config, stats *metrics.Stats, stop <-chan struct{}) {
	const maxRetries = 5
	// Token-expiry refetches deliberately do NOT consume the normal retry
	// budget (see the tokenExpired branch below), but they MUST be bounded:
	// otherwise a permanently-unreachable / mis-signing gate would make a robot
	// spin forever (attempt-- never advances), turning a real outage into an
	// invisible hang instead of a counted failure. The cap is generous —
	// transient expiry during the 78k-concurrency ramp only needs a handful of
	// refetches. Round 19 hardening.
	const maxTokenExpiredRetries = 20
	backoff := 3 * time.Second
	tokenExpiredRetries := 0

	// access_token reconnect cache: carried across retry attempts so a failed
	// session (e.g. scene-ready timeout) doesn't pay the full primary-auth
	// cost again. Cleared implicitly when the server rotates the token.
	var cachedAccessToken string
	var cachedAccessExpire int64

	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-stop:
			return
		default:
		}

		if attempt > 1 {
			zap.L().Info("retrying login", zap.String("account", account), zap.Int("attempt", attempt), zap.Duration("backoff", backoff))
			select {
			case <-stop:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}

		ok, tok, exp, tokenExpired := runRobotOnce(account, cfg, stats, stop, cachedAccessToken, cachedAccessExpire)
		cachedAccessToken, cachedAccessExpire = tok, exp
		if ok {
			return // played successfully (or stop signal)
		}
		// R17 R2 收尾(Round 19): VerifyGateToken 失败 = gate token 已过期,
		// 这是 dispatcher 签名时间 → robot 拨上来的链路延迟问题,不是 robot
		// 自己的错。下一次 attempt 会重新走 resolveGateAddrLocal 拿全新 token,
		// 所以这一轮不应该消耗 retry 预算,也不算 login_fail。
		// 抵消本次 attempt 自增,backoff 用一个非常短的固定值(token 拿新的就 OK)。
		// 但有上限:连续 token 过期超过 maxTokenExpiredRetries 说明 gate 真的有
		// 问题(宕机 / 时钟漂移 / 签名错),此时给一次 LoginFail 让它显式可见,
		// 而不是无限自旋假装"没失败"。
		if tokenExpired {
			tokenExpiredRetries++
			if tokenExpiredRetries > maxTokenExpiredRetries {
				zap.L().Error("robot gave up after repeated gate-token expiry",
					zap.String("account", account),
					zap.Int("tokenExpiredRetries", tokenExpiredRetries))
				stats.LoginFail()
				return
			}
			attempt--
			backoff = 200 * time.Millisecond
		}
	}
	zap.L().Error("robot gave up after retries", zap.String("account", account), zap.Int("maxRetries", maxRetries))
}

// runRobotOnce attempts a single connect→login→play cycle.
// Returns (ok, newAccessToken, newAccessExpire, tokenExpired):
//   - ok=true: session ended gracefully, caller should stop retrying.
//   - ok=false: caller should back off and retry.
//   - tokenExpired=true: VerifyGateToken 失败,外层应该 attempt-- 抵消本次 retry,
//     直接重新走 AssignGate 拿新 token(R17 R2 收尾,Round 19)。
//
// On login failure the previous cached token is preserved (maybe the issue was
// transient). On successful login we hand back whatever the server rotated to,
// falling back to the incoming token if access_token reconnect took the path
// where the server doesn't rotate.
func runRobotOnce(account string, cfg *config.Config, stats *metrics.Stats, stop <-chan struct{}, prevAccessToken string, prevAccessExpire int64) (ok bool, newAccessToken string, newAccessExpire int64, tokenExpired bool) {
	newAccessToken, newAccessExpire = prevAccessToken, prevAccessExpire
	// Fetch a fresh gate token for each connection attempt so the gate token
	// TTL (10min since Round 19, see login assigngatelogic.gateTokenTTL) is
	// never stale — even under high robot counts or retry backoff.
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		zap.L().Error("resolve gate address failed", zap.String("account", account), zap.Error(err))
		stats.LoginFail()
		return
	}
	port, _ := strconv.Atoi(portStr)

	gc, err := pkg.NewGameClient(host, port)
	if err != nil {
		zap.L().Error("connect failed", zap.String("account", account), zap.Error(err))
		stats.LoginFail()
		return
	}
	// Order matters: Disconnect must go out BEFORE we close the TCP, otherwise
	// gate sees an EOF on a verified session, drops it, and login never finds
	// out. Once login receives Disconnect it marks the player_locator session
	// as Disconnecting (with a 30s reconnect lease) — without this, repeated
	// dev runs leave residual ONLINE sessions that flip the next EnterGame
	// into ReplaceLogin against a now-dead gate. See docs/design/
	// stress-test-2026-05-http-login.md §四 #B-1 for the diagnosis.
	defer gc.Close()
	defer sendDisconnectBestEffort(gc)
	stats.Connected()
	defer stats.Disconnected()

	gc.Account = account
	time.Sleep(500 * time.Millisecond)

	if len(tokenPayload) > 0 {
		if err := gc.VerifyGateToken(tokenPayload, tokenSig); err != nil {
			// R17 R2 收尾(Round 19): gate token 过期不算 robot 的错,也不消耗
			// retry 预算 —— 让外层重新走 AssignGate 拿一个全新 token。计入
			// gate_token_retry 计数器以便复盘观测。详见
			// docs/design/stress-1zone-45k-2026-06-04-round18.md §R2。
			zap.L().Warn("gate token verify failed, will refetch", zap.String("account", account), zap.Error(err))
			stats.GateTokenRetry()
			tokenExpired = true
			return
		}
	}

	loginStart := time.Now()
	if err := loginAndEnterWithAuth(gc, cfg, stats, prevAccessToken, prevAccessExpire); err != nil {
		zap.L().Error("login flow failed", zap.String("account", account), zap.Error(err))
		return
	}
	// Capture whatever the server rotated to (password/satoken paths) or keep
	// the prev token (access_token reconnect path doesn't rotate). Read
	// through SnapshotTokens so we don't race the RefreshToken handler.
	if access, _, accessExp, _ := gc.SnapshotTokens(); access != "" {
		newAccessToken = access
		newAccessExpire = accessExp
	}
	stats.LoginOK(time.Since(loginStart))
	stats.EnterOK()

	zap.L().Info("entered game",
		zap.String("account", account),
		zap.Uint64("player_id", gc.PlayerId),
	)

	// Register player in global list so message handlers can find it.
	player := gameobject.NewPlayer(gc.PlayerId)
	gameobject.PlayerList.Set(gc.PlayerId, player)
	defer gameobject.PlayerList.Delete(gc.PlayerId)

	// Register this connection so the RefreshToken handler (which only has
	// the Player, not the GameClient) can patch our token state in place.
	pkg.Clients.Register(gc.PlayerId, gc)
	defer pkg.Clients.Unregister(gc.PlayerId, gc)

	// Start RecvLoop early so we can receive NotifyEnterScene.
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		gc.RecvLoop(func(client *pkg.GameClient, msg *base.MessageContent) {
			stats.MsgRecv()
			handler.MessageBodyHandler(client, msg)
		})
	}()

	// Proactively refresh access_token when it's close to expiry so a very
	// long-running session doesn't force the next reconnect into the
	// primary-auth fallback. Exits when the recv loop or stop closes.
	go runTokenRefresher(gc, cfg.GatewayAddr, stats, stop, recvDone)

	// Wait for scene node to be bound (NotifyEnterScene) before sending scene-targeted messages.
	// Round 20: 15s -> 60s. EnterGame already succeeded here (we have a player_id);
	// a real UE client does NOT tear down its verified session and re-auth just
	// because the first NotifyEnterScene is slow. Under 45k concentrated players the
	// scene EventLoop backlog can delay NotifyEnterScene past 15s, and the old short
	// timeout turned every slow load into disconnect -> reconnect -> re-EnterScene,
	// a self-amplifying retry storm (R19: ~36k give-up leaves churning the scene).
	// A patient wait lets the saturated-but-draining scene deliver the notify, which
	// collapses the churn and lets the backlog drain for everyone.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer waitCancel()
	if err := player.WaitSceneReady(waitCtx); err != nil {
		zap.L().Error("timed out waiting for scene ready", zap.String("account", gc.Account), zap.Error(err))
		return
	}

	// Now safe to send scene-targeted requests.
	_ = gc.SendRequest(game.SceneSkillClientPlayerListSkillsMessageId, &scene.ListSkillsRequest{})

	// Wait for the ListSkills response so the AI loop only casts skills the player actually owns.
	skillsCtx, skillsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer skillsCancel()
	if err := player.WaitSkillsReady(skillsCtx); err != nil {
		zap.L().Warn("timed out waiting for skill list", zap.String("account", gc.Account), zap.Error(err))
	}

	robotAI := ai.NewRobotAI(gc, stats)
	robotAI.SetPlayer(player)
	if len(cfg.SkillIDs) > 0 {
		robotAI.SetSkillIDs(cfg.SkillIDs)
	}
	if cfg.ActionInterval > 0 {
		robotAI.SetInterval(time.Duration(cfg.ActionInterval) * time.Second)
	}
	profile := cfg.ResolveProfile()
	if profile != nil {
		robotAI.SetProfile(profile)
	}
	if cfg.LLM.Enabled {
		robotAI.SetLLM(ai.NewLLMAdvisor(cfg.LLM.Endpoint, cfg.LLM.APIKey, cfg.LLM.Model))
	}
	go robotAI.RunLoop(stop)

	// Block until connection closes or stop signal.
	select {
	case <-recvDone:
	case <-stop:
	}

	// Graceful shutdown: send LeaveGame so server cleans up session.
	_ = gc.SendRequest(game.ClientPlayerLoginLeaveGameMessageId, &login.LeaveGameRequest{})
	if gc.SaToken != "" {
		revokeSaToken(cfg.SaTokenAddr, gc.SaToken)
	}
	// Re-snapshot: an in-session refresh may have rotated the token after
	// the post-login capture above; don't hand a stale value back to runRobot.
	if access, _, accessExp, _ := gc.SnapshotTokens(); access != "" {
		newAccessToken = access
		newAccessExpire = accessExp
	}
	ok = true
	return
}

func initLogger() {
	cfg := zap.NewDevelopmentConfig()
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	cfg.Level.SetLevel(zap.InfoLevel)
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	zap.ReplaceGlobals(logger)
}

type gateAssignmentLocal struct {
	addr      string
	payload   []byte
	signature []byte
}

// resolveGateAddrLocal keeps main.go runnable as a single-file command.
func resolveGateAddrLocal(cfg *config.Config) (host, port string, payload, signature []byte, err error) {
	return resolveGateViaHTTPLocal(cfg)
}

// resolveGateViaHTTPLocal calls POST /api/assign-gate (HTTP mode) with retry.
//
// Queue-aware behavior (added 2026-05): when /api/assign-gate returns
// code=100 (queueing) we delegate to AssignGateWithQueue so the robot
// transparently polls /api/queue-status until admitted, expired, or the
// outer retry budget runs out. This makes a stress run with N > zoneCapacity
// robots actually exercise the queue path instead of treating it as an error.
func resolveGateViaHTTPLocal(cfg *config.Config) (host, port string, payload, signature []byte, err error) {
	zoneID := cfg.ZoneID
	if zoneID == 0 {
		zoneID, err = resolveZoneIDLocal(cfg.GatewayAddr)
		if err != nil {
			zoneID = 0
		}
	}

	const maxRetries = 30
	backoff := 2 * time.Second
	for attempt := 1; attempt <= maxRetries; attempt++ {
		result, assignErr := assignGateHTTPLocal(cfg, zoneID)
		if assignErr == nil {
			h, p, splitErr := net.SplitHostPort(result.addr)
			return h, p, result.payload, result.signature, splitErr
		}
		if attempt < maxRetries {
			time.Sleep(backoff)
			if backoff < 10*time.Second {
				backoff = backoff * 3 / 2
			}
		}
	}
	return "", "", nil, nil, fmt.Errorf("AssignGate failed after retries")
}

func resolveZoneIDLocal(gatewayAddr string) (uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayAddr+"/api/server-list", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Zones []struct {
			ZoneID      uint32 `json:"zone_id"`
			Recommended bool   `json:"recommended"`
		} `json:"zones"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	if len(result.Zones) == 0 {
		return 0, fmt.Errorf("no zones")
	}
	for _, z := range result.Zones {
		if z.Recommended {
			return z.ZoneID, nil
		}
	}
	return result.Zones[0].ZoneID, nil
}

// assignGateHTTPLocal drives one queue-aware AssignGate flow.
//
// On code=100 it sleeps RetryAfterMs and polls /api/queue-status
// (or re-calls /api/assign-gate when the queue source is Bucket4j ratelimit,
// which has no queue token). The caller's outer retry loop handles network
// errors and 4xx; this function only returns when the server says
// "admitted, here's a gate token" or after the queue-aware budget elapses.
//
// Per-robot queue stats (entered / admitted-after-wait / expired / wait
// duration / max rank) are pushed into Stats so the periodic report can
// surface them — that's how we tell "queue worked but is slow" apart from
// "dispatcher hung".
func assignGateHTTPLocal(cfg *config.Config, zoneId uint32) (*gateAssignmentLocal, error) {
	// Total budget for queue-aware polling. Generous on purpose: a real
	// queue under load can take minutes to drain. The outer
	// resolveGateViaHTTPLocal retry loop adds another layer on top.
	const queueBudget = 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), queueBudget)
	defer cancel()

	req := &httpAssignGateRequest{
		ZoneID:  zoneId,
		Account: "", // robot doesn't have a stable account at this stage
	}

	stats := robotStatsRef // pkg-level handle set in main()
	var firstQueueAt time.Time
	var maxRankSeen int64 = -1
	enteredQueue := false

	rsp, err := httpAssignGate(cfg.GatewayAddr, req, 5*time.Second)
	if err != nil {
		return nil, err
	}
	for {
		switch rsp.Code {
		case 0:
			// Admitted. If we ever queued, record wait time.
			if enteredQueue && stats != nil {
				stats.QueueAdmittedAfterWait()
				stats.QueueWait(time.Since(firstQueueAt))
				if maxRankSeen >= 0 {
					stats.QueueRankSeen(maxRankSeen)
				}
			}
			if rsp.GateIP == "" || rsp.GatePort == 0 {
				return nil, fmt.Errorf("AssignGate admitted but returned empty address")
			}
			return &gateAssignmentLocal{
				addr:      fmt.Sprintf("%s:%d", rsp.GateIP, rsp.GatePort),
				payload:   rsp.TokenPayload,
				signature: rsp.TokenSignature,
			}, nil

		case 100:
			if !enteredQueue {
				enteredQueue = true
				firstQueueAt = time.Now()
				if stats != nil {
					stats.QueueEntered()
				}
			}
			if rsp.QueueRank > maxRankSeen {
				maxRankSeen = rsp.QueueRank
			}
			// Stash the token on req so a subsequent /assign-gate retry (e.g.
			// after a transient network blip in the polling loop) goes down
			// the server's reentry path instead of creating a duplicate
			// queue entry. AssignGate handler verifies queue_token and
			// reuses the existing queueId.
			if rsp.QueueSource == "login" && rsp.QueueToken != "" {
				req.QueueToken = rsp.QueueToken
			}

			wait := time.Duration(rsp.RetryAfterMs) * time.Millisecond
			if wait <= 0 {
				wait = 2 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("queue budget exceeded (rank=%d, source=%s)", rsp.QueueRank, rsp.QueueSource)
			case <-time.After(wait):
			}

			// Real-queue path: keep the queue_token and poll /queue-status.
			// Bucket4j-ratelimit path: no token, re-call /assign-gate so a
			// fresh limiter check decides whether we're allowed through now.
			if rsp.QueueSource == "login" && rsp.QueueToken != "" {
				rsp, err = httpQueueStatus(cfg.GatewayAddr, rsp.QueueToken, req.ZoneID, 5*time.Second)
			} else {
				rsp, err = httpAssignGate(cfg.GatewayAddr, req, 5*time.Second)
			}
			if err != nil {
				return nil, err
			}

		case 410:
			// Queue token expired (we polled too slowly, or the dispatcher
			// admitted us but our admit:{queueId} TTL'd before we polled).
			// Restart from /assign-gate without the token — backend will
			// either fast-path us through or re-enqueue with a new token.
			if stats != nil {
				stats.QueueExpiredToken()
			}
			req.QueueToken = ""
			rsp, err = httpAssignGate(cfg.GatewayAddr, req, 5*time.Second)
			if err != nil {
				return nil, err
			}

		case 429:
			return nil, fmt.Errorf("rate-limited: %s", rsp.Error)

		default:
			return nil, fmt.Errorf("AssignGate code=%d err=%q", rsp.Code, rsp.Error)
		}
	}
}

const defaultLoginTimeoutLocal = 15 * time.Second

// loginAndEnterLocal performs: login -> create player (if needed) -> enter game.
func loginAndEnterLocal(gc *pkg.GameClient, password string, stats *metrics.Stats) error {
	var lr login.LoginResponse
	if err := sendAndRecvLocal(gc, stats,
		game.ClientPlayerLoginLoginMessageId,
		&login.LoginRequest{Account: gc.Account, Password: password},
		&lr,
	); err != nil {
		stats.LoginFail()
		return fmt.Errorf("login: %w", err)
	}
	if lr.ErrorMessage != nil {
		stats.LoginFail()
		return fmt.Errorf("login: server error %v", lr.ErrorMessage)
	}

	if lr.AccessToken != "" {
		gc.SetTokens(lr.AccessToken, lr.RefreshToken, lr.AccessTokenExpire, lr.RefreshTokenExpire)
	}

	if len(lr.Players) == 0 {
		var cr login.CreatePlayerResponse
		if err := sendAndRecvLocal(gc, stats,
			game.ClientPlayerLoginCreatePlayerMessageId,
			&login.CreatePlayerRequest{},
			&cr,
		); err != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: %w", err)
		}
		if cr.ErrorMessage != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: server error %v", cr.ErrorMessage)
		}
		lr.Players = cr.Players
	}
	if len(lr.Players) == 0 {
		stats.LoginFail()
		return fmt.Errorf("no players after create")
	}

	playerID := lr.Players[0].GetPlayer().GetPlayerId()
	var er login.EnterGameResponse
	if err := sendAndRecvLocal(gc, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerID},
		&er,
	); err != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: %w", err)
	}
	if er.ErrorMessage != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: server error %v", er.ErrorMessage)
	}

	gc.PlayerId = er.PlayerId
	return nil
}

// loginAndEnterWithAuth routes to the correct login implementation based on cfg.AuthType.
// If prevAccessToken is non-empty and not near expiry, it is tried first; on
// failure we transparently fall back to the configured primary auth provider.
func loginAndEnterWithAuth(gc *pkg.GameClient, cfg *config.Config, stats *metrics.Stats, prevAccessToken string, prevAccessExpire int64) error {
	// Use access_token reconnect when available and with at least 60s of TTL left.
	if prevAccessToken != "" && (prevAccessExpire == 0 || time.Now().Unix() < prevAccessExpire-60) {
		if err := loginAndEnterAccessToken(gc, prevAccessToken, stats); err == nil {
			stats.AccessReconnectOK()
			zap.L().Debug("access_token reconnect ok", zap.String("account", gc.Account))
			return nil
		} else {
			stats.AccessReconnectFallback()
			zap.L().Info("access_token reconnect failed, falling back",
				zap.String("account", gc.Account), zap.Error(err))
		}
	}
	// New path: do the heavy auth via Java Gateway HTTP, then use the
	// returned access_token to do a lightweight Login on the gate TCP
	// channel just to bind the session. Falls back to legacy on any
	// non-2xx so a half-rolled gateway doesn't break stress runs.
	if cfg.UseHttpLogin {
		if err := loginAndEnterViaHttpGateway(gc, cfg, stats); err == nil {
			return nil
		} else {
			zap.L().Warn("http-login path failed, falling back to legacy gate Login RPC",
				zap.String("account", gc.Account), zap.Error(err))
		}
	}
	if cfg.AuthType == "satoken" {
		return loginAndEnterSaToken(gc, cfg.SaTokenAddr, stats)
	}
	return loginAndEnterLocal(gc, cfg.Password, stats)
}

// loginAndEnterViaHttpGateway exercises the new Java Gateway /api/login path.
//
// Why the gate TCP still runs ClientPlayerLogin.Login afterwards:
//
//	The gate-side Login RPC's job is twofold — it (a) verifies credentials
//	and (b) binds the {session_id, account} pair via loginsession.Save +
//	the device-set so that EnterGame can find the account. (a) was already
//	done over HTTP, so we use auth_type="access_token" here to skip the
//	provider round-trip; (b) still has to happen on the actual TCP socket.
//
// This is exactly the static-shape we want for production clients: one
// HTTP round trip moves the OAuth/3rd-party heavy lifting off the gate, and
// the gate Login RPC degenerates into a Redis lookup + a Kafka BindSession.
func loginAndEnterViaHttpGateway(gc *pkg.GameClient, cfg *config.Config, stats *metrics.Stats) error {
	loginReq := &httpLoginRequest{
		ZoneID:   cfg.ZoneID,
		Account:  gc.Account,
		Password: cfg.Password,
		AuthType: cfg.AuthType,
	}
	// SA-Token path: behave exactly like the legacy SA-Token flow, only
	// the credential exchange happens over HTTP instead of via the gate.
	if cfg.AuthType == "satoken" {
		tok, err := fetchSaToken(cfg.SaTokenAddr, gc.Account)
		if err != nil {
			stats.LoginFail()
			return fmt.Errorf("fetch satoken: %w", err)
		}
		gc.SaToken = tok
		loginReq.AuthToken = tok
	}

	rsp, err := httpLogin(cfg.GatewayAddr, loginReq, 10*time.Second)
	if err != nil {
		stats.LoginFail()
		return fmt.Errorf("/api/login: %w", err)
	}
	switch rsp.Code {
	case 0:
		// fall through
	case 100, 101, 429:
		// queue / rate-limit — surface as "fail" so the outer retry loop
		// honours the backoff. retry_after_ms hint is logged but the
		// outer loop owns the actual sleep.
		stats.LoginFail()
		return fmt.Errorf("/api/login queue (code=%d retry_after_ms=%d)", rsp.Code, rsp.RetryAfterMs)
	default:
		stats.LoginFail()
		return fmt.Errorf("/api/login error code=%d msg=%s", rsp.Code, rsp.Message)
	}
	if rsp.AccessToken == "" {
		stats.LoginFail()
		return fmt.Errorf("/api/login returned empty access_token")
	}
	gc.SetTokens(rsp.AccessToken, rsp.RefreshToken, rsp.AccessTokenExpire, rsp.RefreshTokenExpire)

	// Now run the cheap path on the gate TCP channel: server sees
	// auth_type="access_token" and skips provider validation entirely.
	return loginAndEnterAccessToken(gc, rsp.AccessToken, stats)
}

func sendAndRecvLocal(gc *pkg.GameClient, stats *metrics.Stats, msgID uint32, req, resp proto.Message) error {
	if err := gc.SendRequest(msgID, req); err != nil {
		return err
	}
	stats.MsgSent()

	type recvResult struct {
		msg *base.MessageContent
		err error
	}
	ch := make(chan recvResult, 1)

	go func() {
		for {
			raw, err := gc.RecvOne()
			if err != nil {
				ch <- recvResult{err: err}
				return
			}
			stats.MsgRecv()
			if raw.MessageId == msgID {
				ch <- recvResult{msg: raw}
				return
			}
			gc.DeferMessage(raw)
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		return proto.Unmarshal(r.msg.SerializedMessage, resp)
	case <-time.After(defaultLoginTimeoutLocal):
		stats.LoginStuck()
		return fmt.Errorf("STUCK: no response for msg_id=%d within %s", msgID, defaultLoginTimeoutLocal)
	}
}
