package constants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	tradepb "proto/trade"

	"shared/generated/tip"
	"shared/serverbase"
)

// tipCode 记录一个 Err 常量的码值与它应当落在的段。
type tipCode struct {
	code   uint32
	domain string // tip.Segment.Domain:"trade" 或 "common"
	fault  bool   // Tip.xlsx fault 列应为 1
}

// tipCodes 是本包 Err 常量的全量清单,TestNoHandWrittenTipCodes 双向核对,漏补即红。
func tipCodes() map[string]tipCode {
	return map[string]tipCode{
		"ErrInvalidParameter":     {ErrInvalidParameter, "common", false},
		"ErrServiceUnavailable":   {ErrServiceUnavailable, "common", true},
		"ErrListingNotFound":      {ErrListingNotFound, "trade", false},
		"ErrHomeZoneUnknown":      {ErrHomeZoneUnknown, "trade", false},
		"ErrFavoriteLimitReached": {ErrFavoriteLimitReached, "trade", false},
		"ErrFeatureDisabled":      {ErrFeatureDisabled, "trade", false},
	}
}

// allowedTipPrefixes:trade 只许引用这两条码轴(见 constants.go 包注释)。
var allowedTipPrefixes = []string{"TradeError_", "CommonError_"}

func segmentOf(t *testing.T, domain string) tip.Segment {
	t.Helper()
	for _, s := range tip.Segments {
		if s.Domain == domain {
			return s
		}
	}
	t.Fatalf("生成的段表里没有 %s 段:Tip.xlsx 的 //%s_error 组是不是没导表?", domain, domain)
	return tip.Segment{}
}

// TestTipCodesStayInTheirSegments:trade 码落在 trade 段、复用的通用码落在 common 段。
func TestTipCodesStayInTheirSegments(t *testing.T) {
	for name, c := range tipCodes() {
		seg := segmentOf(t, c.domain)
		if c.code < seg.Base || c.code >= seg.Base+seg.Width {
			t.Errorf("%s = %d 落在 %s 段 [%d,%d) 之外(实际域=%s)",
				name, c.code, c.domain, seg.Base, seg.Base+seg.Width, serverbase.TipDomain(c.code))
		}
	}
}

// TestTipCodesAreRecognizedByServerbase:码进了配表就不该被判成未知(否则每次拒绝刷 rpc_inband_unknown_code)。
func TestTipCodesAreRecognizedByServerbase(t *testing.T) {
	for name, c := range tipCodes() {
		if got := serverbase.TipVerdict(c.code); got == serverbase.VerdictUnknown {
			t.Errorf("%s = %d 仍被 serverbase 判成 VerdictUnknown —— 段表没跟上配表?", name, c.code)
		}
	}
}

// TestTipCodesAreUnique:两个不同语义共用一个 id,客户端只会看到其中一个的文案。
func TestTipCodesAreUnique(t *testing.T) {
	seen := map[uint32]string{}
	for name, c := range tipCodes() {
		if prev, dup := seen[c.code]; dup {
			t.Errorf("tip 码 %d 被 %s 与 %s 同时使用", c.code, prev, name)
		}
		seen[c.code] = name
	}
}

// TestTipClassifier:只有 ErrServiceUnavailable 是故障,其余都是业务拒绝(P1-9)。
// Tip.xlsx 的 fault 列被改时这里先红。
func TestTipClassifier(t *testing.T) {
	classify := TipClassifier()
	for name, c := range tipCodes() {
		want := serverbase.VerdictBizReject
		if c.fault {
			want = serverbase.VerdictFault
		}
		if got := classify(c.code); got != want {
			t.Errorf("%s = %d 应判 %v,实际 %v", name, c.code, want, got)
		}
	}
	if got := classify(100000); got != serverbase.VerdictUnknown {
		t.Errorf("轴外码应判 VerdictUnknown,实际 %v", got)
	}
}

