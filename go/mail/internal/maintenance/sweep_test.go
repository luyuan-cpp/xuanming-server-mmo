package maintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"mail/internal/config"
	"mail/internal/data"
	"mail/internal/metrics"
	"shared/drainproof"
	"shared/workshard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 缺口清单第 25 条第四批(docs/design/assetop-redelivery-sharding-20261110.md §14.2、§14.3):过期清理按 mail_id 分片,
// 每一轮各自 recover,活性时刻写在一轮正常返回之后。这里只测循环一侧:交给存储的条件、一轮出错与 panic 时怎么收场、
// 活性序列何时写何时删。条件在真库上的效果见 internal/data/sweep_test.go 的 TiDB 用例。

// fakeSweeper 记下每一轮收到的分片条件;during 在 Sweep 里调用,让用例在一轮中途看指标或制造 panic。
type fakeSweeper struct {
	mu     sync.Mutex
	owned  []workshard.Filter
	during func()
	err    error
}

func (f *fakeSweeper) Sweep(_ context.Context, _ uint64, _ int, _ bool, owned workshard.Filter) (data.SweepReport, error) {
	f.mu.Lock()
	f.owned = append(f.owned, owned)
	f.mu.Unlock()
	if f.during != nil {
		f.during()
	}
	return data.SweepReport{}, f.err
}

func (f *fakeSweeper) filters() []workshard.Filter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workshard.Filter(nil), f.owned...)
}

// held 报告排空 SOURCE 是否被打过 HOLD(结局未知)。会关掉它的入口,只在用例收尾时调。
func held(t *testing.T, root *drainproof.Session) bool {
	t.Helper()
	root.CloseAdmission()
	err := root.Wait(context.Background())
	if err != nil && !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("夹具前提:关掉入口后 Wait 只该回 nil 或 ErrHold,实际 %v", err)
	}
	return err != nil
}

// 片条件按 mail_id 取模(不是收件人:共享邮件的 owner_player_id 都是 0),片号升序只走绑定参数;不分工时不加条件,
// 一片都不归本副本时条件恒假;类别键与附件重投的不同名。
func TestSweepRoundPassesOwnedShardsByMailID(t *testing.T) {
	metrics.Register()
	cfg := Config{}.Defaults()
	mine := map[string]bool{workshard.ShardKey(data.SweepWorkKind, 40): true, workshard.ShardKey(data.SweepWorkKind, 7): true}
	for _, tc := range []struct {
		name     string
		owns     func(string) bool
		wantSQL  string
		wantArgs string
	}{
		{"不分工", nil, "1=1", "[]"},
		{"负责两片", func(key string) bool { return mine[key] }, "MOD(m.mail_id,64) IN(?,?)", "[7 40]"},
		{"一片都不归本副本", func(string) bool { return false }, "1=0", "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSweeper{}
			sweepRound(context.Background(), fake, cfg, tc.owns, drainproof.New())
			got := fake.filters()
			require.Len(t, got, 1, "一轮只读一次候选")
			assert.Equal(t, tc.wantSQL, got[0].SQL())
			assert.Equal(t, tc.wantArgs, fmt.Sprint(got[0].Args()))
		})
	}
	assert.NotEqual(t, "mail-assetop", data.SweepWorkKind, "清理与附件重投各用各的类别键")
}

// 活性时刻写在一轮正常返回之后:一轮进行中还是上一轮的值(原来 defer 在开头就求了 time.Now(),记的是开始时刻);
// 这一轮出错也写(出错有错误日志与排空 HOLD,不该同时报成循环卡死)。
func TestSweepTickMarksLivenessAfterEachFinishedRound(t *testing.T) {
	metrics.Register()
	cfg := Config{}.Defaults()
	metrics.SweepLive.Mark(time.Unix(1000, 0))
	var during float64
	fake := &fakeSweeper{during: func() { during, _ = metrics.SweepLive.Value() }}
	require.True(t, sweepTick(context.Background(), fake, cfg, nil, drainproof.New()))
	assert.Equal(t, float64(1000), during, "一轮进行中不写活性时刻")
	after, ok := metrics.SweepLive.Value()
	require.True(t, ok)
	assert.Greater(t, after, float64(1000), "一轮跑完之后把时刻往前写")

	metrics.SweepLive.Mark(time.Unix(1000, 0))
	failing := &fakeSweeper{err: errors.New("database unavailable")}
	require.True(t, sweepTick(context.Background(), failing, cfg, nil, drainproof.New()), "出错是这一轮自己处理过的失败,算正常跑完")
	value, _ := metrics.SweepLive.Value()
	assert.Greater(t, value, float64(1000), "这一轮出错也算跑完,照样写")
	metrics.SweepLive.Stop()
}

