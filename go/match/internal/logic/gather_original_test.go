package logic

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
	"match/internal/authority"
)

// 2026-11-05(全球局终局协议评审修正):gather 组 Create 原件的两处取值要有会转红的回归。
// 真正跑 runFormationGather 要真 TiDB 加桩 scene / battle,这里测抽出的纯函数,再按源码结构核对 gather 确实用了它们
// (同 global_outcome_recover_test.go 的 AST 守卫做法)。

// 本机墙钟慢于 TiDB 时取成局时刻,快于时取本机时刻:原件创建时刻永远不早于成局时刻,落库前的 completableOriginal 才放行。
func TestCreateOriginalCreatedAtMsNeverPrecedesFormation(t *testing.T) {
	const formation = uint64(1_800_000_000_000)
	require.Equal(t, formation, createOriginalCreatedAtMs(formation-5_000, formation), "本机慢 5 秒:取成局时刻")
	require.Equal(t, formation, createOriginalCreatedAtMs(formation, formation))
	require.Equal(t, formation+20, createOriginalCreatedAtMs(formation+20, formation), "本机快:取本机时刻")
}

// 原件落库前被判不可裁决单独计 original_unterminable(值班告警看它);落库事务本身的失败仍是 intent_failed,TiDB 抖动不误报。
func TestIntentFailureOutcomeSeparatesUnterminableOriginal(t *testing.T) {
	rejected := fmt.Errorf("%w: 持久创建原件与终局身份冲突", authority.ErrUnterminableOriginal)
	require.Equal(t, "original_unterminable", intentFailureOutcome(rejected))
	require.Equal(t, "intent_failed", intentFailureOutcome(errors.New("fixture: TiDB 事务提交失败")))
	require.Equal(t, "intent_failed", intentFailureOutcome(authority.ErrWorkLease))
}

// runFormationGather 组 Create 原件时,CreatedAtMs 必须取 createOriginalCreatedAtMs 的结果,SaveSupportedIntent 的失败必须经
// intentFailureOutcome 计数。把前者改回 nowMs(),本机慢于 TiDB 时全部成局都会在落库前被拒、干净回队却永远开不了局;
// 把后者改回固定的 intent_failed,时钟偏差告警就会被 TiDB 抖动淹没。
func TestGatherUsesCreateOriginalHelpers(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "gather.go", nil, 0)
	require.NoError(t, err, "解析 gather.go 失败;文件改名就同步改这里")
	var gather *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "runFormationGather" {
			gather = fn
		}
	}
	require.NotNil(t, gather, "gather.go 里找不到 runFormationGather;改名就同步改这里")

	callsTo := func(n ast.Node, name string) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		fn, ok := call.Fun.(*ast.Ident)
		return ok && fn.Name == name
	}
	stamped := map[string]bool{} // 由 createOriginalCreatedAtMs 赋值的局部变量
	ast.Inspect(gather.Body, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 && callsTo(assign.Rhs[0], "createOriginalCreatedAtMs") {
			if id, ok := assign.Lhs[0].(*ast.Ident); ok {
				stamped[id.Name] = true
			}
		}
		return true
	})
	createdAtFromHelper, createdAtFields := false, 0
	outcomeFromHelper := false
	ast.Inspect(gather.Body, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "CreatedAtMs" {
				createdAtFields++
				if id, ok := kv.Value.(*ast.Ident); ok && stamped[id.Name] || callsTo(kv.Value, "createOriginalCreatedAtMs") {
					createdAtFromHelper = true
				}
			}
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "fail" && len(call.Args) > 0 && callsTo(call.Args[0], "intentFailureOutcome") {
				outcomeFromHelper = true
			}
		}
		return true
	})
	require.Equal(t, 1, createdAtFields, "runFormationGather 只应组一份 Create 原件;改了结构就同步改这里")
	require.True(t, createdAtFromHelper, "Create 原件的 CreatedAtMs 必须取 createOriginalCreatedAtMs(本机时刻与成局时刻取大)")
	require.True(t, outcomeFromHelper, "SaveSupportedIntent 的失败必须经 intentFailureOutcome 区分 original_unterminable 与 intent_failed")
}
