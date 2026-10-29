package main

// Currency-crash-window verification scenario.
//
// Background: docs/notes/currency-crash-window-verification.md.
// Goal: quantify the data-loss window for in-flight currency mutations
// when the scene process is killed (kill -9) between two periodic
// SavePlayerToRedis ticks.
//
// This file only implements the *robot* side — login → snapshot pre
// balance → GmAddCurrency → re-snapshot to confirm mutation landed in
// scene memory → exit (without LeaveGame so we can be killed by the
// driver script). The "kill scene" + "restart scene" steps belong to
// the orchestration script (tools/scripts/currency_crash_window.ps1
// or similar), not the robot. The robot is run twice per case: once
// before the kill (mode=before), once after the restart (mode=after);
// the script diffs balances across runs.
//
// We avoid embedding kill orchestration here so the robot stays a
// pure client (no special privileges to send Stop-Process), and so
// the scenario can be re-used by the cpp/tests integration variant
// later if we ever consolidate the two test paths.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// CurrencyType.Gold mirrors cpp/libs/modules/currency/constants/currency.h
// kCurrencyGold = 0. Hard-coded here rather than imported from a generated
// enum because the proto side only exposes uint32 (no Go enum is generated
// for the cpp-only header).
const currencyTypeGold uint32 = 0

// readGoldBalance 读玩家的金币余额(GetCurrencyList 回包里 Gold 槽的值)。本场景之外,attribute / pet /
// equip_forge(及复用它的 equip_forge_topup、asset_arrival_prepare)也经它读余额,签名与成功时的返回值保持不变。
//
// 只等本次请求自己的回包(按 MessageContent.id 认领)并直接解析,不再经生成 handler 写 gameobject.Player:
//   - 带请求号的回包不进 RecvLoop 的 onMessage,生成 handler 不跑,Player 上的货币快照与就绪通道不会再被写。
//     全包只有本函数与 gmAddGold 读那份状态,所以直接取 Call 的结果
//     (docs/handoff/client-rpc-callback-robot-handoff-20261019.md §8.1 做法 1)。
//   - 改造前分不清「余额是 0」与「请求被拒」:handler 遇到响应体 error_message 记一份空列表,gate 信封拒绝
//     (限流 / R1 的 1003)时响应体为空、同样解成空列表,读出来都是 (0, nil)。现在信封拒绝、scene 业务拒绝、
//     超时各报各的错(currencyReplyRejection / currencyTransportError)。
//   - 上一次超时请求的迟到回包由 GameClient 计数后丢弃,不会再被当成本次结果。
//
// 调用约束:gc 的 RecvLoop 必须已启动(现有调用方都经 prepareBehaviorClient,它已起),且不得在 RecvLoop goroutine 上调用
// (同步等待回包会自锁)。stats 可为 nil。槽位越界(新号还没有货币记录)按 0,不算失败。
// 第二个参数只为兼容调用方保留,不再读写。
func readGoldBalance(gc *pkg.GameClient, _ *gameobject.Player, stats *metrics.Stats, timeout time.Duration) (uint64, error) {
	const method = "GetCurrencyList"
	response := &scene.GetCurrencyListResponse{}
	caller := newSmokeCaller(gc, stats, gc.Account, timeout, nil)
	envelopeTip, err := caller.call(game.SceneCurrencyClientPlayerGetCurrencyListMessageId, &scene.GetCurrencyListRequest{}, response)
	if err != nil {
		return 0, currencyTransportError(method, err)
	}
	if err := currencyReplyRejection(method, envelopeTip, response.GetErrorMessage().GetId(), currencyExitingHint); err != nil {
		return 0, err
	}
	return goldSlotValue(response), nil
}

// goldSlotValue 取 GetCurrencyList 回包里 Gold 槽的值。槽位越界(新号没有货币记录、repeated 比槽位短)按 0,
// 与改造前经 gameobject.Player.GetCurrencyValue 读出的值一致。
// 槽位规则放在共享 helper 这一侧,场景按需引用它,不让共享 helper 反过来依赖某个场景的私有函数
// (那样场景一改名 / 删函数,attribute / pet / equip_forge / currency 会一起编译失败)。travel 的 readGold 也用它。
func goldSlotValue(response *scene.GetCurrencyListResponse) uint64 {
	values := response.GetCurrency().GetValues()
	if int(currencyTypeGold) >= len(values) {
		return 0
	}
	return values[currencyTypeGold]
}

