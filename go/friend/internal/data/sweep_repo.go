package data

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
)

// sweep_repo.go —— friend_request 终态行的保留期清理,SQL 侧(F2 §3.8)。
// ticker、抖动、指标与告警日志在 internal/logic/sweep.go;本文件只管"数多少、删多少"。
//
// # 为什么只清终态行
//
// accept / reject 只翻 status 不删行,也没有 TTL,所以 friend_request 随社交图的"对数"
// 单调累积。终态行(accepted / rejected)没有业务语义 —— 好友关系的权威在 friend 表,
// 删掉之后再次发起申请等价于一条全新的 pending INSERT。
// **pending 永不在清理范围内**:那是玩家还没处理的东西,清掉等于替玩家做决定。
//
// # 多副本各删各的,不引入 leader
//
// DELETE 幂等,两个副本同时跑最坏只是其中一个删到空批(§3.8)。
// 与在线写路径的锁冲突面也可控:本文件只碰**终态**行,而业务事务只在申请行是 pending 时
// 才继续往下写(AcceptFriend 在 status≠pending 时直接返回),两边要动的行几乎不相交。
// 万一真撞上 1213,一轮失败等下一轮 —— 这正是"后台任务不重试、靠节拍自愈"的用法。

// SweepModeReportOnly / SweepModeDelete 与 config.SweepMode* 的取值**必须逐字一致**。
//
// 这里是刻意的第二份常量:data 包**不能** import config —— config 为了拿 data.DatabaseName
// 已经 import data(F1 §3.1),反向依赖直接成环。谁改一处必须改两处。
// ⚠ 想用测试机械钉住两处一致,断言只能写在**外部测试包**(package data_test)或
// internal/logic 的测试里:包内测试(package data)import config 同样构成 import cycle。
const (
	SweepModeReportOnly = "report_only"
	SweepModeDelete     = "delete"
)

// millisPerDay:保留期配置是"天",updated_ms 是毫秒时间戳,换算只在这里做一次。
const millisPerDay int64 = 24 * 60 * 60 * 1000

// maxRetentionDays 是保留期的合理上限(100 年)。它不是业务约束,是**溢出与误配的护栏**:
// retentionDays 来自 yaml 的 int,写成 1e15 时 `retentionDays * millisPerDay` 会在 int64 上
// 回绕成负数,而负数截止点配合 MySQL 的无符号比较会变成"删掉所有行"(见 SweepTerminalRequests)。
const maxRetentionDays = 36500

