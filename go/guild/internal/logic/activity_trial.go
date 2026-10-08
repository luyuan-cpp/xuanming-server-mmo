package logic

// 同道历练的 logic 层(B6b):邀请房间 → 确认开战 → 结果结算 → 毒消息。
// 设计 docs/design/guild-phase2/06-activities.md §6.24–§6.31(顶部决策覆盖 U2:**阵亡也得奖**,只排除逃跑者)。
// 效力顺序:README §2 / §3 > 06 顶部覆盖块 > 92-handoff §12.2 / §12.4 > 90-consistency > 06 正文;
// 取锁纪律全部落在 data/activity_repo.go,本文件不开事务、不直接碰库锁。
//
// 一局历练在 logic 侧的四段:
//
//	StartGuildTrial          发起人建邀请房间(只写 guild 全局 Redis),推送给被邀请人
//	RespondGuildTrialInvite  被邀请人同意 / 拒绝(发起人拒绝 = 取消);凑齐最后一票的那一次调用负责开战(launchTrial)
//	launchTrial              复核名单 → 调 match 内部服务拿 battle_id → 登记对局行 → 房间进 LAUNCHED
//	SettleTrialResult        对局结果到达(Kafka 消费者,或巡检器从结果记录取回):发帮贡 / 资金 / 物品,销账
//
// 沿用 activity_logic.go 文件头的四条纪律,历练另有四条:
//
//  1. **match 的任何失败都回 tip kGuildTrialServiceBusy,不回 gRPC 错误**(06 契约偏差 16):gRPC 错误会让客户端把
//     整个帮会界面当传输故障进重连隔离。gRPC 错误只留给 guild 自己的 MySQL / Redis 故障。
//  2. **开战成功之后不再失败**:match 一旦回了 battle_id,战斗已是事实。登记对局行、改房间状态失败都只打日志、
//     计指标,本次 RPC 照常成功 —— 结算会补登记(06 §6.27 L4)。这两步用不继承请求取消的独立预算。
//  3. **结算幂等,且只有"有了定论"才销账**:同一局的结果会被重复投递(battle 未销账就重发、Kafka 重放、
//     巡检器与消费者同时处理)。幂等闸门在仓储的对局行上;SharedRedis 的结果记录只在事务提交之后删。
//     闸门有两道:入口处一次不持锁的终态预读(对局行已是 SETTLED 就直接销账,不构建奖励、不发号、不开事务),
//     与事务里持着帮会行锁的权威判定。前一道让重复副本便宜地走掉,也是"卡住的消息"的人工出口(06 §6.35a 做法 A)。
//     暂时性错误(库 / Redis 故障、写冲突、合服闸门、号段不可用)原样返回让调用方退避重试,**不销账**;
//     确定性错误(溢出、奖励包坏、上下文畸形)标成毒消息后销账,不卡住分区(06 §6.31)。
//  4. **名单先判帮会归属、再判在线**:在线状态只对"确认是本帮成员"的人作答,否则任何人都能把陌生玩家的 id
//     塞进名单来探测他在不在线。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/activity"
	"guild/internal/constants"
	"guild/internal/data"
	battlepb "proto/battle"
	assetpb "proto/common/asset"
	base "proto/common/base"
	rollbackpb "proto/common/rollback"
	kafkapb "proto/contracts/kafka"
	pb "proto/guild"
	matchpb "proto/match"
	"shared/assetop"
	tablepb "shared/generated/pb/table"
	"shared/safego"
)

// ── 接缝 ──────────────────────────────────────────────────────

// TrialBattleStarter 是确认开战对 match 的全部要求;生成的 matchpb.MatchInternalClient 满足它。
// 用窄接口而不是直接依赖生成的客户端类型:logic 只用这一个方法,单测的替身也只需实现这一个。
//
// 契约(proto/match/match_internal.proto):业务拒绝走响应里的 reject,不回 gRPC 错误;回 NONE 时 battle_id 非 0,
// match 已为全员建好票并异步启动 gather。调用方**不得**把客户端的会话 metadata 带过去(match 对带会话的调用回
// PermissionDenied)—— gRPC 不会自动把入站 metadata 转成出站,这里也没有手动转。
type TrialBattleStarter interface {
	StartActivityBattle(ctx context.Context, in *matchpb.StartActivityBattleRequest, opts ...grpc.CallOption) (*matchpb.StartActivityBattleResponse, error)
}

// trialStore 是历练对仓储的全部要求,*data.ActivityRepo 满足它。每个方法的语义、错误分类与取锁序列
// 以 data/activity_repo.go 的函数头为准,这里不重述。
type trialStore interface {
	TrialRosterMembers(ctx context.Context, guildID uint64, playerIDs []uint64) (map[uint64]uint64, error)
	TrialBattle(ctx context.Context, battleID uint64) (data.TrialBattleRow, bool, error)
	RegisterTrialBattle(ctx context.Context, battle data.TrialBattleKey, nowMs uint64) error
	SettleTrialBattleTx(ctx context.Context, in data.TrialSettleInput) (data.TrialSettleResult, error)
	MarkTrialBattlePoison(ctx context.Context, battle data.TrialBattleKey, finishedAtMs, nowMs uint64) (data.TrialMarkOutcome, error)
	ExpireTrialBattle(ctx context.Context, battleID, guildID, nowMs uint64) (bool, error)
	ListOverdueTrialBattles(ctx context.Context, createdBeforeMs uint64, limit int) ([]data.OverdueTrialBattle, error)
	ConvertOwedReward(ctx context.Context, owed data.OwedReward, opID, nowMs uint64) (data.OwedConvertStatus, error)
	ListOwedRewards(ctx context.Context, after data.OwedRewardCursor, limit int) ([]data.OwedReward, error)
	CountOwedRewards(ctx context.Context, limit int) (int, error)
	OwedRewardCounts(ctx context.Context, playerID uint64) (map[uint32]uint32, error)
}

var _ trialStore = (*data.ActivityRepo)(nil)

// ── 常量 ──────────────────────────────────────────────────────

const (
	// defaultMatchBudget:调 match 的单次上限的默认值(= etc/guild.yaml 的 MatchRpc.Timeout)。
	defaultMatchBudget = 1500 * time.Millisecond
	// trialLaunchTailReserve:调完 match 之后必须留出的尾巴 —— 登记对局行、把房间写成 LAUNCHED、重建视图与编码回包
	// (06 §6.6 预算表的 500ms)。预算从 ctx 截止时间倒推(裁决 K),不从"进 handler 起算"。
	trialLaunchTailReserve = 500 * time.Millisecond
	// minMatchBudget:低于它就不调 match。match 要逐人做在线 / 战斗锁 / 位置预检并建票,300ms 连一次正常往返都不稳;
	// 硬调只会以超时收场,而超时的那次 match 可能其实已经开战 —— 不如明确地不调。
	// config.MinMatchRpcTimeoutMs 镜像它作 MatchRpc.Timeout 的启动校验下限(上限比它小 = 每次开战都不调 match,
	// 必须拒启而不是带病运行);改这个值要同改那个常量,TestTrialMinMatchBudgetMirrorsConfig 守住两者相等。
	minMatchBudget = 300 * time.Millisecond
	// trialLaunchWindow:全员同意后留给开战的时间(房间停在 LAUNCHING 的最长时间)。它必须大于整请求业务预算
	// (RequestBudget = 3500ms):拿到开战权的那次请求还没走完,房间不能先被折算成"开战失败"。
	trialLaunchWindow = 5 * time.Second
	// trialAfterLaunchBudget:match 返回之后登记对局、改房间状态的独立预算。不继承请求的取消与截止(纪律 2):
	// 开战已是事实,客户端此刻断开或请求预算耗尽,都不该让这一局少一行登记。
	trialAfterLaunchBudget = 2 * time.Second
	// trialLockReadTimeout:视图读战斗锁的独立上限。理由同 DefaultStrictOnlineLookupTimeout:会话 Redis 的客户端没开
	// ContextTimeoutEnabled,ctx 截止管不住套接字读;这一读只是展示,宁可少显示一次"进行中"也不拖住整页。
	trialLockReadTimeout = 500 * time.Millisecond
	// battleLockKeyPrefix:战斗锁键(跨运行时契约,C++ scene 写,值 = 十进制 battle_id;match 也只读它)。
	battleLockKeyPrefix = "battle:lock:"
)

// 名单不合法的原因:kGuildTrialTeamInvalid 的第一个参数,客户端按它选文案(06 §6.24 末段的全集)。
// 第二个参数是当事人的 player_id(十进制),没有具体当事人时为 "0"。
const (
	trialReasonDuplicate        = "duplicate"         // 名单里有 0 或重复的 id
	trialReasonInitiatorMissing = "initiator_missing" // 名单不含发起人
	trialReasonSize             = "size"              // 人数不在 [team_size_min, team_size_max]
	trialReasonNotMember        = "not_member"        // 不是本帮成员
	trialReasonJoinRecent       = "join_recent"       // 入帮未满 activity_join_min_hours
	trialReasonOffline          = "offline"           // 不在线
	trialReasonBusy             = "busy"              // 还在另一个有效的邀请房间里
	trialReasonInBattle         = "in_battle"         // match 预检:已在战斗中
	trialReasonNotReady         = "not_ready"         // match 预检:无场景位置 / 有在途票据
	trialReasonInvalid          = "invalid"           // match 判请求本身非法(guild 的缺陷,同时打 ERROR)
)

