package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/authoritysql"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

const AccountDirectoryTableName = "account_directory"
const AccountRoleTableName = "account_role"
const accountDirectoryDDL = "CREATE TABLE IF NOT EXISTS account_directory (account VARBINARY(191) NOT NULL COMMENT 'pb:1',created_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',PRIMARY KEY(account)) ENGINE=InnoDB"
const accountRoleDDL = "CREATE TABLE IF NOT EXISTS account_role (player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',account VARBINARY(191) NOT NULL COMMENT 'pb:2',role_proto MEDIUMBLOB NOT NULL COMMENT 'pb:3',owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:5',trade_listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:6',initial_player_commit BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:7',PRIMARY KEY(player_id) /*T![clustered_index] NONCLUSTERED */,INDEX idx_account_role_owner(account,player_id)) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"

// AccountDirectoryStore 只拥有全局目录；与快照池隔离，不访问认证 password 或 Redis。
type AccountDirectoryStore struct {
	db              *sql.DB
	backend         authoritysql.Backend
	maxRoles        int
	contracts       *roleAssetContractGuard
	schema          string
	initialRequired bool
	initialStores   map[uint32]bool
	// 账本和展示资料读的名额,见 acquirePlayerStoreRead。
	playerStoreReads chan struct{}
}

// maxOpenConn 是本池的连接上限(config.AccountDirectoryConfig.PoolMaxOpenConn),显式传入而不读 cfg.MaxOpenConn:
// cfg 是共用的全局库连接参数,那里的 MaxOpenConn 属于快照池。越界由 authoritysql.Open 拒绝。
func NewAccountDirectoryStore(cfg MySQLConfig, backend authoritysql.Backend, maxRoles, maxOpenConn int) (*AccountDirectoryStore, error) {
	// 正式目录安全语义不由后端档位或运行模式开关决定。
	return newAccountDirectoryStore(cfg, backend, maxRoles, maxOpenConn, true)
}

// NewLegacyAccountDirectoryStore 只供显式 LegacyNonGlobal 隔离模式装配。
// 不产生初始提交回执；普通构造器永不按后端档位隐式进入此模式。
func NewLegacyAccountDirectoryStore(cfg MySQLConfig, backend authoritysql.Backend, maxRoles, maxOpenConn int) (*AccountDirectoryStore, error) {
	return newAccountDirectoryStore(cfg, backend, maxRoles, maxOpenConn, false)
}

func newAccountDirectoryStore(cfg MySQLConfig, backend authoritysql.Backend, maxRoles, maxOpenConn int, initialRequired bool) (*AccountDirectoryStore, error) {
	if maxRoles < 1 || maxRoles > 100 {
		return nil, accountdirectory.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := authoritysql.Open(ctx, buildDSN(cfg), cfg.DBName, backend, maxOpenConn, cfg.Permission)
	if err != nil {
		return nil, err
	}
	if err = assertAccountDirectoryShape(ctx, db, cfg.DBName); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &AccountDirectoryStore{db: db, backend: backend, maxRoles: maxRoles, schema: cfg.DBName, initialRequired: initialRequired,
		playerStoreReads: make(chan struct{}, playerStoreReadLimit(maxOpenConn))}, nil
}
func (s *AccountDirectoryStore) Close() error { return s.db.Close() }

func assertAccountDirectoryShape(ctx context.Context, db *sql.DB, name string) error {
	if err := assertInitialPlayerCommitShape(ctx, db, name); err != nil {
		return err
	}
	for _, model := range []proto.Message{&dbpb.AccountDirectory{}, &dbpb.AccountRole{}} {
		table := AccountDirectoryTableName
		key := "account"
		if _, ok := model.(*dbpb.AccountRole); ok {
			table = AccountRoleTableName
			key = "player_id"
		}
		if err := assertColumnsPresent(ctx, db, name, model, table); err != nil {
			return err
		}
		keys, err := primaryKeyColumns(ctx, db, name, table)
		if err != nil {
			return err
		}
		if len(keys) != 1 || keys[0] != key {
			return fmt.Errorf("账号目录主键不符: %s", table)
		}
		var typ string
		var width sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME='account'", name, table).Scan(&typ, &width); err != nil {
			return err
		}
		if typ != "varbinary" || !width.Valid || width.Int64 != 191 {
			return fmt.Errorf("账号目录必须逐字节比较: %s", table)
		}
	}
	indexes, err := loadIndexColumns(ctx, db, name, AccountRoleTableName)
	if err != nil {
		return err
	}
	for _, cols := range indexes {
		if len(cols) == 2 && cols[0] == "account" && cols[1] == "player_id" {
			return nil
		}
	}
	return errors.New("账号角色归属索引缺失")
}

