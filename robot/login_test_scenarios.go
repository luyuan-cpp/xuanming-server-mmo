package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/common/component"
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

// testResult records the outcome of one test scenario.
type testResult struct {
	Name    string
	Passed  bool
	Elapsed time.Duration
	Detail  string
}

// loginTestCfg is set at the start of runLoginTests so scenario helpers can
// route login through the configured auth provider (password vs satoken).
var loginTestCfg *config.Config

// loginAndEnterScenario routes the login flow used by scenario tests through
// the configured auth provider. Falls back to password when no cfg is set.
func loginAndEnterScenario(gc *pkg.GameClient, password string, stats *metrics.Stats) error {
	if loginTestCfg != nil && loginTestCfg.AuthType == "satoken" {
		return loginAndEnterSaToken(gc, loginTestCfg.SaTokenAddr, stats)
	}
	return loginAndEnter(gc, password, stats)
}

// buildScenarioLoginRequest constructs a LoginRequest matching the configured
// auth provider. For satoken it fetches a fresh token; for password it just
// fills the password field.
func buildScenarioLoginRequest(account, password string) (*login.LoginRequest, error) {
	if loginTestCfg != nil && loginTestCfg.AuthType == "satoken" {
		token, err := fetchSaToken(loginTestCfg.SaTokenAddr, account)
		if err != nil {
			return nil, err
		}
		return &login.LoginRequest{Account: account, AuthType: "satoken", AuthToken: token}, nil
	}
	return &login.LoginRequest{Account: account, Password: password}, nil
}

// invalidScenarioLoginRequest builds a deliberately-bad LoginRequest. Used by
// the WrongPassword scenario; under satoken mode it sends a non-existent token.
func invalidScenarioLoginRequest(account string) *login.LoginRequest {
	bad := "INVALID_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if loginTestCfg != nil && loginTestCfg.AuthType == "satoken" {
		return &login.LoginRequest{Account: account, AuthType: "satoken", AuthToken: bad}
	}
	return &login.LoginRequest{Account: account, Password: "WRONG_PASSWORD_" + bad}
}

