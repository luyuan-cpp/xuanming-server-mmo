package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"shared/dirshard"
)

// 目录按账号、玩家、名字哈希分成 K 个逻辑库(docs/design/horizontal-scale-logical-db.md §5.1)。
// 每片一个连接池、一份闸门。事务只开在一个逻辑库上,跨片一律分步,所以这里只给「这个键在哪个池」,不给跨片事务。

const (
	// DirectoryShardTableName 每片一行,记这个库是 K 片里的第几片。
	DirectoryShardTableName = "directory_shard"
	directoryShardDDL       = "CREATE TABLE IF NOT EXISTS directory_shard (id INT UNSIGNED NOT NULL COMMENT 'pb:1',logical_shards INT UNSIGNED NOT NULL COMMENT 'pb:2',slot INT UNSIGNED NOT NULL COMMENT 'pb:3',PRIMARY KEY(id)) ENGINE=InnoDB"

	// shardFanOut 是一次批量读同时打的片数,也是启动时同时开池的片数。K=1 时不起协程。
	shardFanOut = 8
	// shardProbeTimeout 是开一片时核对 directory_shard 的上限。
	shardProbeTimeout = 5 * time.Second
)

// shardPools 是目录 K 片的连接池,下标就是片号。构造后只读。
type shardPools struct {
	k     int
	names []string
	pools []*sql.DB
}

// singleShardPools 把一个已开好的池当成 K=1,给直接持池的用例和探针用。
func singleShardPools(db *sql.DB, name string) *shardPools {
	return &shardPools{k: 1, names: []string{name}, pools: []*sql.DB{db}}
}

// openShardPools 逐片开池并核对 directory_shard。open 决定这一片的 DSN、连接池上限和表形状核对。任一片失败就关掉已开的,回错误。
func openShardPools(cfg MySQLConfig, open func(MySQLConfig) (*sql.DB, error)) (*shardPools, error) {
	k := dirshard.Normalize(cfg.LogicalShards)
	if err := dirshard.Validate(k); err != nil {
		return nil, err
	}
	shards := &shardPools{k: k, names: dirshard.DirectoryDBNames(cfg.DBName, k), pools: make([]*sql.DB, k)}
	err := runParallel(k, shardFanOut, func(slot int) error {
		one, err := cfg.ForSchema(shards.names[slot])
		if err != nil {
			return err
		}
		db, err := open(one)
		if err != nil {
			return fmt.Errorf("directory shard %s: %w", shards.names[slot], err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), shardProbeTimeout)
		defer cancel()
		if err := verifyDirectoryShard(ctx, db, slot, k); err != nil {
			_ = db.Close()
			return fmt.Errorf("directory shard %s: %w", shards.names[slot], err)
		}
		shards.pools[slot] = db
		return nil
	})
	if err != nil {
		return nil, errors.Join(err, shards.Close())
	}
	return shards, nil
}

// verifyDirectoryShard 核这个库确实是 K 片里的第 slot 片。配错 K 会把玩家路由到别的片,读到「角色不存在」而且没有报错,所以拒绝启动。
func verifyDirectoryShard(ctx context.Context, db *sql.DB, slot, k int) error {
	var gotShards, gotSlot int
	err := db.QueryRowContext(ctx, "SELECT logical_shards,slot FROM directory_shard WHERE id=1").Scan(&gotShards, &gotSlot)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("directory_shard has no row id=1; run `data_service -f <yaml> -migrate` first")
	}
	if err != nil {
		return fmt.Errorf("read directory_shard: %w", err)
	}
	if gotShards != k || gotSlot != slot {
		return fmt.Errorf("this database is slot %d of %d, but Directory.LogicalShards says slot %d of %d; K cannot change after the tables are created",
			gotSlot, gotShards, slot, k)
	}
	return nil
}

