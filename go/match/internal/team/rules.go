package team

import (
	"sort"

	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

// 组队规则(设计文档 docs/design/team-system.md §D.1、§D.5、§D.6、§D.7)。
//
// 本文件只放纯函数:输入"记录快照 + nowMs(SharedRedis TIME)+ 在线态 + 配置",
// 输出决策(新记录、成员/邀请增减集、推送所需的原因)或 tip 错误码。
// 不做任何 I/O,不读 Go 墙钟(§C.4 唯一时钟源),可表驱动单测。
// 每次 S_COMMIT 版本冲突后,store 会重新读记录并重新调用这里,所以规则必须幂等可重算。

// ---- §D.1 常量 ----

const (
	// Capacity 队伍容量,必须等于 logic.kMaxBattleTeamSize(引擎每队上限)。team 不 import logic,
	// logic 也不能 import team(proto/team 生成物),两边测试各自钉住字面量 5(team_battle_test.go)。
	Capacity = 5

	// ApplicationTTLMs / MaxApplications 申请有效期与每队上限(满了淘汰最早一条,不报错)。
	ApplicationTTLMs uint64 = 120_000
	MaxApplications         = 10

	// InviteTTLMs / MaxInvitesPerTeam 邀请有效期与每队上限(满了淘汰最早一条)。
	InviteTTLMs       uint64 = 60_000
	MaxInvitesPerTeam        = 10

	// MaxPendingInvitesPerInvitee 每个被邀请人的待处理邀请上限,由 S_COMMIT 原子判定({-3,i})。
	MaxPendingInvitesPerInvitee = 10
)

// SessionState 规则层看到的会话状态(player:session:<id>,由 store 的 SessionLoader 提供)。
// 在线态只用于决策,不写进记录(AGENTS §11.6)。
type SessionState uint8

const (
	// SessionUnknown 未查询或读失败(map 缺项即此零值):既不算在线,也不据此判定"离线转让队长"(fail-closed)。
	SessionUnknown SessionState = iota
	// SessionAbsent key 不存在:正常登出或租约到期(§D.7 队长离线判定)。
	SessionAbsent
	// SessionPresent key 存在但不是 ONLINE(如 DISCONNECTING 重连宽限):不算在线,也不算离线。
	SessionPresent
	// SessionOnline SESSION_STATE_ONLINE。
	SessionOnline
)

// Sessions 成员 player_id → 会话状态。
type Sessions map[uint64]SessionState

// RuleConfig 规则层配置。
type RuleConfig struct {
	// AllowCrossZone 对应 yaml Team.AllowCrossZone(默认 false,§D.3)。
	AllowCrossZone bool
}

// OpKind 名册操作种类。StartTeamMatch 的开战锁不走这里(§E.1:见 LockMatch / ReleaseMatchLock)。
type OpKind uint8

const (
	OpCreate OpKind = iota + 1
	// OpRefresh GetMyTeam:只做过期清理与惰性转让队长。
	OpRefresh
	OpApply
	OpHandleApplication
	OpInvite
	OpRespondInvite
	OpLeave
	OpKick
	OpTransferLeader
	OpDisband
)

// Op 一次名册操作的输入。用下面的构造函数创建,不要手填 Kind 与字段组合。
type Op struct {
	Kind   OpKind
	Caller uint64
	// CallerTeamId 调用者索引当前指向的队伍(S_READ 的 tidNow;0 = 无队)。
	// 由 store.Mutate 从同一次 S_READ 填入,调用方不填。
	CallerTeamId uint64
	// CallerZone Create:建队者 home zone(即队伍 zone);Apply:申请人 home zone。
	CallerZone uint32
	// Target HandleApplication:申请人;Invite / Kick / TransferLeader:目标玩家。
	Target uint64
	// TargetZone Invite:被邀请人 home zone。
	TargetZone uint32
	// NewTeamId Create:新发的 team_id(BattleIDGen)。
	NewTeamId uint64
	// Accept HandleApplication 的 approve / RespondInvite 的 accept。
	Accept bool
}

// CreateOp 建队:caller 为队长,zone 为建队者 home zone,teamId 由发号器给出。
func CreateOp(caller, teamId uint64, zone uint32) Op {
	return Op{Kind: OpCreate, Caller: caller, NewTeamId: teamId, CallerZone: zone}
}

// RefreshOp GetMyTeam 的清理 + 惰性转让。
func RefreshOp(caller uint64) Op { return Op{Kind: OpRefresh, Caller: caller} }

// ApplyOp 申请加入(目标队伍由 store 的 BindTarget 决定)。
func ApplyOp(caller uint64, callerZone uint32) Op {
	return Op{Kind: OpApply, Caller: caller, CallerZone: callerZone}
}

// HandleApplicationOp 队长审批申请。
func HandleApplicationOp(caller, applicant uint64, approve bool) Op {
	return Op{Kind: OpHandleApplication, Caller: caller, Target: applicant, Accept: approve}
}

// InviteOp 队长邀请 target(targetZone 为其 home zone)。
func InviteOp(caller, target uint64, targetZone uint32) Op {
	return Op{Kind: OpInvite, Caller: caller, Target: target, TargetZone: targetZone}
}

// RespondInviteOp 被邀请人接受或拒绝(队伍由 BindTarget(team_id) 决定)。
func RespondInviteOp(caller uint64, accept bool) Op {
	return Op{Kind: OpRespondInvite, Caller: caller, Accept: accept}
}

// LeaveOp 离队。
func LeaveOp(caller uint64) Op { return Op{Kind: OpLeave, Caller: caller} }

// KickOp 队长踢人。
func KickOp(caller, target uint64) Op { return Op{Kind: OpKick, Caller: caller, Target: target} }

// TransferLeaderOp 队长转让。
func TransferLeaderOp(caller, target uint64) Op {
	return Op{Kind: OpTransferLeader, Caller: caller, Target: target}
}

// DisbandOp 队长解散。
func DisbandOp(caller uint64) Op { return Op{Kind: OpDisband, Caller: caller} }

// InviteIndexAdd 需要写进 team:invite:<InviteeId> 的反查项(S_COMMIT 的 IA 集合)。
type InviteIndexAdd struct {
	InviteeId  uint64
	ExpireAtMs uint64
}

// Decision 规则输出。
//
// 三种形态:
//   - Code != 0:业务拒绝,不写任何东西;Record 为 nil。CodeParam 是 parameters[0](0 = 不带)。
//   - Code == 0 && !Changed:成功但无需提交(幂等重放 / 无变化)。
//   - Code == 0 && Changed:按集合提交 S_COMMIT。Record 为新记录,nil 表示解散。
//
// 集合契约(store 提交前会校验):Joined ∪ Kept == 新记录成员;Left ⊇ 旧成员 \ 新成员;
// 三者两两不相交;InvitesRemoved ∩ InvitesAdded == ∅。
type Decision struct {
	Code      uint32
	CodeParam uint64

	Changed bool
	Record  *teampb.TeamRecord

	Joined []uint64
	Kept   []uint64
	Left   []uint64

	InvitesAdded   []InviteIndexAdd
	InvitesRemoved []uint64

	// Reason / Actor 供推送(TeamSnapshotS2C.reason / actor_id)。
	Reason teampb.TeamChangeReason
	Actor  uint64

	// LeaderOfflineTransferred 本次惰性转让了队长(§D.7)。
	LeaderOfflineTransferred bool
	// Disbanded 本次解散(显式解散,或最后一人离队 / 被修复移出)。
	Disbanded bool
	// RevokedInvitees 因解散而失效的未过期邀请的被邀请人(推 NotifyTeamEvent INVITE_REVOKED)。
	RevokedInvitees []uint64
	// RejectedApplicant 被拒绝的申请人(推 NotifyTeamEvent APPLICATION_REJECTED);0 = 无。
	RejectedApplicant uint64
	// InvitedPlayer 本次新发/刷新邀请的被邀请人(推 NotifyTeamInvite);0 = 无。
	InvitedPlayer uint64
}

// MatchLockActive 开战锁是否有效(nowMs 为 SharedRedis TIME,§C.4)。
func MatchLockActive(rec *teampb.TeamRecord, nowMs uint64) bool {
	return rec != nil && rec.GetMatchLockToken() != "" && nowMs < rec.GetMatchLockExpireAtMs()
}

// FindMember 在记录里找成员;没有返回 nil。
func FindMember(rec *teampb.TeamRecord, playerId uint64) *teampb.TeamMemberRecord {
	for _, m := range rec.GetMembers() {
		if m.GetPlayerId() == playerId {
			return m
		}
	}
	return nil
}

// MemberIds 按 join_seq 升序返回成员 player_id(显式顺序,§11.6)。
func MemberIds(rec *teampb.TeamRecord) []uint64 {
	members := append([]*teampb.TeamMemberRecord(nil), rec.GetMembers()...)
	sortMembers(members)
	ids := make([]uint64, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.GetPlayerId())
	}
	return ids
}

