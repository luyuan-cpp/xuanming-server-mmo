package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/rolefence"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

const AccountRoleTradeTableName = "account_role_trade"
const AccountRoleTransferTableName = "account_role_transfer_receipt"
const accountRoleTradeDDL = "CREATE TABLE IF NOT EXISTS account_role_trade (listing_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',seller_account VARBINARY(191) NOT NULL COMMENT 'pb:3',seller_owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',request_digest BINARY(32) NOT NULL COMMENT 'pb:5',state INT UNSIGNED NOT NULL COMMENT 'pb:6',freeze_evidence MEDIUMBLOB NOT NULL COMMENT 'pb:7',created_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:8',updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:9',required_owner_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:10',required_save_seq BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:11',PRIMARY KEY(listing_id) /*T![clustered_index] NONCLUSTERED */,INDEX idx_account_role_trade_state(state,created_ms)) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"
const accountRoleTransferDDL = "CREATE TABLE IF NOT EXISTS account_role_transfer_receipt (tx_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',request_digest BINARY(32) NOT NULL COMMENT 'pb:2',listing_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:3',player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',seller_account VARBINARY(191) NOT NULL COMMENT 'pb:5',buyer_account VARBINARY(191) NOT NULL COMMENT 'pb:6',seller_owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',buyer_owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:8',created_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:9',role_proto MEDIUMBLOB NOT NULL COMMENT 'pb:10',PRIMARY KEY(tx_id) /*T![clustered_index] NONCLUSTERED */) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"

// 只补本批已确认语义的新列；其余漂移显式拒绝，不把同名错误类型静默当成可用。
func ensureAccountRoleTradeSchema(ctx context.Context, db *sql.DB, name string) error {
	for n, column := range []string{"required_owner_epoch", "required_save_seq"} {
		number := 10 + n
		var exists int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role_trade' AND COLUMN_NAME=?", name, column).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := db.ExecContext(ctx, "ALTER TABLE account_role_trade ADD COLUMN "+column+" BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:"+strconv.Itoa(number)+"'"); err != nil {
				return err
			}
		}
		var dataType, columnType, nullable string
		if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,COLUMN_TYPE,IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role_trade' AND COLUMN_NAME=?", name, column).Scan(&dataType, &columnType, &nullable); err != nil {
			return err
		}
		if dataType != "bigint" || !strings.Contains(columnType, "unsigned") || nullable != "NO" {
			return errors.New("角色交易 SQL 下限列形状不符")
		}
	}
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role' AND COLUMN_NAME='trade_listing_id'", name).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := db.ExecContext(ctx, "ALTER TABLE account_role ADD COLUMN trade_listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:6'"); err != nil {
			return err
		}
	}
	var listingDataType, listingType, listingNullable string
	if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,COLUMN_TYPE,IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='account_role' AND COLUMN_NAME='trade_listing_id'", name).Scan(&listingDataType, &listingType, &listingNullable); err != nil {
		return err
	}
	if listingDataType != "bigint" || !strings.Contains(listingType, "unsigned") || listingNullable != "NO" {
		return errors.New("角色交易锁列形状不符")
	}
	for _, model := range []proto.Message{&dbpb.AccountRoleTrade{}, &dbpb.AccountRoleTransferReceipt{}} {
		table, key := AccountRoleTradeTableName, "listing_id"
		if _, ok := model.(*dbpb.AccountRoleTransferReceipt); ok {
			table, key = AccountRoleTransferTableName, "tx_id"
		}
		if err := assertColumnsPresent(ctx, db, name, model, table); err != nil {
			return err
		}
		keys, err := primaryKeyColumns(ctx, db, name, table)
		if err != nil {
			return err
		}
		if len(keys) != 1 || keys[0] != key {
			return errors.New("角色交易收据主键不符")
		}
		columns := []string{"seller_account"}
		if table == AccountRoleTransferTableName {
			columns = append(columns, "buyer_account")
		}
		for _, column := range columns {
			var typ string
			var width sql.NullInt64
			if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?", name, table, column).Scan(&typ, &width); err != nil {
				return err
			}
			if typ != "varbinary" || !width.Valid || width.Int64 != 191 {
				return errors.New("角色交易收据账号列必须逐字节比较")
			}
		}
		var digestType string
		var digestWidth sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME='request_digest'", name, table).Scan(&digestType, &digestWidth); err != nil {
			return err
		}
		if digestType != "binary" || !digestWidth.Valid || digestWidth.Int64 != 32 {
			return errors.New("角色交易收据摘要列形状不符")
		}
	}
	return ensureRoleAdmissionSchema(ctx, db, name)
}

// RoleFreezeVerifier 在目录事务外读存档证据,规格 §6。实现不能接受 caller 自报离线。
// 生产仅在显式 TiDB/存储白名单配置后装配;缺配置不能产生 FROZEN。
type RoleFreezeVerifier interface {
	// ReadFrozenEvidence 读证据候选。调用方已核过这一轮的持有者全部排空,holders 是持有者数。
	ReadFrozenEvidence(ctx context.Context, r *dbpb.AccountRoleTrade, holders int) (*dspb.AccountRoleFreezeEvidence, error)
}

type AccountRoleTradeStore struct {
	directory *AccountDirectoryStore
	verifier  RoleFreezeVerifier
}

func NewAccountRoleTradeStore(directory *AccountDirectoryStore, verifier RoleFreezeVerifier) *AccountRoleTradeStore {
	return &AccountRoleTradeStore{directory: directory, verifier: verifier}
}

var _ accountdirectory.RoleTrades = (*AccountRoleTradeStore)(nil)

// listing id 只有 63 位,最高位留给删角令牌。
func roleTradeTupleValid(listing, player uint64, seller string, revision uint64) bool {
	return listing != 0 && !rolefence.IsRemovalToken(listing) && player != 0 && revision != 0 && accountdirectory.ValidAccount(seller)
}
func roleTradeDigest(in proto.Message) ([]byte, error) {
	hexDigest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, accountdirectory.ErrInvalid
	}
	return hex.DecodeString(hexDigest)
}

