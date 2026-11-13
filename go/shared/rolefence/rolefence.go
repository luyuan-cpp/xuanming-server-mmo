// Package rolefence 是每个服务库里的角色护栏副本,规格见 docs/design/horizontal-scale-logical-db.md §3。
//
// 目录 account_role 是唯一权威。服务不跨库读目录,只锁自己库里的副本,串行点从一行目录行变成每个服务各一行副本。
// 副本落后不要紧:缺行、冻结态、修订号落后都走同一条路,读权威状态、按版本合并、再试一次(Guard.RunTx)。
//
// 服务里的写法:
//
//	err := guard.RunTx(ctx, func() error {
//		return repo.tx(ctx, func(tx *sql.Tx) error {
//			fences, err := rolefence.LockAll(ctx, tx, rolefence.Expect{PlayerID: player, Account: account, OwnerRevision: revision, Reject: ErrDenied})
//			if err != nil {
//				return err
//			}
//			...
//		})
//	})
//
// 事务里不调 RPC。所有 RPC 都在 RunTx 的重试间隙,这时事务已经随 action 回滚。
package rolefence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	rfpb "proto/role_fence"
	"shared/accountdirectory"
)

// TableName 是副本表的表名,每个参与者的逻辑库一张。
const TableName = "role_fence"

// DDL 建副本表。account 空串表示已删除,freeze_token 0 表示未冻结,freeze_final 1 表示令牌是最终冻结、0 表示还在投票,home_zone 是原始区,不解析。
const DDL = "CREATE TABLE IF NOT EXISTS role_fence (" +
	"player_id BIGINT UNSIGNED NOT NULL," +
	"account VARBINARY(191) NOT NULL," +
	"owner_revision BIGINT UNSIGNED NOT NULL," +
	"freeze_token BIGINT UNSIGNED NOT NULL," +
	"freeze_final TINYINT(1) NOT NULL," +
	"version BIGINT UNSIGNED NOT NULL," +
	"home_zone INT UNSIGNED NOT NULL," +
	"updated_ms BIGINT UNSIGNED NOT NULL," +
	"PRIMARY KEY(player_id) /*T![clustered_index] NONCLUSTERED */" +
	") ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"

const readSQL = "SELECT account,owner_revision,freeze_token,freeze_final,version,home_zone FROM role_fence WHERE player_id=?"

const lockSQL = readSQL + " FOR UPDATE"

// applySQL 只在 version 更大时覆盖。version 必须排在最后:赋值按列出的顺序生效,排在它后面的 IF 会读到新值。
const applySQL = "INSERT INTO role_fence(player_id,account,owner_revision,freeze_token,freeze_final,version,home_zone,updated_ms) " +
	"VALUES (?,?,?,?,?,?,?,CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)) " +
	"ON DUPLICATE KEY UPDATE " +
	"account=IF(VALUES(version)>version,VALUES(account),account)," +
	"owner_revision=IF(VALUES(version)>version,VALUES(owner_revision),owner_revision)," +
	"freeze_token=IF(VALUES(version)>version,VALUES(freeze_token),freeze_token)," +
	"freeze_final=IF(VALUES(version)>version,VALUES(freeze_final),freeze_final)," +
	"home_zone=IF(VALUES(version)>version,VALUES(home_zone),home_zone)," +
	"updated_ms=IF(VALUES(version)>version,VALUES(updated_ms),updated_ms)," +
	"version=IF(VALUES(version)>version,VALUES(version),version)"

var (
	// ErrMissing 表示本库没有这名角色的副本行。
	ErrMissing = errors.New("角色护栏副本不存在")
	// ErrRoleNotFound 表示目录里没有这名角色。Heal 不写副本,交给调用方判断。
	ErrRoleNotFound = errors.New("目录里没有这名角色")
	// ErrUnavailable 表示权威状态这一次读不到,可以重试。
	ErrUnavailable = errors.New("角色护栏权威状态暂不可读")
	// ErrInvalidState 表示护栏状态的字段不合法,不写副本。
	ErrInvalidState = errors.New("角色护栏状态非法")
	// ErrRejected 是 Expect 没填 Reject 时的拒绝错误。
	ErrRejected = errors.New("角色护栏拒绝")
)

// removalTokenBit 是删角令牌的最高位。发号器只发 63 位,listing id 不会撞上。
const removalTokenBit = uint64(1) << 63

// RemovalToken 是删角用的冻结令牌。
func RemovalToken(player uint64) uint64 { return removalTokenBit | player }

// IsRemovalToken 判断令牌是不是删角令牌。
func IsRemovalToken(token uint64) bool { return token&removalTokenBit != 0 }

// Fence 是本库副本的一行。
type Fence struct {
	PlayerID      uint64
	Account       string
	OwnerRevision uint64
	FreezeToken   uint64
	// FreezeFinal 表示令牌是最终冻结。false 且令牌非 0 是还在投票的准备令牌,撤销了角色就回来了:
	// 业务写入不分这两种,都拒绝;只有不可逆的判定(退款)要认它,准备令牌回可重试。
	FreezeFinal bool
	Version     uint64
	HomeZone    uint32
}