// seedDirectoryShard 种 directory_shard(id=1)。已有行不改,读回来必须一致:K 建表之后不能改。
func seedDirectoryShard(ctx context.Context, db *sql.DB, slot, k int) error {
	if _, err := db.ExecContext(ctx, "INSERT INTO directory_shard(id,logical_shards,slot) VALUES (1,?,?) ON DUPLICATE KEY UPDATE id=id", k, slot); err != nil {
		return fmt.Errorf("seed directory_shard: %w", err)
	}
	return verifyDirectoryShard(ctx, db, slot, k)
}

func (p *shardPools) forPlayer(id uint64) *sql.DB { return p.pools[dirshard.SlotOfPlayer(id, p.k)] }

func (p *shardPools) forAccount(account string) *sql.DB {
	return p.pools[dirshard.SlotOfAccount(account, p.k)]
}

func (p *shardPools) forName(norm string) *sql.DB { return p.pools[dirshard.SlotOfName(norm, p.k)] }

// first 是第 0 片:zone_merged_into 只在这里。
func (p *shardPools) first() *sql.DB { return p.pools[0] }

// Close 关闭全部已开的池。
func (p *shardPools) Close() error {
	var result error
	for _, db := range p.pools {
		if db != nil {
			result = errors.Join(result, db.Close())
		}
	}
	return result
}

// stats 把各片的连接池统计加起来,给 /metrics 用。不做 I/O。
func (p *shardPools) stats() sql.DBStats {
	var total sql.DBStats
	for _, db := range p.pools {
		if db == nil {
			continue
		}
		s := db.Stats()
		total.MaxOpenConnections += s.MaxOpenConnections
		total.OpenConnections += s.OpenConnections
		total.InUse += s.InUse
		total.Idle += s.Idle
		total.WaitCount += s.WaitCount
		total.WaitDuration += s.WaitDuration
		total.MaxIdleClosed += s.MaxIdleClosed
		total.MaxIdleTimeClosed += s.MaxIdleTimeClosed
		total.MaxLifetimeClosed += s.MaxLifetimeClosed
	}
	return total
}

// eachPlayerGroup 把玩家按片拆组,每组调一次 fn。任一组失败整批报错,不回半份结果:半份会被调用方当成「这些人没有」。
// fn 可能并发调用,调用方自己保护结果。组内顺序不变。
func (p *shardPools) eachPlayerGroup(ids []uint64, fn func(db *sql.DB, players []uint64) error) error {
	groups := make(map[int][]uint64)
	for _, id := range ids {
		slot := dirshard.SlotOfPlayer(id, p.k)
		groups[slot] = append(groups[slot], id)
	}
	slots := make([]int, 0, len(groups))
	for slot := range groups {
		slots = append(slots, slot)
	}
	return runParallel(len(slots), shardFanOut, func(i int) error {
		return fn(p.pools[slots[i]], groups[slots[i]])
	})
}

// runParallel 跑 n 次 fn,最多 limit 个同时。全部跑完才回,错误合在一起。n=1 时不起协程。
func runParallel(n, limit int, fn func(i int) error) error {
	if n == 1 {
		return fn(0)
	}
	errs := make([]error, n)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = fn(i)
		}(i)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// txNotBegunError 表示事务根本没开始:ctx 已结束,或者 BEGIN 失败(闸门拒绝、连不上)。一条业务语句都没发,确定什么都没写。
type txNotBegunError struct{ err error }

func (e *txNotBegunError) Error() string { return e.err.Error() }
func (e *txNotBegunError) Unwrap() error { return e.err }

// txNeverBegan 判这个错误是不是事务没开始。别的错误里只有事务内的语句失败能确定没提交,这里不区分,都按结局未知算:宁可保守。
func txNeverBegan(err error) bool {
	var notBegun *txNotBegunError
	return errors.As(err, &notBegun)
}

// shardTx 在一个逻辑库上跑一个事务。读已提交:锁推演只在 RC 下成立。ctx 已结束时不提交。
// 事务没开始的错误能用 txNeverBegan 认出来,错误链不变。
func shardTx(ctx context.Context, db *sql.DB, action func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return &txNotBegunError{err: err}
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return &txNotBegunError{err: err}
	}
	defer tx.Rollback()
	if err = action(tx); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}