// PruneExpired 返回去掉已过期申请与邀请的副本(不改入参、不转让队长),
// 供视图层按 Redis 时钟过滤。rec 为 nil 返回 nil。
func PruneExpired(rec *teampb.TeamRecord, nowMs uint64) *teampb.TeamRecord {
	if rec == nil {
		return nil
	}
	work := proto.Clone(rec).(*teampb.TeamRecord)
	pruneExpired(work, nowMs)
	return work
}

// Apply 对记录快照执行一次名册操作(纯函数)。
// rec 为本轮 S_READ 读到的记录(Create 时必须为 nil);不会被修改。
func Apply(op Op, rec *teampb.TeamRecord, nowMs uint64, sessions Sessions, cfg RuleConfig) Decision {
	if op.Kind == OpCreate {
		return applyCreate(op, rec, nowMs)
	}
	if rec == nil {
		return reject(ErrNoTeam, 0)
	}
	work := proto.Clone(rec).(*teampb.TeamRecord)
	c := &ruleCtx{op: op, rec: work, now: nowMs, sessions: sessions, cfg: cfg}

	// 所有操作前先清理过期项并做惰性转让队长检查(§D.5 前言、§D.7)。
	c.appsExpired, c.invitesExpired = pruneExpired(work, nowMs)
	c.transferred = lazyTransferLeader(work, sessions)

	switch op.Kind {
	case OpRefresh:
	case OpApply:
		c.apply()
	case OpHandleApplication:
		c.handleApplication()
	case OpInvite:
		c.invite()
	case OpRespondInvite:
		c.respondInvite()
	case OpLeave:
		c.leave()
	case OpKick:
		c.kick()
	case OpTransferLeader:
		c.transferLeader()
	case OpDisband:
		c.disband()
	default:
		return reject(ErrInternal, 0)
	}
	return c.finish(rec)
}

