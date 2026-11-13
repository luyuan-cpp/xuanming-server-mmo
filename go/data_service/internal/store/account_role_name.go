package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	base "proto/common/base"
	"shared/accountdirectory"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 名字行和建角提交绑在一起:「预留 → 绑定」状态机的目录一侧(docs/design/guild-phase2/03-names.md §3.4a)。
//
// 名字行在名字片,角色行(提交点)在玩家片,两边是 data_service 自己的两个目录逻辑库,没有跨片事务。所以按下面三条串起来,
// 每一步都只写一个逻辑库、各自幂等,事务里不调 RPC:
//
//   - 绑定:建角第 ③ 步(commitRole)提交之后,先在玩家片重读角色行,确认角色确已提交、名字就是这个,再在名字片把名字行
//     从预留置为已绑定(PlayerNameStore.Bind)。失败不影响建角结局(角色已经提交):名字行停在预留态,仍然受下面第三条保护;
//     由同一建角的重放(预读那条路)或核账工具(cmd/create_residual 的 name_unbound)补上。
//   - 删角释放:删角写完墓碑之后(finishRemoval,或重放读到墓碑),先在玩家片核角色行确是墓碑,再删名字行(不看状态)。
//     小表 account_role_removal 那一行在墓碑事务里留着,释放成功才删:没做完时后台 ResumeRemovals 按它接着做。
//   - 预留态的释放(ReleasePlayerName:login 建角回滚、运维清孤儿)之前,在玩家片证明这名角色永远不会提交
//     (SealUncommittedRole):在役 → 拒绝;墓碑 → 已证明;没有角色行而有路由行 → 写墓碑,迟到的建角在提交点撞上它被拒。
//     已绑定的名字 ReleasePlayerName 一律拒绝,那一步在名字片(PlayerNameStore.Release 的 bound_ms 条件)。