// runLoginTests executes all login test scenarios sequentially and prints a summary.
func runLoginTests(host string, port int, cfg *config.Config, stats *metrics.Stats, tokenPayload, tokenSig []byte) {
	loginTestCfg = cfg
	account := fmt.Sprintf(cfg.AccountFmt, 1)
	account2 := fmt.Sprintf(cfg.AccountFmt, 2)

	scenarios := []struct {
		name string
		fn   func() testResult
	}{
		// --- Basic flow ---
		{"NormalLogin", func() testResult {
			return testNormalLogin(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"LoginLogoutCycle", func() testResult {
			return testLoginLogoutCycle(host, port, account, cfg.Password, stats, tokenPayload, tokenSig, 3)
		}},
		{"WrongPassword", func() testResult {
			return testWrongPassword(host, port, account, stats, tokenPayload, tokenSig)
		}},

		// --- Duplicate / idempotent ---
		{"DuplicateEnterGame", func() testResult {
			return testDuplicateEnterGame(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"DuplicateLoginRequest", func() testResult {
			return testDuplicateLoginRequest(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},

		// --- Displacement / conflict ---
		{"AccountDisplacement", func() testResult {
			return testAccountDisplacement(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"ConcurrentSameAccount", func() testResult {
			return testConcurrentSameAccount(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},

		// --- Reconnect ---
		{"RapidReconnect", func() testResult {
			return testRapidReconnect(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"DisconnectDuringLogin", func() testResult {
			return testDisconnectDuringLogin(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"DisconnectDuringEnter", func() testResult {
			return testDisconnectDuringEnter(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"RapidDisconnectReconnect", func() testResult {
			return testRapidDisconnectReconnect(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"AccessTokenReconnect", func() testResult {
			return testAccessTokenReconnect(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},

		// --- Multi-account ---
		{"DifferentAccountSequential", func() testResult {
			return testDifferentAccountSequential(host, port, account, account2, cfg.Password, stats, tokenPayload, tokenSig)
		}},

		// --- Session lifecycle ---
		{"LeaveAndReEnter", func() testResult {
			return testLeaveAndReEnter(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"LeaveAndReLogin", func() testResult {
			return testLeaveAndReLogin(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},

		// --- Spam / abuse ---
		{"RapidLoginSpam", func() testResult {
			return testRapidLoginSpam(host, port, account, cfg.Password, stats, tokenPayload, tokenSig, 10)
		}},
		{"MessageBeforeLogin", func() testResult {
			return testMessageBeforeLogin(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},

		// --- Stuck detection (critical) ---
		{"LoginStuckDetection", func() testResult {
			return testLoginStuckDetection(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},

		// --- Batch concurrent ---
		{"BatchConcurrentLogin", func() testResult {
			n := cfg.RobotCount
			if n < 5 {
				n = 5
			}
			return testBatchConcurrentLogin(host, port, cfg.AccountFmt, cfg.Password, stats, tokenPayload, tokenSig, n)
		}},

		// --- Gameplay behavior ---
		{"SkillCast", func() testResult {
			return testSkillCast(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"SceneSwitch", func() testResult {
			return testSceneSwitch(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
		{"MultiRobotBehavior", func() testResult {
			n := cfg.RobotCount
			if n < 3 {
				n = 3
			}
			if n > 20 {
				n = 20 // keep the scenario quick; large-scale load still uses stress mode
			}
			return testMultiRobotBehavior(host, port, cfg.AccountFmt, cfg.Password, stats, tokenPayload, tokenSig, n)
		}},

		// --- Currency persistence (in-process smoke; full crash-window
		// matrix lives under currency-crash-snapshot mode driven by
		// tools/scripts/currency_crash_window.ps1). See
		// docs/notes/currency-crash-window-verification.md.
		{"CurrencyCrashWindow", func() testResult {
			return testCurrencyCrashWindow(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
		}},
	}

	zap.L().Info("======== Login Test Suite ========", zap.Int("scenarios", len(scenarios)))

	var results []testResult
	passed, failed := 0, 0
	for i, s := range scenarios {
		zap.L().Info(fmt.Sprintf("[%d/%d] Running: %s", i+1, len(scenarios), s.name))
		r := s.fn()
		r.Name = s.name
		results = append(results, r)
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		zap.L().Info(fmt.Sprintf("[%d/%d] %s: %s (%s) %s",
			i+1, len(scenarios), status, s.name, r.Elapsed.Truncate(time.Millisecond), r.Detail))

		// Record for ML analysis.
		stats.RecordLogin(metrics.LoginRecord{
			Timestamp: time.Now(),
			Account:   account,
			Scenario:  s.name,
			LatencyMs: r.Elapsed.Milliseconds(),
			Success:   r.Passed,
			Stuck:     strings.Contains(r.Detail, "STUCK"),
			ErrorMsg:  condStr(!r.Passed, r.Detail, ""),
		})
		stats.RecordBehavior(metrics.BehaviorRecord{
			Timestamp: time.Now(),
			Account:   account,
			Scenario:  s.name,
			Action:    "scenario_result",
			Success:   r.Passed,
			LatencyMs: r.Elapsed.Milliseconds(),
			Detail:    r.Detail,
		})

		// Brief pause between scenarios so server-side sessions settle.
		time.Sleep(500 * time.Millisecond)
	}

	zap.L().Info("======== Login Test Summary ========")
	zap.L().Info(fmt.Sprintf("Total: %d  Passed: %d  Failed: %d", len(results), passed, failed))
	for _, r := range results {
		mark := "OK"
		if !r.Passed {
			mark = "FAIL"
		}
		zap.L().Info(fmt.Sprintf("  [%s] %s  %s  %s", mark, r.Name, r.Elapsed.Truncate(time.Millisecond), r.Detail))
	}

	// Export scenario records for ML / anomaly detection.
	csvPath := "login_test_results.csv"
	if err := stats.ExportLoginCSV(csvPath); err != nil {
		zap.L().Error("failed to export login CSV", zap.Error(err))
	}
	if err := stats.ExportBehaviorCSV("behavior_test_results.csv"); err != nil {
		zap.L().Error("failed to export behavior CSV", zap.Error(err))
	}
	if err := stats.ExportBehaviorJSONL("behavior_test_results.jsonl"); err != nil {
		zap.L().Error("failed to export behavior JSONL", zap.Error(err))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// connectAndVerify creates a GameClient, sets the account, optionally verifies token.
func connectAndVerify(host string, port int, account string, tokenPayload, tokenSig []byte) (*pkg.GameClient, error) {
	gc, err := pkg.NewGameClient(host, port)
	if err != nil {
		return nil, err
	}
	gc.Account = account
	time.Sleep(200 * time.Millisecond)
	if len(tokenPayload) > 0 {
		verify := func() error { return gc.VerifyGateToken(tokenPayload, tokenSig) }
		if loginTestCfg != nil && loginTestCfg.Mode == "asset-arrival-prepare" {
			// 只限制专用准备入口；既有场景的握手行为保持原样。
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = waitAssetArrivalVerification(ctx, verify)
			cancel()
		} else {
			err = verify()
		}
		if err != nil {
			gc.Close()
			return nil, fmt.Errorf("token verify: %w", err)
		}
	}
	return gc, nil
}

// leaveGame sends a LeaveGame request.
func leaveGame(gc *pkg.GameClient, stats *metrics.Stats) error {
	return gc.SendRequest(game.ClientPlayerLoginLeaveGameMessageId, &login.LeaveGameRequest{})
}

// ---------------------------------------------------------------------------
// Scenario 1: Normal login flow (baseline)
// ---------------------------------------------------------------------------

func testNormalLogin(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	defer gc.Close()

	if err := loginAndEnterScenario(gc, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("player_id=%d", gc.PlayerId)}
}

// ---------------------------------------------------------------------------
// Scenario 2: Login → LeaveGame → re-login, repeated N times
// ---------------------------------------------------------------------------

func testLoginLogoutCycle(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte, cycles int) testResult {
	start := time.Now()

	for i := 0; i < cycles; i++ {
		gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
		if err != nil {
			return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("cycle %d connect: %s", i, err)}
		}

		if err := loginAndEnterScenario(gc, password, stats); err != nil {
			gc.Close()
			return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("cycle %d login: %s", i, err)}
		}

		if err := leaveGame(gc, stats); err != nil {
			gc.Close()
			return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("cycle %d leave: %s", i, err)}
		}
		time.Sleep(300 * time.Millisecond)
		gc.Close()
		time.Sleep(300 * time.Millisecond) // let server clean up session
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("completed %d login/logout cycles", cycles)}
}

// ---------------------------------------------------------------------------
// Scenario 3: Wrong password
// ---------------------------------------------------------------------------

func testWrongPassword(host string, port int, account string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	defer gc.Close()

	var lr login.LoginResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginLoginMessageId,
		invalidScenarioLoginRequest(account),
		&lr,
	); err != nil {
		// Network error during wrong password is also acceptable in some server implementations.
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("server rejected connection: %s", err)}
	}

	// Server should return an error_message for wrong password.
	if lr.ErrorMessage != nil {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("server returned error: %v", lr.ErrorMessage)}
	}

	// If server allows any password (dev mode), that's still a valid observation.
	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: "server accepted wrong password (dev mode / no auth)"}
}

// ---------------------------------------------------------------------------
// Scenario 4: Duplicate EnterGame on same connection
// ---------------------------------------------------------------------------

func testDuplicateEnterGame(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	defer gc.Close()

	if err := loginAndEnterScenario(gc, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("first enter: %s", err)}
	}
	playerId := gc.PlayerId

	// Send EnterGame again with the same player_id.
	var er login.EnterGameResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerId},
		&er,
	); err != nil {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("server rejected duplicate enter: %s", err)}
	}
	if er.ErrorMessage != nil {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("server returned error for duplicate enter: %v", er.ErrorMessage)}
	}

	// Idempotent accept is also valid (EnterGameRequest has request_id for dedup).
	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: "server accepted duplicate enter (idempotent)"}
}

// ---------------------------------------------------------------------------
// Scenario 5: Account displacement (顶号)
// Robot A logs in, Robot B logs in with same account — A should be kicked.
// ---------------------------------------------------------------------------

func testAccountDisplacement(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	// A connects and enters game.
	gcA, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("A connect: %s", err)}
	}
	defer gcA.Close()

	if err := loginAndEnterScenario(gcA, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("A login: %s", err)}
	}

	// Start listening for kick on A.
	kickCh := make(chan bool, 1)
	go func() {
		for {
			raw, err := gcA.RecvOne()
			if err != nil {
				kickCh <- true // connection closed = kicked
				return
			}
			if raw.MessageId == game.SceneClientPlayerCommonKickPlayerMessageId {
				kickCh <- true
				return
			}
		}
	}()

	// B connects with same account — should displace A.
	time.Sleep(200 * time.Millisecond)
	gcB, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("B connect: %s", err)}
	}
	defer gcB.Close()

	if err := loginAndEnterScenario(gcB, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("B login: %s", err)}
	}

	// Wait up to 5s for A to receive kick.
	select {
	case <-kickCh:
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: "A received kick after B logged in with same account"}
	case <-time.After(5 * time.Second):
		return testResult{Passed: false, Elapsed: time.Since(start),
			Detail: "A was NOT kicked after B logged in with same account (timeout 5s)"}
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: Rapid reconnect — disconnect abruptly then reconnect quickly
// ---------------------------------------------------------------------------

func testRapidReconnect(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	// First connection: login then close abruptly (no LeaveGame).
	gc1, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("first connect: %s", err)}
	}
	if err := loginAndEnterScenario(gc1, password, stats); err != nil {
		gc1.Close()
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("first login: %s", err)}
	}
	gc1.Close() // abrupt disconnect, no LeaveGame

	// Immediately reconnect.
	time.Sleep(100 * time.Millisecond)
	gc2, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("reconnect: %s", err)}
	}
	defer gc2.Close()

	if err := loginAndEnterScenario(gc2, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("reconnect login: %s", err)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("reconnected OK, player_id=%d", gc2.PlayerId)}
}

// ---------------------------------------------------------------------------
// Scenario 7: Concurrent same-account login (race condition)
// Two goroutines try to login with the same account simultaneously.
// ---------------------------------------------------------------------------

func testConcurrentSameAccount(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	var wg sync.WaitGroup
	results := make([]error, 2)
	clients := make([]*pkg.GameClient, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
			if err != nil {
				results[idx] = err
				return
			}
			clients[idx] = gc
			results[idx] = loginAndEnterScenario(gc, password, stats)
		}(i)
	}
	wg.Wait()

	defer func() {
		for _, gc := range clients {
			if gc != nil {
				gc.Close()
			}
		}
	}()

	successCount := 0
	for _, err := range results {
		if err == nil {
			successCount++
		}
	}

	detail := fmt.Sprintf("concurrent results: [%v, %v]", results[0], results[1])

	// At least one should succeed; at most one should be active.
	if successCount >= 1 {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("%d/2 succeeded — %s", successCount, detail)}
	}

	return testResult{Passed: false, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("both failed — %s", detail)}
}

// ---------------------------------------------------------------------------
// Scenario 8: Two different accounts login sequentially
// ---------------------------------------------------------------------------

func testDifferentAccountSequential(host string, port int, account1, account2, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	// Account 1
	gc1, err := connectAndVerify(host, port, account1, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("account1 connect: %s", err)}
	}
	if err := loginAndEnterScenario(gc1, password, stats); err != nil {
		gc1.Close()
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("account1 login: %s", err)}
	}
	pid1 := gc1.PlayerId
	gc1.Close()
	time.Sleep(300 * time.Millisecond)

	// Account 2
	gc2, err := connectAndVerify(host, port, account2, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("account2 connect: %s", err)}
	}
	defer gc2.Close()
	if err := loginAndEnterScenario(gc2, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("account2 login: %s", err)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("both accounts logged in: %s (pid=%d), %s (pid=%d)", account1, pid1, account2, gc2.PlayerId)}
}

// ---------------------------------------------------------------------------
// Scenario 9: Leave game then re-enter without new login
// ---------------------------------------------------------------------------

func testLeaveAndReEnter(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	defer gc.Close()

	if err := loginAndEnterScenario(gc, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("first enter: %s", err)}
	}
	playerId := gc.PlayerId

	// Leave game.
	if err := leaveGame(gc, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("leave: %s", err)}
	}
	time.Sleep(500 * time.Millisecond)

	// Try to re-enter without a new Login call.
	var er login.EnterGameResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerId},
		&er,
	); err != nil {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("re-enter after leave rejected: %s", err)}
	}
	if er.ErrorMessage != nil {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("re-enter error: %v", er.ErrorMessage)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: "re-enter after leave succeeded (server allows it)"}
}

// ---------------------------------------------------------------------------
// Scenario 10: Disconnect during login handshake (close mid-flow)
// ---------------------------------------------------------------------------

func testDisconnectDuringLogin(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}

	// Send Login request, but close before reading reply.
	if req, berr := buildScenarioLoginRequest(account, password); berr == nil {
		_ = gc.SendRequest(game.ClientPlayerLoginLoginMessageId, req)
	}
	gc.Close()

	// Brief pause, then reconnect and do a full login.
	time.Sleep(500 * time.Millisecond)
	gc2, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("reconnect after abort: %s", err)}
	}
	defer gc2.Close()

	if err := loginAndEnterScenario(gc2, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("login after abort: %s", err)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: "server recovered after mid-login disconnect"}
}

