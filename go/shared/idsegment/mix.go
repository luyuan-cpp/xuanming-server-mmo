package idsegment

// MixID 把发号器的计数器值一一映射成对外的 id。
//
// 它不决定唯一性:唯一性来自发号器(每个计数器值只发一次),这里只保证不同的输入得到不同的
// 输出。它解决的是索引热点:计数器递增,直接当 id 用的话,新行的主键都挤在索引最右边一个
// Region,一台机器独扛,加机器也没用。
//
// 结构是 SplitMix64 的混合函数收窄到 63 位:每一步都可逆(右移异或、乘奇数),所以是 [0, 2^63)
// 上的双射,没有循环。0 只对应 0,而计数器恒大于 0,所以发不出 0。输出恒小于 2^63,转 int64 不会变负。
//
// 这是公开算法,从 id 能反算出计数器,所以它不提供保密性:不得用「id 猜不到」做权限判断。
//
// **这个函数和两个常量一经发出正式 id 就不能改**:改了之后同一个计数器值会算出不同的 id,可能撞上
// 以前发过的号。C++ 同构实现在 cpp/libs/modules/id_segment/guid_segment_client.cpp,
// 两边用 mix_test.go 的固定样例对拍。设计见 docs/design/player-data-tidb-scaling-plan.md §6.1。
func MixID(x uint64) uint64 {
	x &= mixMask
	x ^= x >> 30
	x = x * mixC1 & mixMask
	x ^= x >> 27
	x = x * mixC2 & mixMask
	x ^= x >> 31
	return x
}

// UnmixID 是 MixID 的逆函数:把 id 还原成计数器值。用于排查、审计,以及测试里证明它是双射。
func UnmixID(y uint64) uint64 {
	y &= mixMask
	y = unXorShift(y, 31)
	y = y * mixC2Inv & mixMask
	y = unXorShift(y, 27)
	y = y * mixC1Inv & mixMask
	y = unXorShift(y, 30)
	return y
}

// unXorShift 求 y = x ^ (x >> k) 的 x:每轮多还原 k 位,63 位 ÷ k 轮之后全部还原。
func unXorShift(y uint64, k uint) uint64 {
	x := y
	for s := k; s < mixBits; s += k {
		x = y ^ (x >> k)
	}
	return x
}

const (
	mixBits = 63
	mixMask = uint64(1)<<mixBits - 1

	// mixC1、mixC2 是 SplitMix64 的两个乘数,都是奇数,所以模 2^63 可逆。
	mixC1 = uint64(0xbf58476d1ce4e5b9)
	mixC2 = uint64(0x94d049bb133111eb)

	// mixC1Inv、mixC2Inv 是它们模 2^63 的乘法逆元,由 mix_test.go 校验。
	mixC1Inv = uint64(0x16de1b173f119089)
	mixC2Inv = uint64(0x319642b2d24d8ec3)
)