// roleNameRegistry 是目录要的几步名字操作,*PlayerNameStore 实现。拆成接口只为在编排的单测里换内存替身。
//   - Bind:建角提交之后绑定;UndoRestore:绑定时补登记撞上并发删角,撤掉本次补写的那一行(undoRestoreIfRemoved)。
//   - ReleaseRemoved:删角写完墓碑之后释放。
//   - BoundTo:后台判陈旧写墓碑之前找「提交过」的证据(committedEvidence)。
type roleNameRegistry interface {
	Bind(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (BindOutcome, uint64, error)
	UndoRestore(ctx context.Context, playerID uint64, norm string, restoredMs uint64) (ReleaseOutcome, error)
	ReleaseRemoved(ctx context.Context, playerID uint64, norm string) (ReleaseOutcome, error)
	BoundTo(ctx context.Context, playerID uint64) (bool, error)
}

const (
	// nameBindTimeout 是提交之后绑定名字的预算。不跟请求 ctx:走到这里角色已经提交,login 的截止可能已过,
	// 绑定还是要做完,少一条要核账补的残留;也不能拖太久占着 handler。
	nameBindTimeout = time.Second
	// removedRoleSettleTimeout 是删角写完墓碑之后收尾(释放名字、删小表那一行)的预算,理由同上。没做完由后台接着做。
	removedRoleSettleTimeout = time.Second
)

// ConfigurePlayerNames 接上名字登记表,在装配完成、对外服务之前调。两边必须开在同一组目录逻辑库上(svc 用同一份 MySQLConfig 开)。
// 不配(nil)时建角不绑定名字、删角不释放名字:名字行停在预留态,ReleasePlayerName 照样要过玩家片的证明。只有单测和工具这样用。
func (s *AccountDirectoryStore) ConfigurePlayerNames(names *PlayerNameStore) {
	if names != nil {
		s.names = names
	}
}

// bindRoleNameBestEffort 是建角提交之后的一步:名字行从预留置为已绑定。失败只记日志并计数(DirectoryEvents.NameBindFailed),
// 不改建角结局。
func (s *AccountDirectoryStore) bindRoleNameBestEffort(ctx context.Context, role *base.AccountSimplePlayer) {
	if s.names == nil || role.GetName() == "" {
		// 没有名字的角色(早于名字功能的建角路径、单测)没有可绑的。
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nameBindTimeout)
	defer cancel()
	if err := s.bindRoleName(ctx, role); err != nil {
		reason := NameBindFailureError
		var failure *nameBindError
		if errors.As(err, &failure) {
			reason = failure.reason
		}
		s.noteNameBindFailed(reason)
		logx.Errorf("[player-name] name of a committed role is not bound, replay the create or run create_residual (plan, then -repair name): player=%d reason=%s: %v",
			role.GetPlayerId(), reason, err)
	}
}

// nameBindError 是绑定没做完的原因,reason 取 NameBindFailure* 常量(进计数)。不带它的错误按 NameBindFailureError 计。
type nameBindError struct {
	reason string
	err    error
}

func (e *nameBindError) Error() string { return e.err.Error() }
func (e *nameBindError) Unwrap() error { return e.err }

func bindFailure(reason string, err error) error { return &nameBindError{reason: reason, err: err} }

// bindRoleName 先在玩家片复核角色确已提交、名字就是这个,再绑定。回错误表示这一次没绑上(名字行可能还是预留态);
// 错误能用 errors.As 取出 *nameBindError 时,里面是原因。
func (s *AccountDirectoryStore) bindRoleName(ctx context.Context, role *base.AccountSimplePlayer) error {
	key, ok := residualNameKeyOf(role.GetName())
	if !ok {
		// 现行字符集规则算不出判重键:名字行是用当时的规则登记的,这里认不出它,也就绑不了。
		return bindFailure(NameBindFailureRulesChanged, errors.New("role name is rejected by the current charset rules, cannot derive its name_norm"))
	}
	owned, err := s.steps.readOwner(ctx, role.GetPlayerId())
	if err != nil {
		return fmt.Errorf("re-read the committed role: %w", err)
	}
	if owned == nil || owned.GetAccount() == "" || owned.GetRole().GetName() != role.GetName() {
		// 提交点之后才会走到这里,角色行一定在。对不上说明有别的写改了它(删角、改名都不该发生):不绑,留给人看。
		return bindFailure(NameBindFailureRoleMismatch, errors.New("the role row is missing, removed or carries another name; refusing to bind"))
	}
	// 补登记(BindRestored)把同一个时刻写进 created_ms 和 bound_ms,撤销按它认出本次写的那一行。
	nowMs := uint64(time.Now().UnixMilli())
	outcome, holder, err := s.names.Bind(ctx, role.GetPlayerId(), key.display, key.norm, nowMs)
	if err != nil {
		return err
	}
	switch outcome {
	case BindBound, BindAlreadyBound:
		return nil
	case BindRestored:
		// 名字行在提交之前就没了(被误释放,或者恢复点错开)。现在补回来了,但中间那段时间别人能登记同一个名字。
		logx.Errorf("[player-name] committed role had lost its name registration, re-registered and bound it: player=%d", role.GetPlayerId())
		err := s.undoRestoreIfRemoved(ctx, role.GetPlayerId(), key.norm, nowMs)
		// 名字行补回来了(或者撞上删角又撤掉了):两种都删缓存。名字缺着的那段时间读路径可能记了「没有名字」的负缓存,
		// 补回之后不删就要等它过期才查得到;撤掉的那种,补回期间读到的名字不能再展示。
		s.invalidateNameCache(ctx, role.GetPlayerId())
		return err
	case BindTaken:
		return bindFailure(NameBindFailureTaken, fmt.Errorf("one name held by two roles: the name is registered to player %d", holder))
	case BindConflict:
		return bindFailure(NameBindFailureConflict, fmt.Errorf("the player already holds another name registration"))
	default:
		return fmt.Errorf("bind outcome %s", outcome)
	}
}

// undoRestoreIfRemoved 是补登记之后的复读(docs/design/guild-phase2/03-names.md §3.4a「补登记撞上删角」)。
//
// 复核角色在役到补登记提交之间没有锁:这名角色可能正好被删,删角收尾释放名字的时候这一行还不在,补回来的名字就绑在墓碑上,
// 小表那一行也删了,再没有谁会放它。所以补登记提交之后再读一次角色行:
//   - 已是墓碑:撤掉本次写的那一行(UndoRestore 按 player_id、判重键、本次的登记时刻和绑定时刻比较,别人的行碰不到);
//   - 还在役:留着。删角的收尾排在墓碑提交之后,墓碑又在这次复读之后,所以收尾一定看得到这一行,会按删角释放放掉它。
//
// 复读失败、角色行不在时什么都不动,回错误进日志:宁可留一条要人看的名字,也不在没有证据时删。
func (s *AccountDirectoryStore) undoRestoreIfRemoved(ctx context.Context, player uint64, norm string, restoredMs uint64) error {
	owned, err := s.steps.readOwner(ctx, player)
	if err != nil {
		return fmt.Errorf("re-read the role after re-registering its name: %w", err)
	}
	if owned == nil {
		return bindFailure(NameBindFailureRoleMismatch, errors.New("the role row is missing after its name was re-registered; leaving the registration for review"))
	}
	if owned.GetAccount() != "" {
		return nil
	}
	outcome, err := s.names.UndoRestore(ctx, player, norm, restoredMs)
	if err != nil {
		return bindFailure(NameBindFailureRoleRemoved, fmt.Errorf("the role was removed while its name was re-registered, and undoing the re-registration failed: %w", err))
	}
	return bindFailure(NameBindFailureRoleRemoved, fmt.Errorf("the role was removed while its name was re-registered; undo outcome=%s", outcome))
}

// settleRemovedRole 是删角写完墓碑之后的收尾:释放这名角色登记的名字,再删小表那一行。回非 nil 表示这一次没做完。
// 小表那一行在墓碑事务里留着(finishRemoval),所以收尾没做完(名字片不可达、进程崩溃)时,后台 ResumeRemovals 照样扫得到它,
// beginRemoval 读到墓碑走回这里接着做。没做完不改删角结局(墓碑已提交):名字留在已删的角色名下,只是暂时没人能用,不会一名两角。
// 每次没做完都计一次(DirectoryEvents.RemovalSettleFailed),后台收尾按它把这一行算成没收完(RemovalResumeResult.Unsettled)。
func (s *AccountDirectoryStore) settleRemovedRole(ctx context.Context, player uint64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removedRoleSettleTimeout)
	defer cancel()
	if err := s.releaseRemovedRoleName(ctx, player); err != nil {
		s.noteRemovalSettleFailed(RemovalSettleReleaseName)
		logx.Errorf("[player-name] name of a removed role is not released yet, the removal resumer retries: player=%d: %v", player, err)
		return fmt.Errorf("release the name of removed role %d: %w", player, err)
	}
	if err := s.dropSettledRemoval(ctx, player); err != nil {
		s.noteRemovalSettleFailed(RemovalSettleDropRow)
		logx.Errorf("[account-role-removal] removal row of a settled role not dropped, the removal resumer retries: player=%d: %v", player, err)
		return fmt.Errorf("drop the removal row of settled role %d: %w", player, err)
	}
	return nil
}

