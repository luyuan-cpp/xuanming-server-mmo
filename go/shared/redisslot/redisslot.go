// Package redisslot 是玩家域 Redis 的 slot 契约:Redis Cluster 的 HashSlot、玩家 tag、按 slot 索引的 slot-tag,
// 以及落进 data_service 字段命名空间的保留后缀表。契约与 C++ 的 cpp/libs/engine/infra/storage/redis_cluster/redis_slot.h
// 逐字一致,两侧金样测试钉住同一组向量(docs/design/redis-player-store-cluster.md §5.2)。
//
// 玩家域的键形规则:键里的玩家 id 用花括号包起来(PlayerTag),跨玩家的按 slot 索引用 PlayerIndexTag,
// 二者必然同槽,所以一名玩家的键与它所在 slot 的索引可以放进同一段 Lua。
package redisslot

import (
	"strconv"
	"strings"
	"sync"
)

// SlotCount 是 Redis Cluster 的 slot 总数(规范常量,不可配置)。
const SlotCount = 16384

// crc16Table 是 CRC16-XMODEM(多项式 0x1021,初值 0)的查表,与 Redis 源码 crc16.c 相同。
var crc16Table = func() [256]uint16 {
	var t [256]uint16
	for i := range t {
		crc := uint16(i) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return t
}()

// CRC16 返回 s 的 CRC16-XMODEM。金样:CRC16("123456789") = 0x31C3(Redis Cluster 规范原文例子)。
func CRC16(s string) uint16 {
	var crc uint16
	for i := 0; i < len(s); i++ {
		crc = crc<<8 ^ crc16Table[byte(crc>>8)^s[i]]
	}
	return crc
}

// hashTagContent 按 Redis Cluster 规范取键里参与哈希的部分:第一个 '{' 之后第一个 '}' 之前若非空就只取这段,
// 否则取整个键。ok 表示键带有非空 hash tag。
func hashTagContent(key string) (content string, ok bool) {
	open := strings.IndexByte(key, '{')
	if open < 0 {
		return key, false
	}
	closeRel := strings.IndexByte(key[open+1:], '}')
	if closeRel <= 0 { // 没有 '}',或 "{}" 空 tag
		return key, false
	}
	return key[open+1 : open+1+closeRel], true
}

// HashSlot 返回键所在的 slot(CRC16 取低 14 位)。
func HashSlot(key string) uint16 {
	content, _ := hashTagContent(key)
	return CRC16(content) & (SlotCount - 1)
}

// HasHashTag 报告键是否带非空 hash tag。玩家域客户端只接受带 tag 的键(设计 §5.1 的接缝规则)。
func HasHashTag(key string) bool {
	_, ok := hashTagContent(key)
	return ok
}

// PlayerSlot 返回玩家所在的 slot:对 player_id 十进制(无前导零)算 CRC16。
func PlayerSlot(playerID uint64) uint16 {
	return CRC16(strconv.FormatUint(playerID, 10)) & (SlotCount - 1)
}

// PlayerTag 返回玩家键里的 hash tag,例如 "{42}"。玩家域的单玩家键一律用它包住玩家 id。
func PlayerTag(playerID uint64) string {
	return "{" + strconv.FormatUint(playerID, 10) + "}"
}

// slotTagAlphabet 是 slot-tag 的字母表。只用小写字母:纯字母的 tag 不会被误解析成玩家 id。
const slotTagAlphabet = "abcdefghijklmnopqrstuvwxyz"

// slotTagMaxLen 是 slot-tag 的最大长度。长度 4 以内已覆盖全部 16384 个 slot;超过就是实现错误。
const slotTagMaxLen = 4

var (
	slotTagsOnce sync.Once
	slotTags     [SlotCount]string
)

// buildSlotTags 按「先短后长、同长按字典序」枚举字母表上的字符串,第一个哈希到 slot s 的串就是 s 的 tag。
// 算法与 C++ 侧逐字一致;整张表按 slot 顺序以 '\n' 连接后的 SHA-256 由两侧金样测试钉住。
func buildSlotTags() {
	filled := 0
	buf := make([]byte, 0, slotTagMaxLen)
	for length := 1; length <= slotTagMaxLen && filled < SlotCount; length++ {
		total := 1
		for range length {
			total *= len(slotTagAlphabet)
		}
		for n := 0; n < total && filled < SlotCount; n++ {
			buf = buf[:length]
			v := n
			for i := length - 1; i >= 0; i-- {
				buf[i] = slotTagAlphabet[v%len(slotTagAlphabet)]
				v /= len(slotTagAlphabet)
			}
			slot := CRC16(string(buf)) & (SlotCount - 1)
			if slotTags[slot] == "" {
				slotTags[slot] = string(buf)
				filled++
			}
		}
	}
	if filled != SlotCount {
		panic("redisslot: slot-tag 表未覆盖全部 slot")
	}
}

// SlotTag 返回 slot 的 tag 内容(不含花括号),满足 HashSlot("{" + SlotTag(s) + "}") == s。
// slot 越界时 panic:调用方只应传 HashSlot / PlayerSlot 的结果。
func SlotTag(slot uint16) string {
	if slot >= SlotCount {
		panic("redisslot: slot 越界")
	}
	slotTagsOnce.Do(buildSlotTags)
	return slotTags[slot]
}

// PlayerIndexTag 返回与玩家同槽的按 slot 索引键 tag,例如 "{afps}"。跨玩家索引(补投索引、会话租约、gate 索引、
// 战斗结算索引)用它,与该玩家的 PlayerTag 键必然同槽。
func PlayerIndexTag(playerID uint64) string {
	return "{" + SlotTag(PlayerSlot(playerID)) + "}"
}

// SlotIndexTag 返回 slot 的索引键 tag(含花括号),供按 slot 遍历的后台扫描器使用。
func SlotIndexTag(slot uint16) string {
	return "{" + SlotTag(slot) + "}"
}

// reservedPlayerKeySuffixes 是落进 player:{<id>}: 前缀、但不归 data_service 字段接口所有的后缀。
// data_service 以 player:{<id>}:<字段> 存自己的玩家字段,并按该前缀 SCAN 未索引玩家的字段;任何模块新增
// player:{<id>}: 键都必须登记到这里,否则 data_service 可能把它当字段读出或覆盖(设计 §5.2)。
// tools/merge_zone 是独立 module,在 player_blob_migrate.go 镜像了这张表(合服拷 blob 时跳过),两边测试钉同一份字面量。
var reservedPlayerKeySuffixes = map[string]struct{}{
	"save_barrier":            {}, // C++ 存盘屏障(shared/savebarrier)
	"asset_sql_commit":        {}, // go/db 资产 SQL 提交证据(shared/assetop)
	"owner_epoch":             {}, // 归属代次
	"handoff":                 {}, // 交接标记
	"location":                {}, // 位置
	"location_revision":       {}, // 位置修订
	"location_revision_floor": {}, // 位置修订下限
	"leave_release_wait":      {}, // 离场释放等待
	"leave_tomb":              {}, // 离场墓碑
	"channel_switch_cooldown": {}, // 换线冷却
	"login_generation":        {}, // 登录代次
	"sync_digest":             {}, // 同步摘要
}

// IsReservedPlayerKeySuffix 报告 player:{<id>}:<suffix> 是否是其他模块的玩家域键。
func IsReservedPlayerKeySuffix(suffix string) bool {
	_, ok := reservedPlayerKeySuffixes[suffix]
	return ok
}

// ReservedPlayerKeySuffixes 返回保留后缀的副本(无序),供契约测试枚举核对。
func ReservedPlayerKeySuffixes() []string {
	out := make([]string, 0, len(reservedPlayerKeySuffixes))
	for s := range reservedPlayerKeySuffixes {
		out = append(out, s)
	}
	return out
}
