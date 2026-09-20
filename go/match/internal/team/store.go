package team

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	base "proto/common/base"
	component "proto/common/component"
	teampb "proto/team"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
)

// 组队存储(设计文档 docs/design/team-system.md §C)。
//
// 权威数据全部在 SharedRedis(单实例,§C.2):team:rec / team:<tid> / team:player / team:invite
// 只经 scripts.go 的 Lua 写。本文件负责"读 → 调纯函数规则 → CAS 提交 → 按返回值重试/修复"。
// 不做推送、不发 scene 事件、不打指标:这些副作用由服务层根据返回值完成。
//
// 并发模型:Store 无可变状态,可被多个 goroutine 共享;正确性全靠 ver CAS 与 Lua 原子判定。
// 超时:所有 Redis 调用都用调用方 ctx(EvalCtx / HgetCtx);提交前检查 ctx.Err(),
// 已过期放弃提交(§C.5 Mutate 第 3 步)。补偿动作由调用方传独立 ctx。

const (
	// TeamIdleTTLSeconds 队伍空闲 TTL(24h),每次提交或触碰续期。
	TeamIdleTTLSeconds = 86400
	// TouchThresholdSeconds 记录剩余 TTL 低于它(12h)时 GetMyTeam 执行 S_TOUCH。
	TouchThresholdSeconds = 43200
	// CommitRetries 版本冲突({0})的最多提交次数,耗尽回 ErrStateChanged。
	CommitRetries = 3

	freeReadRetries    = 3
	readMembersRetries = 3
	teamTTLArg         = "86400" // 与 TeamIdleTTLSeconds 同值,Lua 参数用字符串避免数字格式差异
)

var (
	// ErrUnstableRead 自由读重试耗尽(索引在两次读之间持续变化)。服务层回 ErrStateChanged。
	ErrUnstableRead = errors.New("team: 玩家索引持续变化,自由读重试耗尽")
	// ErrMembersChanged S_READ_MEMBERS 重试耗尽(成员表持续变化)。服务层放弃这次推送并记指标。
	ErrMembersChanged = errors.New("team: 成员表持续变化,S_READ_MEMBERS 重试耗尽")
)

// afterReadHook 测试缝(生产恒为 nil):Mutate 每轮 S_READ 之后、绑定校验之前调用,
// 用来把"迟到执行"的并发时序做成确定性用例(§I.3 #24)。照 logic.requeueFrontHook 的包级变量模式。
var afterReadHook func(bind Binding)

// beforeCommitEvalHook 测试缝(生产恒为 nil):S_COMMIT 的 EVAL 发出之前调用;返回非 nil error 时 commit 不再发 EVAL、
// 直接返回该错误。用来确定性地制造"提交结果未知":钩子里先自己执行同一条 EVAL 再返回错误 = 脚本已执行但回复丢失;
// 执行后返回 nil = go-redis 把同一条 EVAL 重发了一次(§E.3)。
var beforeCommitEvalHook func(d Decision, keys []string, args []any) error

// SessionLoader 批量读成员会话状态(player:session:<id>)。读失败的成员给 SessionUnknown
// (或整批不返回),规则层据此 fail-closed。服务层用 playercontract 实现;测试注入桩。
type SessionLoader func(ctx context.Context, playerIds []uint64) Sessions

// Store 组队存储。
type Store struct {
	rds      *redis.Redis
	sessions SessionLoader
	cfg      RuleConfig
}

// NewStore rds 必须是 svcCtx.SharedRedis(C++ scene 只读共享库,投影必须写在这里)。
// sessions 可为 nil(全员 SessionUnknown:不转让队长、转让目标一律视为离线)。
func NewStore(rds *redis.Redis, sessions SessionLoader, cfg RuleConfig) *Store {
	return &Store{rds: rds, sessions: sessions, cfg: cfg}
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

// BindCreate 建队:expectedVer="new",记录必须不存在。
func BindCreate(playerId, newTeamId uint64) Binding {
	return Binding{mode: bindModeCreate, playerId: playerId, teamId: newTeamId}
}

// PlayerId / TeamId 供测试缝与日志使用。
func (b Binding) PlayerId() uint64 { return b.playerId }
func (b Binding) TeamId() uint64   { return b.teamId }

// Snapshot 一次 S_READ 的结果:玩家索引与某队记录出自同一次原子读(§C.4 前提)。
type Snapshot struct {
	PlayerId     uint64
	PlayerTeamId uint64 // 玩家索引当前 tid;0 = 无队(含索引缺失)
	PlayerEpoch  uint64 // 玩家索引 epoch;索引缺失为 NowMs(不回退,见 scripts.go scriptRead)
	TeamId       uint64 // 本次读的队伍
	Version      uint64 // 记录 ver;0 = 记录不存在
	Record       *teampb.TeamRecord
	// RecordTTLSeconds 记录剩余 TTL:-2 不存在 / -1 没有 TTL。
	RecordTTLSeconds int64
	NowMs            uint64 // SharedRedis TIME
}

// IndexEntry 某玩家索引在一次原子操作中的 (tid, epoch)。
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
	// OutcomeRejected 业务拒绝或冲突耗尽;看 Code / CodeParam。没有写入(Repairs 除外)。
	OutcomeRejected
	// OutcomeNotBound BindCaller 未绑定:调用者已不在 expected 队伍。没有写入。
	OutcomeNotBound
	// OutcomeRecordMissing 绑定的队伍记录不存在。调用者索引仍指向它时已尝试 S_HEAL_ORPHAN(看 HealedOrphan)。
	OutcomeRecordMissing
)