// releaseRemovedRoleName 先在玩家片核角色行确是墓碑,再按墓碑里留着的原名删名字行。墓碑不可逆,这个 player_id 再也不会有角色。
func (s *AccountDirectoryStore) releaseRemovedRoleName(ctx context.Context, player uint64) error {
	if s.names == nil {
		return nil
	}
	owned, err := s.steps.readOwner(ctx, player)
	if err != nil {
		return fmt.Errorf("re-read the removed role: %w", err)
	}
	if owned == nil {
		return nil
	}
	if owned.GetAccount() != "" {
		// 只在墓碑提交之后才调到这里。角色还在役就绝不释放:那是在役角色的名字。
		return errors.New("the role is not removed, refusing to release its name")
	}
	name := owned.GetRole().GetName()
	if name == "" {
		// 早于名字功能的角色,以及 SealUncommittedRole、后台判陈旧写的墓碑(原件里没有名字)。
		return nil
	}
	key, ok := residualNameKeyOf(name)
	if !ok {
		// 现行字符集规则算不出当年的判重键,这一行只能人工清。不回错误:重试也算不出来,小表那一行照常删。
		logx.Errorf("[player-name] name of a removed role cannot be released: the current charset rules reject it, clean it by hand: player=%d", player)
		return nil
	}
	outcome, err := s.names.ReleaseRemoved(ctx, player, key.norm)
	if err != nil {
		return err
	}
	// 已删角色的名字不再展示:删掉它的名字缓存(删失败只记日志,最多按正缓存 TTL 再展示一阵)。
	s.invalidateNameCache(ctx, player)
	logx.Infof("[player-name] released the name of a removed role: player=%d outcome=%s", player, outcome)
	return nil
}

// dropSettledRemoval 删墓碑之后收尾完的小表行:在玩家片的事务里锁角色行,核它确是墓碑才删。在役的角色那一行是删角进行中的准备令牌,不能碰。
func (s *AccountDirectoryStore) dropSettledRemoval(ctx context.Context, player uint64) error {
	return s.playerTx(ctx, player, func(tx *sql.Tx) error {
		old, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, player), player)
		if err != nil {
			return err
		}
		if old.Account != "" {
			return errors.New("the role is not removed, keeping its removal row")
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM account_role_removal WHERE player_id=?", player)
		return err
	})
}

// RoleSeal 是释放预留态名字之前,玩家片上这名角色的结论(SealUncommittedRole)。
type RoleSeal uint8

const (
	// RoleSealCommitted 角色已经提交、还在役:名字属于它,不能释放。
	RoleSealCommitted RoleSeal = iota + 1
	// RoleSealRemoved 已经是墓碑(删了角,或者之前封过):这个 player_id 永远不会再有角色。
	RoleSealRemoved
	// RoleSealSealed 本次写了墓碑:没有角色行、有路由行。之后迟到的建角在提交点撞上已有行被拒(checkCreateReplay)。
	RoleSealSealed
	// RoleSealUnrouted 没有角色行,也没有带归属区的路由行:提交点要求路由行,此刻提交不了。没写墓碑:写不出合法的护栏状态
	// (归属区是护栏状态的必填项)。之后要提交,得先为这个已放弃的 id 登记路由再发建角,login 回滚之后不会再碰这个 id。
	RoleSealUnrouted
)