// TestNoHandWrittenTipCodes:Err 常量必须直接写成 uint32(table.TradeError_kX) 或 uint32(table.CommonError_kX)。
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
			if !isGeneratedTipRef(vs.Values[i], allowedTipPrefixes) {
				t.Errorf("%s 必须直接写成 uint32(table.TradeError_kX) 或 uint32(table.CommonError_kX),"+
					"不能手写数字、别名或引用其他码轴", name.Name)
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

func isGeneratedTipRef(expr ast.Expr, enumPrefixes []string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	convert, ok := call.Fun.(*ast.Ident)
	if !ok || convert.Name != "uint32" {
		return false
	}
	selector, ok := call.Args[0].(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "table" {
		return false
	}
	for _, prefix := range enumPrefixes {
		if strings.HasPrefix(selector.Sel.Name, prefix) {
			return true
		}
	}
	return false
}

// TestIsGeneratedTipRefRejectsOtherShapes 保证护栏本身不是摆设:手写数字、别的码轴、别名都必须被拒。
func TestIsGeneratedTipRefRejectsOtherShapes(t *testing.T) {
	cases := map[string]bool{
		"uint32(table.TradeError_kTradeListingNotFound)": true,
		"uint32(table.CommonError_kInvalidParameter)":    true,
		"uint32(table.GuildError_kGuildNotFound)":        false,
		"uint32(20001)":                                  false,
		"20001":                                          false,
		"uint32(other.TradeError_kTradeListingNotFound)": false,
		"uint64(table.TradeError_kTradeListingNotFound)": false,
	}
	for src, want := range cases {
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("解析 %q: %v", src, err)
		}
		if got := isGeneratedTipRef(expr, allowedTipPrefixes); got != want {
			t.Errorf("isGeneratedTipRef(%s) = %v, want %v", src, got, want)
		}
	}
}

// TestMaxSubcategory 钉住 P1 规格 §4 的子类上限表(与客户端 JubaozhaiCatalog 的共同契约)。
func TestMaxSubcategory(t *testing.T) {
	want := map[tradepb.ListingCategory]uint32{
		tradepb.ListingCategory_LISTING_CATEGORY_CHARACTER:       4,
		tradepb.ListingCategory_LISTING_CATEGORY_PET:             6,
		tradepb.ListingCategory_LISTING_CATEGORY_WEAPON:          5,
		tradepb.ListingCategory_LISTING_CATEGORY_ARMOR:           5,
		tradepb.ListingCategory_LISTING_CATEGORY_SET:             0,
		tradepb.ListingCategory_LISTING_CATEGORY_TREASURE:        0,
		tradepb.ListingCategory_LISTING_CATEGORY_JEWELRY:         0,
		tradepb.ListingCategory_LISTING_CATEGORY_SUMMONING_ORDER: 2,
		tradepb.ListingCategory_LISTING_CATEGORY_CURRENCY:        0,
	}
	for category, limit := range want {
		got, ok := MaxSubcategory(category)
		if !ok || got != limit {
			t.Errorf("MaxSubcategory(%v) = (%d, %v), want (%d, true)", category, got, ok, limit)
		}
	}
	for _, bad := range []tradepb.ListingCategory{
		tradepb.ListingCategory_LISTING_CATEGORY_UNSPECIFIED,
		tradepb.ListingCategory(10),
		tradepb.ListingCategory(-1),
	} {
		if _, ok := MaxSubcategory(bad); ok {
			t.Errorf("MaxSubcategory(%d) 应判非法类目", bad)
		}
	}
	// proto 里每个非 0 类目都必须在表里表态,新增类目漏登记会在这里红。
	for value := range tradepb.ListingCategory_name {
		if value == 0 {
			continue
		}
		if _, ok := MaxSubcategory(tradepb.ListingCategory(value)); !ok {
			t.Errorf("proto 类目 %d 没有登记子类上限", value)
		}
	}
}
