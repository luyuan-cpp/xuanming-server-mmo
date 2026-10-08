package data

// 同道历练的邀请房间(设计 docs/design/guild-phase2/06-activities.md §6.23–§6.27)。
//
// 历练是邀请确认制(06 §6.49 #2):发起人建一个待确认房间,被邀请人**亲自**同意之后才开战,任何成员都不能把别人强拉进战斗。
// 房间是纯易失状态 —— 30 秒左右的邀请、5 秒的开战窗口 —— 放 guild 全局 Redis,不进 MySQL:
// 丢了(Redis 重启 / 被清空)只是邀请作废、重新发起即可,不涉及任何资产(06 §6.27 L7 / L8)。
//
// 状态机(数值与 proto GuildTrialLobbyState 一致,视图直接转):
//
//	PENDING(1) ──全员同意──▶ LAUNCHING(2) ──开战成功──▶ LAUNCHED(3)
//	    │ 有人拒绝 / 发起人取消           │ 开战失败
//	    ▼                               ▼
//	 ENDED(4) ◀─────────────────────────┘
//
// 超时不落库,读的时候折算(TrialLobby.EffectiveAt):PENDING 过了 exp、LAUNCHING 过了 ldl 都算 ENDED。
// 这样"全员同意的那一次调用拿到开战权后进程崩了"也能在 5 秒后自动解套(L3),不需要任何清理任务。
//
// 键(全部在 guild 全局 Redis;TTL 到期自然消失):
//
//	guild:trial:lobby:seq                STRING  无 TTL   INCR 发 lobby_id
//	guild:trial:lobby:{lobby_id}         HASH    邀请有效期 + 60s   房间本体(字段见 trialLobbyField*)
//	guild:trial:lobby:of:{player_id}     STRING  同上     该玩家最近所在房间的 id(每人同时只在一个有效房间里)
//	guild:trial:cooldown:{initiator}     STRING  建房冷却  发起人两次建房的最小间隔
//	guild:trial:sweep:lease              STRING  调用方给  历练巡检器的多副本互斥(06 §6.33)
//
// **只在单节点 Redis 上成立**:建房脚本要在脚本里按 of 键的值拼出另一个房间的键再读它,这个键没有出现在 KEYS 里。
// guild 全局 Redis 目前是单节点 *redis.Client(svc.ServiceContext.RedisClient);迁 Redis Cluster 时必须先改成两段式
// (先读 of 键、再把涉及的房间键作为 KEYS 传入并在脚本里复核),否则跨槽访问会直接报错。
//
// 所有判定都在 Lua 里原子完成,"现在"由调用方传入(与本次请求的其它判定同一个 now),脚本不读 Redis 的时钟。
// 玩家 id / 房间 id 在脚本里一律按**字符串**比较与存取,不经 Lua 的 number(double 存不下 64 位雪花 id)。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	pb "proto/guild"

	"guild/internal/constants"
)

const (
	trialLobbySeqKey       = "guild:trial:lobby:seq"
	trialLobbyKeyPrefix    = "guild:trial:lobby:"
	trialLobbyOfKeyPrefix  = "guild:trial:lobby:of:"
	trialCooldownKeyPrefix = "guild:trial:cooldown:"
	trialSweepLeaseKey     = "guild:trial:sweep:lease"

	// trialLobbyKeyGrace:房间键比邀请有效期多留的时间。开战后的 LAUNCHED 状态、结束原因要让客户端还读得到;
	// 也给"过期后折算成 ENDED"留出展示窗口(06 §6.23 的 ttl + 60s)。
	trialLobbyKeyGrace = 60 * time.Second

	// trialLobbyBudget:单次房间操作的子预算。都是单键 / 小脚本,跑满 1s 说明 Redis 出了状况,
	// 尽早把错误还给调用方比吃满请求预算好(与 economyReadBudget 同一档)。
	trialLobbyBudget = 1000 * time.Millisecond

	// minTrialLobbyRoster / maxTrialLobbyRoster:名单人数的结构性上下限。业务上限由配表定(历练队伍 2–5 人,
	// activity.ValidateTables);这里只守两条结构约束:一个人的房间永远等不来"别人同意";同意位图存在 Lua 的 double 里,
	// 16 位远在 2^53 的精确范围之内。
	minTrialLobbyRoster = 2
	maxTrialLobbyRoster = 16

	// trialLobbyParamSep:结束原因参数的分隔符(ASCII Unit Separator)。参数是 tip 的占位值(原因码、玩家 id),不会含它;
	// Finish 仍会拒绝含它的参数,免得存进去再也拆不对。
	trialLobbyParamSep = "\x1f"
)