// CommitResult 一次成功的 S_COMMIT。
type CommitResult struct {
	TeamId   uint64
	Version  uint64 // 提交后的 ver(解散时是被删记录的最后一版 +1)
	Decision Decision
	// Indexes Joined / Kept / Left 每个人提交后的 (tid, epoch),出自同一条 Lua。
	// 推送视图时只能用这里的 epoch,且只给 TeamId 与视图 team_id 一致的人推(§C.4)。
	Indexes map[uint64]IndexEntry
	NowMs   uint64 // 本轮 S_READ 的 Redis 时钟(规则用的 now)
	// PushTip 快照推送附带的原因(TeamSnapshotS2C.tip)。存储层不填不读,服务层在派发推送前设置;
	// 目前只有整队开战建票失败的 MATCH_FAILED 带(§E.1 第 7 步,parameters[0] = 出问题的成员)。
	PushTip *base.TipInfoMessage
}

// MutateResult Mutate 的完整结果。
type MutateResult struct {
	Outcome   Outcome
	Code      uint32
	CodeParam uint64
	// Snapshot 最后一轮 S_READ(调用者 tid/epoch + 绑定队伍的记录)。失败回包的视图取调用者自己的
	// 自由读(ReadFree),不要把这里的记录和另一时刻的 epoch 拼在一起。
	Snapshot Snapshot
	// Decision 最后一轮规则输出(Committed / Unchanged / 规则拒绝时有意义)。
	Decision Decision
	Commit   *CommitResult
	// Repairs {-2} 触发的修复提交(把索引已指向别队的成员移出本队,Reason=HEALED)。
	// 即使最终 Outcome 不是 Committed,这些修复也已经落盘,服务层要照常推送并通知被移出者的 scene。
	Repairs []CommitResult
	// HealedOrphan 调用者的孤儿索引已被 S_HEAL_ORPHAN 置 0(需通知其 scene)。
	HealedOrphan bool
	// Conflicts 本次 Mutate 遇到的 S_COMMIT 版本冲突({0})次数,含修复提交的冲突。
	// 服务层据此记 team_commit_retry_total(§G.3);存储层自己不打指标。
	Conflicts int
}

