package svc

import (
	"errors"
	"testing"

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
