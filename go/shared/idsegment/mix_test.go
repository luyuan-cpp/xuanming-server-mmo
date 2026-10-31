package idsegment

import "testing"

// mixGolden 是 Go 与 C++ 共用的固定样例,C++ 侧见 cpp/tests/bag_test/bag_test.cpp 的 MixGuidMatchesGoGolden。
// 改了这张表就等于改了发出去的 id,正式发号之后绝不能改。
var mixGolden = []struct{ in, out uint64 }{
	{0, 0},
	{1, 2558316640067307269},
	{2, 6616413020432831626},
	{3, 2185194620014831856},
	{100, 6504034234019148180},
	{16777216, 6406066033425044679},
	{16777217, 442559913109642155},
	{4294967295, 806922830091569532},
	{4294967296, 6350277681932728639},
	{436207616000000, 1867450719239334575},
	{436207616000001, 3294820914267284580},
	{144115188075855871, 1772925764027790080},
	{144115188075855872, 5350149144691749192},
	{9223372036854775807, 6514504133438201533},
}

func TestMixID_Golden(t *testing.T) {
	for _, g := range mixGolden {
		if got := MixID(g.in); got != g.out {
			t.Errorf("MixID(%d) = %d, want %d", g.in, got, g.out)
		}
		if got := UnmixID(g.out); got != g.in {
			t.Errorf("UnmixID(%d) = %d, want %d", g.out, got, g.in)
		}
	}
}

func TestMixID_InverseConstants(t *testing.T) {
	// 经变量相乘:常量表达式相乘会在编译期报溢出,而这里要的就是模 2^64 回绕。
	c1, c1Inv, c2, c2Inv := mixC1, mixC1Inv, mixC2, mixC2Inv
	if got := c1 * c1Inv & mixMask; got != 1 {
		t.Fatalf("mixC1 * mixC1Inv = %d, want 1", got)
	}
	if got := c2 * c2Inv & mixMask; got != 1 {
		t.Fatalf("mixC2 * mixC2Inv = %d, want 1", got)
	}
}

// 逆函数往返成立就证明是单射,而定义域和值域都是 [0, 2^63),所以是双射。
func TestMixID_RoundTrip(t *testing.T) {
	// 既覆盖连续的小值,也覆盖真实计数器的形状(高位是时间,低位是序号)和值域边界。
	bases := []uint64{0, 1 << 24, 26_000_000 << 24, 1<<57 - 1<<20, 1<<63 - 1<<20}
	for _, base := range bases {
		for i := uint64(0); i < 1<<20; i++ {
			x := base + i
			y := MixID(x)
			if y > mixMask {
				t.Fatalf("MixID(%d) = %d exceeds 63 bits", x, y)
			}
			if back := UnmixID(y); back != x {
				t.Fatalf("UnmixID(MixID(%d)) = %d", x, back)
			}
		}
	}
}

// 0 是非法 id:任何大于 0 的计数器都不能映射成 0。
func TestMixID_ZeroOnlyFromZero(t *testing.T) {
	if MixID(0) != 0 || UnmixID(0) != 0 {
		t.Fatal("0 must map to 0")
	}
}

// 打散的目的:连续的计数器值,低位分片和高位分布都要均匀。
func TestMixID_SpreadsSequentialInput(t *testing.T) {
	const n = 1 << 16
	const shards = 64
	const regions = 16
	var low [shards]int
	var high [regions]int
	for i := uint64(1); i <= n; i++ {
		y := MixID(26_000_000<<24 + i)
		low[y%shards]++
		high[y>>(mixBits-4)]++
	}
	// 每个桶的期望分别是 1024 和 4096,放宽到 ±30%,这不是统计检验,只防明显退化。
	for k, c := range low {
		if c < 716 || c > 1332 {
			t.Errorf("low bits shard %d has %d, want about 1024", k, c)
		}
	}
	for k, c := range high {
		if c < 2867 || c > 5325 {
			t.Errorf("high bits region %d has %d, want about 4096", k, c)
		}
	}
}