// Mutate 名册写操作的唯一入口(§C.5 Mutate 循环)。
// 返回 error 仅表示 Redis / 序列化等内部故障(服务层回 ErrInternal);业务结果看 Outcome。
// StartTeamMatch 的开战锁与 EndMatch 不走这里(§E.1:见 CommitMatchLock / EndMatch)。
func (s *Store) Mutate(ctx context.Context, bind Binding, op Op) (*MutateResult, error) {
	res := &MutateResult{}
	repairs := 0
	for {
		snap, err := s.read(ctx, bind.playerId, bind.teamId)
		if err != nil {
			return nil, err
		}
		res.Snapshot = *snap
		if hook := afterReadHook; hook != nil {
			hook(bind)
		}

		if bind.mode == bindModeCaller && (bind.teamId == 0 || snap.PlayerTeamId != bind.teamId) {
			res.Outcome = OutcomeNotBound
			return res, nil
		}
		if bind.mode == bindModeCreate {
			if snap.Record != nil {
				return rejected(res, ErrInternal, 0), nil // 新发的 team_id 已有记录:发号器故障
			}
		} else if snap.Record == nil {
			res.Outcome = OutcomeRecordMissing
			if bind.teamId != 0 && snap.PlayerTeamId == bind.teamId {
				healed, err := s.HealOrphan(ctx, bind.playerId, bind.teamId)
				if err != nil {
					return nil, err
				}
				res.HealedOrphan = healed
			}
			return res, nil
		}

		sessions := s.loadSessions(ctx, snap.Record)
		op.CallerTeamId = snap.PlayerTeamId
		d := Apply(op, snap.Record, snap.NowMs, sessions, s.cfg)
		res.Decision = d
		if d.Code != 0 {
			return rejected(res, d.Code, d.CodeParam), nil
		}
		if !d.Changed {
			res.Outcome = OutcomeUnchanged
			return res, nil
		}
		if ctx.Err() != nil {
			return rejected(res, ErrStateChanged, 0), nil
		}

		expectedVer := strconv.FormatUint(snap.Version, 10)
		if bind.mode == bindModeCreate {
			expectedVer = "new"
		}
		out, err := s.commit(ctx, bind.teamId, expectedVer, d, snap.NowMs)
		if err != nil {
			return nil, err
		}
		switch out.status {
		case commitOK:
			res.Outcome = OutcomeCommitted
			res.Commit = out.result
			return res, nil
		case commitConflict:
			res.Conflicts++
			if res.Conflicts >= CommitRetries {
				return rejected(res, ErrStateChanged, 0), nil
			}
		case commitMemberInTeam:
			return rejected(res, ErrMemberInTeam, d.Joined[out.index-1]), nil
		case commitInviteLimit:
			return rejected(res, ErrInviteLimit, d.InvitesAdded[out.index-1].InviteeId), nil
		case commitIndexMismatch:
			repairs++
			if repairs > Capacity || ctx.Err() != nil {
				return rejected(res, ErrStateChanged, 0), nil
			}
			fixed, err := s.repairIndexMismatch(ctx, snap, expectedVer, d, out.index, sessions)
			if err != nil {
				return nil, err
			}
			switch {
			case fixed == nil:
				return rejected(res, ErrStateChanged, 0), nil
			case fixed.status == commitOK:
				res.Repairs = append(res.Repairs, *fixed.result)
			case fixed.status == commitConflict:
				res.Conflicts++
				if res.Conflicts >= CommitRetries {
					return rejected(res, ErrStateChanged, 0), nil
				}
			default:
				return rejected(res, ErrStateChanged, 0), nil
			}
		}
		// 回到第 1 步:重新 S_READ 并重做绑定校验,重试不会换队、也不会在已删除的记录上重建。
	}
}

func rejected(res *MutateResult, code uint32, param uint64) *MutateResult {
	res.Outcome = OutcomeRejected
	res.Code, res.CodeParam = code, param
	return res
}

// ReadFree 自由读(GetMyTeam、CreateTeam 预检、失败回包里的调用者视图):
// 先 HGET 预读 tid,再 S_READ;返回的 tidNow 与预读不符就重读,保证调用者 epoch 与记录 ver 同源。
// 索引指向的记录不存在时执行 S_HEAL_ORPHAN 并重读(healed=true,调用方要通知该玩家的 scene)。
func (s *Store) ReadFree(ctx context.Context, playerId uint64) (snap *Snapshot, healed bool, err error) {
	for i := 0; i < freeReadRetries; i++ {
		tid, err := s.indexTeamId(ctx, playerId)
		if err != nil {
			return nil, healed, err
		}
		snap, err := s.read(ctx, playerId, tid)
		if err != nil {
			return nil, healed, err
		}
		if snap.PlayerTeamId != tid {
			continue
		}
		if tid != 0 && snap.Record == nil {
			ok, err := s.HealOrphan(ctx, playerId, tid)
			if err != nil {
				return nil, healed, err
			}
			healed = healed || ok
			continue
		}
		return snap, healed, nil
	}
	return nil, healed, ErrUnstableRead
}

// NeedsTouch 记录剩余 TTL 不足 12h(或没有 TTL)时需要 S_TOUCH。
func NeedsTouch(snap *Snapshot) bool {
	return snap != nil && snap.Record != nil && snap.RecordTTLSeconds < TouchThresholdSeconds
}

// Touch S_TOUCH:ver 仍等于 snap.Version 才续期记录、投影(缺失时按该版记录重写)与仍指向本队的成员索引。
// 不改 ver。返回 false 表示 ver 已变或记录已不存在(不是错误)。
func (s *Store) Touch(ctx context.Context, snap *Snapshot) (bool, error) {
	if snap == nil || snap.Record == nil {
		return false, nil
	}
	projPb, err := proto.Marshal(projectionOf(snap.Record))
	if err != nil {
		return false, fmt.Errorf("team: 序列化投影失败: %w", err)
	}
	tid := snap.Record.GetTeamId()
	keys := []string{recordKey(tid), projectionKey(tid)}
	for _, pid := range MemberIds(snap.Record) {
		keys = append(keys, playerIndexKey(pid))
	}
	raw, err := s.rds.EvalCtx(ctx, scriptTouch, keys,
		strconv.FormatUint(snap.Version, 10), teamTTLArg, string(projPb), strconv.FormatUint(tid, 10))
	if err != nil {
		return false, err
	}
	n, _ := raw.(int64)
	return n == 1, nil
}

