package logic

import (
	"context"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/routing"
	"data_service/internal/svc"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

func newTestSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)

	c := config.Config{
		MappingRedis: redis.RedisConf{
			Host: mr.Addr(),
			Type: "node",
		},
		DevRedis: config.DevRedisConfig{
			Host: mr.Addr(),
			DB:   0,
		},
		PlayerLockTTLSec: 3,
	}

	r := routing.NewRouter(c)
	t.Cleanup(func() { r.Close() })

	sc := &svc.ServiceContext{
		Config: c,
		Router: r,
	}
	return sc, mr
}

func setupPlayer(t *testing.T, svcCtx *svc.ServiceContext, playerID uint64, zoneID uint32) {
	t.Helper()
	ctx := context.Background()
	err := svcCtx.Router.RegisterPlayerZone(ctx, playerID, zoneID)
	assert.NoError(t, err)
}

// ── LoadPlayerData ─────────────────────────────────────────────

func TestLoadPlayerData_Empty(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 1, 1)

	resp, err := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{PlayerID: 1})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.Empty(t, resp.Data)
}

func TestLoadPlayerData_SpecificFields(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 1, 1)

	// Save some data first
	_, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 1,
		Data: map[string][]byte{
			"bag":   []byte("bag_data"),
			"equip": []byte("equip_data"),
			"quest": []byte("quest_data"),
		},
	})
	assert.NoError(t, err)

	// Load specific fields
	resp, err := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{
		PlayerID: 1,
		Fields:   []string{"bag", "equip"},
	})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.Equal(t, []byte("bag_data"), resp.Data["bag"])
	assert.Equal(t, []byte("equip_data"), resp.Data["equip"])
	assert.Nil(t, resp.Data["quest"]) // not requested
}

func TestLoadPlayerData_AllFields(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 2, 1)

	_, _ = SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 2,
		Data: map[string][]byte{
			"bag":  []byte("b"),
			"attr": []byte("a"),
		},
	})

	resp, err := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{PlayerID: 2})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.Equal(t, []byte("b"), resp.Data["bag"])
	assert.Equal(t, []byte("a"), resp.Data["attr"])
	assert.True(t, resp.Version > 0)
}

// ── SavePlayerData ─────────────────────────────────────────────

func TestSavePlayerData_Basic(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 10, 1)

	resp, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 10,
		Data:     map[string][]byte{"hp": []byte("100")},
	})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.Equal(t, uint64(1), resp.NewVersion)

	// Save again, version increments
	resp2, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 10,
		Data:     map[string][]byte{"hp": []byte("200")},
	})
	assert.NoError(t, err)
	assert.Equal(t, uint64(2), resp2.NewVersion)
}

func TestSavePlayerData_VersionCheck_OK(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 20, 1)

	resp, _ := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 20,
		Data:     map[string][]byte{"x": []byte("1")},
	})
	ver := resp.NewVersion

	resp2, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID:        20,
		Data:            map[string][]byte{"x": []byte("2")},
		ExpectedVersion: ver,
	})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp2.ErrorCode)
	assert.Equal(t, ver+1, resp2.NewVersion)
}

func TestSavePlayerData_VersionCheck_Mismatch(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 30, 1)

	SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 30,
		Data:     map[string][]byte{"x": []byte("1")},
	})

	resp, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID:        30,
		Data:            map[string][]byte{"x": []byte("2")},
		ExpectedVersion: 999, // wrong version
	})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeVersionMismatch, resp.ErrorCode)
}

func TestSaveFieldsAtomic_ExpiredOwnerCannotWrite(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 31, 1)
	client, err := svcCtx.Router.ClientForPlayer(ctx, 31)
	require.NoError(t, err)

	staleToken, locked, err := svcCtx.Router.AcquirePlayerLock(ctx, client, 31)
	require.NoError(t, err)
	require.True(t, locked)

	// 让双锁都过期，再由后来者取得同一玩家的锁。
	mr.FastForward(4 * time.Second)
	currentToken, locked, err := svcCtx.Router.AcquirePlayerLock(ctx, client, 31)
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { _ = svcCtx.Router.ReleasePlayerLock(ctx, client, 31, currentToken) })

	status, _, err := saveFieldsAtomic(ctx, client, 31, staleToken, 0,
		map[string][]byte{"hp": []byte("stale")})
	require.NoError(t, err)
	assert.Equal(t, saveFieldsLockLost, status)

	value, err := GetPlayerField(ctx, svcCtx, 31, "hp")
	require.NoError(t, err)
	assert.Nil(t, value, "TTL 后恢复的旧持锁者不得写入任何字段")

	status, _, err = saveFieldsAtomic(ctx, client, 31, currentToken, 0,
		map[string][]byte{"hp": []byte("current")})
	require.NoError(t, err)
	assert.Equal(t, saveFieldsOK, status)
}