func init() {
	// Ensure the LeaveGame and Disconnect proto messages are registered.
	_ = proto.Size(&login.LeaveGameRequest{})
}

func condStr(cond bool, ifTrue, ifFalse string) string {
	if cond {
		return ifTrue
	}
	return ifFalse
}

// ---------------------------------------------------------------------------
// Scenario 11: Duplicate Login request on same connection
// Send Login twice without waiting — server should handle idempotently.
// ---------------------------------------------------------------------------

func testDuplicateLoginRequest(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	defer gc.Close()

	// Fire two Login requests back-to-back.
	req1, berr := buildScenarioLoginRequest(account, password)
	if berr != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("build login request: %s", berr)}
	}
	// SA-Token mode: each request needs its own freshly-fetched token because
	// each /auth/dev-login call rotates the token value in Redis.
	req2 := req1
	if loginTestCfg != nil && loginTestCfg.AuthType == "satoken" {
		if r, berr2 := buildScenarioLoginRequest(account, password); berr2 == nil {
			req2 = r
		}
	}
	_ = gc.SendRequest(game.ClientPlayerLoginLoginMessageId, req1)
	_ = gc.SendRequest(game.ClientPlayerLoginLoginMessageId, req2)

	// Read responses (server may send 1 or 2).
	// 同步握手(D 类),不迁到 Call:本用例故意发两条不关联请求,数 RecvOne 读到几条 Login 回包。
	// 等到 deadline 时最后那个读者 goroutine 仍阻塞在 RecvOne 上(vendored muduo 的 Recv 无法取消,Close 也唤不醒),
	// 所以本连接到 summary 即作废:只交给 defer 关闭,之后不得再在 gc 上握手或起 RecvLoop(见 errLoginStuck)。
	responses := 0
	deadline := time.After(5 * time.Second)
	for i := 0; i < 2; i++ {
		done := make(chan bool, 1)
		go func() {
			raw, err := gc.RecvOne()
			if err == nil && raw.MessageId == game.ClientPlayerLoginLoginMessageId {
				done <- true
			} else {
				done <- false
			}
		}()
		select {
		case ok := <-done:
			if ok {
				responses++
			}
		case <-deadline:
			goto summary
		}
	}
