package savebarrier

import "fmt"

// 本文件是「Redis 屏障 vs SQL 行围栏」的判定规则,是这条业务知识唯一的权威实现
// (docs/design/zone-redis-regression-guard-20261022.md §4.1.4)。只做纯计算,不碰 Redis 与 SQL。
//
// 版本串都是 Version 产出的 41 字节定宽格式,缺失记为空串。定宽串的逐字节顺序等于 (owner_epoch, save_seq)
// 的元组顺序,空串小于任何版本,所以这里直接用 Go 的字符串比较。

// BarrierState 是屏障 hash 里 pending 与两表 ACK 三个字段之间的关系,只看 Redis 一侧。
type BarrierState uint8

const (
	// BarrierCommitted:三个字段都为空(没有存盘历史),或三者相等(最近一次存盘两张表都已落 SQL)。
	BarrierCommitted BarrierState = iota + 1
	// BarrierInflight:pending 存在,至少一张表的 ACK 缺失或低于 pending。正常的在途存盘,补投会追平。
	BarrierInflight
	// BarrierAckAhead:有 ACK 高于 pending,或 pending 缺失而 ACK 存在。
	// go/db 只在 SQL 提交 V 之后才写 ACK=V,而 V 必然先作为 pending 写进过屏障、pending 又只升不降,
	// 所以正常流程里 ACK ≤ pending 恒成立;违反它只能是 Redis 回退、手工改写或合服拷错了屏障。
	BarrierAckAhead
)

// ClassifyBarrier 判定屏障三个字段的关系。任一非空字段不是规范版本串时返回错误(调用方 fail-closed)。
func ClassifyBarrier(pending, ackFirst, ackSecond string) (BarrierState, error) {
	if err := requireVersions(pending, ackFirst, ackSecond); err != nil {
		return 0, err
	}
	switch {
	case ackFirst > pending || ackSecond > pending:
		return BarrierAckAhead, nil
	case pending == "":
		return BarrierCommitted, nil
	case ackFirst == pending && ackSecond == pending:
		return BarrierCommitted, nil
	default:
		return BarrierInflight, nil
	}
}

// Verdict 是登录核验路径把 Redis 屏障与 SQL 行围栏对齐之后的结论。
type Verdict uint8

const (
	// VerdictConsistent:SQL 两行、pending、两表 ACK 全部相等(含双方都没有历史)。Redis 一侧可信。
	VerdictConsistent Verdict = iota + 1
	// VerdictAdoptSQL:SQL 两行同版本,不低于 pending 与两表 ACK,且不是全部相等。
	// Redis 丢过已确认的写(或只丢了 ACK),以已提交的 SQL 为准重建父快照与屏障。
	VerdictAdoptSQL
	// VerdictInflight:pending 新于 SQL。正常在途,等补投追平。
	VerdictInflight
	// VerdictReconciling:SQL 两行版本不同,且较新的一行新于 pending。另一张表的任务多半还在
	// Kafka 积压或重试队列里,等它落地;长期不收敛转人工 save_barrier_restore。
	VerdictReconciling
	// VerdictSQLBehind:某个 ACK 高于 SQL 两行。ACK 只在 SQL 提交之后才写,
	// 高于 SQL 只能是 SQL 侧回档或损坏,不自动处置(db-write-fencing-mysql.md §9.2)。
	VerdictSQLBehind
)

// String 用作指标 label,取值集合封闭。
func (v Verdict) String() string {
	switch v {
	case VerdictConsistent:
		return "consistent"
	case VerdictAdoptSQL:
		return "adopted"
	case VerdictInflight:
		return "inflight"
	case VerdictReconciling:
		return "reconciling"
	case VerdictSQLBehind:
		return "sql_behind"
	default:
		return "unknown"
	}
}

// Observation 是一次核验读到的全部版本。
type Observation struct {
	Pending, AckFirst, AckSecond string // 屏障 hash 的 pending 与两表 ACK,缺失为空串
	SQLFirst, SQLSecond          string // 两张 SQL 行的行内围栏版本;行不存在或围栏为 (0,0) 为空串,见 RowVersion
}

// RowVersion 把 SQL 行内围栏换成版本串。(0,0) 表示这一行从未经围栏写入,记为空串。
func RowVersion(fenceOwnerEpoch, fenceSaveSeq uint64) string {
	if fenceOwnerEpoch == 0 && fenceSaveSeq == 0 {
		return ""
	}
	return Version(fenceOwnerEpoch, fenceSaveSeq)
}

// Reconcile 按设计 §4.1.4 的判定表给出结论。规则顺序有含义:先排除「SQL 落后」,再看两行是否一致,
// 最后比较 SQL 与 Redis 屏障。
func Reconcile(o Observation) (Verdict, error) {
	if err := requireVersions(o.Pending, o.AckFirst, o.AckSecond, o.SQLFirst, o.SQLSecond); err != nil {
		return 0, err
	}
	sqlNewest := max(o.SQLFirst, o.SQLSecond)
	if max(o.AckFirst, o.AckSecond) > sqlNewest {
		return VerdictSQLBehind, nil
	}
	if o.SQLFirst != o.SQLSecond {
		if sqlNewest > o.Pending {
			return VerdictReconciling, nil
		}
		return VerdictInflight, nil
	}
	sql := o.SQLFirst
	switch {
	case sql == o.Pending && o.AckFirst == sql && o.AckSecond == sql:
		return VerdictConsistent, nil
	case sql >= o.Pending:
		// 走到这里 sql 必然非空:sql 为空时 pending 也为空,而两表 ACK 不高于 sql(上面已排除),只能也为空,落在上一支。
		return VerdictAdoptSQL, nil
	default:
		return VerdictInflight, nil
	}
}

func requireVersions(versions ...string) error {
	for _, v := range versions {
		if v == "" {
			continue
		}
		if _, _, err := Parse(v); err != nil {
			return fmt.Errorf("存盘屏障版本 %.48q 不规范: %w", v, err)
		}
	}
	return nil
}