// String 进日志,固定的英文词。
func (r RoleSeal) String() string {
	switch r {
	case RoleSealCommitted:
		return "committed"
	case RoleSealRemoved:
		return "removed"
	case RoleSealSealed:
		return "sealed"
	case RoleSealUnrouted:
		return "unrouted"
	default:
		return "unknown"
	}
}

// roleSealAttempts:写墓碑撞上并发的墓碑(后台判陈旧不锁路由行)时回滚重来一次,重来就读得到那块墓碑。
const roleSealAttempts = 2

// errRoleSealRaced 只在本文件内流转:插墓碑撞键,别的写者先写了这一行,回滚重读。
var errRoleSealRaced = errors.New("account_role row appeared while sealing")

// SealUncommittedRole 在释放预留态的名字之前,证明这名角色永远不会提交(docs/design/guild-phase2/03-names.md §3.4a)。
//
// 玩家片上一个事务:先锁路由行,再锁角色行。建角的提交点(commitRole)也是先锁路由行、再严格插入角色行,
// 两者在路由行上串行,所以「这里读到没有角色行」和「之后提交」不可能同时成立:
//   - 在役的角色行 → RoleSealCommitted,什么都不写;
//   - 墓碑 → RoleSealRemoved,什么都不写;
//   - 没有角色行、有带归属区的路由行 → 写墓碑(原件只带 player_id 和归属区,和后台判陈旧写的一样)→ RoleSealSealed;
//   - 没有角色行、没有路由行 → RoleSealUnrouted,什么都不写(见常量注释)。
//
// 墓碑不可逆:这个 player_id 从此不能建角。调用方只在这个 id 已被放弃时调(login 建角回滚,玩家重试会铸新 id;运维清孤儿)。
// 写者闸门冻结、库不可达时回错误,调用方据此拒绝释放(fail-closed:宁可留一条孤儿名字,也不能一名两角)。
func (s *AccountDirectoryStore) SealUncommittedRole(ctx context.Context, player uint64) (RoleSeal, error) {
	if player == 0 {
		return 0, accountdirectory.ErrInvalid
	}
	for attempt := 1; ; attempt++ {
		seal, err := s.sealUncommittedRoleOnce(ctx, player)
		if errors.Is(err, errRoleSealRaced) && attempt < roleSealAttempts {
			continue
		}
		return seal, err
	}
}

func (s *AccountDirectoryStore) sealUncommittedRoleOnce(ctx context.Context, player uint64) (RoleSeal, error) {
	var seal RoleSeal
	err := s.playerTx(ctx, player, func(tx *sql.Tx) error {
		var err error
		seal, err = sealRoleTx(ctx, tx, player)
		return err
	})
	if err != nil {
		return 0, err
	}
	if seal == RoleSealSealed {
		logx.Infof("[player-name] sealed an uncommitted role before releasing its reserved name: player=%d", player)
	}
	return seal, nil
}

// sealRoleTx 是证明的事务体,在调用方开在这名玩家所在片上的事务里跑(锁序、结论见 SealUncommittedRole)。
// 核账工具用维护身份直连、不过写者闸门,在同一个事务里先锁本库闸门行再调它(create_residual_sql.go 的 sealOrphanRole)。
// 插墓碑撞键回 errRoleSealRaced,调用方回滚重来。
func sealRoleTx(ctx context.Context, tx *sql.Tx, player uint64) (RoleSeal, error) {
	var route RouteRow
	routed := true
	if err := scanRouteRow(tx.QueryRowContext(ctx, routeLockRowSQL, routeShardArg(player), player), &route, false); errors.Is(err, sql.ErrNoRows) {
		routed = false
	} else if err != nil {
		return 0, err
	}
	old, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, player), player)
	switch {
	case err == nil && old.Account != "":
		return RoleSealCommitted, nil
	case err == nil:
		return RoleSealRemoved, nil
	case !errors.Is(err, accountdirectory.ErrMissing):
		return 0, err
	}
	if !routed || !route.HasHome() {
		return RoleSealUnrouted, nil
	}
	raw, err := proto.Marshal(&base.AccountSimplePlayer{PlayerId: player, ZoneId: route.HomeZone})
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO account_role(player_id,account,role_proto,owner_revision,updated_ms,initial_player_commit,fence_version) VALUES (?,?,?,1,"+dbNowMsSQL+",0,1)",
		player, []byte{}, raw)
	var duplicate *mysql.MySQLError
	if errors.As(err, &duplicate) && duplicate.Number == mysqlErrDupEntry {
		return 0, errRoleSealRaced
	}
	if err != nil {
		return 0, err
	}
	return RoleSealSealed, nil
}