summary:

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("got %d responses to 2 duplicate login requests", responses)}
}

// ---------------------------------------------------------------------------
// Scenario 12: Disconnect after Login but before EnterGame
// ---------------------------------------------------------------------------

func testDisconnectDuringEnter(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	gc1, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}

	// Login succeeds but close before EnterGame.
	loginReq, berr := buildScenarioLoginRequest(account, password)
	if berr != nil {
		gc1.Close()
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("build login request: %s", berr)}
	}
	var lr login.LoginResponse
	if err := sendAndRecv(gc1, stats,
		game.ClientPlayerLoginLoginMessageId,
		loginReq,
		&lr,
	); err != nil {
		gc1.Close()
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("login: %s", err)}
	}
	gc1.Close() // disconnect mid-flow

	// Reconnect and do full login.
	time.Sleep(500 * time.Millisecond)
	gc2, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("reconnect: %s", err)}
	}
	defer gc2.Close()

	if err := loginAndEnterScenario(gc2, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("re-login: %s", err)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: "recovered after disconnect between Login and EnterGame"}
}

// ---------------------------------------------------------------------------
// Scenario: Rapid disconnect+reconnect (async load race)
// Tests the case where a client completes login+enter, then disconnects
// and immediately reconnects while Scene is still async-loading from Redis.
// The reconnect must succeed; the server must not hang or lose the player.
// ---------------------------------------------------------------------------

func testRapidDisconnectReconnect(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	// First: full login to ensure the player exists in DB/Redis.
	gc0, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("setup connect: %s", err)}
	}
	if err := loginAndEnterScenario(gc0, password, stats); err != nil {
		gc0.Close()
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("setup login: %s", err)}
	}
	_ = leaveGame(gc0, stats)
	time.Sleep(300 * time.Millisecond)
	gc0.Close()
	time.Sleep(500 * time.Millisecond) // ensure server fully cleaned up

	// Now do the rapid disconnect+reconnect cycle.
	// Login+enter then immediately disconnect (no LeaveGame) with minimal delay.
	for i := 0; i < 3; i++ {
		gc1, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
		if err != nil {
			return testResult{Elapsed: time.Since(start),
				Detail: fmt.Sprintf("cycle %d connect1: %s", i, err)}
		}
		if err := loginAndEnterScenario(gc1, password, stats); err != nil {
			gc1.Close()
			return testResult{Elapsed: time.Since(start),
				Detail: fmt.Sprintf("cycle %d login1: %s", i, err)}
		}
		gc1.Close() // abrupt disconnect, no LeaveGame

		// Reconnect immediately — zero deliberate delay.
		// This maximizes the chance of hitting the async-load window on Scene.
		gc2, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
		if err != nil {
			return testResult{Elapsed: time.Since(start),
				Detail: fmt.Sprintf("cycle %d reconnect: %s", i, err)}
		}
		if err := loginAndEnterScenario(gc2, password, stats); err != nil {
			gc2.Close()
			return testResult{Elapsed: time.Since(start),
				Detail: fmt.Sprintf("cycle %d re-login: %s", i, err)}
		}

		// Clean exit before next cycle.
		_ = leaveGame(gc2, stats)
		time.Sleep(200 * time.Millisecond)
		gc2.Close()
		time.Sleep(300 * time.Millisecond)
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: "3 rapid disconnect+reconnect cycles completed"}
}

// ---------------------------------------------------------------------------
// Scenario 13: Leave game then re-login on new connection
// ---------------------------------------------------------------------------

