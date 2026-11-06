package logic

// 写请求的「预期帮会」栅栏(docs/design/client-request-ordering.md O14 / §6.4,批次 E2)。
//
// 要防的事:gate 路由 lane 让同一会话的请求按收包顺序逐条发出,但路由服按超时回 1003、gate 放行下一条之后,
// 上一条仍可能在本服务里跑完(go-zero 超时拦截器到期后 handler 不停;截止前已发出的那一次 COMMIT 也撤不回)。
// 客户端在这段时间里可能已经离开 G、加入了 H —— 迟到的 LeaveGuild / DisbandGuild / KickGuildMember /
// TransferGuildLeader / SetGuildMemberRole 若按「操作者此刻所在的帮」执行,就会作用到 H 上:把玩家从新帮会移出、
// 解散新帮会、在新帮会里踢人 / 转让 / 任免。
//
// 修法:请求带上客户端发出时看到的本人帮会 expected_guild_id,它**取代** operatorGuild 的查询结果作为事务的 guild_id。
// 事务在锁住的行上复核操作者是不是该帮成员(repo 的既有语义:不是成员 → ErrNotGuildMember,帮会行已不存在 → ErrGuildGone;
// 解散只比对 leader_id,由 data.disbandRefusal 在同一把 guild 行锁下补读成员行区分「不是帮主」与「已不是成员」),
// 所以「是不是预期帮会」与写入在同一个事务里原子判定,不是检查到使用的两步:给帮会增删成员的事务都先锁 guild 行,
// 复核与提交之间没人能把操作者换进 / 换出这个帮。
//
// 语义:
//   - expected_guild_id = 0:旧客户端。行为与加字段之前完全相同(operatorGuild 定位帮会),只多计一次
//     guild_manage_expected_guild_total{outcome="absent"}。五个方法都只受理客户端来源(无会话调用在身份闸就被拒,
//     走不到这里),所以计数只衡量客户端。
//   - expected_guild_id ≠ 0 且操作者此刻不在该帮(在别的帮、不在任何帮、该帮已解散):回 kGuildNotInGuild,不写、不推送、
//     不 bump;回包有 guild 字段的三个方法附上操作者**当前**所在帮会的快照(不在任何帮则不填),客户端据此直接纠正界面。
//     计 outcome="mismatch"。
//   - 一致:照常执行,结果与 expected_guild_id = 0 时相同。
//
// 为什么事务前还要预检一次:expected_guild_id 来自客户端。直接拿去进事务,等于让任何人都能对任意帮会的 guild 行加
// FOR UPDATE,踢人 / 解散还会在事务外给那个帮的成员建状态行(KickMember 的 G4 预读、DisbandGuild 的事务外建行)。
// 预检先按缓存比对、不一致再用 MySQL 复核(顺手纠正陈旧映射),确认他此刻在该帮才进事务;判据仍是事务里那次复核
// (AGENTS.md §11.3:预检不代替提交点复核)。预检与原来的 operatorGuild 同价:缓存命中且一致时不读 MySQL。
//
// 退帮的一个差别:expected_guild_id = 0 时「事务说不在、MySQL 复核也为 0」按幂等成功处理;expected_guild_id ≠ 0 时
// 一律回 kGuildNotInGuild —— 它回答的是「你不在该帮」,客户端拉 GetPlayerGuild 刷新即可。缓存已失效时,旧路径的重放
// 本来也是 operatorGuild 回 kGuildNotInGuild,两条路径对客户端是同一个码。

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"

	"guild/internal/constants"
	"guild/internal/data"
	base "proto/common/base"
	pb "proto/guild"
)

// expected_guild 指标的 method 取值。**固定集合**(AGENTS.md §9:不放 player_id / guild_id)。
const (
	expectedGuildMethodLeave    = "leave"
	expectedGuildMethodDisband  = "disband"
	expectedGuildMethodKick     = "kick"
	expectedGuildMethodTransfer = "transfer"
	expectedGuildMethodSetRole  = "set_role"
)

// expected_guild 指标的 outcome 取值:absent = 客户端没带(旧客户端,行为不变);mismatch = 带了但操作者此刻不在该帮,已拒绝、未写。
// 带了且一致的请求不计:它与 go-zero 自带的按方法请求计数之差就是 absent,另计一份只是重复。
const (
	expectedGuildOutcomeAbsent   = "absent"
	expectedGuildOutcomeMismatch = "mismatch"
)

var guildExpectedGuildTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "guild",
	Subsystem: "manage",
	Name:      "expected_guild_total",
	Help: "带 expected_guild_id 的五个写 RPC(leave / disband / kick / transfer / set_role)的栅栏计数。" +
		"absent = 客户端没带(旧客户端,按服务端查到的帮会执行);mismatch = 操作者此刻不在预期帮会,已拒绝、未写。" +
		"absent 降到 0 之前,超时后晚到的旧请求仍可能作用到玩家的新帮会。",
	Labels: []string{"method", "outcome"},
})

// expectedGuildObserved 是计数的唯一出口。包级可替换钩子,**只许单测替换**(与 data.txDeadlockObserved 同一惯例:
// go-zero 的指标在未启用 Prometheus 时 Inc 是空操作,测试里读不回来)。
var expectedGuildObserved = func(method, outcome string) { guildExpectedGuildTotal.Inc(method, outcome) }