const roleTradeColumns = "player_id,seller_account,seller_owner_revision,request_digest,state,freeze_evidence,created_ms,updated_ms,required_owner_epoch,required_save_seq"

func scanRoleTrade(row interface{ Scan(...any) error }, listing uint64) (*dbpb.AccountRoleTrade, error) {
	r := &dbpb.AccountRoleTrade{ListingId: listing}
	err := row.Scan(&r.PlayerId, &r.SellerAccount, &r.SellerOwnerRevision, &r.RequestDigest, &r.State, &r.FreezeEvidence, &r.CreatedMs, &r.UpdatedMs, &r.RequiredOwnerEpoch, &r.RequiredSaveSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, accountdirectory.ErrMissing
	}
	if err != nil {
		return nil, err
	}
	if !roleTradeTupleValid(listing, r.PlayerId, r.SellerAccount, r.SellerOwnerRevision) || len(r.RequestDigest) != 32 || r.State < uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING) || r.State > uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING) || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
		return nil, errors.New("角色冻结收据损坏")
	}
	return r, nil
}
func readRoleTrade(ctx context.Context, tx *sql.Tx, listing uint64) (*dbpb.AccountRoleTrade, error) {
	return scanRoleTrade(tx.QueryRowContext(ctx, "SELECT "+roleTradeColumns+" FROM account_role_trade WHERE listing_id=? FOR UPDATE", listing), listing)
}

// readListing 普通读收据。ErrMissing 是没有这一行。
func (st *sqlDirectorySteps) readListing(ctx context.Context, player, listing uint64) (*dbpb.AccountRoleTrade, error) {
	return scanRoleTrade(st.shards.forPlayer(player).QueryRowContext(ctx, "SELECT "+roleTradeColumns+" FROM account_role_trade WHERE listing_id=?", listing), listing)
}
func matchesRoleTrade(r *dbpb.AccountRoleTrade, player uint64, seller string, revision uint64) bool {
	return r.PlayerId == player && r.SellerAccount == seller && r.SellerOwnerRevision == revision
}
func freezeEvidenceValid(e *dspb.AccountRoleFreezeEvidence, r *dbpb.AccountRoleTrade) bool {
	return accountdirectory.ValidFreezeEvidence(e) && e.PlayerId == r.PlayerId && e.ListingId == r.ListingId && e.OwnerRevision == r.SellerOwnerRevision && !saveFloorAfter(r.RequiredOwnerEpoch, r.RequiredSaveSeq, e.OwnerEpoch, e.SaveSeq)
}
func tradeReceiptWithHolders(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade) (*dspb.AccountRoleTradeReceipt, error) {
	out, err := tradeReceipt(r)
	if err != nil {
		return nil, err
	}
	out.Holders, err = readRoleTradeHolders(ctx, tx, r.ListingId)
	if err == nil {
		for _, h := range out.Holders {
			if h.Seal.PlayerId != r.PlayerId || h.Seal.OwnerRevision != r.SellerOwnerRevision || ((out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN || out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED || out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED) && !h.Drained) {
				return nil, errors.New("角色交易 holder 与原 listing 身份不符")
			}
		}
		if accountdirectory.NoActorFreezeEvidence(out.FreezeEvidence) && len(out.Holders) > 0 {
			return nil, errors.New("无入场证据不能带原生holder")
		}
	}
	return out, err
}
func tradeReceipt(r *dbpb.AccountRoleTrade) (*dspb.AccountRoleTradeReceipt, error) {
	out := &dspb.AccountRoleTradeReceipt{ListingId: r.ListingId, PlayerId: r.PlayerId, SellerAccount: r.SellerAccount, SellerOwnerRevision: r.SellerOwnerRevision, State: dspb.AccountRoleTradeState(r.State)}
	if len(r.FreezeEvidence) > 1024 {
		return nil, errors.New("角色冻结证据越界")
	}
	if len(r.FreezeEvidence) > 0 {
		e := &dspb.AccountRoleFreezeEvidence{}
		if err := proto.Unmarshal(r.FreezeEvidence, e); err != nil || !freezeEvidenceValid(e, r) {
			return nil, errors.New("角色冻结证据损坏")
		}
		out.FreezeEvidence = e
	}
	if (out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN || out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED) && out.FreezeEvidence == nil {
		return nil, errors.New("角色冻结收据缺证据")
	}
	return out, nil
}
func (s *AccountRoleTradeStore) ready() bool {
	return s != nil && s.directory != nil && s.directory.steps != nil
}

// insertRoleTrade 插入 listing 收据,已存在就只取行锁。Begin、Cancel 都从这里开始,锁序都是 listing 行到角色行。
func insertRoleTrade(ctx context.Context, tx *sql.Tx, listing, player uint64, seller string, revision uint64, digest []byte, state dspb.AccountRoleTradeState) (inserted bool, err error) {
	result, err := tx.ExecContext(ctx, "INSERT INTO account_role_trade(listing_id,player_id,seller_account,seller_owner_revision,request_digest,state,freeze_evidence,created_ms,updated_ms) VALUES (?,?,?,?,?,?,'',"+dbNowMsSQL+","+dbNowMsSQL+") ON DUPLICATE KEY UPDATE listing_id=listing_id", listing, player, []byte(seller), revision, digest, uint32(state))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected != 0, nil
}

// rolePrepareDeadline 是一轮准备的时限。投票期间参与者拒绝这名角色的业务写入,不能一直挂着:
// 重放时超过它还问不齐就撤销,没人重放的由后台撤销(AbortStalePreparing)。撤销总是安全的,版本加 1,在途的 Prepare 作废。
const rolePrepareDeadline = 30 * time.Second

