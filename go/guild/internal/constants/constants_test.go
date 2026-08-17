package constants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"shared/generated/pb/table"
	"shared/serverbase"
)

// scanTipCodes 直接扫描 constants.go 的 AST,取出本文件里所有
// `ErrXxx uint32 = <整数字面量>` 形式的常量。
//
// 为什么用 AST 而不是在测试里手抄一份常量表:手抄的表挡不住「后来的人又加了一个
// ErrFoo uint32 = 3」——那正是本次要修的那类撞号。扫源码才能让新增常量自动进入
// 下面的所有断言。
//
// 任何 Err* 常量只要写成扫不出来的形式(缺 uint32 类型、值不是整数字面量),
// 测试直接失败:与其静默漏检,不如逼着改回可扫描的写法。
func scanTipCodes(t *testing.T) map[string]uint64 {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "constants.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 constants.go 失败: %v", err)
	}

	codes := make(map[string]uint64)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Err") {
					continue
				}
				ident, ok := vs.Type.(*ast.Ident)
				if !ok || ident.Name != "uint32" {
					t.Fatalf("%s 必须显式声明为 uint32,否则无法参与号段扫描", name.Name)
				}
				if i >= len(vs.Values) {
					t.Fatalf("%s 必须显式赋值整数字面量,否则无法参与号段扫描", name.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s 的值必须是整数字面量,否则无法参与号段扫描", name.Name)
				}
				v, err := strconv.ParseUint(lit.Value, 0, 32)
				if err != nil {
					t.Fatalf("%s 的值 %q 解析失败: %v", name.Name, lit.Value, err)
				}
				codes[name.Name] = v
			}
		}
	}

	if len(codes) == 0 {
		t.Fatal("没扫到任何 Err* 常量,扫描逻辑或常量写法已失效")
	}
	return codes
}

// TestTipCodesStayInGuildSegment 钉死「公会 tip 码只能落在自己的号段里」。
func TestTipCodesStayInGuildSegment(t *testing.T) {
	for name, code := range scanTipCodes(t) {
		if code < uint64(GuildTipCodeLo) || code > uint64(GuildTipCodeHi) {
			t.Errorf("%s = %d 越出公会号段 [%d, %d]", name, code, GuildTipCodeLo, GuildTipCodeHi)
		}
	}
}

// TestTipCodesDoNotCollideWithGeneratedTipTable 是本次修复的回归护栏:
// 公会码曾经从 1 开始手写,与 common 段(kInvalidTableId=2 / kInvalidTableData=3 /
// kIndexOutOfRange=8)整段重叠,客户端按 id 查提示表会显示完全无关的文案。
//
// 断言口径是「整条已生成的 tip 数轴」而不只是 common 段:tip 是单一扁平命名空间,
// 撞上 login / scene / team 任意一段的后果一模一样。
func TestTipCodesDoNotCollideWithGeneratedTipTable(t *testing.T) {
	// 抽查几个当初真实撞上的 common 码,写死在这里当锚点,
	// 免得将来有人把 TipMaxKnownCode 改小就让下面的范围断言失效。
	collided := []struct {
		name string
		code uint32
	}{
		{"kInvalidTableId", uint32(table.CommonError_kInvalidTableId)},
		{"kInvalidTableData", uint32(table.CommonError_kInvalidTableData)},
		{"kIndexOutOfRange", uint32(table.CommonError_kIndexOutOfRange)},
		{"kSuccess", uint32(table.CommonError_kSuccess)},
	}

	for name, code := range scanTipCodes(t) {
		if code <= uint64(serverbase.TipMaxKnownCode) {
			t.Errorf("%s = %d 落在已生成的 tip 数轴 [0, %d] 内(域=%s),会与配表码撞号",
				name, code, serverbase.TipMaxKnownCode, serverbase.TipDomain(uint32(code)))
		}
		for _, c := range collided {
			if code == uint64(c.code) {
				t.Errorf("%s = %d 与 common 段的 %s 完全相同", name, code, c.name)
			}
		}
	}
}

// TestTipCodesAreUnique 防止段内自撞:两个不同语义的错误共用一个 id,
// 客户端同样会显示错文案。
func TestTipCodesAreUnique(t *testing.T) {
	seen := make(map[uint64]string)
	for name, code := range scanTipCodes(t) {
		if prev, ok := seen[code]; ok {
			t.Errorf("%s 与 %s 都取值 %d", name, prev, code)
			continue
		}
		seen[code] = name
	}
}

// TestTipClassifier 覆盖定性函数的三条分支:段内故障码、段内业务拒绝码、段外交回全局。
func TestTipClassifier(t *testing.T) {
	classify := TipClassifier()

	if got := classify(ErrIDGenUnavailable); got != serverbase.VerdictFault {
		t.Errorf("ErrIDGenUnavailable 应判为 fault,实际 %v", got)
	}
	for _, code := range []uint32{ErrAlreadyInGuild, ErrGuildFull, ErrNotLeader, ErrNotRanked} {
		if got := classify(code); got != serverbase.VerdictBizReject {
			t.Errorf("码 %d 应判为 biz_reject,实际 %v", code, got)
		}
	}
	// 段外:仍走 serverbase 的全局判定,不被本服务的号段接管。
	if got := classify(uint32(table.CommonError_kServiceUnavailable)); got != serverbase.VerdictFault {
		t.Errorf("common 段 kServiceUnavailable 应判为 fault,实际 %v", got)
	}
	if got := classify(uint32(table.CommonError_kCommon_errorOK)); got != serverbase.VerdictOK {
		t.Errorf("码 0 应判为 ok,实际 %v", got)
	}
}
