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

func TestGlobalAuthorityCannotSilentlyDisableSQLAndLegacyIsExplicit(t *testing.T) {
	mutations := []func(*Config){func(c *Config) { c.Store.Required = false }, func(c *Config) { c.SnapshotMySQL.Backend = "oracle" }, func(c *Config) { c.RouteStore.MergedZoneRefreshSec = 61 }, func(c *Config) { c.AccountDirectory.RoleTradeSQLProof.Enabled = false }}
	for n, change := range mutations {
		t.Run(string(rune('a'+n)), func(t *testing.T) {
			c := globalAuthorityFixture()
			change(&c)
			_, err := c.ResolveGlobalAuthority(func(string) string { return "zone_1_db" })
			require.Error(t, err)
		})
	}
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

func TestGlobalAuthorityValidatesLogicalShards(t *testing.T) {
	for k, ok := range map[int]bool{0: true, 1: true, 2: true, 4: true, 64: true, 3: false, -1: false, 128: false} {
		c := globalAuthorityFixture()
		c.Directory.LogicalShards = k
		_, err := c.ResolveGlobalAuthority(func(string) string { return "zone_1_db" })
		if ok {
			require.NoError(t, err, "K=%d", k)
		} else {
			require.ErrorContains(t, err, "Directory.LogicalShards", "K=%d", k)
		}
	}
}

// 写资格清单:目录各片、流水库、配置的存档库,一个不多一个不少。
func TestWriterSchemasListsEveryLogicalDatabase(t *testing.T) {
	c := globalAuthorityFixture()
	c.SnapshotMySQL.DBName = "mmorpg_global"
	c.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs = []uint32{1, 1000000}
	got, err := c.WriterSchemas()
	require.NoError(t, err)
	require.Equal(t, []string{"mmorpg_global", "mmorpg_global_ledger", "zone_1_db", "player_store_1000000_db"}, got)

	c.Directory.LogicalShards = 2
	got, err = c.WriterSchemas()
	require.NoError(t, err)
	require.Equal(t, []string{"mmorpg_global_00", "mmorpg_global_01", "mmorpg_global_ledger", "zone_1_db", "player_store_1000000_db"}, got)

	c.Directory.LogicalShards = 3
	_, err = c.WriterSchemas()
	require.Error(t, err)
}

func TestDirectoryConfigFromYaml(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	require.Equal(t, 1, c.Directory.Shards(), "没写按 1")
	c = loadYaml(t, minimalYaml+"\nDirectory:\n  LogicalShards: 64\n  StoragePools:\n    MaxOpenConn: 4\n    MaxIdleConn: 1\n    ConnMaxIdleSeconds: 30\n")
	require.Equal(t, 64, c.Directory.Shards())
	require.Equal(t, StoragePoolConfig{MaxOpenConn: 4, MaxIdleConn: 1, ConnMaxIdleSeconds: 30}, c.Directory.StoragePools)
}