// 删除模式下清理出错按结局未知打 HOLD;只读模式(report_only)出错不打:它什么都没删。
func TestSweepRoundHoldsOnDeleteFailureOnly(t *testing.T) {
	metrics.Register()
	failing := &fakeSweeper{err: errors.New("database unavailable")}

	reportOnly := drainproof.New()
	sweepRound(context.Background(), failing, Config{Mode: config.SweepModeReportOnly}.Defaults(), nil, reportOnly)
	assert.False(t, held(t, reportOnly), "只读模式出错不算结局未知")

	deleting := drainproof.New()
	sweepRound(context.Background(), failing, Config{Mode: config.SweepModeDelete}.Defaults(), nil, deleting)
	assert.True(t, held(t, deleting), "删除模式出错:这一轮删到哪一步不知道,排空证明停在 HOLD")
}

// 一轮 panic:不往外抛(进程不死)、不写活性时刻、排空 SOURCE 打了 HOLD;下一拍照常跑。
// 原来 panic 会一路抛出 Run 把进程打死;按片分工之后,一封坏邮件会随着片换主把每个副本挨个打死。
func TestSweepTickSurvivesAPanickingRound(t *testing.T) {
	metrics.Register()
	cfg := Config{Mode: config.SweepModeDelete}.Defaults()
	root := drainproof.New()
	metrics.SweepLive.Mark(time.Unix(1000, 0))

	poisoned := &fakeSweeper{during: func() { panic("test: poisoned mail row") }}
	require.False(t, sweepTick(context.Background(), poisoned, cfg, nil, root), "panic 的一轮被兜住,回 false")
	value, ok := metrics.SweepLive.Value()
	require.True(t, ok)
	assert.Equal(t, float64(1000), value, "panic 的一轮不写活性时刻")

	healthy := &fakeSweeper{}
	require.True(t, sweepTick(context.Background(), healthy, cfg, nil, root), "panic 之后下一拍照常跑")
	require.Len(t, healthy.filters(), 1, "panic 那一轮占的入口已经还回去,下一拍进得去")
	value, _ = metrics.SweepLive.Value()
	assert.Greater(t, value, float64(1000))
	assert.True(t, held(t, root), "panic 的那一轮结局未知,排空证明停在 HOLD")
	metrics.SweepLive.Stop()
}

// 排空入口已关(停机排空):这一轮跳过,不碰存储。
func TestSweepRoundSkipsWhenAdmissionIsClosed(t *testing.T) {
	metrics.Register()
	root := drainproof.New()
	root.CloseAdmission()
	fake := &fakeSweeper{}
	sweepRound(context.Background(), fake, Config{}.Defaults(), nil, root)
	assert.Empty(t, fake.filters(), "入口关了之后不再开新的一轮")
}

// 循环一起来就写一次(首轮之前有至多一个周期的随机抖动,不能等第一轮);ctx 结束、有意停止时删掉序列(停掉的循环不算卡住)。
func TestRunMarksLivenessAtStartAndStopsItOnExit(t *testing.T) {
	metrics.Register()
	metrics.SweepLive.Stop()
	_, ok := metrics.SweepLive.Value()
	require.False(t, ok, "夹具前提:开始时没有序列")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, &fakeSweeper{}, Config{Interval: time.Hour}, nil, drainproof.New())
	}()
	require.Eventually(t, func() bool { _, ok := metrics.SweepLive.Value(); return ok }, 2*time.Second, 10*time.Millisecond,
		"循环一起来就写一次")
	cancel()
	<-done
	_, ok = metrics.SweepLive.Value()
	assert.False(t, ok, "循环停止之后删掉序列")
}
