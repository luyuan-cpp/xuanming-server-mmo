package config

import (
	"strings"
	"testing"
)

// prodConfig 是一份生产模式下除合规闸外都合法的配置,开着 App Store。
func prodConfig(gate string) Config {
	var c Config
	c.Name, c.ListenOn, c.Mode, c.Timeout = "payment.rpc", "127.0.0.1:51200", "pro", 3500
	c.Etcd.Hosts = []string{"127.0.0.1:2379"}
	c.ZoneId, c.DSNEnv, c.MaxOpenConns, c.Backend = 1, "MMORPG_PAYMENT_SQL_DSN", 16, "tidb"
	c.Payment.NotifyListenAddr = "127.0.0.1:18130"
	c.Payment.Mall.BaseURL = "https://mall.example.com"
	c.Payment.Compliance.MinorSpendingGate = gate
	c.Payment.Apple.Enabled = true
	return c
}

// 生产模式的 App 渠道只能在合规闸 realname(大陆)或 overseas(大陆以外)下开,realname 必须配 data_service;未知取值拒启。
func TestValidateMinorSpendingGate(t *testing.T) {
	withDirectory := prodConfig(MinorSpendingGateRealName)
	withDirectory.DataServiceRpc.Etcd.Key = "dataservice.rpc"
	miniGameOnly := prodConfig(MinorSpendingGateNone)
	miniGameOnly.Payment.Apple.Enabled = false
	miniGameOnly.Payment.WechatMinigame.Enabled = true
	devApple := prodConfig(MinorSpendingGateNone)
	devApple.Mode = "dev"
	for _, c := range []struct {
		name    string
		config  Config
		wantErr string // 空 = 应当通过
	}{
		{"app channel without the gate", prodConfig(MinorSpendingGateNone), "MinorSpendingGate=none"},
		{"realname without data_service", prodConfig(MinorSpendingGateRealName), "DataServiceRpc"},
		{"realname with data_service", withDirectory, ""},
		{"overseas deployments open app channels without real names", prodConfig(MinorSpendingGateOverseas), ""},
		{"mini-game channels need no gate", miniGameOnly, ""},
		{"dev mode may open app channels", devApple, ""},
		{"unknown gate", prodConfig("strict"), "unknown"},
	} {
		err := c.config.Validate()
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err=%v, want it to mention %q", c.name, err, c.wantErr)
		}
	}
}
