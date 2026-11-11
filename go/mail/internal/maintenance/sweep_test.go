package maintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"mail/internal/data"
	"mail/internal/metrics"
	"shared/workshard"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 缺口清单第 25 条第四批(docs/design/assetop-redelivery-sharding-20261110.md §14.2、§14.3):过期清理按 mail_id 分片,
// 循环的活性时刻写在每一轮结束时。这里只测循环一侧:交给存储的条件、活性序列何时写何时删。条件在真库上的效果见
// internal/data/sweep_test.go 的 TiDB 用例。

// fakeSweeper 记下每一轮收到的分片条件;during 在 Sweep 里调用,让用例在一轮中途看指标。
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

// sweepLastRun 读 mail_sweep_last_run_unixtime;没有序列(或读不出)时 ok=false。不接 *testing.T:
// require.Eventually 在别的 goroutine 里调条件函数,那里不能 FailNow。
func sweepLastRun() (float64, bool) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, false
	}
	for _, mf := range families {
		if mf.GetName() == "mail_sweep_last_run_unixtime" && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
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
			require.NoError(t, sweepRound(context.Background(), fake, cfg, tc.owns))
			got := fake.filters()
			require.Len(t, got, 1, "一轮只读一次候选")
			assert.Equal(t, tc.wantSQL, got[0].SQL())
			assert.Equal(t, tc.wantArgs, fmt.Sprint(got[0].Args()))
		})
	}
	assert.NotEqual(t, "mail-assetop", data.SweepWorkKind, "清理与附件重投各用各的类别键")
}

// 活性时刻写在这一轮结束时:一轮进行中还是上一轮的值;写进去的是结束时刻(原来 defer 在开头就求了 time.Now(),记的是开始时刻);
// 这一轮出错也写(出错有错误日志与排空 HOLD,不该同时报成循环卡死)。
func TestSweepRoundMarksTheEndOfEachFinishedRound(t *testing.T) {
	metrics.Register()
	cfg := Config{}.Defaults()
	metrics.MarkSweepRun(time.Unix(1000, 0))
	var startSecond int64
	var during float64
	fake := &fakeSweeper{during: func() {
		startSecond = time.Now().Unix()
		during, _ = sweepLastRun()
		// 睡到下一秒开头:开始与结束落在不同的秒里,才分得出写的是哪一个时刻。
		time.Sleep(time.Until(time.Unix(startSecond+1, 0)))
	}}
	require.NoError(t, sweepRound(context.Background(), fake, cfg, nil))
	assert.Equal(t, float64(1000), during, "一轮开始时不写活性时刻")
	end, ok := sweepLastRun()
	require.True(t, ok)
	assert.Greater(t, end, float64(startSecond), "写的是这一轮结束的时刻")

	metrics.MarkSweepRun(time.Unix(1000, 0))
	failing := &fakeSweeper{err: errors.New("database unavailable")}
	require.Error(t, sweepRound(context.Background(), failing, cfg, nil))
	value, ok := sweepLastRun()
	require.True(t, ok)
	assert.Greater(t, value, float64(1000), "这一轮出错也算跑完,照样写")
}

// 循环一起来就写一次(首轮之前有至多一个周期的随机抖动,不能等第一轮),退出时删掉序列(停掉的循环不算卡住)。
func TestRunMarksLivenessAtStartAndClearsItOnExit(t *testing.T) {
	metrics.Register()
	metrics.ClearSweepRun()
	_, ok := sweepLastRun()
	require.False(t, ok, "夹具前提:开始时没有序列")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, &fakeSweeper{}, Config{Interval: time.Hour}, nil)
	}()
	require.Eventually(t, func() bool { _, ok := sweepLastRun(); return ok }, 2*time.Second, 10*time.Millisecond,
		"循环一起来就写一次")
	cancel()
	<-done
	_, ok = sweepLastRun()
	assert.False(t, ok, "循环退出之后删掉序列")
}
