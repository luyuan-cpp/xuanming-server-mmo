// sweep.go —— friend_request 终态行与 friend_capacity 零好友行的周期清理
// (规格 §3.8 的 **ticker 侧**;SQL 侧在 internal/data/sweep_repo.go)。
//
// # 为什么需要它
//
// friend_request 每对 (from,to) 至多一行,accept / reject 只翻 status 不删行,也没有 TTL。
// 终态行没有任何业务语义(好友关系的权威在 friend 表),但会随社交图的"对数"单调累积;
// 超过保留期删掉后再次发起申请 = 一条全新的 pending INSERT,行为等价。
// **pending 永不清** —— 那是玩家还没处理的东西,清掉就是替玩家做决定。
//
// # 也回收 friend_capacity 的零好友行
//
// 为什么需要:AddFriendRequest 为了拿容量守卫,会为**任意** to_player_id 建一行 friend_capacity ——
// friend 没有玩家名册,验证不了 target 是否真实存在;Block → Unblock 反复换目标同理(Unblock 只删
// friend_block,不碰容量行)。这张表没有 TTL,于是随"被发起过申请 / 被拉黑过的 id 个数"单调增长,
// 且增长可由客户端驱动;唯一的闸是每分钟频率配额,而配额在 Redis 故障时按设计 fail-open
// (见 rate_quota.go 文件头)。详见 docs/design/friend-handoff-20260920.md §3 第 1 条。
//
// 为什么删了无害:只回收零好友(friend_count = 0)、且距建行或最近一次减计数已超过保留期的行
// (created_ms 记的是"本行被(重新)建出、或最近一次 friend_count 减少的时刻",不只是建行时刻 ——
// 减计数为什么也要刷新它,见 data/friend_repo.go 的 deleteFriendEdges);之后该玩家再进任何写路径,
// ensureFriendCapacityRows 会按 friend 表的权威边数重新建行,**绝不猜 0**(handoff §5.6 不变量 (b)),
// 硬上限不会因此放宽。回收与写路径的竞态(ensure 之后、取守卫之前行被删)由 data 层的
// runGuardedWrite 重新 ensure 并有上限地重试吸收(至多重试两次,共 capacityGuardMaxAttempts 遍;
// 为什么恰好够见 data/friend_repo.go 顶部锁序说明 (5)),不会把正常请求打成 fault。
// 与终态申请行的差别:created_ms = 0 的存量行**可以**直接回收,所以这一段没有
// "updated_ms 恒为 0 就拒删"那道保险。
//
// 两段共用同一份 Friend.Sweep 参数(Mode / RetentionDays / BatchLimit),不新增配置键。
//
// # 多副本:各跑各的,不引入 leader
//
// DELETE 幂等,两个副本同时跑最坏只是其中一个删到空批。为此专门引入 shared/leader
// 会把一个"删旧数据"的后台任务变成有分布式状态的东西(AGENTS.md §11.2 YAGNI)。
// 启动时加随机抖动,只是为了让多副本不要恰好同一秒一起压库。
//
// # 职责分界(看懂这条再改)
//
// 本文件只负责:节拍(ticker + 抖动)、panic 边界(safego)、单轮预算、指标与日志。
// **模式判定、LIMIT、以及"待删行数异常就只打日志不删"那道保险都在 SQL 侧**
// (data 层同时拿得到"待清理行数"与"有没有 updated_ms 恒为 0 的终态行",判据只在那里完整)。
// 这里按返回的两个数字做可观测性:delete 模式下"有待清理行却一行没删"多半就是保险被触发。
//
// # 调用方
//
// go/friend/friend.go 的 runFriend,在 `metrics.Start(c.MetricsListenAddr)` 之后一行:
//
//	logic.StartSweep(ctx, deps)
//
// 那个 ctx 就是 main 里 signal.NotifyContext 的 ctx:进程收到 SIGTERM 时循环自行退出,
// 不需要额外的停机钩子(ticker 在 safego.Loop 的 defer 里 Stop)。
// 停机**不等它退出**:sweep 是幂等的批量 DELETE,被打断只是少删一批、下一轮接着删,
// 为它加停机等待反而会挤占 24s 硬预算(契约 §9.1)。
//
// ⚠ 默认 `Sweep.Mode = report_only`,接上之后**不会删任何数据**,只统计 + 打 WARN + 更新
// gauge。要真删必须显式把配置改成 delete,而改之前必须先确认 updated_ms 有写入方
// (F2 已补齐四个写路径:AddFriend 的 upsert、AcceptFriend 的正反向 UPDATE、RejectFriend 的
// CAS、Block 取消双向 pending);否则全表 updated_ms 恒为 0,每一行都会被判成已过期。
package logic

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"friend/internal/config"
	"friend/internal/metrics"
	"shared/safego"
)