// ── 小工具 ────────────────────────────────────────────────────

func trialNotOpenTip() *base.TipInfoMessage {
	return tipErr(constants.ErrActivityNotOpen, "guild trial not open")
}

func trialInviteExpiredTip() *base.TipInfoMessage {
	return tipErr(constants.ErrTrialInviteExpired, "guild trial invite expired")
}

// trialTeamInvalidParams 是 kGuildTrialTeamInvalid 的参数 [reason, player_id]。只放参数本身(客户端按位次填文案)。
func trialTeamInvalidParams(reason string, offender uint64) []string {
	return []string{reason, strconv.FormatUint(offender, 10)}
}

func trialTeamInvalidTip(reason string, offender uint64) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: constants.ErrTrialTeamInvalid, Parameters: trialTeamInvalidParams(reason, offender)}
}

// trialCooldownTip:建房冷却中,参数 = 剩余秒数(向上取整,至少 1:显示"0 秒后可用"而仍被拒绝只会让人困惑)。
func trialCooldownTip(remaining time.Duration) *base.TipInfoMessage {
	seconds := max(int64((remaining+time.Second-1)/time.Second), 1)
	return &base.TipInfoMessage{Id: constants.ErrTrialInviteCooldown, Parameters: []string{strconv.FormatInt(seconds, 10)}}
}

// matchBudgetFor 算调 match 的预算。ok=false = 不够,不调(房间以 kGuildTrialServiceBusy 结束)。
// ctx 没有截止(单测、内部调用)时按满额算。
func matchBudgetFor(ctx context.Context, matchBudget time.Duration) (time.Duration, bool) {
	remaining := matchBudget + trialLaunchTailReserve
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	return clampMatchBudget(matchBudget, remaining)
}

// clampMatchBudget:budget = min(matchBudget, remaining − trialLaunchTailReserve),低于 minMatchBudget 判为不够。
// 拆成纯函数只为可测(不依赖墙钟)。
func clampMatchBudget(matchBudget, remaining time.Duration) (time.Duration, bool) {
	budget := min(matchBudget, remaining-trialLaunchTailReserve)
	return budget, budget >= minMatchBudget
}

// normalizeTrialRoster 校验并规范化客户端给的参战名单(06 §6.24 第 3 步)。合法时 reason 为空,
// roster 是"发起人在首位、其余保持客户端给的相对顺序"的新切片(站位顺序即队伍顺序)。
//
// 判定次序:人数超上限 → 含 0 / 重复 → 不含发起人 → 人数不足。超上限放最前面是为了在分配任何东西之前
// 就拒掉超长名单(名单来自客户端);其余次序是"先指出具体哪个 id 有问题"。
func normalizeTrialRoster(initiator uint64, ids []uint64, sizeMin, sizeMax uint32) (roster []uint64, reason string, offender uint64) {
	if len(ids) > int(sizeMax) {
		return nil, trialReasonSize, 0
	}
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, trialReasonDuplicate, 0
		}
		if _, dup := seen[id]; dup {
			return nil, trialReasonDuplicate, id
		}
		seen[id] = struct{}{}
	}
	if _, ok := seen[initiator]; !ok {
		return nil, trialReasonInitiatorMissing, 0
	}
	if len(ids) < int(sizeMin) {
		return nil, trialReasonSize, 0
	}
	roster = make([]uint64, 0, len(ids))
	roster = append(roster, initiator)
	for _, id := range ids {
		if id != initiator {
			roster = append(roster, id)
		}
	}
	return roster, "", 0
}

// verifyTrialRoster 权威核对名单(建房与确认开战共用):全员是本帮成员且入帮满 N 小时(MySQL 普通读),全员在线
// (会话 Redis,严格版)。reason 为空 = 通过;否则 offender 是名单顺序里第一个不满足的人。
// err 非 nil = guild 自己的库 / Redis 故障,调用方按故障处理(读不到不当成"不在帮"或"离线",fail-closed)。
//
// 先帮会归属、后在线(文件头纪律 4)。不用缓存快照判成员:快照可能落后一个代次,
// 刚入帮的人会被误拒、刚被踢的人会被放行;这里的一次主键范围读就是权威答案。
// 它只用来拒绝开战,不构成发奖依据 —— 发奖时的成员资格由结算事务在锁内复核。
func (l *GuildLogic) verifyTrialRoster(ctx context.Context, d *ActivityDeps, guildID uint64, roster []uint64,
	nowMs uint64, joinMinHours uint32) (reason string, offender uint64, err error) {
	members, err := d.trials.TrialRosterMembers(ctx, guildID, roster)
	if err != nil {
		return "", 0, fmt.Errorf("verify trial roster of guild %d: %w", guildID, err)
	}
	for _, playerID := range roster {
		joinTimeMs, isMember := members[playerID]
		if !isMember {
			return trialReasonNotMember, playerID, nil
		}
		if !activity.JoinedLongEnough(joinTimeMs, nowMs, joinMinHours) {
			return trialReasonJoinRecent, playerID, nil
		}
	}
	online, err := l.onlineResolver.BatchResolveStrict(ctx, roster)
	if err != nil {
		return "", 0, fmt.Errorf("resolve online state of trial roster (guild %d): %w", guildID, err)
	}
	for _, playerID := range roster {
		if !online[playerID] {
			return trialReasonOffline, playerID, nil
		}
	}
	return "", 0, nil
}

// trialRewardChannelTip:历练配了物品奖励,而资产通道关闭 / 发号器未接线时,在建房之前就拒绝(activity_logic.go 纪律 4)。
// 放这一局开打,打赢之后物品指令要么发不出号、要么插进去没人投递。nil = 可以继续。
// 只在建房时判:房间从建好到开战至多几十秒,这期间通道被关掉属于运维窗口,结算对它另有处理(见 SettleTrialResult)。
func trialRewardChannelTip(d *ActivityDeps, bundle *assetpb.AssetBundle) *base.TipInfoMessage {
	switch {
	case bundle == nil:
		return nil
	case d.Loop == nil:
		return assetChannelDisabledTip()
	case d.OpIDs == nil:
		logx.Error("[GuildTrial] asset op id minter not wired, refusing to open a trial lobby with item rewards")
		return tipErr(constants.ErrIDGenUnavailable, "asset op id generator unavailable")
	default:
		return nil
	}
}

// trialMergeFenceTip:帮会所在 zone 正在合服(或闸门读不到,fail-closed)时不许发起、不许凑齐开战,回 kGuildZoneMerging。
//
// 灯会 / 团圆的合服闸门在写事务里按锁住的 guild.zone_id 判;建房与开战不开 MySQL 事务,只能在这里按缓存快照的 zone 判。
// 快照的 zone 在合服刚结束时可能落后一个缓存代次,所以它只是"尽量别在合服窗口里开新的对局"的前置闸:
// 权威的闸门仍在结算事务里(SettleTrialResult 传的 Fence)。少了这道前置闸的代价是:合服窗口里打完的对局,
// 它的结果会让结果消费停在那条消息上重试到合服结束 —— 消费是单协程串行的,停住的是**这个 guild 副本分到的全部分区**
// (只有一个副本时就是全服),别的 zone、别的帮会的结算跟着一起等,只能靠巡检器在登记 420s 之后从结果记录兜底
// (kafka/trial_result_consumer.go 文件头纪律 2)。
func (l *GuildLogic) trialMergeFenceTip(ctx context.Context, a activityActor) *base.TipInfoMessage {
	return l.mergeFenceTip(ctx, a.guild.ZoneID)
}

// notifyTrialRoster 给名单里除 actor 以外的人推一条 ACTIVITY_CHANGED(房间状态变了,去拉活动页)。
func (l *GuildLogic) notifyTrialRoster(lobby *data.TrialLobby, actor uint64) {
	l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED, lobby.GuildID, actor, 0, exceptPlayer(lobby.Roster, actor))
}

// ── 视图 ──────────────────────────────────────────────────────

// trialViewState 是历练行视图独有的三样。零值 = 历练未开放。
type trialViewState struct {
	// available:依赖齐全(ActivityDeps.trialAvailable)。为假时视图强制 DISABLED + 未开放。
	available bool
	// battleID:本人战斗锁指向、且仍为 STARTED 的历练对局;0 = 无。
	battleID uint64
	// lobby:本人所在、属于本帮本活动的邀请房间(已折算成有效状态);nil = 无。
	lobby *pb.GuildTrialLobbyView
}

