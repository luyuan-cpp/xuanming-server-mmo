package team

import (
	"context"
	"errors"
	"fmt"
	"match/internal/config"

	teampb "proto/team"
)

// 组队存储(设计文档 docs/design/team-system.md §C)。
//
// 权威数据全部在 SQL(team_authority / team_membership / team_invite_index / team_application_index,
// 实现见 sql_store.go、sql_mutate.go、sql_launch.go)。本文件定义存储层对服务层的契约:
// 读 → 调纯函数规则 → 在事务里提交。不做推送、不发 scene 事件、不打指标:这些副作用由服务层根据返回值完成。
//
// 并发模型:Store 无可变状态,可被多个 goroutine 共享;正确性靠事务内的行锁与版本比较。
// 超时:所有 SQL 调用都用调用方 ctx。补偿动作由调用方传独立 ctx。

const (
	// TeamIdleTTLSeconds 队伍空闲期限(24h),每次提交或触碰续期。
	TeamIdleTTLSeconds = config.TeamIdleTTLSeconds
	// TouchThresholdSeconds 记录剩余期限低于它(12h)时 GetMyTeam 续期。
	TouchThresholdSeconds = 43200
	// CommitRetries 观察到的记录在提交前变化时的最多提交次数,耗尽回 ErrStateChanged。
	CommitRetries = 3
)

// SessionLoader 批量读成员会话状态(player:session:{<id>})。读失败的成员给 SessionUnknown
// (或整批不返回),规则层据此 fail-closed。服务层用 playercontract 实现;测试注入桩。
type SessionLoader func(ctx context.Context, playerIds []uint64) Sessions

// Store 组队存储。sessions 可为 nil(全员 SessionUnknown:不转让队长、转让目标一律视为离线)。
type Store struct {
	sql      *sqlTeamStore
	sessions SessionLoader
	cfg      RuleConfig
}

type bindMode uint8

const (
	bindModeCaller bindMode = iota + 1
	bindModeTarget
	bindModeCreate
)

// Binding 决定 Mutate 操作哪一个队伍(§C.5 "t 的来源")。整个 Mutate(含重试)绑定同一个 tid,绝不换队。
type Binding struct {
	mode     bindMode
	playerId uint64
	teamId   uint64
}

// BindCaller 带 expected_team_id 的写 RPC:调用者当前队伍 ≠ expectedTeamId(含 0)时不写,返回 OutcomeNotBound。
func BindCaller(playerId, expectedTeamId uint64) Binding {
	return Binding{mode: bindModeCaller, playerId: playerId, teamId: expectedTeamId}
}

// BindTarget RespondInvite(team_id)/ ApplyJoinTeam(pre 阶段读到的目标 tid):操作 teamId,
// 调用者自己的 tid 只交给规则判断"是否已在别的队"。
func BindTarget(playerId, teamId uint64) Binding {
	return Binding{mode: bindModeTarget, playerId: playerId, teamId: teamId}
}

// BindCreate 建队:记录必须不存在。
func BindCreate(playerId, newTeamId uint64) Binding {
	return Binding{mode: bindModeCreate, playerId: playerId, teamId: newTeamId}
}

// PlayerId / TeamId 供日志使用。
func (b Binding) PlayerId() uint64 { return b.playerId }
func (b Binding) TeamId() uint64   { return b.teamId }

// Snapshot 一次读的结果:玩家索引与某队记录出自同一个事务(§C.4 前提)。
type Snapshot struct {
	PlayerId     uint64
	PlayerTeamId uint64 // 玩家索引当前 tid;0 = 无队(含索引缺失)
	PlayerEpoch  uint64 // 玩家索引 epoch
	TeamId       uint64 // 本次读的队伍
	Version      uint64 // 记录 ver;0 = 记录不存在
	Record       *teampb.TeamRecord
	// RecordTTLSeconds 记录距空闲期限的剩余秒数;-2 = 记录不存在。
	RecordTTLSeconds int64
	NowMs            uint64 // SQL 服务器时钟
}

// IndexEntry 某玩家索引在一次事务中的 (tid, epoch)。
type IndexEntry struct {
	TeamId uint64
	Epoch  uint64
}

// Outcome Mutate 的结局。
type Outcome uint8

