package data

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateServerVersion:启动期版本下限自检的纯函数(不连库,任何环境都跑)。
// 边界:8.0.28 拒、8.0.29 过;修订号按数值比(8.0.3 < 8.0.29,按字符串比会判反);发行版后缀忽略;
// TiDB 按字样放行(它的 8.0.11 前缀只是兼容协议号);MariaDB 与解析不出的版本串一律拒绝(fail-closed)。
func TestValidateServerVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		ok      bool
	}{
		{"8.0.28 拒", "8.0.28", false},
		{"8.0.29 过", "8.0.29", true},
		{"8.0.30 过", "8.0.30", true},
		{"修订号按数值比:8.0.3 拒", "8.0.3", false},
		{"修订号按数值比:8.0.100 过", "8.0.100", true},
		{"发行版后缀:8.0.35-0ubuntu 过", "8.0.35-0ubuntu", true},
		{"发行版后缀:8.0.35-0ubuntu0.22.04.1 过", "8.0.35-0ubuntu0.22.04.1", true},
		{"发行版后缀:8.0.28-log 拒", "8.0.28-log", false},
		{"8.4 LTS 过", "8.4.3", true},
		{"9.x 创新版 过", "9.1.0", true},
		{"5.7.44 拒", "5.7.44", false},
		{"前后空白忽略", " 8.0.29\n", true},
		{"TiDB 放行", "8.0.11-TiDB-v7.5.1", true},
		{"MariaDB 拒", "10.11.6-MariaDB", false},
		{"只有两段 拒", "8.0", false},
		{"段为空 拒", "8..29", false},
		{"空串 拒", "", false},
		{"无法解析 拒", "unknown", false},
	}
	for _, tc := range cases {
		err := validateServerVersion(tc.version)
		if tc.ok {
			assert.NoError(t, err, "%s:%q", tc.name, tc.version)
			continue
		}
		if assert.Error(t, err, "%s:%q", tc.name, tc.version) {
			assert.Contains(t, err.Error(), "8.0.29", "%s:拒绝原因要写明版本下限", tc.name)
		}
	}
}

// TestParseMySQLVersion:只取开头的"主.次.修订",其后的第四段与后缀都忽略。
func TestParseMySQLVersion(t *testing.T) {
	got, ok := parseMySQLVersion("8.0.35-0ubuntu0.22.04.1")
	require.True(t, ok)
	assert.Equal(t, [3]int{8, 0, 35}, got)

	got, ok = parseMySQLVersion("8.4.3.7")
	require.True(t, ok)
	assert.Equal(t, [3]int{8, 4, 3}, got)

	_, ok = parseMySQLVersion("v8.0.29")
	assert.False(t, ok, "开头不是数字:解析不出")
}

// TestCheckServerVersion_TestDatabase:真库(GUILD_TEST_MYSQL_DSN 未设即 Skip)。本包的锁序并发回归都以
// "MySQL 8.0.29+ 或 TiDB"为前提,测试库低于下限时这里先红,免得那些用例在错误的前提上绿 / 红。
// 版本串打进测试日志,交付时照抄(验收要求写明跑的是哪个版本)。
func TestCheckServerVersion_TestDatabase(t *testing.T) {
	ctx, _, repo := openGuildIntegrationRepo(t)
	version, err := repo.CheckServerVersion(ctx)
	require.NoError(t, err, "测试库版本 %q 不满足帮会服务的版本下限", version)
	t.Logf("测试库 VERSION() = %s", version)
}