// trialLobbyView 把房间转成视图。调用方给的必须是已按 EffectiveAt 折算过的房间:客户端不自己判超时。
func trialLobbyView(lobby *data.TrialLobby) *pb.GuildTrialLobbyView {
	if lobby == nil {
		return nil
	}
	return &pb.GuildTrialLobbyView{
		LobbyId:           lobby.LobbyID,
		InitiatorPlayerId: lobby.InitiatorID,
		MemberPlayerIds:   slices.Clone(lobby.Roster),
		AcceptedPlayerIds: lobby.Accepted(),
		State:             lobby.State,
		ExpireAtMs:        lobby.ExpireAtMs,
		BattleId:          lobby.BattleID,
		EndTipId:          lobby.EndTipID,
		EndParameters:     slices.Clone(lobby.EndParams),
	}
}

func trialRowOf(rows []*tablepb.GuildActivityTable) *tablepb.GuildActivityTable {
	for _, row := range rows {
		if row.GetType() == activity.TypeTrial {
			return row
		}
	}
	return nil
}

// trialViewExtras 读历练行视图独有的数据(06 §6.8 第 6 步与第 7c 步)。rows 里没有历练行时什么都不读。
//
// 失败口径同 activityViews:MySQL 读失败返回错误;Redis 读失败(战斗锁、邀请房间)降级为"没有"并记日志 ——
// 少显示一次"进行中"或一张邀请,玩家再拉一次就对齐,不值得让整页失败。
// 待入队行数不看历练此刻开没开:物品是已经欠下的,关了历练也要让玩家看得见它还在路上。
func (l *GuildLogic) trialViewExtras(ctx context.Context, d *ActivityDeps, a activityActor,
	rows []*tablepb.GuildActivityTable, nowMs uint64) (map[uint32]uint32, trialViewState, error) {
	row := trialRowOf(rows)
	if row == nil || d.trials == nil {
		return nil, trialViewState{}, nil
	}
	owed, err := d.trials.OwedRewardCounts(ctx, a.playerID)
	if err != nil {
		return nil, trialViewState{}, err
	}
	if !d.trialAvailable() {
		return owed, trialViewState{}, nil
	}
	battleID, err := l.myTrialBattleID(ctx, d, a.playerID)
	if err != nil {
		return nil, trialViewState{}, err
	}
	return owed, trialViewState{
		available: true,
		battleID:  battleID,
		lobby:     l.myTrialLobbyView(ctx, d, a, row.GetId(), nowMs),
	}, nil
}

// myTrialBattleID 返回本人正在进行的历练对局(06 §6.8 第 6 步):战斗锁的值是一个 battle_id,且那一局在对局表里
// 仍是 STARTED。锁不在、锁指向的不是历练对局、对局已结算 / 已判过期 → 0。
//
// 为什么以战斗锁为准而不是"最近登记的 STARTED 行":gather 失败的对局(有人换场景、掉线)永远等不来结果,
// 登记行要到巡检器判过期才离开 STARTED;按行判会把发起人锁在"进行中"好几分钟(06 附录评审 #8)。
func (l *GuildLogic) myTrialBattleID(ctx context.Context, d *ActivityDeps, playerID uint64) (uint64, error) {
	battleID, err := lockedBattleID(ctx, d.BattleLocks, playerID)
	if err != nil {
		logx.Errorf("[GuildTrial] read battle lock of player %d failed, view shows no ongoing trial: %v", playerID, err)
		return 0, nil
	}
	if battleID == 0 {
		return 0, nil
	}
	row, found, err := d.trials.TrialBattle(ctx, battleID)
	if err != nil {
		return 0, err
	}
	if !found || row.State != pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED {
		return 0, nil
	}
	return battleID, nil
}

// lockedBattleID 读 battle:lock:{player_id}。键不存在、值不是非 0 的十进制整数 → 0(不是历练关心的锁)。
// rdb 为 nil(未接线)→ 0。
//
// 有独立上限,写法同 OnlineStatusResolver.mgetWithin:GET 放进单独的协程,本协程在"结果到达"与"上限到期"之间择先。
// 被丢下的协程不会泄漏:通道带 1 格缓冲,它最迟在一次套接字读超时后返回。
func lockedBattleID(ctx context.Context, rdb *redis.Client, playerID uint64) (uint64, error) {
	if rdb == nil || playerID == 0 {
		return 0, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, trialLockReadTimeout)
	defer cancel()

	type lockRead struct {
		raw string
		err error
	}
	done := make(chan lockRead, 1)
	key := battleLockKeyPrefix + strconv.FormatUint(playerID, 10)
	safego.Go("guild.trial_battle_lock_get", func() {
		raw, err := rdb.Get(lookupCtx, key).Result()
		done <- lockRead{raw: raw, err: err}
	})
	select {
	case res := <-done:
		if errors.Is(res.err, redis.Nil) {
			return 0, nil
		}
		if res.err != nil {
			return 0, res.err
		}
		battleID, err := strconv.ParseUint(res.raw, 10, 64)
		if err != nil {
			return 0, nil
		}
		return battleID, nil
	case <-lookupCtx.Done():
		return 0, lookupCtx.Err()
	}
}

// myTrialLobbyView 返回本人最近所在、且属于本帮本活动的邀请房间;没有、已随键过期、或属于别的帮 / 别的活动 → nil。
// 已结束的房间在键过期之前照样返回:客户端要靠它显示"谁拒绝了 / 为什么没开成"。
func (l *GuildLogic) myTrialLobbyView(ctx context.Context, d *ActivityDeps, a activityActor, activityID uint32, nowMs uint64) *pb.GuildTrialLobbyView {
	lobby, err := d.Lobby.LoadForPlayer(ctx, a.playerID, nowMs)
	if err != nil {
		logx.Errorf("[GuildTrial] load trial lobby of player %d failed, view shows no invite: %v", a.playerID, err)
		return nil
	}
	if lobby == nil || lobby.GuildID != a.guild.GuildID || lobby.ActivityID != activityID {
		return nil
	}
	return trialLobbyView(lobby)
}

// trialView 重建历练行的视图,作两个写 RPC 的回包。与活动页走同一条装配路径(activityViewsOf),
// 所以回包里的次数、进度、房间与随后拉到的活动页逐字段同源。
//
// 全部 best-effort:房间已经写进 Redis(或战斗已经开始),回读失败只记日志、回 nil ——
// 把它报成 RPC 失败,玩家会以为没点成功而再点一次,换来一句"邀请冷却中"。客户端拿到空视图时自己再拉活动页。
func (l *GuildLogic) trialView(ctx context.Context, d *ActivityDeps, a activityActor, row *tablepb.GuildActivityTable, now time.Time) *pb.GuildActivityView {
	if row == nil {
		return nil
	}
	rule, ok := activity.Rule()
	if !ok {
		logx.Errorf("[GuildTrial] GuildRule row %d missing, trial view omitted (player %d)", activity.RuleRowID, a.playerID)
		return nil
	}
	views, err := l.activityViewsOf(ctx, d, a, rule, now, []*tablepb.GuildActivityTable{row})
	if err != nil {
		logx.Errorf("[GuildTrial] rebuild trial view failed, response carries no view (player %d, activity %d): %v",
			a.playerID, row.GetId(), err)
		return nil
	}
	if len(views) == 0 {
		return nil
	}
	return views[0]
}

// ── RPC:建邀请房间(06 §6.24)────────────────────────────────

// StartGuildTrial:发起人建邀请房间,**不直接开战**。流程:前置 → 合服闸门 → 预检(开放 / 帮会等级 / 入帮时长 / 今日次数)→
// 名单规范化 → 权威核对名单(本帮成员、入帮时长、在线)→ [物品通道] → 建房(Redis 原子脚本:冷却、每人同时只在
// 一个有效房间)→ 推送给被邀请人(target = 被邀请人,客户端据此弹邀请)→ 回带房间的视图。
//
// 名单校验失败都发生在建房之前,不消耗冷却。发起人今日次数已满时不能发起;其他成员次数用完仍可随队
// (结算时没有奖励,由界面提示)。
func (l *GuildLogic) StartGuildTrial(ctx context.Context, req *pb.StartGuildTrialRequest) (*pb.StartGuildTrialResponse, error) {
	tip, view, err := l.startGuildTrial(ctx, req)
	guildActivityActionTotal.Inc(activityTypeTrial, activityActionInvite, activityResultOf(tip, err))
	if err != nil {
		return nil, err
	}
	return &pb.StartGuildTrialResponse{ErrorMessage: tip, Activity: view}, nil
}

