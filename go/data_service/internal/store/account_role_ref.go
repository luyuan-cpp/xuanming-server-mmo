package store

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 账号到角色的索引,放账号所在分片(docs/design/horizontal-scale-logical-db.md §5.2)。
//
// 不变量:ref 集合包含全部归属真相。真相永远是玩家分片的 account_role.account,ref 只是索引:读列表时按真相过滤。
// 所以写 ref 必须先于提交点,删 ref 必须能证明这个账号不会再因为在途的写拥有这名角色。
//
// 证明按 ref 的状态分三种,都只在读完 ref 之后读真相:
//   - ACTIVE:只有提交点之后才会变成 ACTIVE。真相不是这个账号 = 已经易主或删除。
//   - PENDING 且 tx_id 是 0(建角):角色行已存在且不是这个账号,严格插入不会再成功。没有角色行时,超过 refPendingGrace 才写一条墓碑
//     挡住迟到的建角,然后才算陈旧。
//   - PENDING 且 tx_id 非 0(过户):提交要求 listing 是 FROZEN,而 FROZEN 只会走到 TRANSFERRED 或 CANCELLED。
//     listing 到了这两个终态、真相又不是这个账号,这笔过户就不会再提交。还在 FROZEN 不算陈旧。
//     这一种要先读 listing、后读真相,见 reapRef。
//
// 重新占用(过户第 1 步)可能发生在证明之后,所以删除按 nonce 比较:任何写都换 nonce,读到的 nonce 不再是当前值就什么都不做。
//
// 还有一种不需要证明的删除:建角在提交点之前失败,删掉这次尝试自己写的那条 ref,立刻退还名额,见 releaseOwnRef。

const (
	// AccountRoleRefTableName 是索引表。
	AccountRoleRefTableName = "account_role_ref"

	accountRoleRefDDL = "CREATE TABLE IF NOT EXISTS account_role_ref (account VARBINARY(191) NOT NULL COMMENT 'pb:1',player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',state INT UNSIGNED NOT NULL COMMENT 'pb:3',tx_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:4',listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:5',home_zone INT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:6',nonce BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',created_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:8',PRIMARY KEY(account,player_id) /*T![clustered_index] NONCLUSTERED */,INDEX idx_account_role_ref_state(state,created_ms)) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"

	refStatePending uint32 = 1
	refStateActive  uint32 = 2

	// refPendingGrace 是建角留下的 PENDING 多久之后才能写墓碑判陈旧。远大于 login 一次建角的预算,所以迟到的建角只可能是挂住的请求。
	refPendingGrace = 5 * time.Minute

	// maxRefRows 是一个账号的 ref 硬上限,和角色数配置上限相同:每条 ref 都要过上限判定才写得进来。
	maxRefRows = 100

	// staleRefQueueSize 是读路径发现的陈旧 ref 排队上限。满了就丢,下一次读还会发现。
	staleRefQueueSize = 1024

	// refReleaseTimeout 是退还名额那一次删除的预算。走到那里时请求预算可能已经见底,不跟请求 ctx;
	// 也不能拖太久,否则 login 等不到「确定没提交」,只能按结局未知处理。
	refReleaseTimeout = 500 * time.Millisecond
)

// roleRef 是 account_role_ref 的一行。
type roleRef struct {
	Account   string
	PlayerID  uint64
	State     uint32
	TxID      uint64
	ListingID uint64
	HomeZone  uint32
	Nonce     uint64
	// AgeMs 由同一条语句里的库时钟算出,不依赖本机时钟。
	AgeMs uint64
}

// staleRefKey 是读路径发现的陈旧 ref,等后台清。
type staleRefKey struct {
	account string
	player  uint64
}

func newRefNonce() uint64 { return rand.Uint64() >> 1 }

// noteStaleRef 把读路径发现的陈旧 ref 交给后台。不阻塞:这是登录的读路径。
func (s *AccountDirectoryStore) noteStaleRef(account string, player uint64) {
	select {
	case s.staleRefs <- staleRefKey{account: account, player: player}:
	default:
	}
}

