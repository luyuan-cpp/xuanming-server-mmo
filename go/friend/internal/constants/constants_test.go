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

// 好友 tip 码的回归护栏。
//
// # 这组测试守的是什么
//
// 好友码曾经从 1 开始手写,ErrFriendListFull=3 与 common 段的 kInvalidTableData=3
// 完全撞号 —— 客户端按 id 查提示表,拿到的是「无效表数据」而不是「好友列表已满」。
// 当时的修法是手写一个私有段 [220,239] 并用 AST 扫描钉住「常量都在段内」。
//
// 2026-09-02 起改成配表统一发号(见 constants.go 的注释与
// docs/design/tip-code-axis.md),于是这组测试的重点也变了:
//
//	旧:断言手写的数字落在手写的段内           —— 守一个「约定」
//	新:断言这里根本没有手写的数字             —— 守「机制」本身
//
// # 为什么码分成两张表
//
// friend 引用两条码轴:自己的 friend 段,加上 common 段的参数非法 / 限流 / 存储故障。
// 「段内」断言必须按轴分开做:把 common 码塞进 tipCodes() 会让
// TestTipCodesStayInFriendSegment 直接红(1005 当然不在 [15000,16000) 里),
// 而那不是缺陷,是复用。所以 tipCodes() 只放 friend 段的码,common 段另立一张
// commonTipCodes();跨轴的断言(唯一性、被 serverbase 认得、定性、AST 护栏)
// 一律走 allTipCodes()。

// tipCodes 是本包 friend 段的 tip 码。往 Tip.xlsx 的 //friend_error 组加码时在这里补一行。
//
// 后三个是 F2 批新增的(拉黑 / 黑名单满 / 对方收件箱满)。它们的 xlsx 行要等合并回 main 时
// 与 regen 同窗口添加(见 constants.go 顶部的说明),在那之前整包编译不过 ——
// 所以这张表先登记好:AST 护栏是双向核对的,少登记一个就会在
// TestNoHandWrittenTipCodes 里报「新增了 ErrX 但没纳入护栏」。
func tipCodes() map[string]uint32 {
	return map[string]uint32{
		"ErrCannotAddSelf":        ErrCannotAddSelf,
		"ErrAlreadyFriends":       ErrAlreadyFriends,
		"ErrFriendListFull":       ErrFriendListFull,
		"ErrRequestAlreadySent":   ErrRequestAlreadySent,
		"ErrTargetFriendListFull": ErrTargetFriendListFull,
		"ErrNoPendingRequest":     ErrNoPendingRequest,
		"ErrTooManyPending":       ErrTooManyPending,
		"ErrBlocked":              ErrBlocked,
		"ErrBlockListFull":        ErrBlockListFull,
		"ErrTargetInboxFull":      ErrTargetInboxFull,
	}
}

// commonTipCodes 是本包复用的 common 段码(跨域语义,不在好友段发号)。
func commonTipCodes() map[string]uint32 {
	return map[string]uint32{
		"ErrInvalidParameter": ErrInvalidParameter,
		"ErrRateLimited":      ErrRateLimited,
		"ErrStorage":          ErrStorage,
	}
}

// allTipCodes 是本包对外提供的全部 tip 码,给跨轴断言与 AST 护栏用。
func allTipCodes() map[string]uint32 {
	all := make(map[string]uint32, len(tipCodes())+len(commonTipCodes()))
	for name, code := range tipCodes() {
		all[name] = code
	}
	for name, code := range commonTipCodes() {
		all[name] = code
	}
	return all
}

// faultCodeNames:本包里 Tip.xlsx fault 列应为 1 的常量。
//
// 只有 ErrStorage 一个。这张表小得像多余,但它是「码的定性」这件事在本包的唯一书面预期:
// 谁把 fault 列改错(比如给「好友列表已满」标了 1,或把 kServiceUnavailable 的 1 去掉),
// TestTipCodesVerdicts 会先红,而不是等线上告警刷起来才发现。
var faultCodeNames = map[string]struct{}{
	"ErrStorage": {},
}

// enumPrefixFor 返回某个 Err 常量**必须**引用的生成枚举前缀。
// ok=false 表示这个常量还没纳入上面两张表的护栏 —— AST 扫描据此报错,
// 所以新增一个 Err 常量就必须同步登记,不存在「加了码但没人守」的状态。
func enumPrefixFor(name string) (prefix string, ok bool) {
	if _, isFriend := tipCodes()[name]; isFriend {
		return "FriendError_", true
	}
	if _, isCommon := commonTipCodes()[name]; isCommon {
		return "CommonError_", true
	}
	return "", false
}

// segmentOf 从生成的段表里取某个域的段。段表源头是 data/tip/Tip.xlsx 的组头行。
func segmentOf(t *testing.T, domain string) tip.Segment {
	t.Helper()
	for _, s := range tip.Segments {
		if s.Domain == domain {
			return s
		}
	}
	t.Fatalf("生成的段表里没有 %s 段:Tip.xlsx 的 //%s_error 组是不是被删了 / 没导表?", domain, domain)
	return tip.Segment{}
}