// gmAddGold 发 GmAddCurrency 给玩家加 amount 金币,等到回包(即 scene 内存里的 CurrencyComp 已改),返回 balance_after。
// 认领方式、调用约束与 readGoldBalance 相同;第二个参数同样只为兼容调用方保留。
//
// 改造前它不看响应体 error_message,被拒时返回 (0, nil),调用方只能靠事后重读余额才发现「没加上」;
// 现在 gate GM 闸(信封 kFeatureUnavailable)、scene GM 闸 / 冻结 / 封禁(响应体 tip)与超时都直接返回 error。
func gmAddGold(gc *pkg.GameClient, _ *gameobject.Player, stats *metrics.Stats, amount int64, timeout time.Duration) (uint64, error) {
	const method = "GmAddCurrency"
	request := &scene.GmAddCurrencyRequest{
		CurrencyType: currencyTypeGold,
		Amount:       amount,
	}
	response := &scene.GmAddCurrencyResponse{}
	caller := newSmokeCaller(gc, stats, gc.Account, timeout, nil)
	envelopeTip, err := caller.call(game.SceneCurrencyClientPlayerGmAddCurrencyMessageId, request, response)
	if err != nil {
		return 0, currencyTransportError(method, err)
	}
	if err := currencyReplyRejection(method, envelopeTip, response.GetErrorMessage().GetId(), currencyGmGuardHint); err != nil {
		return 0, err
	}
	return response.GetBalanceAfter(), nil
}

// currencyTransportError 包装 smokeCaller 的失败(没发出 / 超时 / 断线),沿用改造前的报错前缀:
// 没发出报 "send <method>",其余报 "wait <method> response"。超时的 error 包裹 pkg.ErrRPCTimeout。
func currencyTransportError(method string, err error) error {
	if errors.Is(err, pkg.ErrRPCNotSent) {
		return fmt.Errorf("send %s: %w", method, err)
	}
	return fmt.Errorf("wait %s response: %w", method, err)
}

// scene 在响应体回 kFeatureUnavailable 的来源因方法而异(SceneHandler::ProcessClientPlayerMessage,scene_handler.cpp):
// 退出闸(实体带 UnregisterPlayer,只放行 ExitGame)对所有方法生效;GM 闸只拦 Gm 前缀的方法。
// 原因由调用点按方法选,免得非 GM 方法被拒时把人引去查 SCENE_RUN_MODE。
const (
	currencyExitingHint = " (kFeatureUnavailable: the scene rejected it because the player is exiting (UnregisterPlayer))"
	currencyGmGuardHint = " (kFeatureUnavailable: the scene GM guard rejected it because SCENE_RUN_MODE is not dev, " +
		"or the player is exiting (UnregisterPlayer))"
)

// currencyReplyRejection 判一次货币 RPC 是否被拒:envelopeTip 是 gate 的信封拒绝码(非 0 时响应体未填充),
// bodyTip 是 scene 写在响应体 error_message 里的业务拒绝码,都为 0 返回 nil。
// 两种分开报:信封拒绝说明请求没到 scene 的货币逻辑,业务拒绝说明到了、被 scene 拒绝。
// featureUnavailableHint 是 bodyTip 为 kFeatureUnavailable 时拼在码后面的原因(currencyExitingHint / currencyGmGuardHint)。
func currencyReplyRejection(method string, envelopeTip, bodyTip uint32, featureUnavailableHint string) error {
	if envelopeTip != 0 {
		return fmt.Errorf("%s rejected by gate envelope tip=%d%s", method, envelopeTip, sceneRequestEnvelopeHint(envelopeTip))
	}
	if bodyTip != 0 {
		hint := ""
		if bodyTip == uint32(tiptable.CommonError_kFeatureUnavailable) {
			hint = featureUnavailableHint
		}
		return fmt.Errorf("%s rejected by scene tip=%d%s", method, bodyTip, hint)
	}
	return nil
}