// Begin 两阶段(规格 §3.5)。新建时先试探,有挂起回 ErrConflict,目录一个字都不写。
// 通过之后目录事务写 prepare_token 和 PREPARING;事务外让全部参与者冻结;全通过才写 trade_listing_id、进 FREEZING。
// 准备令牌不挡入场:转 FREEZING 的事务按角色行锁快照 holder,此后 RegisterRoleAdmission 才被挡。
//
// 真正的 Prepare 有挂起回 ErrConflict,不留痕。参与者不可达或版本过时回 ErrRetryLater,令牌留着,用同一 listing 重放接着做;
// 收据超过 rolePrepareDeadline 还是这样就撤销,回 ErrRetryLater:下一次重放从试探重来,不再写令牌。
// 问不齐时这次调用写过的只有第 1 步,提交已确认,没有结局未知,所以不回 ErrUnavailable:那会把排空证明打成 Hold(规格 §3.6)。
func (s *AccountRoleTradeStore) Begin(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if !s.ready() || s.directory.fences == nil {
		return nil, accountdirectory.ErrRetryLater
	}
	if in == nil || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(in)
	if err != nil {
		return nil, err
	}
	if err := s.probeBegin(ctx, in); err != nil {
		return nil, err
	}
	state, expired, replay, err := s.beginPrepare(ctx, in, digest)
	if err != nil {
		return nil, err
	}
	if replay {
		return s.replayRoleTrade(ctx, in.ListingId, in.PlayerId)
	}
	switch s.directory.fences.Prepare(ctx, state) {
	case rolefence.PrepareClean:
		return s.finishPrepare(ctx, in, digest, state)
	case rolefence.PrepareBlocked:
		return s.abortPrepare(ctx, in, digest, state, accountdirectory.ErrConflict)
	default:
		if expired {
			logx.Errorf("[account-role-trade] prepare exceeded its deadline, aborting: player=%d listing=%d version=%d", in.PlayerId, in.ListingId, state.Version)
			return s.abortPrepare(ctx, in, digest, state, accountdirectory.ErrRetryLater)
		}
		return nil, accountdirectory.ErrRetryLater
	}
}

// probeBegin 是新建 Begin 的试探,事务外。注定被拒的 Begin 不该写令牌:写了就要让全部参与者拦这名玩家的业务写入,
// 而 trade 的对账循环会一直重放它。有挂起回 ErrConflict,问不齐回 ErrRetryLater,目录都不写。
//
// 已有收据不试探,要走第 1 步接着做或者重放。试探通过不算数,提交点仍是真正的 Prepare。
func (s *AccountRoleTradeStore) probeBegin(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest) error {
	_, err := s.directory.steps.readListing(ctx, in.PlayerId, in.ListingId)
	if err == nil {
		return nil
	}
	if !errors.Is(err, accountdirectory.ErrMissing) {
		return readFailed(err)
	}
	state, err := s.directory.steps.readFence(ctx, in.PlayerId)
	if err != nil {
		return readFailed(err)
	}
	if state.Account != in.SellerAccount || state.OwnerRevision != in.ExpectedOwnerRevision || state.FreezeToken != 0 {
		return accountdirectory.ErrConflict
	}
	// 挂起判定按这笔的令牌放行本 listing 自己的 rmb_freezing。
	state.FreezeToken = in.ListingId
	switch s.directory.fences.DryRun(ctx, state) {
	case rolefence.PrepareClean:
		return nil
	case rolefence.PrepareBlocked:
		return accountdirectory.ErrConflict
	default:
		return accountdirectory.ErrRetryLater
	}
}

// beginPrepare 是第 1 步。回 replay 表示收据已经离开 PREPARING,到此为止;回 state 表示要去问参与者,expired 表示这轮准备已经超时。
func (s *AccountRoleTradeStore) beginPrepare(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest, digest []byte) (state *rfpb.RoleFenceState, expired, replay bool, err error) {
	err = s.directory.playerTx(ctx, in.PlayerId, func(tx *sql.Tx) error {
		inserted, err := insertRoleTrade(ctx, tx, in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision, digest, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING)
		if err != nil {
			return err
		}
		prior, err := readRoleTrade(ctx, tx, in.ListingId)
		if err != nil {
			return err
		}
		if !inserted {
			if !bytes.Equal(prior.RequestDigest, digest) {
				return accountdirectory.ErrConflict
			}
			if prior.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING) {
				replay = true
				return nil
			}
		}
		owned, version, err := lockFencedOwnership(ctx, tx, in.PlayerId)
		if err != nil {
			return err
		}
		if !inserted {
			if owned.PrepareToken != in.ListingId {
				return errors.New("角色冻结收据与角色行不符")
			}
			var now uint64
			if err := tx.QueryRowContext(ctx, "SELECT "+dbNowMsSQL).Scan(&now); err != nil {
				return err
			}
			expired = now >= prior.CreatedMs+uint64(rolePrepareDeadline.Milliseconds())
			state = fenceStateOf(in.PlayerId, owned, version)
			return nil
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != 0 || owned.PrepareToken != 0 {
			return accountdirectory.ErrConflict
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET prepare_token=?,fence_version=fence_version+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", in.ListingId, in.PlayerId); err != nil {
			return err
		}
		owned.PrepareToken = in.ListingId
		state = fenceStateOf(in.PlayerId, owned, version+1)
		return nil
	})
	if err != nil {
		return nil, false, false, err
	}
	return state, expired, replay, nil
}

// replayRoleTrade 回已经离开 PREPARING 的收据。trade 的对账循环靠重放 Begin 拿收据,Seal 不到的死实例要在这里判掉,
// 否则循环永远停在 Seal 失败,走不到 Confirm。判死在目录事务外(规格 §6),收据在它之后读。
func (s *AccountRoleTradeStore) replayRoleTrade(ctx context.Context, listing, player uint64) (*dspb.AccountRoleTradeReceipt, error) {
	if err := s.directory.fenceDeadTradeHolders(ctx, listing, player); err != nil {
		return nil, err
	}
	var out *dspb.AccountRoleTradeReceipt
	err := s.directory.playerTx(ctx, player, func(tx *sql.Tx) error {
		r, err := readRoleTrade(ctx, tx, listing)
		if err != nil {
			return err
		}
		out, err = tradeReceiptWithHolders(ctx, tx, r)
		return err
	})
	if err != nil {
		// 这个事务只读。
		return nil, readFailed(err)
	}
	return out, nil
}

