package data

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
)

// sweep_repo.go —— friend_request 终态行的保留期清理(F2 §3.8)与 friend_capacity 零好友行的回收,SQL 侧。
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
//
// # 第二类清理对象:零好友的 friend_capacity 行(SweepIdleCapacityRows)
//
// AddFriend 为了拿容量守卫会给**任意** target 建一行 friend_capacity(friend 服务没有玩家名册,
// 无法验证 target 是否存在),所以该表随"被申请过的 id 个数"单调增长,且增长可由客户端驱动。
// 回收对象是 `friend_count = 0 AND created_ms < 截止点` 的行,走 (friend_count, created_ms) 复合索引;
// 保留期、批大小、模式与终态申请清理共用同一份 Friend.Sweep 配置,不新增配置键。
// ⚠ "走索引、扫描量恒等于命中行数"的前提是该索引**真实存在**:go/schemamigrate 对已存在的表只
// ADD COLUMN,缺普通索引只出一条 warning"缺索引(不会自动建)"、退出码仍是 0(见 plan.go 的 drift)。
// 本批之前已经建过 friend_capacity 的环境,重跑 -migrate 只会补 created_ms 列,候选读会静默退化成
// 每轮全表扫(report_only 下也会执行)。补救:手工执行
// `ALTER TABLE friend_capacity ADD INDEX idx_friend_capacity_0 (friend_count, created_ms)`
// (索引名以 -migrate 的 warning 原文为准);friend 未上线、无存量,也可以直接删库重建。
//
// **为什么删行无害**:ensureFriendCapacityRows 补行时从 `SELECT COUNT(*) FROM friend WHERE player_id = ?`
// 重算初值、绝不猜 0(TestMissingCapacityRowUsesAuthoritativeFriendCount 钉着),而且这里只删
// friend_count = 0 的行 —— 删掉之后再被 ensure 建回来,得到的仍是同一个权威值。
// 上面这句只论证了"不会偏小"。**偏大**要另外挡:ensure 的 COUNT 与 INSERT 不原子,交错
// "ensure 读到 COUNT=1 → RemoveFriend 提交(行变成 friend_count=0)→ 回收删行 → 陈旧的
// 补行 INSERT (P, 1) 落地"会留下一行 friend_count=1 而真实边数为 0 的计数,此后减不到它、
// 它也永不再进回收 —— 上限永久少 1、零报错。挡法在写路径一侧:deleteFriendEdges 减计数时同时把
// created_ms 刷成当前时刻,所以 created_ms 的含义是"本行被(重新)建出、或最近一次 friend_count
// 减少的时刻",刚减到 0 的行要再过一个完整保留期才会出现在这里的候选里。
// 残留:陈旧窗口(COUNT 与 INSERT 之间)跨过整个 RetentionDays 才可能复现,视为不可达。
//
// **为什么逐行删,不许改成一条批量 DELETE**(防 ABBA 的关键):
//  1. 候选是**普通读**(不加锁):`SELECT player_id FROM friend_capacity WHERE friend_count = 0 AND created_ms < ? LIMIT ?`
//  2. 逐行、**自动提交**、按主键删:`DELETE FROM friend_capacity WHERE player_id = ? AND friend_count = 0 AND created_ms < ?`
//
// 写成 `DELETE ... WHERE friend_count = 0 AND created_ms < ? LIMIT ?` 不行:那条语句在**一个**语句事务里
// 按**二级索引序**锁住多行守卫行,而所有业务写事务按 **player_id 升序**锁两行守卫行 —— 两种取锁顺序
// 不同,回收与业务写之间可以成环(1213)。逐行自动提交时,回收任一时刻**至多持有一把守卫锁、
// 且持锁时不再等别的锁**,不可能处在等待环里。
// DELETE 里重复写 `friend_count = 0 AND created_ms < ?` 是提交点复核:候选读与删除之间该行可能已被
// AcceptFriend 加过好友 —— RC 下 DELETE 拿到行锁后按最新已提交版本重判 WHERE,不匹配就不删。
// (这两条语句不开事务,走的是连接默认隔离级别而不是写路径的 RC;复核照样成立 —— InnoDB 的 DELETE
// 在任何隔离级别下都是**当前读**。候选行刚被别人删掉时,RR 下主键等值未命中会留一把间隙锁,
// 但它只活到这条自动提交语句结束,同样不构成"持有并等待"。)
//
// **为什么不需要 updated_ms 那种"发现 0 就拒删"的保险**:created_ms = 0(从旧共享库搬来的存量行)
// 恰恰是安全的 —— 零好友的存量行被立刻回收没有任何语义损失(理由同"为什么删行无害")。
//
// **与写路径的配合**:回收可能插在写路径的"事务外 ensure"与"事务内取守卫"之间,或在守卫的
// FOR UPDATE 正等着这行时把它删掉,此时 lockCapacityRows 会缺行。那是预期内的竞态:
// runGuardedWrite(friend_repo.go)认 errCapacityRowsMissing,回到事务外重新 ensure 并重试,至多重试两次
// (一次写至多两行守卫行、每行至多被回收一次,所以 3 遍之内必过);每一遍都缺行才 fail-closed。
// 另一处配合:本文件的 DELETE 会留下 delete-marked 的主键记录,并发 ensure 同一玩家时可能在它上面
// 撞 1213 —— 由 ensureFriendCapacityRows 自己有上限地重试。两者详见 friend_repo.go 顶部锁序说明 (5)。

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
// 回绕成负数,而负数截止点配合 MySQL 的无符号比较会变成"删掉所有行"(见 sweepCutoffMs)。
const maxRetentionDays = 36500

