package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"

	"github.com/go-sql-driver/mysql"
)

// directorySteps 是建角、过户、读列表、判死编排用到的每一步。每个方法只碰一个逻辑库,一个方法最多一个事务。
//
// 编排(失败路径、重放、顺序)在 AccountDirectoryStore 和 AccountRoleTradeStore 里,这里只有 SQL。分开是为了能用内存替身把
// 「每一步失败之后的结局和重放」跑一遍:那些组合要真库跑得一个个摆故障,而结局只取决于编排。
type directorySteps interface {
	// 账号分片。
	readAccount(ctx context.Context, account string) (exists bool, refs []roleRef, err error)
	ensureAccount(ctx context.Context, account string) error
	readRef(ctx context.Context, account string, player uint64) (*roleRef, error)
	reserveRef(ctx context.Context, account string, want roleRef, maxRoles int) ([]roleRef, error)
	activateRef(ctx context.Context, account string, player, txID uint64) error
	promoteRef(ctx context.Context, account string, ref roleRef) error
	dropRef(ctx context.Context, account string, ref roleRef) (bool, error)
	scanPendingRefs(ctx context.Context, olderThan time.Duration, limit int) ([]roleRef, error)

	// 玩家分片。readOwner 把墓碑也读出来(Account 为空),没有行回 nil。
	readOwner(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error)
	readOwners(ctx context.Context, players []uint64) (map[uint64]*dspb.AccountRoleOwnership, error)
	readRoute(ctx context.Context, player uint64) (RouteRow, bool, error)
	commitRole(ctx context.Context, account string, role *base.AccountSimplePlayer, raw []byte, storage uint32) error
	tombstoneMissingRole(ctx context.Context, account string, ref roleRef) (won bool, err error)
	readListingState(ctx context.Context, player, listing uint64) (dspb.AccountRoleTradeState, bool, error)
	readListing(ctx context.Context, player, listing uint64) (*dbpb.AccountRoleTrade, error)
	readTransfer(ctx context.Context, player, txID uint64) (*dbpb.AccountRoleTransferReceipt, error)
	commitTransfer(ctx context.Context, in *dspb.TransferAccountRoleRequest, digest []byte) (*dspb.AccountRoleTransferReceipt, error)
	readFence(ctx context.Context, player uint64) (*rfpb.RoleFenceState, error)
	hasPendingHolders(ctx context.Context, player, listing uint64) (bool, error)
	readDeadHolderPlan(ctx context.Context, listing, player uint64) (*deadHolderPlan, error)
	markHoldersDrained(ctx context.Context, plan *deadHolderPlan) error

	// 存档库。
	storageAllowed(storage uint32) bool
	writeInitialRows(ctx context.Context, storage uint32, role *base.AccountSimplePlayer) error
	readStoreOwner(ctx context.Context, storage uint32, player uint64) (*lockedPlayerStoreOwner, error)
	raiseStoreEpoch(ctx context.Context, storage uint32, player uint64, seen lockedPlayerStoreOwner, epoch uint64) (bool, error)
}

// sqlDirectorySteps 是生产实现。
type sqlDirectorySteps struct {
	shards    *shardPools
	directory *AccountDirectoryStore
	// sweepCursor 让后台清理逐轮换起点。
	sweepCursor atomic.Uint64
}

// readAccount 读目录行和全部 ref,不取行锁。
func (st *sqlDirectorySteps) readAccount(ctx context.Context, account string) (bool, []roleRef, error) {
	var exists bool
	var refs []roleRef
	err := shardTx(ctx, st.shards.forAccount(account), func(tx *sql.Tx) error {
		var stored []byte
		err := tx.QueryRowContext(ctx, "SELECT account FROM account_directory WHERE account=?", []byte(account)).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		refs, err = readRefs(ctx, tx, account)
		return err
	})
	if err != nil {
		return false, nil, err
	}
	return exists, refs, nil
}

func (st *sqlDirectorySteps) ensureAccount(ctx context.Context, account string) error {
	return shardTx(ctx, st.shards.forAccount(account), func(tx *sql.Tx) error {
		return lockDirectory(ctx, tx, account, true)
	})
}

func (st *sqlDirectorySteps) readOwner(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	owned, err := scanOwnership(st.shards.forPlayer(player).QueryRowContext(ctx, "SELECT "+ownershipColumns+" FROM account_role WHERE player_id=?", player), player)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return nil, nil
	}
	return owned, err
}

