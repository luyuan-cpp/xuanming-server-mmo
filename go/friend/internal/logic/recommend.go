package logic

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"

	"friend/internal/constants"
	"friend/internal/data"
	pb "proto/friend"
)

// 好友推荐的策略链与限幅(规格 §3.7;SQL 侧在 internal/data/recommend_repo.go)。
//
// 推荐是**可降级的展示功能**,这决定了本文件所有的取舍:
//   - 越界的 limit 钳到上限而不是报错(一个写错的 limit 不该让整个面板打不开);
//   - 在线状态取不到就当全部离线,候选照样返回;
//   - 候选允许轻微陈旧(见 recommend_repo.go 顶部第 1 条),不做一致性承诺。
// 唯一 fail-closed 的输入是 exclude 条数:它是客户端可控的数组长度,直接决定 SQL 的占位符
// 个数与集合运算量,越界必须拒(ErrInvalidParameter),不能"帮它截断"——截断会让客户端
// 以为自己排除了某些人,结果那些人又被推荐回来,行为比报错更难查。

// recommendLimit 把客户端请求的 limit 收敛到配置区间。
//
// 参数取三个裸 uint32(而不是整个 FriendConf):这条规则是纯函数,单测不必装配配置结构。
//
// 规则:0 表示"按服务端默认"(proto 里就是这么约定的,客户端不填即 0);
// 超过 maxLimit 钳到 maxLimit。
// 最后那个 `if maxLimit > 0 && limit > maxLimit` 里的 maxLimit>0 不是多余:
// config.Validate 已拒绝阈值为 0,但本函数也被单测直接调用,给 0 时应表现为"不钳"而不是
// 把 limit 钳成 0(悄悄返回空候选是最难排查的一种行为)。
func recommendLimit(requested, defaultLimit, maxLimit uint32) uint32 {
	limit := requested
	if limit == 0 {
		limit = defaultLimit
	}
	if maxLimit > 0 && limit > maxLimit {
		limit = maxLimit
	}
	return limit
}

