package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 账本和展示资料读和登录共用账号目录池,不能把连接占满。
func TestPlayerStoreReadLimitLeavesConnectionsForLogin(t *testing.T) {
	for maxOpenConn, want := range map[int]int{1: 1, 2: 1, 8: 4, 128: 64} {
		require.Equal(t, want, playerStoreReadLimit(maxOpenConn), "max_open_conn=%d", maxOpenConn)
	}
}

// 名额用完时等到调用方的截止时间就失败,不会一直等。
func TestAcquirePlayerStoreReadHonorsTheCallerDeadline(t *testing.T) {
	s := &AccountDirectoryStore{playerStoreReads: make(chan struct{}, 1)}
	release, err := s.acquirePlayerStoreRead(context.Background())
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.acquirePlayerStoreRead(canceled)
	require.ErrorIs(t, err, context.Canceled)

	release()
	release, err = s.acquirePlayerStoreRead(context.Background())
	require.NoError(t, err)
	release()
}
