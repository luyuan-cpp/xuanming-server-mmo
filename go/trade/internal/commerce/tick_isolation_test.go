package commerce

// tick_isolation_test.go —— Tick 单行隔离的纯函数部分(2026-10-20,第一轮审计 §2.5 go-asset-services#0),不连库。
// 端到端行为(队头失败不挡其余、坏数据隔离、持有竞价不占队头、ACK 翻页)在 tick_isolation_db_test.go 的真库用例里。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"shared/assetop"

	"github.com/stretchr/testify/require"
)

func TestTickFailureClassification(t *testing.T) {
	pending := fmt.Errorf("推进交易合同: %w", fmt.Errorf("%w (player=1 stream=3 pending=16)", assetop.ErrTooManyPending))
	require.Equal(t, failureTooManyPending, failureClass(pending))
	require.Equal(t, failureRetry, failureClass(errors.New("i/o timeout")))
	bad := corrupt(sql.ErrNoRows)
	require.Equal(t, failureCorrupt, failureClass(bad))
	require.ErrorIs(t, bad, sql.ErrNoRows, "corrupt 包装不能吞掉原错误")
	require.Equal(t, failureCorrupt, failureClass(fmt.Errorf("推进交易合同: %w", corrupt(pending))),
		"坏数据优先于暂时失败:否则会按暂时失败无限重试")
}

// 2026-10-22:ErrConflict 不再一律算暂时失败。乐观冲突(conflict)下一轮立即重试,角色冲突(role_conflict)分钟级复查;
// 两种包装都必须保留 ErrConflict,客户端拿到的 tip 码(commerceCode 用 errors.As)不变。
func TestTickConflictClassification(t *testing.T) {
	stale := fmt.Errorf("推进交易合同: %w", staleObservation())
	require.Equal(t, failureConflict, failureClass(stale))
	require.ErrorIs(t, stale, ErrConflict)
	var rejection Rejection
	require.ErrorAs(t, stale, &rejection)
	require.Equal(t, ErrConflict, rejection, "客户端看到的仍是原来的状态冲突码")

	role := fmt.Errorf("推进交易合同: %w", roleConflict())
	require.Equal(t, failureRoleConflict, failureClass(role))
	require.ErrorIs(t, role, ErrConflict)

	// 角色目录回的 ErrConflict(冻结、目录行不合格)经 asRoleConflict 归入 role_conflict;
	// 存储 / RPC 错误原样返回,仍按暂时失败重试,不能被误判成长期状态。
	require.Equal(t, failureRoleConflict, failureClass(asRoleConflict(ErrConflict)))
	require.Equal(t, role, asRoleConflict(role), "已经标过的不重复包装")
	storage := errors.New("lock wait timeout")
	require.Equal(t, storage, asRoleConflict(storage))
	require.Equal(t, failureRetry, failureClass(asRoleConflict(storage)))

	require.Equal(t, failureCorrupt, failureClass(corrupt(roleConflict())), "坏数据仍然优先")
}

// must_fix(2026-10-22 复核):单行子预算必须从父截止时刻推出、严格早于它结束,并给推迟 / 隔离与收据 ACK 留出余量。
// 以前固定 5s,父 ctx(svc 每轮 3s)先到期,卡住的行连推迟都做不了,下一秒又排在队头吃满整轮。
func TestRowBudgetDerivesFromParentDeadline(t *testing.T) {
	// base 放在一小时之后:ctx 在用例期间不会真的到期,预算只由 base 与截止时刻之差决定,不依赖墙钟。
	base := time.Now().Add(time.Hour)
	budgetWith := func(remaining time.Duration) (time.Duration, bool) {
		ctx, cancel := context.WithDeadline(context.Background(), base.Add(remaining))
		defer cancel()
		return rowBudgetFor(ctx, base)
	}

	budget, ok := budgetWith(3 * time.Second) // 生产:svc.AssetChannel.Start 每轮给合同推进 3s
	require.True(t, ok)
	require.Equal(t, 1100*time.Millisecond, budget)
	require.Less(t, budget+tickDeferBudget+tickAckReserve, 3*time.Second,
		"卡住的行先撞上自己的子预算,父 ctx 还剩推迟与 ACK 的余量")

	// 卡住一行并推迟之后,同一轮后面的行仍然拿得到预算。
	budget, ok = budgetWith(3*time.Second - 1100*time.Millisecond - tickDeferBudget)
	require.True(t, ok)
	require.Equal(t, 400*time.Millisecond, budget)

	budget, ok = budgetWith(time.Minute)
	require.True(t, ok)
	require.Equal(t, tickRowBudget, budget, "父预算很长时不超过绝对上限")

	_, ok = budgetWith(tickAckReserve + tickDeferBudget + 2*tickRowMinBudget - time.Millisecond)
	require.False(t, ok, "剩余不够一行的最小预算时不再开新行,把时间留给收据 ACK")
	_, ok = budgetWith(-time.Second)
	require.False(t, ok)

	// 性质:任何剩余时间下,开得出的子预算加上推迟与 ACK 的预留都不越过父截止时刻。
	for remaining := time.Duration(0); remaining <= 20*time.Second; remaining += 50 * time.Millisecond {
		if budget, ok := budgetWith(remaining); ok {
			require.LessOrEqual(t, budget+tickDeferBudget+tickAckReserve, remaining, "remaining=%s", remaining)
			require.GreaterOrEqual(t, budget, tickRowMinBudget, "remaining=%s", remaining)
		}
	}

	budget, ok = rowBudgetFor(context.Background(), base)
	require.True(t, ok)
	require.Equal(t, tickRowBudget, budget, "没有截止时刻时用绝对上限")
}

// 推迟必须有界、带抖动,且积压背压(ErrTooManyPending)比一般暂时失败退得更远。
func TestTickFailureBackoffIsBoundedAndJittered(t *testing.T) {
	const now = uint64(1_800_000_000_000)
	pending := fmt.Errorf("%w (player=1 stream=3 pending=16)", assetop.ErrTooManyPending)
	cases := []struct {
		name     string
		cause    error
		min, max uint64
	}{
		{"积压背压", pending, uint64(tickPendingBackoff.Milliseconds()) * 8 / 10, uint64(tickPendingBackoff.Milliseconds()) * 12 / 10},
		{"一般暂时失败", errors.New("store timeout"), uint64(tickRetryBackoff.Milliseconds()) * 8 / 10, uint64(tickRetryBackoff.Milliseconds()) * 12 / 10},
		{"角色冲突", roleConflict(), uint64(tickRoleConflictBackoff.Milliseconds()) * 8 / 10, uint64(tickRoleConflictBackoff.Milliseconds()) * 12 / 10},
	}
	for _, c := range cases {
		var seen []uint64
		for _, r := range []float64{0, 0.5, 0.999999} {
			delay := tickFailureBackoff(now, c.cause, func() float64 { return r }) - now
			require.GreaterOrEqual(t, delay, c.min, c.name)
			require.LessOrEqual(t, delay, c.max, c.name)
			seen = append(seen, delay)
		}
		require.Less(t, seen[0], seen[2], "%s:抖动必须生效,多行同时失败不能在同一毫秒一起回来", c.name)
	}
	require.Greater(t, tickFailureBackoff(now, pending, nil), tickFailureBackoff(now, errors.New("x"), nil))
	require.Greater(t, tickFailureBackoff(now, roleConflict(), nil), tickFailureBackoff(now, pending, nil),
		"长期状态退得最远:换主 / 冻结不会在一分钟内消失")
}
