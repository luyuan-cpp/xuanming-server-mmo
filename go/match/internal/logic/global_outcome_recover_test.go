package logic

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"match/internal/authority"
)

// 2026-10-30:到期终局恢复从撮合轮 runGlobalRound 挪到独立循环 match.global.outcome.recover。
// 2026-11-05:按 battle_id 分车道,吞吐随副本数增长;全量页(退休旧调度 + 不分车道一页)每圈只跑一次(global_outcome_recover.go)。

// recovererCall 记一次调用:full=true 是全量页 RecoverExpiredOutcomes,否则是车道页。
type recovererCall struct {
	full        bool
	lanes, lane uint64
	cursor      authority.ExpiredOutcomeCursor
}

type recovererReply struct {
	next authority.ExpiredOutcomeCursor
	err  error
}

// recovererFake 按调用顺序分别回放车道页与全量页的预置回包;预置用完之后回「这一页是空的」。
type recovererFake struct {
	calls       []recovererCall
	laneReplies []recovererReply
	fullReplies []recovererReply
	lanePages   int
	fullPages   int
	deadlines   []time.Duration
}

func (f *recovererFake) budget(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Hour)
	}
	f.deadlines = append(f.deadlines, time.Until(deadline))
}

func (f *recovererFake) RecoverExpiredOutcomeLane(ctx context.Context, lanes, lane uint64, cursor authority.ExpiredOutcomeCursor) (authority.ExpiredOutcomeCursor, error) {
	f.calls = append(f.calls, recovererCall{lanes: lanes, lane: lane, cursor: cursor})
	f.budget(ctx)
	i := f.lanePages
	f.lanePages++
	if i >= len(f.laneReplies) {
		return authority.ExpiredOutcomeCursor{}, nil
	}
	return f.laneReplies[i].next, f.laneReplies[i].err
}

func (f *recovererFake) RecoverExpiredOutcomes(ctx context.Context, afterRetryMs, afterBattle uint64) (uint64, uint64, error) {
	f.calls = append(f.calls, recovererCall{full: true, cursor: authority.ExpiredOutcomeCursor{AfterRetryMs: afterRetryMs, AfterBattle: afterBattle}})
	f.budget(ctx)
	i := f.fullPages
	f.fullPages++
	if i >= len(f.fullReplies) {
		return 0, 0, nil
	}
	return f.fullReplies[i].next.AfterRetryMs, f.fullReplies[i].next.AfterBattle, f.fullReplies[i].err
}

// fixedLaneOrder 让每一圈都按 0,1,2,… 走车道,用例不依赖随机数。
func fixedLaneOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	return order
}