// 房间 HASH 的字段名。
const (
	trialLobbyFieldGuild     = "gid"
	trialLobbyFieldActivity  = "aid"
	trialLobbyFieldInitiator = "init"
	trialLobbyFieldRoster    = "roster"  // 逗号分隔的十进制 player_id,发起人在首位
	trialLobbyFieldAccepted  = "acc"     // 同意位图:bit i ↔ roster[i]
	trialLobbyFieldState     = "state"   // 1..4,数值同 GuildTrialLobbyState
	trialLobbyFieldExpire    = "exp"     // PENDING 截止时刻(毫秒)
	trialLobbyFieldLaunchBy  = "ldl"     // LAUNCHING 截止时刻(毫秒);未到 LAUNCHING 为 0
	trialLobbyFieldBattle    = "bid"     // LAUNCHED 时的 battle_id
	trialLobbyFieldEndTip    = "etip"    // ENDED 原因 tip
	trialLobbyFieldEndParams = "epar"    // ENDED 原因参数,trialLobbyParamSep 分隔
	trialLobbyFieldCreated   = "created" // 建房时刻(毫秒)
)

// Lua 脚本的返回码(第一个返回值)。只在本文件内部使用,对外翻成各自的结果枚举。
const (
	luaCreateOK       = 1
	luaCreateBusy     = -1 // 第二个返回值 = 名单里那个人的下标
	luaCreateCooldown = -5 // 第二个返回值 = 冷却剩余毫秒
	luaCreateCollide  = -6 // 房间键已存在(seq 被清空后回卷)

	luaRespondMissing    = 0
	luaRespondAccepted   = 1
	luaRespondLaunch     = 2
	luaRespondDeclined   = 3
	luaRespondNotInList  = -2
	luaRespondNotPending = -3 // 第二个返回值 = 本人此前是否已同意(0 / 1)
	luaRespondExpired    = -4
)

// trialLobbyCreateScript 建房。
// KEYS[1] = 房间键,KEYS[2] = 发起人冷却键,KEYS[3..] = 名单各人的 of 键(与名单同序)。
// ARGV[1] = now(毫秒),[2] = lobby_id,[3] = 房间键与 of 键的 TTL(毫秒),[4] = 冷却(毫秒,0 = 不设),[5..] = HSET 的字段值对。
//
// 顺序:冷却 → 房间键撞号 → 名单里是否有人还在别的有效房间(PENDING 未过期,或 LAUNCHING 未超开战截止)→ 写入。
// 任何一步拒绝都不写任何键,所以名单不合法不消耗冷却。
var trialLobbyCreateScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local cd = redis.call('PTTL', KEYS[2])
if cd > 0 then return {` + strconv.Itoa(luaCreateCooldown) + `, cd} end
if redis.call('EXISTS', KEYS[1]) == 1 then return {` + strconv.Itoa(luaCreateCollide) + `, 0} end
for i = 3, #KEYS do
  local other = redis.call('GET', KEYS[i])
  if other then
    local h = redis.call('HMGET', '` + trialLobbyKeyPrefix + `' .. other, 'state', 'exp', 'ldl')
    local st = tonumber(h[1] or '0') or 0
    local exp = tonumber(h[2] or '0') or 0
    local ldl = tonumber(h[3] or '0') or 0
    if (st == 1 and now < exp) or (st == 2 and now < ldl) then
      return {` + strconv.Itoa(luaCreateBusy) + `, i - 3}
    end
  end
end
redis.call('HSET', KEYS[1], unpack(ARGV, 5))
redis.call('PEXPIRE', KEYS[1], ARGV[3])
for i = 3, #KEYS do redis.call('SET', KEYS[i], ARGV[2], 'PX', ARGV[3]) end
if tonumber(ARGV[4]) > 0 then redis.call('SET', KEYS[2], '1', 'PX', ARGV[4]) end
return {` + strconv.Itoa(luaCreateOK) + `, 0}
`)

// trialLobbyRespondScript 同意 / 拒绝(发起人拒绝即取消)。
// KEYS[1] = 房间键。ARGV[1] = 调用者 player_id,[2] = '1' 同意 / '0' 拒绝,[3] = now(毫秒),
// [4] = 若本次凑齐全员同意则写入的开战截止时刻(毫秒,调用方算好:now + 开战窗口),[5] = 拒绝时写入的结束原因 tip。
//
// 两个人几乎同时点同意时,脚本是串行执行的:只有把最后一位置上的那一次返回"开战",另一次返回"已同意"(06 §6.27 L2)。
var trialLobbyRespondScript = redis.NewScript(`
local h = redis.call('HMGET', KEYS[1], 'state', 'exp', 'roster', 'acc')
if not h[1] then return {` + strconv.Itoa(luaRespondMissing) + `, 0} end
local now = tonumber(ARGV[3])
local idx = -1
local n = 0
for id in string.gmatch(h[3] or '', '[^,]+') do
  if id == ARGV[1] then idx = n end
  n = n + 1