// RecommendFriends 推荐可加的好友。
//
// 策略链固定两级:mutual(好友的好友,按共同好友数降序)→ random(随机锚点兜底),
// 凑够 limit 即止;每选中一批就追加进 exclude,后一级不会重复前一级已选中的人。
//
// 刻意**不**移植 A 仓的 RecommendStrategy 接口 + buildStrategies(按配置名装配策略链):
// 那套抽象的存在理由是 A 仓有 `recommend_strategies` 配置项可以改顺序 / 加策略,
// 本仓 FriendConf 没有这个轴,只有两个固定级别。为一个不存在的变化点引入接口 +
// 注册表属 AGENTS §11.2 的 YAGNI —— 真要加第三种策略时,在下面的 steps 里加一项即可。
//
// 服务端对推荐**不留任何状态**:"换一批"靠客户端把已看过的 id 回传在 exclude_player_ids 里。
// 所以同一个请求重放会得到不同结果(mutual 同分随机、random 锚点随机),这是设计而非缺陷。
func (l *FriendLogic) RecommendFriends(ctx context.Context, req *pb.RecommendFriendsRequest) (*pb.RecommendFriendsResponse, error) {
	// 整请求预算(F2-13):本方法最多打三次 MySQL(mutual、random 的区间查询与锚点扫)
	// 再加一次 Redis MGET,串起来必须在 zrpc Timeout 之前结束 —— 否则 go-zero 的超时拦截器
	// 会先回 DeadlineExceeded,下面这些 in-band 的 TipInfoMessage 全部被丢掉。
	// 预算与其余 9 个 handler 同源,非正预算的处理见 withRequestBudget。
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()

	reject := func(code uint32, msg string) (*pb.RecommendFriendsResponse, error) {
		// in-band 返回(F2-1):(resp, nil) 而不是 (nil, err)。后者会被路由服翻成信封级错误,
		// 客户端拿不到 TipInfoMessage,只能看到一个没有文案的失败。
		return &pb.RecommendFriendsResponse{ErrorMessage: tipErr(code, msg)}, nil
	}

	me, ok := l.requireCaller(ctx, "RecommendFriends")
	if !ok {
		return reject(constants.ErrInvalidParameter, "missing session identity")
	}

	cfg := l.deps.SvcCtx.Config.Friend
	// 与 int 比较而不是把 len 转成 uint32:转换本身在理论上会截断(64 位 int → uint32),
	// 而阈值一侧转成 int 永远安全。
	if len(req.GetExcludePlayerIds()) > int(cfg.RecommendMaxExclude) {
		return reject(constants.ErrInvalidParameter, "too many exclude_player_ids")
	}
	limit := recommendLimit(req.GetLimit(), cfg.RecommendDefaultLimit, cfg.RecommendMaxLimit)
	if limit == 0 {
		// 只有"两个阈值都被配成 0"才会走到这里(config.Validate 会先拒掉)。
		// 回空候选而不是报错:这是配置问题,不是玩家或存储的问题。
		logx.WithContext(ctx).Errorf("[friend] RecommendFriends 有效 limit 为 0(RecommendDefaultLimit=%d RecommendMaxLimit=%d):配置漏配,推荐恒空",
			cfg.RecommendDefaultLimit, cfg.RecommendMaxLimit)
		return &pb.RecommendFriendsResponse{}, nil
	}

	// 排除集 = 客户端回传的已看过 id + 自己 + 本次已选中的候选。
	// 自己也进排除集,尽管两条 SQL 里都已有 `player_id <> ?` 的自排除 —— 两处都写是刻意的:
	// 将来加第三种策略时,忘写自排除只会让它命中排除集,不会把玩家推荐给自己。
	// 容量一次算足,避免 append 在循环里反复扩容。
	exclude := make([]uint64, 0, len(req.GetExcludePlayerIds())+1+int(limit))
	exclude = append(exclude, req.GetExcludePlayerIds()...)
	exclude = append(exclude, me)

	// want 是"还缺几个",不是 limit:第二级只需要补齐缺口,多查出来的行会被丢掉却照样花开销。
	steps := []struct {
		name  string
		fetch func(ctx context.Context, exclude []uint64, want uint32) ([]data.RecommendCandidate, error)
	}{
		{
			name: "mutual",
			fetch: func(ctx context.Context, exclude []uint64, want uint32) ([]data.RecommendCandidate, error) {
				return l.deps.Repo.RecommendByMutual(ctx, me, exclude, want)
			},
		},
		{
			name: "random",
			fetch: func(ctx context.Context, exclude []uint64, want uint32) ([]data.RecommendCandidate, error) {
				return l.deps.Repo.RecommendRandom(ctx, me, exclude, want)
			},
		},
	}

	candidates := make([]data.RecommendCandidate, 0, limit)
	for _, step := range steps {
		if uint32(len(candidates)) >= limit {
			break
		}
		picked, err := step.fetch(ctx, exclude, limit-uint32(len(candidates)))
		if err != nil {
			// 存储故障 in-band 化(F2-1):回 ErrStorage(本域唯一的 fault 码)并打错误日志,
			// 由 serverbase 记成 rpc_inband_fault。**不做部分降级**:第一级失败时不带着
			// 空结果继续跑第二级 —— MySQL 出问题时第二级几乎必然同样失败,继续只是把一次
			// 故障变成两次超时,还会把真实原因(第一条查询的报错)藏在第二条的报错后面。
			logx.WithContext(ctx).Errorf("[friend] RecommendFriends 策略 %s 查询失败 player=%d: %v", step.name, me, err)
			return reject(constants.ErrStorage, "recommend candidates unavailable")
		}
		for _, c := range picked {
			candidates = append(candidates, c)
			// 追加进 exclude,后一级策略不会再把同一个人推一遍。
			exclude = append(exclude, c.CandidatePlayerID)
		}
	}

	entries := make([]*pb.RecommendEntry, 0, len(candidates))
	for _, c := range candidates {
		entries = append(entries, &pb.RecommendEntry{
			CandidatePlayerId: c.CandidatePlayerID,
			MutualFriends:     c.MutualFriends,
		})
	}
	l.fillRecommendOnline(ctx, entries)
	return &pb.RecommendFriendsResponse{Candidates: entries}, nil
}

// fillRecommendOnline 给候选补 is_online / last_active_ms。
//
// 语义照规格 §3.9:在线态的唯一事实源是共享库的契约 key `player:session:{id}`
// (MGET → PlayerSession → State == SESSION_STATE_ONLINE 判在线,last_active_ts 填
// last_active_ms),由 internal/data/session_reader.go 统一读,本文件不自己碰 Redis ——
// friend 私有缓存与跨运行时契约 key 必须用不同的句柄(契约 §4)。
//
// 在线态是**展示字段**:句柄缺失或读失败时全部按离线返回,推荐照常可用
// (pb 的零值就是 is_online=false / last_active_ms=0,与"离线且无记录"一致)。
// 降级不打日志也不计指标 —— 那两件事属 session_reader 自己(ObserveOnlineLookup)。
//
// ⚠ **本函数是本文件与 internal/data/session_reader.go 的唯一接触点**;
// 签名已在 F2 裁决中与 data/session_reader.go 对齐(SessionStore.FillOnlineStatus)。
// 分批(按 ListReadHardLimit 切 MGET)也在 session_reader 里做,不在这里 ——
// 推荐一次最多 RecommendMaxLimit(≤20)个 id,本路径永远不会触发分批。
func (l *FriendLogic) fillRecommendOnline(ctx context.Context, entries []*pb.RecommendEntry) {
	if len(entries) == 0 || l.deps.Sessions == nil {
		return
	}
	ids := make([]uint64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.CandidatePlayerId)
	}
	statuses := l.deps.Sessions.FillOnlineStatus(ctx, ids)
	for _, e := range entries {
		status, ok := statuses[e.CandidatePlayerId]
		if !ok {
			// 没有会话记录 = 离线。零值已经对,不必显式写回。
			continue
		}
		e.IsOnline = status.Online
		e.LastActiveMs = status.LastActiveMs
	}
}