// lockPreparing 在目录事务里复核第 1 步写下的 PREPARING 还在。回 replay 表示已经离开 PREPARING。
func (s *AccountRoleTradeStore) lockPreparing(ctx context.Context, tx *sql.Tx, in *dspb.BeginAccountRoleTradeRequest, digest []byte, state *rfpb.RoleFenceState) (*dbpb.AccountRoleTrade, bool, error) {
	r, err := readRoleTrade(ctx, tx, in.ListingId)
	if errors.Is(err, accountdirectory.ErrMissing) {
		// 别的重放因为有挂起撤销了,收据已经删了。
		return nil, false, accountdirectory.ErrConflict
	}
	if err != nil {
		return nil, false, err
	}
	if !bytes.Equal(r.RequestDigest, digest) {
		return nil, false, accountdirectory.ErrConflict
	}
	if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING) {
		return nil, true, nil
	}
	owned, version, err := lockFencedOwnership(ctx, tx, in.PlayerId)
	if err != nil {
		return nil, false, err
	}
	// Prepare 用的是这个版本。版本变了说明又开始了一轮,这一轮的结论不算数。事务随之回滚,这一步确定没写。
	if owned.PrepareToken != in.ListingId || version != state.Version {
		return nil, false, accountdirectory.ErrRetryLater
	}
	return r, false, nil
}

// finishPrepare 是全部参与者通过之后的目录事务:准备令牌换成最终令牌,PREPARING 进 FREEZING,按角色行锁固定本轮 holder。
// 准备期间进场的实例都在这一批里,提交之后 RegisterRoleAdmission 才被挡。判死在事务提交之后做。
func (s *AccountRoleTradeStore) finishPrepare(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest, digest []byte, state *rfpb.RoleFenceState) (*dspb.AccountRoleTradeReceipt, error) {
	rejected := false
	var frozen *rfpb.RoleFenceState
	err := s.directory.playerTx(ctx, in.PlayerId, func(tx *sql.Tx) error {
		r, replay, err := s.lockPreparing(ctx, tx, in, digest, state)
		if err != nil || replay {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET trade_listing_id=?,prepare_token=0,fence_version=fence_version+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", in.ListingId, in.PlayerId); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET state=?,updated_ms="+dbNowMsSQL+" WHERE listing_id=?", uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING), in.ListingId); err != nil {
			return err
		}
		r.State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING)
		err = snapshotRoleTradeHolders(ctx, tx, r)
		rejected = errors.Is(err, accountdirectory.ErrConflict)
		if err != nil {
			return err
		}
		frozen, err = readFenceState(ctx, tx, in.PlayerId)
		return err
	})
	if rejected {
		// holder 全集不合规,这笔冻结做不了。令牌不能留着,否则重放永远停在这里。
		return s.abortPrepare(ctx, in, digest, state, accountdirectory.ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	s.directory.pushFence(ctx, frozen)
	out, err := s.replayRoleTrade(ctx, in.ListingId, in.PlayerId)
	if err != nil && frozen != nil {
		// 这次调用已经提交了 FREEZING,判死再失败也不能回确定性的拒绝。调用方重放拿收据。
		return nil, notRejection(err)
	}
	return out, err
}

// abortPrepare 撤销还没进 FREEZING 的冻结,撤销成了回 outcome。别的路径已经把收据推走了就重放收据。
func (s *AccountRoleTradeStore) abortPrepare(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest, digest []byte, state *rfpb.RoleFenceState, outcome error) (*dspb.AccountRoleTradeReceipt, error) {
	var thawed *rfpb.RoleFenceState
	replayed := false
	err := s.directory.playerTx(ctx, in.PlayerId, func(tx *sql.Tx) error {
		_, replay, err := s.lockPreparing(ctx, tx, in, digest, state)
		if err != nil {
			return err
		}
		if replay {
			replayed = true
			return nil
		}
		thawed, err = clearPreparingTx(ctx, tx, in.ListingId, in.PlayerId)
		return err
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		return s.replayRoleTrade(ctx, in.ListingId, in.PlayerId)
	}
	s.directory.pushFence(ctx, thawed)
	return nil, outcome
}

// clearPreparingTx 清准备令牌、版本加 1、删收据,和被拒的 Begin 一样不留痕。调用方已按 listing → 角色的锁序锁住两行,回撤销之后的护栏状态。
func clearPreparingTx(ctx context.Context, tx *sql.Tx, listing, player uint64) (*rfpb.RoleFenceState, error) {
	if _, err := tx.ExecContext(ctx, "UPDATE account_role SET prepare_token=0,fence_version=fence_version+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", player); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_role_trade WHERE listing_id=?", listing); err != nil {
		return nil, err
	}
	return readFenceState(ctx, tx, player)
}

// PrepareAbortResult 是一轮后台撤销的结果。Failed 留在表里,下一轮再试。
type PrepareAbortResult struct {
	Aborted, Failed int
}

type stalePreparing struct {
	listing, player uint64
}

// AbortStalePreparing 撤销超过 rolePrepareDeadline 还在 PREPARING 的收据,给没人重放的用:调用方挂了、合同走了别的路。
// 有人重放的 Begin 自己会撤销。不需要协调者:撤销只放开限制,推送丢了副本也会自愈。
// 每轮最多 limit 行,逐片读,每轮换一个起点。多副本同时跑靠收据行锁串行,先到的撤销,后到的什么都不做。
func (s *AccountDirectoryStore) AbortStalePreparing(ctx context.Context, limit int) (PrepareAbortResult, error) {
	var out PrepareAbortResult
	stale, err := s.readStalePreparing(ctx, limit)
	if err != nil {
		return out, err
	}
	var failure error
	for _, r := range stale {
		aborted, err := s.abortStalePreparing(ctx, r)
		switch {
		case err != nil:
			out.Failed++
			failure = errors.Join(failure, err)
		case aborted:
			out.Aborted++
		}
	}
	return out, failure
}

func (s *AccountDirectoryStore) readStalePreparing(ctx context.Context, limit int) ([]stalePreparing, error) {
	k := s.shards.k
	start := int(s.preparingCursor.Add(1)) % k
	var stale []stalePreparing
	for i := 0; i < k && len(stale) < limit; i++ {
		rows, err := s.shards.pools[(start+i)%k].QueryContext(ctx, "SELECT listing_id,player_id FROM account_role_trade WHERE state=? AND created_ms<"+dbNowMsSQL+"-? ORDER BY created_ms LIMIT ?",
			uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING), uint64(rolePrepareDeadline.Milliseconds()), limit-len(stale))
		if err != nil {
			return nil, err
		}
		found, err := scanStalePreparing(rows)
		if err != nil {
			return nil, err
		}
		stale = append(stale, found...)
	}
	return stale, nil
}