// SweepTerminalRequests 跑一轮清理,返回(本轮看到的待清理行数, 实际删除行数)。
//
// 契约(与 logic.SweepStore 的注释逐条对应):
//   - mode 不是 SweepModeReportOnly / SweepModeDelete 之一时(含空串)**一行都不删**,
//     按 report_only 处理。logic 那边会为未知模式单独打一条错误日志,这里不重复打。
//   - report_only:返回 (待清理行数, 0, nil)。
//   - delete:返回 (待清理行数, 实际删除行数, nil);两个数都受 batchLimit 封顶,
//     所以"等于 batchLimit"只说明积压 ≥ 一批,**不是精确积压**。
//   - 保险触发(存在 updated_ms 恒为 0 的终态行)时只统计不删,返回 deleted=0 且 **err=nil** ——
//     它不是故障,是刻意的拒绝;拒绝原因在这里打一条错误日志(logic 的 WARN 会指向它)。
//   - nowMs 由调用方给(单测要固定时钟),本层不取 time.Now。
//   - "终态"逐字等于 `status IN (accepted, rejected)`,**不是** `status <> pending`:
//     `status = 0` 的行不再被扫到。现实中不存在那种行(status 只写 1/2/3),这正是想要的
//     语义收紧 —— 真出现 0 说明有人绕过本层直写库,那种行不该被一个定时任务悄悄删掉。
//     换成 IN 的性能理由见 countTerminalRequestsBefore。
func (r *FriendRepo) SweepTerminalRequests(ctx context.Context, mode string,
	retentionDays, batchLimit int, nowMs int64) (int64, int64, error) {
	// 这三条在生产不可能出现(config.Validate 拒收 ≤0 的 RetentionDays / BatchLimit),
	// 但这里必须 fail-fast 而不是"当默认值":batchLimit=0 会让 DELETE 不带 LIMIT
	// (一条长事务锁住整张 friend_request,把好友申请写路径一起卡住),
	// retentionDays=0 会把刚被拒的申请立刻删掉。
	if batchLimit <= 0 {
		return 0, 0, fmt.Errorf("sweep batchLimit 必须为正数,得到 %d", batchLimit)
	}
	if retentionDays <= 0 || retentionDays > maxRetentionDays {
		return 0, 0, fmt.Errorf("sweep retentionDays 必须在 [1, %d] 内,得到 %d", maxRetentionDays, retentionDays)
	}

	cutoffMs := nowMs - int64(retentionDays)*millisPerDay
	if cutoffMs <= 0 {
		// 只有"进程时钟没设对"才会走到这里(nowMs 小于保留期本身)。
		// **绝不能**把负数截止点传给 SQL:updated_ms 是 bigint unsigned,MySQL 在
		// 无符号列与负值比较时会把负值按无符号解释成一个天文数字,
		// `updated_ms < <天文数字>` 就是"匹配所有行" —— delete 模式下等于清空整张表。
		logx.WithContext(ctx).Errorf("[friend] sweep 截止点非正(nowMs=%d retention_days=%d),本轮不做任何事:检查进程时钟",
			nowMs, retentionDays)
		return 0, 0, nil
	}
	cutoff := uint64(cutoffMs)

	pending, err := r.countTerminalRequestsBefore(ctx, cutoff, batchLimit)
	if err != nil {
		return 0, 0, err
	}
	if pending == 0 || mode != SweepModeDelete {
		return pending, 0, nil
	}

	// ── delete 模式的保险(F2 §3.8 的"待删行数异常就不删",本实现把"异常"定成一个精确判据)──
	//
	// 规格举的例子是"待删行数等于总行数"。这里改用**更精确也更便宜**的判据:
	// 存在终态(status IN (accepted, rejected))且 updated_ms = 0 的行。理由:
	//   - 它直接命中真正的危险来源(F2-14:updated_ms 一度没有任何写入方,全表恒为 0。
	//     拿 0 当"最后变更时刻",每一行都会被判成早已过期,delete 模式一轮就清空整表);
	//   - "等于总行数"要一次**全表** COUNT(*),而这是在线写路径上的表;
	//     而且一张确实全部过期的小表会被误判成异常,永远清不掉。
	// 这是**推断出来的防御**(规格没给具体判据),所以判据与代价都写在这里。
	//
	// 触发后 fail-closed:只统计不删,并打出补救办法。新库由 schemamigrate 从基线建全、
	// 没有历史行,正常情况下这条保险永远不会触发;它要挡的是"某条写路径漏写 updated_ms"
	// 与"从旧共享库 mmorpg 搬数据时没给 updated_ms 赋值"(friend_table.proto 里写了
	// 搬迁时应取 request_time_ms)。
	missing, err := r.countTerminalRequestsMissingUpdatedMs(ctx, batchLimit)
	if err != nil {
		return pending, 0, err
	}
	if missing > 0 {
		logx.WithContext(ctx).Errorf("[friend] sweep(delete)拒绝删除:发现 %d 行终态好友申请的 updated_ms=0,"+
			"说明有写路径没写 updated_ms(F2-14)或旧数据搬迁时漏了这一列。"+
			"在修好写入方之前继续按观察模式运行;存量修法:UPDATE friend_request SET updated_ms=request_time_ms "+
			"WHERE status IN (%d, %d) AND updated_ms=0", missing, requestStatusAccepted, requestStatusRejected)
		return pending, 0, nil
	}

	deleted, err := r.deleteTerminalRequestsBefore(ctx, cutoff, batchLimit)
	if err != nil {
		return pending, 0, err
	}
	return pending, deleted, nil
}

