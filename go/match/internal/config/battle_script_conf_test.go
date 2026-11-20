package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/conf"
)

// 审核流水保留期:仓内 yaml 不写这一段,按 default 180 天;0 / 漏配(直接构造 Config)同样 180 天;非 0 越出 [30, 365] 拒启。
func TestBattleScriptReviewRetention(t *testing.T) {
	var loaded Config
	require.NoError(t, conf.Load(filepath.Join("..", "..", "etc", "match_service.yaml"), &loaded))
	require.EqualValues(t, DefaultBattleScriptReviewRetentionDays, loaded.BattleScript.ReviewRetentionDays)
	require.Equal(t, 180*24*time.Hour, loaded.BattleScript.ReviewRetention())
	require.Equal(t, 180*24*time.Hour, BattleScriptConf{}.ReviewRetention(), "直接构造 Config 的零值按缺省")
	require.Equal(t, 30*24*time.Hour, BattleScriptConf{ReviewRetentionDays: 30}.ReviewRetention())

	for days, ok := range map[uint32]bool{0: true, 29: false, 30: true, 180: true, 365: true, 366: false} {
		c := validForTest()
		c.BattleScript.ReviewRetentionDays = days
		if ok {
			require.NoError(t, c.Validate(), "days=%d", days)
		} else {
			require.Error(t, c.Validate(), "days=%d", days)
		}
	}
}