// ValidState 核护栏状态的字段。
func ValidState(state *rfpb.RoleFenceState) bool {
	if state == nil || state.PlayerId == 0 || state.OwnerRevision == 0 || state.Version == 0 || state.HomeZone == 0 {
		return false
	}
	if state.FreezeFinal && state.FreezeToken == 0 {
		return false
	}
	if state.Account == "" {
		return state.FreezeToken == 0
	}
	return accountdirectory.ValidAccount(state.Account)
}

// Lock 锁住本库的副本行(SELECT ... FOR UPDATE),取代 account_role 上的 FOR UPDATE。
// 没有副本行时回 ErrMissing,此时 Fence 只有 PlayerID。
func Lock(ctx context.Context, tx *sql.Tx, player uint64) (Fence, error) {
	return scanFence(tx.QueryRowContext(ctx, lockSQL, player), player)
}

// Read 不加锁读副本行,只给事务外的预检用。提交点仍然要 Lock。
func Read(ctx context.Context, db *sql.DB, player uint64) (Fence, error) {
	return scanFence(db.QueryRowContext(ctx, readSQL, player), player)
}

func scanFence(row *sql.Row, player uint64) (Fence, error) {
	fence := Fence{PlayerID: player}
	var account []byte
	err := row.Scan(&account, &fence.OwnerRevision, &fence.FreezeToken, &fence.FreezeFinal, &fence.Version, &fence.HomeZone)
	if errors.Is(err, sql.ErrNoRows) {
		return fence, ErrMissing
	}
	if err != nil {
		return fence, err
	}
	fence.Account = string(account)
	if !ValidState(&rfpb.RoleFenceState{PlayerId: player, Account: fence.Account, OwnerRevision: fence.OwnerRevision, FreezeToken: fence.FreezeToken, FreezeFinal: fence.FreezeFinal, Version: fence.Version, HomeZone: fence.HomeZone}) {
		return fence, fmt.Errorf("角色护栏副本损坏: player=%d", player)
	}
	return fence, nil
}

// Execer 是 *sql.DB 与 *sql.Tx 共有的写接口。
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Apply 写入副本,只有 version 更大才覆盖,所以推送乱序、重复、丢失都没有影响。applied 表示这一条真的改了副本。
func Apply(ctx context.Context, execer Execer, state *rfpb.RoleFenceState) (applied bool, err error) {
	if !ValidState(state) {
		return false, ErrInvalidState
	}
	result, err := execer.ExecContext(ctx, applySQL, state.PlayerId, []byte(state.Account), state.OwnerRevision, state.FreezeToken, state.FreezeFinal, state.Version, state.HomeZone)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected != 0, nil
}

// StaleError 表示本库副本判不了,要读权威状态。action 把它原样返回,事务回滚之后由 Guard.RunTx 自愈并重试一次。
type StaleError struct {
	Players []uint64
	// Reject 是自愈之后还不过时回给调用方的错误。
	Reject error
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("角色护栏副本要自愈: %d 名角色", len(e.Players))
}

func (e *StaleError) rejection() error {
	if e.Reject != nil {
		return e.Reject
	}
	return ErrRejected
}

// Expect 是对一名角色的放行条件。
type Expect struct {
	PlayerID uint64
	// Account、OwnerRevision 是会话或持久合同捕获的原身份。OwnerRevision 为 0 表示调用方没有原身份(对端、目标),只判冻结和删除。
	Account       string
	OwnerRevision uint64
	// AllowToken 是这笔业务自己持有的冻结令牌(trade 的人民币寄售),令牌等于它时不算冻结。
	AllowToken uint64
	// Reject 是服务原来的拒绝错误:确定拒绝、自愈后还不过都回它。
	Reject error
}

func (e Expect) rejection() error {
	if e.Reject != nil {
		return e.Reject
	}
	return ErrRejected
}

func (e Expect) stale() error {
	return &StaleError{Players: []uint64{e.PlayerID}, Reject: e.Reject}
}

// hasOriginal 报告 expect 是否带完整的原身份(角色、账号、修订号)。归属预检只认完整的原身份。
func (e Expect) hasOriginal() bool {
	return e.PlayerID != 0 && e.Account != "" && e.OwnerRevision != 0
}

// Check 判定 Lock 的结果。回 nil 是放行;回 *StaleError 是该自愈;其余是拒绝或 SQL 错误,原样往外返回。
//
// 过户之后旧主的会话:副本修订号比会话大,直接拒绝,不自愈。新主的会话:副本修订号比会话小,自愈后通过。
// 副本冻结时也要自愈,可能只是解冻推送丢了;真冻结的话自愈之后还是冻结,第二次回 Reject。
// 令牌不分准备还是最终:投票期间业务写入也要拦,否则 Prepare 读完挂起之后还能新增。
func Check(fence Fence, lockErr error, expect Expect) error {
	if errors.Is(lockErr, ErrMissing) {
		return expect.stale()
	}
	if lockErr != nil {
		return lockErr
	}
	if expect.OwnerRevision != 0 {
		if fence.OwnerRevision > expect.OwnerRevision {
			return expect.rejection()
		}
		if fence.OwnerRevision < expect.OwnerRevision {
			return expect.stale()
		}
		if fence.Account != expect.Account {
			return expect.rejection()
		}
	}
	if fence.FreezeToken != 0 && fence.FreezeToken != expect.AllowToken {
		return expect.stale()
	}
	if fence.Account == "" {
		return expect.rejection()
	}
	return nil
}