// sweepPoint 是 safego 的点位名,会直接变成 Prometheus label safego_panic_total{point=...}。
// 必须是编译期常量(不许拼进任何运行期值),改它等于改告警规则认的字符串。
const sweepPoint = "friend.request_sweep"

// sweepRoundBudget 是单轮清理的上限。一轮跑不完就等下一轮:
// 没有上限时一条慢 DELETE 会把连接占到下一轮 ticker 触发,两轮叠在一起压库。
// 真实的一轮是终态申请的一两条带 LIMIT 的语句,加上容量行回收的一条候选读与至多 BatchLimit 条
// 按主键的单行 DELETE(逐行自动提交,理由见 data/sweep_repo.go);30s 是两段**共用**的宽松兜底,
// 不是预期耗时。预算到期时没删完的行留给下一轮,不丢任何东西。
const sweepRoundBudget = 30 * time.Second

// SweepStore 是清理需要的 SQL 能力(实现在 data/sweep_repo.go)。
//
// 每个方法都同时吃 mode:模式决定"只 COUNT"还是"COUNT 后真删",而"能不能删"的判据
// (updated_ms 是否已被写入方填上)只有 SQL 侧看得见,拆成多次调用会让判定与动作之间
// 出现窗口,也会把同一个决定分给两层做。
//
// 契约:
//   - mode 不是 report_only / delete 两个值之一时(含空串、大小写不符),**一行都不许删**;
//   - report_only 返回 (待清理行数, 0, nil);
//   - delete 返回 (待清理行数, 实际删除行数, nil),两个数都受 batchLimit 封顶;
//   - 待删行数异常(存在 updated_ms 恒为 0 的终态行 ⇒ 写入方漏写,见 F2-14)时
//     **只统计不删**,返回 deleted=0 且不报错 —— 它不是故障,是刻意的拒绝;
//   - nowMs 由调用方给(单测要固定时钟),data 层不自己取 time.Now。
type SweepStore interface {
	SweepTerminalRequests(ctx context.Context, mode string, retentionDays, batchLimit int, nowMs int64) (pending int64, deleted int64, err error)
	// SweepIdleCapacityRows:回收零好友且超过保留期的 friend_capacity 行,契约同上(无 updated_ms 保险)。
	SweepIdleCapacityRows(ctx context.Context, mode string, retentionDays, batchLimit int, nowMs int64) (idle int64, deleted int64, err error)
}

// StartSweep 起后台清理循环,立即返回;ctx 结束时循环自行退出。
//
// 首轮不是立刻跑,而是先等一个 [0, Interval) 的随机抖动再进入 ticker 节拍:
// 多副本同时重启(滚动升级、K8s 驱逐)时不要在同一秒一起对 friend_request 发 DELETE。
func StartSweep(ctx context.Context, deps *Deps) {
	cfg := deps.SvcCtx.Config.Friend.Sweep
	if deps.Sweeps == nil {
		logx.Errorf("[friend] sweep 未装配 SweepStore,清理不启动(终态好友申请与零好友容量行都不会被回收)")
		return
	}
	// Interval <= 0 在生产不可能出现(config.Validate 拒收),这里不启动而不是用 0 间隔 ——
	// 0 间隔的 ticker 会 panic,safego.Loop 也会拒绝启动,拒绝得越早越好排查。
	if cfg.Interval <= 0 {
		logx.Errorf("[friend] sweep Interval=%v 非法,清理不启动", cfg.Interval)
		return
	}

	logx.Infof("[friend] sweep 启动 mode=%s interval=%v retention_days=%d batch=%d(清理对象:friend_request 终态行 + friend_capacity 零好友行,共用这一份参数)",
		cfg.Mode, cfg.Interval, cfg.RetentionDays, cfg.BatchLimit)

	safego.Go(sweepPoint+".start", func() {
		jitter := time.Duration(rand.Int64N(int64(cfg.Interval)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter):
		}
		// safego.Loop 的 recover 作用域精确到"一轮":某一轮 panic 只丢那一轮,
		// 节拍继续。裸 `go func(){ for { … } }()` 做不到这件事 —— 一轮炸掉整个循环
		// 就静默停摆,而外面没人知道。
		safego.Loop(ctx, sweepPoint, cfg.Interval, func(ctx context.Context) {
			deps.runSweepRound(ctx)
		})
	})
}