// HealOrphan S_HEAL_ORPHAN:记录不存在且玩家索引仍指向 teamId 时把索引置 0、epoch+1。
func (s *Store) HealOrphan(ctx context.Context, playerId, teamId uint64) (bool, error) {
	raw, err := s.rds.EvalCtx(ctx, scriptHealOrphan,
		[]string{playerIndexKey(playerId), recordKey(teamId)},
		strconv.FormatUint(teamId, 10), teamTTLArg)
	if err != nil {
		return false, err
	}
	n, _ := raw.(int64)
	return n == 1, nil
}

// MembersSnapshot S_READ_MEMBERS 的结果:记录与每名成员索引出自同一次原子读。
type MembersSnapshot struct {
	TeamId  uint64
	Version uint64             // 0 = 记录不存在
	Record  *teampb.TeamRecord // nil = 记录不存在
	NowMs   uint64
	// Indexes 记录里每名成员的 (tid, epoch)。只给 TeamId == 本队的成员推送,用这里的 epoch。
	Indexes map[uint64]IndexEntry
}

// ReadMembers S_READ_MEMBERS:按 knownMembers 读,若记录成员集合与传入不等则用新成员表重读,
// 最多 3 次,仍不等返回 ErrMembersChanged。记录不存在返回 Record=nil、error=nil。
func (s *Store) ReadMembers(ctx context.Context, teamId uint64, knownMembers []uint64) (*MembersSnapshot, error) {
	members := knownMembers
	for i := 0; i < readMembersRetries; i++ {
		keys := make([]string, 0, 1+len(members))
		keys = append(keys, recordKey(teamId))
		for _, pid := range members {
			keys = append(keys, playerIndexKey(pid))
		}
		raw, err := s.rds.EvalCtx(ctx, scriptReadMembers, keys)
		if err != nil {
			return nil, err
		}
		arr, ok := raw.([]any)
		if !ok || len(arr) != 3+2*len(members) {
			return nil, fmt.Errorf("team: S_READ_MEMBERS 返回形态非法: %T len=%d", raw, len(arr))
		}
		out := &MembersSnapshot{TeamId: teamId, NowMs: parseUint(arr[2])}
		out.Version = parseUint(arr[0])
		if out.Version == 0 {
			return out, nil
		}
		rec, err := unmarshalRecord(arr[1])
		if err != nil {
			return nil, err
		}
		ids := MemberIds(rec)
		if !sameIdSet(ids, members) {
			members = ids
			continue
		}
		out.Record = rec
		out.Indexes = make(map[uint64]IndexEntry, len(members))
		for j, pid := range members {
			out.Indexes[pid] = IndexEntry{TeamId: parseUint(arr[3+2*j]), Epoch: parseUint(arr[4+2*j])}
		}
		return out, nil
	}
	return nil, ErrMembersChanged
}

// InviteIndexEntry team:invite:<pid> 里的一项。
type InviteIndexEntry struct {
	TeamId     uint64
	ExpireAtMs uint64
	// Score Redis 返回的 score 原字符串,只用于 PruneInvite 的 CAS 比较。
	Score string
}

// ListInvites S_INVITE_LIST:按 Redis 时钟剔除过期项并列出剩余项。
func (s *Store) ListInvites(ctx context.Context, playerId uint64) (nowMs uint64, entries []InviteIndexEntry, err error) {
	raw, err := s.rds.EvalCtx(ctx, scriptInviteList, []string{inviteIndexKey(playerId)})
	if err != nil {
		return 0, nil, err
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) < 1 || len(arr)%2 != 1 {
		return 0, nil, fmt.Errorf("team: S_INVITE_LIST 返回形态非法: %T", raw)
	}
	nowMs = parseUint(arr[0])
	for i := 1; i+1 < len(arr); i += 2 {
		score, _ := arr[i+1].(string)
		expire, err := strconv.ParseFloat(score, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("team: 邀请索引 score %q 非法: %w", score, err)
		}
		entries = append(entries, InviteIndexEntry{TeamId: parseUint(arr[i]), ExpireAtMs: uint64(expire), Score: score})
	}
	return nowMs, entries, nil
}

// PruneInvite S_INVITE_PRUNE:score 仍等于 ListInvites 看到的值才删(不会误删队长刚重邀写入的新项)。
func (s *Store) PruneInvite(ctx context.Context, playerId, teamId uint64, score string) (bool, error) {
	raw, err := s.rds.EvalCtx(ctx, scriptInvitePrune, []string{inviteIndexKey(playerId)},
		strconv.FormatUint(teamId, 10), score)
	if err != nil {
		return false, err
	}
	n, _ := raw.(int64)
	return n == 1, nil
}

// ---- §E 整队开战:钉版本提交与 EndMatch 专用循环 ----

