package constants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"shared/generated/pb/table"
	"shared/generated/tip"
	"shared/serverbase"
)

// 公会 tip 码的回归护栏。
//
// # 这组测试守的是什么
//
// 公会码曾经从 1 开始手写,与 common 段(kInvalidTableId=2 / kInvalidTableData=3 /
// kIndexOutOfRange=8)整段重叠 —— 客户端按 id 查提示表会显示完全无关的文案。
// 当时的修法是手写一个私有段 [200,219] 并用 AST 扫描钉住「常量都在段内」。
//
// 2026-09-02 起改成配表统一发号(见 constants.go 的注释与
// docs/design/tip-code-axis.md),于是这组测试的重点也变了:
//
//	旧:断言手写的数字落在手写的段内           —— 守一个「约定」
//	新:断言这里根本没有手写的数字             —— 守「机制」本身
//
// 第二条更强:只要没人再手写数字,撞号就不可能发生,因为发号器按段分配并自检。

// tipCodes 是本包对外提供的全部 tip 码。
// 新增码时在这里补一行 —— 漏补会被 TestNoHandWrittenTipCodes 之外的
// 覆盖率检查放过,但至少 constants.go 里的引用本身仍由发号器保证正确。
func tipCodes() map[string]uint32 {
	return map[string]uint32{
		"ErrAlreadyInGuild":   ErrAlreadyInGuild,
		"ErrGuildNotFound":    ErrGuildNotFound,
		"ErrNotInGuild":       ErrNotInGuild,
		"ErrGuildFull":        ErrGuildFull,
		"ErrLeaderCantLeave":  ErrLeaderCantLeave,
		"ErrNotLeader":        ErrNotLeader,
		"ErrNoPermission":     ErrNoPermission,
		"ErrNotRanked":        ErrNotRanked,
		"ErrIDGenUnavailable": ErrIDGenUnavailable,
	}
}

// guildSegment 从生成的段表里取公会段。段表源头是 data/tip/Tip.xlsx 的组头行。
func guildSegment(t *testing.T) tip.Segment {
	t.Helper()
	for _, s := range tip.Segments {
		if s.Domain == "guild" {
			return s
		}
	}
	t.Fatal("生成的段表里没有 guild 段:Tip.xlsx 的 //guild_error 组是不是被删了?")
	return tip.Segment{}
}

// TestTipCodesStayInGuildSegment 钉死「公会 tip 码只能落在自己的段里」。
//
// 改造后这条由发号器在生成期保证,本测试是编译进二进制之后的第二道网:
// 万一有人绕过配表直接写了个数字,这里会红。
func TestTipCodesStayInGuildSegment(t *testing.T) {
	seg := guildSegment(t)
	for name, code := range tipCodes() {
		if code < seg.Base || code >= seg.Base+seg.Width {
			t.Errorf("%s = %d 落在公会段 [%d,%d) 之外(域=%s)",
				name, code, seg.Base, seg.Base+seg.Width, serverbase.TipDomain(code))
		}
	}
}

// TestTipCodesAreRecognizedByServerbase 守住「码进了配表就不该再被判成未知」。
//
// 改造前公会码在 serverbase 眼里全是 VerdictUnknown(它只认 1..129),
// 所以每次公会业务拒绝都会刷一条 rpc_inband_unknown_code。
// 进配表之后必须是正常的业务拒绝或故障。
func TestTipCodesAreRecognizedByServerbase(t *testing.T) {
	for name, code := range tipCodes() {
		if got := serverbase.TipVerdict(code); got == serverbase.VerdictUnknown {
			t.Errorf("%s = %d 仍被 serverbase 判成 VerdictUnknown —— 段表没跟上配表?", name, code)
		}
	}
}

// TestTipCodesDoNotCollideWithCommonSegment 是最初那次事故的锚点用例。
// 抽查几个当年真实撞上的 common 码,写死在这里。
func TestTipCodesDoNotCollideWithCommonSegment(t *testing.T) {
	collided := []struct {
		name string
		code uint32
	}{
		{"kInvalidTableId", uint32(table.CommonError_kInvalidTableId)},
		{"kInvalidTableData", uint32(table.CommonError_kInvalidTableData)},
		{"kIndexOutOfRange", uint32(table.CommonError_kIndexOutOfRange)},
		{"kSuccess", uint32(table.CommonError_kSuccess)},
	}
	for name, code := range tipCodes() {
		for _, c := range collided {
			if code == c.code {
				t.Errorf("%s = %d 与 common 段的 %s 完全相同", name, code, c.name)
			}
		}
	}
}

// TestTipCodesAreUnique 防止段内自撞:两个不同语义的错误共用一个 id,
// 客户端只会看到其中一个的文案。
func TestTipCodesAreUnique(t *testing.T) {
	seen := map[uint32]string{}
	for name, code := range tipCodes() {
		if prev, dup := seen[code]; dup {
			t.Errorf("tip 码 %d 被 %s 与 %s 同时使用", code, prev, name)
		}
		seen[code] = name
	}
}

// TestNoHandWrittenTipCodes 是这次改造真正的护栏:constants.go 里不许再出现
// 「Err* = 整数字面量」。
//
// 手写数字正是所有撞号的源头 —— 它绕开发号器,于是段、重名、文案三道保证全部失效。
// 正确写法是 `ErrX = uint32(table.GuildError_kGuildX)`,数字由配表发。
func TestNoHandWrittenTipCodes(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "constants.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 constants.go 失败: %v", err)
	}

	want := tipCodes()
	seen := make(map[string]struct{}, len(want))
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "Err") {
				continue
			}
			seen[name.Name] = struct{}{}
			if i >= len(vs.Values) {
				t.Errorf("%s 必须显式写生成枚举引用,不能省略 RHS 继承上一常量", name.Name)
				continue
			}
			if !isGeneratedTipRef(vs.Values[i], "GuildError_") {
				t.Errorf("%s 必须直接写成 uint32(table.GuildError_kGuildX),不能手写数字、别名或引用其他码轴",
					name.Name)
			}
		}
		return true
	})
	for name := range want {
		if _, ok := seen[name]; !ok {
			t.Errorf("tipCodes 声明了 %s,但 constants.go 没有对应 Err 常量", name)
		}
	}
	for name := range seen {
		if _, ok := want[name]; !ok {
			t.Errorf("constants.go 新增了 %s,必须同步纳入 tipCodes 的全量护栏", name)
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

// TestTipClassifier 覆盖定性函数的两条分支:本域故障码、其余交回全局。
func TestTipClassifier(t *testing.T) {
	c := TipClassifier()

	if got := c(ErrIDGenUnavailable); got != serverbase.VerdictFault {
		t.Errorf("发号器不可用应判 VerdictFault, 实际 %v", got)
	}
	if got := c(ErrGuildFull); got != serverbase.VerdictBizReject {
		t.Errorf("公会已满应判 VerdictBizReject, 实际 %v", got)
	}
	// 段外的码交回全局判定:common 段的服务不可用是故障。
	if got := c(uint32(table.CommonError_kServiceUnavailable)); got != serverbase.VerdictFault {
		t.Errorf("common 段的服务不可用应判 VerdictFault, 实际 %v", got)
	}
	// 落在所有段之外的码仍应是 VerdictUnknown。
	if got := c(100000); got != serverbase.VerdictUnknown {
		t.Errorf("轴外码应判 VerdictUnknown, 实际 %v", got)
	}
}
