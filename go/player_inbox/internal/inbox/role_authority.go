package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"

	base "proto/common/base"
	dspb "proto/data_service"
	inboxpb "proto/player_inbox"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/assetop"
	"shared/rolefence"
)

var ErrOriginalRole = errors.New("inbox: 原角色身份缺失、已换主或冻结")

// RoleAuthority 隔离角色护栏依赖(docs/design/horizontal-scale-logical-db.md §3)。原件由首次新入箱捕获，历史条目只核验、不补写。
type RoleAuthority interface {
	// RunTx 跑一次 action;action 回 *rolefence.StaleError 时读权威状态写入副本,再跑一次。action 自己开事务,RPC 只发生在两次之间。
	RunTx(ctx context.Context, action func() error) error
	// LockCurrent 锁本库副本,回当前归属。成功后持有到提交。副本判不了回 *rolefence.StaleError,冻结或已删除回 ErrOriginalRole。
	LockCurrent(ctx context.Context, tx *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error)
	// VerifyAdmission 在事务外向目录核完整入场原件,事务里不调 RPC。
	VerifyAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) error
}

// FenceRoleAuthority 读本库的 role_fence 副本,入场登记走目录 RPC。
type FenceRoleAuthority struct {
	guard      *rolefence.Guard
	admissions rolefence.AdmissionVerifier
}

func NewFenceRoleAuthority(db *sql.DB, states rolefence.StateReader, admissions rolefence.AdmissionVerifier) (*FenceRoleAuthority, error) {
	if db == nil || states == nil || admissions == nil {
		return nil, errors.New("inbox: 角色护栏缺连接、权威状态读取或入场登记校验")
	}
	return &FenceRoleAuthority{guard: rolefence.NewGuard(db, states), admissions: admissions}, nil
}

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

func (s *FenceRoleAuthority) RunTx(ctx context.Context, action func() error) error {
	return s.guard.RunTx(ctx, action)
}

// VerifyAdmission 不再和 CloseRoleAdmissions 同事务串行:校验之后、事务提交之前,这一世代可能刚好关闭。
// 不影响资产:归属变更先要冻结全部副本,事务在副本行锁上被拒;scene 关闭世代之前已结束自己的实体和回调,
// 迟到的 Fetch 只多记一次投递,迟到的 Ack 报的是它已落盘的结局,流行的 fence_epoch 照常挡旧 owner_epoch。
func (s *FenceRoleAuthority) VerifyAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) error {
	if !accountdirectory.ValidAdmission(admission) {
		return ErrOriginalRole
	}
	valid, err := s.admissions.VerifyRoleAdmission(ctx, admission)
	if err != nil {
		return err
	}
	if !valid {
		return ErrOriginalRole
	}
	return nil
}

func (s *FenceRoleAuthority) LockCurrent(ctx context.Context, tx *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error) {
	if s == nil || tx == nil || player == 0 {
		return nil, ErrOriginalRole
	}
	fences, err := rolefence.LockAll(ctx, tx, rolefence.Expect{PlayerID: player, Reject: ErrOriginalRole})
	if err != nil {
		return nil, err
	}
	fence := fences[player]
	return &dspb.AccountRoleOwnership{Account: fence.Account, OwnerRevision: fence.OwnerRevision, Role: &base.AccountSimplePlayer{PlayerId: player}}, nil
}

func (s *Store) withPlayerTx(ctx context.Context, player uint64, action func(*sql.Tx, *dspb.AccountRoleOwnership) error) error {
	if s.roles == nil {
		return ErrOriginalRole
	}
	err := s.roles.RunTx(ctx, func() error {
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
	})
	if errors.Is(err, rolefence.ErrRoleNotFound) {
		return ErrOriginalRole
	}
	return err
}

// requireAdmissionOriginal 在持锁的事务里核入场原件的归属修订。原件本身已在事务外由目录核过(VerifyAdmission)。
func requireAdmissionOriginal(admission *base.RoleAdmissionIdentity, original *dspb.AccountRoleOwnership) error {
	if admission.GetPlayerId() != original.GetRole().GetPlayerId() || admission.GetOwnerRevision() != original.GetOwnerRevision() {
		return ErrOriginalRole
	}
	return nil
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

// roleBlockerItem 是低基数枚举,进协调者的指标 label。
const roleBlockerItem = "player_inbox_item"

// RolePending 是收件箱的挂起全集,在 PrepareRoleFreeze 的事务里跑,此时已持有副本行锁。
// 未知/未终局条目属于原主,不能在维护或交易中换主。
func RolePending(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
	var pending uint64
	err := tx.QueryRowContext(ctx, "SELECT player_id FROM "+itemTable+" WHERE shard=? AND player_id=? AND (state IS NULL OR state NOT IN "+terminalStatesSQL+") LIMIT 1 FOR UPDATE", RowShard(state.PlayerId), state.PlayerId).Scan(&pending)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return roleBlockerItem, nil
}