func (l *GuildLogic) startGuildTrial(ctx context.Context, req *pb.StartGuildTrialRequest) (*base.TipInfoMessage, *pb.GuildActivityView, error) {
	d := l.activities
	if d == nil {
		return activitiesNotWiredTip("StartGuildTrial"), nil, nil
	}
	now := activityNow(d)
	nowMs := uint64(now.UnixMilli())
	a, tip, err := l.activityPrelude(ctx, false)
	if err != nil || tip != nil {
		return tip, nil, err
	}
	// 依赖不齐(没配 match、结果消费没开、房间 Redis 没接)与"活动没开"同一答复,且排在任何历练读写之前。
	if !d.trialAvailable() {
		return trialNotOpenTip(), nil, nil
	}
	if tip := l.trialMergeFenceTip(ctx, a); tip != nil {
		return tip, nil, nil
	}
	pre, tip, err := l.precheckActivity(ctx, d, a, activity.TypeTrial, req.GetActivityId(), now)
	if err != nil || tip != nil {
		return tip, nil, err
	}
	row := pre.row

	roster, reason, offender := normalizeTrialRoster(a.playerID, req.GetMemberPlayerIds(), row.GetTeamSizeMin(), row.GetTeamSizeMax())
	if reason != "" {
		return trialTeamInvalidTip(reason, offender), nil, nil
	}
	reason, offender, err = l.verifyTrialRoster(ctx, d, a.guild.GuildID, roster, nowMs, pre.rule.GetActivityJoinMinHours())
	if err != nil {
		return nil, nil, err
	}
	if reason != "" {
		return trialTeamInvalidTip(reason, offender), nil, nil
	}
	if tip := trialRewardChannelTip(d, pre.bundle); tip != nil {
		return tip, nil, nil
	}

	created, err := d.Lobby.Create(ctx, data.TrialLobbyCreate{
		GuildID:    a.guild.GuildID,
		ActivityID: row.GetId(),
		Roster:     roster,
		TTL:        time.Duration(pre.rule.GetTrialInviteTtlSeconds()) * time.Second,
		Cooldown:   time.Duration(pre.rule.GetTrialInviteCooldownSeconds()) * time.Second,
		NowMs:      nowMs,
	})
	if err != nil {
		// 房间 Redis 故障(或配表的邀请有效期被错误热更成 0):guild 自己的故障,不折成业务提示(06 §6.27 L7)。
		return nil, nil, fmt.Errorf("create trial lobby (guild %d, initiator %d): %w", a.guild.GuildID, a.playerID, err)
	}
	switch created.Status {
	case data.TrialLobbyCreated:
	case data.TrialLobbyCooldown:
		return trialCooldownTip(created.CooldownRemaining), nil, nil
	case data.TrialLobbyMemberBusy:
		return trialTeamInvalidTip(trialReasonBusy, created.BusyPlayerID), nil, nil
	default:
		return nil, nil, status.Errorf(codes.Internal, "trial lobby create returned unknown status %d", created.Status)
	}

	// 逐人推、target = 被邀请人(06 §6.24 第 9 步):客户端靠 target 是不是自己来决定弹不弹邀请框。
	// 推送丢了也无妨:被邀请人打开活动页就能看到这张邀请(L1)。
	lobby := created.Lobby
	for _, invitee := range lobby.Roster[1:] {
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED, lobby.GuildID, a.playerID, invitee, []uint64{invitee})
	}
	return nil, l.trialView(ctx, d, a, row, now), nil
}

// ── RPC:同意 / 拒绝 / 取消(06 §6.25)─────────────────────────

// trialRespondOutcome 是一次 RespondGuildTrialInvite 的结局。
type trialRespondOutcome struct {
	tip  *base.TipInfoMessage
	view *pb.GuildActivityView
	// result 非空时作指标 label;为空则由 activityResultOf 从 tip / 错误推出。
	// 只有"拒绝 / 取消"需要它:那是一次成功的响应,没有 tip 可推。
	result string
}

// RespondGuildTrialInvite:被邀请人同意 / 拒绝;发起人 accept=false = 取消房间。
// 全员同意的那一次调用拿到开战权(房间脚本保证只有一次),由它在本次请求里确认开战(launchTrial);
// 开战失败时响应同时带 error_message(原因)与视图(房间已是 ENDED):客户端先应用视图再显示原因。
//
// 重复同意是幂等的:本人此前已同意而房间已不在等待确认(正在开战 / 已开战 / 已结束),当作成功回视图。
func (l *GuildLogic) RespondGuildTrialInvite(ctx context.Context, req *pb.RespondGuildTrialInviteRequest) (*pb.RespondGuildTrialInviteResponse, error) {
	out, err := l.respondGuildTrialInvite(ctx, req)
	result := out.result
	if result == "" {
		result = activityResultOf(out.tip, err)
	}
	guildActivityActionTotal.Inc(activityTypeTrial, activityActionRespond, result)
	if err != nil {
		return nil, err
	}
	return &pb.RespondGuildTrialInviteResponse{ErrorMessage: out.tip, Activity: out.view}, nil
}

func (l *GuildLogic) respondGuildTrialInvite(ctx context.Context, req *pb.RespondGuildTrialInviteRequest) (trialRespondOutcome, error) {
	d := l.activities
	if d == nil {
		return trialRespondOutcome{tip: activitiesNotWiredTip("RespondGuildTrialInvite")}, nil
	}
	now := activityNow(d)
	nowMs := uint64(now.UnixMilli())
	a, tip, err := l.activityPrelude(ctx, false)
	if err != nil || tip != nil {
		return trialRespondOutcome{tip: tip}, err
	}
	if !d.trialAvailable() {
		return trialRespondOutcome{tip: trialNotOpenTip()}, nil
	}

	// 先读一次房间:只接受"本帮的、名单里有我"的房间。房间不存在、属于别的帮、我不在名单里一律回同一句
	// "邀请已失效" —— 分得更细只会泄露别人的房间状态。名单与归属建房后不变,这次读到的可以直接用于开战。
	lobby, err := d.Lobby.Load(ctx, req.GetLobbyId())
	if err != nil {
		return trialRespondOutcome{}, fmt.Errorf("load trial lobby %d: %w", req.GetLobbyId(), err)
	}
	if lobby == nil || lobby.GuildID != a.guild.GuildID || !lobby.Has(a.playerID) {
		return trialRespondOutcome{tip: trialInviteExpiredTip()}, nil
	}
	// 合服期间不接受"同意"(它可能就是凑齐开战的那一票);拒绝 / 取消照常,让房间能被主动解散。房间本身不动,到期自然失效。
	if req.GetAccept() {
		if tip := l.trialMergeFenceTip(ctx, a); tip != nil {
			return trialRespondOutcome{tip: tip}, nil
		}
	}
	res, err := d.Lobby.Respond(ctx, lobby.LobbyID, a.playerID, req.GetAccept(), nowMs, trialLaunchWindow)
	if err != nil {
		return trialRespondOutcome{}, fmt.Errorf("respond to trial lobby %d (player %d): %w", lobby.LobbyID, a.playerID, err)
	}

	row := activity.Row(lobby.ActivityID)
	switch res.Status {
	case data.TrialLobbyRespondGone:
		return trialRespondOutcome{tip: trialInviteExpiredTip()}, nil
	case data.TrialLobbyRespondNotPending:
		if !res.AlreadyAccepted {
			return trialRespondOutcome{tip: trialInviteExpiredTip()}, nil
		}
		return trialRespondOutcome{view: l.trialView(ctx, d, a, row, now)}, nil
	case data.TrialLobbyRespondDeclined:
		l.notifyTrialRoster(lobby, a.playerID)
		return trialRespondOutcome{view: l.trialView(ctx, d, a, row, now), result: activityResultDeclined}, nil
	case data.TrialLobbyRespondAccepted:
		l.notifyTrialRoster(lobby, a.playerID)
		return trialRespondOutcome{view: l.trialView(ctx, d, a, row, now)}, nil
	case data.TrialLobbyRespondLaunch:
		tip, err := l.launchTrial(ctx, d, a.playerID, lobby, now)
		if err != nil {
			return trialRespondOutcome{}, err
		}
		return trialRespondOutcome{tip: tip, view: l.trialView(ctx, d, a, row, now)}, nil
	default:
		return trialRespondOutcome{}, status.Errorf(codes.Internal, "trial lobby respond returned unknown status %d", res.Status)
	}
}

// ── 确认开战(06 §6.26)───────────────────────────────────────

// trialEnd 是开战失败的原因:既写进房间(end_tip_id / end_parameters),也是本次 RPC 的 error_message。
// 零值 = 没有失败。params 只有带占位符的码才有(kGuildTrialTeamInvalid 的 [reason, player_id])。
type trialEnd struct {
	tipID  uint32
	params []string
}

func (e trialEnd) failed() bool { return e.tipID != 0 }

func (e trialEnd) tip() *base.TipInfoMessage { return blockedTip(e.tipID, e.params) }

func trialServiceBusy() trialEnd { return trialEnd{tipID: constants.ErrTrialServiceBusy} }