const (
	// endMatchRoundTimeout EndMatch 每一轮(S_READ + 会话读 + S_COMMIT)的独立超时,不继承任何请求 ctx。
	endMatchRoundTimeout = 2 * time.Second
	// endMatchMaxDuration 进程内单调时钟兜底上限(5 人锁 83s,§E.1 EndMatch 第 4 步)。
	endMatchMaxDuration = 90 * time.Second
	// 冲突 / 故障退避:50ms 起翻倍,上限 1s,±20% 抖动。
	endMatchBackoffInitial = 50 * time.Millisecond
	endMatchBackoffMax     = time.Second
	endMatchBackoffJitter  = 0.2
)

// EndMatch 的测试缝(生产值不可在运行期修改):
//
//	afterMatchReadHook  每轮 S_READ 之后、判定之前调用,确定性地制造"锁期间别人提交"的 ver 冲突(§I.3 #30);
//	endMatchSleepFn     退避等待,测试换成记录器不真睡。
var (
	afterMatchReadHook func(teamId uint64)
	endMatchSleepFn    = time.Sleep
)

// PinnedResult 钉版本提交(开战锁 / EndMatch 清锁)的结果。
type PinnedResult struct {
	// Code / CodeParam 规则拒绝,没有写入。
	Code      uint32
	CodeParam uint64
	// Commit 非 nil = 已提交。
	Commit *CommitResult
	// Repairs {-2} 触发的修复提交(已落盘,服务层照常推 HEALED 并通知被移出者的 scene)。
	Repairs []CommitResult
	// Retry 没有提交:版本已变({0}),或刚做过修复。开战整轮重来;EndMatch 重读。
	Retry bool
}

// CommitMatchLock 开战锁提交入口(§E.1 第 6 步):在 snap(本轮 S_READ)的记录上加锁,expectedVer 钉死为
// snap.Version,**不**走 Mutate 的"重读并重算规则"循环 —— 那种循环会在新名单上加锁、却按旧 roster 建票,
// 破坏整队不可拆分。Retry=true 时调用方必须整轮重来(重新读记录、重排 roster、重新预检)。
// ctx 已过期时放弃提交(Code=ErrStateChanged)。error 只表示 Redis / 序列化故障。
func (s *Store) CommitMatchLock(ctx context.Context, snap *Snapshot, caller uint64, token string,
	roster []uint64, expireAtMs uint64,
) (*PinnedResult, error) {
	if snap == nil || snap.Record == nil || snap.Version == 0 {
		return nil, errors.New("team: 开战锁提交缺少记录快照")
	}
	d := LockMatch(snap.Record, caller, token, roster, expireAtMs, snap.NowMs)
	if d.Code != 0 {
		return &PinnedResult{Code: d.Code, CodeParam: d.CodeParam}, nil
	}
	if ctx.Err() != nil {
		return &PinnedResult{Code: ErrStateChanged}, nil
	}
	return s.commitPinned(ctx, snap, d)
}

// EndMatchStop EndMatch 的结局。
type EndMatchStop uint8

const (
	// EndMatchReleased 已清锁(EndMatchResult.Commit 有值)。
	EndMatchReleased EndMatchStop = iota + 1
	// EndMatchRecordMissing 记录已不存在,不写。
	EndMatchRecordMissing
	// EndMatchTokenMismatch 锁已被清或已重新加锁(token 不符),不写。
	EndMatchTokenMismatch
	// EndMatchLockExpired 锁按 Redis 时钟已自然过期,不写。
	EndMatchLockExpired
	// EndMatchDeadline 进程内上限耗尽仍未提交(持续冲突或 Redis 故障),锁靠自然过期。
	EndMatchDeadline
)

// EndMatchResult EndMatch 的完整结果。
type EndMatchResult struct {
	Stop   EndMatchStop
	Commit *CommitResult
	// Repairs 过程中落盘的 {-2} 修复提交,服务层照常推送。
	Repairs   []CommitResult
	Conflicts int
	// LastErr 最后一次 Redis / 序列化故障(循环退避后重试,只在 EndMatchDeadline 时有参考意义)。
	LastErr error
}

