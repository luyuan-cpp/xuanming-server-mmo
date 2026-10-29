package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"

	"google.golang.org/protobuf/proto"
)

const AccountRoleTradeTableName = "account_role_trade"
const AccountRoleTransferTableName = "account_role_transfer_receipt"
const accountRoleTradeDDL = "CREATE TABLE IF NOT EXISTS account_role_trade (listing_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',seller_account VARBINARY(191) NOT NULL COMMENT 'pb:3',seller_owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',request_digest BINARY(32) NOT NULL COMMENT 'pb:5',state INT UNSIGNED NOT NULL COMMENT 'pb:6',freeze_evidence MEDIUMBLOB NOT NULL COMMENT 'pb:7',created_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:8',updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:9',required_owner_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:10',required_save_seq BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:11',PRIMARY KEY(listing_id) /*T![clustered_index] NONCLUSTERED */) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"
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

// RoleFreezeVerifier 在目录同一 SQL 事务内验证原生冻结、严格存盘和旧入场链排空证据。
// 实现只允许同事务 SQL 读，不能在持锁期间调用 Redis、RPC 或接受 caller 自报离线。
// 生产仅在显式 TiDB/db_first/存储白名单配置后装配；缺配置不能产生 FROZEN。
type RoleFreezeVerifier interface {
	VerifyFrozen(context.Context, *sql.Tx, *dbpb.AccountRoleTrade) (*dspb.AccountRoleFreezeEvidence, error)
}

type AccountRoleTradeStore struct {
	directory        *AccountDirectoryStore
	verifier         RoleFreezeVerifier
	requireGuildExit bool
	guildSchema      string
	contracts        *roleAssetContractGuard
}

func NewAccountRoleTradeStore(directory *AccountDirectoryStore, verifier RoleFreezeVerifier) *AccountRoleTradeStore {
	var contracts *roleAssetContractGuard
	if directory != nil {
		contracts = directory.contracts
	}
	return &AccountRoleTradeStore{directory: directory, verifier: verifier, requireGuildExit: true, contracts: contracts}
}

var _ accountdirectory.RoleTrades = (*AccountRoleTradeStore)(nil)

func roleTradeTupleValid(listing, player uint64, seller string, revision uint64) bool {
	return listing != 0 && player != 0 && revision != 0 && accountdirectory.ValidAccount(seller)
}
func roleTradeDigest(in proto.Message) ([]byte, error) {
	hexDigest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, accountdirectory.ErrInvalid
	}
	return hex.DecodeString(hexDigest)
}
func lockAccounts(ctx context.Context, tx *sql.Tx, accounts ...string) error {
	sort.Strings(accounts) // Go 字符串比较是原始 UTF-8 字节序，不 trim、不 casefold。
	for i, account := range accounts {
		if i > 0 && account == accounts[i-1] {
			continue
		}
		if err := lockDirectory(ctx, tx, account, true); err != nil {
			return err
		}
	}
	return nil
}
func readRoleTrade(ctx context.Context, tx *sql.Tx, listing uint64) (*dbpb.AccountRoleTrade, error) {
	r := &dbpb.AccountRoleTrade{ListingId: listing}
	err := tx.QueryRowContext(ctx, "SELECT player_id,seller_account,seller_owner_revision,request_digest,state,freeze_evidence,created_ms,updated_ms,required_owner_epoch,required_save_seq FROM account_role_trade WHERE listing_id=? FOR UPDATE", listing).Scan(&r.PlayerId, &r.SellerAccount, &r.SellerOwnerRevision, &r.RequestDigest, &r.State, &r.FreezeEvidence, &r.CreatedMs, &r.UpdatedMs, &r.RequiredOwnerEpoch, &r.RequiredSaveSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, accountdirectory.ErrMissing
	}
	if err != nil {
		return nil, err
	}
	if !roleTradeTupleValid(listing, r.PlayerId, r.SellerAccount, r.SellerOwnerRevision) || len(r.RequestDigest) != 32 || r.State < uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING) || r.State > uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING) || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
		return nil, errors.New("角色冻结收据损坏")
	}
	return r, nil
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
	return s != nil && s.directory != nil && s.directory.db != nil
}