func (s *AccountDirectoryStore) transaction(ctx context.Context, action func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
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
func lockDirectory(ctx context.Context, tx *sql.Tx, account string, create bool) error {
	if !accountdirectory.ValidAccount(account) {
		return accountdirectory.ErrInvalid
	}
	if create {
		if _, err := tx.ExecContext(ctx, "INSERT INTO account_directory(account,created_ms) VALUES (?,UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000) ON DUPLICATE KEY UPDATE account=account", []byte(account)); err != nil {
			return err
		}
	}
	var stored []byte
	err := tx.QueryRowContext(ctx, "SELECT account FROM account_directory WHERE account=? FOR UPDATE", []byte(account)).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return accountdirectory.ErrMissing
	}
	return err
}

type directoryQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readRoles(ctx context.Context, q directoryQuery, account string) (*base.AccountSimplePlayerList, error) {
	rows, err := q.QueryContext(ctx, "SELECT player_id,role_proto,owner_revision FROM account_role WHERE account=? ORDER BY player_id LIMIT 101", []byte(account))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &base.AccountSimplePlayerList{}
	for rows.Next() {
		var id, rev uint64
		var raw []byte
		if err = rows.Scan(&id, &raw, &rev); err != nil {
			return nil, err
		}
		role := &base.AccountSimplePlayer{}
		if len(raw) > 16384 {
			return nil, errors.New("账号角色原件越界")
		}
		if err = proto.Unmarshal(raw, role); err != nil || id == 0 || role.PlayerId != id || rev == 0 {
			return nil, errors.New("账号角色原件损坏")
		}
		out.Players = append(out.Players, role)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Players) > 100 {
		return nil, errors.New("账号角色目录超过硬上限")
	}
	return out, nil
}

