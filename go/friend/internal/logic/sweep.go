// sweep.go —— friend_request 终态行的周期清理(规格 §3.8 的 **ticker 侧**;SQL 侧在
// internal/data/sweep_repo.go)。
//
// # 为什么需要它
//
// friend_request 每对 (from,to) 至多一行,accept / reject 只翻 status 不删行,也没有 TTL。
// 终态行没有任何业务语义(好友关系的权威在 friend 表),但会随社交图的"对数"单调累积;
// 超过保留期删掉后再次发起申请 = 一条全新的 pending INSERT,行为等价。
// **pending 永不清** —— 那是玩家还没处理的东西,清掉就是替玩家做决定。
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
// 真实的一轮只有一两条带 LIMIT 的语句,30s 是宽松的兜底而不是预期耗时。
const sweepRoundBudget = 30 * time.Second

// SweepStore 是清理需要的 SQL 能力(实现在 data/sweep_repo.go)。
//
// 一个方法同时吃 mode:模式决定"只 COUNT"还是"COUNT 后真删",而"能不能删"的判据
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
}

// StartSweep 起后台清理循环,立即返回;ctx 结束时循环自行退出。
//
// 首轮不是立刻跑,而是先等一个 [0, Interval) 的随机抖动再进入 ticker 节拍:
// 多副本同时重启(滚动升级、K8s 驱逐)时不要在同一秒一起对 friend_request 发 DELETE。
func StartSweep(ctx context.Context, deps *Deps) {
	cfg := deps.SvcCtx.Config.Friend.Sweep
	if deps.Sweeps == nil {
		logx.Errorf("[friend] sweep 未装配 SweepStore,清理不启动(终态好友申请不会被回收)")
		return
	}
	// Interval <= 0 在生产不可能出现(config.Validate 拒收),这里不启动而不是用 0 间隔 ——
	// 0 间隔的 ticker 会 panic,safego.Loop 也会拒绝启动,拒绝得越早越好排查。
	if cfg.Interval <= 0 {
		logx.Errorf("[friend] sweep Interval=%v 非法,清理不启动", cfg.Interval)
		return
	}

	logx.Infof("[friend] sweep 启动 mode=%s interval=%v retention_days=%d batch=%d",
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

// runSweepRound 跑一轮清理。
//
// 失败只打日志:清理是后台任务,一轮失败等下一轮,不影响任何在线请求。
func (d *Deps) runSweepRound(ctx context.Context) {
	cfg := d.SvcCtx.Config.Friend.Sweep
	ctx, cancel := context.WithTimeout(ctx, sweepRoundTimeout(cfg.Interval))
	defer cancel()

	pending, deleted, err := d.Sweeps.SweepTerminalRequests(
		ctx, cfg.Mode, cfg.RetentionDays, cfg.BatchLimit, d.now().UnixMilli())
	if err != nil {
		logx.WithContext(ctx).Errorf("[friend] sweep 执行失败 mode=%s: %v", cfg.Mode, err)
		return
	}

	// 每轮都刷 Gauge(包括 0):长期不更新是"sweep 根本没在跑"的唯一信号,
	// 只在 >0 时刷会让"没有积压"和"循环死了"在看板上长得一样。
	// ⚠ pending 受 BatchLimit 封顶,等于 BatchLimit 只说明"积压 ≥ 一批",不是精确积压。
	// ⚠ 未知模式**不刷** Gauge —— label 必须是 metrics 包声明的有限枚举(metrics.go 顶部),
	// 拿配置里拼错的串当 label 会造出一条没人认识的序列;拼错这件事由 default 分支的错误日志表达。
	// 所以 Set 放在两个合法 case 的第一行,而不是 switch 之前。
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
	default:
		// 配置里拼错了模式。config.Validate 本该拒掉(它按 options 标签校验),
		// 走到这里说明校验被绕过或配置在运行期被改过 —— 必须报出来,
		// 因为 SQL 侧对未知模式的处理是"一行不删",表现和 report_only 一样,
		// 静默下去就会以为清理在跑。
		logx.WithContext(ctx).Errorf("[friend] sweep 模式 %q 不是 %q / %q,本轮未清理任何行",
			cfg.Mode, config.SweepModeReportOnly, config.SweepModeDelete)
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
