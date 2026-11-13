package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// 截止作废的取证(logic.BindDeadlineAbortEvidence,2026-11-06,cross-zone-matchmaking.md §13.3)必须在起后台循环、注册 RPC
// 之前接到成局权威上:没接时一律判不了,截止 ABORT 只在硬上限之后才写 —— 不报错,只是每一局作废都让全员多占约 10 分钟。
// stable 与 canary 都要接(帮会取消是 RPC)。按源码结构机械核对 match_service.go 的 main:删掉这次调用,或挪到
// StartMatcherLoop / InitGatherSlots / zrpc.MustNewServer 之后,用例转红。
func TestMainBindsDeadlineAbortEvidenceBeforeLoopsAndRPC(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "match_service.go", nil, 0)
	require.NoError(t, err, "解析 match_service.go 失败;文件改名就同步改这里")
	var entry *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			entry = fn
		}
	}
	require.NotNil(t, entry, "match_service.go 里找不到 main")
	first := map[string]token.Pos{}
	ast.Inspect(entry, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if _, seen := first[sel.Sel.Name]; !seen {
				first[sel.Sel.Name] = call.Pos()
			}
		}
		return true
	})
	bind, ok := first["BindDeadlineAbortEvidence"]
	require.True(t, ok, "main 不再调 BindDeadlineAbortEvidence:截止作废拿不到任何证据")
	for _, later := range []string{"StartMatcherLoop", "InitGatherSlots", "MustNewServer"} {
		pos, ok := first[later]
		require.True(t, ok, "main 里找不到 %s;改了写法就同步改这里", later)
		require.Less(t, int(bind), int(pos), "取证要在 %s 之前接好", later)
	}
}

// 原执行进程失效的提前作废(logic.StartBattleInstanceLossWatcher,2026-11-11,battle-instance-loss-early-abort.md §4)的取证循环
// 两条轨道都要起:可疑表只由本副本收到的补签驱动,全员金丝雀的对局只会向 canary 副本补签。没起时不报错,只是 battle 崩溃后
// 全员要等到截止才放人。按源码结构机械核对:main 必须调它,而且不能放在只给 stable 走的 `if globalBackground` 里
// (含它的 else 分支);还要排在 BindDeadlineAbortEvidence(装配)之后、MustNewServer(开始收补签)之前。
func TestMainStartsBattleInstanceLossWatcherOnEveryTrack(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "match_service.go", nil, 0)
	require.NoError(t, err, "解析 match_service.go 失败;文件改名就同步改这里")
	var entry *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			entry = fn
		}
	}
	require.NotNil(t, entry, "match_service.go 里找不到 main")
	first := map[string]token.Pos{}
	var trackOnly []*ast.IfStmt
	ast.Inspect(entry, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if _, seen := first[sel.Sel.Name]; !seen {
					first[sel.Sel.Name] = node.Pos()
				}
			}
		case *ast.IfStmt:
			if cond, ok := node.Cond.(*ast.Ident); ok && cond.Name == "globalBackground" {
				trackOnly = append(trackOnly, node)
			}
		}
		return true
	})
	start, ok := first["StartBattleInstanceLossWatcher"]
	require.True(t, ok, "main 不再调 StartBattleInstanceLossWatcher:battle 崩溃后的对局只能等截止作废")
	require.NotEmpty(t, trackOnly, "main 里找不到 if globalBackground;改了写法就同步改这里")
	for _, branch := range trackOnly {
		require.False(t, branch.Pos() <= start && start < branch.End(), "取证循环不能只在一条轨道上起")
	}
	bind, ok := first["BindDeadlineAbortEvidence"]
	require.True(t, ok)
	require.Less(t, int(bind), int(start), "先装配(BindDeadlineAbortEvidence)再起取证循环")
	server, ok := first["MustNewServer"]
	require.True(t, ok, "main 里找不到 MustNewServer;改了写法就同步改这里")
	require.Less(t, int(start), int(server), "取证循环要在开始收补签之前起好")
}