func (s *AccountRoleTradeStore) Begin(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if !s.ready() {
		return nil, accountdirectory.ErrUnavailable
	}
	if in == nil || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(in)
	if err != nil {
		return nil, err
	}
	var out *dspb.AccountRoleTradeReceipt
	err = s.directory.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockAccounts(ctx, tx, in.SellerAccount); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO account_role_trade(listing_id,player_id,seller_account,seller_owner_revision,request_digest,state,freeze_evidence,created_ms,updated_ms) VALUES (?,?,?,?,?,?,'',UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000,UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000) ON DUPLICATE KEY UPDATE listing_id=listing_id", in.ListingId, in.PlayerId, []byte(in.SellerAccount), in.ExpectedOwnerRevision, digest, uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING))
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		prior, err := readRoleTrade(ctx, tx, in.ListingId)
		if err != nil {
			return err
		}
		if inserted == 0 {
			if !bytes.Equal(prior.RequestDigest, digest) {
				return accountdirectory.ErrConflict
			}
			out, err = tradeReceiptWithHolders(ctx, tx, prior)
			return err
		}
		owned, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", in.PlayerId), in.PlayerId)
		if err != nil {
			return err
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != 0 {
			return accountdirectory.ErrConflict
		}
		if err := s.verifyGuildExit(ctx, tx, in.PlayerId); err != nil {
			return err
		}
		if err := s.verifyNoAssetContracts(ctx, tx, in); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET trade_listing_id=?,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE player_id=?", in.ListingId, in.PlayerId); err != nil {
			return err
		}
		if err = snapshotRoleTradeHolders(ctx, tx, prior); err != nil {
			return err
		}
		out, err = tradeReceiptWithHolders(ctx, tx, prior)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *AccountRoleTradeStore) Confirm(ctx context.Context, in *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if !s.ready() || s.verifier == nil {
		return nil, accountdirectory.ErrUnavailable
	}
	if in == nil || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, accountdirectory.ErrInvalid
	}
	var out *dspb.AccountRoleTradeReceipt
	err := s.directory.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockAccounts(ctx, tx, in.SellerAccount); err != nil {
			return err
		}
		r, err := readRoleTrade(ctx, tx, in.ListingId)
		if err != nil {
			return err
		}
		if !matchesRoleTrade(r, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
			return accountdirectory.ErrConflict
		}
		if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING) {
			if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN) {
				return accountdirectory.ErrConflict
			}
			out, err = tradeReceiptWithHolders(ctx, tx, r)
			return err
		}
		owned, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", in.PlayerId), in.PlayerId)
		if err != nil {
			return err
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != in.ListingId {
			return accountdirectory.ErrConflict
		}
		evidence, err := s.verifier.VerifyFrozen(ctx, tx, proto.Clone(r).(*dbpb.AccountRoleTrade))
		if err != nil {
			return err
		}
		if !freezeEvidenceValid(evidence, r) || proto.Size(evidence) > 1024 {
			return accountdirectory.ErrUnavailable
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

func (s *AccountRoleTradeStore) Cancel(ctx context.Context, in *dspb.CancelAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if !s.ready() {
		return nil, accountdirectory.ErrUnavailable
	}
	if in == nil || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, accountdirectory.ErrInvalid
	}
	var out *dspb.AccountRoleTradeReceipt
	err := s.directory.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockAccounts(ctx, tx, in.SellerAccount); err != nil {
			return err
		}
		r, err := readRoleTrade(ctx, tx, in.ListingId)
		if errors.Is(err, accountdirectory.ErrMissing) {
			// 取消可以先于尚未提交的Begin赢得裁决。永久墓碑让迟到原Begin只重放CANCELLED。
			owned, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", in.PlayerId), in.PlayerId)
			if err != nil {
				return err
			}
			if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != 0 {
				return accountdirectory.ErrConflict
			}
			original := &dspb.BeginAccountRoleTradeRequest{ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, ExpectedOwnerRevision: in.ExpectedOwnerRevision}
			digest, err := roleTradeDigest(original)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO account_role_trade(listing_id,player_id,seller_account,seller_owner_revision,request_digest,state,freeze_evidence,created_ms,updated_ms) VALUES (?,?,?,?,?,?,'',"+dbNowMsSQL+","+dbNowMsSQL+")", in.ListingId, in.PlayerId, []byte(in.SellerAccount), in.ExpectedOwnerRevision, digest, uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)); err != nil {
				return err
			}
			r, err = readRoleTrade(ctx, tx, in.ListingId)
			if err != nil {
				return err
			}
			out, err = tradeReceiptWithHolders(ctx, tx, r)
			return err
		}
		if err != nil {
			return err
		}
		if !matchesRoleTrade(r, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
			return accountdirectory.ErrConflict
		}
		if r.State == uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED) {
			out, err = tradeReceiptWithHolders(ctx, tx, r)
			return err
		}
		if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING) && r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN) && r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING) {
			return accountdirectory.ErrConflict
		}
		owned, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", in.PlayerId), in.PlayerId)
		if err != nil {
			return err
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != in.ListingId {
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
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET trade_listing_id=0,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE player_id=?", in.PlayerId); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET state=?,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE listing_id=?", uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED), in.ListingId); err != nil {
			return err
		}
		r.State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)
		out, err = tradeReceiptWithHolders(ctx, tx, r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readTransferReceipt(ctx context.Context, tx *sql.Tx, id uint64) (*dbpb.AccountRoleTransferReceipt, error) {
	r := &dbpb.AccountRoleTransferReceipt{TxId: id}
	err := tx.QueryRowContext(ctx, "SELECT request_digest,listing_id,player_id,seller_account,buyer_account,seller_owner_revision,buyer_owner_revision,created_ms,role_proto FROM account_role_transfer_receipt WHERE tx_id=? FOR UPDATE", id).Scan(&r.RequestDigest, &r.ListingId, &r.PlayerId, &r.SellerAccount, &r.BuyerAccount, &r.SellerOwnerRevision, &r.BuyerOwnerRevision, &r.CreatedMs, &r.RoleProto)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, accountdirectory.ErrMissing
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
func (s *AccountRoleTradeStore) Transfer(ctx context.Context, in *dspb.TransferAccountRoleRequest) (*dspb.AccountRoleTransferReceipt, error) {
	if !s.ready() {
		return nil, accountdirectory.ErrUnavailable
	}
	if in == nil || in.TxId == 0 || !roleTradeTupleValid(in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) || in.ExpectedOwnerRevision == math.MaxUint64 || !accountdirectory.ValidAccount(in.BuyerAccount) || in.BuyerAccount == in.SellerAccount {
		return nil, accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(in)
	if err != nil {
		return nil, err
	}
	var out *dspb.AccountRoleTransferReceipt
	err = s.directory.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockAccounts(ctx, tx, in.SellerAccount, in.BuyerAccount); err != nil {
			return err
		}
		listing, listingErr := readRoleTrade(ctx, tx, in.ListingId)
		if listingErr != nil && !errors.Is(listingErr, accountdirectory.ErrMissing) {
			return listingErr
		}
		var now uint64
		if err = tx.QueryRowContext(ctx, "SELECT "+dbNowMsSQL).Scan(&now); err != nil {
			return err
		}
		// 同一 tx_id 的不同账号调用也在 SQL 主键上串行，不能把并发 duplicate 当新交易。
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
		owned, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", in.PlayerId), in.PlayerId)
		if err != nil {
			return err
		}
		if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != in.ListingId {
			return accountdirectory.ErrConflict
		}
		if owned.OwnerRevision == math.MaxUint64 {
			return errors.New("角色归属修订号耗尽")
		}
		buyer, err := readRoles(ctx, tx, in.BuyerAccount)
		if err != nil {
			return err
		}
		if len(buyer.Players) >= s.directory.maxRoles {
			return accountdirectory.ErrFull
		}
		raw, err := proto.Marshal(owned.Role)
		if err != nil {
			return err
		}
		r := &dbpb.AccountRoleTransferReceipt{TxId: in.TxId, RequestDigest: digest, ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, BuyerAccount: in.BuyerAccount, SellerOwnerRevision: in.ExpectedOwnerRevision, BuyerOwnerRevision: owned.OwnerRevision + 1, CreatedMs: now, RoleProto: raw}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET account=?,owner_revision=?,trade_listing_id=0,updated_ms=? WHERE player_id=?", []byte(in.BuyerAccount), r.BuyerOwnerRevision, now, in.PlayerId); err != nil {
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
