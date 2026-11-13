package authority

import (
	"context"
	"database/sql"
	"errors"

	pb "proto/match"
	"shared/accountdirectory"
	"shared/rolefence"
	"shared/roleidentity"
)

var ErrOriginalPlayer = errors.New("匹配原会话角色归属已失效")

// ConfigureRoleFence 只在装配时调用。角色归属锁本库的护栏副本(docs/design/horizontal-scale-logical-db.md §3),
// 目录只有 data_service 连,这里不跨库读。副本表由 -migrate 建,这里只读核形状。
// states 是目录的权威状态:副本判不了时经它自愈,事务外读 home_zone 也走它。admissions 校验入场登记。
func (s *Store) ConfigureRoleFence(ctx context.Context, states rolefence.StateReader, admissions rolefence.AdmissionVerifier) error {
	if states == nil || admissions == nil {
		return errors.New("匹配角色护栏缺目录客户端")
	}
	if err := rolefence.VerifySchema(ctx, s.DB); err != nil {
		return err
	}
	s.fence = rolefence.NewGuard(s.DB, states)
	s.fenceStates, s.admissions = states, admissions
	s.roleGuard = lockOriginalFences
	// 独立状态机测试会预先注入入场替身,这里不覆盖。
	if s.entryGuard == nil {
		s.entryGuard = s.requireEntryOriginalsTx
	}
	return nil
}

// Fenced 跑一段自己开事务、自己回滚的事务体。副本判不了时事务体回 *rolefence.StaleError,这里读权威状态写入副本再跑一次。
// RPC 只发生在两次尝试之间,事务里不调 RPC。第二次还判不了就是真实拒绝,回 ErrOriginalPlayer。
// 目录里没有这名角色同样是 ErrOriginalPlayer,和旧的 account_role 缺行一样。
func (s *Store) Fenced(ctx context.Context, attempt func() error) error {
	err := s.fence.RunTx(ctx, attempt)
	if errors.Is(err, rolefence.ErrRoleNotFound) {
		return ErrOriginalPlayer
	}
	return err
}

// VerifyOriginalOwner 是客户端入口的归属预检(docs/design/gate-online-identity.md §9.3):不加锁读本库护栏副本,
// 判 ctx 里原会话身份的角色此刻是否仍归它。战报列表与回放、作战脚本列表这些只读方法不经 Fenced,这是它们唯一的归属检查;
// 排队、应战、组队、网络探测等写入仍在提交点持副本行锁按同一份原件复核。
//
// 只判归属、不看冻结(rolefence.CheckOwner)。确定不归(已换主、已删、目录里没有)回 ErrOriginalPlayer;
// 其余错误是这一次判不了,入口按可重试处理。
func (s *Store) VerifyOriginalOwner(ctx context.Context) error {
	original, ok := roleidentity.Original(ctx)
	if !ok || s == nil || s.fence == nil {
		return ErrOriginalPlayer
	}
	err := s.fence.VerifyOwner(ctx, rolefence.Expect{PlayerID: original.Role.GetPlayerId(), Account: original.Account, OwnerRevision: original.OwnerRevision, Reject: ErrOriginalPlayer})
	if errors.Is(err, rolefence.ErrRoleNotFound) {
		return ErrOriginalPlayer
	}
	return err
}

// OwnerRejected 判 VerifyOriginalOwner 的错误是不是确定不归;为假就是这一次判不了。
func OwnerRejected(err error) bool { return errors.Is(err, ErrOriginalPlayer) }