end
if idx < 0 then return {` + strconv.Itoa(luaRespondNotInList) + `, 0} end
local acc = tonumber(h[4] or '0') or 0
local bit = 2 ^ idx
local mine = math.floor(acc / bit) % 2
if tonumber(h[1]) ~= 1 then return {` + strconv.Itoa(luaRespondNotPending) + `, mine} end
if now >= (tonumber(h[2] or '0') or 0) then return {` + strconv.Itoa(luaRespondExpired) + `, 0} end
if ARGV[2] == '0' then
  redis.call('HSET', KEYS[1], 'state', '4', 'etip', ARGV[5], 'epar', ARGV[1])
  return {` + strconv.Itoa(luaRespondDeclined) + `, 0}
end
if mine == 0 then
  acc = acc + bit
  redis.call('HSET', KEYS[1], 'acc', tostring(acc))
end
if acc == 2 ^ n - 1 then
  redis.call('HSET', KEYS[1], 'state', '2', 'ldl', ARGV[4])
  return {` + strconv.Itoa(luaRespondLaunch) + `, 0}
end
return {` + strconv.Itoa(luaRespondAccepted) + `, 0}
`)

// trialLobbyFinishScript 状态迁移(开战成功 / 开战失败)。只有当前状态等于 ARGV[1] 才迁移,返回 1;否则不动,返回 0。
// KEYS[1] = 房间键。ARGV[1] = from,[2] = to,[3] = battle_id,[4] = 结束原因 tip,[5] = 结束原因参数。
var trialLobbyFinishScript = redis.NewScript(`
local st = redis.call('HGET', KEYS[1], 'state')
if not st or st ~= ARGV[1] then return 0 end
redis.call('HSET', KEYS[1], 'state', ARGV[2], 'bid', ARGV[3], 'etip', ARGV[4], 'epar', ARGV[5])
return 1
`)

// TrialLobby 是一个邀请房间。Load 返回库里的原样;要给玩家看、或判断"房间是否还有效"时用 EffectiveAt 折算。
type TrialLobby struct {
	LobbyID, GuildID, InitiatorID uint64
	ActivityID                    uint32

	// Roster:参战名单,发起人在首位。AcceptedMask 的第 i 位对应 Roster[i](发起人建房即同意)。
	Roster       []uint64
	AcceptedMask uint32

	State pb.GuildTrialLobbyState

	// ExpireAtMs:PENDING 的截止时刻;LaunchDeadlineMs:LAUNCHING 的截止时刻(未到 LAUNCHING 为 0)。
	ExpireAtMs, LaunchDeadlineMs uint64

	// BattleID:LAUNCHED 时非 0。
	BattleID uint64

	// EndTipID / EndParams:ENDED 的原因 tip 及其参数。
	EndTipID  uint32
	EndParams []string

	CreatedMs uint64
}

// Accepted 返回已同意的玩家,保持名单顺序。
func (lb TrialLobby) Accepted() []uint64 {
	out := make([]uint64, 0, len(lb.Roster))
	for i, playerID := range lb.Roster {
		if lb.AcceptedMask&(1<<uint(i)) != 0 {
			out = append(out, playerID)
		}
	}
	return out
}

// Has 报告 playerID 是否在名单里。
func (lb TrialLobby) Has(playerID uint64) bool {
	for _, id := range lb.Roster {
		if id == playerID {
			return true
		}
	}
	return false
}

// EffectiveAt 返回 nowMs 时刻的**有效**状态(06 §6.23"有效状态折算"):
//   - PENDING 且 now ≥ ExpireAtMs → ENDED,原因 kGuildTrialInviteExpired(邀请超时);
//   - LAUNCHING 且 now ≥ LaunchDeadlineMs → ENDED,原因 kGuildTrialServiceBusy(拿到开战权的那次调用没走完);
//   - 其余原样。
//
// 只改返回的副本,不写回 Redis:建房脚本对"别的房间是否仍有效"用的是同一条规则,折算与否不影响任何判定。
func (lb TrialLobby) EffectiveAt(nowMs uint64) TrialLobby {
	switch {
	case lb.State == pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING && nowMs >= lb.ExpireAtMs:
		lb.State, lb.EndTipID, lb.EndParams = pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, constants.ErrTrialInviteExpired, nil
	case lb.State == pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING && nowMs >= lb.LaunchDeadlineMs:
		lb.State, lb.EndTipID, lb.EndParams = pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, constants.ErrTrialServiceBusy, nil
	}
	return lb
}

// TrialLobbyRepo 是邀请房间的存取入口。线程模型:构造后只读,可被多个请求 goroutine 共享。
type TrialLobbyRepo struct {
	rdb *redis.Client
}

// NewTrialLobbyRepo。rdb 是 guild 全局 Redis(单节点,见文件头);nil 时报错 —— 没有它历练就开不了房,
// 装配层据此把历练视为未开放,而不是等到第一个玩家点按钮时空指针。
func NewTrialLobbyRepo(rdb *redis.Client) (*TrialLobbyRepo, error) {
	if rdb == nil {
		return nil, errors.New("guild trial lobby repo: nil redis client")
	}
	return &TrialLobbyRepo{rdb: rdb}, nil
}

func trialLobbyKey(lobbyID uint64) string {
	return trialLobbyKeyPrefix + strconv.FormatUint(lobbyID, 10)
}

func trialLobbyOfKey(playerID uint64) string {
	return trialLobbyOfKeyPrefix + strconv.FormatUint(playerID, 10)
}

func trialCooldownKey(initiatorID uint64) string {
	return trialCooldownKeyPrefix + strconv.FormatUint(initiatorID, 10)
}

// ── 建房 ─────────────────────────────────────────────────────

// TrialLobbyCreate 是建房的输入。名单的业务校验(人数是否合配表、是否本帮成员、是否在线、入帮时长)由 logic 在调用前做完;
// 这里只复核结构约束。
type TrialLobbyCreate struct {
	GuildID    uint64
	ActivityID uint32

	// Roster:发起人在首位,其余保持玩家选择的顺序;minTrialLobbyRoster..maxTrialLobbyRoster 人,不含 0、不重复。
	Roster []uint64

	// TTL:邀请有效期(GuildRule.trial_invite_ttl_seconds),> 0。Cooldown:同一发起人两次建房的最小间隔,0 = 不限。
	TTL, Cooldown time.Duration

	// NowMs:本次请求的"现在"(UTC 毫秒,> 0)。
	NowMs uint64
}

// TrialLobbyCreateStatus 是建房的去向。零值无效。
type TrialLobbyCreateStatus uint8

const (
	// TrialLobbyCreated:房间已建好,Lobby 是它的初始状态(PENDING,仅发起人已同意)。
	TrialLobbyCreated TrialLobbyCreateStatus = iota + 1
	// TrialLobbyCooldown:发起人仍在建房冷却中,CooldownRemaining 是剩余时间。什么都没写。
	TrialLobbyCooldown
	// TrialLobbyMemberBusy:名单里有人还在另一个有效房间里,BusyPlayerID 是其中排在最前的那个人。什么都没写,不消耗冷却。
	TrialLobbyMemberBusy
)

// TrialLobbyCreateResult 是建房结果。
type TrialLobbyCreateResult struct {
	Status            TrialLobbyCreateStatus
	Lobby             *TrialLobby
	CooldownRemaining time.Duration
	BusyPlayerID      uint64
}

func (in TrialLobbyCreate) validate() error {
	switch {
	case in.GuildID == 0 || in.ActivityID == 0 || in.NowMs == 0:
		return fmt.Errorf("guild trial lobby: guild (%d), activity (%d) and now (%d) must be non-zero", in.GuildID, in.ActivityID, in.NowMs)
	case in.TTL <= 0 || in.Cooldown < 0:
		return fmt.Errorf("guild trial lobby: ttl (%v) must be positive and cooldown (%v) non-negative", in.TTL, in.Cooldown)
	case len(in.Roster) < minTrialLobbyRoster || len(in.Roster) > maxTrialLobbyRoster:
		return fmt.Errorf("guild trial lobby: roster of %d players out of [%d,%d]", len(in.Roster), minTrialLobbyRoster, maxTrialLobbyRoster)
	}
	seen := make(map[uint64]bool, len(in.Roster))
	for _, playerID := range in.Roster {
		if playerID == 0 || seen[playerID] {
			return fmt.Errorf("guild trial lobby: roster contains a zero or repeated player id (%d)", playerID)
		}
		seen[playerID] = true
	}
	return nil
}

// Create 建一个邀请房间(06 §6.24 第 8 步)。lobby_id 由本方法 INCR 发号:Redis 被清空后序号回卷、撞上还活着的房间键时
// 换一个号再试一次(L8),仍撞才报错。
//
// 返回的三种 Status 都不是错误;error 只表示输入结构不合法(调用方编程错误)或 Redis 故障 —— 后者由 logic 按故障上抛,
// 不折成业务提示(06 §6.27 L7)。
func (r *TrialLobbyRepo) Create(ctx context.Context, in TrialLobbyCreate) (TrialLobbyCreateResult, error) {
	if err := in.validate(); err != nil {
		return TrialLobbyCreateResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, trialLobbyBudget)
	defer cancel()

	const attempts = 2
	for attempt := 1; ; attempt++ {
		id, err := r.rdb.Incr(ctx, trialLobbySeqKey).Result()
		if err != nil {
			return TrialLobbyCreateResult{}, fmt.Errorf("guild trial lobby: allocate lobby id: %w", err)
		}
		if id <= 0 {
			return TrialLobbyCreateResult{}, fmt.Errorf("guild trial lobby: lobby id sequence returned %d", id)
		}
		lobby := TrialLobby{
			LobbyID:      uint64(id),
			GuildID:      in.GuildID,
			InitiatorID:  in.Roster[0],
			ActivityID:   in.ActivityID,
			Roster:       append([]uint64(nil), in.Roster...),
			AcceptedMask: 1, // 发起人建房即同意
			State:        pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING,
			ExpireAtMs:   in.NowMs + uint64(in.TTL.Milliseconds()),
			CreatedMs:    in.NowMs,
		}
		code, arg, err := r.runCreate(ctx, lobby, in)
		if err != nil {
			return TrialLobbyCreateResult{}, err
		}
		switch code {
		case luaCreateOK:
			return TrialLobbyCreateResult{Status: TrialLobbyCreated, Lobby: &lobby}, nil
		case luaCreateCooldown:
			return TrialLobbyCreateResult{Status: TrialLobbyCooldown, CooldownRemaining: time.Duration(arg) * time.Millisecond}, nil
		case luaCreateBusy:
			if arg < 0 || arg >= int64(len(in.Roster)) {
				return TrialLobbyCreateResult{}, fmt.Errorf("guild trial lobby: create script returned roster index %d out of range", arg)
			}
			return TrialLobbyCreateResult{Status: TrialLobbyMemberBusy, BusyPlayerID: in.Roster[arg]}, nil
		case luaCreateCollide:
			if attempt >= attempts {
				return TrialLobbyCreateResult{}, fmt.Errorf("guild trial lobby: lobby id %d collided with a live room after %d attempts", id, attempt)
			}
		default:
			return TrialLobbyCreateResult{}, fmt.Errorf("guild trial lobby: create script returned unknown code %d", code)
		}
	}
}

func (r *TrialLobbyRepo) runCreate(ctx context.Context, lobby TrialLobby, in TrialLobbyCreate) (code, arg int64, err error) {
	keys := make([]string, 0, len(lobby.Roster)+2)
	keys = append(keys, trialLobbyKey(lobby.LobbyID), trialCooldownKey(lobby.InitiatorID))
	for _, playerID := range lobby.Roster {
		keys = append(keys, trialLobbyOfKey(playerID))
	}
	keyTTL := in.TTL + trialLobbyKeyGrace
	args := []any{
		formatU64(in.NowMs),
		formatU64(lobby.LobbyID),
		strconv.FormatInt(keyTTL.Milliseconds(), 10),
		strconv.FormatInt(in.Cooldown.Milliseconds(), 10),
		trialLobbyFieldGuild, formatU64(lobby.GuildID),
		trialLobbyFieldActivity, formatU64(uint64(lobby.ActivityID)),
		trialLobbyFieldInitiator, formatU64(lobby.InitiatorID),
		trialLobbyFieldRoster, joinU64(lobby.Roster),
		trialLobbyFieldAccepted, formatU64(uint64(lobby.AcceptedMask)),
		trialLobbyFieldState, formatU64(uint64(lobby.State)),
		trialLobbyFieldExpire, formatU64(lobby.ExpireAtMs),
		trialLobbyFieldLaunchBy, "0",
		trialLobbyFieldBattle, "0",
		trialLobbyFieldEndTip, "0",
		trialLobbyFieldEndParams, "",
		trialLobbyFieldCreated, formatU64(lobby.CreatedMs),
	}
	return runPairScript(ctx, r.rdb, trialLobbyCreateScript, "create", keys, args...)
}

// ── 同意 / 拒绝 ──────────────────────────────────────────────

// TrialLobbyRespondStatus 是一次同意 / 拒绝的去向。零值无效。
type TrialLobbyRespondStatus uint8

const (
	// TrialLobbyRespondGone:房间不存在、调用者不在名单里、或邀请已过期。logic 回 kGuildTrialInviteExpired。
	TrialLobbyRespondGone TrialLobbyRespondStatus = iota + 1
	// TrialLobbyRespondNotPending:房间已不在等待确认(开战中 / 已开战 / 已结束)。AlreadyAccepted 为真 = 本人此前已同意,
	// 这是一次重复点击,logic 当作成功回视图;为假则回 kGuildTrialInviteExpired。
	TrialLobbyRespondNotPending
	// TrialLobbyRespondAccepted:已记下同意,还在等其他人。
	TrialLobbyRespondAccepted
	// TrialLobbyRespondLaunch:本次凑齐了全员同意,房间进入 LAUNCHING —— **本次调用负责开战**(之后必须 Finish)。
	TrialLobbyRespondLaunch
	// TrialLobbyRespondDeclined:已拒绝(发起人拒绝即取消),房间进入 ENDED,原因 kGuildTrialInviteDeclined、参数为拒绝者 id。
	TrialLobbyRespondDeclined
)

// TrialLobbyRespondResult 是同意 / 拒绝的结果。
type TrialLobbyRespondResult struct {
	Status          TrialLobbyRespondStatus
	AlreadyAccepted bool
}

// Respond 记录 playerID 对房间 lobbyID 的同意(accept=true)或拒绝(06 §6.25)。launchWindow 是凑齐全员后留给开战的时间
// (设计值 5s),> 0;nowMs 是本次请求的"现在"。
//
// 幂等:重复同意不改变任何东西;拿到 TrialLobbyRespondLaunch 的只会有一次调用。
// error 只表示输入不合法或 Redis 故障。
func (r *TrialLobbyRepo) Respond(ctx context.Context, lobbyID, playerID uint64, accept bool, nowMs uint64, launchWindow time.Duration) (TrialLobbyRespondResult, error) {
	if lobbyID == 0 || playerID == 0 || nowMs == 0 || launchWindow <= 0 {
		return TrialLobbyRespondResult{}, fmt.Errorf("guild trial lobby: respond needs non-zero lobby (%d), player (%d), now (%d) and a positive launch window (%v)",
			lobbyID, playerID, nowMs, launchWindow)
	}
	ctx, cancel := context.WithTimeout(ctx, trialLobbyBudget)
	defer cancel()

	acceptArg := "0"
	if accept {
		acceptArg = "1"
	}
	code, arg, err := runPairScript(ctx, r.rdb, trialLobbyRespondScript, "respond", []string{trialLobbyKey(lobbyID)},
		formatU64(playerID), acceptArg, formatU64(nowMs),
		formatU64(nowMs+uint64(launchWindow.Milliseconds())),
		formatU64(uint64(constants.ErrTrialInviteDeclined)))
	if err != nil {
		return TrialLobbyRespondResult{}, err
	}
	switch code {
	case luaRespondMissing, luaRespondNotInList, luaRespondExpired:
		return TrialLobbyRespondResult{Status: TrialLobbyRespondGone}, nil
	case luaRespondNotPending:
		return TrialLobbyRespondResult{Status: TrialLobbyRespondNotPending, AlreadyAccepted: arg == 1}, nil
	case luaRespondAccepted:
		return TrialLobbyRespondResult{Status: TrialLobbyRespondAccepted}, nil
	case luaRespondLaunch:
		return TrialLobbyRespondResult{Status: TrialLobbyRespondLaunch}, nil
	case luaRespondDeclined:
		return TrialLobbyRespondResult{Status: TrialLobbyRespondDeclined}, nil
	default:
		return TrialLobbyRespondResult{}, fmt.Errorf("guild trial lobby: respond script returned unknown code %d", code)
	}
}

// ── 开战结果 ─────────────────────────────────────────────────

// Finish 把房间从 from 迁到 to,并写入 battle_id 与结束原因(06 §6.26):
//   - 开战成功:Finish(id, LAUNCHING, LAUNCHED, battleID, 0, nil);
//   - 开战失败:Finish(id, LAUNCHING, ENDED, 0, 原因 tip, 参数)。
//
// 返回 false = 房间当前不在 from(或已不存在),什么都没改。只比较库里的状态,不看截止时刻:
// 开战超过了 5 秒窗口、对局却真的开成了,仍然要把房间写成 LAUNCHED。
// endParams 的元素不得含 trialLobbyParamSep。error 只表示输入不合法或 Redis 故障。
func (r *TrialLobbyRepo) Finish(ctx context.Context, lobbyID uint64, from, to pb.GuildTrialLobbyState, battleID uint64, endTipID uint32, endParams []string) (bool, error) {
	if lobbyID == 0 {
		return false, errors.New("guild trial lobby: finish needs a non-zero lobby id")
	}
	for _, param := range endParams {
		if strings.Contains(param, trialLobbyParamSep) {
			return false, fmt.Errorf("guild trial lobby: end parameter %q contains the separator byte", param)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, trialLobbyBudget)
	defer cancel()

	moved, err := trialLobbyFinishScript.Run(ctx, r.rdb, []string{trialLobbyKey(lobbyID)},
		formatU64(uint64(from)), formatU64(uint64(to)), formatU64(battleID),
		formatU64(uint64(endTipID)), strings.Join(endParams, trialLobbyParamSep)).Int64()
	if err != nil {
		return false, fmt.Errorf("guild trial lobby: finish lobby %d: %w", lobbyID, err)
	}
	return moved == 1, nil
}

// ── 读 ───────────────────────────────────────────────────────

// Load 读房间的**原样**(不折算超时)。房间不存在(从未建过,或键已过期)返回 (nil, nil)。
// 字段缺失或不是合法数字 → error:那是数据损坏,不能当成"没有房间"悄悄放过。
func (r *TrialLobbyRepo) Load(ctx context.Context, lobbyID uint64) (*TrialLobby, error) {
	if lobbyID == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, trialLobbyBudget)
	defer cancel()
	return r.load(ctx, lobbyID)
}

func (r *TrialLobbyRepo) load(ctx context.Context, lobbyID uint64) (*TrialLobby, error) {
	fields, err := r.rdb.HGetAll(ctx, trialLobbyKey(lobbyID)).Result()
	if err != nil {
		return nil, fmt.Errorf("guild trial lobby: load lobby %d: %w", lobbyID, err)
	}
	if len(fields) == 0 {
		return nil, nil
	}
	lobby, err := parseTrialLobby(lobbyID, fields)
	if err != nil {
		return nil, fmt.Errorf("guild trial lobby: lobby %d is corrupt: %w", lobbyID, err)
	}
	return lobby, nil
}

// LoadForPlayer 返回 playerID 最近所在房间在 nowMs 时刻的**有效**状态(已按 EffectiveAt 折算);没有则 (nil, nil)。
// 视图用它填 trial_lobby(06 §6.8 第 6 步);调用方还要自己核对 GuildID / ActivityID 属于当前活动。
// of 键指向的房间里已没有这个人(房间号回卷后被别人复用)时同样返回 (nil, nil)。
func (r *TrialLobbyRepo) LoadForPlayer(ctx context.Context, playerID, nowMs uint64) (*TrialLobby, error) {
	if playerID == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, trialLobbyBudget)
	defer cancel()

	raw, err := r.rdb.Get(ctx, trialLobbyOfKey(playerID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("guild trial lobby: read lobby of player %d: %w", playerID, err)
	}
	lobbyID, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || lobbyID == 0 {
		return nil, fmt.Errorf("guild trial lobby: lobby pointer of player %d is not a lobby id: %q", playerID, raw)
	}
	lobby, err := r.load(ctx, lobbyID)
	if err != nil || lobby == nil || !lobby.Has(playerID) {
		return nil, err
	}
	effective := lobby.EffectiveAt(nowMs)
	return &effective, nil
}

// parseTrialLobby 把房间 HASH 解析成结构。建房时全部字段一次写齐,所以任何字段缺失都算损坏。
func parseTrialLobby(lobbyID uint64, fields map[string]string) (*TrialLobby, error) {
	number := func(name string, bits int) (uint64, error) {
		raw, ok := fields[name]
		if !ok {
			return 0, fmt.Errorf("field %s missing", name)
		}
		v, err := strconv.ParseUint(raw, 10, bits)
		if err != nil {
			return 0, fmt.Errorf("field %s = %q is not an unsigned %d-bit integer", name, raw, bits)
		}
		return v, nil
	}
	var (
		lobby = TrialLobby{LobbyID: lobbyID}
		err   error
	)
	read64 := func(name string, dst *uint64) {
		if err == nil {
			*dst, err = number(name, 64)
		}
	}
	read32 := func(name string, dst *uint32) {
		if err == nil {
			var v uint64
			v, err = number(name, 32)
			*dst = uint32(v)
		}
	}
	var state uint32
	read64(trialLobbyFieldGuild, &lobby.GuildID)
	read32(trialLobbyFieldActivity, &lobby.ActivityID)
	read64(trialLobbyFieldInitiator, &lobby.InitiatorID)
	read32(trialLobbyFieldAccepted, &lobby.AcceptedMask)
	read32(trialLobbyFieldState, &state)
	read64(trialLobbyFieldExpire, &lobby.ExpireAtMs)
	read64(trialLobbyFieldLaunchBy, &lobby.LaunchDeadlineMs)
	read64(trialLobbyFieldBattle, &lobby.BattleID)
	read32(trialLobbyFieldEndTip, &lobby.EndTipID)
	read64(trialLobbyFieldCreated, &lobby.CreatedMs)
	if err != nil {
		return nil, err
	}
	lobby.State = pb.GuildTrialLobbyState(state)

	rosterRaw, ok := fields[trialLobbyFieldRoster]
	if !ok || rosterRaw == "" {
		return nil, fmt.Errorf("field %s missing", trialLobbyFieldRoster)
	}
	for _, part := range strings.Split(rosterRaw, ",") {
		playerID, err := strconv.ParseUint(part, 10, 64)
		if err != nil || playerID == 0 {
			return nil, fmt.Errorf("field %s = %q has a bad player id %q", trialLobbyFieldRoster, rosterRaw, part)
		}
		lobby.Roster = append(lobby.Roster, playerID)
	}
	if params := fields[trialLobbyFieldEndParams]; params != "" {
		lobby.EndParams = strings.Split(params, trialLobbyParamSep)
	}
	return &lobby, nil
}

// ── 巡检器租约 ───────────────────────────────────────────────

// AcquireSweepLease 抢历练巡检器本轮的租约(06 §6.33:SET NX PX)。owner 是本实例的标识(非空),ttl 略短于巡检间隔(> 0)。
// 返回 true = 本实例本轮负责巡检;false = 别的实例持有,本轮跳过。不提供释放:让它自然到期,
// 一轮巡检提前跑完也不该让另一个实例紧接着再扫一遍。
// 租约只是省重复劳动,不承担正确性:两个实例同时巡检时,结算与判过期各自在事务里按对局行的状态复核。
func (r *TrialLobbyRepo) AcquireSweepLease(ctx context.Context, owner string, ttl time.Duration) (bool, error) {
	if owner == "" || ttl <= 0 {
		return false, fmt.Errorf("guild trial sweep lease: owner must be non-empty and ttl (%v) positive", ttl)
	}
	ctx, cancel := context.WithTimeout(ctx, trialLobbyBudget)
	defer cancel()
	acquired, err := r.rdb.SetNX(ctx, trialSweepLeaseKey, owner, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("guild trial sweep lease: %w", err)
	}
	return acquired, nil
}

// ── 小工具 ───────────────────────────────────────────────────

// runPairScript 跑一条返回 {code, arg} 两个整数的脚本。
func runPairScript(ctx context.Context, rdb *redis.Client, script *redis.Script, what string, keys []string, args ...any) (code, arg int64, err error) {
	reply, err := script.Run(ctx, rdb, keys, args...).Int64Slice()
	if err != nil {
		return 0, 0, fmt.Errorf("guild trial lobby: %s script: %w", what, err)
	}
	if len(reply) != 2 {
		return 0, 0, fmt.Errorf("guild trial lobby: %s script returned %d values, want 2", what, len(reply))
	}
	return reply[0], reply[1], nil
}

func formatU64(v uint64) string { return strconv.FormatUint(v, 10) }

func joinU64(ids []uint64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = formatU64(id)
	}
	return strings.Join(parts, ",")
}