// runSweepRound 跑一轮清理:先终态好友申请,再零好友容量行,两段共用同一个单轮预算。
//
// 失败只打日志:清理是后台任务,一轮失败等下一轮,不影响任何在线请求。
//
// **两段互不影响**:前一段失败(含超时)也照样跑后一段,所以两段都不向这里返回 error ——
// 两张表的回收没有先后依赖,让 friend_request 上的一条慢语句连带停掉 friend_capacity 的回收,
// 等于把后者的增长闸门挂在前者的健康上。预算被前一段耗尽时,后一段会立刻因 ctx 到期失败并
// 打一条失败日志,这是刻意接受的噪声:它如实说明"这一轮容量行没回收",比静默跳过好排查。
func (d *Deps) runSweepRound(ctx context.Context) {
	cfg := d.SvcCtx.Config.Friend.Sweep
	ctx, cancel := context.WithTimeout(ctx, sweepRoundTimeout(cfg.Interval))
	defer cancel()

	// 配置里拼错了模式。config.Validate 本该拒掉(它按 options 标签校验),
	// 走到这里说明校验被绕过或配置在运行期被改过 —— 必须报出来,
	// 因为 SQL 侧对未知模式的处理是"一行不删",表现和 report_only 一样,
	// 静默下去就会以为清理在跑。
	// 在这里统一打**一次**,而不是两段各打一遍:同一个配置错误一轮两条一模一样的日志,
	// 只会让人以为是两个问题。两段照常调用 SQL 侧(它对未知模式只数不删),行为与引入第二段之前一致。
	if !isKnownSweepMode(cfg.Mode) {
		logx.WithContext(ctx).Errorf("[friend] sweep 模式 %q 不是 %q / %q,本轮未清理任何行",
			cfg.Mode, config.SweepModeReportOnly, config.SweepModeDelete)
	}

	d.sweepTerminalRequests(ctx, cfg)
	d.sweepIdleCapacityRows(ctx, cfg)
}

// isKnownSweepMode 报告 mode 是不是两个合法取值之一。
// Gauge 的 mode label 必须是 metrics 包声明的有限枚举(metrics.go 顶部),所以"刷不刷 Gauge"
// 与"报不报未知模式"是同一个判据,收在这一处,别在两段里各写一份。
func isKnownSweepMode(mode string) bool {
	return mode == config.SweepModeReportOnly || mode == config.SweepModeDelete
}

// sweepTerminalRequests 是一轮里的第一段:friend_request 的终态行。
func (d *Deps) sweepTerminalRequests(ctx context.Context, cfg config.SweepConf) {
	pending, deleted, err := d.Sweeps.SweepTerminalRequests(
		ctx, cfg.Mode, cfg.RetentionDays, cfg.BatchLimit, d.now().UnixMilli())
	if err != nil {
		// data 层逐行按主键删,出错时已删的行是真删了:行数要进日志(与容量行回收那一支同口径)。
		logx.WithContext(ctx).Errorf("[friend] sweep 清理终态申请失败 mode=%s(中止前看到 %d 行、已删 %d 行): %v",
			cfg.Mode, pending, deleted, err)
		return
	}

	// 每轮都刷 Gauge(包括 0):长期不更新是"sweep 根本没在跑"的唯一信号,
	// 只在 >0 时刷会让"没有积压"和"循环死了"在看板上长得一样。
	// ⚠ pending 受 BatchLimit 封顶,等于 BatchLimit 只说明"积压 ≥ 一批",不是精确积压。
	// ⚠ 未知模式**不刷** Gauge —— label 必须是 metrics 包声明的有限枚举(metrics.go 顶部),
	// 拿配置里拼错的串当 label 会造出一条没人认识的序列;拼错这件事由 runSweepRound 的错误日志表达。
	// 所以 Set 放在两个合法 case 的第一行,而不是 switch 之前;switch 刻意没有 default。
	switch cfg.Mode {
	case config.SweepModeDelete:
		metrics.SetSweepPendingRows(cfg.Mode, float64(pending))
		if deleted > 0 {
			logx.WithContext(ctx).Infof("[friend] sweep 删除 %d 行终态好友申请(retention_days=%d,batch=%d)",
				deleted, cfg.RetentionDays, cfg.BatchLimit)
			return
		}
		if pending > 0 {
			// delete 模式、有待清理行、却一行没删:最可能是 SQL 侧那道保险触发了
			// (发现终态行的 updated_ms 恒为 0 ⇒ 某条写路径漏写 updated_ms,F2-14),
			// 也可能是并发的另一个副本刚把这一批删掉了。两种都要看得见:
			// 前者是必须修的缺陷,后者是无害的空批,分辨靠 data 层那条拒绝日志。
			logx.WithContext(ctx).Errorf("[friend] WARN sweep(delete)有 %d 行待清理却一行未删:检查 data 层是否触发了「待删行数异常」保险(updated_ms 写入方缺失),或只是被另一个副本抢先删了",
				pending)
		}
	case config.SweepModeReportOnly:
		metrics.SetSweepPendingRows(cfg.Mode, float64(pending))
		// 默认模式:只观察,不动数据。go-zero 的 logx 没有 Warn 级,
		// 按仓内先例用 Errorf 打 "WARN " 前缀。
		if pending > 0 {
			logx.WithContext(ctx).Errorf("[friend] WARN sweep(report_only)发现 %d 行终态好友申请已过保留期(retention_days=%d,统计上限 %d);切 delete 前先确认这个数字合理",
				pending, cfg.RetentionDays, cfg.BatchLimit)
		}
	}
}