// reserveRefWithEviction 是建角第 1 步和过户第 1 步:写 PENDING ref。到上限时先删被证明陈旧的 ref,再判。
// 跨片的证明读在事务外,所以这里是两轮事务,不是一个。
func (s *AccountDirectoryStore) reserveRefWithEviction(ctx context.Context, account string, want roleRef) error {
	for attempt := 0; attempt < 2; attempt++ {
		full, err := s.steps.reserveRef(ctx, account, want, s.maxRoles)
		if err != nil {
			return err
		}
		if full == nil {
			return nil
		}
		dropped, err := s.reapRefs(ctx, account, full)
		if dropped == 0 {
			if err != nil {
				// 有的证明读失败了:不能说是满了,玩家重试可能就过了。
				return err
			}
			return accountdirectory.ErrFull
		}
	}
	return accountdirectory.ErrFull
}

// reapRefs 对一组 ref 逐个判陈旧并删除,回删掉的条数。一条失败不影响别的。
func (s *AccountDirectoryStore) reapRefs(ctx context.Context, account string, refs []roleRef) (int, error) {
	dropped := 0
	var failure error
	for _, ref := range refs {
		ok, err := s.reapRef(ctx, account, ref)
		if err != nil {
			failure = errors.Join(failure, err)
			continue
		}
		if ok {
			dropped++
		}
	}
	return dropped, failure
}

// reapRef 判这条 ref 陈旧就删掉,回是否删了。真相就是这个账号时补上没做完的 ACTIVE。证明见文件头。
//
// 过户的 PENDING 要先读 listing、后读真相。反过来的话:读到真相是卖家,这时过户提交,再读到 listing 已是 TRANSFERRED,就把买家的 ref
// 当陈旧删了。买家拥有角色却没有 ref,列表里看不到,也没有任何路径能再发现。先读 listing 的话,读到终态就说明这笔过户的提交已经在
// 这之前,接着读到的真相一定盖住了它;读到 FROZEN 就不会删。
func (s *AccountDirectoryStore) reapRef(ctx context.Context, account string, ref roleRef) (bool, error) {
	var listingFound, listingSettled bool
	if ref.State == refStatePending && ref.TxID != 0 {
		state, found, err := s.steps.readListingState(ctx, ref.PlayerID, ref.ListingID)
		if err != nil {
			return false, err
		}
		listingFound = found
		listingSettled = state == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED || state == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED
	}
	owned, err := s.steps.readOwner(ctx, ref.PlayerID)
	if err != nil {
		return false, err
	}
	if owned != nil && owned.Account == account {
		if ref.State == refStatePending {
			return false, s.steps.promoteRef(ctx, account, ref)
		}
		return false, nil
	}
	switch {
	case ref.State == refStateActive:
		if owned == nil {
			// ACTIVE 只在提交点之后写,角色行又从不删除。库被人动过,留给人看。
			logx.Errorf("[account-role-ref] active ref without a role row: player=%d", ref.PlayerID)
			return false, nil
		}
	case ref.TxID != 0:
		if !listingFound {
			// 第 1 步之前核过 listing 是 FROZEN,FROZEN 的收据不会删。库被人动过,留给人看。
			logx.Errorf("[account-role-ref] pending transfer ref without its listing: player=%d listing=%d", ref.PlayerID, ref.ListingID)
			return false, nil
		}
		if !listingSettled {
			return false, nil
		}
	case owned == nil:
		if ref.AgeMs < uint64(refPendingGrace.Milliseconds()) {
			return false, nil
		}
		won, err := s.steps.tombstoneMissingRole(ctx, account, ref)
		if err != nil {
			return false, err
		}
		if won {
			// 迟到的建角先提交了,真相就是这个账号。
			return false, s.steps.promoteRef(ctx, account, ref)
		}
	}
	return s.steps.dropRef(ctx, account, ref)
}

// reapOwnRef 读这条 ref 再判。过户提交之后删卖家 ref、删角之后删 ref 都走它,失败无害:读路径会过滤,后台会再清。
func (s *AccountDirectoryStore) reapOwnRef(ctx context.Context, account string, player uint64) {
	ref, err := s.steps.readRef(ctx, account, player)
	if err == nil && ref != nil {
		_, err = s.reapRef(ctx, account, *ref)
	}
	if err != nil {
		logx.Errorf("[account-role-ref] reap failed, the background sweep retries: player=%d: %v", player, err)
	}
}

