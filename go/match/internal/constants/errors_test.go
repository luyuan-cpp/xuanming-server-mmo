package constants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"shared/generated/tip"
	"shared/serverbase"
)

func tipCodes() map[string]uint32 {
	return map[string]uint32{
		"ErrInBattle":               ErrInBattle,
		"ErrAlreadyQueued":          ErrAlreadyQueued,
		"ErrModeNotOpen":            ErrModeNotOpen,
		"ErrTeamSizeNotConfigured":  ErrTeamSizeNotConfigured,
		"ErrInternal":               ErrInternal,
		"ErrTicketMismatch":         ErrTicketMismatch,
		"ErrCancelTooLate":          ErrCancelTooLate,
		"ErrChallengeSelf":          ErrChallengeSelf,
		"ErrChallengeTargetOffline": ErrChallengeTargetOffline,
		"ErrChallengeTargetBusy":    ErrChallengeTargetBusy,
		"ErrChallengeSelfBusy":      ErrChallengeSelfBusy,
		"ErrChallengePending":       ErrChallengePending,
		"ErrChallengeExpired":       ErrChallengeExpired,
		"ErrChallengeNotTarget":     ErrChallengeNotTarget,
		"ErrSpectateWhileQueued":    ErrSpectateWhileQueued,
		"ErrSpectateWhileInBattle":  ErrSpectateWhileInBattle,
		"ErrAlreadyWatching":        ErrAlreadyWatching,
		"ErrNoWatchableBattle":      ErrNoWatchableBattle,
		"ErrBattleNotWatchable":     ErrBattleNotWatchable,
		"ErrSpectateOffline":        ErrSpectateOffline,
		"ErrNotInScene":             ErrNotInScene,
	}
}

func matchSegment(t *testing.T) tip.Segment {
	t.Helper()
	for _, segment := range tip.Segments {
		if segment.Domain == "match" {
			return segment
		}
	}
	t.Fatal("生成的段表里没有 match 段")
	return tip.Segment{}
}

func TestTipCodesStayInMatchSegmentAndAreRecognized(t *testing.T) {
	segment := matchSegment(t)
	for name, code := range tipCodes() {
		if code < segment.Base || code >= segment.Base+segment.Width {
			t.Errorf("%s = %d 落在 match 段 [%d,%d) 之外", name, code,
				segment.Base, segment.Base+segment.Width)
		}
		if verdict := serverbase.TipVerdict(code); verdict == serverbase.VerdictUnknown {
			t.Errorf("%s = %d 未被生成段表识别", name, code)
		}
	}
}

func TestTipCodesAreUnique(t *testing.T) {
	seen := make(map[uint32]string)
	for name, code := range tipCodes() {
		if previous, exists := seen[code]; exists {
			t.Errorf("tip 码 %d 被 %s 与 %s 同时使用", code, previous, name)
		}
		seen[code] = name
	}
}

func TestNoHandWrittenTipCodes(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "errors.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 errors.go 失败: %v", err)
	}

	want := tipCodes()
	seen := make(map[string]struct{}, len(want))
	ast.Inspect(file, func(node ast.Node) bool {
		valueSpec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for index, name := range valueSpec.Names {
			if !strings.HasPrefix(name.Name, "Err") {
				continue
			}
			seen[name.Name] = struct{}{}
			if index >= len(valueSpec.Values) {
				t.Errorf("%s 必须显式写生成枚举引用,不能省略 RHS 继承上一常量", name.Name)
				continue
			}
			if !isGeneratedTipRef(valueSpec.Values[index], "MatchError_") {
				t.Errorf("%s 必须直接写成 uint32(table.MatchError_kMatchX),不能手写数字、别名或引用其他码轴",
					name.Name)
			}
		}
		return true
	})
	for name := range want {
		if _, ok := seen[name]; !ok {
			t.Errorf("tipCodes 声明了 %s,但 errors.go 没有对应 Err 常量", name)
		}
	}
	for name := range seen {
		if _, ok := want[name]; !ok {
			t.Errorf("errors.go 新增了 %s,必须同步纳入 tipCodes 的全量护栏", name)
		}
	}
}

func isGeneratedTipRef(expr ast.Expr, enumPrefix string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	convert, ok := call.Fun.(*ast.Ident)
	if !ok || convert.Name != "uint32" {
		return false
	}
	selector, ok := call.Args[0].(*ast.SelectorExpr)
	if !ok || !strings.HasPrefix(selector.Sel.Name, enumPrefix) {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "table"
}
