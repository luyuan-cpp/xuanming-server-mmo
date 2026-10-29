package currencytype

import (
	"math"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	basepb "proto/common/base"
)

func TestIsKnownMatchesTheProtoEnum(t *testing.T) {
	for _, v := range []uint32{Gold, Diamond, BindDiamond} {
		if !IsKnown(v) {
			t.Fatalf("IsKnown(%d) = false, want true", v)
		}
	}
	for _, v := range []uint32{uint32(len(basepb.CurrencyType_name)), math.MaxInt32 + 1, math.MaxUint32} {
		if IsKnown(v) {
			t.Fatalf("IsKnown(%d) = true, want false", v)
		}
	}
}

// 取值必须从 0 连续:CurrencyComp.values 以币种为下标,C++ 用 ARRAYSIZE 当上限。
// 删值、跳号或复用会让已入箱的旧条目在所有版本上永远判成版本错位(C++ 侧
// CurrencyTest.CurrencyTypeEnumIsDerivedFromTheProtoTable 守同一条)。
func TestValuesAreContiguousFromZero(t *testing.T) {
	values := basepb.CurrencyType(0).Descriptor().Values()
	if values.Len() != len(basepb.CurrencyType_name) {
		t.Fatalf("descriptor has %d values, name table has %d", values.Len(), len(basepb.CurrencyType_name))
	}
	for i := 0; i < values.Len(); i++ {
		if values.ByNumber(protoreflect.EnumNumber(i)) == nil {
			t.Fatalf("currency_type.proto is missing value %d; values must be contiguous from 0", i)
		}
		if !IsKnown(uint32(i)) {
			t.Fatalf("IsKnown(%d) = false for a value in the table", i)
		}
	}
}

func TestNamedConstantsMatchTheProtoNames(t *testing.T) {
	want := map[uint32]string{
		Gold:        "CURRENCY_TYPE_GOLD",
		Diamond:     "CURRENCY_TYPE_DIAMOND",
		BindDiamond: "CURRENCY_TYPE_BIND_DIAMOND",
	}
	for v, name := range want {
		if got := basepb.CurrencyType_name[int32(v)]; got != name {
			t.Fatalf("value %d is %q in currency_type.proto, want %q", v, got, name)
		}
	}
}
