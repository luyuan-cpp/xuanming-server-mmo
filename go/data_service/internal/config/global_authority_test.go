package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func globalAuthorityFixture() Config {
	c := Config{}
	c.Store.Required = true
	c.SnapshotMySQL.Backend = "tidb"
	c.RouteStore = RouteStoreConfig{Mode: RouteStoreModeDBFirst, ReadRequiresReady: true, Reconcile: RouteReconcileConfig{ArmReadyMarker: true}}
	c.AccountDirectory.RoleTradeSQLProof.Enabled = true
	return c
}

func TestGlobalAuthorityUsesIndependentPlayerDatabaseAllowlist(t *testing.T) {
	c := globalAuthorityFixture()
	out, err := c.ResolveGlobalAuthority(func(name string) string {
		require.Equal(t, RoleProofDatabaseAllowlistEnv, name)
		return "zone_102_db,player_store_1000000_db,zone_101_db"
	})
	require.NoError(t, err)
	require.Equal(t, []uint32{101, 102, 1000000}, out.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs)
	require.Empty(t, c.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs, "不能改调用方配置或用当前zone补全")
	for n, raw := range []string{"", "zone_0_db", "zone_01_db", "other_db", "zone_1_db,zone_1_db", "zone_1_db,", strings.Repeat("zone_1_db,", 1024) + "zone_2_db"} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			_, err := c.ResolveGlobalAuthority(func(string) string { return raw })
			require.Error(t, err)
		})
	}
	c.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs = []uint32{101}
	_, err = c.ResolveGlobalAuthority(func(string) string { return "zone_102_db" })
	require.Error(t, err, "两个来源分叉不得静默择一")
}

func TestGlobalAuthorityCannotSilentlyDisableSQLOrReadyAndLegacyIsExplicit(t *testing.T) {
	mutations := []func(*Config){func(c *Config) { c.Store.Required = false }, func(c *Config) { c.SnapshotMySQL.Backend = "oracle" }, func(c *Config) { c.RouteStore.Mode = RouteStoreModeRedis }, func(c *Config) { c.RouteStore.ReadRequiresReady = false }, func(c *Config) { c.RouteStore.Reconcile.ArmReadyMarker = false }, func(c *Config) { v := false; c.RouteStore.Reconcile.Enabled = &v }, func(c *Config) { c.AccountDirectory.RoleTradeSQLProof.Enabled = false }}
	for n, change := range mutations {
		t.Run(string(rune('a'+n)), func(t *testing.T) {
			c := globalAuthorityFixture()
			change(&c)
			_, err := c.ResolveGlobalAuthority(func(string) string { return "zone_1_db" })
			require.Error(t, err)
		})
	}
	c := Config{}
	c.LegacyNonGlobal = true
	c.Mode = "dev"
	_, err := c.ResolveGlobalAuthority(func(string) string { t.Fatal("隔离工具无需读取全球白名单"); return "" })
	require.NoError(t, err)
	c.Mode = "pro"
	_, err = c.ResolveGlobalAuthority(func(string) string { return "" })
	require.Error(t, err)
	c.Mode = "dev"
	c.SnapshotMySQL.Backend = "oracle"
	_, err = c.ResolveGlobalAuthority(func(string) string { return "" })
	require.Error(t, err, "隔离夹具也不能带非法档位")
}

// 门禁只看档位是否合法:两档都能走完正式全球装配(player-data-tidb-scaling-plan.md §9)。
func TestGlobalAuthorityAcceptsEveryLegalBackend(t *testing.T) {
	for _, backend := range []string{"", "tidb", "mysql"} {
		c := globalAuthorityFixture()
		c.SnapshotMySQL.Backend = backend
		_, err := c.ResolveGlobalAuthority(func(string) string { return "zone_1_db" })
		require.NoError(t, err, "Backend=%q", backend)
	}
}