// RepairRemoveMember 生成"把 playerId 从本队移除"的修复决策:S_COMMIT 返回 {-2,i}
// (保留成员的索引已指向别的队)时使用(§C.5 Mutate 第 3 步)。等价于 RepairRemoveMembers(rec, [playerId])。
func RepairRemoveMember(rec *teampb.TeamRecord, playerId uint64, nowMs uint64, sessions Sessions) Decision {
	return RepairRemoveMembers(rec, []uint64{playerId}, nowMs, sessions)
}

// RepairRemoveMembers 生成"把 playerIds 一次性从本队移除"的修复决策。同一队可能有多名成员的索引都已指向别队,
// 而 S_COMMIT 只回报第一个错位下标:修复提交本身再返回 {-2,j} 时,store 把新错位者并入集合、基于**同一份原记录**
// 重新生成本决策,Left 始终相对原记录计算,推送与 scene 信号不漏人。
// 这些成员的索引不属于本队,Lua 对 Left 里 tid 不等的索引不写,只回报其当前 (tid, epoch)。
// Actor 为集合里第一个实际被移出的成员。集合里没有一个在记录中时返回 !Changed。
func RepairRemoveMembers(rec *teampb.TeamRecord, playerIds []uint64, nowMs uint64, sessions Sessions) Decision {
	if rec == nil {
		return reject(ErrNoTeam, 0)
	}
	work := proto.Clone(rec).(*teampb.TeamRecord)
	c := &ruleCtx{rec: work, now: nowMs, sessions: sessions}
	c.appsExpired, c.invitesExpired = pruneExpired(work, nowMs)
	c.transferred = lazyTransferLeader(work, sessions)
	actor := uint64(0)
	for _, pid := range playerIds {
		if pid == 0 || FindMember(work, pid) == nil {
			continue
		}
		c.removeMember(pid)
		// 开战锁有效期间也可能走到这里(索引被淘汰后成员又进了别的队):锁内名单同步去掉他,
		// 保持"锁期间 match_lock_roster == members"(§E.1 第 6 步);已建的票与在途 gather 不受影响。
		work.MatchLockRoster = removeId(work.GetMatchLockRoster(), pid)
		if actor == 0 {
			actor = pid
		}
	}
	if actor != 0 {
		c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_HEALED, actor)
	}
	return c.finish(rec)
}

// ---- §E 整队开战:开战锁规则 ----

// CheckMatchStart 开战前置规则(§E.1 第 2 步,nowMs 为 S_READ 的 Redis 时钟):
// 调用者必须是队长,开战锁不能有效(重复开战回 ErrInMatch,客户端视为进行中,§D.6)。返回 0 表示通过。
func CheckMatchStart(rec *teampb.TeamRecord, caller, nowMs uint64) uint32 {
	switch {
	case rec == nil:
		return ErrNoTeam
	case caller == 0 || rec.GetLeaderId() != caller:
		return ErrNotLeader
	case MatchLockActive(rec, nowMs):
		return ErrInMatch
	}
	return 0
}

// CheckMatchTeamSize 副本人数规则(§E.1 第 3 步)。required 是副本组队人数上限(已按引擎每队上限收口,
// 0 = 未开放组队);人数少于 required 允许开战(J-5)。返回 0 表示通过。
func CheckMatchTeamSize(rec *teampb.TeamRecord, required uint32) uint32 {
	switch {
	case required == 0:
		return ErrDungeonNotOpen
	case uint32(len(rec.GetMembers())) > required:
		return ErrSizeExceeded
	}
	return 0
}