// launchTrial 确认开战。调用者是凑齐最后一票、从房间脚本拿到开战权的那一次请求(房间已是 LAUNCHING,
// 别的请求不会再碰它);actor 是那位最后同意的玩家。
//
// 返回 (tip, err):
//   - 都为 nil:已开战,房间进了 LAUNCHED(或至少 match 已回 battle_id,见纪律 2);
//   - tip 非 nil:开战失败,房间已转 ENDED,tip 即结束原因。match 的任何失败都落在这一支(纪律 1);
//   - err 非 nil:guild 自己的库 / Redis 故障。房间同样已转 ENDED(原因 kGuildTrialServiceBusy),
//     被邀请的其他人看到的是"服务繁忙"而不是一个 5 秒后才解套的"开战中"。
//
// 无论哪种结局都推一条 ACTIVITY_CHANGED 给名单里的其他人。
func (l *GuildLogic) launchTrial(ctx context.Context, d *ActivityDeps, actor uint64, lobby *data.TrialLobby, now time.Time) (*base.TipInfoMessage, error) {
	battle, end, fault := l.startTrialBattle(ctx, d, lobby, now)
	if fault != nil {
		end = trialServiceBusy()
	}
	if end.failed() {
		l.endTrialLobby(ctx, d, lobby, end)
		l.notifyTrialRoster(lobby, actor)
		if fault != nil {
			guildActivityActionTotal.Inc(activityTypeTrial, activityActionLaunch, activityResultOf(nil, fault))
			return nil, fault
		}
		tip := end.tip()
		guildActivityActionTotal.Inc(activityTypeTrial, activityActionLaunch, activityResultOf(tip, nil))
		return tip, nil
	}
	l.recordTrialLaunched(ctx, d, lobby, battle, uint64(now.UnixMilli()))
	l.notifyTrialRoster(lobby, actor)
	guildActivityActionTotal.Inc(activityTypeTrial, activityActionLaunch, activityResultOK)
	return nil, nil
}

// startTrialBattle 做开战前的复核并调 match。成功时返回字段齐全的对局键(含 match 发的 battle_id);
// end.failed() = 业务上开不了(原因即 end);fault 非 nil = guild 自己的故障。
//
// 复核的都是"从建房到全员同意这几十秒里可能变了"的东西:活动是否还开着、名单里的人是否还在帮 / 还在线、
// 发起人是否还有次数(他可能刚打完另一局)。活动上下文(两个周期键)取**本次请求**的时刻,也就是开战时刻(§6.26 第 6 步)。
func (l *GuildLogic) startTrialBattle(ctx context.Context, d *ActivityDeps, lobby *data.TrialLobby, now time.Time) (data.TrialBattleKey, trialEnd, error) {
	nowMs := uint64(now.UnixMilli())
	row, ok := activity.PickForWrite(activity.Rows(), activity.TypeTrial, lobby.ActivityID, nowMs)
	if !ok {
		return data.TrialBattleKey{}, trialEnd{tipID: constants.ErrActivityNotOpen}, nil
	}
	rule, ok := activity.Rule()
	if !ok {
		return data.TrialBattleKey{}, trialEnd{}, status.Errorf(codes.Internal, "GuildRule row %d missing", activity.RuleRowID)
	}
	reason, offender, err := l.verifyTrialRoster(ctx, d, lobby.GuildID, lobby.Roster, nowMs, rule.GetActivityJoinMinHours())
	if err != nil {
		return data.TrialBattleKey{}, trialEnd{}, err
	}
	if reason != "" {
		return data.TrialBattleKey{}, trialEnd{tipID: constants.ErrTrialTeamInvalid, params: trialTeamInvalidParams(reason, offender)}, nil
	}
	// 读失败按 0 次处理(activityUsedToday):这里只是不让"明知没次数"的发起人白开一局,权威判定在结算事务里。
	if l.activityUsedToday(ctx, d, lobby.InitiatorID, row.GetId(), now) >= row.GetDailyLimit() {
		return data.TrialBattleKey{}, trialEnd{tipID: constants.ErrActivityAlreadyClaimed}, nil
	}

	battle := data.TrialBattleKey{
		GuildID:           lobby.GuildID,
		ActivityID:        row.GetId(),
		PeriodKey:         activity.DayKey(now),
		GuildPeriodKey:    activity.GuildPeriodKey(row, now),
		InitiatorPlayerID: lobby.InitiatorID,
	}
	battleID, end := callTrialMatch(ctx, d, lobby, row.GetDungeonId(), battle)
	if end.failed() {
		return data.TrialBattleKey{}, end, nil
	}
	battle.BattleID = battleID
	return battle, trialEnd{}, nil
}

// callTrialMatch 按剩余预算调 MatchInternal.StartActivityBattle,并把一切失败映射成房间的结束原因(纪律 1):
//   - 预算不足(不调)、gRPC 错误、超时、match 回 INTERNAL 或不认识的拒绝码、回成功却没给 battle_id → kGuildTrialServiceBusy;
//   - MEMBER_OFFLINE / MEMBER_IN_BATTLE / MEMBER_NOT_READY → kGuildTrialTeamInvalid [offline | in_battle | not_ready, 当事人];
//   - INVALID_ARGUMENT → kGuildTrialTeamInvalid [invalid, 0],并打 ERROR(请求是 guild 拼的,走到这里是 guild 的缺陷)。
//
// 超时的那一次 match 可能其实已经开战:房间仍以"服务繁忙"结束,但那一局的结果到达时结算会补登记、照常发奖(06 §6.34 T1)。
func callTrialMatch(ctx context.Context, d *ActivityDeps, lobby *data.TrialLobby, dungeonID uint32, battle data.TrialBattleKey) (uint64, trialEnd) {
	budget, ok := matchBudgetFor(ctx, d.MatchBudget)
	if !ok {
		logx.Infof("[GuildTrial] launch skipped, remaining budget %v too small to call match (guild %d lobby %d)",
			budget, lobby.GuildID, lobby.LobbyID)
		return 0, trialServiceBusy()
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	resp, err := d.Match.StartActivityBattle(callCtx, &matchpb.StartActivityBattleRequest{
		BattleConfigId:  dungeonID,
		MemberPlayerIds: slices.Clone(lobby.Roster),
		ActivityContext: &battlepb.BattleActivityContext{
			Kind:              battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL,
			GuildId:           battle.GuildID,
			ActivityId:        battle.ActivityID,
			PeriodKey:         battle.PeriodKey,
			InitiatorPlayerId: battle.InitiatorPlayerID,
			GuildPeriodKey:    battle.GuildPeriodKey,
		},
	})
	if err != nil {
		logx.Errorf("[GuildTrial] match StartActivityBattle failed, lobby ends as service busy (guild %d lobby %d): %v",
			lobby.GuildID, lobby.LobbyID, err)
		return 0, trialServiceBusy()
	}
	offender := resp.GetOffenderPlayerId()
	switch resp.GetReject() {
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE:
		if resp.GetBattleId() == 0 {
			logx.Errorf("[GuildTrial] match accepted the battle but returned battle_id 0 (guild %d lobby %d)", lobby.GuildID, lobby.LobbyID)
			return 0, trialServiceBusy()
		}
		return resp.GetBattleId(), trialEnd{}
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE:
		return 0, trialEnd{tipID: constants.ErrTrialTeamInvalid, params: trialTeamInvalidParams(trialReasonOffline, offender)}
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE:
		return 0, trialEnd{tipID: constants.ErrTrialTeamInvalid, params: trialTeamInvalidParams(trialReasonInBattle, offender)}
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY:
		return 0, trialEnd{tipID: constants.ErrTrialTeamInvalid, params: trialTeamInvalidParams(trialReasonNotReady, offender)}
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT:
		logx.Errorf("[GuildTrial] match rejected the request as invalid, this is a guild defect (guild %d lobby %d activity %d dungeon %d roster %v)",
			lobby.GuildID, lobby.LobbyID, battle.ActivityID, dungeonID, lobby.Roster)
		return 0, trialEnd{tipID: constants.ErrTrialTeamInvalid, params: trialTeamInvalidParams(trialReasonInvalid, 0)}
	default:
		logx.Errorf("[GuildTrial] match rejected the battle with %v, lobby ends as service busy (guild %d lobby %d)",
			resp.GetReject(), lobby.GuildID, lobby.LobbyID)
		return 0, trialServiceBusy()
	}
}

// endTrialLobby 把房间从 LAUNCHING 写成 ENDED 并记下原因。失败只记日志:房间 5 秒后会被折算成 ENDED
// (原因显示为服务繁忙,纯展示偏差),成员照样可以重新发起。用独立预算:走到这里时请求预算可能已经见底。
func (l *GuildLogic) endTrialLobby(ctx context.Context, d *ActivityDeps, lobby *data.TrialLobby, end trialEnd) {
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trialAfterLaunchBudget)
	defer cancel()
	moved, err := d.Lobby.Finish(postCtx, lobby.LobbyID,
		pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED,
		0, end.tipID, end.params)
	if err != nil {
		logx.Errorf("[GuildTrial] mark lobby %d ended (tip %d) failed, it will read as ended after the launch window: %v",
			lobby.LobbyID, end.tipID, err)
		return
	}
	if !moved {
		logx.Infof("[GuildTrial] lobby %d was no longer launching when marking it ended (tip %d)", lobby.LobbyID, end.tipID)
	}
}

