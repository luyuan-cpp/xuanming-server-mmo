package main

// TestMirroredRouteScriptsMatchShared:route_publish.go / route_store.go 镜像的 Redis 契约必须与 go/shared/placement 逐字节相同。
// merge_zone 不 import shared/placement,两份代码只能靠这个用例钉住:用 go/parser 读 ../../go/shared/placement/route.go 与
// route_ready.go 的字符串 / 数值常量,逐个与本模块的镜像比较。shared 那边改了脚本或常量而这里没跟,这个用例就红 —— 两份一旦分叉,
// 工具写下的标记值会与 data_service 的判定口径不一致,而且不会报在任何一侧。shared 文件读不到、常量改了名,一律 FAIL
// (不 SKIP:守卫不能悄悄失效)。

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// sharedPlacementDir 是 go/shared/placement 相对本包目录(tools/merge_zone)的位置。
const sharedPlacementDir = "../../go/shared/placement"

func TestMirroredRouteScriptsMatchShared(t *testing.T) {
	consts := loadSharedPlacementConsts(t, "route.go", "route_ready.go")

	for _, c := range []struct {
		shared, mirrorName, mirror string
	}{
		{"RouteReadyCompareAndDeleteScript", "routeCompareAndDeleteScript", routeCompareAndDeleteScript},
		{"RouteWatermarkKey", "routeWatermarkKey", routeWatermarkKey},
		{"MergedIntoKeyPrefix", "mergedIntoKey (prefix)", strings.TrimSuffix(mergedIntoKey(7), "7")},
	} {
		v, ok := consts[c.shared]
		if !ok {
			t.Fatalf("shared/placement constant %s not found (renamed?): update %s and this test together", c.shared, c.mirrorName)
		}
		if v.Kind() != constant.String {
			t.Fatalf("shared/placement %s is %v, want a string constant", c.shared, v.Kind())
		}
		if got := constant.StringVal(v); got != c.mirror {
			t.Fatalf("%s drifted from shared/placement.%s:\n%s", c.mirrorName, c.shared, firstDiff(got, c.mirror))
		}
	}

	for _, c := range []struct {
		shared, mirrorName string
		mirror             int64
	}{
		{"RouteRowShards", "routeRowShards", int64(routeRowShards)},
		{"RouteWatermarkFormatVersion", "routeWatermarkFormatVersion", routeWatermarkFormatVersion},
		{"routeReadyGenerationMaxLen", "routeGenerationMaxLen", routeGenerationMaxLen},
	} {
		v, ok := consts[c.shared]
		if !ok {
			t.Fatalf("shared/placement constant %s not found (renamed?): update %s and this test together", c.shared, c.mirrorName)
		}
		got, exact := constant.Int64Val(constant.ToInt(v))
		if !exact || got != c.mirror {
			t.Fatalf("%s = %d drifted from shared/placement.%s = %v", c.mirrorName, c.mirror, c.shared, v)
		}
	}
}

// loadSharedPlacementConsts 解析 shared/placement 的若干文件,求出其中能在本地求值的包级常量
// (字面量、括号、+ - * 与引用同批常量;iota 与类型转换求不出,跳过)。
func loadSharedPlacementConsts(t *testing.T, files ...string) map[string]constant.Value {
	t.Helper()
	pending := map[string]ast.Expr{}
	fset := token.NewFileSet()
	for _, name := range files {
		path := filepath.Join(sharedPlacementDir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v (the mirror guard must not silently pass)", path, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						pending[n.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	known := map[string]constant.Value{}
	for progress := true; progress; {
		progress = false
		for name, expr := range pending {
			if v, ok := evalSharedConst(expr, known); ok {
				known[name] = v
				delete(pending, name)
				progress = true
			}
		}
	}
	return known
}

func evalSharedConst(e ast.Expr, known map[string]constant.Value) (constant.Value, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(x.Value, x.Kind, 0)
		return v, v.Kind() != constant.Unknown
	case *ast.ParenExpr:
		return evalSharedConst(x.X, known)
	case *ast.Ident:
		v, ok := known[x.Name]
		return v, ok
	case *ast.BinaryExpr:
		switch x.Op {
		case token.ADD, token.SUB, token.MUL:
		default:
			return nil, false
		}
		a, okA := evalSharedConst(x.X, known)
		b, okB := evalSharedConst(x.Y, known)
		if !okA || !okB {
			return nil, false
		}
		return constant.BinaryOp(a, x.Op, b), true
	}
	return nil, false
}

// firstDiff 给出两段文本第一处不同的位置与上下文(脚本很长,整段打印没法看)。
func firstDiff(got, want string) string {
	n := min(len(got), len(want))
	i := 0
	for i < n && got[i] == want[i] {
		i++
	}
	lo := max(0, i-40)
	return "first difference at byte " + itoa(i) + "\n shared: …" + clip(got, lo, i+40) + "\n mirror: …" + clip(want, lo, i+40)
}

func clip(s string, lo, hi int) string {
	return s[min(lo, len(s)):min(hi, len(s))]
}

func itoa(i int) string {
	return constant.MakeInt64(int64(i)).ExactString()
}