const (
	// OutcomeCommitted 已提交;MutateResult.Commit 有值。
	OutcomeCommitted Outcome = iota + 1
	// OutcomeUnchanged 规则判定成功但无需写(幂等重放 / 无变化)。
	OutcomeUnchanged
	// OutcomeRejected 业务拒绝或冲突耗尽;看 Code / CodeParam。没有写入。
	OutcomeRejected
	// OutcomeNotBound BindCaller 未绑定:调用者已不在 expected 队伍。没有写入。
	OutcomeNotBound
	// OutcomeRecordMissing 绑定的队伍记录不存在或已解散。没有写入。
	OutcomeRecordMissing
)

// CommitResult 一次成功的提交。
type CommitResult struct {
	TeamId   uint64
	Version  uint64 // 提交后的 ver(解散时是墓碑的版本)
	Decision Decision
	// Indexes Joined / Kept / Left 每个人提交后的 (tid, epoch),出自同一个事务。
	// 推送视图时只能用这里的 epoch,且只给 TeamId 与视图 team_id 一致的人推(§C.4)。
	Indexes map[uint64]IndexEntry
	NowMs   uint64 // 本次事务的 SQL 时钟(规则用的 now)
}

// MutateResult Mutate 的完整结果。
type MutateResult struct {
	Outcome   Outcome
	Code      uint32
	CodeParam uint64
	// Snapshot 提交事务里读到的调用者 tid/epoch + 绑定队伍的记录。
	Snapshot Snapshot
	// Decision 规则输出(Committed / Unchanged / 规则拒绝时有意义)。
	Decision Decision
	Commit   *CommitResult
	// Conflicts 本次 Mutate 遇到的记录变化次数。服务层据此记 team_commit_retry_total(§G.3);存储层自己不打指标。
	Conflicts int
}

// Mutate 名册写操作的唯一入口(§C.5)。
// 返回 error 仅表示 SQL / 序列化等内部故障(服务层回 ErrInternal);业务结果看 Outcome。
// StartTeamMatch 的开战 launch 不走这里(§E.1:见 sqlTeamStore.createLaunch)。
func (s *Store) Mutate(ctx context.Context, bind Binding, op Op) (*MutateResult, error) {
	return s.sql.mutate(ctx, s, bind, op)
}

// ReadFree 自由读(GetMyTeam、CreateTeam 预检、失败回包里的调用者视图):调用者索引与它指向的记录出自同一个事务。
func (s *Store) ReadFree(ctx context.Context, playerId uint64) (*Snapshot, error) {
	return s.sql.read(ctx, playerId, 0, true)
}

// NeedsTouch 记录剩余期限不足 12h 时需要续期。
func NeedsTouch(snap *Snapshot) bool {
	return snap != nil && snap.Record != nil && snap.RecordTTLSeconds < TouchThresholdSeconds
}

// Touch ver 仍等于 snap.Version 才续期记录,不改 ver。返回 false 表示 ver 已变或记录已不存在(不是错误)。
func (s *Store) Touch(ctx context.Context, snap *Snapshot) (bool, error) {
	return s.sql.touch(ctx, snap)
}

// MembersSnapshot 记录与每名成员索引出自同一个事务。
type MembersSnapshot struct {
	TeamId  uint64
	Version uint64             // 0 = 记录不存在
	Record  *teampb.TeamRecord // nil = 记录不存在或已解散
	NowMs   uint64
	// Indexes 记录里每名成员的 (tid, epoch)。只给 TeamId == 本队的成员推送,用这里的 epoch。
	Indexes map[uint64]IndexEntry
}

// ReadMembers 读记录与全体成员索引。记录不存在返回 Record=nil、error=nil;成员索引与记录矛盾是完整性故障,返回 error。
func (s *Store) ReadMembers(ctx context.Context, teamId uint64) (*MembersSnapshot, error) {
	return s.sql.readMembers(ctx, teamId)
}

// InviteIndexEntry team_invite_index 里的一项。
type InviteIndexEntry struct {
	TeamId     uint64
	ExpireAtMs uint64
	// Score ExpireAtMs 的十进制串,只用于 PruneInvite 的比较。
	Score string
}

// ListInvites 列出 playerId 收到的未过期邀请。
func (s *Store) ListInvites(ctx context.Context, playerId uint64) (nowMs uint64, entries []InviteIndexEntry, err error) {
	return s.sql.listInvites(ctx, playerId)
}

