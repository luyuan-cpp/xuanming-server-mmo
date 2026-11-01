package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/placement"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/syncx"
)

// 存档库和目录可能不在一个集群(docs/design/horizontal-scale-logical-db.md §6),所以 data_service 按 storage_id 各持一个连接池,
// 闸门按库。和 go/db 同口径:第一次用到才打开,之后常驻;打开只连接和只读核对,从不建库。

const (
	// playerStoreOpenTimeout 是一次打开的预算。打开发生在请求路径上。
	playerStoreOpenTimeout = 10 * time.Second
	// playerStoreOpenBackoff 是打开失败后的冷却:期间直接回上次的错误,坏库不拖住别的玩家。
	playerStoreOpenBackoff = 10 * time.Second
)

// PlayerStorePoolOptions 是每个存档库连接池的参数(config.DirectoryConfig.StoragePools)。
type PlayerStorePoolOptions struct {
	MaxOpenConn     int
	MaxIdleConn     int
	ConnMaxIdleTime time.Duration
}

type playerStoreOpenFailure struct {
	at  time.Time
	err error
}

// PlayerStorePools 按 storage_id 给出已打开的存档库。并发安全。
type PlayerStorePools struct {
	cfg     MySQLConfig
	backend authoritysql.Backend
	options PlayerStorePoolOptions
	allowed map[uint32]bool
	now     func() time.Time
	flight  syncx.SingleFlight
	// opener 打开一个库,nil 时走 openPool。单测换成替身。
	opener func(schema string) (*sql.DB, error)

	mu       sync.RWMutex
	pools    map[uint32]*sql.DB
	failures map[uint32]playerStoreOpenFailure
}

// newPlayerStorePools 校验白名单。白名单是部署声明的可访问范围,不从 Regions 或本实例 zone 推导。
func newPlayerStorePools(cfg MySQLConfig, backend authoritysql.Backend, storageIDs []uint32, options PlayerStorePoolOptions) (*PlayerStorePools, error) {
	if len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return nil, accountdirectory.ErrUnavailable
	}
	allowed := make(map[uint32]bool, len(storageIDs))
	for _, id := range storageIDs {
		if _, ok := placement.StoreDBName(id); !ok || allowed[id] {
			return nil, accountdirectory.ErrInvalid
		}
		allowed[id] = true
	}
	if options.MaxOpenConn <= 0 {
		options.MaxOpenConn = 8
	}
	if options.MaxIdleConn <= 0 {
		options.MaxIdleConn = 2
	}
	return &PlayerStorePools{
		cfg:      cfg,
		backend:  backend,
		options:  options,
		allowed:  allowed,
		now:      time.Now,
		flight:   syncx.NewSingleFlight(),
		pools:    map[uint32]*sql.DB{},
		failures: map[uint32]playerStoreOpenFailure{},
	}, nil
}

// Allowed 报告这个落点在不在白名单里。
func (p *PlayerStorePools) Allowed(storageID uint32) bool { return p != nil && p.allowed[storageID] }

// DB 返回落点的连接池和库名,必要时打开。不在白名单回 ErrUnavailable;打开失败回包着原因的错误。
// 打开不用调用方的 ctx:同一个库的打开结果被并发请求共享,不能随第一个调用方的取消一起失败。
func (p *PlayerStorePools) DB(_ context.Context, storageID uint32) (*sql.DB, string, error) {
	if !p.Allowed(storageID) {
		return nil, "", accountdirectory.ErrUnavailable
	}
	schema, ok := placement.StoreDBName(storageID)
	if !ok {
		return nil, "", accountdirectory.ErrInvalid
	}
	p.mu.RLock()
	db := p.pools[storageID]
	p.mu.RUnlock()
	if db != nil {
		return db, schema, nil
	}
	v, err := p.flight.Do(schema, func() (any, error) { return p.open(storageID, schema) })
	if err != nil {
		return nil, "", err
	}
	db, _ = v.(*sql.DB)
	if db == nil {
		return nil, "", fmt.Errorf("open player store %s: no pool", schema)
	}
	return db, schema, nil
}

func (p *PlayerStorePools) open(storageID uint32, schema string) (*sql.DB, error) {
	p.mu.RLock()
	db := p.pools[storageID]
	failure, failed := p.failures[storageID]
	p.mu.RUnlock()
	if db != nil {
		return db, nil
	}
	if failed && p.now().Sub(failure.at) < playerStoreOpenBackoff {
		return nil, failure.err
	}
	opener := p.opener
	if opener == nil {
		opener = p.openPool
	}
	db, err := opener(schema)
	if err != nil {
		wrapped := fmt.Errorf("open player store %s: %w", schema, err)
		// 闸门冻结是搬库窗口,秒级,库本身没有坏:不进冷却,窗口一过下一个请求就能打开。
		if authoritysql.IsWriterFrozen(err) {
			logx.Infof("[player-store] open hit a frozen writer gate, not backing off: %s", schema)
			return nil, wrapped
		}
		p.mu.Lock()
		p.failures[storageID] = playerStoreOpenFailure{at: p.now(), err: wrapped}
		p.mu.Unlock()
		logx.Errorf("[player-store] open failed, next attempt in %v: %s: %v", playerStoreOpenBackoff, schema, err)
		return nil, wrapped
	}
	p.mu.Lock()
	p.pools[storageID] = db
	delete(p.failures, storageID)
	p.mu.Unlock()
	return db, nil
}

func (p *PlayerStorePools) openPool(schema string) (*sql.DB, error) {
	one, err := p.cfg.ForSchema(schema)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), playerStoreOpenTimeout)
	defer cancel()
	db, err := authoritysql.Open(ctx, buildDSN(one), schema, p.backend, p.options.MaxOpenConn, one.Permission, one.PoolOptions...)
	if err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(p.options.MaxIdleConn)
	if p.options.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(p.options.ConnMaxIdleTime)
	}
	// 只读核对:缺表的库早点报出来,不等到第一笔写。go/db 的迁移才建表。
	var present int
	if err := db.QueryRowContext(ctx, "SELECT 1 FROM player_store LIMIT 1").Scan(&present); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Close 关闭全部已打开的池。
func (p *PlayerStorePools) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var result error
	for id, db := range p.pools {
		result = errors.Join(result, db.Close())
		delete(p.pools, id)
	}
	return result
}