// MatchRoster 开战名单(§E.1 第 4 步):队长在前,其余按 join_seq 升序(显式字段,§11.6)。
// 这个顺序就是 gather 的 PrepareBattle / 快照顺序(站位按此顺序,J-20)。
func MatchRoster(rec *teampb.TeamRecord) []uint64 {
	leader := rec.GetLeaderId()
	ids := MemberIds(rec)
	roster := make([]uint64, 0, len(ids))
	if FindMember(rec, leader) != nil {
		roster = append(roster, leader)
	}
	for _, pid := range ids {
		if pid != leader {
			roster = append(roster, pid)
		}
	}
	return roster
}

// LockMatch 开战锁决策(§E.1 第 6 步)。rec 是开战这一轮 S_READ 读到的记录,store 按它的 ver 钉死提交、
// 不重算:预检期间任何名单变化都会让 ver+1,提交返回 {0},调用方整轮重来。
// 结果:写 token / expire / roster,Kept = 全体成员,Reason = MATCH_STARTED,Actor = caller。
// 不做惰性转让(同一 ver 的 Refresh 已判过);与所有写同口径地清理过期申请 / 邀请。
// 防御性复核:队长 / 锁 → 对应码;token 为空或截止不晚于 now → ErrInternal;
// roster 与成员集合不等 → ErrStateChanged(绝不在另一份名单上加锁)。
func LockMatch(rec *teampb.TeamRecord, caller uint64, token string, roster []uint64, expireAtMs, nowMs uint64) Decision {
	if code := CheckMatchStart(rec, caller, nowMs); code != 0 {
		return reject(code, 0)
	}
	if token == "" || expireAtMs <= nowMs {
		return reject(ErrInternal, 0)
	}
	if !sameIdSet(roster, MemberIds(rec)) {
		return reject(ErrStateChanged, 0)
	}
	work := proto.Clone(rec).(*teampb.TeamRecord)
	c := &ruleCtx{op: Op{Caller: caller}, rec: work, now: nowMs}
	c.appsExpired, c.invitesExpired = pruneExpired(work, nowMs)
	work.MatchLockToken = token
	work.MatchLockExpireAtMs = expireAtMs
	work.MatchLockRoster = append([]uint64(nil), roster...)
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_STARTED, caller)
	return c.finish(rec)
}

// ReleaseMatchLock EndMatch 清锁决策(§E.1 EndMatch 第 2–3 步)。只有锁仍属于 token、且按 nowMs 仍有效才清:
// Changed=true,Reason = ok ? MATCH_ENDED : MATCH_FAILED,Kept = 全体成员。
// 锁已被清、已重新加锁或已自然过期 → 返回零值 Decision(!Changed,调用方停止,不写)。
// 与所有写同口径:先清理过期项并做惰性转让检查(§D.7)。
func ReleaseMatchLock(rec *teampb.TeamRecord, token string, ok bool, nowMs uint64, sessions Sessions) Decision {
	if rec == nil || token == "" || rec.GetMatchLockToken() != token || !MatchLockActive(rec, nowMs) {
		return Decision{}
	}
	work := proto.Clone(rec).(*teampb.TeamRecord)
	c := &ruleCtx{rec: work, now: nowMs, sessions: sessions}
	c.appsExpired, c.invitesExpired = pruneExpired(work, nowMs)
	c.transferred = lazyTransferLeader(work, sessions)
	work.MatchLockToken = ""
	work.MatchLockExpireAtMs = 0
	work.MatchLockRoster = nil
	reason := teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED
	if ok {
		reason = teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED
	}
	c.markOp(reason, 0)
	return c.finish(rec)
}

// ---- 内部实现 ----

type ruleCtx struct {
	op       Op
	rec      *teampb.TeamRecord // 工作副本;disbanded 时成员已清空
	now      uint64
	sessions Sessions
	cfg      RuleConfig

	code      uint32
	codeParam uint64

	appsExpired    bool
	invitesExpired bool
	transferred    bool

	opChanged bool
	reason    teampb.TeamChangeReason
	actor     uint64

	disbanded bool
	revoked   []uint64
	extraLeft []uint64
	added     []InviteIndexAdd
	rejected  uint64
	invited   uint64
}

func reject(code uint32, param uint64) Decision {
	return Decision{Code: code, CodeParam: param}
}

func (c *ruleCtx) fail(code uint32, param uint64) {
	if c.code == 0 {
		c.code, c.codeParam = code, param
	}
}

func (c *ruleCtx) markOp(reason teampb.TeamChangeReason, actor uint64) {
	c.opChanged = true
	c.reason = reason
	c.actor = actor
}

func (c *ruleCtx) isLeader(playerId uint64) bool {
	return playerId != 0 && c.rec.GetLeaderId() == playerId
}

func (c *ruleCtx) full() bool { return len(c.rec.GetMembers()) >= Capacity }

func (c *ruleCtx) locked() bool { return MatchLockActive(c.rec, c.now) }

