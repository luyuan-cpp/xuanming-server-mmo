package logic

// 成员 / 帮主展示名的批量解析(设计 docs/design/guild-phase2/03-names.md §3.21,
// 装配方式以 90-consistency.md Y-02 为准:函数式 Option WithPlayerNames)。
//
// 两条贯穿全文件的纪律:
//
//  1. **名字是展示数据,读取侧 fail-open**。data_service 的名字注册表是唯一真源
//     (proto/data_service/data_service.proto「Player name registry」),guild 不落库、不缓存。
//     拿不到名字时成员列表照常返回、名字留空,客户端回落成"道友 · <编号>"。
//     把一次取名失败升级成 GetGuild 失败,等于让一个纯展示依赖决定帮会功能的可用性。
//  2. **每个填充点一次请求只发一次批量查询**。成员 + 帮主、一页榜单、一份申请名单
//     各自合并成一次 BatchGetPlayerName;在循环里逐个查会把一次读放大成 N 次 RPC。

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"

	dspb "proto/data_service"
)

// PlayerNameResolver 批量取展示名。
//
// 契约(实现必须全部满足):
//   - fail-open:任何错误(RPC 失败、超时、依赖未接线)都返回空 map,**不返回 error**;
//   - 查不到名字的 id(早于名字功能建的角色、已释放的名字)不出现在返回的 map 里;
//   - 入参里的 0 与重复 id 由实现自己处理,调用方不必预先清洗;
//   - 返回值只读:调用方只按 id 取值,不得写入。
type PlayerNameResolver interface {
	BatchResolve(ctx context.Context, playerIDs []uint64) map[uint64]string
}

// DefaultPlayerNameLookupTimeout 是一次 BatchGetPlayerName 的预算。
//
// 预算口径(§3.21):guild zrpc 服务端超时 4000ms,整请求业务预算 3500ms(config.RequestBudget);
// GetGuild = 仓储读 + 在线 MGET + 取名,取名给 800ms。这是**上限**不是保底:lookupCtx 派生自请求 ctx,
// 实际最多等 min(800ms, 整请求剩余预算);排在前面的在线 MGET 没有独立超时,它卡住时留给取名的可能是 0
// (BatchResolve 对"ctx 已结束"单独处理,不计失败)。data_service 侧命中缓存时是一次 Redis MGET,
// 未登记的 id 还有 60s 负缓存,所以无名成员不会让每次拉取都回源打库。
// 刻意小于 DefaultHomeZoneLookupTimeout(1500ms):归属区查不到要拒绝请求,值得多等;
// 名字查不到只是少显示几个字,不值得。
const DefaultPlayerNameLookupTimeout = 800 * time.Millisecond

// playerNameBatchLimit 与 data_service 的 store.PlayerNameBatchLimit 同值(契约 §3.1)。
// 两个 module 不互相 import,所以各留一份;data_service 对超限请求回 InvalidArgument 而不是截断。
//
// 客户端路径到不了这个数:单帮成员 ≤ constants.MaxGuildMembersCap(100),客户端一页榜单 ≤
// constants.MaxRankPageSize(50),待审名单 ≤ GuildRule 校验上限 500。唯一可能超限的是内部调用的
// GetGuildRank(页长不夹)。真超了就只查前 500 个并记 ERROR —— 后面的人名字留空,
// 好过整批被 data_service 拒掉、所有人都没名字。
const playerNameBatchLimit = 500

// guildPlayerNameLookupFailedTotal 计"整次批量取名失败"的次数(不是按 id 计)。
// 只计**真的发出去又失败**的 RPC;请求 ctx 在发 RPC 之前就已结束的不计(见 BatchResolve),
// 那种情况的病根在上游(断线 / 预算被别的依赖吃光),算进来会把告警指向 data_service。
//
// 没有 label:失败原因在 ERROR 日志里;**绝不**把 player_id / guild_id 做成 label(AGENTS.md §9)。
//
// 用 go-zero core/metric 而不是裸 prometheus,是跟随本服务既有写法(push.go 的 guildPushTotal、
// data/guild_manage_repo.go 的四个 guild_tx_* / guild_cache_* 计数器)。已核实它在本服务**确实生效**:
// core/metric 的每次 Inc/Add 都要过 prometheus.Enabled() 全局开关,而 guild 的配置内嵌
// zrpc.RpcServerConf、etc/guild.yaml 配了 Prometheus.Host —— zrpc.MustNewServer → ServiceConf.SetUp
// → prometheus.StartAgent 会把开关打开并在 :9220 暴露 /metrics
// (internal/config/config_test.go 的 TestEtcYamlEnablesPrometheus 钉住了这段配置)。
// 这与 data_service 不同:那边从不启 go-zero 的 agent,所以同样的写法在那边一个样本都不产生。
// 代价:Prometheus.Host 留空的环境里本计数器恒为 0(与本服务其它计数器同口径);
// 单测进程里开关同样是关的,所以单测不断言它的数值。
var guildPlayerNameLookupFailedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "guild",
	Name:      "player_name_lookup_failed_total",
	Help:      "向 data_service 批量取展示名失败的次数(按批计;失败时名字留空,不影响 RPC 结果)。",
})

