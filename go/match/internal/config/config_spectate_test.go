package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/conf"
)

// yamlWithoutSpectate 返回仓内 yaml 去掉顶层 Spectate 段后的内容(部署可能在这个文件里给 dev 值,这里要测的是漏配)。
func yamlWithoutSpectate(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "etc", "match_service.yaml"))
	require.NoError(t, err)
	var kept []string
	inSection := false
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "Spectate:") {
			inSection = true
			continue
		}
		if inSection && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			continue
		}
		inSection = false
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// 整段缺席时按默认值回填(K8s ConfigMap 漏写也能起服),开关是关的,不会把观众放进 L1 / L2。
func TestSpectateDefaultsWhenSectionAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "match_service.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlWithoutSpectate(t)), 0o600))
	var c Config
	require.NoError(t, conf.Load(path, &c))
	require.False(t, c.Spectate.RelayEnabled)
	require.Equal(t, 4, c.Spectate.BroadcastMaxConcurrent)
	require.Equal(t, 32, c.Spectate.BroadcastAutoGateThreshold)
	require.Equal(t, int64(60), c.Spectate.TicketTTLSeconds)
	require.Empty(t, c.Spectate.TicketSecret)
	require.Empty(t, c.Spectate.OpsCallerSecret)
	require.False(t, c.Spectate.RelayUsable())
	require.False(t, c.Spectate.OpsEntryEnabled())
}

// conf.Load 会自动调 Validate:生产模式打开中继而密钥不够长,加载期就拒启。
func TestSpectateSecretEnforcedAtLoadInProd(t *testing.T) {
	src := yamlWithoutSpectate(t)
	load := func(mode, secret string) error {
		text := strings.Replace(src, "Mode: dev", "Mode: "+mode, 1) +
			"\nSpectate:\n  RelayEnabled: true\n  TicketSecret: \"" + secret + "\"\n"
		path := filepath.Join(t.TempDir(), "match_service.yaml")
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
		var c Config
		return conf.Load(path, &c)
	}
	require.Error(t, load("pro", ""), "生产空密钥拒启")
	require.Error(t, load("pro", strings.Repeat("s", 31)), "生产短密钥拒启")
	require.Error(t, load("pro", strings.Repeat(" ", 40)), "纯空白等同没配")
	require.NoError(t, load("pro", strings.Repeat("s", 32)))
	require.NoError(t, load("dev", ""), "dev 放行(启动告警,中继按关闭处理)")
	require.NoError(t, load("test", ""), "test 放行")
}

func TestSpectateValidate(t *testing.T) {
	c := validForTest()
	require.NoError(t, c.Validate(), "开关关闭时不要求密钥")

	for name, mutate := range map[string]func(c *Config){
		"配额为负":         func(c *Config) { c.Spectate.BroadcastMaxConcurrent = -1 },
		"阈值为负":         func(c *Config) { c.Spectate.BroadcastAutoGateThreshold = -1 },
		"运营密钥不足 32 字节": func(c *Config) { c.Spectate.OpsCallerSecret = strings.Repeat("o", 31) },
	} {
		c := validForTest()
		mutate(&c)
		require.Error(t, c.Validate(), name)
	}

	c = validForTest()
	c.Spectate.OpsCallerSecret = "   "
	require.NoError(t, c.Validate(), "纯空白等同没配:运营入口关闭")
	require.False(t, c.Spectate.OpsEntryEnabled())

	c = validForTest()
	c.Spectate.OpsCallerSecret = strings.Repeat("o", 32)
	require.NoError(t, c.Validate())
	require.True(t, c.Spectate.OpsEntryEnabled())
}

func TestSpectateTicketTTLDefault(t *testing.T) {
	require.Equal(t, 60*time.Second, SpectateConf{}.TicketTTL(), "直接构造 Config 的零值按默认 60 秒")
	require.Equal(t, 60*time.Second, SpectateConf{TicketTTLSeconds: -1}.TicketTTL())
	require.Equal(t, 90*time.Second, SpectateConf{TicketTTLSeconds: 90}.TicketTTL())
}

func TestSpectateRelayUsableRequiresSecret(t *testing.T) {
	require.True(t, SpectateConf{RelayEnabled: true, TicketSecret: "s"}.RelayUsable())
	require.False(t, SpectateConf{RelayEnabled: true}.RelayUsable())
	require.False(t, SpectateConf{RelayEnabled: true, TicketSecret: "  "}.RelayUsable())
	require.False(t, SpectateConf{TicketSecret: "s"}.RelayUsable())
}
