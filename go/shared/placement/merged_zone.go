package placement

// 别名合服的归属解析(docs/design/player-data-tidb-scaling-plan.md §7.1)。
//
// 合服只写一条 zone_merged_into「源区 → 目标区」的映射,玩家行和路由行都不动,home_zone 还是源区。
// 所有按区分组的地方(榜单、频道、市场、公会、进场判断)先把 home_zone 解析成最终区再用。
// 解析规则只有 ResolveMergedZone 一份;ResolveMergedZoneSQL 是给事务内直接读 player_route.home_zone 的调用方用的同规则点查。
//
// 原始归属只有三处用,这三处不解析:go/db 按 home_zone 选库(玩家数据不搬,原值才找得到库)、
// RegisterPlayerZone 写入、路由工具。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MaxMergedZoneHops 是沿合服映射解析的跳数上限。连续合服(5→6→7)每次加一跳,16 跳远超运营上可能发生的合服次数;
// 超过它只可能是映射表写坏了,按成环同样处理。
const MaxMergedZoneHops = 16

// ErrMergedZoneUnresolvable 表示合服映射解析不出最终区:成环、超过 MaxMergedZoneHops 跳,或目标区是 0。
// 调用方必须 fail-closed:解析错了就是把玩家分进错的区(榜单、频道、市场全错),宁可这次请求失败。
var ErrMergedZoneUnresolvable = errors.New("placement: merged zone chain cycles, exceeds the hop limit or targets zone 0")

// ResolveMergedZone 沿 merges(源区 → 目标区)解析 zone 的最终区。zone 不在表里返回自身;
// 恰好 MaxMergedZoneHops 跳走完算成功,再多一跳(或成环)回 ErrMergedZoneUnresolvable。
// 纯函数,不改 merges;merges 为 nil 等于空表。
func ResolveMergedZone(merges map[uint32]uint32, zone uint32) (uint32, error) {
	cur := zone
	for hops := 0; ; hops++ {
		next, merged := merges[cur]
		if !merged {
			return cur, nil
		}
		if next == 0 || hops == MaxMergedZoneHops {
			return 0, fmt.Errorf("%w: zone %d stuck at %d after %d hops (next %d)", ErrMergedZoneUnresolvable, zone, cur, hops, next)
		}
		cur = next
	}
}

// MergedZoneQueryer 是 *sql.DB 与 *sql.Tx 共有的单行读接口。
type MergedZoneQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ResolveMergedZoneSQL 是 ResolveMergedZone 的 SQL 形态:逐跳点查 schema 库的 zone_merged_into(主键 src_zone),不加锁。
// 给事务内或批外直接读 player_route.home_zone 用来分组的调用方;这些地方拿不到 data_service 的映射缓存,
// 而映射表极小、很少变、点查走主键,多一条语句的代价可以接受。不在事务快照之外另开连接:q 传 *sql.Tx 就读事务的快照。
//
// 跳数上限与成环判据同 ResolveMergedZone(ErrMergedZoneUnresolvable);schema 不合法回 ErrRoleRouteFenced 同口径的拒绝;
// SQL 错误原样包装返回,调用方不得当成「没有合服」。
func ResolveMergedZoneSQL(ctx context.Context, q MergedZoneQueryer, schema string, zone uint32) (uint32, error) {
	if q == nil || !roleRouteSchemaPattern.MatchString(schema) {
		return 0, fmt.Errorf("%w: resolve merged zone needs a queryer and a valid schema", ErrRoleRouteFenced)
	}
	query := "SELECT dst_zone FROM `" + schema + "`.zone_merged_into WHERE src_zone=?"
	cur := zone
	for hops := 0; ; hops++ {
		var next uint32
		err := q.QueryRowContext(ctx, query, cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read zone_merged_into src_zone=%d: %w", cur, err)
		}
		if next == 0 || hops == MaxMergedZoneHops {
			return 0, fmt.Errorf("%w: zone %d stuck at %d after %d hops (next %d)", ErrMergedZoneUnresolvable, zone, cur, hops, next)
		}
		cur = next
	}
}