// readOwners 按玩家分片拆组批量读。没有行的 id 不出现在结果里。
func (st *sqlDirectorySteps) readOwners(ctx context.Context, players []uint64) (map[uint64]*dspb.AccountRoleOwnership, error) {
	out := make(map[uint64]*dspb.AccountRoleOwnership, len(players))
	var mu sync.Mutex
	err := st.shards.eachPlayerGroup(players, func(db *sql.DB, group []uint64) error {
		args := make([]any, 0, len(group))
		for _, id := range group {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, "SELECT player_id,"+ownershipColumns+" FROM account_role WHERE player_id IN (?"+strings.Repeat(",?", len(group)-1)+")", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, rev, listing, prepare uint64
			var account string
			var raw []byte
			if err := rows.Scan(&id, &account, &raw, &rev, &listing, &prepare); err != nil {
				return err
			}
			owned, err := decodeOwnership(id, account, raw, rev, listing, prepare)
			if err != nil {
				return err
			}
			mu.Lock()
			out[id] = owned
			mu.Unlock()
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readRoute 强读路由行。found=false 是确定没有这一行。
func (st *sqlDirectorySteps) readRoute(ctx context.Context, player uint64) (RouteRow, bool, error) {
	routes, err := loadRoutes(ctx, st.shards.forPlayer(player), []uint64{player})
	if err != nil {
		return RouteRow{}, false, err
	}
	row, ok := routes[player]
	return row, ok, nil
}

// commitRole 是建角的提交点:核路由行,严格插入 account_role。
//
// route_meta.writable 只普通读:每次建角都对这一行 FOR UPDATE 的话全片建角排成一列。读到旧值时提交点的保证靠写者闸门:
// 事务从 BEGIN 起持着本写者的 writer_slot 行锁,持锁后复核过 route_meta,而冻结推进 epoch 要先拿到所有在册写者的 slot 行锁,
// 所以冻结只能排在这笔事务提交之后。这条普通读只负责把「已冻结」翻译成 ErrRouteNotWritable。
//
// BEGIN 就失败(闸门拒绝、连不上)回 ErrNotCommitted:一条业务语句都没发。别的失败调用方都按结局未知处理。
func (st *sqlDirectorySteps) commitRole(ctx context.Context, account string, role *base.AccountSimplePlayer, raw []byte, storage uint32) error {
	err := shardTx(ctx, st.shards.forPlayer(role.PlayerId), func(tx *sql.Tx) error {
		if err := requireRouteWritable(ctx, tx); err != nil {
			return err
		}
		var route RouteRow
		err := scanRouteRow(tx.QueryRowContext(ctx, routeLockRowSQL, routeShardArg(role.PlayerId), role.PlayerId), &route, false)
		if errors.Is(err, sql.ErrNoRows) {
			return accountdirectory.ErrConflict
		}
		if err != nil {
			return err
		}
		// 第 2 步写的就是这个落点。storage_id 建角后不变,不等就是有人绕过 store 改过。
		if route.HomeZone != role.ZoneId || route.Rev == 0 || route.StorageID != storage {
			return accountdirectory.ErrConflict
		}
		old, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, role.PlayerId), role.PlayerId)
		if err == nil {
			return checkCreateReplay(old, account, role)
		}
		if !errors.Is(err, accountdirectory.ErrMissing) {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO account_role(player_id,account,role_proto,owner_revision,updated_ms,initial_player_commit,fence_version) VALUES (?,?,?,1,UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000,1,1)", role.PlayerId, []byte(account), raw)
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == mysqlErrDupEntry {
			return accountdirectory.ErrConflict
		}
		return err
	})
	if txNeverBegan(err) {
		return notCommitted(err)
	}
	return err
}

func (st *sqlDirectorySteps) readListingState(ctx context.Context, player, listing uint64) (dspb.AccountRoleTradeState, bool, error) {
	var state uint32
	err := st.shards.forPlayer(player).QueryRowContext(ctx, "SELECT state FROM account_role_trade WHERE listing_id=?", listing).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return dspb.AccountRoleTradeState(state), true, nil
}

func (st *sqlDirectorySteps) readFence(ctx context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	return readFenceState(ctx, st.shards.forPlayer(player), player)
}

func (st *sqlDirectorySteps) storageAllowed(storage uint32) bool {
	return st.directory.stores.Allowed(storage)
}

// writeInitialRows 是建角第 2 步,见 upsertInitialPlayerStoreRows。
func (st *sqlDirectorySteps) writeInitialRows(ctx context.Context, storage uint32, role *base.AccountSimplePlayer) error {
	db, _, err := st.directory.stores.DB(ctx, storage)
	if err != nil {
		return err
	}
	if err := shardTx(ctx, db, func(tx *sql.Tx) error { return upsertInitialPlayerStoreRows(ctx, tx, role) }); err != nil {
		if errors.Is(err, accountdirectory.ErrConflict) {
			return err
		}
		return fmt.Errorf("提交玩家初始存档行: %w", err)
	}
	return nil
}
