package main

import (
	"os"
	"path/filepath"
	"testing"

	"proto/common/base"
	"proto/login"
	"robot/config"
)

func currencyTopupTestConfig() *config.Config {
	return &config.Config{GatewayAddr: "http://127.0.0.1:8081", ZoneID: 1, RobotCount: 1, AuthType: "password",
		CurrencyTopup: config.CurrencyTopupConfig{Amount: 100000, ReceiptPath: "unused.jsonl", Targets: []config.CurrencyTopupTarget{{Account: "dev_01", PlayerID: 22}}}}
}

func TestCurrencyTopupRequiresExactExistingRole(t *testing.T) {
	cfg := currencyTopupTestConfig()
	if err := validateCurrencyTopup(cfg); err != nil {
		t.Fatal(err)
	}
	response := &login.LoginResponse{Players: []*login.AccountSimplePlayerWrapper{
		{Player: &base.AccountSimplePlayer{PlayerId: 11}}, {Player: &base.AccountSimplePlayer{PlayerId: 22}},
	}}
	if id, err := featureExistingPlayerID(response, cfg.CurrencyTopup.Targets[0].PlayerID); err != nil || id != 22 {
		t.Fatal("did not select the explicit second role")
	}
	if _, err := featureExistingPlayerID(response, 33); err == nil {
		t.Fatal("unknown role must not fall back to first role")
	}
	cfg.CurrencyTopup.Targets[0].PlayerID = 0
	if err := validateCurrencyTopup(cfg); err == nil {
		t.Fatal("zero role must fail before login")
	}
}

func TestCurrencyTopupRejectsUnsafeBatchBeforeNetwork(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"remote":                  func(c *config.Config) { c.GatewayAddr = "http://example.com:8081" },
		"no zone":                 func(c *config.Config) { c.ZoneID = 0 },
		"non development account": func(c *config.Config) { c.CurrencyTopup.Targets[0].Account = "production" },
		"no receipt":              func(c *config.Config) { c.CurrencyTopup.ReceiptPath = "" },
		"negative":                func(c *config.Config) { c.CurrencyTopup.Amount = -1 },
		"duplicate role": func(c *config.Config) {
			c.CurrencyTopup.Targets = append(c.CurrencyTopup.Targets, c.CurrencyTopup.Targets[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := currencyTopupTestConfig()
			mutate(cfg)
			if err := RunCurrencyTopup(cfg, nil); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

func TestCurrencyTopupReceiptRejectsRepeatedBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.jsonl")
	file, err := openCurrencyTopupReceipt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendCurrencyTopupReceipt(file, currencyTopupReceipt{Stage: "grant_intent", PlayerID: 9223372036854775800, Amount: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if second, err := openCurrencyTopupReceipt(path); err == nil {
		second.Close()
		t.Fatal("repeated batch accepted")
	}
	cfg := currencyTopupTestConfig()
	cfg.CurrencyTopup.ReceiptPath = path
	if err := RunCurrencyTopup(cfg, nil); err == nil {
		t.Fatal("existing receipt must reject before network")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatal("intent was not retained")
	}
}

func TestCurrencyTopupVerifiesOnlySilverChanged(t *testing.T) {
	if err := currencyTopupVerify([]uint64{50, 9}, []uint64{100050, 9, 0}, 100050); err != nil {
		t.Fatal(err)
	}
	if err := currencyTopupVerify([]uint64{50, 9}, []uint64{100050, 10}, 100050); err == nil {
		t.Fatal("unrelated currency mutation accepted")
	}
	if err := currencyTopupVerify([]uint64{50}, []uint64{200050}, 100050); err == nil {
		t.Fatal("duplicate grant accepted")
	}
}