// zoneCheck 跨区校验(§D.3):zone 为 0 视为查不到 home zone,fail-closed;不通过时记下错误码并返回 false。
func (c *ruleCtx) zoneCheck(zone uint32) bool {
	if zone == 0 {
		c.fail(ErrHomeZoneUnknown, 0)
		return false
	}
	if !c.cfg.AllowCrossZone && zone != c.rec.GetZoneId() {
		c.fail(ErrCrossZoneDenied, 0)
		return false
	}
	return true
}

func (c *ruleCtx) apply() {
	caller := c.op.Caller
	if FindMember(c.rec, caller) != nil || c.op.CallerTeamId != 0 {
		c.fail(ErrMemberInTeam, 0)
		return
	}
	if !c.zoneCheck(c.op.CallerZone) {
		return
	}
	if c.full() {
		c.fail(ErrMembersFull, 0)
		return
	}
	if app := findApplication(c.rec, caller); app != nil {
		// 重复申请:刷新过期时间(§D.6),保留首次申请时刻,淘汰顺序不变。
		app.ExpireAtMs = c.now + ApplicationTTLMs
		app.ZoneId = c.op.CallerZone
	} else {
		c.rec.Applications = append(c.rec.Applications, &teampb.TeamApplicationRecord{
			PlayerId:    caller,
			ZoneId:      c.op.CallerZone,
			AppliedAtMs: c.now,
			ExpireAtMs:  c.now + ApplicationTTLMs,
		})
		evictOldestApplications(c.rec, caller)
	}
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED, caller)
}

func (c *ruleCtx) handleApplication() {
	applicant := c.op.Target
	if !c.isLeader(c.op.Caller) {
		c.fail(ErrNotLeader, 0)
		return
	}
	app := findApplication(c.rec, applicant)
	if !c.op.Accept {
		if app == nil {
			return // 拒绝不存在的申请:幂等成功
		}
		removeApplication(c.rec, applicant)
		c.rejected = applicant
		c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED, applicant)
		return
	}
	if FindMember(c.rec, applicant) != nil {
		return // 已是本队成员:幂等成功
	}
	if app == nil {
		c.fail(ErrApplicationNotFound, 0)
		return
	}
	if c.locked() {
		c.fail(ErrInMatch, 0)
		return
	}
	if c.full() {
		c.fail(ErrMembersFull, 0)
		return
	}
	// 按申请记录里的 zone 复核:开关中途改成 false 也拦得住(§D.3)。
	if !c.zoneCheck(app.GetZoneId()) {
		return
	}
	c.addMember(applicant, app.GetZoneId())
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_JOINED, applicant)
}

func (c *ruleCtx) invite() {
	target := c.op.Target
	if !c.isLeader(c.op.Caller) {
		c.fail(ErrNotLeader, 0)
		return
	}
	if target == 0 || target == c.op.Caller {
		c.fail(ErrPlayerId, 0)
		return
	}
	if FindMember(c.rec, target) != nil {
		c.fail(ErrMemberInTeam, target)
		return
	}
	if !c.zoneCheck(c.op.TargetZone) {
		return
	}
	if c.full() {
		c.fail(ErrMembersFull, 0)
		return
	}
	expire := c.now + InviteTTLMs
	if inv := findInvite(c.rec, target); inv != nil {
		inv.ExpireAtMs = expire
		inv.InviterId = c.op.Caller
		inv.ZoneId = c.op.TargetZone
	} else {
		c.rec.Invites = append(c.rec.Invites, &teampb.TeamInviteRecord{
			InviteeId:   target,
			InviterId:   c.op.Caller,
			ZoneId:      c.op.TargetZone,
			InvitedAtMs: c.now,
			ExpireAtMs:  expire,
		})
		evictOldestInvites(c.rec, target) // 被淘汰者在 finish 的差集里进入 InvitesRemoved
	}
	c.added = append(c.added, InviteIndexAdd{InviteeId: target, ExpireAtMs: expire})
	c.invited = target
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_INVITE_CHANGED, target)
}

func (c *ruleCtx) respondInvite() {
	caller := c.op.Caller
	inv := findInvite(c.rec, caller)
	if !c.op.Accept {
		if inv == nil {
			return // 拒绝不存在的邀请:幂等成功
		}
		removeInvite(c.rec, caller)
		c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_INVITE_CHANGED, caller)
		return
	}
	if FindMember(c.rec, caller) != nil {
		return // 已是本队成员:幂等成功
	}
	if inv == nil {
		c.fail(ErrInviteNotFound, 0)
		return
	}
	if c.locked() {
		c.fail(ErrInMatch, 0)
		return
	}
	if c.op.CallerTeamId != 0 && c.op.CallerTeamId != c.rec.GetTeamId() {
		c.fail(ErrMemberInTeam, 0)
		return
	}
	if c.full() {
		c.fail(ErrMembersFull, 0)
		return
	}
	if !c.zoneCheck(inv.GetZoneId()) {
		return
	}
	c.addMember(caller, inv.GetZoneId())
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_JOINED, caller)
}