// EndMatch 清开战锁的专用循环(§E.1 EndMatch)。不复用 Mutate 的 3 次上限:锁期间队外玩家的申请 /
// 被邀请持续让 ver+1,数量不受本队控制,3 次就放弃会让全员停在 STARTING 直到锁过期。
//
//  1. S_READ 本队记录(nowMs 取 Redis TIME);
//  2. 记录不存在 / token 不符 / nowMs ≥ 锁截止 → 停止,不写;
//  3. 否则按 expectedVer=ver 提交清锁;{0} → 退避后回第 1 步;{-2} → 修复后立即回第 1 步。
//
// 截止:锁自己的截止时间(每轮用 nowMs 判)+ 进程内单调时钟 endMatchMaxDuration 兜底(§11.3)。
// 同步阻塞直到结局,调用方放在后台 goroutine;不推送、不打指标(服务层按结果处理)。
func (s *Store) EndMatch(teamId uint64, token string, ok bool) *EndMatchResult {
	res := &EndMatchResult{}
	start := time.Now()
	backoff := endMatchBackoffInitial
	wait := func() {
		endMatchSleepFn(jittered(backoff))
		if backoff *= 2; backoff > endMatchBackoffMax {
			backoff = endMatchBackoffMax
		}
	}
	for {
		if time.Since(start) >= endMatchMaxDuration {
			res.Stop = EndMatchDeadline
			return res
		}
		roundCtx, cancel := context.WithTimeout(context.Background(), endMatchRoundTimeout)
		stop, pinned, err := s.endMatchRound(roundCtx, teamId, token, ok)
		cancel()
		if err != nil {
			res.LastErr = err
			wait()
			continue
		}
		if stop != 0 {
			res.Stop = stop
			return res
		}
		res.Repairs = append(res.Repairs, pinned.Repairs...)
		if pinned.Commit != nil {
			res.Stop = EndMatchReleased
			res.Commit = pinned.Commit
			return res
		}
		if len(pinned.Repairs) > 0 {
			continue // 修复已落盘、ver 已变:立即重读
		}
		res.Conflicts++
		wait()
	}
}

// ReleaseMatchLockOnce 按 token 清开战锁(ok=false)的单轮版本:读 → 判定 → 钉版本提交,不退避、不重试,受调用方 ctx 约束。
// StartTeamMatch 在开战锁提交没拿到"已提交"时用它确认本 token 的锁不在(§E.3 结果未知窗口)。
// stop≠0:记录不存在 / token 不符 / 锁已过期,均不写;stop==0:PinnedResult.Commit≠nil 表示已清,否则没清(冲突或修复)。
func (s *Store) ReleaseMatchLockOnce(ctx context.Context, teamId uint64, token string) (EndMatchStop, *PinnedResult, error) {
	return s.endMatchRound(ctx, teamId, token, false)
}

// endMatchRound EndMatch 的一轮:读、判定、钉版本提交。stop≠0 表示应当停止(不写)。ctx 由调用方给单轮预算。
func (s *Store) endMatchRound(ctx context.Context, teamId uint64, token string, ok bool) (EndMatchStop, *PinnedResult, error) {
	// 只关心记录:S_READ 的玩家索引位传 0(team:player:0 恒不存在,脚本只读不写)。
	snap, err := s.read(ctx, 0, teamId)
	if err != nil {
		return 0, nil, err
	}
	if hook := afterMatchReadHook; hook != nil {
		hook(teamId)
	}
	switch {
	case snap.Record == nil:
		return EndMatchRecordMissing, nil, nil
	case snap.Record.GetMatchLockToken() != token:
		return EndMatchTokenMismatch, nil, nil
	case !MatchLockActive(snap.Record, snap.NowMs):
		return EndMatchLockExpired, nil, nil
	}
	d := ReleaseMatchLock(snap.Record, token, ok, snap.NowMs, s.loadSessions(ctx, snap.Record))
	if !d.Changed {
		return EndMatchTokenMismatch, nil, nil // 防御:上面已判过;规则层再拒即视为锁已不属于本次
	}
	pinned, err := s.commitPinned(ctx, snap, d)
	return 0, pinned, err
}

// commitPinned 按 snap.Version 钉死提交 d(不重读、不重算规则)。
// {-2,i}:把索引已指向别队的保留成员移出(repairIndexMismatch,修复提交同样钉在 snap.Version,与 Mutate 同口径),
// 原决策本次不提交,返回 Retry。d 不含 Joined / InvitesAdded,{-1}/{-3} 不可能出现,出现即程序缺陷。
func (s *Store) commitPinned(ctx context.Context, snap *Snapshot, d Decision) (*PinnedResult, error) {
	expectedVer := strconv.FormatUint(snap.Version, 10)
	out, err := s.commit(ctx, snap.TeamId, expectedVer, d, snap.NowMs)
	if err != nil {
		return nil, err
	}
	res := &PinnedResult{}
	switch out.status {
	case commitOK:
		res.Commit = out.result
	case commitConflict:
		res.Retry = true
	case commitIndexMismatch:
		res.Retry = true
		fixed, err := s.repairIndexMismatch(ctx, snap, expectedVer, d, out.index, s.loadSessions(ctx, snap.Record))
		if err != nil {
			return nil, err
		}
		if fixed != nil && fixed.status == commitOK {
			res.Repairs = append(res.Repairs, *fixed.result)
		}
	default:
		return nil, fmt.Errorf("team: 钉版本提交出现意外返回 status=%d index=%d", out.status, out.index)
	}
	return res, nil
}

