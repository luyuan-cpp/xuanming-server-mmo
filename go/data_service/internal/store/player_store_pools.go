package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/placement"
)

// 存档库和目录可能不在一个集群(docs/design/horizontal-scale-logical-db.md §6),所以 data_service 按 storage_id 各持一个连接池,
// 闸门按库。和 go/db 同口径:第一次用到才打开,之后常驻;打开只连接和只读核对,从不建库。
// 「成功后常驻、失败冷却、闸门冻结不进冷却」这套规则在 openOnce(lazy_open.go),目录各片和流水库的首次核对也用它。

// playerStoreOpenTimeout 是一次打开的预算。打开发生在请求路径上。
const playerStoreOpenTimeout = 10 * time.Second

// PlayerStorePoolOptions 是每个存档库连接池的参数(config.DirectoryConfig.StoragePools)。
type PlayerStorePoolOptions struct {
	MaxOpenConn     int
	MaxIdleConn     int
	ConnMaxIdleTime time.Duration
}

// PlayerStorePools 按 storage_id 给出已打开的存档库。并发安全。
type PlayerStorePools struct {
	cfg     MySQLConfig
	backend authoritysql.Backend
	options PlayerStorePoolOptions
	allowed map[uint32]bool
	// opener 打开一个库,nil 时走 openPool。单测换成替身。
	opener func(schema string) (*sql.DB, error)
	// reads 是每个库留给账本、展示读的名额(readSlots),白名单里的库各一份,构造后只读。
	reads map[uint32]readSlots
	// opens 是每个库的打开状态,白名单里的库各一份,这张表构造后只读。
	opens map[uint32]*openOnce[*sql.DB]
}

// newPlayerStorePools 校验白名单。白名单是部署声明的可访问范围,不从 Regions 或本实例 zone 推导。
func newPlayerStorePools(cfg MySQLConfig, backend authoritysql.Backend, storageIDs []uint32, options PlayerStorePoolOptions) (*PlayerStorePools, error) {
	if len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return nil, accountdirectory.ErrUnavailable
	}
	allowed := make(map[uint32]bool, len(storageIDs))
	opens := make(map[uint32]*openOnce[*sql.DB], len(storageIDs))
	for _, id := range storageIDs {
		schema, ok := placement.StoreDBName(id)
		if !ok || allowed[id] {
			return nil, accountdirectory.ErrInvalid
		}
		allowed[id] = true
		opens[id] = newOpenOnce[*sql.DB]("player store "+schema, time.Now)
	}
	if options.MaxOpenConn <= 0 {
		options.MaxOpenConn = 8
	}
	if options.MaxIdleConn <= 0 {
		options.MaxIdleConn = 2
	}
	reads := make(map[uint32]readSlots, len(allowed))
	for id := range allowed {
		reads[id] = newReadSlots(options.MaxOpenConn)
	}
	return &PlayerStorePools{cfg: cfg, backend: backend, options: options, allowed: allowed, reads: reads, opens: opens}, nil
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
	db, err := p.opens[storageID].get(func() (*sql.DB, error) {
		opener := p.opener
		if opener == nil {
			opener = p.openPool
		}
		return opener(schema)
	})
	if err != nil {
		return nil, "", err
	}
	if db == nil {
		return nil, "", fmt.Errorf("open player store %s: no pool", schema)
	}
	return db, schema, nil
}

// ReadDB 给账本和展示读用:返回落点的连接池,并占住这个库的一个读名额,release 必须在这次读用完连接之后调用。
// 建角、判死释放、交易冻结证据走 DB,不占名额:名额就是为了不让读挤掉它们。
// 先打开再取名额,第一次打开一个库的那几秒不占着名额。
func (p *PlayerStorePools) ReadDB(ctx context.Context, storageID uint32) (db *sql.DB, release func(), err error) {
	db, _, err = p.DB(ctx, storageID)
	if err != nil {
		return nil, nil, err
	}
	release, err = p.reads[storageID].acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	return db, release, nil
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
	var result error
	for _, once := range p.opens {
		if db, ok := once.loaded(); ok && db != nil {
			result = errors.Join(result, db.Close())
		}
	}
	return result
}