// releaseOwnRef 在建角提交点之前失败时退还名额:这次尝试不会再提交,它自己写的 ref 可以直接删,不用证明。
// nonce 是这次尝试生成的:ref 是上一次尝试留下的话对不上,什么都不做,留给后台判陈旧。第 1 步结局未知时也调,写没写成都对。
// 失败无害:ref 占着名额直到后台清掉。
func (s *AccountDirectoryStore) releaseOwnRef(ctx context.Context, account string, ref roleRef) {
	release, cancel := context.WithTimeout(context.WithoutCancel(ctx), refReleaseTimeout)
	defer cancel()
	if _, err := s.steps.dropRef(release, account, ref); err != nil {
		logx.Errorf("[account-role-ref] release failed, the background sweep retries: player=%d: %v", ref.PlayerID, err)
	}
}

// SweepResult 是一轮后台清理的结果。
type SweepResult struct {
	Scanned, Dropped, Failed int
}

// SweepStaleRefs 清陈旧 ref:读路径发现的,加上超过 olderThan 还是 PENDING 的。流程和上限判定用的是同一个证明,多副本同时跑靠 nonce 互斥。
func (s *AccountDirectoryStore) SweepStaleRefs(ctx context.Context, olderThan time.Duration, limit int) (SweepResult, error) {
	var out SweepResult
	var failure error
	reap := func(account string, ref roleRef) {
		out.Scanned++
		dropped, err := s.reapRef(ctx, account, ref)
		switch {
		case err != nil:
			out.Failed++
			failure = errors.Join(failure, err)
		case dropped:
			out.Dropped++
		}
	}
drain:
	for out.Scanned < limit {
		select {
		case key := <-s.staleRefs:
			ref, err := s.steps.readRef(ctx, key.account, key.player)
			if err != nil {
				out.Failed++
				failure = errors.Join(failure, err)
				continue
			}
			if ref != nil {
				reap(key.account, *ref)
			}
		default:
			break drain
		}
	}
	if out.Scanned >= limit {
		return out, failure
	}
	refs, err := s.steps.scanPendingRefs(ctx, olderThan, limit-out.Scanned)
	if err != nil {
		return out, errors.Join(failure, err)
	}
	for _, ref := range refs {
		reap(ref.Account, ref)
	}
	return out, failure
}

// ── SQL ─────────────────────────────────────────────────────────────────────

const refColumns = "player_id,state,tx_id,listing_id,home_zone,nonce,created_ms," + dbNowMsSQL

func scanRef(row interface{ Scan(...any) error }, account string) (roleRef, error) {
	ref := roleRef{Account: account}
	var createdMs, nowMs uint64
	if err := row.Scan(&ref.PlayerID, &ref.State, &ref.TxID, &ref.ListingID, &ref.HomeZone, &ref.Nonce, &createdMs, &nowMs); err != nil {
		return roleRef{}, err
	}
	if nowMs > createdMs {
		ref.AgeMs = nowMs - createdMs
	}
	if ref.PlayerID == 0 || (ref.State != refStatePending && ref.State != refStateActive) {
		return roleRef{}, errors.New("账号角色索引原件损坏")
	}
	return ref, nil
}