func scanStalePreparing(rows *sql.Rows) ([]stalePreparing, error) {
	defer rows.Close()
	var out []stalePreparing
	for rows.Next() {
		var r stalePreparing
		if err := rows.Scan(&r.listing, &r.player); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// abortStalePreparing 撤销一条收据,回是否撤销了。读完之后被重放推走或撤销了就什么都不做。锁序和 Begin 相同:listing 行 → 角色行。
func (s *AccountDirectoryStore) abortStalePreparing(ctx context.Context, stale stalePreparing) (bool, error) {
	var thawed *rfpb.RoleFenceState
	err := s.playerTx(ctx, stale.player, func(tx *sql.Tx) error {
		r, err := readRoleTrade(ctx, tx, stale.listing)
		if errors.Is(err, accountdirectory.ErrMissing) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING) {
			return nil
		}
		owned, _, err := lockFencedOwnership(ctx, tx, r.PlayerId)
		if err != nil {
			return err
		}
		if owned.PrepareToken != stale.listing {
			return errors.New("角色冻结收据与角色行不符")
		}
		thawed, err = clearPreparingTx(ctx, tx, stale.listing, r.PlayerId)
		return err
	})
	if err != nil {
		return false, err
	}
	if thawed == nil {
		return false, nil
	}
	s.pushFence(ctx, thawed)
	return true, nil
}

// Confirm 把 FREEZING 推进到 FROZEN(规格 §6),存档证据在目录事务外读,目录事务内核:
//
//  1. 判死。判死抬代次只发生在读证据之前。
//  2. 核这一轮的持有者全部排空。排空之后存档不再变,所以接下来读到的证据到提交时仍然成立。
//  3. 读证据。
//  4. 目录事务:收据还是 FREEZING、归属没变、持有者还是这一批且全部排空,证据不低于下限,初始提交标记对得上,才落证据。
func (s *AccountRoleTradeStore) Confirm(ctx context.Context, in *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if !s.ready() || s.verifier == nil {
		return nil, accountdirectory.ErrRetryLater
	}
	if in == nil || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, accountdirectory.ErrInvalid
	}
	evidence, holders, err := s.readConfirmEvidence(ctx, in)
	if err != nil {
		return nil, err
	}
	var out *dspb.AccountRoleTradeReceipt
	err = s.directory.playerTx(ctx, in.PlayerId, func(tx *sql.Tx) error {
		r, err := readRoleTrade(ctx, tx, in.ListingId)
		if err != nil {
			return err
		}
		if !matchesRoleTrade(r, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
			return accountdirectory.ErrConflict
		}
		if r.State == uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING) {
			return accountdirectory.ErrRetryLater
		}
		if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING) {
			if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN) {
				return accountdirectory.ErrConflict
			}
			out, err = tradeReceiptWithHolders(ctx, tx, r)
			return err
		}
		if evidence == nil {
			// 事务外读到的是 FROZEN,这里却还是 FREEZING:收据不会往回走,库被人动过。
			return errors.New("角色冻结收据状态回退")
		}
		owned, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, in.PlayerId), in.PlayerId)
		if err != nil {
			return err
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != in.ListingId {
			return accountdirectory.ErrConflict
		}
		fresh, err := requireDrainedHolders(ctx, tx, r)
		if err != nil {
			return err
		}
		if fresh != holders || !freezeEvidenceValid(evidence, r) || proto.Size(evidence) > 1024 {
			return accountdirectory.ErrRetryLater
		}
		if err := checkEvidenceInTx(ctx, tx, evidence, r, fresh); err != nil {
			return err
		}
		raw, err := proto.Marshal(evidence)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET state=?,freeze_evidence=?,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE listing_id=?", uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN), raw, in.ListingId); err != nil {
			return err
		}
		r.State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN)
		r.FreezeEvidence = raw
		out, err = tradeReceiptWithHolders(ctx, tx, r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readConfirmEvidence 是 Confirm 事务外的部分。收据已经是 FROZEN 时回 nil 证据,事务里只重放收据。
func (s *AccountRoleTradeStore) readConfirmEvidence(ctx context.Context, in *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleFreezeEvidence, int, error) {
	r, err := s.directory.steps.readListing(ctx, in.PlayerId, in.ListingId)
	if err != nil {
		return nil, 0, readFailed(err)
	}
	if !matchesRoleTrade(r, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, 0, accountdirectory.ErrConflict
	}
	switch r.State {
	case uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING):
		return nil, 0, accountdirectory.ErrRetryLater
	case uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN):
		return nil, 0, nil
	case uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING):
	default:
		return nil, 0, accountdirectory.ErrConflict
	}
	if err := s.directory.fenceDeadTradeHolders(ctx, in.ListingId, in.PlayerId); err != nil {
		return nil, 0, err
	}
	// 判死之后重读:下限和排空状态都以这一次为准。
	var holders int
	err = s.directory.playerTx(ctx, in.PlayerId, func(tx *sql.Tx) error {
		r, err = readRoleTrade(ctx, tx, in.ListingId)
		if err != nil {
			return err
		}
		holders, err = requireDrainedHolders(ctx, tx, r)
		return err
	})
	if err != nil {
		// 这个事务只读。
		return nil, 0, readFailed(err)
	}
	evidence, err := s.verifier.ReadFrozenEvidence(ctx, proto.Clone(r).(*dbpb.AccountRoleTrade), holders)
	if err != nil {
		return nil, 0, readFailed(err)
	}
	if evidence == nil {
		return nil, 0, accountdirectory.ErrRetryLater
	}
	return evidence, holders, nil
}