// PruneInvite 期限仍等于 ListInvites 看到的值才删(不会误删队长刚重邀写入的新项)。
func (s *Store) PruneInvite(ctx context.Context, playerId, teamId uint64, score string) (bool, error) {
	return s.sql.pruneInvite(ctx, playerId, teamId, score)
}

// PinnedResult 钉版本提交(开战 launch / 清开战锁)的结果。
type PinnedResult struct {
	// Code / CodeParam 规则拒绝,没有写入。
	Code      uint32
	CodeParam uint64
	// Commit 非 nil = 已提交。
	Commit *CommitResult
	// Retry 没有提交:版本已变。开战整轮重来。
	Retry bool
}

// EndMatchStop 清开战锁没有写入时的原因。
type EndMatchStop uint8

const (
	// EndMatchTokenMismatch 没有这把锁对应的原 launch,不写。
	EndMatchTokenMismatch EndMatchStop = iota + 1
	// EndMatchDeadline 成局还没有确定的结局(未到可清锁的状态),不写,恢复循环下一轮再看。
	EndMatchDeadline
)

// read 读 playerId 的索引 + teamId 的记录。
func (s *Store) read(ctx context.Context, playerId, teamId uint64) (*Snapshot, error) {
	return s.sql.read(ctx, playerId, teamId, false)
}

func (s *Store) loadSessions(ctx context.Context, rec *teampb.TeamRecord) Sessions {
	if s.sessions == nil || rec == nil {
		return nil
	}
	return s.sessions(ctx, MemberIds(rec))
}

// validateCommitSets 提交前校验 Decision 的集合契约(见 Decision 注释)。违反即程序缺陷,
// 返回 error(服务层回 ErrInternal),绝不带着错的集合去写。
func validateCommitSets(teamId uint64, d Decision) error {
	if teamId == 0 {
		return errors.New("team: 提交的 team_id 为 0")
	}
	seen := map[uint64]string{}
	for name, group := range map[string][]uint64{"Joined": d.Joined, "Kept": d.Kept, "Left": d.Left} {
		for _, pid := range group {
			if pid == 0 {
				return fmt.Errorf("team: %s 含 player_id 0", name)
			}
			if prev, dup := seen[pid]; dup {
				return fmt.Errorf("team: player %d 同时出现在 %s 与 %s", pid, prev, name)
			}
			seen[pid] = name
		}
	}
	if d.Record == nil {
		if len(d.Joined)+len(d.Kept) != 0 {
			return errors.New("team: 解散提交不能有 Joined / Kept")
		}
	} else {
		if d.Record.GetTeamId() != teamId {
			return fmt.Errorf("team: 记录 team_id %d 与绑定 %d 不一致", d.Record.GetTeamId(), teamId)
		}
		if !sameIdSet(MemberIds(d.Record), append(append([]uint64(nil), d.Joined...), d.Kept...)) {
			return errors.New("team: Joined ∪ Kept 与新记录成员集合不一致")
		}
		if FindMember(d.Record, d.Record.GetLeaderId()) == nil {
			return fmt.Errorf("team: 队长 %d 不在成员里", d.Record.GetLeaderId())
		}
		if len(d.Record.GetMembers()) > Capacity {
			return fmt.Errorf("team: 成员数 %d 超过容量", len(d.Record.GetMembers()))
		}
	}
	added := map[uint64]bool{}
	for _, a := range d.InvitesAdded {
		if a.InviteeId == 0 || added[a.InviteeId] {
			return fmt.Errorf("team: InvitesAdded 含非法或重复项 %d", a.InviteeId)
		}
		added[a.InviteeId] = true
	}
	removed := map[uint64]bool{}
	for _, pid := range d.InvitesRemoved {
		if pid == 0 || added[pid] || removed[pid] {
			return fmt.Errorf("team: InvitesRemoved 含非法、重复或与 InvitesAdded 重叠的项 %d", pid)
		}
		removed[pid] = true
	}
	return nil
}

func sameIdSet(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[uint64]int, len(a))
	for _, id := range a {
		set[id]++
	}
	for _, id := range b {
		if set[id] == 0 {
			return false
		}
		set[id]--
	}
	return true
}
