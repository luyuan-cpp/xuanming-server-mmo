package data

import (
	"context"
	"database/sql"
	"errors"

	"shared/workshard"
)

// Sweep 只回收过期的个人/纯文本广播邮件；冻结名单及其邮件原件保留作资格和幂等审计。
// PENDING/MANUAL领取、未终结资产指令、未知状态、零过期时间永不由普通过期清理删除。
// mail_box、号源、asset-op和campaign清单不在此清理范围。总实删行数（含派生状态）不超过limit。
type SweepStats struct{ Candidates, Deleted, Invalid uint64 }
type SweepReport struct{ Personal, System, SystemState SweepStats }

// SweepWorkKind 是过期清理按片分工的归属键前缀,片号 = mail_id % 64(缺口清单第 25 条第四批,
// docs/design/assetop-redelivery-sharding-20261110.md §14.2)。与附件重投的 mail-assetop 不同名:同一片的重投与清理不必落在同一个副本上。
const SweepWorkKind = "mail-expiry-sweep"

// SweepShardColumn 是清理分片取模的列。按 mail_id 而不按收件人:清理的单位是一封邮件;系统、帮会、活动邮件的
// owner_player_id 都是 0,按收件人分会全挤进第 0 片;mail_id 是发号器发的打散值,取模均匀。
// 片条件在回表之后判:候选读走 (kind, expire_ms) 索引,本来就要回表取 campaign_id(TiDB 上主键非聚簇,索引条目里也不带 mail_id),
// 每检查一行的代价不变。清理每 5 分钟一轮、稳态候选很少,不为它改索引(设计 §14.2)。
const SweepShardColumn = "m.mail_id"

// sweepEligible 是「可以物理删除」的条件,只有一个参数 cutoff。候选读与删除前的点锁复核共用它。
const sweepEligible = `m.expire_ms>0 AND m.expire_ms<? AND m.campaign_id=0 AND m.kind IN (1,2,3)
  AND NOT EXISTS(SELECT 1 FROM mail_state s WHERE s.mail_id=m.mail_id AND (s.claim_state IS NULL OR s.claim_state NOT IN (0,1,3)))
  AND NOT EXISTS(SELECT 1 FROM mail_asset_op o WHERE o.mail_id=m.mail_id AND (o.status IS NULL OR o.status NOT IN (2,3,5)))`

// sweepCandidateQuery 读 owned 那些片里可删的邮件,条件在 ORDER BY / LIMIT 之前:别的副本的片再早过期也不占本副本的名额。
// owned 只来自 workshard.SQLFilter(列名写死、片号只走绑定参数)。实参顺序:cutoff、片号……、limit。
func sweepCandidateQuery(owned workshard.Filter) string {
	return "SELECT m.mail_id,m.kind FROM mail_message m WHERE " + sweepEligible + " AND " + owned.SQL() + " ORDER BY m.expire_ms,m.mail_id LIMIT ?"
}

// sweepInvalidSampleQuery 在 owned 那些片里按 mail_id 取前 limit 行,数其中 expire_ms = 0 的(有界样本,只计数不修不删)。
// 各副本只抽自己的片,样本不重叠,计数跨副本求和不重复。实参顺序:片号……、limit。
func sweepInvalidSampleQuery(owned workshard.Filter) string {
	return "SELECT COUNT(*) FROM (SELECT m.expire_ms FROM mail_message m WHERE " + owned.SQL() + " ORDER BY m.mail_id LIMIT ?) sample WHERE expire_ms=0"
}

// Sweep 清 owned 那些片里保留期外的邮件。owned 是本副本负责的片(片号 = mail_id % 64,按 SweepShardColumn 生成),
// 零值不过滤(清全部片)。正确性不靠分工:每封都在点锁之后按原条件复核再删,两个副本删同一封,后到的复核落空。
func (r *Repo) Sweep(ctx context.Context, cutoff uint64, limit int, deleteRows bool, owned workshard.Filter) (report SweepReport, err error) {
	if cutoff == 0 || limit < 1 || limit > 10000 {
		return report, ErrInvalid
	}
	args := append([]any{cutoff}, owned.Args()...)
	rows, e := r.db.QueryContext(ctx, sweepCandidateQuery(owned), append(args, limit)...)
	if e != nil {
		return report, e
	}
	type candidate struct {
		id   uint64
		kind uint32
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if e = rows.Scan(&c.id, &c.kind); e != nil {
			rows.Close()
			return report, e
		}
		candidates = append(candidates, c)
		if c.kind == 1 {
			report.Personal.Candidates++
		} else {
			report.System.Candidates++
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return report, e
	}
	// 非法行只计有界样本，不修复也不删除。
	e = r.db.QueryRowContext(ctx, sweepInvalidSampleQuery(owned), append(append([]any{}, owned.Args()...), limit)...).Scan(&report.Personal.Invalid)
	if e != nil {
		return report, e
	}
	if !deleteRows {
		return report, nil
	}
	remaining := limit
	for _, c := range candidates {
		if remaining == 0 {
			break
		}
		tx, e := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if e != nil {
			return report, e
		}
		var locked uint64
		e = tx.QueryRowContext(ctx, "SELECT m.mail_id FROM mail_message m WHERE m.mail_id=? AND "+sweepEligible+" FOR UPDATE", c.id, cutoff).Scan(&locked)
		if errors.Is(e, sql.ErrNoRows) {
			_ = tx.Rollback()
			continue
		}
		if e != nil {
			_ = tx.Rollback()
			return report, e
		}
		// 保留期至少一天，已过期请求不再能创建新的领取；仍在途和人工状态在同一事务重新复核。
		deletedStates, e := tx.ExecContext(ctx, "DELETE FROM mail_state WHERE mail_id=? AND claim_state IN (0,1,3) LIMIT ?", c.id, remaining)
		if e != nil {
			_ = tx.Rollback()
			return report, e
		}
		n, e := deletedStates.RowsAffected()
		if e != nil {
			_ = tx.Rollback()
			return report, e
		}
		used := int(n)
		var removed int64
		if used < remaining {
			result, e := tx.ExecContext(ctx, "DELETE FROM mail_message WHERE mail_id=? AND expire_ms>0 AND expire_ms<? AND campaign_id=0 AND NOT EXISTS(SELECT 1 FROM mail_state s WHERE s.mail_id=?) AND NOT EXISTS(SELECT 1 FROM mail_asset_op o WHERE o.mail_id=? AND (o.status IS NULL OR o.status NOT IN (2,3,5)))", c.id, cutoff, c.id, c.id)
			if e != nil {
				_ = tx.Rollback()
				return report, e
			}
			removed, e = result.RowsAffected()
			if e != nil {
				_ = tx.Rollback()
				return report, e
			}
			used += int(removed)
		}
		if e = tx.Commit(); e != nil {
			return report, e
		}
		remaining -= used
		report.SystemState.Deleted += uint64(n)
		if c.kind == 1 {
			report.Personal.Deleted += uint64(removed)
		} else {
			report.System.Deleted += uint64(removed)
		}
	}
	return report, nil
}