// 游标语义与分车道之前相同,只是每条车道各一份:每页从这条车道上一页的返回值接着翻,出错也按返回值推进
// (单条损坏不卡整页),扫到末尾回零值;每拍有 3 秒预算;未确认时 HoldRelay 并打 ERROR。
func TestGlobalOutcomeRecoveryKeepsPerLaneCursorAcrossRounds(t *testing.T) {
	logs := logtest.NewCollector(t)
	broken := errors.New("fixture 原件损坏")
	lanes := authority.ExpiredOutcomeLanes
	order := 0
	recovery := &globalOutcomeRecovery{shuffle: func(int) []int {
		order++
		return []int{3, 5} // 每「圈」只排两条车道,便于看清同一车道的游标跨圈接续
	}}
	fake := &recovererFake{laneReplies: []recovererReply{
		{next: authority.ExpiredOutcomeCursor{AfterRetryMs: 100, AfterBattle: 7}},              // 第 1 拍:车道 3
		{next: authority.ExpiredOutcomeCursor{AfterRetryMs: 250, AfterBattle: 9}, err: broken}, // 第 2 拍:车道 5,未确认
		{next: authority.ExpiredOutcomeCursor{}},                                               // 第 3 拍:车道 3 翻到末尾
	}}
	var held []error
	for range 3 {
		recovery.round(context.Background(), fake, func(err error) { held = append(held, err) })
	}
	var lanePages []recovererCall
	for _, call := range fake.calls {
		if !call.full {
			lanePages = append(lanePages, call)
		}
	}
	require.Equal(t, []recovererCall{
		{lanes: lanes, lane: 3},
		{lanes: lanes, lane: 5},
		{lanes: lanes, lane: 3, cursor: authority.ExpiredOutcomeCursor{AfterRetryMs: 100, AfterBattle: 7}},
	}, lanePages, "同一车道从它上一页的返回值接着翻")
	require.Equal(t, authority.ExpiredOutcomeCursor{}, recovery.lanes[3], "扫到末尾回到起点")
	require.Equal(t, authority.ExpiredOutcomeCursor{AfterRetryMs: 250, AfterBattle: 9}, recovery.lanes[5], "出错也推进")
	require.Equal(t, 2, order, "一圈走完才重新洗牌")
	require.Len(t, held, 1, "只有未确认的那一拍 Hold")
	require.ErrorIs(t, held[0], broken)
	require.Contains(t, logs.String(), "全球匹配到期终局恢复未确认")
	for _, budget := range fake.deadlines {
		require.LessOrEqual(t, budget, globalOutcomeRecoverBudget)
		require.Greater(t, budget, time.Duration(0))
	}
}

// 全量页(退休旧调度 + 不分车道一页)要走完在途的 state 4 行,成本与积压无关,只在每圈第一拍跑一次;其余拍只有车道页。
// 评审修正前,积压为空时车道页总是不满,每拍都补一页全量页,再加两次退休扫描,副本越多 TiDB 常态负载越高。
// 全量页有自己的游标,跨圈接续;出错同样 Hold。
func TestGlobalOutcomeRecoveryRunsFullPageOncePerCycle(t *testing.T) {
	broken := errors.New("fixture 全量页未确认")
	recovery := &globalOutcomeRecovery{shuffle: fixedLaneOrder}
	fake := &recovererFake{fullReplies: []recovererReply{
		{next: authority.ExpiredOutcomeCursor{AfterRetryMs: 20, AfterBattle: 2}},
		{next: authority.ExpiredOutcomeCursor{AfterRetryMs: 30, AfterBattle: 3}, err: broken},
	}}
	lanes := int(authority.ExpiredOutcomeLanes)
	var held []error
	for range 2 * lanes {
		recovery.round(context.Background(), fake, func(err error) { held = append(held, err) })
	}
	require.Equal(t, 2*lanes, fake.lanePages, "每拍一页车道页")
	require.Equal(t, 2, fake.fullPages, "全量页每圈一次,积压为空时也不多跑")
	var full []recovererCall
	for i, call := range fake.calls {
		if call.full {
			full = append(full, call)
			require.Greater(t, i, 0, "全量页不能先于车道页")
			require.False(t, fake.calls[i-1].full, "全量页紧跟在那一圈第一拍的车道页之后")
			require.EqualValues(t, 0, fake.calls[i-1].lane, "固定顺序下每圈第一拍是车道 0")
		}
	}
	require.Equal(t, []recovererCall{
		{full: true},
		{full: true, cursor: authority.ExpiredOutcomeCursor{AfterRetryMs: 20, AfterBattle: 2}},
	}, full, "全量页从上一圈的返回值接着翻")
	require.Equal(t, authority.ExpiredOutcomeCursor{AfterRetryMs: 30, AfterBattle: 3}, recovery.overall, "出错也推进")
	require.Len(t, held, 1)
	require.ErrorIs(t, held[0], broken)
}

