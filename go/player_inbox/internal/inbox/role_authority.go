package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dspb "proto/data_service"
	inboxpb "proto/player_inbox"
	"shared/accountdirectory"
	"shared/assetop"
	"shared/authoritysql"
	"shared/placement"
)

var ErrOriginalRole = errors.New("inbox: 原角色身份缺失、已换主或冻结")

var roleSchemaPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// RoleAuthority 隔离同事务域SQL目录依赖。原件由首次新入箱捕获，历史条目只核验、不补写。
// LockCurrent先取得完整meta/64片/route前缀，再取得原account_role P；成功后全部持有到提交。
type RoleAuthority interface {
	LockCurrent(context.Context, *sql.Tx, uint64) (*dspb.AccountRoleOwnership, error)
	VerifyAdmission(context.Context, *sql.Tx, *dspb.AccountRoleOwnership, *base.RoleAdmissionIdentity) error
}

type SQLRoleAuthority struct{ schema string }

// 永久终态历史不能让每次挂起核验退化为扫描全部历史；旧表缺索引须先执行正规迁移。
func CheckAuthorityIndexes(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT index_name,column_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='player_inbox_item' ORDER BY index_name,seq_in_index")
	if err != nil {
		return err
	}
	defer rows.Close()
	indexes := map[string][]string{}
	for rows.Next() {
		var name, column string
		if err = rows.Scan(&name, &column); err != nil {
			return err
		}
		indexes[name] = append(indexes[name], column)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, columns := range indexes {
		if strings.Join(columns, ",") == "shard,player_id,state" {
			return nil
		}
	}
	return errors.New("inbox: 缺(shard,player_id,state)挂起索引，先执行迁移计划再接流")
}

func NewSQLRoleAuthority(ctx context.Context, db *sql.DB, schema string, backend authoritysql.Backend) (*SQLRoleAuthority, error) {
	if db == nil || !roleSchemaPattern.MatchString(schema) {
		return nil, ErrOriginalRole
	}
	if err := authoritysql.AssertCurrentSession(ctx, db, backend); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT player_id,account,owner_revision,trade_listing_id FROM `"+schema+"`.account_role LIMIT 0")
	if err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, "SELECT player_id,scene_instance_id,highest_generation,closed_through,identity_proto FROM `"+schema+"`.account_role_admission LIMIT 0")
	if err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = placement.CheckRoleRouteContract(ctx, db, schema); err != nil {
		return nil, err
	}
	return &SQLRoleAuthority{schema: schema}, nil
}

func (s *SQLRoleAuthority) VerifyAdmission(ctx context.Context, tx *sql.Tx, original *dspb.AccountRoleOwnership, admission *base.RoleAdmissionIdentity) error {
	if !accountdirectory.ValidAdmission(admission) || admission.PlayerId != original.GetRole().GetPlayerId() || admission.OwnerRevision != original.GetOwnerRevision() {
		return ErrOriginalRole
	}
	var highest, closed uint64
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT highest_generation,closed_through,identity_proto FROM `"+s.schema+"`.account_role_admission WHERE player_id=? AND scene_instance_id=? FOR UPDATE", admission.PlayerId, []byte(admission.SceneInstanceId)).Scan(&highest, &closed, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOriginalRole
	}
	if err != nil {
		return err
	}
	registered := &base.RoleAdmissionIdentity{}
	if proto.Unmarshal(raw, registered) != nil || highest != admission.Generation || closed >= admission.Generation || !proto.Equal(registered, admission) {
		return ErrOriginalRole
	}
	return nil
}

func (s *SQLRoleAuthority) LockCurrent(ctx context.Context, tx *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error) {
	if s == nil || tx == nil || player == 0 || !roleSchemaPattern.MatchString(s.schema) {
		return nil, ErrOriginalRole
	}
	if err := placement.LockRoleRoutes(ctx, tx, s.schema, []uint64{player}); err != nil {
		return nil, err
	}
	var account []byte
	var revision, listing uint64
	err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+s.schema+"`.account_role WHERE player_id=? FOR UPDATE", player).Scan(&account, &revision, &listing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOriginalRole
	}
	if err != nil {
		return nil, err
	}
	if !accountdirectory.ValidAccount(string(account)) || revision == 0 || listing != 0 {
		return nil, ErrOriginalRole
	}
	return &dspb.AccountRoleOwnership{Account: string(account), OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: player}}, nil
}

func (s *Store) withPlayerTx(ctx context.Context, player uint64, action func(*sql.Tx, *dspb.AccountRoleOwnership) error) error {
	if s.roles == nil {
		return ErrOriginalRole
	}
	return assetop.WithTxRetryConfig(ctx, s.db, s.txCfg, func(tx *sql.Tx) error {
		original, err := s.roles.LockCurrent(ctx, tx, player)
		if err != nil {
			return err
		}
		if original.GetRole().GetPlayerId() != player || !accountdirectory.ValidAccount(original.GetAccount()) || original.GetOwnerRevision() == 0 {
			return ErrOriginalRole
		}
		return action(tx, original)
	})
}

func requireItemOriginal(row *inboxpb.PlayerInboxItemRow, original *dspb.AccountRoleOwnership) error {
	if row == nil {
		return nil
	}
	if row.PlayerId != original.GetRole().GetPlayerId() || row.OwnerRevision == 0 || row.OwnerRevision != original.GetOwnerRevision() || !bytes.Equal([]byte(row.OwnerAccount), []byte(original.GetAccount())) {
		return ErrOriginalRole
	}
	return nil
}

// 持有同P后普通读足以与新增/消费/封存互斥，不在二级索引上追加反向行锁。
// 所有未终局条目都必须属于同一持久原账号；缺原件、未知状态一律保留护栏。
func requirePendingOriginalsTx(ctx context.Context, tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
	var seq uint64
	err := tx.QueryRowContext(ctx, "SELECT `seq` FROM "+itemTable+" WHERE `shard`=? AND `player_id`=? AND (`state` IS NULL OR `state` NOT IN "+terminalStatesSQL+") AND (`state` IS NULL OR `state`<>? OR `owner_account` IS NULL OR BINARY `owner_account`<>? OR `owner_revision` IS NULL OR `owner_revision`<>?) LIMIT 1",
		RowShard(original.GetRole().GetPlayerId()), original.GetRole().GetPlayerId(), uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING), []byte(original.GetAccount()), original.GetOwnerRevision()).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrOriginalRole
}
