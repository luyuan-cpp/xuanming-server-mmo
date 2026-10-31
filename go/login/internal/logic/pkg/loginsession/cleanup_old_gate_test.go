package loginsession

import (
	"context"
	"testing"

	"shared/redisslot"

	"github.com/stretchr/testify/require"
)

// 旧 gate 的异步入场清理可能晚于新 gate 的同号连接登录；只能清理旧连接。
func TestCleanupOldGateLeavesRestartedGateSessionAndDevice(t *testing.T) {
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	oldRef := mustRef(t, oldGateDetails())
	newRef := mustRef(t, restartedGateDetails())
	const newAccount = "restarted_gate_account"
	for _, entry := range []struct {
		ref     Ref
		account string
	}{{oldRef, victimAccount}, {newRef, newAccount}} {
		require.NoError(t, rdb.Set(ctx, entry.ref.Key(), entry.account, TTL()).Err())
		require.NoError(t, rdb.SAdd(ctx, redisslot.AccountDeviceSetKey(entry.account), entry.ref.DeviceMember()).Err())
	}

	for range 2 {
		Cleanup(ctx, rdb, oldRef, "test")
		account, err := GetAccount(ctx, rdb, newRef)
		require.NoError(t, err)
		require.Equal(t, newAccount, account, "旧连接迟到或重复清理不能删除新连接的权威登录态")
		member, err := rdb.SIsMember(ctx, redisslot.AccountDeviceSetKey(newAccount), newRef.DeviceMember()).Result()
		require.NoError(t, err)
		require.True(t, member, "新连接的账号设备成员必须保留")
	}
	require.Zero(t, rdb.Exists(ctx, oldRef.Key()).Val())
	require.False(t, rdb.SIsMember(ctx, redisslot.AccountDeviceSetKey(victimAccount), oldRef.DeviceMember()).Val())
}