// CheckOwner 判 Read 的结果里角色是否仍归 expect 的原身份,只给只读入口的归属预检用(Guard.VerifyOwner)。
// 回 nil 是仍归它;回 *StaleError 是该自愈;其余是拒绝或 SQL 错误,原样往外返回。expect 必须带完整原身份,不用 AllowToken。
//
// 与 Check 只差一处:不看冻结。冻结期间(公示、投票、删角待确认)角色仍归原账号,读自己的数据不必拦;
// 写在提交点由 Check 持锁拦。冻结的副本可能停在过户之前,这一点由 Guard.VerifyOwner 先自愈再判,不在这里处理。
func CheckOwner(fence Fence, readErr error, expect Expect) error {
	if !expect.hasOriginal() {
		return expect.rejection()
	}
	if errors.Is(readErr, ErrMissing) {
		return expect.stale()
	}
	if readErr != nil {
		return readErr
	}
	if fence.OwnerRevision > expect.OwnerRevision {
		return expect.rejection()
	}
	if fence.OwnerRevision < expect.OwnerRevision {
		return expect.stale()
	}
	// 修订号相同而账号不同:角色已删(账号清空),或这份原身份本来就不是这名角色的。
	if fence.Account != expect.Account {
		return expect.rejection()
	}
	return nil
}

// LockAll 按 player_id 升序锁全部副本并逐个 Check,回本次锁到的副本。升序是所有服务共同的锁序,要先于业务行。
//
// 该自愈的角色一次收齐,否则一个事务里第二名角色还要再自愈一次,就被 RunTx 当成真实拒绝了。
// 确定拒绝立刻返回。同一名角色可以出现多次(actor 同时是目标),锁一次,每个条件都判。
func LockAll(ctx context.Context, tx *sql.Tx, expects ...Expect) (map[uint64]Fence, error) {
	ordered := append([]Expect(nil), expects...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].PlayerID < ordered[j].PlayerID })
	fences := make(map[uint64]Fence, len(ordered))
	var pending *StaleError
	var fence Fence
	var lockErr error
	for i, expect := range ordered {
		if expect.PlayerID == 0 {
			return nil, expect.rejection()
		}
		fresh := i == 0 || expect.PlayerID != ordered[i-1].PlayerID
		if fresh {
			fence, lockErr = Lock(ctx, tx, expect.PlayerID)
		}
		err := Check(fence, lockErr, expect)
		var stale *StaleError
		switch {
		case errors.As(err, &stale):
			if pending == nil {
				pending = stale
			} else if pending.Players[len(pending.Players)-1] != expect.PlayerID {
				pending.Players = append(pending.Players, expect.PlayerID)
			}
		case err != nil:
			return nil, err
		default:
			fences[expect.PlayerID] = fence
		}
	}
	if pending != nil {
		return nil, pending
	}
	return fences, nil
}

// EnsureSchema 建副本表并核形状,可重复执行。给各服务的迁移入口用,db 要有建表权限。
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("角色护栏连接未配置")
	}
	if _, err := db.ExecContext(ctx, DDL); err != nil {
		return err
	}
	return VerifySchema(ctx, db)
}

// VerifySchema 只读核本库 role_fence 的形状,不建表、不补列。给启动自检用,写者账号也能跑。
func VerifySchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("角色护栏连接未配置")
	}
	columns, err := db.QueryContext(ctx, "SELECT player_id,account,owner_revision,freeze_token,freeze_final,version,home_zone,updated_ms FROM role_fence LIMIT 0")
	if err != nil {
		return err
	}
	if err = columns.Close(); err != nil {
		return err
	}
	var dataType string
	var width sql.NullInt64
	if err = db.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='role_fence' AND COLUMN_NAME='account'").Scan(&dataType, &width); err != nil {
		return err
	}
	if dataType != "varbinary" || !width.Valid || width.Int64 != 191 {
		return errors.New("角色护栏账号列必须逐字节比较")
	}
	keys, err := db.QueryContext(ctx, "SELECT COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='role_fence' AND INDEX_NAME='PRIMARY' ORDER BY SEQ_IN_INDEX")
	if err != nil {
		return err
	}
	defer keys.Close()
	var primaryKey []string
	for keys.Next() {
		var column string
		if err = keys.Scan(&column); err != nil {
			return err
		}
		primaryKey = append(primaryKey, column)
	}
	if err = keys.Err(); err != nil {
		return err
	}
	if len(primaryKey) != 1 || primaryKey[0] != "player_id" {
		return errors.New("角色护栏主键必须是 player_id")
	}
	return nil
}