// assertInSegment 断言码落在声明段 [Base, Base+Width) 内。
// 判段只看声明,不看 Lo/Hi —— 后者随新增码变化,不是契约。
func assertInSegment(t *testing.T, name string, code uint32, domain string) {
	t.Helper()
	seg := segmentOf(t, domain)
	if code < seg.Base || code >= seg.Base+seg.Width {
		t.Errorf("%s = %d 落在 %s 段 [%d,%d) 之外(实际域=%s)",
			name, code, domain, seg.Base, seg.Base+seg.Width, serverbase.TipDomain(code))
	}
}

// TestTipCodesStayInFriendSegment 钉死「好友 tip 码只能落在自己的段里」。
func TestTipCodesStayInFriendSegment(t *testing.T) {
	for name, code := range tipCodes() {
		assertInSegment(t, name, code, "friend")
	}
}

// TestCommonTipCodesAreCommonSegment 钉死「复用的三个码真的是 common 段的码」。
//
// 这条防的是「以后有人图省事,把 ErrStorage 改成指向某个域私有的故障码」:
// 那样 friend 的存储故障就不再和其它服务聚合在同一个码上,
// 按码写的告警规则会漏掉 friend,而编译和其它用例都不会有任何反应。
func TestCommonTipCodesAreCommonSegment(t *testing.T) {
	for name, code := range commonTipCodes() {
		assertInSegment(t, name, code, "common")
	}
}

// TestTipCodesAreRecognizedByServerbase 守住「码进了配表就不该再被判成未知」。
//
// 改造前好友码在 serverbase 眼里全是 VerdictUnknown(它只认 1..129),
// 每次业务拒绝都会刷一条 rpc_inband_unknown_code。
func TestTipCodesAreRecognizedByServerbase(t *testing.T) {
	for name, code := range allTipCodes() {
		if got := serverbase.TipVerdict(code); got == serverbase.VerdictUnknown {
			t.Errorf("%s = %d 仍被 serverbase 判成 VerdictUnknown —— 段表没跟上配表?", name, code)
		}
	}
}

// TestTipCodesDoNotCollideWithCommonSegment 是最初那次事故的锚点用例:
// **好友段**的码不许和 common 段的低位码撞号。
// 本包显式复用的 common 码走 commonTipCodes(),那是有意为之,不在此列。
func TestTipCodesDoNotCollideWithCommonSegment(t *testing.T) {
	collided := []struct {
		name string
		code uint32
	}{
		{"kSuccess", uint32(table.CommonError_kSuccess)},
		{"kInvalidTableId", uint32(table.CommonError_kInvalidTableId)},
		{"kInvalidTableData", uint32(table.CommonError_kInvalidTableData)},
	}
	for name, code := range tipCodes() {
		for _, c := range collided {
			if code == c.code {
				t.Errorf("%s = %d 与 common 段的 %s 完全相同", name, code, c.name)
			}
		}
	}
}

// TestTipCodesAreUnique 防止段内自撞,也防止两条轴之间撞:
// 两个不同语义共用一个 id,客户端只会看到其中一个的文案。
func TestTipCodesAreUnique(t *testing.T) {
	seen := map[uint32]string{}
	for name, code := range allTipCodes() {
		if prev, dup := seen[code]; dup {
			t.Errorf("tip 码 %d 被 %s 与 %s 同时使用", code, prev, name)
		}
		seen[code] = name
	}
}

// TestTipCodesVerdicts 钉住每个码的定性:好友段 10 码 + 参数非法 + 限流都是业务拒绝,
// **只有** ErrStorage 是故障。
//
// 这条分界线决定了日志与告警:故障码会打 Error、计 rpc_inband_fault、配告警;
// 业务拒绝只计数。把「好友列表已满」误标成故障,等于让正常的游戏规则拒绝天天报警。
//
// F2 批新增的 ErrBlocked / ErrBlockListFull / ErrTargetInboxFull 尤其要靠这条守住:
// 它们的 Tip.xlsx 行是在合并窗口里手工补的(constants.go 顶部列了要补的三行),
// 而那张表的 fault 列只要有人顺手填个 1,这三种最常见的业务拒绝就会变成
// 「好友服务故障」告警,且代码这边零报错、零感知 —— 唯一会红的就是这条用例。
func TestTipCodesVerdicts(t *testing.T) {
	for name, code := range allTipCodes() {
		want := serverbase.VerdictBizReject
		if _, isFault := faultCodeNames[name]; isFault {
			want = serverbase.VerdictFault
		}
		if got := serverbase.TipVerdict(code); got != want {
			t.Errorf("%s = %d 应判 %v,实际 %v —— Tip.xlsx 的 fault 列被改了?", name, code, want, got)
		}
	}
}

