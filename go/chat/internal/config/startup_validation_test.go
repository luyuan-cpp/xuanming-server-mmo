package config

import (
	"testing"

	"chat/internal/constants"

	"github.com/stretchr/testify/require"
)

func TestValidateHistoryLimitsAndContentGuardBeforeServiceStartup(t *testing.T) {
	cfg := Config{
		ZoneId: 1, LeaseTTL: 60, MaxContentBytes: 512, RateLimitPerSecond: 5,
		HistoryMaxEntries: 200, HistoryTTLSeconds: 604800, HistoryDefaultLimit: 20,
		HistoryMaxLimit: 50, RequestIdTTLSeconds: 60,
		DSNEnv: "CHAT_AUTHORITY_DSN", Backend: "tidb", MaxOpenConns: 16,
	}
	cfg.DataServiceRpc.Endpoints = []string{"127.0.0.1:9000"}
	cfg.ListenOn = "127.0.0.1:50700"
	cfg.Timeout = 3500
	cfg.Etcd.Hosts = []string{"127.0.0.1:2379"}
	// 缺省严格:没配内容安全的目标就拒启,不能让 staging / prod 带着不送检的聊天起来。
	require.ErrorContains(t, cfg.Validate(), "ContentGuard")
	cfg.ContentGuard.Optional = true
	require.NoError(t, cfg.Validate(), "本机显式写 Optional 才允许不配")
	cfg.HistoryMaxLimit = 200
	cfg.HistoryMaxEntries = 1000
	require.NoError(t, cfg.Validate(), "可见存储容量独立配置，单次200条合法")
	cfg.HistoryMaxLimit = 201
	require.ErrorContains(t, cfg.Validate(), "HistoryMaxLimit")
	cfg.HistoryMaxLimit = 200
	cfg.ContentGuard.Optional = false
	cfg.ContentGuard.Rpc.Endpoints = []string{"127.0.0.1:50250"}
	cfg.ContentGuard.TimeoutMs = 300
	require.NoError(t, cfg.Validate(), "配了目标的严格模式")
}

// 世界频道发言间隔只许在 [0, 上限] 内;历史可见窗口不得长过原件保留期减一天(原件到期由库删除)。
func TestValidateWorldCooldownAndHistoryWindowAgainstRetention(t *testing.T) {
	valid := func() Config {
		cfg := Config{
			ZoneId: 1, LeaseTTL: 60, MaxContentBytes: 512, RateLimitPerSecond: 5,
			HistoryMaxEntries: 200, HistoryTTLSeconds: 604800, HistoryDefaultLimit: 20,
			HistoryMaxLimit: 50, RequestIdTTLSeconds: 60,
			DSNEnv: "CHAT_AUTHORITY_DSN", Backend: "tidb", MaxOpenConns: 16,
		}
		cfg.DataServiceRpc.Endpoints = []string{"127.0.0.1:9000"}
		cfg.ListenOn = "127.0.0.1:50700"
		cfg.Timeout = 3500
		cfg.Etcd.Hosts = []string{"127.0.0.1:2379"}
		cfg.ContentGuard.Optional = true
		return cfg
	}
	cfg := valid()
	require.NoError(t, cfg.Validate(), "间隔 0 = 不限")
	cfg.WorldChannelCooldownMs = constants.MaxWorldChannelCooldownMs
	require.NoError(t, cfg.Validate())
	cfg.WorldChannelCooldownMs = constants.MaxWorldChannelCooldownMs + 1
	require.ErrorContains(t, cfg.Validate(), "WorldChannelCooldownMs")
	cfg.WorldChannelCooldownMs = -1
	require.ErrorContains(t, cfg.Validate(), "WorldChannelCooldownMs")

	cfg = valid()
	cfg.HistoryTTLSeconds = constants.MaxHistoryTTLSeconds
	require.NoError(t, cfg.Validate())
	cfg.HistoryTTLSeconds = constants.MaxHistoryTTLSeconds + 1
	require.ErrorContains(t, cfg.Validate(), "HistoryTTLSeconds")

	cfg = valid()
	cfg.WorldMinLevel = -1
	require.ErrorContains(t, cfg.Validate(), "WorldMinLevel")
	cfg = valid()
	cfg.DuplicateSpamWindowMs = 600001
	require.ErrorContains(t, cfg.Validate(), "DuplicateSpamWindowMs")
	cfg = valid()
	cfg.NewAccountAgeHours = 721
	require.ErrorContains(t, cfg.Validate(), "NewAccountAgeHours")
}