// errExpectedGuildMismatch:预检已确认操作者此刻不在 expected_guild_id 指的帮。只在 bindExpectedGuild 与
// expectedGuildLost 之间流转,调用方必须先用 expectedGuildLost 截住它,不得原样当 gRPC 错误返回。
var errExpectedGuildMismatch = errors.New("operator is no longer in the expected guild")

// bindExpectedGuild 决定本次写事务用哪个 guild_id(见文件头)。
//
// 返回约定与 operatorGuild 一致:err 非 nil = 故障,tip 非 nil = 业务拒绝(只来自 expected = 0 时的 operatorGuild:
// 不在任何帮);另有 err = errExpectedGuildMismatch 表示预检确认不在预期帮,调用方用 expectedGuildLost 识别。
// 其余情况 guildID 就是事务要用的帮会。
func (l *GuildLogic) bindExpectedGuild(ctx context.Context, who caller, expected uint64, method string) (uint64, *base.TipInfoMessage, error) {
	if expected == 0 {
		expectedGuildObserved(method, expectedGuildOutcomeAbsent)
		return l.operatorGuild(ctx, who.playerID)
	}
	cached, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
	if err != nil {
		return 0, nil, err
	}
	if cached == expected {
		// 缓存一致就直接进事务:缓存陈旧时由事务里的成员行复核兜住(ErrNotGuildMember / ErrGuildGone)。
		return expected, nil, nil
	}
	// 缓存说不一致:以 MySQL 复核(不同则顺手失效映射),免得一个陈旧的缓存把合法请求挡回去。
	current, err := l.repo.VerifyPlayerGuildID(ctx, who.playerID, cached)
	if err != nil {
		return 0, nil, err
	}
	if current != expected {
		return 0, nil, errExpectedGuildMismatch
	}
	return expected, nil, nil
}

// expectedGuildLost:expected ≠ 0 时,这些错误都意味着「操作者此刻不在 expected 帮」——预检的 errExpectedGuildMismatch、
// 事务锁成员行没找到(ErrNotGuildMember)、帮会行已不存在(ErrGuildGone)。expected = 0 恒为 false,旧路径的错误映射不变。
func expectedGuildLost(expected uint64, err error) bool {
	if expected == 0 || err == nil {
		return false
	}
	return errors.Is(err, errExpectedGuildMismatch) || errors.Is(err, data.ErrNotGuildMember) || errors.Is(err, data.ErrGuildGone)
}

// rejectExpectedGuild 组「不在预期帮」的答复:计 mismatch、记 Info,返回 kGuildNotInGuild。
//
// 事务那一支(cause 不是 errExpectedGuildMismatch)说明缓存曾说他在 expected、MySQL 说不在:先用 verifyMapping 纠正映射,
// 调用方接着取的当前快照才读得到真相。预检那一支已经由 VerifyPlayerGuildID 纠正过,不再重复一次 MySQL 读与代次翻转。
func (l *GuildLogic) rejectExpectedGuild(ctx context.Context, playerID, expected uint64, method string, cause error) *base.TipInfoMessage {
	if !errors.Is(cause, errExpectedGuildMismatch) {
		l.verifyMapping(ctx, playerID, expected)
	}
	expectedGuildObserved(method, expectedGuildOutcomeMismatch)
	// 记 Info 不记 Error:晚到的旧请求被拦下正是栅栏在起作用,不是故障。
	logx.WithContext(ctx).Infof("[guild] %s refused: player %d is no longer in expected guild %d (%v)",
		method, playerID, expected, cause)
	return tipErr(constants.ErrNotInGuild, "not a member of the expected guild")
}

// rejectExpectedGuildWithView 是回包有 guild 字段的三个方法(任免 / 踢人 / 转让)用的答复:rejectExpectedGuild 之后
// 附上操作者**当前**所在帮会的快照。顺序不能换:先纠正映射,再取快照。
func (l *GuildLogic) rejectExpectedGuildWithView(ctx context.Context, playerID, expected uint64, method string, cause error) (*base.TipInfoMessage, *pb.GuildInfo) {
	tip := l.rejectExpectedGuild(ctx, playerID, expected, method, cause)
	return tip, l.currentGuildView(ctx, playerID)
}

// currentGuildView 取操作者当前所在帮会的快照,不在任何帮回 nil。
//
// 展示字段:读不到就不填、只记日志 —— 拒绝本身已经由事务(或 MySQL 预检)决定,不能因为附带的快照读失败而改成故障。
// 走 ResolvePlayerGuild:缓存说 0 或快照里没有本人时以 MySQL 复核,不把陈旧快照当成「当前帮会」回给客户端。
func (l *GuildLogic) currentGuildView(ctx context.Context, playerID uint64) *pb.GuildInfo {
	guild, err := l.repo.ResolvePlayerGuild(ctx, playerID)
	if err != nil {
		logx.WithContext(ctx).Errorf("[guild] WARN resolve current guild of player %d for the expected-guild reply failed, "+
			"replying without a snapshot: %v", playerID, err)
		return nil
	}
	if guild == nil {
		return nil
	}
	return l.guildInfoFor(ctx, guild, playerID)
}