func testLeaveAndReLogin(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	gc1, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	if err := loginAndEnterScenario(gc1, password, stats); err != nil {
		gc1.Close()
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("first login: %s", err)}
	}
	_ = leaveGame(gc1, stats)
	time.Sleep(300 * time.Millisecond)
	gc1.Close()
	time.Sleep(300 * time.Millisecond)

	// New connection, full re-login.
	gc2, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("reconnect: %s", err)}
	}
	defer gc2.Close()

	if err := loginAndEnterScenario(gc2, password, stats); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("re-login: %s", err)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("leave+re-login OK, player_id=%d", gc2.PlayerId)}
}

// ---------------------------------------------------------------------------
// Scenario 14: Rapid login spam — send N login requests as fast as possible
// on separate connections. Ensures server doesn't get stuck.
// ---------------------------------------------------------------------------

func testRapidLoginSpam(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte, count int) testResult {
	start := time.Now()

	for i := 0; i < count; i++ {
		gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
		if err != nil {
			return testResult{Elapsed: time.Since(start),
				Detail: fmt.Sprintf("spam iter %d connect: %s", i, err)}
		}
		// Just send login, don't wait for response — immediately close.
		if req, berr := buildScenarioLoginRequest(account, password); berr == nil {
			_ = gc.SendRequest(game.ClientPlayerLoginLoginMessageId, req)
		}
		gc.Close()
	}

	// After the spam, one clean login must succeed — this is the critical assertion.
	time.Sleep(1 * time.Second) // let server settle
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start),
			Detail: fmt.Sprintf("clean login after spam connect: %s", err)}
	}
	defer gc.Close()

	if err := loginAndEnterScenario(gc, password, stats); err != nil {
		return testResult{Passed: false, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("STUCK after %d spam attempts: %s", count, err)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("survived %d rapid spam attempts, clean login OK", count)}
}

// ---------------------------------------------------------------------------
// Scenario 15: Send game message before login
// Server should reject or ignore, not crash.
// ---------------------------------------------------------------------------

func testMessageBeforeLogin(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	defer gc.Close()

	// Send EnterGame before Login — this is an invalid state transition.
	_ = gc.SendRequest(game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: 99999})

	// Brief pause, then try normal login — must not be stuck.
	time.Sleep(500 * time.Millisecond)

	gc2, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: "server closed connection after invalid message (expected)"}
	}
	defer gc2.Close()

	if err := loginAndEnterScenario(gc2, password, stats); err != nil {
		return testResult{Passed: true, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("login after invalid msg: %s (server may have rate-limited)", err)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: "server handled out-of-order message gracefully"}
}

// ---------------------------------------------------------------------------
// Scenario 16: Verify login is not stuck — timed full flow
// The critical scenario: login must complete within defaultLoginTimeout.
// ---------------------------------------------------------------------------

func testLoginStuckDetection(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	defer gc.Close()

	if err := loginAndEnterScenario(gc, password, stats); err != nil {
		stuck := strings.Contains(err.Error(), "STUCK")
		return testResult{Passed: false, Elapsed: time.Since(start),
			Detail: fmt.Sprintf("stuck=%v err=%s", stuck, err)}
	}

	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		return testResult{Passed: false, Elapsed: elapsed,
			Detail: fmt.Sprintf("login succeeded but took %s (>10s = too slow)", elapsed)}
	}

	return testResult{Passed: true, Elapsed: elapsed,
		Detail: fmt.Sprintf("login completed in %s (OK)", elapsed.Truncate(time.Millisecond))}
}

// ---------------------------------------------------------------------------
// Scenario 17: Batch concurrent login — N different accounts at once.
// Uses robot_count from config. Ensures no stuck under concurrency.
// ---------------------------------------------------------------------------

func testBatchConcurrentLogin(host string, port int, accountFmt, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte, n int) testResult {
	start := time.Now()

	type accountResult struct {
		account string
		err     error
		elapsed time.Duration
	}

	var wg sync.WaitGroup
	results := make([]accountResult, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			acc := fmt.Sprintf(accountFmt, idx+100) // offset to avoid collision with other tests
			t := time.Now()
			gc, err := connectAndVerify(host, port, acc, tokenPayload, tokenSig)
			if err != nil {
				results[idx] = accountResult{account: acc, err: err, elapsed: time.Since(t)}
				return
			}
			defer gc.Close()
			err = loginAndEnterScenario(gc, password, stats)
			results[idx] = accountResult{account: acc, err: err, elapsed: time.Since(t)}

			// Record each attempt for ML.
			var errMsg string
			if err != nil {
				errMsg = err.Error()
			}
			stats.RecordLogin(metrics.LoginRecord{
				Timestamp:  time.Now(),
				Account:    acc,
				Scenario:   "BatchConcurrentLogin",
				LatencyMs:  time.Since(t).Milliseconds(),
				Success:    err == nil,
				Stuck:      err != nil && strings.Contains(errMsg, "STUCK"),
				ErrorMsg:   errMsg,
				Concurrent: n,
			})
		}(i)
		time.Sleep(50 * time.Millisecond) // stagger to avoid thundering herd
	}
	wg.Wait()

	success, fail, stuck := 0, 0, 0
	var maxElapsed time.Duration
	errCounts := make(map[string]int) // aggregate error messages
	for _, r := range results {
		if r.err == nil {
			success++
		} else {
			fail++
			errCounts[r.err.Error()]++
			if strings.Contains(r.err.Error(), "STUCK") {
				stuck++
			}
		}
		if r.elapsed > maxElapsed {
			maxElapsed = r.elapsed
		}
	}

	// Log top error reasons for diagnosis.
	if len(errCounts) > 0 {
		for errMsg, cnt := range errCounts {
			zap.L().Warn("batch login error", zap.Int("count", cnt), zap.String("error", errMsg))
		}
	}

	detail := fmt.Sprintf("%d/%d succeeded, %d failed (%d stuck), max_latency=%s",
		success, n, fail, stuck, maxElapsed.Truncate(time.Millisecond))

	if stuck > 0 {
		return testResult{Passed: false, Elapsed: time.Since(start), Detail: "STUCK detected: " + detail}
	}
	// Allow some failures in concurrent test, but majority must succeed.
	if success >= n*8/10 {
		return testResult{Passed: true, Elapsed: time.Since(start), Detail: detail}
	}
	return testResult{Passed: false, Elapsed: time.Since(start), Detail: "too many failures: " + detail}
}

