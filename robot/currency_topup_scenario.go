package main

// 本地开发运维：逐一给明确指定的已有角色补银两。无建角、发物、升级或自动重试。
import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"proto/login"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"
)

func validateCurrencyTopup(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("configuration is required")
	}
	u, err := url.Parse(cfg.GatewayAddr)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || cfg.ZoneID == 0 {
		return fmt.Errorf("currency topup requires a local HTTP gateway and explicit zone")
	}
	if cfg.AuthType != "" && cfg.AuthType != "password" || cfg.UseHttpLogin || cfg.LLM.Enabled || cfg.RobotCount != 1 {
		return fmt.Errorf("currency topup requires one password-authenticated client without HTTP login or AI")
	}
	opts := cfg.CurrencyTopup
	if opts.Amount <= 0 || strings.TrimSpace(opts.ReceiptPath) == "" || len(opts.Targets) == 0 {
		return fmt.Errorf("positive amount, exclusive receipt path and explicit targets are required")
	}
	seen := make(map[uint64]bool)
	for _, target := range opts.Targets {
		if target.PlayerID == 0 || target.Account != strings.TrimSpace(target.Account) ||
			(!strings.HasPrefix(target.Account, "robot_") && !strings.HasPrefix(target.Account, "dev_")) ||
			len(target.Account) <= 4 || seen[target.PlayerID] {
			return fmt.Errorf("every target must select a unique existing player id and explicit development account")
		}
		seen[target.PlayerID] = true
	}
	return nil
}

type currencyTopupReceipt struct {
	Stage    string `json:"stage"`
	TimeUTC  string `json:"time_utc"`
	Account  string `json:"account,omitempty"`
	PlayerID uint64 `json:"player_id,string,omitempty"`
	Amount   int64  `json:"amount"`
	Before   uint64 `json:"before,string"`
	After    uint64 `json:"after,string"`
	Targets  int    `json:"targets,omitempty"`
}

func openCurrencyTopupReceipt(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot create exclusive topup receipt; do not retry a previous batch: %w", err)
	}
	return file, nil
}

func appendCurrencyTopupReceipt(file *os.File, receipt currencyTopupReceipt) error {
	receipt.TimeUTC = time.Now().UTC().Format(time.RFC3339Nano)
	if err := json.NewEncoder(file).Encode(receipt); err != nil {
		return err
	}
	return file.Sync()
}

func RunCurrencyTopup(cfg *config.Config, stats *metrics.Stats) error {
	if err := validateCurrencyTopup(cfg); err != nil {
		return err
	}
	local := *cfg
	if password := os.Getenv("LOGIN_DEV_PASSWORD_SHARED_SECRET"); password != "" {
		local.Password = password
	}
	cfg = &local
	loginTestCfg = cfg
	if stats == nil {
		stats = metrics.NewStats()
	}
	receipt, err := openCurrencyTopupReceipt(cfg.CurrencyTopup.ReceiptPath)
	if err != nil {
		return err
	}
	defer receipt.Close()
	if err := appendCurrencyTopupReceipt(receipt, currencyTopupReceipt{Stage: "batch_started", Amount: cfg.CurrencyTopup.Amount, Targets: len(cfg.CurrencyTopup.Targets)}); err != nil {
		return err
	}
	for _, target := range cfg.CurrencyTopup.Targets {
		if err := currencyTopupOne(cfg, stats, receipt, target); err != nil {
			return fmt.Errorf("account=%s player=%d stopped without retry; inspect receipt before any further grant: %w", target.Account, target.PlayerID, err)
		}
	}
	if err := appendCurrencyTopupReceipt(receipt, currencyTopupReceipt{Stage: "batch_complete", Amount: cfg.CurrencyTopup.Amount, Targets: len(cfg.CurrencyTopup.Targets)}); err != nil {
		return err
	}
	fmt.Printf("CURRENCY_TOPUP_OK targets=%d amount_each=%d\n", len(cfg.CurrencyTopup.Targets), cfg.CurrencyTopup.Amount)
	return nil
}