// recordTrialLaunched 在 match 回了 battle_id 之后登记对局行、把房间写成 LAUNCHED。两步都不让本次 RPC 失败(纪律 2):
//   - 登记失败:计 guild_trial_register_fail_total 并打 ERROR。结算会补登记;代价是结果到达之前巡检器看不见这一局。
//     帮会恰好在这一刻被解散(ErrGuildGone)不算失败:没有帮会就没有可登记的东西,结果到达时结算回"帮会已解散"。
//   - 改房间状态失败:房间 5 秒后显示成"服务繁忙",而参战者其实已经收到开战通知 —— 纯展示偏差,接受(06 §6.27 L4)。
func (l *GuildLogic) recordTrialLaunched(ctx context.Context, d *ActivityDeps, lobby *data.TrialLobby, battle data.TrialBattleKey, nowMs uint64) {
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trialAfterLaunchBudget)
	defer cancel()
	switch err := d.trials.RegisterTrialBattle(postCtx, battle, nowMs); {
	case err == nil:
	case errors.Is(err, data.ErrGuildGone):
		logx.Infof("[GuildTrial] guild %d was disbanded right after battle %d started, nothing to register", battle.GuildID, battle.BattleID)
	default:
		guildTrialRegisterFailTotal.Inc()
		logx.Errorf("[GuildTrial] register battle %d of guild %d failed, settlement will backfill the row: %v",
			battle.BattleID, battle.GuildID, err)
	}
	moved, err := d.Lobby.Finish(postCtx, lobby.LobbyID,
		pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED,
		battle.BattleID, 0, nil)
	if err != nil || !moved {
		logx.Errorf("[GuildTrial] mark lobby %d launched (battle %d) failed, it will read as service busy (moved=%t): %v",
			lobby.LobbyID, battle.BattleID, moved, err)
	}
}

// ── 结算(06 §6.29)───────────────────────────────────────────

// TrialSettleOutcome 是 SettleTrialResult 处理一条对局结果的去向。零值无效(只与非 nil 的 error 同时出现)。
type TrialSettleOutcome uint8

const (
	// TrialOutcomeSettled:本次把这一局结算了(胜 / 负 / 配表缺行都算),结果记录已销账。
	TrialOutcomeSettled TrialSettleOutcome = iota + 1
	// TrialOutcomeDuplicate:这一局此前已结算(重复投递),什么都没改,已销账。
	TrialOutcomeDuplicate
	// TrialOutcomeGuildGone:帮会已解散,不发奖,已销账。
	TrialOutcomeGuildGone
	// TrialOutcomeBadContext:是历练的结果,但活动上下文残缺或对局模式不对,无法处理;battle_id 非 0 时已销账。
	TrialOutcomeBadContext
	// TrialOutcomeContextMismatch:这一局登记在别的帮名下,什么都没改,已销账。
	TrialOutcomeContextMismatch
	// TrialOutcomePoison:确定性失败(帮贡 / 资金溢出、奖励包坏、结果过旧、输入畸形),对局标成 POISON,
	// 不发奖、已销账;需要运维按 ERROR 日志人工补发。
	TrialOutcomePoison
	// TrialOutcomeSkipped:不是帮会历练的结果(没有活动上下文,或是别的活动类型)。什么都没读、什么都没改,
	// **也不销账** —— 别的活动类型的结果记录不归 guild 删。
	TrialOutcomeSkipped
)

// errTrialNotWired:结算被调用时历练的仓储或结果记录没有接线。只可能是装配错误;按暂时性错误返回,
// 让消费者停在这条消息上重试并持续报错(fail-closed:宁可卡住也不把结果丢掉)。
var errTrialNotWired = errors.New("guild trial settlement not wired (ActivityDeps.Repo / Results missing)")

// errTrialDeterministic 标记 logic 自己发现的确定性失败(奖励包序列化不了),与仓储的两个确定性哨兵同样走毒消息。
var errTrialDeterministic = errors.New("guild trial: deterministic failure")

// trialSyncDeliveryReserve:结算提交之后,给本局全部物品指令的同步投递合计留的时间上限之外的尾巴,
// 取值与用法同 syncTailReserve(assetop 落库 700ms + 余量)。结算没有 RPC 截止时间,
// 所以由 SettleTrialResult 自己给"提交后同步投递"这一段设一个合计上限 SyncBudget + 本值,
// 不让一局 5 个人的投递把结果消费(单协程串行,本副本名下的全部分区)拖住十几秒;投不完的交给重投循环(行上的租约到期即被领取)。
const trialSyncDeliveryReserve = syncTailReserve

// SettleTrialResult 结算一条对局结果事件。结果消费者(Kafka match-results,消费组 guild-trial)与巡检器共用。
//
// 返回语义(消费者据此决定提交还是重试):
//   - error 为 nil:这条消息处理完了 —— 已结算、判为重复 / 帮会已解散 / 上下文不可用 / 毒消息,或与 guild 无关。
//     消费者**提交 offset**。outcome 说明是哪一种,只用于日志与测试。
//   - error 非 nil:**暂时性失败**(MySQL / Redis 故障、写冲突、合服闸门、号段不可用、未接线)。什么都没提交,
//     结果记录没有销账;消费者**不提交**,退避后带同一条消息重调。每次调用都会重取"现在"。
//
// 幂等:同一条消息可以调任意多次、可以与巡检器并发调,发奖至多一次。幂等闸门在对局行上,有两道:
// 入口处不持锁的终态预读(trialSettledBefore,已是 SETTLED 就直接销账回 Duplicate),与事务里持帮会行锁的权威判定(data 层)。
// 线程模型:可并发调用;本方法不持有任何跨调用的状态。ctx 取消时尽快返回(事务回滚,返回 error)。
//
// 发奖名单 = team 0 的玩家 − 逃跑者(用户决策 U2:阵亡者也得奖)∩ 结算时仍在本帮 ∩ 开战那个游戏日次数未满;
// 后两条由仓储在事务里按锁住的行判。合服闸门照常生效:帮会所在 zone 正在合服时回暂时性错误,合服结束后照常结算。
func (l *GuildLogic) SettleTrialResult(ctx context.Context, ev *kafkapb.BattleResultEvent) (TrialSettleOutcome, error) {
	actx := ev.GetActivityContext()
	if actx.GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL {
		return TrialOutcomeSkipped, nil
	}
	d := l.activities
	if d == nil || d.trials == nil || d.Results == nil {
		return 0, errTrialNotWired
	}
	now := activityNow(d)
	nowMs := uint64(now.UnixMilli())
	battle := data.TrialBattleKey{
		BattleID:          ev.GetBattleId(),
		GuildID:           actx.GetGuildId(),
		ActivityID:        actx.GetActivityId(),
		PeriodKey:         actx.GetPeriodKey(),
		GuildPeriodKey:    actx.GetGuildPeriodKey(),
		InitiatorPlayerID: actx.GetInitiatorPlayerId(),
	}
	if battle.BattleID == 0 || battle.GuildID == 0 || battle.ActivityID == 0 || battle.PeriodKey == 0 || battle.GuildPeriodKey == 0 ||
		ev.GetMatchMode() != uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM) {
		// 上下文是 guild 自己发给 match、battle 原样回显的,正常流程不会残缺。真出现时无从结算,也没有重试的意义。
		logx.Errorf("[GuildTrial] result with unusable activity context dropped (battle %d guild %d activity %d period %d guild_period %d match_mode %d)",
			battle.BattleID, battle.GuildID, battle.ActivityID, battle.PeriodKey, battle.GuildPeriodKey, ev.GetMatchMode())
		guildTrialResultTotal.Inc(trialResultBadContext)
		if battle.BattleID != 0 {
			l.ackTrialResult(ctx, d, battle.BattleID)
		}
		return TrialOutcomeBadContext, nil
	}

	// 幂等短路:这一局已有终态 → 这是一份重复的结果,销账了事。排在构建奖励包、发号、开事务**之前**,为的是两件事:
	//   - battle 在结果记录被销账之前每 10s 按原字节重发一次(至多 30 次,key = battle_id,同一分区)。这些副本不必各烧
	//     一批指令号、各抢一次帮会行锁;合服期间它们也不会被事务里的闸门拦成"暂时性失败"而把消费卡住。
	//   - 它是"卡住的消息"的人工出口(06 §6.35a 做法 A):运维把对局行手工写成终态之后,不论原先失败在这一步之后的
	//     哪个位置(奖励包、发号、合服闸门、事务里的任何一步,包括在那些位置上 panic),这条消息连同排在它后面的全部
	//     重发副本都从这里走掉。没有这一步,闸门只在事务里、排在合服闸门之后,前面几步的失败就没有不重启的出口。
	// 读失败按暂时性错误返回:事务用的是同一个库,硬往下走只会多烧一批号。
	settled, err := trialSettledBefore(ctx, d, battle)
	if err != nil {
		return 0, fmt.Errorf("read trial battle %d of guild %d before settling: %w", battle.BattleID, battle.GuildID, err)
	}
	if settled {
		l.ackTrialResult(ctx, d, battle.BattleID)
		guildTrialResultTotal.Inc(trialResultDuplicate)
		return TrialOutcomeDuplicate, nil
	}

	// 配表行不存在或已不是历练 → 交给仓储记 CONFIG_MISSING、不发奖。不要求此刻仍在档期内:对局是开战时合法发起的。
	row := activity.Row(battle.ActivityID)
	if row != nil && row.GetType() != activity.TypeTrial {
		row = nil
	}
	if row == nil {
		logx.Errorf("[GuildTrial] GuildActivity[%d] missing or no longer a trial, battle %d of guild %d settles without reward",
			battle.ActivityID, battle.BattleID, battle.GuildID)
	}
	in := data.TrialSettleInput{
		Battle:       battle,
		Activity:     row,
		Win:          ev.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
		Candidates:   trialCandidates(ev),
		FinishedAtMs: ev.GetFinishedAtMs(),
		NowMs:        nowMs,
		Fence:        l.economyFence,
	}
	var bundle *assetpb.AssetBundle
	if in.Win && row != nil && len(in.Candidates) > 0 {
		if bundle, err = activity.BuildRewardBundle(row.GetRewardId()); err != nil {
			return l.poisonTrialBattle(ctx, d, battle, in.FinishedAtMs, nowMs, fmt.Sprintf("build reward bundle: %v", err))
		}
		if bundle != nil {
			in.Reward, err = prepareTrialReward(ctx, d, in.Candidates, bundle, nowMs)
			if errors.Is(err, errTrialDeterministic) {
				return l.poisonTrialBattle(ctx, d, battle, in.FinishedAtMs, nowMs, err.Error())
			}
			if err != nil {
				return 0, fmt.Errorf("prepare reward ops of trial battle %d (guild %d): %w", battle.BattleID, battle.GuildID, err)
			}
		}
	}

	res, err := d.trials.SettleTrialBattleTx(ctx, in)
	switch {
	case err == nil:
	case errors.Is(err, data.ErrTrialInputInvalid), errors.Is(err, data.ErrActivityPoison):
		return l.poisonTrialBattle(ctx, d, battle, in.FinishedAtMs, nowMs, err.Error())
	default:
		return 0, fmt.Errorf("settle trial battle %d of guild %d: %w", battle.BattleID, battle.GuildID, err)
	}

	switch res.Status {
	case data.TrialSettleSettled:
	case data.TrialSettleDuplicate:
		l.ackTrialResult(ctx, d, battle.BattleID)
		guildTrialResultTotal.Inc(trialResultDuplicate)
		return TrialOutcomeDuplicate, nil
	case data.TrialSettleGuildGone:
		logx.Infof("[GuildTrial] guild %d no longer exists, battle %d settles without reward", battle.GuildID, battle.BattleID)
		l.ackTrialResult(ctx, d, battle.BattleID)
		guildTrialResultTotal.Inc(trialResultGuildGone)
		return TrialOutcomeGuildGone, nil
	case data.TrialSettleContextMismatch:
		logx.Errorf("[GuildTrial] battle %d is registered to another guild than the result says (%d), nothing settled",
			battle.BattleID, battle.GuildID)
		l.ackTrialResult(ctx, d, battle.BattleID)
		guildTrialResultTotal.Inc(trialResultContextMismatch)
		return TrialOutcomeContextMismatch, nil
	default:
		return 0, fmt.Errorf("settle trial battle %d of guild %d: repo returned unknown status %d", battle.BattleID, battle.GuildID, res.Status)
	}

	// 已提交。此后全部 best-effort(activity_logic.go 纪律 3):销账最先 —— 它决定 battle 还要不要重发。
	l.ackTrialResult(ctx, d, battle.BattleID)
	countTrialSettled(res, in.FinishedAtMs, nowMs, bundle != nil)
	l.notifyTrialSettled(ctx, battle, ev, res.FundsGranted)
	l.deliverTrialRewards(ctx, d, battle, bundle, res.Enqueued)
	return TrialOutcomeSettled, nil
}