// readRefs 读一个账号的 ref,按 player_id 升序。超过硬上限报错,不截断:截断的列表会漏掉真实角色。
func readRefs(ctx context.Context, q directoryQuery, account string) ([]roleRef, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+refColumns+" FROM account_role_ref WHERE account=? ORDER BY player_id LIMIT ?", []byte(account), maxRefRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []roleRef
	for rows.Next() {
		ref, err := scanRef(rows, account)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(refs) > maxRefRows {
		return nil, errors.New("账号角色目录超过硬上限")
	}
	return refs, nil
}

// readRef 读一条 ref。没有回 nil。
func (st *sqlDirectorySteps) readRef(ctx context.Context, account string, player uint64) (*roleRef, error) {
	ref, err := scanRef(st.shards.forAccount(account).QueryRowContext(ctx, "SELECT "+refColumns+" FROM account_role_ref WHERE account=? AND player_id=?", []byte(account), player), account)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// errRefsFull 只在本文件内流转:到上限了,事务回滚,把读到的 ref 交给调用方判陈旧。
var errRefsFull = errors.New("账号角色索引已到上限")

// reserveRef 在账号分片的一个事务里:锁目录行,核上限,写 PENDING。回 full 非 nil 表示到上限了,什么都没写。
// want.Nonce 由调用方生成:这样写没写成都能按它删自己写的那条(releaseOwnRef)。
//
// 已有这条 ref 时不占新名额。建角的重放不动它;过户不管原来是什么状态都改写成这笔过户并换 nonce,挡住在途的清理。
func (st *sqlDirectorySteps) reserveRef(ctx context.Context, account string, want roleRef, maxRoles int) ([]roleRef, error) {
	var full []roleRef
	err := shardTx(ctx, st.shards.forAccount(account), func(tx *sql.Tx) error {
		if err := lockDirectory(ctx, tx, account, true); err != nil {
			return err
		}
		refs, err := readRefs(ctx, tx, account)
		if err != nil {
			return err
		}
		existing := slices.ContainsFunc(refs, func(ref roleRef) bool { return ref.PlayerID == want.PlayerID })
		if existing && want.TxID == 0 {
			return nil
		}
		if !existing && len(refs) >= maxRoles {
			full = refs
			if full == nil {
				full = []roleRef{}
			}
			return errRefsFull
		}
		return writePendingRef(ctx, tx, account, want)
	})
	if errors.Is(err, errRefsFull) {
		return full, nil
	}
	return nil, err
}

// writePendingRefSQL 用 upsert,不用先读后 UPDATE:后台的 dropRef 不持目录行锁,可能在读完 ref 之后抢先删掉它。
// UPDATE 这时影响 0 行还回成功,过户就在没有 ref 的情况下提交了。upsert 把它写回来,名额不变:读到它时已经算在里面。
const writePendingRefSQL = "INSERT INTO account_role_ref(account,player_id,state,tx_id,listing_id,home_zone,nonce,created_ms) VALUES (?,?,?,?,?,?,?," + dbNowMsSQL + ") " +
	"ON DUPLICATE KEY UPDATE state=VALUES(state),tx_id=VALUES(tx_id),listing_id=VALUES(listing_id),home_zone=VALUES(home_zone),nonce=VALUES(nonce),created_ms=VALUES(created_ms)"

func writePendingRef(ctx context.Context, tx *sql.Tx, account string, want roleRef) error {
	_, err := tx.ExecContext(ctx, writePendingRefSQL, []byte(account), want.PlayerID, refStatePending, want.TxID, want.ListingID, want.HomeZone, want.Nonce)
	return err
}

// activateRefSQL 写 ACTIVE。已有的行只在没有在途过户(tx_id 是 0)或者属于本笔过户时才改:
// 别的过户第 1 步可能刚把它改写成 PENDING,覆盖成 ACTIVE 的话,后台会按「ACTIVE 且真相不是这个账号」把它删掉,那笔过户提交之后买家就没有 ref 了。
// 没改到不算错:那笔过户提交之后自己会写 ACTIVE,判陈旧时也会补。
//
// tx_id 放在最后赋值:ON DUPLICATE KEY UPDATE 按从左到右算,前面的条件要看到改之前的值。
const activateRefSQL = "INSERT INTO account_role_ref(account,player_id,state,tx_id,listing_id,home_zone,nonce,created_ms) VALUES (?,?,?,0,0,0,?," + dbNowMsSQL + ") " +
	"ON DUPLICATE KEY UPDATE state=IF(tx_id IN (0,?),VALUES(state),state),listing_id=IF(tx_id IN (0,?),0,listing_id),nonce=IF(tx_id IN (0,?),VALUES(nonce),nonce),tx_id=IF(tx_id IN (0,?),0,tx_id)"

// activateRef 在确认真相就是这个账号之后写 ACTIVE。行没了就补:少一条 ref 这个角色就从列表里消失。txID 是这笔过户,建角传 0。
func (st *sqlDirectorySteps) activateRef(ctx context.Context, account string, player, txID uint64) error {
	_, err := st.shards.forAccount(account).ExecContext(ctx, activateRefSQL, []byte(account), player, refStateActive, newRefNonce(), txID, txID, txID, txID)
	return err
}

// promoteRef 把读到的 PENDING 改成 ACTIVE。nonce 变了说明又有新的写,不动。
func (st *sqlDirectorySteps) promoteRef(ctx context.Context, account string, ref roleRef) error {
	_, err := st.shards.forAccount(account).ExecContext(ctx, "UPDATE account_role_ref SET state=?,tx_id=0,listing_id=0,nonce=? WHERE account=? AND player_id=? AND nonce=?",
		refStateActive, newRefNonce(), []byte(account), ref.PlayerID, ref.Nonce)
	return err
}

// dropRef 按 nonce 比较并删除,回是否删了。
func (st *sqlDirectorySteps) dropRef(ctx context.Context, account string, ref roleRef) (bool, error) {
	result, err := st.shards.forAccount(account).ExecContext(ctx, "DELETE FROM account_role_ref WHERE account=? AND player_id=? AND nonce=?", []byte(account), ref.PlayerID, ref.Nonce)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// scanPendingRefs 逐片找超时的 PENDING。从上一轮停下的片接着走,积压多的片不会饿死别的片。
func (st *sqlDirectorySteps) scanPendingRefs(ctx context.Context, olderThan time.Duration, limit int) ([]roleRef, error) {
	var out []roleRef
	k := st.shards.k
	start := int(st.sweepCursor.Add(1)) % k
	for i := 0; i < k && len(out) < limit; i++ {
		rows, err := st.shards.pools[(start+i)%k].QueryContext(ctx, "SELECT account,"+refColumns+" FROM account_role_ref WHERE state=? AND created_ms+?<"+dbNowMsSQL+" ORDER BY created_ms LIMIT ?",
			refStatePending, olderThan.Milliseconds(), limit-len(out))
		if err != nil {
			return out, err
		}
		found, err := scanAccountRefs(rows)
		if err != nil {
			return out, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func scanAccountRefs(rows *sql.Rows) ([]roleRef, error) {
	defer rows.Close()
	var out []roleRef
	for rows.Next() {
		ref := roleRef{}
		var account []byte
		var createdMs, nowMs uint64
		if err := rows.Scan(&account, &ref.PlayerID, &ref.State, &ref.TxID, &ref.ListingID, &ref.HomeZone, &ref.Nonce, &createdMs, &nowMs); err != nil {
			return nil, err
		}
		ref.Account = string(account)
		if nowMs > createdMs {
			ref.AgeMs = nowMs - createdMs
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// tombstoneMissingRole 在玩家分片的事务里:角色行还没有就写墓碑,迟到的建角从此撞上已有行被拒。回 won 表示这个账号的建角已经先提交了。
func (st *sqlDirectorySteps) tombstoneMissingRole(ctx context.Context, account string, ref roleRef) (won bool, err error) {
	if ref.HomeZone == 0 {
		// 建角的 ref 都带 home_zone,没有就写不出合法的护栏状态。
		return false, errors.New("建角索引缺归属区")
	}
	role, err := proto.Marshal(&base.AccountSimplePlayer{PlayerId: ref.PlayerID, ZoneId: ref.HomeZone})
	if err != nil {
		return false, err
	}
	err = shardTx(ctx, st.shards.forPlayer(ref.PlayerID), func(tx *sql.Tx) error {
		old, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, ref.PlayerID), ref.PlayerID)
		if err == nil {
			won = old.Account == account
			return nil
		}
		if !errors.Is(err, accountdirectory.ErrMissing) {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO account_role(player_id,account,role_proto,owner_revision,updated_ms,initial_player_commit,fence_version) VALUES (?,?,?,1,"+dbNowMsSQL+",0,1)", ref.PlayerID, []byte{}, role)
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == mysqlErrDupEntry {
			// 建角抢先了。回滚这个事务,下一轮再判。
			return accountdirectory.ErrUnavailable
		}
		return err
	})
	return won, err
}