// requireDrainedHolders 核这一轮的持有者全部排空,回持有者数。没排空不能只凭存档行放行。
func requireDrainedHolders(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade) (int, error) {
	holders, err := readRoleTradeHolders(ctx, tx, r.ListingId)
	if err != nil {
		return 0, err
	}
	for _, h := range holders {
		if !h.Drained || h.Seal.PlayerId != r.PlayerId || h.Seal.OwnerRevision != r.SellerOwnerRevision {
			return 0, accountdirectory.ErrConflict
		}
	}
	return len(holders), nil
}

// checkEvidenceInTx 在目录事务里核只有目录知道的部分。初始提交的证据要求角色确实是初始提交建的、修订号还是 1、没有历史下限、没有持有者。
func checkEvidenceInTx(ctx context.Context, tx *sql.Tx, evidence *dspb.AccountRoleFreezeEvidence, r *dbpb.AccountRoleTrade, holders int) error {
	if evidence.Kind != dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT {
		return nil
	}
	if r.RequiredOwnerEpoch != 0 || r.RequiredSaveSeq != 0 || holders != 0 {
		return accountdirectory.ErrConflict
	}
	var marker, revision uint64
	if err := tx.QueryRowContext(ctx, "SELECT initial_player_commit,owner_revision FROM account_role WHERE player_id=? FOR UPDATE", r.PlayerId).Scan(&marker, &revision); err != nil {
		return err
	}
	if marker != 1 || revision != 1 || r.SellerOwnerRevision != revision {
		return accountdirectory.ErrConflict
	}
	return nil
}

// Cancel 的 PREPARING 与 FREEZING 同路径,区别只是令牌在哪一列。终态提交之后推送解冻,丢了由副本下一次访问自愈,所以没装配协调者也能取消。
func (s *AccountRoleTradeStore) Cancel(ctx context.Context, in *dspb.CancelAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if !s.ready() {
		return nil, accountdirectory.ErrRetryLater
	}
	if in == nil || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(&dspb.BeginAccountRoleTradeRequest{ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, ExpectedOwnerRevision: in.ExpectedOwnerRevision})
	if err != nil {
		return nil, err
	}
	// 判死在目录事务外:死实例不判掉的话,取消永远停在 CANCEL_PENDING。收据不存在或不是开着的时候它什么都不做。
	// 没有未排空的持有者就不碰存档库:PREPARING 没有 holder,存档库故障时取消照样能完成。
	if err := s.directory.fenceDeadTradeHolders(ctx, in.ListingId, in.PlayerId); err != nil {
		return nil, err
	}
	var out *dspb.AccountRoleTradeReceipt
	var thawed *rfpb.RoleFenceState
	err = s.directory.playerTx(ctx, in.PlayerId, func(tx *sql.Tx) error {
		// 取消可以先于尚未提交的 Begin 赢得裁决。永久墓碑让迟到原 Begin 只重放 CANCELLED。
		// 墓碑候选先插入,和 Begin 同一锁序;后面任何拒绝都回滚,不留墓碑。
		inserted, err := insertRoleTrade(ctx, tx, in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision, digest, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)
		if err != nil {
			return err
		}
		r, err := readRoleTrade(ctx, tx, in.ListingId)
		if err != nil {
			return err
		}
		if inserted {
			owned, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, in.PlayerId), in.PlayerId)
			if err != nil {
				return err
			}
			if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != 0 || owned.PrepareToken != 0 {
				return accountdirectory.ErrConflict
			}
			out, err = tradeReceiptWithHolders(ctx, tx, r)
			return err
		}
		if !matchesRoleTrade(r, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
			return accountdirectory.ErrConflict
		}
		if r.State == uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED) {
			out, err = tradeReceiptWithHolders(ctx, tx, r)
			return err
		}
		if r.State == uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED) {
			return accountdirectory.ErrConflict
		}
		owned, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, in.PlayerId), in.PlayerId)
		if err != nil {
			return err
		}
		held := owned.TradeListingId
		if r.State == uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING) {
			held = owned.PrepareToken
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || held != in.ListingId {
			return accountdirectory.ErrConflict
		}
		holders, err := readRoleTradeHolders(ctx, tx, r.ListingId)
		if err != nil {
			return err
		}
		for _, h := range holders {
			if !h.Drained {
				if _, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET state=?,updated_ms="+dbNowMsSQL+" WHERE listing_id=?", uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING), in.ListingId); err != nil {
					return err
				}
				r.State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING)
				out, err = tradeReceiptWithHolders(ctx, tx, r)
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET trade_listing_id=0,prepare_token=0,fence_version=fence_version+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", in.PlayerId); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET state=?,updated_ms="+dbNowMsSQL+" WHERE listing_id=?", uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED), in.ListingId); err != nil {
			return err
		}
		r.State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)
		out, err = tradeReceiptWithHolders(ctx, tx, r)
		if err != nil {
			return err
		}
		thawed, err = readFenceState(ctx, tx, in.PlayerId)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.directory.pushFence(ctx, thawed)
	return out, nil
}

const transferReceiptColumns = "request_digest,listing_id,player_id,seller_account,buyer_account,seller_owner_revision,buyer_owner_revision,created_ms,role_proto"

func scanTransferReceipt(row interface{ Scan(...any) error }, id uint64) (*dbpb.AccountRoleTransferReceipt, error) {
	r := &dbpb.AccountRoleTransferReceipt{TxId: id}
	err := row.Scan(&r.RequestDigest, &r.ListingId, &r.PlayerId, &r.SellerAccount, &r.BuyerAccount, &r.SellerOwnerRevision, &r.BuyerOwnerRevision, &r.CreatedMs, &r.RoleProto)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, accountdirectory.ErrMissing
	}
	return r, err
}
func readTransferReceipt(ctx context.Context, tx *sql.Tx, id uint64) (*dbpb.AccountRoleTransferReceipt, error) {
	return scanTransferReceipt(tx.QueryRowContext(ctx, "SELECT "+transferReceiptColumns+" FROM account_role_transfer_receipt WHERE tx_id=? FOR UPDATE", id), id)
}