// repairIndexMismatch 处理 S_COMMIT 对决策 d 返回的 {-2,index}(§C.5 Mutate 第 3 步、§C.6):把索引已指向别队的
// 保留成员移出本队,修复提交与原决策钉在同一个 expectedVer。
//
// 同一队可能有多名成员错位,而 Lua 只回报第一个错位下标。修复提交本身再返回 {-2,j} 时,把 fix.Kept[j-1] 并入
// 待移出集合,基于同一份 snap.Record 重新生成修复再提交(判定段只读,被拒的修复没有写入,ver 不变)。
// 若只修第一个就回原操作,重读得到同一 ver、同一记录、同一个第一错位者,永远修不动。
// 每轮至少多移出一名成员,成员数不超过 Capacity,全员移出后 Kept 为空、不可能再 {-2},所以 Capacity 轮内必然收敛。
//
// 返回 nil = 没有可修的(错位者已不在记录里);否则是最后一次修复提交的结果(commitOK 已落盘 / commitConflict
// ver 已变)。error 表示 Redis / 序列化故障,或下标越界、不收敛这类程序缺陷。
func (s *Store) repairIndexMismatch(ctx context.Context, snap *Snapshot, expectedVer string, d Decision,
	index int, sessions Sessions,
) (*commitOutcome, error) {
	kept := d.Kept
	var bad []uint64
	for round := 0; round < Capacity; round++ {
		if index < 1 || index > len(kept) {
			return nil, fmt.Errorf("team: {-2} 下标 %d 越界(Kept=%d)", index, len(kept))
		}
		bad = append(bad, kept[index-1])
		fix := RepairRemoveMembers(snap.Record, bad, snap.NowMs, sessions)
		if !fix.Changed {
			return nil, nil
		}
		out, err := s.commit(ctx, snap.TeamId, expectedVer, fix, snap.NowMs)
		if err != nil {
			return nil, err
		}
		if out.status != commitIndexMismatch {
			return out, nil
		}
		kept, index = fix.Kept, out.index
	}
	return nil, fmt.Errorf("team: {-2} 修复 %d 轮仍未收敛(待移出 %v)", Capacity, bad)
}

// jittered 给退避加 ±endMatchBackoffJitter 的均匀抖动(选时不要求确定性,math/rand 即可)。
func jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (1 + endMatchBackoffJitter*(2*rand.Float64()-1)))
}

// ---- 内部实现 ----

// read S_READ(playerId 的索引 + teamId 的记录)。
func (s *Store) read(ctx context.Context, playerId, teamId uint64) (*Snapshot, error) {
	raw, err := s.rds.EvalCtx(ctx, scriptRead, []string{playerIndexKey(playerId), recordKey(teamId)})
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) != 6 {
		return nil, fmt.Errorf("team: S_READ 返回形态非法: %T", raw)
	}
	snap := &Snapshot{
		PlayerId:     playerId,
		PlayerTeamId: parseUint(arr[0]),
		PlayerEpoch:  parseUint(arr[1]),
		TeamId:       teamId,
		Version:      parseUint(arr[2]),
		NowMs:        parseUint(arr[5]),
	}
	snap.RecordTTLSeconds, _ = arr[4].(int64)
	if snap.Version != 0 {
		rec, err := unmarshalRecord(arr[3])
		if err != nil {
			return nil, err
		}
		snap.Record = rec
	}
	return snap, nil
}

func (s *Store) indexTeamId(ctx context.Context, playerId uint64) (uint64, error) {
	v, err := s.rds.HgetCtx(ctx, playerIndexKey(playerId), indexFieldTid)
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 10, 64)
}

func (s *Store) loadSessions(ctx context.Context, rec *teampb.TeamRecord) Sessions {
	if s.sessions == nil || rec == nil {
		return nil
	}
	return s.sessions(ctx, MemberIds(rec))
}

type commitStatus uint8

const (
	commitOK commitStatus = iota + 1
	commitConflict
	commitMemberInTeam
	commitIndexMismatch
	commitInviteLimit
)

type commitOutcome struct {
	status commitStatus
	index  int // {-1,i} / {-2,i} / {-3,i} 的 i(1 起)
	result *CommitResult
}

