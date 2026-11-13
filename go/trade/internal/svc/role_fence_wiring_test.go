package svc

// 角色护栏必须无条件装配(docs/design/gate-online-identity.md §9.3「读路径归属」):入口归属预检覆盖每个客户端请求,
// 资产通道关闭(AssetOp.Enabled=false)时浏览、详情、收藏照常服务,同样要按护栏副本核归属。
// 以前护栏只在资产通道开启时才建;挪回 `if assets != nil` 里面,通道关闭时 ServiceContext.Roles 是 nil,
// 预检对每个客户端请求都判「不归」(fail-closed),整个聚宝斋对玩家不可用,而且不报任何启动错误。
// 按源码结构机械核对 NewServiceContextWithAdmission:commerce.NewRoleAuthority 恰好调用一次、不在任何条件或循环之内,
// ServiceContext 字面量给 Roles 赋值。改了写法就同步改这里。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestRoleAuthorityIsWiredUnconditionally(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "servicecontext.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 servicecontext.go 失败(文件改名就同步改这里): %v", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Recv == nil && f.Name.Name == "NewServiceContextWithAdmission" {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("servicecontext.go 里找不到 NewServiceContextWithAdmission")
	}

	calls := 0
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewRoleAuthority" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "commerce" {
			return true
		}
		calls++
		for _, outer := range stack[:len(stack)-1] {
			switch outer.(type) {
			case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.FuncLit:
				t.Errorf("commerce.NewRoleAuthority 在 %s 里面(%s):护栏要无条件装配,资产通道关闭时预检同样要用它",
					fset.Position(outer.Pos()), describeNode(outer))
			}
		}
		return true
	})
	if calls != 1 {
		t.Fatalf("NewServiceContextWithAdmission 里 commerce.NewRoleAuthority 应恰好调用一次,实际 %d 次", calls)
	}

	assigned := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "ServiceContext" {
			return true
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Roles" {
					assigned = true
				}
			}
		}
		return true
	})
	if !assigned {
		t.Fatal("ServiceContext 字面量没有给 Roles 赋值:入口归属预检会拿到 nil,所有客户端请求被拒")
	}
}

func describeNode(n ast.Node) string {
	switch n.(type) {
	case *ast.IfStmt:
		return "if"
	case *ast.ForStmt, *ast.RangeStmt:
		return "for"
	case *ast.SwitchStmt, *ast.TypeSwitchStmt:
		return "switch"
	case *ast.SelectStmt:
		return "select"
	case *ast.FuncLit:
		return "func literal"
	}
	return "block"
}
