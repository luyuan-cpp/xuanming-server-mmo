package authority

import (
	"bytes"
	"testing"

	"chat/internal/constants"

	"github.com/stretchr/testify/require"
)

func TestPolicyPinsEveryEffectiveRuleAndWordBoundary(t *testing.T) {
	original := fixtureRules()
	before := policyBytes(original)
	for _, change := range []func(*Rules){func(r *Rules) { r.MaxContentBytes++ }, func(r *Rules) { r.RateLimitPerSecond++ }, func(r *Rules) { r.HistoryMaxEntries++ }, func(r *Rules) { r.HistoryTTLSeconds++ }, func(r *Rules) { r.HistoryDefaultLimit++ }, func(r *Rules) { r.HistoryMaxLimit++ }, func(r *Rules) { r.SensitiveWords = []string{"广", "告"} }} {
		changed := original
		change(&changed)
		require.False(t, bytes.Equal(before, policyBytes(changed)))
	}
	left, right := original, original
	left.SensitiveWords = []string{"a", "bc"}
	right.SensitiveWords = []string{"ab", "c"}
	require.NotEqual(t, policyBytes(left), policyBytes(right))
	require.Equal(t, before, policyBytes(original), "编码不得改变调用者原规则")
}

// 只影响本进程读库频率、或运营会调的值不进策略行:滚动改它们不该被当成规则漂移而拒启。
// 两个存储布局常量(世界频道分段数、原件保留天数)钉在 v2 的策略字节里,改它们的构建对着旧库起不来。
func TestPolicyLeavesOperationalKnobsOutAndPinsStorageLayout(t *testing.T) {
	original := fixtureRules()
	before := policyBytes(original)
	for name, change := range map[string]func(*Rules){
		"身份校验缓存":   func(r *Rules) { r.ActorVerifyTTLSeconds = 30 },
		"世界频道快照时长": func(r *Rules) { r.WorldHistoryCacheMs = 500 },
		"世界频道发言间隔": func(r *Rules) { r.WorldChannelCooldownMs = 5000 },
	} {
		changed := original
		change(&changed)
		require.Equal(t, before, policyBytes(changed), name)
	}
	require.True(t, bytes.HasPrefix(before, []byte("chat-authority-v2")))
	// 两个布局常量排在六条规则之后、敏感词之前,各占 8 字节大端。
	layout := before[len("chat-authority-v2")+6*8:]
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0, constants.WorldStreamShards, 0, 0, 0, 0, 0, 0, 0, constants.OriginalRetentionDays}, layout[:16])
}