// trialSettledBefore 报告这一局是否已经有了终态:对局行是 SETTLED,且登记在结果所说的那个帮名下。普通读,不持任何锁。
//
// SETTLED 是不可变终态:写对局行的五条路径(登记 / 结算 / 标毒 / 判过期 / 解散)没有一条会改或删 SETTLED 行
// (data.readTrialGate 见到它就回"已结算",解散只删 STARTED 行),所以不持锁读到它就是权威的。
// 其余情况 —— 没有行、STARTED、EXPIRED、登记在别的帮名下 —— 一律回 false:由结算事务里持着帮会行锁的闸门去判,
// 这里不下结论(不持锁读到的"还没结算"随时可能过期)。err 非 nil = guild 自己的库故障。
func trialSettledBefore(ctx context.Context, d *ActivityDeps, battle data.TrialBattleKey) (bool, error) {
	row, found, err := d.trials.TrialBattle(ctx, battle.BattleID)
	if err != nil {
		return false, err
	}
	return found && row.GuildID == battle.GuildID &&
		row.State == pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED, nil
}

// trialTeamZero 返回结果事件里己方(team_index 0)的玩家,去掉 0、保持出现顺序(可能有重复,由使用方去重)。
func trialTeamZero(ev *kafkapb.BattleResultEvent) []uint64 {
	var out []uint64
	for _, team := range ev.GetTeams() {
		if team.GetTeamIndex() != 0 {
			continue
		}
		for _, playerID := range team.GetPlayerIds() {
			if playerID != 0 {
				out = append(out, playerID)
			}
		}
	}
	return out
}

