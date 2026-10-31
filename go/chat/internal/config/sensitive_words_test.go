package config

import (
	"testing"

	"chat/internal/textfilter"
	"shared/playerredis"

	"github.com/stretchr/testify/require"
)

func TestValidateSensitiveWordsBeforeServiceStartup(t *testing.T) {
	cfg := Config{
		ZoneId: 1, LeaseTTL: 60, MaxContentBytes: 512, RateLimitPerSecond: 5,
		HistoryMaxEntries: 200, HistoryTTLSeconds: 604800, HistoryDefaultLimit: 20,
		HistoryMaxLimit: 50, RequestIdTTLSeconds: 60,
		DSNEnv: "CHAT_AUTHORITY_DSN", Backend: "tidb", MaxOpenConns: 16, AccountRoleSchema: "mmorpg_global", GlobalLoginSessions: true, LoginRedis: playerredis.LoginReaderConfig{Addrs: []string{"127.0.0.1:6379"}, DB: 0, PoolSizePerNode: 4},
	}
	cfg.ListenOn = "127.0.0.1:50700"
	cfg.Timeout = 3500
	cfg.Etcd.Hosts = []string{"127.0.0.1:2379"}
	cfg.Redis.Host = "127.0.0.1:6379"
	cfg.SensitiveWords = []string{"广告", "Spam", ""}
	require.NoError(t, cfg.Validate())
	cfg.HistoryMaxLimit = 200
	cfg.HistoryMaxEntries = 1000
	require.NoError(t, cfg.Validate(), "可见存储容量独立配置，单次200条合法")
	cfg.HistoryMaxLimit = 201
	require.ErrorContains(t, cfg.Validate(), "HistoryMaxLimit")
	cfg.HistoryMaxLimit = 200
	cfg.SensitiveWords = make([]string, textfilter.MaxWords+1)
	require.ErrorContains(t, cfg.Validate(), "SensitiveWords")
}