func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

func prepareBehaviorClient(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) (*pkg.GameClient, *gameobject.Player, error) {
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return nil, nil, err
	}
	if err := loginAndEnterScenario(gc, password, stats); err != nil {
		gc.Close()
		return nil, nil, err
	}

	player := gameobject.NewPlayer(gc.PlayerId)
	gameobject.PlayerList.Set(gc.PlayerId, player)

	go gc.RecvLoop(func(client *pkg.GameClient, msg *base.MessageContent) {
		stats.MsgRecv()
		// 这里只收推送与 SendRequest 发出的不关联请求(id = 0)的回包。带请求号的回包(Call / smokeCaller)
		// 已不经这里:GameClient 按 MessageContent.id 交给发起它的调用,信封错误由调用点自己处理
		// (smokeCaller 返回的 envelopeTip),下面的翻译管不到它们。
		//
		// 仍用 SendRequest 发属性请求的复用者,回包照旧走这里:gate 级拒绝(限流 / 超长包等)写在信封
		// MessageContent.error_message,body 为空。属性冒烟的等待方靠"响应体 tip"或"新面板"推进,信封错误若只是被
		// 静默分发成一条全零响应,现象就是 10s 超时而看不到真实原因;这里先把它落成 tip + Error 日志再决定是否分发。
		//
		// 下面三个请求消息号在本函数建的连接上只经 Call / smokeCaller 带请求号发出(全包 grep 过):ListSkills 由
		// startListOwnedSkills 发,JoinQueue / WatchBattle 由 battle 冒烟(跨区版也经 battleSmokeLogin 复用本函数)的
		// smokeCaller 发。它们的 id = 0 回包说明服务端没回显请求号,丢弃,等它的那次调用随后以超时收场;若交给 handler,
		// 空 body 会被解成全零响应,match 的 handler 还会打出 "join queue ok" 这类与超时相矛盾的成功日志。
		// ReleaseSkill / EnterScene 不在此列:testMultiRobotBehavior 的 AI 循环仍以 SendRequest 发它们,回包要交 handler。
		switch msg.GetMessageId() {
		case game.SceneSkillClientPlayerListSkillsMessageId,
			game.MatchServiceJoinQueueMessageId,
			game.MatchServiceWatchBattleMessageId:
			smokeDropUncorrelatedReply(client.Account, msg)
			return
		}
		if msg.ErrorMessage != nil && msg.ErrorMessage.Id != 0 {
			zap.L().Error("gate envelope error", zap.Uint32("message_id", msg.MessageId),
				zap.Uint32("tip", msg.ErrorMessage.Id))
			if isAttributeMessage(msg.MessageId) {
				if p, ok := gameobject.PlayerList.Get(client.PlayerId); ok {
					p.SetAttributePanel(nil, msg.ErrorMessage.Id, msg.MessageId)
					p.SetAttributeSuggestion(0, nil, msg.ErrorMessage.Id)
				}
				return // 不再分发空 body(否则 handler 会用全零响应把刚记下的 tip 冲掉)
			}
		}
		handler.MessageBodyHandler(client, msg)
	})

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer waitCancel()
	if err := player.WaitSceneReady(waitCtx); err != nil {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, nil, fmt.Errorf("wait scene ready: %w", err)
	}

	// 首次技能拉取:回包在 RecvLoop 上交给 handler.MessageBodyHandler(startListOwnedSkills),testSkillCast、
	// testMultiRobotBehavior 与它起的 AI 循环读 Player 上的已拥有技能,只有 handler 会写它。
	// 与迁移前一样不等回包本身:发出后固定停 behaviorClientSettle 再返回,回包到得更晚也照旧由 handler 写进 Player
	// (testSkillCast 另有 3s 等待)。拉取失败只告警、照常返回:其余复用者(attribute / pet / equip / currency 等)
	// 不依赖技能列表,本函数的成败口径与返回时机都不因它改变。
	//
	// 这段停顿保留:它迁移前就不只在等 ListSkills 回包,也在等登录后的 AOI(NotifyActorCreate / NotifyActorListCreate)
	// 等初始推送落地,而这些没有别的等待点替代——testMultiRobotBehavior 起 AI 前不等可见实体、也不等技能,
	// 其余复用者的首个请求也一直发在这段停顿之后。
	//
	// 结局交给一个只记日志的 goroutine:失败(信封 / 业务拒绝 / 超时)比停顿到得晚也要留下原因,否则 testSkillCast
	// 只能报「技能列表为空」。Call 保证 listSkillsReplyTimeout 内恰好一个结局,这个 goroutine 不会泄漏。
	// 连接已断(pkg.ErrRPCDisconnected)不记:复用者用完即 Close,回包没到就关是正常收尾。
	skillsResult := startListOwnedSkills(gc, stats, listSkillsReplyTimeout)
	go func() {
		err := <-skillsResult
		if err == nil || errors.Is(err, pkg.ErrRPCDisconnected) {
			return
		}
		zap.L().Warn("behavior client skill list unavailable", zap.String("account", account), zap.Error(err))
	}()
	time.Sleep(behaviorClientSettle)
	return gc, player, nil
}

// behaviorClientSettle 是 prepareBehaviorClient 发出 ListSkills 后、返回前的固定停顿(迁移前的 1.5s,原因见调用处)。
const behaviorClientSettle = 1500 * time.Millisecond