func currencyTopupOne(cfg *config.Config, stats *metrics.Stats, receipt *os.File, target config.CurrencyTopupTarget) error {
	local := *cfg
	local.FeaturesSmoke = config.FeaturesSmokeConfig{Account: target.Account, PlayerID: target.PlayerID}
	session, err := openFeatureSmokeSession(&local, stats)
	if err != nil {
		return fmt.Errorf("existing-role login: %w", err)
	}
	defer func() {
		if session != nil {
			session.close()
		}
	}()
	if session.gc.PlayerId != target.PlayerID {
		return fmt.Errorf("selected role mismatch")
	}
	before, err := currencyTopupRead(session)
	if err != nil {
		return err
	}
	gold := currencyTopupSlot(before, 0)
	if uint64(cfg.CurrencyTopup.Amount) > math.MaxUint64-gold {
		return fmt.Errorf("gold overflow")
	}
	want := gold + uint64(cfg.CurrencyTopup.Amount)
	record := currencyTopupReceipt{Stage: "grant_intent", Account: target.Account, PlayerID: target.PlayerID, Amount: cfg.CurrencyTopup.Amount, Before: gold, After: want}
	// 意图必须先持久化。即使进程在收回包前崩溃，也禁止把同批次盲目重发。
	if err := appendCurrencyTopupReceipt(receipt, record); err != nil {
		return err
	}
	got, err := gmAddGold(session.gc, session.player, stats, cfg.CurrencyTopup.Amount, featureSmokeTimeout)
	if err != nil {
		return fmt.Errorf("grant outcome requires inspection: %w", err)
	}
	if got != want {
		return fmt.Errorf("grant balance mismatch got=%d want=%d", got, want)
	}
	record.Stage = "grant_acknowledged"
	if err := appendCurrencyTopupReceipt(receipt, record); err != nil {
		return err
	}
	after, err := currencyTopupRead(session)
	if err != nil {
		return err
	}
	if err := currencyTopupVerify(before, after, want); err != nil {
		return err
	}
	if err := currencyTopupLeave(session); err != nil {
		return err
	}
	session = nil
	// 等正式离场链释放存盘写权，重登只能选同一个已有角色。
	time.Sleep(2 * time.Second)
	session, err = openFeatureSmokeSession(&local, stats)
	if err != nil {
		return fmt.Errorf("relogin after grant: %w", err)
	}
	reloaded, err := currencyTopupRead(session)
	if err != nil {
		return err
	}
	if err := currencyTopupVerify(before, reloaded, want); err != nil {
		return fmt.Errorf("relogin verification: %w", err)
	}
	if err := currencyTopupLeave(session); err != nil {
		return err
	}
	session = nil
	record.Stage = "relogin_verified"
	if err := appendCurrencyTopupReceipt(receipt, record); err != nil {
		return err
	}
	fmt.Printf("CURRENCY_TOPUP_ROLE_OK account=%s player=%d gold=%d->%d relogin_verified=true\n", target.Account, target.PlayerID, gold, want)
	return nil
}

func currencyTopupRead(session *featureSmokeSession) ([]uint64, error) {
	response := &scene.GetCurrencyListResponse{}
	caller := newSmokeCaller(session.gc, session.stats, session.gc.Account, featureSmokeTimeout, nil)
	tip, err := caller.call(game.SceneCurrencyClientPlayerGetCurrencyListMessageId, &scene.GetCurrencyListRequest{}, response)
	if err != nil {
		return nil, err
	}
	if err := currencyReplyRejection("GetCurrencyList", tip, response.GetErrorMessage().GetId(), currencyExitingHint); err != nil {
		return nil, err
	}
	return response.GetCurrency().GetValues(), nil
}

func currencyTopupSlot(values []uint64, index int) uint64 {
	if index >= len(values) {
		return 0
	}
	return values[index]
}

func currencyTopupVerify(before, after []uint64, want uint64) error {
	if currencyTopupSlot(after, 0) != want {
		return fmt.Errorf("silver readback mismatch")
	}
	for index := 1; index < len(before) || index < len(after); index++ {
		if currencyTopupSlot(before, index) != currencyTopupSlot(after, index) {
			return fmt.Errorf("unrelated currency changed at slot %d", index)
		}
	}
	return nil
}

func currencyTopupLeave(session *featureSmokeSession) error {
	caller := newSmokeCaller(session.gc, session.stats, session.gc.Account, featureSmokeTimeout, nil)
	tip, err := caller.call(game.ClientPlayerLoginLeaveGameMessageId, &login.LeaveGameRequest{}, &login.LoginEmptyResponse{})
	gameobject.PlayerList.Delete(session.gc.PlayerId)
	pkg.Clients.Unregister(session.gc.PlayerId, session.gc)
	session.gc.Close()
	if err != nil {
		return fmt.Errorf("formal leave response: %w", err)
	}
	if tip != 0 {
		return fmt.Errorf("formal leave rejected tip=%d", tip)
	}
	return nil
}