// sceneRequestEnvelopeHint 给 gate 对「发往 scene 的客户端请求」回的信封拒绝码配一句原因(拼在 "tip=<数字>" 后面);
// 不认识的码返回空串,只打印数字。不提「gate 不认识这个消息号」:未知消息号 gate 不回信封,只会表现为超时。
// 这几个码的来源与 gate 一侧的定位线索只该写在这一处,共享 helper 与各场景共用;
// 场景只在需要时覆盖个别码的提示(travelSmokeEnvelopeHint 把限流指到它自己的节流常量)。
func sceneRequestEnvelopeHint(tip uint32) string {
	switch tip {
	case uint32(tiptable.CommonError_kFeatureUnavailable):
		return " (kFeatureUnavailable: the gate GM guard rejected it before it reached the scene; gate GATE_RUN_MODE is not dev)"
	case uint32(tiptable.CommonError_kServiceUnavailable):
		// 两个来源:gate 自己选不到 scene 节点(client_session_handler.cpp 打 [TCP Node] WARN),
		// 或 R1 之后 scene 在派发前早退、由 gate 补回带请求号的 1003。
		return " (kServiceUnavailable: the gate session is not bound to a scene node yet (Scene not ready) or the scene RpcClient " +
			"is not attached, see the gate's [TCP Node] WARN; or the scene dropped the request before dispatch " +
			"(session not in the scene / player still loading / request failed to parse))"
	case uint32(tiptable.CommonError_kRateLimitExceeded):
		return " (kRateLimitExceeded: gate rate limit, the request never reached the scene; space out same-id requests)"
	}
	return ""
}

// currencyCrashWindowSnapshot is the round-trip the scenario performs:
// login → read pre balance → GM-add → read post balance. Returns the
// numbers the driver script needs to log. addAmount=0 disables the
// AddCurrency step (used by the post-kill replay — we just want to
// re-read the balance after the scene restart).
func currencyCrashWindowSnapshot(
	host string, port int, account, password string,
	stats *metrics.Stats, tokenPayload, tokenSig []byte,
	addAmount int64,
) (preBalance, postBalance uint64, err error) {
	gc, player, prepErr := prepareBehaviorClient(host, port, account, password, stats, tokenPayload, tokenSig)
	if prepErr != nil {
		err = fmt.Errorf("prepare client: %w", prepErr)
		return
	}
	defer func() {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
	}()

	pre, readErr := readGoldBalance(gc, player, stats, 5*time.Second)
	if readErr != nil {
		err = fmt.Errorf("read pre balance: %w", readErr)
		return
	}
	preBalance = pre

	if addAmount == 0 {
		postBalance = pre
		return
	}

	post, addErr := gmAddGold(gc, player, stats, addAmount, 5*time.Second)
	if addErr != nil {
		err = fmt.Errorf("gm add: %w", addErr)
		return
	}
	postBalance = post

	zap.L().Info("currency-crash-window snapshot",
		zap.Uint64("player", gc.PlayerId),
		zap.Uint64("pre", pre),
		zap.Int64("delta", addAmount),
		zap.Uint64("post", post))
	return
}

// testCurrencyCrashWindow is the in-process scenario variant. It runs the
// pre-kill leg (snapshot pre, add gold, snapshot post) WITHOUT actually
// killing the scene — used as a smoke test that the wiring works and as
// the case D ("normal logout, no kill") regression check.
//
// The full 4-case matrix (A short window / B cross-period / C accelerated /
// D normal logout) is driven from the orchestration script which calls
// the robot in `currency-crash-snapshot` sub-mode (see currencyCrashSnapshotMode).
func testCurrencyCrashWindow(host string, port int, account, password string, stats *metrics.Stats, tokenPayload, tokenSig []byte) testResult {
	start := time.Now()
	const addAmount int64 = 10000

	pre, post, err := currencyCrashWindowSnapshot(host, port, account, password, stats, tokenPayload, tokenSig, addAmount)
	if err != nil {
		return testResult{Elapsed: time.Since(start), Detail: err.Error()}
	}
	if post != pre+uint64(addAmount) {
		return testResult{
			Elapsed: time.Since(start),
			Detail:  fmt.Sprintf("balance mismatch: pre=%d delta=%d post=%d (expected post=pre+delta)", pre, addAmount, post),
		}
	}
	return testResult{
		Passed:  true,
		Elapsed: time.Since(start),
		Detail:  fmt.Sprintf("pre=%d +%d -> post=%d", pre, addAmount, post),
	}
}