// DataServicePlayerNames 用 data_service.BatchGetPlayerName 实现 PlayerNameResolver。
type DataServicePlayerNames struct {
	client  dspb.DataServiceClient
	timeout time.Duration
}

// NewDataServicePlayerNames。timeout<=0 用 DefaultPlayerNameLookupTimeout。
// client 为 nil 也能构造:BatchResolve 对它恒回空 map(见下),不会 panic。
func NewDataServicePlayerNames(client dspb.DataServiceClient, timeout time.Duration) *DataServicePlayerNames {
	if timeout <= 0 {
		timeout = DefaultPlayerNameLookupTimeout
	}
	return &DataServicePlayerNames{client: client, timeout: timeout}
}

// BatchResolve 见 PlayerNameResolver 的契约。永远返回非 nil 的 map。
//
// nil 接收者也安全:`(*DataServicePlayerNames)(nil)` 装进接口之后 != nil(与 guild.go 里
// mergeFence 躲的是同一个坑),WithPlayerNames 的判空拦不住它,所以这里再兜一层。
func (r *DataServicePlayerNames) BatchResolve(ctx context.Context, playerIDs []uint64) map[uint64]string {
	names := make(map[uint64]string, len(playerIDs))
	if r == nil || r.client == nil {
		return names
	}

	// 去重 + 丢 0(uniqueNonZero 在 push.go,保持首次出现的顺序)。帮主几乎总是同时出现在
	// 成员列表里,不去重会让"100 人的帮"变成 101 个 id;0 表示"没有这个人",查它没有意义。
	ids := uniqueNonZero(playerIDs)
	if len(ids) == 0 {
		return names
	}
	if len(ids) > playerNameBatchLimit {
		logx.Errorf("[guild] player name lookup got %d distinct ids, over the batch limit %d; "+
			"only the first %d are resolved, the rest stay unnamed", len(ids), playerNameBatchLimit, playerNameBatchLimit)
		ids = ids[:playerNameBatchLimit]
	}

	// 请求 ctx 已经结束就不发 RPC:玩家断线,或整请求预算被前面的步骤吃光 —— 典型是 locator Redis 卡住时
	// 排在取名前面的在线 MGET(它没有独立超时,见 toProtoGuild 的注释)。此时发出去也是立刻
	// Canceled / DeadlineExceeded,而这次"失败"与 data_service 无关:只记 Info、**不计**
	// guild_player_name_lookup_failed_total,保住该计数器"向 data_service 取名出了问题"的含义,
	// 不把排障引到错误的依赖上。RPC 进行中父 ctx 才到期的情形分不清是谁慢,仍按取名失败计。
	if err := ctx.Err(); err != nil {
		logx.Infof("[guild] player name lookup skipped (n=%d): request context already done before the RPC: %v", len(ids), err)
		return names
	}

	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.client.BatchGetPlayerName(lookupCtx, &dspb.BatchGetPlayerNameRequest{PlayerIds: ids})
	if err != nil {
		// 只记条数不记 id 列表:一次最多 500 个 id,整串打进日志既冗长又没有排障价值。
		logx.Errorf("[guild] player name lookup failed (n=%d): %v", len(ids), err)
		guildPlayerNameLookupFailedTotal.Inc()
		return names
	}
	// 拷一份而不是直接返回 resp.GetNames():返回值要非 nil(响应里没有任何名字时 GetNames 是 nil map),
	// 且不把 pb 消息内部的 map 交到调用方手里。空串与"没登记"同义,不放进结果。
	for id, name := range resp.GetNames() {
		if name != "" {
			names[id] = name
		}
	}
	return names
}

// ── logic 侧助手 ───────────────────────────────────────────────

// resolveNames 是 l.playerNames 的 nil 安全包装,所有填充点都经它取名。
//
// 未注入 resolver(没配 DataServiceRpc,或单测)时返回 nil map:对 nil map 按键取值
// 得到零值 "",正好是"名字留空"的语义,调用方不必再判空。
func (l *GuildLogic) resolveNames(ctx context.Context, playerIDs []uint64) map[uint64]string {
	if l.playerNames == nil || len(playerIDs) == 0 {
		return nil
	}
	return l.playerNames.BatchResolve(ctx, playerIDs)
}
