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
