package svc

import (
	"errors"
	"testing"

	"data_service/internal/config"

	"github.com/stretchr/testify/require"
)

// fakePlacements 按库名回落点:bad 里的库解析不出。
type fakePlacements struct{ bad map[string]error }

func (f fakePlacements) ResolvePlacement(logicalDB string) (string, uint64, error) {
	if err := f.bad[logicalDB]; err != nil {
		return "", 0, err
	}
	return "10.0.0.1:4000", 1, nil
}

// 落点解析不出是配置错误,不是集群失联:装配之前就拒启,错误点名是哪个库(docs/design/data-service-startup-isolation.md §5)。
func TestRequirePlacementsRefusesAnUnresolvableDatabase(t *testing.T) {
	schemas := []string{"mmorpg_global_00", "mmorpg_global_01", "mmorpg_global_ledger"}
	require.NoError(t, requirePlacements(fakePlacements{}, schemas))

	unknown := errors.New(`db placement: mmorpg_global_01 is on cluster "east", which SqlClusters does not list`)
	err := requirePlacements(fakePlacements{bad: map[string]error{"mmorpg_global_01": unknown}}, schemas)
	require.ErrorIs(t, err, unknown)
	require.ErrorContains(t, err, "mmorpg_global_01")
}

// 缺目录片的写资格时启动自检过不了,原地重启也永远过不了(写资格在 init 里定了):要重建 Pod,data_service.go 以 78 退出。
// 缺流水库、存档库的写资格不挡启动,不算(docs/design/data-service-startup-isolation.md §7.5)。
func TestMissingDirectoryShardGrants(t *testing.T) {
	var c config.Config
	c.SnapshotMySQL.DBName = "mmorpg_global"
	c.Directory.LogicalShards = 2
	s := &ServiceContext{Config: c, MissingWriterGrants: []string{"mmorpg_global_ledger", "player_store_1000000_db"}}
	require.False(t, s.MissingDirectoryShardGrants())
	s.MissingWriterGrants = append(s.MissingWriterGrants, "mmorpg_global_01")
	require.True(t, s.MissingDirectoryShardGrants())
}
