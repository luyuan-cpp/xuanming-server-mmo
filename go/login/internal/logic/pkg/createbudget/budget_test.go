package createbudget

// CreatePlayer 截止分摊的单测(docs/design/client-request-ordering.md §5.3 / §6.1)。全部用注入的 now 与固定的
// handler 截止推算,不依赖墙钟;只有 Derive 的用例会真的建 ctx,截止都取在一小时以后,结论不随机器快慢变化。

import (
	"context"
	"errors"
	"testing"
	"time"

	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/playernamereg"

	"github.com/stretchr/testify/require"
)

// login 现行 zrpc Timeout(go/login/etc/login.yaml、deploy/login-stack.linux/login.yaml)。
const serverTimeout = 4000 * time.Millisecond

func TestHopNamesCoverEveryHop(t *testing.T) {
	for h := Hop(0); h < hopCount; h++ {
		require.NotEmpty(t, hopNames[h], "hop %d has no name", h)
	}
}

// 缺省方案(RegisterTimeout 不写 = homezone 缺省)正好放进 4000:四跳上限 300 + 1700 + 800 + 800、本地余量 250、
// 三个跳间间隙 150。两份 login.yaml 与 K8s ConfigMap(不渲染 HomeZone 块)都落在这一档。
func TestDefaultPlanFitsServerTimeout(t *testing.T) {
	p := New(0)
	require.Equal(t, homezone.DefaultRegisterTimeout, p.Cap(HopRegister))
	require.Equal(t, serverTimeout, p.Required())
	require.NoError(t, p.Validate(serverTimeout))
	require.NoError(t, New(homezone.DefaultRegisterTimeout).Validate(serverTimeout))
}

// 门禁反例:Timeout 放不下、player_route 登记沿用旧的 3s、Timeout 为 0 / 负,都拒绝启动。
func TestValidateRejectsBudgetsThatDoNotFit(t *testing.T) {
	require.Error(t, New(0).Validate(serverTimeout-time.Millisecond), "少 1ms 也必须拒绝")
	require.Error(t, New(3*time.Second).Validate(serverTimeout), "旧的 3s 登记预算放不进 4000")
	require.Error(t, New(0).Validate(0))
	require.Error(t, New(0).Validate(-time.Second))
}

// 下限高于上限的跳永远发不出去;上限为 0 的跳没有界。两种都拒绝启动。
func TestValidateRejectsInconsistentHop(t *testing.T) {
	p := New(0)
	p.hops[HopName].floor = p.hops[HopName].cap + time.Millisecond
	require.Error(t, p.Validate(time.Hour))
	p = New(0)
	p.hops[HopMint].cap = 0
	require.Error(t, p.Validate(time.Hour))
}

// 名字这一跳的上限放得下「一次 + 同名重试一次」:第一次登记恰好超时返回后,剩下的仍不少于一个单次预算,
// tryReserveName 的同名重试门槛(hasBudget(DefaultReserveTimeout))过得去。下限就是单次预算。
func TestNameHopKeepsSameNameRetry(t *testing.T) {
	p := New(0)
	require.Equal(t, playernamereg.DefaultReserveTimeout, p.Floor(HopName))
	afterFirstTimeout := p.Cap(HopName) - playernamereg.DefaultReserveTimeout
	require.Greater(t, afterFirstTimeout, playernamereg.DefaultReserveTimeout,
		"first attempt timing out must still leave a full single-reserve budget for the same-name retry")
}

// 余量 = 后面每一跳的下限 + 跳间间隙;提交点(围栏写)的下限就是它的上限。
func TestReserveAfterSumsLaterFloorsAndGaps(t *testing.T) {
	p := New(0)
	commit := p.Floor(HopCommit) + HopGap
	register := p.Floor(HopRegister) + HopGap
	name := p.Floor(HopName) + HopGap
	require.Equal(t, time.Duration(0), p.ReserveAfter(HopCommit))
	require.Equal(t, commit, p.ReserveAfter(HopRegister))
	require.Equal(t, commit+register, p.ReserveAfter(HopName))
	require.Equal(t, commit+register+name, p.ReserveAfter(HopMint))
	require.Equal(t, CommitCap, p.Floor(HopCommit), "the commit point always gets its full upper bound")
}