// currencyCrashSnapshot is the JSON shape written by mode=currency-crash-snapshot.
// One file per leg (pre-kill / post-restart); the orchestration script diffs
// the two and tabulates per-case results.
type currencyCrashSnapshot struct {
	Account     string `json:"account"`
	PlayerID    uint64 `json:"player_id"`
	PreBalance  uint64 `json:"pre_balance"`
	PostBalance uint64 `json:"post_balance"`
	AddAmount   int64  `json:"add_amount"` // 0 for read-only legs
	TimestampMs int64  `json:"timestamp_ms"`
	Error       string `json:"error,omitempty"`
}

// runCurrencyCrashSnapshotMode is the main.go entry point for
// `mode: currency-crash-snapshot`. It does ONE snapshot pass (login →
// optional GmAddCurrency → readback) and writes the result to
// CurrencyCrash.OutputPath as JSON. Caller (PowerShell script) is
// responsible for everything else: clearing data, starting/killing scene,
// retrying on transient failures, comparing snapshots across legs.
func runCurrencyCrashSnapshotMode(
	host string, port int, cfg *config.Config, stats *metrics.Stats,
	tokenPayload, tokenSig []byte,
) {
	account := cfg.CurrencyCrash.Account
	if account == "" {
		account = fmt.Sprintf(cfg.AccountFmt, 1)
	}
	outputPath := cfg.CurrencyCrash.OutputPath
	if outputPath == "" {
		outputPath = "logs/currency_crash_window/snapshot.json"
	}

	snap := currencyCrashSnapshot{
		Account:     account,
		AddAmount:   cfg.CurrencyCrash.AddAmount,
		TimestampMs: time.Now().UnixMilli(),
	}

	// Capture the player_id by piggybacking on currencyCrashWindowSnapshot.
	// We open a separate prepareBehaviorClient call instead of refactoring
	// the helper because mode-snapshot wants the player_id explicitly while
	// the in-process variant doesn't care; one extra getter is cheaper than
	// changing a stable function's signature.
	gc, player, prepErr := prepareBehaviorClient(host, port, account, cfg.Password, stats, tokenPayload, tokenSig)
	if prepErr != nil {
		snap.Error = "prepare client: " + prepErr.Error()
		writeCurrencyCrashSnapshot(outputPath, snap)
		zap.L().Error("currency-crash-snapshot prepare failed", zap.Error(prepErr))
		return
	}
	defer func() {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
	}()
	snap.PlayerID = gc.PlayerId

	pre, readErr := readGoldBalance(gc, player, stats, 5*time.Second)
	if readErr != nil {
		snap.Error = "read pre balance: " + readErr.Error()
		writeCurrencyCrashSnapshot(outputPath, snap)
		zap.L().Error("currency-crash-snapshot pre-read failed", zap.Error(readErr))
		return
	}
	snap.PreBalance = pre
	snap.PostBalance = pre

	if cfg.CurrencyCrash.AddAmount != 0 {
		post, addErr := gmAddGold(gc, player, stats, cfg.CurrencyCrash.AddAmount, 5*time.Second)
		if addErr != nil {
			snap.Error = "gm add: " + addErr.Error()
			writeCurrencyCrashSnapshot(outputPath, snap)
			zap.L().Error("currency-crash-snapshot add failed", zap.Error(addErr))
			return
		}
		snap.PostBalance = post
	}

	writeCurrencyCrashSnapshot(outputPath, snap)
	zap.L().Info("currency-crash-snapshot leg complete",
		zap.String("account", account),
		zap.Uint64("player_id", snap.PlayerID),
		zap.Uint64("pre", snap.PreBalance),
		zap.Int64("delta", snap.AddAmount),
		zap.Uint64("post", snap.PostBalance),
		zap.String("output", outputPath))
}

// writeCurrencyCrashSnapshot serializes the snapshot to disk. Errors here
// are logged and ignored — the run already happened, we don't want a stat
// failure to mask the real result.
func writeCurrencyCrashSnapshot(path string, snap currencyCrashSnapshot) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			zap.L().Error("mkdir snapshot dir", zap.String("dir", dir), zap.Error(err))
		}
	}
	body, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		zap.L().Error("marshal snapshot", zap.Error(err))
		return
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		zap.L().Error("write snapshot", zap.String("path", path), zap.Error(err))
	}
}