// readTransfer 普通读过户收据。没有回 nil。
func (st *sqlDirectorySteps) readTransfer(ctx context.Context, player, txID uint64) (*dbpb.AccountRoleTransferReceipt, error) {
	r, err := scanTransferReceipt(st.shards.forPlayer(player).QueryRowContext(ctx, "SELECT "+transferReceiptColumns+" FROM account_role_transfer_receipt WHERE tx_id=?", txID), txID)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return nil, nil
	}
	return r, err
}
func transferReceipt(r *dbpb.AccountRoleTransferReceipt) (*dspb.AccountRoleTransferReceipt, error) {
	role := &base.AccountSimplePlayer{}
	if len(r.RequestDigest) != 32 || !roleTradeTupleValid(r.ListingId, r.PlayerId, r.SellerAccount, r.SellerOwnerRevision) || !accountdirectory.ValidAccount(r.BuyerAccount) || r.BuyerAccount == r.SellerAccount || r.SellerOwnerRevision == math.MaxUint64 || r.BuyerOwnerRevision != r.SellerOwnerRevision+1 || len(r.RoleProto) > 16384 || proto.Unmarshal(r.RoleProto, role) != nil || role.PlayerId != r.PlayerId {
		return nil, errors.New("角色过户收据损坏")
	}
	return &dspb.AccountRoleTransferReceipt{TxId: r.TxId, ListingId: r.ListingId, PlayerId: r.PlayerId, SellerAccount: r.SellerAccount, BuyerAccount: r.BuyerAccount, SellerOwnerRevision: r.SellerOwnerRevision, BuyerOwnerRevision: r.BuyerOwnerRevision, Role: role, CreatedMs: r.CreatedMs}, nil
}