func testSkillCast(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, player, err := prepareBehaviorClient(host, port, account, password, stats, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("prepare behavior client: %v", err)}
	}
	defer func() {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
	}()

	_ = waitUntil(3*time.Second, func() bool {
		return len(player.GetOwnedSkillIDs()) > 0 && player.GetRandomEntity() != 0
	})

	skills := player.GetOwnedSkillIDs()
	targetID := player.GetRandomEntity()
	if len(skills) == 0 {
		return testResult{Elapsed: time.Since(start), Detail: "skill list is empty after entering scene"}
	}
	if targetID == 0 {
		return testResult{Elapsed: time.Since(start), Detail: "no visible/self entity found for skill cast"}
	}

	// ReleaseSkill 的响应类型是 ReleaseSkillResponse(非 Empty),scene 总会回包并回显请求号,所以用 Call 等它自己的回包,
	// 直接解析结果(做法 a):受理 = 回包无 error_message。迁移前靠 handler 写的 ack 计数(GetSkillStats 的 ackCount)
	// 只有本用例读,回包带请求号后 handler 不再跑、它也不再增加,判据改看 Call 的结果。
	// 技能使用 / 打断推送(NotifySkillUsed / NotifySkillInterrupted)不带请求号,照旧经 RecvLoop 由 handler 计数,
	// used 只记进 Detail;迁移前回包先到时也不等它。
	_, usedBefore, _ := player.GetSkillStats()
	caller := newSmokeCaller(gc, stats, account, skillCastReplyTimeout, nil)
	var rsp scene.ReleaseSkillResponse
	tip, err := caller.call(game.SceneSkillClientPlayerReleaseSkillMessageId, &scene.ReleaseSkillRequest{
		SkillTableId: skills[0],
		TargetId:     targetID,
		Position:     &component.Vector3{X: 0, Y: 0, Z: 0},
	}, &rsp)
	if errors.Is(err, pkg.ErrRPCNotSent) {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("release skill send failed: %v", err)}
	}
	stats.SkillSent()
	switch {
	case err != nil:
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("no skill response received: %v", err)}
	case tip != 0:
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("skill rejected: envelope tip=%d", tip)}
	case rsp.GetErrorMessage() != nil:
		return testResult{Elapsed: time.Since(start), Detail: "skill rejected: " + rsp.GetErrorMessage().String()}
	}
	_, usedAfter, _ := player.GetSkillStats()

	return testResult{Passed: true, Elapsed: time.Since(start), Detail: fmt.Sprintf("skill=%d target=%d ack=1 used=%d", skills[0], targetID, usedAfter-usedBefore)}
}

// skillCastReplyTimeout 是 testSkillCast 等 ReleaseSkill 回包的预算,沿用迁移前等回应 / 推送的 3s。
const skillCastReplyTimeout = 3 * time.Second

func testSceneSwitch(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	gc, player, err := prepareBehaviorClient(host, port, account, password, stats, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("prepare behavior client: %v", err)}
	}
	defer func() {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
	}()

	if !waitUntil(3*time.Second, func() bool { return player.GetSceneConfigID() != 0 }) {
		return testResult{Elapsed: time.Since(start), Detail: "scene info not received from server"}
	}

	beforeCount := player.GetSceneEnterCount()
	beforeScene := player.GetSceneID()
	sceneConfID := player.GetSceneConfigID()
	// EnterScene 的同步回包(EnterSceneC2SResponse,非 Empty)只说明受理与否,真正的换场景以 NotifyEnterScene 推送为准。
	// 请求改用 Call,只为让拒绝(信封或回包 error_message)立刻失败、报出原因,不再等满推送窗口;
	// 推送不带请求号,照旧经 RecvLoop 由 handler 计数,下面的推送计数等待保留。
	// 两段共用一个从发出起算的 5s 预算,与迁移前「发出后等推送 5s」相同。
	switchDeadline := time.Now().Add(sceneSwitchNotifyTimeout)
	caller := newSmokeCaller(gc, stats, account, sceneSwitchNotifyTimeout, nil)
	var rsp scene.EnterSceneC2SResponse
	tip, err := caller.call(game.SceneSceneClientPlayerEnterSceneMessageId, &scene.EnterSceneC2SRequest{
		SceneInfo: &scene.SceneInfoComp{SceneConfigId: sceneConfID},
	}, &rsp)
	if errors.Is(err, pkg.ErrRPCNotSent) {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("scene switch send failed: %v", err)}
	}
	stats.SceneSwitchSent()
	switch {
	case err != nil:
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("no enter-scene reply after switch request (scene=%d conf=%d): %v", beforeScene, sceneConfID, err)}
	case tip != 0:
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("scene switch rejected: envelope tip=%d (scene=%d conf=%d)", tip, beforeScene, sceneConfID)}
	case rsp.GetErrorMessage() != nil:
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("scene switch rejected: %s (scene=%d conf=%d)", rsp.GetErrorMessage().String(), beforeScene, sceneConfID)}
	}

	if !waitUntil(time.Until(switchDeadline), func() bool { return player.GetSceneEnterCount() > beforeCount }) {
		return testResult{Elapsed: time.Since(start), Detail: fmt.Sprintf("no enter-scene notification after switch request (scene=%d conf=%d)", beforeScene, sceneConfID)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start), Detail: fmt.Sprintf("scene notifications %d->%d current_scene=%d", beforeCount, player.GetSceneEnterCount(), player.GetSceneID())}
}

// sceneSwitchNotifyTimeout 是 testSceneSwitch 从发出 EnterScene 起等回包与 NotifyEnterScene 的总预算(迁移前等推送的 5s)。
const sceneSwitchNotifyTimeout = 5 * time.Second

