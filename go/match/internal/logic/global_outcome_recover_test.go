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
)

// 2026-10-30:到期终局恢复从撮合轮 runGlobalRound 挪到独立循环 match.global.outcome.recover。

type recovererCall struct{ afterRetryMs, afterBattle uint64 }

type recovererFake struct {
	calls     []recovererCall
	returns   []recovererCall
	errs      []error
	deadlines []time.Duration
}

func (f *recovererFake) RecoverExpiredOutcomes(ctx context.Context, afterRetryMs, afterBattle uint64) (uint64, uint64, error) {
	i := len(f.calls)
	f.calls = append(f.calls, recovererCall{afterRetryMs, afterBattle})
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Hour)
	}
	f.deadlines = append(f.deadlines, time.Until(deadline))
	return f.returns[i].afterRetryMs, f.returns[i].afterBattle, f.errs[i]
}

// 游标语义与挪出撮合轮之前相同:每页从上一页的返回值接着翻,出错也按返回值推进(单条损坏不卡整页),
// 扫到末尾回 0;每页有 3 秒预算;未确认时 HoldRelay 并打 ERROR,成功时不 Hold。
func TestGlobalOutcomeRecoveryKeepsCursorAcrossRounds(t *testing.T) {
	logs := logtest.NewCollector(t)
	broken := errors.New("fixture 原件损坏")
	fake := &recovererFake{
		returns: []recovererCall{{100, 7}, {250, 9}, {0, 0}},
		errs:    []error{nil, broken, nil},
	}
	var held []error
	recovery := &globalOutcomeRecovery{}
	for range 3 {
		recovery.round(context.Background(), fake, func(err error) { held = append(held, err) })
	}
	require.Equal(t, []recovererCall{{0, 0}, {100, 7}, {250, 9}}, fake.calls, "每页从上一页的返回值接着翻")
	require.Equal(t, globalOutcomeRecovery{}, *recovery, "扫到末尾回到起点")
	require.Equal(t, []error{broken}, held, "只有未确认的那一页 Hold")
	require.Contains(t, logs.String(), "全球匹配到期终局恢复未确认")
	for _, budget := range fake.deadlines {
		require.LessOrEqual(t, budget, globalOutcomeRecoverBudget)
		require.Greater(t, budget, time.Duration(0))
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
			case "RecoverExpiredOutcomes", "RecoverExpiredOutcome":
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