// Transfer 是过户的三步(规格 §5.4),每步幂等:
//
//  1. 买家账号分片事务:锁目录行,核上限,写 ref(PENDING,带 tx_id)。
//  2. 玩家分片事务:写收据、改 account、owner_revision+1、清令牌、fence_version+1、收据 TRANSFERRED。这是提交点。
//  3. 买家 ref 改 ACTIVE,删卖家 ref,推 ApplyRoleFence。读护栏状态或写 ACTIVE 失败回 ErrRetryLater,其余失败无害。
//
// 第 2 步被拒时删第 1 步的 ref。同 tx_id 重放读收据,补做第 3 步。买家满位回 ErrFull,不写收据:trade 拿它去 Cancel,和提交点串行,Cancel 成了才退款(§5.4)。
func (s *AccountRoleTradeStore) Transfer(ctx context.Context, in *dspb.TransferAccountRoleRequest) (*dspb.AccountRoleTransferReceipt, error) {
	if !s.ready() || s.directory.fences == nil {
		return nil, accountdirectory.ErrRetryLater
	}
	if in == nil || in.TxId == 0 || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) || in.ExpectedOwnerRevision == math.MaxUint64 || !accountdirectory.ValidAccount(in.BuyerAccount) || in.BuyerAccount == in.SellerAccount {
		return nil, accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(in)
	if err != nil {
		return nil, err
	}
	out, err := s.replayTransfer(ctx, in, digest)
	if out != nil || err != nil {
		return out, err
	}
	// 第 1 步之前核 listing 已是 FROZEN:写 ref 就要占买家的名额,注定被拒的请求不该占。
	// 陈旧证明也靠这一条:带 listing 的 PENDING ref 写下时 listing 已是 FROZEN,而 FROZEN 只会走到终态。
	listing, err := s.directory.steps.readListing(ctx, in.PlayerId, in.ListingId)
	if err != nil {
		return nil, readFailed(err)
	}
	if !matchesRoleTrade(listing, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) || listing.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN) {
		// 同一笔过户可能就在上面两次读之间提交了:listing 已是 TRANSFERRED,收据也有了。
		out, err := s.replayTransfer(ctx, in, digest)
		if out != nil || err != nil {
			return out, err
		}
		return nil, accountdirectory.ErrConflict
	}
	if err := s.directory.reserveRefWithEviction(ctx, in.BuyerAccount, roleRef{PlayerID: in.PlayerId, TxID: in.TxId, ListingID: in.ListingId, Nonce: newRefNonce()}); err != nil {
		return nil, err
	}
	out, err = s.directory.steps.commitTransfer(ctx, in, digest)
	if err != nil {
		if errors.Is(err, accountdirectory.ErrConflict) || errors.Is(err, accountdirectory.ErrMissing) {
			s.directory.reapOwnRef(ctx, in.BuyerAccount, in.PlayerId)
		}
		return nil, err
	}
	if err := s.finishTransfer(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// replayTransfer 读同 tx_id 的收据并补做第 3 步。没有收据回 (nil, nil)。摘要不同回 ErrConflict:tx_id 被别的请求用过了。
func (s *AccountRoleTradeStore) replayTransfer(ctx context.Context, in *dspb.TransferAccountRoleRequest, digest []byte) (*dspb.AccountRoleTransferReceipt, error) {
	prior, err := s.directory.steps.readTransfer(ctx, in.PlayerId, in.TxId)
	if err != nil {
		return nil, readFailed(err)
	}
	if prior == nil {
		return nil, nil
	}
	if !bytes.Equal(prior.RequestDigest, digest) {
		return nil, accountdirectory.ErrConflict
	}
	out, err := transferReceipt(prior)
	if err != nil {
		return nil, readFailed(err)
	}
	if err := s.finishTransfer(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// finishTransfer 是第 3 步。真相还是买家才写 ACTIVE:这笔过户之后角色可能又卖了,重放不能把陈旧的 ref 写回去。
//
// 读护栏状态或写 ACTIVE 失败回 ErrRetryLater,trade 用同一 tx_id 重放:买家的列表靠这条 ref,这一步要么确认写成,要么让调用方再来一次。
// 这时过户已经提交,结局已知,排空证明不用 Hold(规格 §3.6)。不能留哨兵的错误链:ErrMissing 这些会被映射成确定性的拒绝,trade 会当成没过户。
// 只有写 ACTIVE 这条语句本身结局未知时才回 ErrUnavailable,照旧算未知。
// 删卖家 ref 和推送失败都无害:读路径按真相过滤,副本下一次访问自愈。
func (s *AccountRoleTradeStore) finishTransfer(ctx context.Context, out *dspb.AccountRoleTransferReceipt) error {
	settled, err := s.directory.steps.readFence(ctx, out.PlayerId)
	if err != nil {
		return fmt.Errorf("%w: read fence state after transfer: player=%d: %v", accountdirectory.ErrRetryLater, out.PlayerId, err)
	}
	var activateErr error
	if settled.Account == out.BuyerAccount {
		activateErr = s.directory.steps.activateRef(ctx, out.BuyerAccount, out.PlayerId, out.TxId)
	}
	s.directory.reapOwnRef(ctx, out.SellerAccount, out.PlayerId)
	s.directory.pushFence(ctx, settled)
	switch {
	case activateErr == nil:
		return nil
	case errors.Is(activateErr, authoritysql.ErrOutcomeUnknown):
		return fmt.Errorf("%w: activate buyer ref after transfer: player=%d: %v", accountdirectory.ErrUnavailable, out.PlayerId, activateErr)
	default:
		return fmt.Errorf("%w: activate buyer ref after transfer: player=%d: %v", accountdirectory.ErrRetryLater, out.PlayerId, activateErr)
	}
}

// RoleSlotFull 只数真相是这个账号的角色,在途和陈旧的 ref 不算,所以说有名额不等于过户第 1 步一定过:那里满位由 trade 走退款(规格 §5.4)。
// 只读。账号没有目录行就是没有角色。
func (s *AccountRoleTradeStore) RoleSlotFull(ctx context.Context, account string) (bool, error) {
	if !s.ready() {
		return false, accountdirectory.ErrRetryLater
	}
	if !accountdirectory.ValidAccount(account) {
		return false, accountdirectory.ErrInvalid
	}
	_, refs, err := s.directory.steps.readAccount(ctx, account)
	if err != nil {
		return false, err
	}
	roles, err := s.directory.rolesOfRefs(ctx, account, refs)
	if err != nil {
		return false, err
	}
	return len(roles.Players) >= s.directory.maxRoles, nil
}

// commitTransfer 是过户的提交点,在玩家分片的一个事务里。买家上限在第 1 步按 ref 判过了,这里不再判。
func (st *sqlDirectorySteps) commitTransfer(ctx context.Context, in *dspb.TransferAccountRoleRequest, digest []byte) (*dspb.AccountRoleTransferReceipt, error) {
	var out *dspb.AccountRoleTransferReceipt
	err := shardTx(ctx, st.shards.forPlayer(in.PlayerId), func(tx *sql.Tx) error {
		listing, listingErr := readRoleTrade(ctx, tx, in.ListingId)
		if listingErr != nil && !errors.Is(listingErr, accountdirectory.ErrMissing) {
			return listingErr
		}
		var now uint64
		if err := tx.QueryRowContext(ctx, "SELECT "+dbNowMsSQL).Scan(&now); err != nil {
			return err
		}
		// 同一 tx_id 的并发调用在 SQL 主键上串行，不能把并发 duplicate 当新交易。
		// 新收据暂存原条款；任何后续拒绝都会回滚，不留下假的成功收据。
		result, err := tx.ExecContext(ctx, "INSERT INTO account_role_transfer_receipt(tx_id,request_digest,listing_id,player_id,seller_account,buyer_account,seller_owner_revision,buyer_owner_revision,created_ms,role_proto) VALUES (?,?,?,?,?,?,?,?,?,'') ON DUPLICATE KEY UPDATE tx_id=tx_id", in.TxId, digest, in.ListingId, in.PlayerId, []byte(in.SellerAccount), []byte(in.BuyerAccount), in.ExpectedOwnerRevision, in.ExpectedOwnerRevision+1, now)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		prior, err := readTransferReceipt(ctx, tx, in.TxId)
		if err != nil {
			return err
		}
		if inserted == 0 {
			if !bytes.Equal(prior.RequestDigest, digest) {
				return accountdirectory.ErrConflict
			}
			out, err = transferReceipt(prior)
			return err
		}
		if listingErr != nil {
			return listingErr
		}
		if !matchesRoleTrade(listing, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) || listing.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN) {
			return accountdirectory.ErrConflict
		}
		if _, err = tradeReceipt(listing); err != nil {
			return err
		}
		owned, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, in.PlayerId), in.PlayerId)
		if err != nil {
			return err
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != in.ListingId {
			return accountdirectory.ErrConflict
		}
		if owned.OwnerRevision == math.MaxUint64 {
			return errors.New("角色归属修订号耗尽")
		}
		raw, err := proto.Marshal(owned.Role)
		if err != nil {
			return err
		}
		r := &dbpb.AccountRoleTransferReceipt{TxId: in.TxId, RequestDigest: digest, ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, BuyerAccount: in.BuyerAccount, SellerOwnerRevision: in.ExpectedOwnerRevision, BuyerOwnerRevision: owned.OwnerRevision + 1, CreatedMs: now, RoleProto: raw}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET account=?,owner_revision=?,trade_listing_id=0,fence_version=fence_version+1,updated_ms=? WHERE player_id=?", []byte(in.BuyerAccount), r.BuyerOwnerRevision, now, in.PlayerId); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET state=?,updated_ms=? WHERE listing_id=?", uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED), now, in.ListingId); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role_transfer_receipt SET role_proto=? WHERE tx_id=?", r.RoleProto, r.TxId); err != nil {
			return err
		}
		out, err = transferReceipt(r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