func (c *ruleCtx) leave() {
	caller := c.op.Caller
	if FindMember(c.rec, caller) == nil {
		// 索引指向本队但记录里没有我(索引与记录矛盾):提交 L=[caller] 把索引置 0,
		// Lua 只在索引 tid 仍等于本队时才写。
		if c.op.CallerTeamId == c.rec.GetTeamId() {
			c.extraLeft = append(c.extraLeft, caller)
			c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_LEFT, caller)
		}
		return
	}
	if c.locked() {
		c.fail(ErrInMatch, 0)
		return
	}
	c.removeMember(caller)
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_LEFT, caller)
}

func (c *ruleCtx) kick() {
	target := c.op.Target
	if !c.isLeader(c.op.Caller) {
		c.fail(ErrKickNotLeader, 0)
		return
	}
	if target == c.op.Caller {
		c.fail(ErrKickSelf, 0)
		return
	}
	if FindMember(c.rec, target) == nil {
		c.fail(ErrMemberNotInTeam, target)
		return
	}
	if c.locked() {
		c.fail(ErrInMatch, 0)
		return
	}
	c.removeMember(target)
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_KICKED, target)
}

// transferLeader 判定顺序与设计稿 §D.5 的差异:"转给自己"放在"目标已是队长"之前,
// 否则队长转给自己会被当成幂等成功,4007 永远不可达;重放安全(§D.6)不受影响。
func (c *ruleCtx) transferLeader() {
	target := c.op.Target
	if target == 0 {
		c.fail(ErrPlayerId, 0)
		return
	}
	if target == c.op.Caller {
		c.fail(ErrAppointSelf, 0)
		return
	}
	if c.isLeader(target) {
		return // 目标已是队长:幂等成功
	}
	if !c.isLeader(c.op.Caller) {
		c.fail(ErrAppointNotLeader, 0)
		return
	}
	if FindMember(c.rec, target) == nil {
		c.fail(ErrMemberNotInTeam, target)
		return
	}
	if c.sessions[target] != SessionOnline {
		c.fail(ErrMemberOffline, target)
		return
	}
	if c.locked() {
		c.fail(ErrInMatch, 0)
		return
	}
	c.rec.LeaderId = target
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_LEADER_TRANSFERRED, target)
}

func (c *ruleCtx) disband() {
	if !c.isLeader(c.op.Caller) {
		c.fail(ErrDisbandNotLeader, 0)
		return
	}
	if c.locked() {
		c.fail(ErrInMatch, 0)
		return
	}
	c.rec.Members = nil
	c.dissolve()
	c.markOp(teampb.TeamChangeReason_TEAM_CHANGE_REASON_DISBANDED, c.op.Caller)
}

// addMember 加入成员(seq = next_join_seq++),并删掉此人在本队的申请与邀请。
func (c *ruleCtx) addMember(playerId uint64, zone uint32) {
	seq := c.rec.GetNextJoinSeq()
	for _, m := range c.rec.GetMembers() {
		if m.GetJoinSeq() >= seq {
			seq = m.GetJoinSeq() + 1 // 防御:next_join_seq 缺失或落后
		}
	}
	if seq == 0 {
		seq = 1
	}
	c.rec.Members = append(c.rec.Members, &teampb.TeamMemberRecord{
		PlayerId:   playerId,
		ZoneId:     zone,
		JoinedAtMs: c.now,
		JoinSeq:    seq,
	})
	c.rec.NextJoinSeq = seq + 1
	sortMembers(c.rec.Members)
	removeApplication(c.rec, playerId)
	removeInvite(c.rec, playerId)
}

// removeMember 移出成员;没有剩余成员则解散;移出的是队长则转给
// 在线且 join_seq 最小的成员,没有在线成员取 join_seq 最小的(§D.5 LeaveTeam)。
func (c *ruleCtx) removeMember(playerId uint64) {
	kept := c.rec.Members[:0:0]
	for _, m := range c.rec.GetMembers() {
		if m.GetPlayerId() != playerId {
			kept = append(kept, m)
		}
	}
	c.rec.Members = kept
	if len(kept) == 0 {
		c.dissolve()
		return
	}
	if c.rec.GetLeaderId() == playerId {
		c.rec.LeaderId = pickLeader(kept, c.sessions, false)
	}
}

// dissolve 标记解散:未过期邀请的被邀请人收 INVITE_REVOKED;全部邀请在差集里进入 InvitesRemoved。
func (c *ruleCtx) dissolve() {
	c.disbanded = true
	for _, inv := range c.rec.GetInvites() {
		c.revoked = append(c.revoked, inv.GetInviteeId())
	}
	c.rec.Invites = nil
	c.rec.Applications = nil
}