// trialCandidates 从结果事件里取发奖候选:己方玩家 − 逃跑者,去重、升序。
// 用户决策 U2(README §3、06 顶部覆盖块):**阵亡者也得奖**,dead_player_ids 不参与过滤(字段保留只供统计)。
// 0 不是玩家,直接丢弃(仓储会把含 0 的候选整局判成畸形输入,不能因为一个坏 id 让其他人都拿不到奖)。
func trialCandidates(ev *kafkapb.BattleResultEvent) []uint64 {
	fled := make(map[uint64]struct{}, len(ev.GetFledPlayerIds()))
	for _, playerID := range ev.GetFledPlayerIds() {
		fled[playerID] = struct{}{}
	}
	var out []uint64
	for _, playerID := range trialTeamZero(ev) {
		if _, ran := fled[playerID]; !ran {
			out = append(out, playerID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// prepareTrialReward 为每位候选人预备一份物品指令参数(事务前发号、生成租约令牌;06 §6.29 第 5 步)。
// 候选人里谁最终得奖要到事务里才知道,所以按候选人全量预备;没用上的号浪费掉即可(号段只进不退)。
//
// 错误分两类:包了 errTrialDeterministic 的是确定性失败(奖励包序列化不了)→ 调用方走毒消息;
// 其余(发号器未接线 / 发号失败 / 取不到随机数)是暂时性的 → 调用方原样返回,由消费者退避重试。
// **不看资产通道开没开**(d.Loop):建房时已拒绝了"通道关着还带物品"的历练;开战之后通道才被关掉的,
// 指令行照插、保持 PENDING,通道重新打开后由重投循环投递 —— 永不中止的物品不能因为一次运维操作被跳过。
func prepareTrialReward(ctx context.Context, d *ActivityDeps, candidates []uint64, bundle *assetpb.AssetBundle, nowMs uint64) (*data.TrialReward, error) {
	payload, err := proto.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal reward bundle: %v", errTrialDeterministic, err)
	}
	if d.OpIDs == nil {
		return nil, errors.New("asset op id minter not wired")
	}
	lease := d.Lease
	if lease <= 0 {
		lease = defaultInsertLease
	}
	ops := make([]data.TrialRewardOp, 0, len(candidates))
	for _, playerID := range candidates {
		token, err := newAssetOpToken()
		if err != nil {
			return nil, err
		}
		opID, err := d.OpIDs.Mint(ctx)
		if err != nil {
			return nil, fmt.Errorf("mint asset op id: %w", err)
		}
		if opID == 0 {
			return nil, errors.New("asset op id generator returned 0")
		}
		ops = append(ops, data.TrialRewardOp{PlayerID: playerID, OpID: opID, LeaseToken: token})
	}
	return &data.TrialReward{Payload: payload, LeaseUntilMs: nowMs + durationMs(lease), Ops: ops}, nil
}

// ackTrialResult 销账:删掉 battle 落在 SharedRedis 上的结果记录,battle 的重发随之停止。
// 失败只记日志:battle 重发后 guild 再走一遍幂等闸门回 Duplicate,然后再销一次;最坏由 7 天 TTL 兜底。
func (l *GuildLogic) ackTrialResult(ctx context.Context, d *ActivityDeps, battleID uint64) {
	if err := d.Results.Ack(ctx, battleID); err != nil {
		logx.Errorf("[GuildTrial] ack result record of battle %d failed, battle will resend and get a duplicate answer: %v", battleID, err)
	}
}

// poisonTrialBattle 把一局标成"确定性失败、不发奖"并销账(06 §6.31)。结算与巡检器共用;
// battle 至少要有 BattleID 与 GuildID(巡检器只有这两个),结算会把上下文的其余字段也带上,
// 这样连登记都没成功的局也能留下一行可查。cause 只进日志。
//
// 返回 (outcome, nil) = 这一局不必再处理:
//   - 标成了 POISON,或没有登记行可标 → TrialOutcomePoison。打 ERROR:需要人工核对并补发;
//   - 发现它其实早已结算 → TrialOutcomeDuplicate(这次的"坏"只是一条重复消息的坏,什么都不用补);
//   - 帮会已解散 → TrialOutcomeGuildGone;登记在别的帮名下 → TrialOutcomeContextMismatch。
//
// 标记本身遇到库故障 / 写冲突时返回暂时性 error,调用方下一轮再来,不销账。
func (l *GuildLogic) poisonTrialBattle(ctx context.Context, d *ActivityDeps, battle data.TrialBattleKey,
	finishedAtMs, nowMs uint64, cause string) (TrialSettleOutcome, error) {
	mark, err := d.trials.MarkTrialBattlePoison(ctx, battle, finishedAtMs, nowMs)
	if err != nil && !errors.Is(err, data.ErrTrialInputInvalid) {
		return 0, fmt.Errorf("mark trial battle %d of guild %d as poison (cause: %s): %w", battle.BattleID, battle.GuildID, cause, err)
	}
	outcome, label := TrialOutcomePoison, trialResultPoison
	switch {
	case err != nil:
		// 连标记的入参都不合法(键为 0):同样是确定性的,重试多少次都一样。留不下行,只能靠这条日志。
		logx.Errorf("[GuildTrial] POISON battle %d of guild %d cannot even be marked (%v), result dropped, manual check needed: %s",
			battle.BattleID, battle.GuildID, err, cause)
	case mark == data.TrialMarkAlreadySettled:
		outcome, label = TrialOutcomeDuplicate, trialResultDuplicate
		logx.Infof("[GuildTrial] battle %d of guild %d was already settled, bad duplicate ignored: %s", battle.BattleID, battle.GuildID, cause)
	case mark == data.TrialMarkGuildGone:
		outcome, label = TrialOutcomeGuildGone, trialResultGuildGone
		logx.Infof("[GuildTrial] guild %d no longer exists, bad result of battle %d ignored: %s", battle.GuildID, battle.BattleID, cause)
	case mark == data.TrialMarkMismatch:
		outcome, label = TrialOutcomeContextMismatch, trialResultContextMismatch
		logx.Errorf("[GuildTrial] battle %d is registered to another guild than %d, bad result ignored: %s", battle.BattleID, battle.GuildID, cause)
	case mark == data.TrialMarkMissing:
		logx.Errorf("[GuildTrial] POISON battle %d of guild %d has no registered row to mark, no reward granted, manual compensation may be needed: %s",
			battle.BattleID, battle.GuildID, cause)
	default:
		logx.Errorf("[GuildTrial] POISON battle %d of guild %d (activity %d period %d) marked, no reward granted, manual compensation needed: %s",
			battle.BattleID, battle.GuildID, battle.ActivityID, battle.PeriodKey, cause)
	}
	if battle.BattleID != 0 {
		l.ackTrialResult(ctx, d, battle.BattleID)
	}
	guildTrialResultTotal.Inc(label)
	return outcome, nil
}

// countTrialSettled 给一次成功的结算计指标。hasItems = 本局配了物品奖励。
func countTrialSettled(res data.TrialSettleResult, finishedAtMs, nowMs uint64, hasItems bool) {
	switch res.Result {
	case pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN:
		guildTrialResultTotal.Inc(trialResultSettledWin)
	case pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_CONFIG_MISSING:
		guildTrialResultTotal.Inc(trialResultConfigMissing)
	default:
		guildTrialResultTotal.Inc(trialResultSettledLoss)
	}
	if finishedAtMs != 0 && nowMs >= finishedAtMs {
		guildTrialResultLagSeconds.ObserveFloat(float64(nowMs-finishedAtMs) / 1000)
	}
	if res.FundsGranted {
		guildActivityFundsGrantedTotal.Inc(activityTypeTrial)
	}
	// 物品去向按人计。没配物品时得奖者都记 no_items。
	if !hasItems {
		if n := len(res.Rewarded); n > 0 {
			guildActivityRewardTotal.Add(float64(n), activityTypeTrial, activityRewardNoItems)
		}
		return
	}
	if n := len(res.Enqueued); n > 0 {
		guildActivityRewardTotal.Add(float64(n), activityTypeTrial, activityRewardEnqueued)
	}
	if n := len(res.Owed); n > 0 {
		guildActivityRewardTotal.Add(float64(n), activityTypeTrial, activityRewardOwed)
	}
}

// notifyTrialSettled 在结算提交后推 ACTIVITY_CHANGED(06 §6.29 第 7 步):本局计了帮会资金 → 全帮成员
// (资金与今日胜场是全帮都看得到的数);否则只给己方参战者(他们的"进行中"要消掉,得奖者的次数与帮贡变了)。
// 结算没有"操作者":没有谁手上拿着回包,所以参战者全部在收件人里;actor 填发起人只作展示。
// 取成员名单失败时退回只推参战者:推送本来就是至多一次的提示。
func (l *GuildLogic) notifyTrialSettled(ctx context.Context, battle data.TrialBattleKey, ev *kafkapb.BattleResultEvent, fundsGranted bool) {
	recipients := trialTeamZero(ev)
	if fundsGranted && l.repo != nil {
		g, err := l.repo.GetGuild(ctx, battle.GuildID)
		if err != nil {
			logx.Errorf("[GuildTrial] load guild %d for settle push failed, notifying participants only: %v", battle.GuildID, err)
		} else {
			recipients = append(recipients, membersExcept(g)...)
		}
	}
	l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED, battle.GuildID, battle.InitiatorPlayerID, 0, recipients)
}

// deliverTrialRewards 在结算提交后把本局插下的物品指令各同步投一次,让玩家打完就看到物品到账(06 §6.29 第 7 步)。
//
// 与 RPC 里的同步投递有两点不同:
//   - **不打 withSyncDelivery 标记**:这里没有调用方拿着回包,指令终结时要照常推 DELIVERY_DONE 给玩家;
//   - 结算没有请求截止时间,所以整段设一个合计上限(SyncBudget + trialSyncDeliveryReserve),逐个按剩余时间取预算,
//     不足 300ms 的不投、计 guild_asset_sync_skipped_total{kind="activity"}。行上带着插行租约,到期后由重投循环接手。
//
// 逐个顺序投,不并发:每个副本压向 scene 的资产 RPC 并发数由重投循环的 Workers 封顶,这里不再另开一路。
// 资产通道关着(Loop 为 nil)时不投:行保持 PENDING,通道打开后由重投循环投递。错误只打日志。
func (l *GuildLogic) deliverTrialRewards(ctx context.Context, d *ActivityDeps, battle data.TrialBattleKey,
	bundle *assetpb.AssetBundle, enqueued []data.TrialEnqueuedReward) {
	if len(enqueued) == 0 {
		return
	}
	if d.Loop == nil {
		logx.Errorf("[GuildTrial] asset channel is closed, %d reward ops of battle %d stay pending until it is enabled",
			len(enqueued), battle.BattleID)
		return
	}
	postCtx, cancel := context.WithTimeout(ctx, d.SyncBudget+trialSyncDeliveryReserve)
	defer cancel()
	for i, reward := range enqueued {
		budget, ok := syncBudgetFor(postCtx, d.SyncBudget)
		if !ok {
			// 时间只会更少:剩下的都不投了。
			guildAssetSyncSkippedTotal.Add(float64(len(enqueued)-i), assetKindActivity)
			logx.Infof("[GuildTrial] sync delivery of %d reward ops skipped, left to reconcile loop (battle %d)", len(enqueued)-i, battle.BattleID)
			return
		}
		deliverCtx, cancelOne := context.WithTimeout(postCtx, budget)
		_, err := d.Loop.ProcessOne(deliverCtx, assetop.Op{
			OpID:          reward.OpID,
			PlayerID:      reward.PlayerID,
			Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
			Seq:           reward.Seq,
			StreamEpoch:   reward.StreamEpoch,
			CorrelationID: reward.OpID,
			TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD),
			Bundle:        bundle,
			// 活动奖励永不中止(06 §6.9):背包满就保持 PENDING,腾出空间后自动到账。
			DeadlineMs: 0,
			LeaseToken: reward.LeaseToken,
		})
		cancelOne()
		if err != nil {
			logx.Errorf("[GuildTrial] sync delivery failed, left to reconcile loop op_id=%d battle=%d: %v", reward.OpID, battle.BattleID, err)
		}
	}
}
