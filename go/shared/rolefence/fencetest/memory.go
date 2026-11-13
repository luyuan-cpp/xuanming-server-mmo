package fencetest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"shared/rolefence"
)

// MemoryReplica 是内存里的 role_fence 副本表,给不连库的单测用:测服务把 rolefence.Guard 接进入口归属预检的那一小段
// (取原身份、错误映射)。它只认 rolefence 不加锁的那两条语句:按主键读副本、按版本合并写副本;别的语句一律报错。
//
// 不支持事务,也不认 FOR UPDATE:提交点的加锁复核要用真库测(各服务的 TiDB 用例)。「版本大的赢」在这里只是照着模拟,
// SQL 的真实语义由 rolefence_tidb_test.go 钉住;本替身与 rolefence 的语句对不上时由 memory_test.go 报出来。
type MemoryReplica struct {
	db *sql.DB

	mu    sync.Mutex
	rows  map[uint64]rolefence.Fence
	reads []uint64
	err   error
}

// NewMemoryReplica 建一张空的内存副本表。连接池随用例结束关闭。
func NewMemoryReplica(t testing.TB) *MemoryReplica {
	t.Helper()
	m := &MemoryReplica{rows: map[uint64]rolefence.Fence{}}
	m.db = sql.OpenDB(memoryConnector{m})
	t.Cleanup(func() { _ = m.db.Close() })
	return m
}

// DB 是接在这张表上的连接池,交给 rolefence.NewGuard 或服务自己的装配函数。
func (m *MemoryReplica) DB() *sql.DB { return m.db }

// Put 直接摆一行副本,不走版本合并。version、home zone 为 0 时补成 1(副本行不允许这两项为 0)。
func (m *MemoryReplica) Put(fence rolefence.Fence) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fence.Version == 0 {
		fence.Version = 1
	}
	if fence.HomeZone == 0 {
		fence.HomeZone = 1
	}
	m.rows[fence.PlayerID] = fence
}

// Row 回一名角色现在的副本行。
func (m *MemoryReplica) Row(player uint64) (rolefence.Fence, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fence, ok := m.rows[player]
	return fence, ok
}

// Reads 回按顺序读过的角色,测「稳态只读一次本库」用。
func (m *MemoryReplica) Reads() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uint64(nil), m.reads...)
}

// FailWith 让之后的每条语句都回这个错误,模拟库故障;传 nil 恢复。
func (m *MemoryReplica) FailWith(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

var errMemoryReplicaUnsupported = errors.New("fencetest.MemoryReplica only serves the lock-free replica read and the versioned replica write")

type memoryConnector struct{ replica *MemoryReplica }

func (c memoryConnector) Connect(context.Context) (driver.Conn, error) {
	return memoryConn{c.replica}, nil
}
func (c memoryConnector) Driver() driver.Driver { return nil }

type memoryConn struct{ replica *MemoryReplica }

func (c memoryConn) Prepare(string) (driver.Stmt, error) { return nil, errMemoryReplicaUnsupported }
func (c memoryConn) Close() error                        { return nil }
func (c memoryConn) Begin() (driver.Tx, error)           { return nil, errMemoryReplicaUnsupported }

// 保留原始类型:删角令牌的最高位过不了默认转换器,真驱动也有自己的 CheckNamedValue。
func (c memoryConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c memoryConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	m := c.replica
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if !strings.HasPrefix(query, "SELECT account,owner_revision,freeze_token,freeze_final,version,home_zone FROM role_fence WHERE player_id=?") ||
		strings.Contains(query, "FOR UPDATE") || len(args) != 1 {
		return nil, fmt.Errorf("%w: %s", errMemoryReplicaUnsupported, query)
	}
	player, ok := args[0].Value.(uint64)
	if !ok {
		return nil, fmt.Errorf("%w: player id is %T", errMemoryReplicaUnsupported, args[0].Value)
	}
	m.reads = append(m.reads, player)
	fence, found := m.rows[player]
	return &memoryRows{fence: fence, left: found}, nil
}

func (c memoryConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	m := c.replica
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if !strings.HasPrefix(query, "INSERT INTO role_fence(player_id,account,owner_revision,freeze_token,freeze_final,version,home_zone,updated_ms)") || len(args) != 7 {
		return nil, fmt.Errorf("%w: %s", errMemoryReplicaUnsupported, query)
	}
	player, playerOK := args[0].Value.(uint64)
	account, accountOK := args[1].Value.([]byte)
	revision, revisionOK := args[2].Value.(uint64)
	token, tokenOK := args[3].Value.(uint64)
	final, finalOK := args[4].Value.(bool)
	version, versionOK := args[5].Value.(uint64)
	homeZone, homeZoneOK := args[6].Value.(uint32)
	if !playerOK || !accountOK || !revisionOK || !tokenOK || !finalOK || !versionOK || !homeZoneOK {
		return nil, fmt.Errorf("%w: unexpected argument types for the replica write", errMemoryReplicaUnsupported)
	}
	if old, exists := m.rows[player]; exists && version <= old.Version {
		return driver.RowsAffected(0), nil
	}
	m.rows[player] = rolefence.Fence{PlayerID: player, Account: string(account), OwnerRevision: revision, FreezeToken: token, FreezeFinal: final, Version: version, HomeZone: homeZone}
	return driver.RowsAffected(1), nil
}

type memoryRows struct {
	fence rolefence.Fence
	left  bool
}

func (r *memoryRows) Columns() []string {
	return []string{"account", "owner_revision", "freeze_token", "freeze_final", "version", "home_zone"}
}
func (r *memoryRows) Close() error { return nil }
func (r *memoryRows) Next(dest []driver.Value) error {
	if !r.left {
		return io.EOF
	}
	r.left = false
	dest[0], dest[1], dest[2], dest[3], dest[4], dest[5] = []byte(r.fence.Account), r.fence.OwnerRevision, r.fence.FreezeToken, r.fence.FreezeFinal, r.fence.Version, int64(r.fence.HomeZone)
	return nil
}