// finish 汇总成 Decision。orig 是规则执行前的原始记录(用于求成员/邀请差集)。
func (c *ruleCtx) finish(orig *teampb.TeamRecord) Decision {
	if c.code != 0 {
		return reject(c.code, c.codeParam)
	}
	d := Decision{
		LeaderOfflineTransferred: c.transferred,
		RejectedApplicant:        c.rejected,
		InvitedPlayer:            c.invited,
	}
	housekeeping := c.appsExpired || c.invitesExpired || c.transferred
	if !c.opChanged && !housekeeping {
		return d
	}
	d.Changed = true
	switch {
	case c.opChanged:
		d.Reason, d.Actor = c.reason, c.actor
	case c.transferred:
		d.Reason, d.Actor = teampb.TeamChangeReason_TEAM_CHANGE_REASON_LEADER_OFFLINE_TRANSFERRED, c.rec.GetLeaderId()
	case c.appsExpired:
		d.Reason = teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED
	default:
		d.Reason = teampb.TeamChangeReason_TEAM_CHANGE_REASON_INVITE_CHANGED
	}

	oldMembers := memberSet(orig)
	newMembers := map[uint64]bool{}
	if c.disbanded {
		d.Disbanded = true
		d.RevokedInvitees = c.revoked
	} else {
		sortMembers(c.rec.Members)
		d.Record = c.rec
		newMembers = memberSet(c.rec)
	}
	for _, id := range MemberIds(c.rec) {
		if oldMembers[id] {
			d.Kept = append(d.Kept, id)
		} else {
			d.Joined = append(d.Joined, id)
		}
	}
	for _, id := range MemberIds(orig) {
		if !newMembers[id] {
			d.Left = append(d.Left, id)
		}
	}
	for _, id := range c.extraLeft {
		if !newMembers[id] && !oldMembers[id] {
			d.Left = append(d.Left, id)
		}
	}

	d.InvitesAdded = c.added
	addedSet := map[uint64]bool{}
	for _, a := range c.added {
		addedSet[a.InviteeId] = true
	}
	newInvitees := map[uint64]bool{}
	for _, inv := range c.rec.GetInvites() {
		newInvitees[inv.GetInviteeId()] = true
	}
	seen := map[uint64]bool{}
	for _, inv := range orig.GetInvites() {
		id := inv.GetInviteeId()
		if newInvitees[id] || addedSet[id] || seen[id] { // ID := ID \ IA
			continue
		}
		seen[id] = true
		d.InvitesRemoved = append(d.InvitesRemoved, id)
	}
	return d
}

func applyCreate(op Op, rec *teampb.TeamRecord, nowMs uint64) Decision {
	switch {
	case rec != nil || op.NewTeamId == 0 || op.Caller == 0:
		return reject(ErrInternal, 0)
	case op.CallerZone == 0:
		return reject(ErrHomeZoneUnknown, 0)
	case op.CallerTeamId != 0:
		return reject(ErrMemberInTeam, 0)
	}
	record := &teampb.TeamRecord{
		TeamId:      op.NewTeamId,
		LeaderId:    op.Caller,
		ZoneId:      op.CallerZone,
		CreatedAtMs: nowMs,
		Members: []*teampb.TeamMemberRecord{{
			PlayerId:   op.Caller,
			ZoneId:     op.CallerZone,
			JoinedAtMs: nowMs,
			JoinSeq:    1,
		}},
		NextJoinSeq: 2,
	}
	return Decision{
		Changed: true,
		Record:  record,
		Joined:  []uint64{op.Caller},
		Reason:  teampb.TeamChangeReason_TEAM_CHANGE_REASON_CREATED,
		Actor:   op.Caller,
	}
}

// pruneExpired 按 Redis 时钟删掉 expire_at_ms <= nowMs 的申请与邀请
// (与 S_INVITE_LIST 的 ZREMRANGEBYSCORE -inf now 同口径)。
func pruneExpired(rec *teampb.TeamRecord, nowMs uint64) (appsExpired, invitesExpired bool) {
	apps := rec.Applications[:0:0]
	for _, a := range rec.GetApplications() {
		if a.GetExpireAtMs() > nowMs {
			apps = append(apps, a)
		} else {
			appsExpired = true
		}
	}
	rec.Applications = apps
	invites := rec.Invites[:0:0]
	for _, inv := range rec.GetInvites() {
		if inv.GetExpireAtMs() > nowMs {
			invites = append(invites, inv)
		} else {
			invitesExpired = true
		}
	}
	rec.Invites = invites
	return appsExpired, invitesExpired
}