// countTerminalRequestsBefore 数"终态且已过保留期"的行,**最多数 limit 行**。
//
// 为什么套一层派生表:`SELECT COUNT(*) ... LIMIT ?` 的 LIMIT 作用在**结果集**上
// (结果只有一行,LIMIT 什么也没限),扫描量仍是全部匹配行 —— 积压百万行时这条 COUNT
// 自己就是一次事故。把 LIMIT 放进派生表里,引擎取满 limit 行就停,扫描量因此有界。
// 代价是返回值在 limit 处饱和,调用方必须知道"等于 limit"只意味着"≥ limit"。
//
// 索引:条件走 (status, updated_ms)。**必须写成 `status IN (2,3)` 而不是 `status <> 1`**:
// `<>` 会被范围优化器拆成两个开区间(status<1 与 status>1),第一列一旦是范围,
// `updated_ms` 就只能当 ICP 过滤条件、不是索引边界 —— 没有过期行可清的常态下 LIMIT 永远
// 取不满,每轮都会扫完全部终态行(而 friend_request 正在在线写路径上)。
// 写成 IN 之后每个 status 值各是一段等值前缀,`updated_ms < cutoff` 成为真正的区间上界,
// 扫描量恒等于命中行数。两列都在索引里,不必回表。
func (r *FriendRepo) countTerminalRequestsBefore(ctx context.Context, cutoffMs uint64, limit int) (int64, error) {
	var rows int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
		     SELECT 1 FROM friend_request WHERE status IN (?, ?) AND updated_ms < ? LIMIT ?
		 ) AS bounded`,
		requestStatusAccepted, requestStatusRejected, cutoffMs, limit).Scan(&rows); err != nil {
		return 0, fmt.Errorf("count terminal friend requests before %d: %w", cutoffMs, err)
	}
	return rows, nil
}

// countTerminalRequestsMissingUpdatedMs 数"终态但 updated_ms 还是 0"的行(同样有界)。
// 它是上面那道保险的探针;0 就是健康。
func (r *FriendRepo) countTerminalRequestsMissingUpdatedMs(ctx context.Context, limit int) (int64, error) {
	var rows int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
		     SELECT 1 FROM friend_request WHERE status IN (?, ?) AND updated_ms = 0 LIMIT ?
		 ) AS bounded`,
		requestStatusAccepted, requestStatusRejected, limit).Scan(&rows); err != nil {
		return 0, fmt.Errorf("count terminal friend requests missing updated_ms: %w", err)
	}
	return rows, nil
}

// deleteTerminalRequestsBefore 删一批终态过期行,返回实际删除行数。
//
// 单条自动提交语句,不开事务:分批的意义就是让每次持锁窗口足够短,
// 再把几批包进一个事务会把这个意义抹掉。
// 不加 ORDER BY:删哪一批无所谓(迟早都要删),而 ORDER BY 会为这条 DELETE 加一次排序。
// `DELETE ... LIMIT` 在 STATEMENT 格式的 binlog 下是不确定语句(从库可能删到不同的行),
// 本仓 MySQL 显式配了 binlog_format=ROW(见 friend_repo.go 顶部核对到的
// deploy/k8s/manifests/infra/mysql.yaml:60),ROW 格式记录的是具体行,所以安全。
func (r *FriendRepo) deleteTerminalRequestsBefore(ctx context.Context, cutoffMs uint64, limit int) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM friend_request WHERE status IN (?, ?) AND updated_ms < ? LIMIT ?",
		requestStatusAccepted, requestStatusRejected, cutoffMs, limit)
	if err != nil {
		return 0, fmt.Errorf("delete terminal friend requests before %d: %w", cutoffMs, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete terminal friend requests: rows affected: %w", err)
	}
	return deleted, nil
}