func TestInternalLockFieldCannotBeReadOrOverwritten(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 32, 1)
	client, err := svcCtx.Router.ClientForPlayer(ctx, 32)
	require.NoError(t, err)
	token, locked, err := svcCtx.Router.AcquirePlayerLock(ctx, client, 32)
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { _ = svcCtx.Router.ReleasePlayerLock(ctx, client, 32, token) })

	loaded, err := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{PlayerID: 32})
	require.NoError(t, err)
	assert.NotContains(t, loaded.Data, "__lock", "distributed-lock token must never leak as player data")

	explicit, err := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{PlayerID: 32, Fields: []string{"__lock"}})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, explicit.ErrorCode)

	saveResp, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 32,
		Data:     map[string][]byte{"__lock": []byte("attacker")},
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, saveResp.ErrorCode)
	assert.Equal(t, token, client.Get(ctx, routing.PlayerDataLockKey(32)).Val(),
		"reserved-field validation must leave the real lock token untouched")
}

// ── GetPlayerField / SetPlayerField ────────────────────────────

func TestGetSetPlayerField(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 40, 1)

	// Get non-existent field returns nil
	val, err := GetPlayerField(ctx, svcCtx, 40, "gold")
	assert.NoError(t, err)
	assert.Nil(t, val)

	// Set field
	resp, err := SetPlayerField(ctx, svcCtx, 40, "gold", []byte("9999"), 0)
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.True(t, resp.NewVersion > 0)

	// Get field
	val, err = GetPlayerField(ctx, svcCtx, 40, "gold")
	assert.NoError(t, err)
	assert.Equal(t, []byte("9999"), val)
}

func TestSetPlayerField_VersionMismatch(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 50, 1)

	// First set (creates version 1)
	SetPlayerField(ctx, svcCtx, 50, "level", []byte("1"), 0)

	// Set with wrong expected version
	resp, err := SetPlayerField(ctx, svcCtx, 50, "level", []byte("2"), 999)
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeVersionMismatch, resp.ErrorCode)
}

// ── DeletePlayerData ───────────────────────────────────────────

func TestDeletePlayerData(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 60, 1)

	// Save some data
	SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 60,
		Data: map[string][]byte{
			"bag":  []byte("items"),
			"attr": []byte("stats"),
		},
	})

	// Verify data exists
	resp, _ := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{PlayerID: 60})
	assert.True(t, len(resp.Data) > 0)

	// Delete without zone mapping
	delResp, err := DeletePlayerData(ctx, svcCtx, &DeletePlayerDataReq{
		PlayerID:          60,
		DeleteZoneMapping: false,
	})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, delResp.ErrorCode)
	assert.True(t, delResp.KeysDeleted > 0)

	// Zone mapping still exists
	zone, err := svcCtx.Router.GetPlayerHomeZone(ctx, 60)
	assert.NoError(t, err)
	assert.Equal(t, uint32(1), zone)
}

func TestDeletePlayerData_WithZoneMapping(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 70, 2)

	SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 70,
		Data:     map[string][]byte{"x": []byte("y")},
	})

	delResp, err := DeletePlayerData(ctx, svcCtx, &DeletePlayerDataReq{
		PlayerID:          70,
		DeleteZoneMapping: true,
	})
	assert.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, delResp.ErrorCode)

	// Zone mapping should be gone
	_, err = svcCtx.Router.GetPlayerHomeZone(ctx, 70)
	assert.Error(t, err)
}

func TestDeletePlayerData_ExpiredOwnerCannotDeleteDataOrMapping(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 71, 2)

	_, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID: 71,
		Data:     map[string][]byte{"inventory": []byte("current")},
	})
	require.NoError(t, err)
	client, err := svcCtx.Router.ClientForPlayer(ctx, 71)
	require.NoError(t, err)

	staleToken, locked, err := svcCtx.Router.AcquirePlayerLock(ctx, client, 71)
	require.NoError(t, err)
	require.True(t, locked)
	mr.FastForward(4 * time.Second)
	currentToken, locked, err := svcCtx.Router.AcquirePlayerLock(ctx, client, 71)
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { _ = svcCtx.Router.ReleasePlayerLock(ctx, client, 71, currentToken) })

	owned, err := deletePlayerKeysAtomic(ctx, client, routing.PlayerDataLockKey(71), staleToken,
		[]string{playerField(71, "inventory")})
	require.NoError(t, err)
	assert.False(t, owned)

	owned, _, err = svcCtx.Router.DeletePlayerZoneIfLocked(ctx, 71, staleToken)
	require.NoError(t, err)
	assert.False(t, owned)

	value, err := GetPlayerField(ctx, svcCtx, 71, "inventory")
	require.NoError(t, err)
	assert.Equal(t, []byte("current"), value)
	zone, err := svcCtx.Router.GetPlayerHomeZone(ctx, 71)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), zone)
}