// sweepCutoffMs 校验 sweep 入参并算出截止点。ok=false 且 err=nil 表示"截止点非正,本轮什么都不做"(已打日志)。
//
// 两个 Sweep 方法(终态申请 / 零好友容量行)共用这一份:"负截止点 = 清空整表"的护栏只许有一处,
// 各写一遍迟早会有一边漏掉。updated_ms 与 created_ms 都是 bigint unsigned,下面的论证对两者同样成立。
func sweepCutoffMs(ctx context.Context, retentionDays, batchLimit int, nowMs int64) (cutoff uint64, ok bool, err error) {
	// 这三条在生产不可能出现(config.Validate 拒收 ≤0 的 RetentionDays / BatchLimit),
	// 但这里必须 fail-fast 而不是"当默认值":batchLimit=0 会让 DELETE 不带 LIMIT
	// (一条长事务锁住整张 friend_request,把好友申请写路径一起卡住),
	// retentionDays=0 会把刚被拒的申请立刻删掉。
	if batchLimit <= 0 {
		return 0, false, fmt.Errorf("sweep batchLimit 必须为正数,得到 %d", batchLimit)
	}
	if retentionDays <= 0 || retentionDays > maxRetentionDays {
		return 0, false, fmt.Errorf("sweep retentionDays 必须在 [1, %d] 内,得到 %d", maxRetentionDays, retentionDays)
	}

	cutoffMs := nowMs - int64(retentionDays)*millisPerDay
	if cutoffMs <= 0 {
		// 只有"进程时钟没设对"才会走到这里(nowMs 小于保留期本身)。
		// **绝不能**把负数截止点传给 SQL:updated_ms 是 bigint unsigned,MySQL 在
		// 无符号列与负值比较时会把负值按无符号解释成一个天文数字,
		// `updated_ms < <天文数字>` 就是"匹配所有行" —— delete 模式下等于清空整张表。
		logx.WithContext(ctx).Errorf("[friend] sweep 截止点非正(nowMs=%d retention_days=%d),本轮不做任何事:检查进程时钟",
			nowMs, retentionDays)
		return 0, false, nil
	}
	return uint64(cutoffMs), true, nil
}

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
	cutoff, ok, err := sweepCutoffMs(ctx, retentionDays, batchLimit, nowMs)
	if err != nil || !ok {
		return 0, 0, err
	}

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
		// 逐行删:出错前已删掉的行是真删了,把计数带回去,别让日志把它们记成 0。
		return pending, deleted, err
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
// **候选普通读 + 逐行按主键删**,与容量行回收(deleteIdleCapacityRow)同一个写法,不是一条批量
// `DELETE ... WHERE status IN (...) AND updated_ms < ? LIMIT ?`。原因见 friend_repo.go 顶部锁序说明 (6):
// 批量 DELETE 走 idx_status_updated 的范围,**先锁二级索引项、后锁主键**;而玩家重新申请一个很久以前
// 被拒过的人时,AddFriendRequest 的 upsert 是**先锁主键**(④ 的 FOR UPDATE)、后改这一行的二级索引 ——
// 两者恰好撞在同一行上就是反序,1213。批量 DELETE 一次删到上千行、undo 大,InnoDB 更可能牺牲的是
// 玩家那一侧,等于把后台清理的锁冲突转嫁成玩家可见的失败。逐行按主键删时任一时刻只持一行、先主键,
// 与 upsert 同序,不可能成环。
//
// 代价:一轮至多 batchLimit 次往返(默认 1000),对 5 分钟一轮的后台任务可以接受。
// 每行 DELETE 的 WHERE 重复终态与截止点条件,是提交点复核:候选读与删除之间该行可能已被 upsert
// 改回 pending(玩家重新发起申请),这时必须不删。多副本同时跑时,后到的一方删到 0 行,无害。
// 单条自动提交语句,不开事务:分批的意义就是让每次持锁窗口足够短。
func (r *FriendRepo) deleteTerminalRequestsBefore(ctx context.Context, cutoffMs uint64, limit int) (int64, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT from_player_id, to_player_id FROM friend_request WHERE status IN (?, ?) AND updated_ms < ? LIMIT ?",
		requestStatusAccepted, requestStatusRejected, cutoffMs, limit)
	if err != nil {
		return 0, fmt.Errorf("list terminal friend requests before %d: %w", cutoffMs, err)
	}
	type requestKey struct{ from, to uint64 }
	var candidates []requestKey
	for rows.Next() {
		var k requestKey
		if err := rows.Scan(&k.from, &k.to); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan terminal friend request: %w", err)
		}
		candidates = append(candidates, k)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate terminal friend requests: %w", err)
	}

	var deleted int64
	for _, k := range candidates {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		result, err := r.db.ExecContext(ctx, deleteTerminalRequestSQL,
			k.from, k.to, requestStatusAccepted, requestStatusRejected, cutoffMs)
		if err != nil {
			return deleted, fmt.Errorf("delete terminal friend request %d->%d: %w", k.from, k.to, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("delete terminal friend request: rows affected: %w", err)
		}
		deleted += n
	}
	return deleted, nil
}