// Read 读账号的角色列表。登录每次都走这里,所以先无锁读,目录行已在就只读返回,不碰任何行锁
// (docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)。
// 只有 create=true 且目录行不存在才进入加锁建行的路径,那条路径在锁内复核。
//
// 不加锁之后 Read 不再排在同账号在途的 Create / Repair / Remove 后面:对方提交之前读到的是提交之前的列表,
// 提交之后才开始的 Read 一定读得到(强读会话,TiDB 读 leader、MySQL 档连主库)。调用方不依赖那次排队:
// 列表只用来展示和选角,改归属的操作在各自事务里按 owner_revision 复核,入场用 Lookup 强读复核;
// 建角超时的恢复是先 Lookup 确认已提交,再 Read。目录行只增不删,所以目录行与角色列表分两条语句读没有一致性问题。
func (s *AccountDirectoryStore) Read(ctx context.Context, account string, create bool) (*base.AccountSimplePlayerList, error) {
	if !accountdirectory.ValidAccount(account) {
		return nil, accountdirectory.ErrInvalid
	}
	var out *base.AccountSimplePlayerList
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var stored []byte
		err := tx.QueryRowContext(ctx, "SELECT account FROM account_directory WHERE account=?", []byte(account)).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return accountdirectory.ErrMissing
		}
		if err != nil {
			return err
		}
		out, err = readRoles(ctx, tx, account)
		return err
	})
	if !create || !errors.Is(err, accountdirectory.ErrMissing) {
		return out, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockDirectory(ctx, tx, account, true); err != nil {
			return err
		}
		var err error
		out, err = readRoles(ctx, tx, account)
		return err
	})
	return out, err
}
func scanOwnership(row interface{ Scan(...any) error }, id uint64) (*dspb.AccountRoleOwnership, error) {
	var account string
	var raw []byte
	var rev, listing uint64
	if err := row.Scan(&account, &raw, &rev, &listing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, accountdirectory.ErrMissing
		}
		return nil, err
	}
	role := &base.AccountSimplePlayer{}
	if len(raw) > 16384 {
		return nil, errors.New("角色归属原件越界")
	}
	if err := proto.Unmarshal(raw, role); err != nil || role.PlayerId != id || id == 0 || rev == 0 || (account != "" && !accountdirectory.ValidAccount(account)) || (account == "" && listing != 0) {
		return nil, errors.New("角色归属原件损坏")
	}
	return &dspb.AccountRoleOwnership{Account: account, Role: role, OwnerRevision: rev, TradeListingId: listing}, nil
}
func (s *AccountDirectoryStore) Lookup(ctx context.Context, id uint64) (*dspb.AccountRoleOwnership, error) {
	if id == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	owned, err := scanOwnership(s.db.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=?", id), id)
	if err == nil && owned.Account == "" {
		return nil, accountdirectory.ErrMissing
	}
	return owned, err
}
func validRole(role *base.AccountSimplePlayer) bool {
	return role != nil && role.PlayerId != 0 && proto.Size(role) <= 16384
}
func (s *AccountDirectoryStore) Create(ctx context.Context, account string, role *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	out, err := s.CreateWithInitialCommit(ctx, account, role)
	if err != nil {
		return nil, err
	}
	return out.Players, nil
}

// CreateWithInitialCommit 严格新建:同一事务写角色目录和 player_store 初始行。未知提交由原 Lookup 恢复。
func (s *AccountDirectoryStore) CreateWithInitialCommit(ctx context.Context, account string, role *base.AccountSimplePlayer) (*dspb.AccountDirectoryResponse, error) {
	players, err := s.createRole(ctx, account, role, roleCreateNormal, nil, true)
	if err != nil {
		return nil, err
	}
	return &dspb.AccountDirectoryResponse{Players: players}, nil
}

// ImportRole 只供受本地目标／离线检查约束的调试导入工具使用；普通 RPC 永不复活删除墓碑。
func (s *AccountDirectoryStore) ImportRole(ctx context.Context, account string, role *base.AccountSimplePlayer, allowRestore bool, requestID []byte) (*base.AccountSimplePlayerList, error) {
	mode := roleImportExisting
	if allowRestore {
		mode = roleImportRestore
	}
	if len(requestID) != 32 {
		return nil, accountdirectory.ErrInvalid
	}
	return s.createRole(ctx, account, role, mode, requestID, false)
}

type roleCreateMode uint8

const (
	roleCreateNormal roleCreateMode = iota
	roleImportExisting
	roleImportRestore
)

func (s *AccountDirectoryStore) createRole(ctx context.Context, account string, role *base.AccountSimplePlayer, mode roleCreateMode, requestID []byte, strictInitial bool) (*base.AccountSimplePlayerList, error) {
	if !validRole(role) {
		return nil, accountdirectory.ErrInvalid
	}
	raw, err := proto.Marshal(role)
	if err != nil {
		return nil, err
	}
	var out *base.AccountSimplePlayerList
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockDirectory(ctx, tx, account, true); err != nil {
			return err
		}
		if mode == roleCreateNormal {
			if err := s.lockInitialPlayerRoute(ctx, tx, role); err != nil {
				return err
			}
		}
		var imported *dbpb.AccountRoleImportReceipt
		if mode != roleCreateNormal {
			var err error
			imported, err = beginRoleImport(ctx, tx, requestID, account, role.PlayerId, raw, mode)
			if err != nil {
				return err
			}
		}
		old, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", role.PlayerId), role.PlayerId)
		if mode != roleCreateNormal && (err == nil || errors.Is(err, accountdirectory.ErrMissing)) {
			if err := requireClosedAdmissions(ctx, tx, role.PlayerId); err != nil {
				return err
			}
			if err := s.requireNoAssetContracts(ctx, tx, role.PlayerId); err != nil {
				return err
			}
		}
		if err == nil && old.Account != "" {
			if old.TradeListingId != 0 {
				return accountdirectory.ErrConflict
			}
			if old.Account != account || !proto.Equal(old.Role, role) {
				return accountdirectory.ErrConflict
			}
			if imported != nil {
				if imported.OwnerRevision != 0 {
					if old.OwnerRevision != imported.OwnerRevision {
						return accountdirectory.ErrConflict
					}
				} else {
					if old.OwnerRevision == math.MaxUint64 {
						return errors.New("角色归属修订号耗尽")
					}
					if _, err = tx.ExecContext(ctx, "UPDATE account_role SET owner_revision=owner_revision+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", role.PlayerId); err != nil {
						return err
					}
					if err = finishRoleImport(ctx, tx, imported, old.OwnerRevision+1); err != nil {
						return err
					}
				}
			}
			out, err = readRoles(ctx, tx, account)
			return err
		}
		if err != nil && !errors.Is(err, accountdirectory.ErrMissing) {
			return err
		}
		if imported != nil && imported.OwnerRevision != 0 {
			return accountdirectory.ErrConflict // 原请求已提交过，不能复活后来删除的目录。
		}
		if old != nil && mode != roleImportRestore {
			return accountdirectory.ErrConflict
		}
		out, err = readRoles(ctx, tx, account)
		if err != nil {
			return err
		}
		if len(out.Players) >= s.maxRoles {
			return accountdirectory.ErrFull
		}
		wroteInitial := false
		if old == nil && mode == roleCreateNormal {
			wrote, err := s.insertInitialPlayerRows(ctx, tx, role)
			if err != nil {
				return err
			}
			wroteInitial = wrote
		}
		if old != nil {
			if old.OwnerRevision == math.MaxUint64 {
				return errors.New("角色归属修订号耗尽")
			}
			_, err = tx.ExecContext(ctx, "UPDATE account_role SET account=?,role_proto=?,owner_revision=?,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE player_id=?", []byte(account), raw, old.OwnerRevision+1, role.PlayerId)
		} else {
			initialMarker := uint64(0)
			if strictInitial && wroteInitial {
				initialMarker = 1
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO account_role(player_id,account,role_proto,owner_revision,updated_ms,initial_player_commit) VALUES (?,?,?,1,UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000,?)", role.PlayerId, []byte(account), raw, initialMarker)
		}
		if err != nil {
			var duplicate *mysql.MySQLError
			if errors.As(err, &duplicate) && duplicate.Number == 1062 {
				return accountdirectory.ErrConflict
			}
			return err
		}
		revision := uint64(1)
		if old != nil {
			revision = old.OwnerRevision + 1
		}
		if err = finishRoleImport(ctx, tx, imported, revision); err != nil {
			return err
		}
		out.Players = append(out.Players, proto.Clone(role).(*base.AccountSimplePlayer))
		sort.Slice(out.Players, func(i, j int) bool { return out.Players[i].PlayerId < out.Players[j].PlayerId })
		return nil
	})
	return out, err
}
func mergeRole(old, patch *base.AccountSimplePlayer) (*base.AccountSimplePlayer, error) {
	copy := proto.Clone(old).(*base.AccountSimplePlayer)
	if patch.ClassId != 0 {
		if copy.ClassId != 0 && copy.ClassId != patch.ClassId {
			return nil, accountdirectory.ErrConflict
		}
		copy.ClassId = patch.ClassId
	}
	if patch.Gender != 0 {
		if copy.Gender != 0 && copy.Gender != patch.Gender {
			return nil, accountdirectory.ErrConflict
		}
		copy.Gender = patch.Gender
	}
	if patch.AppearanceId != "" {
		if copy.AppearanceId != "" && copy.AppearanceId != patch.AppearanceId {
			return nil, accountdirectory.ErrConflict
		}
		copy.AppearanceId = patch.AppearanceId
	}
	if patch.Name != "" {
		if copy.Name != "" && copy.Name != patch.Name {
			return nil, accountdirectory.ErrConflict
		}
		copy.Name = patch.Name
	}
	return copy, nil
}
func (s *AccountDirectoryStore) Repair(ctx context.Context, account string, role *base.AccountSimplePlayer, rev uint64) (*base.AccountSimplePlayerList, error) {
	if !validRole(role) || rev == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	var out *base.AccountSimplePlayerList
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockDirectory(ctx, tx, account, false); err != nil {
			return err
		}
		old, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", role.PlayerId), role.PlayerId)
		if err != nil {
			return err
		}
		if old.Account != account || old.OwnerRevision != rev || old.TradeListingId != 0 {
			return accountdirectory.ErrConflict
		}
		merged, err := mergeRole(old.Role, role)
		if err != nil {
			return err
		}
		raw, err := proto.Marshal(merged)
		if err != nil {
			return err
		}
		before, err := proto.Marshal(old.Role)
		if err != nil {
			return err
		}
		if !bytes.Equal(before, raw) {
			if _, err = tx.ExecContext(ctx, "UPDATE account_role SET role_proto=?,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE player_id=? AND account=? AND owner_revision=?", raw, role.PlayerId, []byte(account), rev); err != nil {
				return err
			}
		}
		out, err = readRoles(ctx, tx, account)
		return err
	})
	return out, err
}
func (s *AccountDirectoryStore) Remove(ctx context.Context, account string, roles []*dspb.AccountRoleVersion) error {
	if len(roles) == 0 || len(roles) > 100 {
		return accountdirectory.ErrInvalid
	}
	ordered := append([]*dspb.AccountRoleVersion(nil), roles...)
	for _, r := range ordered {
		if r == nil || r.PlayerId == 0 || r.OwnerRevision == 0 {
			return accountdirectory.ErrInvalid
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PlayerId < ordered[j].PlayerId })
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockDirectory(ctx, tx, account, false); err != nil {
			return err
		}
		for i, r := range ordered {
			if i > 0 && r.PlayerId == ordered[i-1].PlayerId {
				return accountdirectory.ErrInvalid
			}
			old, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", r.PlayerId), r.PlayerId)
			if errors.Is(err, accountdirectory.ErrMissing) {
				continue
			}
			if err != nil {
				return err
			}
			if old.Account == "" {
				continue
			}
			if old.Account != account || old.OwnerRevision != r.OwnerRevision || old.TradeListingId != 0 {
				return accountdirectory.ErrConflict
			}
			if old.OwnerRevision == math.MaxUint64 {
				return errors.New("角色归属修订号耗尽")
			}
			if err := requireClosedAdmissions(ctx, tx, r.PlayerId); err != nil {
				return err
			}
			if err := s.requireNoAssetContracts(ctx, tx, r.PlayerId); err != nil {
				return err
			}
			// 保留墓碑与单调修订号；恢复同 player_id 后旧登录链也不能通过 ABA 重获修改权。
			if _, err = tx.ExecContext(ctx, "UPDATE account_role SET account='',owner_revision=owner_revision+1,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE player_id=? AND account=? AND owner_revision=?", r.PlayerId, []byte(account), r.OwnerRevision); err != nil {
				return err
			}
		}
		return nil
	})
}