// commit 把 Decision 翻译成 S_COMMIT 的 KEYS/ARGV 并解析返回值。
func (s *Store) commit(ctx context.Context, teamId uint64, expectedVer string, d Decision, nowMs uint64) (*commitOutcome, error) {
	if err := validateCommitSets(teamId, d); err != nil {
		return nil, err
	}
	keys, args, err := buildCommitArgs(teamId, expectedVer, d)
	if err != nil {
		return nil, err
	}
	if hook := beforeCommitEvalHook; hook != nil {
		if err := hook(d, keys, args); err != nil {
			return nil, err
		}
	}
	raw, err := s.rds.EvalCtx(ctx, scriptCommit, keys, args...)
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil, fmt.Errorf("team: S_COMMIT 返回形态非法: %T", raw)
	}
	code, _ := arr[0].(int64)
	switch code {
	case 1:
	case 0:
		return &commitOutcome{status: commitConflict}, nil
	case -1, -2, -3:
		if len(arr) != 2 {
			return nil, fmt.Errorf("team: S_COMMIT 拒绝分支返回形态非法: %v", arr)
		}
		idx, _ := arr[1].(int64)
		status := map[int64]commitStatus{-1: commitMemberInTeam, -2: commitIndexMismatch, -3: commitInviteLimit}[code]
		return &commitOutcome{status: status, index: int(idx)}, nil
	default:
		return nil, fmt.Errorf("team: S_COMMIT 未知返回码 %d", code)
	}

	people := make([]uint64, 0, len(d.Joined)+len(d.Kept)+len(d.Left))
	people = append(append(append(people, d.Joined...), d.Kept...), d.Left...)
	if len(arr) != 2+2*len(people) {
		return nil, fmt.Errorf("team: S_COMMIT 成功分支长度 %d,期望 %d", len(arr), 2+2*len(people))
	}
	result := &CommitResult{
		TeamId:   teamId,
		Version:  parseUint(arr[1]),
		Decision: d,
		Indexes:  make(map[uint64]IndexEntry, len(people)),
		NowMs:    nowMs,
	}
	for i, pid := range people {
		result.Indexes[pid] = IndexEntry{TeamId: parseUint(arr[2+2*i]), Epoch: parseUint(arr[3+2*i])}
	}
	return &commitOutcome{status: commitOK, result: result}, nil
}

// buildCommitArgs 组装 S_COMMIT 的 KEYS / ARGV(布局见 scripts.go scriptCommit)。
// 这里只翻译,不去重、不改集合:集合的正确性由 validateCommitSets 把关,
// Lua 的"先 ZREM 后 ZADD"是第二道保险(测试直接喂重叠集合验证)。
func buildCommitArgs(teamId uint64, expectedVer string, d Decision) ([]string, []any, error) {
	var recPb, projPb []byte
	if d.Record != nil {
		var err error
		if recPb, err = proto.Marshal(d.Record); err != nil {
			return nil, nil, fmt.Errorf("team: 序列化 TeamRecord 失败: %w", err)
		}
		if projPb, err = proto.Marshal(projectionOf(d.Record)); err != nil {
			return nil, nil, fmt.Errorf("team: 序列化 TeamInfo 投影失败: %w", err)
		}
	}
	keys := make([]string, 0, 2+len(d.Joined)+len(d.Kept)+len(d.Left)+len(d.InvitesAdded)+len(d.InvitesRemoved))
	keys = append(keys, recordKey(teamId), projectionKey(teamId))
	for _, group := range [][]uint64{d.Joined, d.Kept, d.Left} {
		for _, pid := range group {
			keys = append(keys, playerIndexKey(pid))
		}
	}
	for _, a := range d.InvitesAdded {
		keys = append(keys, inviteIndexKey(a.InviteeId))
	}
	for _, pid := range d.InvitesRemoved {
		keys = append(keys, inviteIndexKey(pid))
	}
	args := []any{
		expectedVer, string(recPb), string(projPb), teamTTLArg, strconv.FormatUint(teamId, 10),
		strconv.Itoa(len(d.Joined)), strconv.Itoa(len(d.Kept)), strconv.Itoa(len(d.Left)),
		strconv.Itoa(len(d.InvitesAdded)), strconv.Itoa(len(d.InvitesRemoved)),
	}
	for _, a := range d.InvitesAdded {
		args = append(args, strconv.FormatUint(a.ExpireAtMs, 10))
	}
	args = append(args, strconv.Itoa(MaxPendingInvitesPerInvitee))
	return keys, args, nil
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

// projectionOf 由记录生成 scene 读的投影 TeamInfo(members 集合语义,这里按 join_seq 升序只为确定性)。
func projectionOf(rec *teampb.TeamRecord) *component.TeamInfo {
	return &component.TeamInfo{
		TeamId:   rec.GetTeamId(),
		LeaderId: rec.GetLeaderId(),
		Members:  MemberIds(rec),
	}
}

func unmarshalRecord(v any) (*teampb.TeamRecord, error) {
	s, _ := v.(string)
	rec := &teampb.TeamRecord{}
	if err := proto.Unmarshal([]byte(s), rec); err != nil {
		return nil, fmt.Errorf("team: TeamRecord 反序列化失败: %w", err)
	}
	return rec, nil
}

// parseUint 解析 Lua 返回的十进制字符串或整数;空串 / 非法值为 0。
func parseUint(v any) uint64 {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseUint(x, 10, 64)
		if err != nil {
			return 0
		}
		return n
	case int64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	}
	return 0
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