// deleteTerminalRequestSQL:按 friend_request 完整主键删一行终态过期申请,WHERE 里重复终态与截止点条件
// 作提交点复核。写成包级常量是为了让 TestLockingStatementsArePrimaryKeyPointLookups 对它做 EXPLAIN。
const deleteTerminalRequestSQL = "DELETE FROM friend_request WHERE from_player_id = ? AND to_player_id = ? AND status IN (?, ?) AND updated_ms < ?"

// SweepIdleCapacityRows 跑一轮 friend_capacity 回收,返回(本轮看到的可回收行数, 实际删除行数)。
//
// mode / retentionDays / batchLimit / nowMs 的语义与 SweepTerminalRequests 完全一致
// (同一份 Friend.Sweep 配置,不新增配置键):
//   - mode 不是 SweepModeDelete(含 report_only、空串与任何未知值)→ 只数不删;
//   - batchLimit <= 0、retentionDays 越界 → 返回 error;截止点非正 → 打错误日志并返回 (0,0,nil);
//   - 两个返回值都受 batchLimit 封顶,"等于 batchLimit"只说明积压 ≥ 一批,不是精确积压;
//   - deleted 可以小于 idle 而不是故障:候选读与逐行删除之间,该行可能已被 AcceptFriend 加过好友
//     (提交点复核不再匹配),或者被另一个副本抢先删了。
//
// 与 SweepTerminalRequests 的差别:**没有** "updated_ms=0 就拒删"那道保险 —— created_ms=0 的零好友行
// 被回收是无害的(补行时按权威边数重算)。为什么逐行删、为什么无害,见文件头。
//
// 出错语义:某一行 DELETE 失败(或 ctx 已取消)就停在那里,带着**已经删掉的行数**返回 error ——
// 前面那些行是各自独立提交的,已经删了就是删了,报 0 会让指标与日志对不上账。
// 不重试:后台任务靠节拍自愈,剩下的行下一轮还在候选里。
func (r *FriendRepo) SweepIdleCapacityRows(ctx context.Context, mode string,
	retentionDays, batchLimit int, nowMs int64) (int64, int64, error) {
	cutoff, ok, err := sweepCutoffMs(ctx, retentionDays, batchLimit, nowMs)
	if err != nil || !ok {
		return 0, 0, err
	}

	playerIDs, err := r.listIdleCapacityRowsBefore(ctx, cutoff, batchLimit)
	if err != nil {
		return 0, 0, err
	}
	idle := int64(len(playerIDs))
	if idle == 0 || mode != SweepModeDelete {
		return idle, 0, nil
	}

	var deleted int64
	for _, playerID := range playerIDs {
		// 逐行之间检查 ctx:单轮预算(logic 的 sweepRoundBudget)用完或进程在退出时,
		// 停在行与行之间,而不是让下一条 DELETE 带着一个已取消的 ctx 去报一条含糊的驱动错误。
		if err := ctx.Err(); err != nil {
			return idle, deleted, fmt.Errorf("sweep idle friend capacity rows: 已删 %d/%d 行后中止: %w", deleted, idle, err)
		}
		removed, err := r.deleteIdleCapacityRow(ctx, playerID, cutoff)
		if err != nil {
			return idle, deleted, err
		}
		deleted += removed
	}
	return idle, deleted, nil
}