// 前置步骤用满本地余量、每一跳都拖到自己的截止、跳间用满间隙:每一跳仍拿满自己的上限,
// 而且最后一跳的截止恰好落在 handler 截止上、不越过它。这是启动门禁 Required ≤ Timeout 保证的性质。
func TestEveryHopGetsItsFullCapWhenEarlierHopsRunToTheirDeadline(t *testing.T) {
	p := New(0)
	start := time.Unix(1_000_000, 0)
	deadline := start.Add(serverTimeout)
	now := start.Add(LocalAllowance)
	for h := Hop(0); h < hopCount; h++ {
		end, available, ok := p.Window(h, now, deadline, true)
		require.True(t, ok, "hop %s skipped", h)
		require.Equal(t, p.Cap(h), available, "hop %s squeezed", h)
		require.False(t, end.After(deadline), "hop %s ends after the handler deadline", h)
		now = end.Add(HopGap)
	}
}

// 任意时刻、任意一跳拿到的截止都不晚于 handler 截止,也不晚于 handler 截止 − 余量。
func TestWindowNeverEndsAfterHandlerDeadline(t *testing.T) {
	p := New(0)
	start := time.Unix(1_000_000, 0)
	deadline := start.Add(serverTimeout)
	for h := Hop(0); h < hopCount; h++ {
		for at := time.Duration(0); at <= serverTimeout+time.Second; at += 50 * time.Millisecond {
			end, _, _ := p.Window(h, start.Add(at), deadline, true)
			require.False(t, end.After(deadline.Add(-p.ReserveAfter(h))), "hop %s at +%s", h, at)
		}
	}
}

// 前置步骤拖慢之后,前面的跳先被截短;剩下的放不下本跳下限 + 后面的余量时不发。
func TestWindowSkipsHopWhenBudgetIsShort(t *testing.T) {
	p := New(0)
	start := time.Unix(1_000_000, 0)
	deadline := start.Add(serverTimeout)

	// 发号:handler 截止 − 余量之前还有 10ms → 截短到 10ms 照发;一点都没有 → 不发。
	at := deadline.Add(-p.ReserveAfter(HopMint) - 10*time.Millisecond)
	_, available, ok := p.Window(HopMint, at, deadline, true)
	require.True(t, ok)
	require.Equal(t, 10*time.Millisecond, available)
	_, _, ok = p.Window(HopMint, deadline.Add(-p.ReserveAfter(HopMint)), deadline, true)
	require.False(t, ok, "no time left for minting must skip it")

	// 名字:只剩不到一个单次预算 → 不发第一次登记。
	at = deadline.Add(-p.ReserveAfter(HopName) - p.Floor(HopName) + time.Millisecond)
	_, _, ok = p.Window(HopName, at, deadline, true)
	require.False(t, ok)

	// player_route 登记 / 提交:拿不满下限(= 上限)就不发。
	at = deadline.Add(-p.ReserveAfter(HopRegister) - p.Cap(HopRegister) + time.Millisecond)
	_, _, ok = p.Window(HopRegister, at, deadline, true)
	require.False(t, ok)
	_, _, ok = p.Window(HopCommit, deadline.Add(-CommitCap+time.Millisecond), deadline, true)
	require.False(t, ok, "the commit point must not be sent with less than its full budget")
	_, _, ok = p.Window(HopCommit, deadline.Add(-CommitCap), deadline, true)
	require.True(t, ok)
}

// handler 没有截止(单测、离线工具):只按上限截。
func TestWindowWithoutHandlerDeadlineUsesCap(t *testing.T) {
	p := New(0)
	now := time.Unix(1_000_000, 0)
	for h := Hop(0); h < hopCount; h++ {
		end, available, ok := p.Window(h, now, time.Time{}, false)
		require.True(t, ok)
		require.Equal(t, p.Cap(h), available)
		require.Equal(t, now.Add(p.Cap(h)), end)
	}
}

func TestDeriveReturnsParentErrorAndShortBudget(t *testing.T) {
	p := New(0)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := p.Derive(cancelled, HopMint, time.Now())
	require.ErrorIs(t, err, context.Canceled)

	// 截止在一小时以后,注入的 now 落在截止前 1s:名字这一跳要给后面留 1700ms 余量,一点都拿不到,不发;
	// 提交点是最后一跳,不留余量,拿满 800ms 上限。
	parent, cancelParent := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancelParent()
	deadline, _ := parent.Deadline()
	_, _, err = p.Derive(parent, HopName, deadline.Add(-time.Second))
	var short *ShortBudgetError
	require.True(t, errors.As(err, &short))
	require.Equal(t, HopName, short.Hop)
	require.Equal(t, p.Floor(HopName), short.Need)

	ctx, cancelHop, err := p.Derive(parent, HopCommit, deadline.Add(-time.Second))
	require.NoError(t, err)
	defer cancelHop()
	end, ok := ctx.Deadline()
	require.True(t, ok)
	require.False(t, end.After(deadline))
	require.Equal(t, deadline.Add(-time.Second).Add(CommitCap), end)
}