// sweepIdleCapacityRows 是一轮里的第二段:friend_capacity 的零好友行(为什么要回收见文件头)。
//
// 与第一段的差别只有一处:delete 模式下"看到了却一行没删"**不告警**。这张表没有
// "updated_ms 恒为 0 就拒删"那道保险,零删除只可能是提交点复核生效了 —— 候选读与逐行 DELETE
// 之间该玩家刚加上好友(friend_count 不再是 0),或另一个副本抢先删了这一批。两种都是预期内的
// 竞态而不是缺陷,为它打 WARN 只会制造常亮的噪音。
func (d *Deps) sweepIdleCapacityRows(ctx context.Context, cfg config.SweepConf) {
	idle, deleted, err := d.Sweeps.SweepIdleCapacityRows(
		ctx, cfg.Mode, cfg.RetentionDays, cfg.BatchLimit, d.now().UnixMilli())
	if err != nil {
		// data 层出错时仍返回已删行数(逐行独立提交),这里必须带进日志,否则已删的行无处可查。
		// Gauge 在失败分支**不刷**:候选读失败时 idle = 0,刷了会把"失败"伪装成"无积压"。
		logx.WithContext(ctx).Errorf("[friend] sweep 回收零好友容量行失败 mode=%s(中止前看到 %d 行、已删 %d 行): %v",
			cfg.Mode, idle, deleted, err)
		return
	}

	// Gauge 的刷新纪律与第一段逐条相同:合法模式每轮都刷(含 0),未知模式不刷,所以没有 default。
	// ⚠ idle 同样受 BatchLimit 封顶。
	switch cfg.Mode {
	case config.SweepModeDelete:
		metrics.SetSweepIdleCapacityRows(cfg.Mode, float64(idle))
		if deleted > 0 {
			logx.WithContext(ctx).Infof("[friend] sweep 回收 %d 行零好友容量行(本轮看到 %d 行,retention_days=%d,batch=%d)",
				deleted, idle, cfg.RetentionDays, cfg.BatchLimit)
		}
	case config.SweepModeReportOnly:
		metrics.SetSweepIdleCapacityRows(cfg.Mode, float64(idle))
		if idle > 0 {
			logx.WithContext(ctx).Errorf("[friend] WARN sweep(report_only)发现 %d 行零好友容量行已过保留期(retention_days=%d,统计上限 %d);这个数字持续上涨说明有人在对大量不同目标发申请 / 反复拉黑换目标",
				idle, cfg.RetentionDays, cfg.BatchLimit)
		}
	}
}

// sweepRoundTimeout 取单轮预算:不超过 sweepRoundBudget,也不超过一个 ticker 间隔
// (跑得比节拍还慢就会两轮叠在一起)。
func sweepRoundTimeout(interval time.Duration) time.Duration {
	if interval > 0 && interval < sweepRoundBudget {
		return interval
	}
	return sweepRoundBudget
}