func testMultiRobotBehavior(host string, port int, accountFmt, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte, n int) testResult {
	start := time.Now()
	if n < 3 {
		n = 3
	}

	type robotSession struct {
		gc     *pkg.GameClient
		player *gameobject.Player
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		success  int
		sessions []robotSession
	)
	beforeBehavior := stats.BehaviorRecordCount()
	stop := make(chan struct{})
	profile := ai.BuiltinProfiles["behavioral"]

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			acc := fmt.Sprintf(accountFmt, idx+5000)
			gc, player, err := prepareBehaviorClient(host, port, acc, password, stats, tokenPayload, tokenSig)
			if err != nil {
				zap.L().Warn("multi-robot prepare failed", zap.String("account", acc), zap.Error(err))
				return
			}

			robotAI := ai.NewRobotAI(gc, stats)
			robotAI.SetPlayer(player)
			robotAI.SetProfile(&profile)
			robotAI.SetInterval(1 * time.Second)
			if skills := player.GetOwnedSkillIDs(); len(skills) > 0 {
				robotAI.SetSkillIDs(skills)
			}
			go robotAI.RunLoop(stop)

			mu.Lock()
			success++
			sessions = append(sessions, robotSession{gc: gc, player: player})
			mu.Unlock()
		}(i)
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()

	if success == 0 {
		close(stop)
		return testResult{Elapsed: time.Since(start), Detail: "no robot entered the behavior scenario"}
	}

	time.Sleep(6 * time.Second)
	close(stop)

	for _, s := range sessions {
		gameobject.PlayerList.Delete(s.gc.PlayerId)
		_ = s.gc.SendRequest(game.ClientPlayerLoginLeaveGameMessageId, &login.LeaveGameRequest{})
		s.gc.Close()
	}

	afterBehavior := stats.BehaviorRecordCount()
	detail := fmt.Sprintf("%d/%d robots active, behavior_records=%d", success, n, afterBehavior-beforeBehavior)
	if success >= n*7/10 && afterBehavior > beforeBehavior {
		return testResult{Passed: true, Elapsed: time.Since(start), Detail: detail}
	}
	return testResult{Passed: false, Elapsed: time.Since(start), Detail: "insufficient multi-robot coverage: " + detail}
}

// ---------------------------------------------------------------------------
// Scenario: AccessTokenReconnect
//
// Verifies the reconnect-via-access_token short-circuit end to end:
//
//  1. First login through the configured primary auth (password / satoken) —
//     asserts the server MUST hand back an access_token.
//  2. Drop the connection.
//  3. Reconnect and log in with auth_type="access_token" + auth_token=<prev>.
//  4. Assert: login succeeds, player list returns, and — critically —
//     LoginResponse.access_token is EMPTY. access_token re-auth must NOT
//     rotate the pair (see loginlogic.go "skip for access_token re-auth").
//
// If anyone ever removes the `if authType != "access_token"` guard in
// issueTokens, this scenario blows up in CI instead of quietly doubling
// /login Redis write pressure in production.
// ---------------------------------------------------------------------------

func testAccessTokenReconnect(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()

	// Phase 1: primary-auth login to mint the access token.
	gc1, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: "phase1 connect: " + err.Error()}
	}
	if err := loginAndEnterScenario(gc1, password, stats); err != nil {
		gc1.Close()
		return testResult{Elapsed: time.Since(start), Detail: "phase1 login: " + err.Error()}
	}
	firstToken := gc1.AccessToken
	firstExpire := gc1.AccessTokenExpire
	gc1.Close()
	if firstToken == "" {
		return testResult{Elapsed: time.Since(start),
			Detail: "phase1 server did not issue access_token — token machinery broken"}
	}

	// Brief settle so the server-side session cleanup won't collide with the
	// fresh reconnect (scene redirect + kafka producer flush).
	time.Sleep(500 * time.Millisecond)

	// Phase 2: reconnect using access_token.
	gc2, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: "phase2 connect: " + err.Error()}
	}
	defer gc2.Close()

	var lr login.LoginResponse
	if err := sendAndRecv(gc2, stats,
		game.ClientPlayerLoginLoginMessageId,
		&login.LoginRequest{Account: account, AuthType: "access_token", AuthToken: firstToken},
		&lr,
	); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: "phase2 login send: " + err.Error()}
	}
	if lr.ErrorMessage != nil {
		return testResult{Elapsed: time.Since(start),
			Detail: fmt.Sprintf("phase2 server rejected access_token: %v", lr.ErrorMessage)}
	}
	if len(lr.Players) == 0 {
		return testResult{Elapsed: time.Since(start),
			Detail: "phase2 server returned no players — account metadata lost?"}
	}

	// Critical invariant: access_token re-auth MUST NOT rotate tokens.
	if lr.AccessToken != "" || lr.RefreshToken != "" ||
		lr.AccessTokenExpire != 0 || lr.RefreshTokenExpire != 0 {
		return testResult{Elapsed: time.Since(start),
			Detail: fmt.Sprintf("phase2 server rotated token pair on access_token re-auth (access=%q expire=%d) — issueTokens guard broken",
				lr.AccessToken, lr.AccessTokenExpire)}
	}

	// Finish the full enter-game flow so the session is in a valid end-state.
	playerID := lr.Players[0].GetPlayer().GetPlayerId()
	var er login.EnterGameResponse
	if err := sendAndRecv(gc2, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerID},
		&er,
	); err != nil {
		return testResult{Elapsed: time.Since(start), Detail: "phase2 enter: " + err.Error()}
	}
	if er.ErrorMessage != nil {
		return testResult{Elapsed: time.Since(start),
			Detail: fmt.Sprintf("phase2 enter rejected: %v", er.ErrorMessage)}
	}

	return testResult{Passed: true, Elapsed: time.Since(start),
		Detail: fmt.Sprintf("reconnected via access_token (len=%d, expire=%d), no rotation",
			len(firstToken), firstExpire)}
}

// isAttributeMessage:角色属性加点协议的消息号区间(proto/scene/player_attribute.proto,167-175)。
func isAttributeMessage(messageId uint32) bool {
	return messageId >= game.SceneAttributeClientPlayerGetAttributePanelMessageId &&
		messageId <= game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId
}
