//go:build integration

package routing

// 集成测试:路由器接真 RouteStore(storetest 一次性库),走一遍建角登记与归属读。
//
// 跑法:  DATA_SERVICE_IT_MYSQL_USER=root DATA_SERVICE_IT_MYSQL_PASSWORD=<deploy/docker-compose.yml> \
//          go test -tags=integration ./internal/routing/...
//
// RegisterHome 各分支与并发的判定由 store 包的真库用例钉住,这里只钉路由器这一层接上真库之后的结论。

import (
	"context"
	"testing"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"shared/placement"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouter_RealStoreRegisterThenRead(t *testing.T) {
	ctx := context.Background()
	db := storetest.NewMigratedDB(t)
	rs, err := store.NewRouteStore(db.Cfg, store.RouteStoreOptions{
		Writer: store.NewRouteWriterID("routing_it", "test"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rs.Close() })
	r := NewRouter(rs)

	// 新玩家:提交即完成,归属与落点在同一行。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90501, 61, placement.DefaultGlobalStorageID))
	rows, err := rs.GetRoutes(ctx, []uint64{90501})
	require.NoError(t, err)
	require.Contains(t, rows, uint64(90501))
	assert.Equal(t, uint32(61), rows[90501].HomeZone)
	assert.Equal(t, placement.DefaultGlobalStorageID, rows[90501].StorageID)
	assert.Equal(t, uint64(1), rows[90501].Rev)

	// 同 home 重试:幂等,rev 不变。
	require.NoError(t, r.RegisterPlayerZone(ctx, 90501, 61, placement.DefaultGlobalStorageID))
	rows, err = rs.GetRoutes(ctx, []uint64{90501})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), rows[90501].Rev)

	// 异 home:拒绝覆盖,带出既有值。
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, r.RegisterPlayerZone(ctx, 90501, 62, placement.DefaultGlobalStorageID), &conflict)
	assert.Equal(t, uint32(61), conflict.Existing)

	// 读:有行回 home,没有行是权威缺席。
	zone, err := r.GetPlayerHomeZone(ctx, 90501)
	require.NoError(t, err)
	assert.Equal(t, uint32(61), zone)
	_, err = r.GetPlayerHomeZone(ctx, 90502)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped)

	batch, err := r.BatchGetPlayerHomeZone(ctx, []uint64{90501, 90502})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{90501: 61}, batch)
}