// lazyTransferLeader §D.7:队长 player:session 不存在(SessionAbsent)时,
// 转给在线成员里 join_seq 最小的;没有在线成员不转。Unknown / Present 不触发。
func lazyTransferLeader(rec *teampb.TeamRecord, sessions Sessions) bool {
	leader := rec.GetLeaderId()
	if FindMember(rec, leader) == nil || sessions[leader] != SessionAbsent {
		return false
	}
	others := make([]*teampb.TeamMemberRecord, 0, len(rec.GetMembers()))
	for _, m := range rec.GetMembers() {
		if m.GetPlayerId() != leader {
			others = append(others, m)
		}
	}
	next := pickLeader(others, sessions, true)
	if next == 0 {
		return false
	}
	rec.LeaderId = next
	return true
}

// pickLeader 在候选里选在线且 join_seq 最小的;onlineOnly=false 时没有在线者退回 join_seq 最小的。
func pickLeader(candidates []*teampb.TeamMemberRecord, sessions Sessions, onlineOnly bool) uint64 {
	sorted := append([]*teampb.TeamMemberRecord(nil), candidates...)
	sortMembers(sorted)
	for _, m := range sorted {
		if sessions[m.GetPlayerId()] == SessionOnline {
			return m.GetPlayerId()
		}
	}
	if onlineOnly || len(sorted) == 0 {
		return 0
	}
	return sorted[0].GetPlayerId()
}

func sortMembers(members []*teampb.TeamMemberRecord) {
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].GetJoinSeq() != members[j].GetJoinSeq() {
			return members[i].GetJoinSeq() < members[j].GetJoinSeq()
		}
		return members[i].GetPlayerId() < members[j].GetPlayerId()
	})
}

// removeId 返回去掉 id 的副本(不改入参)。
func removeId(ids []uint64, id uint64) []uint64 {
	out := make([]uint64, 0, len(ids))
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

func memberSet(rec *teampb.TeamRecord) map[uint64]bool {
	set := make(map[uint64]bool, len(rec.GetMembers()))
	for _, m := range rec.GetMembers() {
		set[m.GetPlayerId()] = true
	}
	return set
}

func findApplication(rec *teampb.TeamRecord, playerId uint64) *teampb.TeamApplicationRecord {
	for _, a := range rec.GetApplications() {
		if a.GetPlayerId() == playerId {
			return a
		}
	}
	return nil
}

func removeApplication(rec *teampb.TeamRecord, playerId uint64) {
	kept := rec.Applications[:0:0]
	for _, a := range rec.GetApplications() {
		if a.GetPlayerId() != playerId {
			kept = append(kept, a)
		}
	}
	rec.Applications = kept
}

func findInvite(rec *teampb.TeamRecord, inviteeId uint64) *teampb.TeamInviteRecord {
	for _, inv := range rec.GetInvites() {
		if inv.GetInviteeId() == inviteeId {
			return inv
		}
	}
	return nil
}

func removeInvite(rec *teampb.TeamRecord, inviteeId uint64) {
	kept := rec.Invites[:0:0]
	for _, inv := range rec.GetInvites() {
		if inv.GetInviteeId() != inviteeId {
			kept = append(kept, inv)
		}
	}
	rec.Invites = kept
}

// evictOldestApplications 超过上限时按 applied_at_ms(同刻按 player_id)淘汰最早的;
// keep 是本次刚追加的申请人,永不淘汰(同一毫秒内多条时不能把刚加的挤掉)。
func evictOldestApplications(rec *teampb.TeamRecord, keep uint64) {
	for len(rec.Applications) > MaxApplications {
		oldest := -1
		for i, a := range rec.Applications {
			if a.GetPlayerId() == keep {
				continue
			}
			if oldest < 0 {
				oldest = i
				continue
			}
			o := rec.Applications[oldest]
			if a.GetAppliedAtMs() < o.GetAppliedAtMs() ||
				(a.GetAppliedAtMs() == o.GetAppliedAtMs() && a.GetPlayerId() < o.GetPlayerId()) {
				oldest = i
			}
		}
		if oldest < 0 {
			return
		}
		rec.Applications = append(rec.Applications[:oldest:oldest], rec.Applications[oldest+1:]...)
	}
}

// evictOldestInvites 超过上限时按 invited_at_ms(同刻按 invitee_id)淘汰最早的;
// keep 是本次刚追加的被邀请人,永不淘汰(否则 IA 写了反查索引、记录里却没有这条邀请)。
func evictOldestInvites(rec *teampb.TeamRecord, keep uint64) {
	for len(rec.Invites) > MaxInvitesPerTeam {
		oldest := -1
		for i, inv := range rec.Invites {
			if inv.GetInviteeId() == keep {
				continue
			}
			if oldest < 0 {
				oldest = i
				continue
			}
			o := rec.Invites[oldest]
			if inv.GetInvitedAtMs() < o.GetInvitedAtMs() ||
				(inv.GetInvitedAtMs() == o.GetInvitedAtMs() && inv.GetInviteeId() < o.GetInviteeId()) {
				oldest = i
			}
		}
		if oldest < 0 {
			return
		}
		rec.Invites = append(rec.Invites[:oldest:oldest], rec.Invites[oldest+1:]...)
	}
}