// 车道页与全量页互不依赖:每圈第一拍的车道页出错,同一拍的全量页照跑,两份错误一起 Hold。
func TestGlobalOutcomeRecoveryFullPageRunsEvenIfLanePageFails(t *testing.T) {
	laneBroken := errors.New("fixture 车道页未确认")
	fullBroken := errors.New("fixture 全量页未确认")
	recovery := &globalOutcomeRecovery{shuffle: fixedLaneOrder}
	fake := &recovererFake{
		laneReplies: []recovererReply{{next: authority.ExpiredOutcomeCursor{AfterRetryMs: 5, AfterBattle: 5}, err: laneBroken}},
		fullReplies: []recovererReply{{err: fullBroken}},
	}
	var held []error
	recovery.round(context.Background(), fake, func(err error) { held = append(held, err) })
	require.Equal(t, 1, fake.lanePages)
	require.Equal(t, 1, fake.fullPages)
	require.Len(t, held, 1)
	require.ErrorIs(t, held[0], laneBroken)
	require.ErrorIs(t, held[0], fullBroken)
}

// 每个副本每圈恰好把每条车道访问一遍(用真随机排列),所以任一车道的访问间隔不超过两圈;全量页每圈一次。
// 两个副本各自洗牌,这里不断言它们的重合率(随机),只断言各自的覆盖。
func TestGlobalOutcomeRecoveryVisitsEveryLaneOncePerCycle(t *testing.T) {
	lanes := int(authority.ExpiredOutcomeLanes)
	for _, recovery := range []*globalOutcomeRecovery{{}, {}} {
		fake := &recovererFake{}
		for range 3 * lanes {
			recovery.round(context.Background(), fake, func(error) {})
		}
		var visited []uint64
		for _, call := range fake.calls {
			if !call.full {
				require.Equal(t, authority.ExpiredOutcomeLanes, call.lanes)
				visited = append(visited, call.lane)
			}
		}
		require.Len(t, visited, 3*lanes)
		require.Equal(t, 3, fake.fullPages)
		for cycle := range 3 {
			seen := map[uint64]bool{}
			for _, lane := range visited[cycle*lanes : (cycle+1)*lanes] {
				require.Less(t, lane, authority.ExpiredOutcomeLanes)
				require.False(t, seen[lane], "一圈内车道不重复")
				seen[lane] = true
			}
			require.Len(t, seen, lanes, "一圈走遍全部车道")
		}
	}
}

// 撮合轮 runGlobalRound 不再调用到期恢复,恢复只由 StartMatcherLoop 装配的独立循环跑。两条都要守住:
// 挪回撮合轮会让两个 goroutine 共用游标(数据竞争)、恢复变慢拖住 Pending;独立循环漏装配则过截止的成局再没有出口。
// runGlobalRound 依赖真实 TiDB(Pending),这里按源码结构机械核对(同 team_logic_layering_test.go 的做法)。
func TestGlobalRoundNoLongerRunsExpiredOutcomeRecovery(t *testing.T) {
	fset := token.NewFileSet()
	parse := func(name string) *ast.File {
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "解析 %s 失败;文件改名就同步改这里", name)
		return file
	}
	var round *ast.FuncDecl
	for _, decl := range parse("global_match.go").Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "runGlobalRound" {
			round = fn
		}
	}
	require.NotNil(t, round, "global_match.go 里找不到 runGlobalRound;改名就同步改这里")
	ast.Inspect(round.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "RecoverExpiredOutcomes", "RecoverExpiredOutcome", "RecoverExpiredOutcomeLane":
				t.Errorf("runGlobalRound 在 %s 又调用了 %s:到期恢复只能在 match.global.outcome.recover 循环里跑",
					fset.Position(sel.Pos()), sel.Sel.Name)
			}
		}
		return true
	})

	registered := false
	ast.Inspect(parse("matcher.go"), func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "StartRelayLoop" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if name, err := strconv.Unquote(lit.Value); err == nil && name == "match.global.outcome.recover" {
					registered = true
				}
			}
		}
		return true
	})
	require.True(t, registered, "StartMatcherLoop 没有装配 match.global.outcome.recover 循环:过截止的成局将没有出口")
}