// TestTipClassifier 守的是「本服务交出去的定性函数」与上面那张预期表一致,
// 且段外码仍判 VerdictUnknown(将来若有人在这里包一层本地 map,这条会红)。
func TestTipClassifier(t *testing.T) {
	c := TipClassifier()

	for name, code := range allTipCodes() {
		if got, want := c(code), serverbase.TipVerdict(code); got != want {
			t.Errorf("%s 的定性 %v 与全局判定 %v 不一致:TipClassifier 不该自己改定性", name, got, want)
		}
	}
	if got := c(uint32(table.CommonError_kServiceUnavailable)); got != serverbase.VerdictFault {
		t.Errorf("common 段的服务不可用应判 VerdictFault, 实际 %v", got)
	}
	if got := c(100000); got != serverbase.VerdictUnknown {
		t.Errorf("轴外码应判 VerdictUnknown, 实际 %v", got)
	}
}

// TestNoHandWrittenTipCodes 是这次改造真正的护栏:constants.go 里不许出现
// 「Err* = 整数字面量」。手写数字绕开发号器,段、重名、文案三道保证全部失效。
//
// 扫描按常量逐个查它该用哪条轴(enumPrefixFor):好友码必须写 table.FriendError_kX,
// 复用的通用码必须写 table.CommonError_kX。跨轴写反也算违规 ——
// 比如把 ErrStorage 指到某个域私有的故障码上,码值仍然「是生成的」,但语义已经跑了。
func TestNoHandWrittenTipCodes(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "constants.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 constants.go 失败: %v", err)
	}

	want := allTipCodes()
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
			prefix, registered := enumPrefixFor(name.Name)
			if !registered {
				// 下面的双向核对也会报这个名字,但在这里就停住,免得拿空前缀去判 RHS。
				continue
			}
			if i >= len(vs.Values) {
				t.Errorf("%s 必须显式写生成枚举引用,不能省略 RHS 继承上一常量", name.Name)
				continue
			}
			if !isGeneratedTipRef(vs.Values[i], prefix) {
				t.Errorf("%s 必须直接写成 uint32(table.%skX),不能手写数字、别名或引用其他码轴",
					name.Name, prefix)
			}
		}
		return true
	})
	for name := range want {
		if _, ok := seen[name]; !ok {
			t.Errorf("护栏清单声明了 %s,但 constants.go 没有对应 Err 常量", name)
		}
	}
	for name := range seen {
		if _, ok := want[name]; !ok {
			t.Errorf("constants.go 新增了 %s,必须同步纳入 tipCodes / commonTipCodes 的全量护栏", name)
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

// TestIsGeneratedTipRefRejectsOtherShapes 保证护栏本身不是摆设:
// 手写数字、别的码轴、别名包、别的转换类型都必须被拒。
func TestIsGeneratedTipRefRejectsOtherShapes(t *testing.T) {
	cases := []struct {
		src    string
		prefix string
		want   bool
	}{
		{"uint32(table.FriendError_kFriendListFull)", "FriendError_", true},
		{"uint32(table.CommonError_kInvalidParameter)", "CommonError_", true},
		// 跨轴写反:码值是生成的,但语义跑到别的域去了。
		{"uint32(table.CommonError_kInvalidParameter)", "FriendError_", false},
		{"uint32(table.GuildError_kGuildNotFound)", "FriendError_", false},
		{"uint32(15002)", "FriendError_", false},
		{"15002", "FriendError_", false},
		{"uint32(other.FriendError_kFriendListFull)", "FriendError_", false},
		{"uint64(table.FriendError_kFriendListFull)", "FriendError_", false},
	}
	for _, c := range cases {
		expr, err := parser.ParseExpr(c.src)
		if err != nil {
			t.Fatalf("解析 %q: %v", c.src, err)
		}
		if got := isGeneratedTipRef(expr, c.prefix); got != c.want {
			t.Errorf("isGeneratedTipRef(%s, %q) = %v, want %v", c.src, c.prefix, got, c.want)
		}
	}
}

// TestEnumPrefixForRejectsUnregistered:没登记的常量名必须拿不到前缀,
// 否则 TestNoHandWrittenTipCodes 的「新增必须登记」那半边就形同虚设。
func TestEnumPrefixForRejectsUnregistered(t *testing.T) {
	if _, ok := enumPrefixFor("ErrSomethingNobodyRegistered"); ok {
		t.Error("未登记的常量名不该拿到枚举前缀")
	}
	if prefix, ok := enumPrefixFor("ErrCannotAddSelf"); !ok || prefix != "FriendError_" {
		t.Errorf("好友段常量应要求 FriendError_,得到 (%q, %v)", prefix, ok)
	}
	if prefix, ok := enumPrefixFor("ErrStorage"); !ok || prefix != "CommonError_" {
		t.Errorf("common 段常量应要求 CommonError_,得到 (%q, %v)", prefix, ok)
	}
}
