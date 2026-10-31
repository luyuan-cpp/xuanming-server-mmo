package main

// TestIntegrationTestsCallRequireIT:带 merge_integration 构建标签的测试文件里,每个 Test*(TestMain 除外)的第一句必须是
// requireIT(t)(D1 规格 §13.3 第 7 条)。漏了它的用例在依赖不可达时既不 SKIP、也不按 MERGE_ZONE_IT_REQUIRE=1 FAIL,
// 而是一头撞上不可达的 MySQL / Redis,结论取决于它碰巧先做了什么 —— PASS / FAIL / SKIP 又分不清了。
// 同时钉住 TestMain 不再用 os.Exit(0) 吞掉「依赖不可达」(那会让 SKIP 与 PASS 的退出码相同)。
//
// 本文件不带构建标签:普通 `go test` 就会检查集成用例的源码(只解析,不编译它们)。

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationTestsCallRequireIT(t *testing.T) {
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob test files: %v", err)
	}
	var files, tests int
	for _, path := range paths {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if !isMergeIntegrationFile(f) {
			continue
		}
		files++
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "Test") {
				continue
			}
			if fd.Name.Name == "TestMain" {
				checkTestMainKeepsSkipDistinct(t, fset, fd)
				continue
			}
			tests++
			if !firstStmtIsRequireIT(fd) {
				t.Errorf("%s: %s must start with requireIT(t) (dependency missing → SKIP, or FAIL under %s=1)",
					fset.Position(fd.Pos()), fd.Name.Name, itRequireEnvName)
			}
		}
	}
	// integration_test.go、route_store_integration_test.go、team_authority_integration_test.go、tidb_fixture_integration_test.go。
	if files < 3 {
		t.Fatalf("found %d merge_integration test files, want at least 3: the build-tag detection is broken", files)
	}
	if tests == 0 {
		t.Fatal("no integration test functions found: the guard checks nothing")
	}
}

// itRequireEnvName 与 integration_test.go 的 itRequireEnv 同值(那个常量带 merge_integration 标签,普通构建里不存在)。
const itRequireEnvName = "MERGE_ZONE_IT_REQUIRE"

// isMergeIntegrationFile 报告文件的 //go:build 约束是否「只在 merge_integration 标签下才编译」。
func isMergeIntegrationFile(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				return false
			}
			withTag := expr.Eval(func(tag string) bool { return tag == "merge_integration" })
			withoutTags := expr.Eval(func(string) bool { return false })
			return withTag && !withoutTags
		}
	}
	return false
}

// firstStmtIsRequireIT 报告函数体的第一句是不是 requireIT(<第一个参数>)。
func firstStmtIsRequireIT(fd *ast.FuncDecl) bool {
	params := fd.Type.Params.List
	if fd.Body == nil || len(fd.Body.List) == 0 || len(params) != 1 || len(params[0].Names) != 1 {
		return false
	}
	es, ok := fd.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "requireIT" {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == params[0].Names[0].Name
}

// checkTestMainKeepsSkipDistinct:TestMain 里不得出现 os.Exit(0)。
func checkTestMainKeepsSkipDistinct(t *testing.T, fset *token.FileSet, fd *ast.FuncDecl) {
	t.Helper()
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Exit" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == "0" {
			t.Errorf("%s: TestMain must not os.Exit(0) — an unreachable dependency would look like PASS; record the reason and let requireIT skip",
				fset.Position(call.Pos()))
		}
		return true
	})
}