// lockOriginalFences 按 player_id 升序锁全部原件的副本行,先于所有匹配票、成局、意图行。
// 原件必须带完整身份:修订号 0 在 Expect 里的意思是「没有原身份」,这里不能放进去。
func lockOriginalFences(ctx context.Context, tx *sql.Tx, originals []*pb.MatchAuthorityTicketRecord) error {
	expects := make([]rolefence.Expect, 0, len(originals))
	for _, original := range originals {
		if original == nil || original.PlayerId == 0 || !accountdirectory.ValidAccount(original.OwnerAccount) || original.OwnerRevision == 0 {
			return ErrOriginalPlayer
		}
		expects = append(expects, rolefence.Expect{PlayerID: original.PlayerId, Account: original.OwnerAccount, OwnerRevision: original.OwnerRevision, Reject: ErrOriginalPlayer})
	}
	_, err := rolefence.LockAll(ctx, tx, expects...)
	return err
}

// 角色P按uint64升序，先于所有匹配票/成局/意图行。测试只可显式注入独立目录替身。
func (s *Store) lockOriginalPlayers(ctx context.Context, tx *sql.Tx, originals []*pb.MatchAuthorityTicketRecord) error {
	if s == nil || s.roleGuard == nil || len(originals) == 0 {
		return ErrOriginalPlayer
	}
	return s.roleGuard(ctx, tx, originals)
}

func originalPlayer(ctx context.Context, player uint64) *pb.MatchAuthorityTicketRecord {
	original, ok := roleidentity.Original(ctx)
	if !ok || original.Role.GetPlayerId() != player {
		return &pb.MatchAuthorityTicketRecord{PlayerId: player}
	}
	return &pb.MatchAuthorityTicketRecord{PlayerId: player, OwnerAccount: original.Account, OwnerRevision: original.OwnerRevision}
}

func (s *Store) lockActor(ctx context.Context, tx *sql.Tx, player uint64) error {
	return s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{originalPlayer(ctx, player)})
}

// 只用于首次创建邀请的新对端捕获；重放/应答必须沿持久原件，不能再次调用它补新owner。
// 对端副本缺行时回 StaleError,由 Fenced 在事务外自愈后重来。没有冻结的副本行不会落后于目录:过户、删角之前全部参与者都先冻结了。
func (s *Store) captureChallengeOriginals(ctx context.Context, tx *sql.Tx, actor, target uint64) (*pb.MatchAuthorityTicketRecord, *pb.MatchAuthorityTicketRecord, error) {
	if s.fence == nil || actor == 0 || target == 0 || actor == target {
		return nil, nil, ErrOriginalPlayer
	}
	original := originalPlayer(ctx, actor)
	if !accountdirectory.ValidAccount(original.OwnerAccount) || original.OwnerRevision == 0 {
		return nil, nil, ErrOriginalPlayer
	}
	fences, err := rolefence.LockAll(ctx, tx,
		rolefence.Expect{PlayerID: actor, Account: original.OwnerAccount, OwnerRevision: original.OwnerRevision, Reject: ErrOriginalPlayer},
		rolefence.Expect{PlayerID: target, Reject: ErrOriginalPlayer})
	if err != nil {
		return nil, nil, err
	}
	peer := fences[target]
	return original, &pb.MatchAuthorityTicketRecord{PlayerId: target, OwnerAccount: peer.Account, OwnerRevision: peer.OwnerRevision}, nil
}

func (s *Store) ticketForActor(ctx context.Context, player uint64) (result *pb.MatchAuthorityTicketRecord, err error) {
	err = s.Fenced(ctx, func() (e error) { result, e = s.ticketForActorOnce(ctx, player); return e })
	return result, err
}

func (s *Store) ticketForActorOnce(ctx context.Context, player uint64) (*pb.MatchAuthorityTicketRecord, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, player); err != nil {
		return nil, err
	}
	actor := originalPlayer(ctx, player)
	if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND owner_account=? AND owner_revision=? AND state=1 AND expires_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)", player, actor.OwnerAccount, actor.OwnerRevision); err != nil {
		return nil, err
	}
	result, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", player))
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
		result = nil
	}
	if err != nil {
		return nil, err
	}
	if result != nil && (result.OwnerAccount != actor.OwnerAccount || result.OwnerRevision != actor.OwnerRevision) {
		result = nil
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