// listIdleCapacityRowsBefore 取"零好友且建出来已过保留期"的容量行主键,最多 limit 个。
//
// **普通读,不加锁**(不写 FOR UPDATE、不开事务):候选只是"值得去试着删"的名单,
// 权威判定在 deleteIdleCapacityRow 的 WHERE 里。这里一旦加锁,回收就会同时握着多把守卫锁,
// 文件头那条"至多持有一把"的论证当场失效。
// 索引:(friend_count, created_ms),第一列等值、第二列区间,扫描量恒等于命中行数
// (前提是索引真实存在,存量表不会被自动补建 —— 见文件头);
// 二级索引项自带主键 player_id,不必回表。不加 ORDER BY:先删哪一批无所谓。
func (r *FriendRepo) listIdleCapacityRowsBefore(ctx context.Context, cutoffMs uint64, limit int) ([]uint64, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT player_id FROM friend_capacity WHERE friend_count = 0 AND created_ms < ? LIMIT ?",
		cutoffMs, limit)
	if err != nil {
		return nil, fmt.Errorf("list idle friend capacity rows before %d: %w", cutoffMs, err)
	}
	defer rows.Close()

	var playerIDs []uint64
	for rows.Next() {
		var playerID uint64
		if err := rows.Scan(&playerID); err != nil {
			return nil, fmt.Errorf("scan idle friend capacity row: %w", err)
		}
		playerIDs = append(playerIDs, playerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate idle friend capacity rows: %w", err)
	}
	return playerIDs, nil
}

// deleteIdleCapacityRow 按主键删一行容量行,返回实际删除行数(0 或 1)。单条自动提交语句。
//
// WHERE 里重复写 `friend_count = 0 AND created_ms < ?` 是**提交点复核**,不是冗余:
// 候选读与这条 DELETE 之间,该行可能已被 AcceptFriend 加过好友,也可能被删掉后又由 ensure 重新建出来
// (created_ms 变成当前时刻)。DELETE 是当前读,拿到行锁后按最新已提交版本重判 WHERE,不匹配就不删。
// 去掉这两个条件,回收就会删掉一行 friend_count > 0 的权威计数 —— 虽然 ensure 能按边数重算回来,
// 但那等于让"只删零好友行"这条前提靠运气成立。
// 经由 SweepIdleCapacityRows 进来的单线程用例测不到这两个条件(候选读已经按同样的条件滤过一遍),
// 所以由 sweep_repo_test.go 的 TestDeleteIdleCapacityRow_RechecksAtCommitPoint 直调本函数逐条钉住。
// 若该行此刻正被某个写事务的守卫锁着,这条 DELETE 会等到对方提交(此时我们手里没有任何别的锁)。
func (r *FriendRepo) deleteIdleCapacityRow(ctx context.Context, playerID, cutoffMs uint64) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		"DELETE FROM friend_capacity WHERE player_id = ? AND friend_count = 0 AND created_ms < ?",
		playerID, cutoffMs)
	if err != nil {
		return 0, fmt.Errorf("delete idle friend capacity row %d: %w", playerID, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete idle friend capacity row %d: rows affected: %w", playerID, err)
	}
	return removed, nil
}
