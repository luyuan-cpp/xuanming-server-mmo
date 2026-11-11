package redisslot

import (
	"strconv"
	"strings"
)

// 控制集群的 hash tag(docs/design/redis-cluster-all-domains-20261110.md §3)。
//
// 控制集群放玩家域、登录域与 match 私有键之外的全部 Redis 用途。同一个多键操作的键共用一个 tag,形状是
// {<族前缀>:<身份>},族前缀登记在下面这张表里。表与 C++ 的 cpp/libs/engine/infra/storage/redis_cluster/redis_slot.h
// 的 kControlFamilyPrefixes 逐字一致(control_keys_test.go 读 C++ 头核对);改任何一项都要两侧同批改,上线后不得改
// (改前缀会让存量键换槽)。
//
// 两个域靠 tag 的形状区分(IsControlTag):控制集群的 tag 是登记过的族前缀加冒号再加非空身份;玩家域的 tag 是十进制
// 玩家 id、纯小写字母的 slot-tag,或登录连接身份 "<gate 实例号>:<序号>"。前两种不含冒号;gate 实例号是进程启动时生成的
// 唯一串,不会恰好等于某个族前缀。

// ControlFamily 是控制集群的一个族。底层类型按族的个数选(现 12 个)。
type ControlFamily uint8

const (
	// ControlFamilySceneControl:scene_manager 控制域全部键,身份是 zone。C++ scene 只读分线目录、神秘商人,并做归属对账。
	ControlFamilySceneControl ControlFamily = iota
	// ControlFamilyLoginQueue:login 排队、准入与放行锁,身份是 zone。
	ControlFamilyLoginQueue
	// ControlFamilyGateMark:gate 排空 / 停写标记,身份是 zone。
	ControlFamilyGateMark
	// ControlFamilyAssetWake:落点唤醒的分片频道,身份是 player_id % 64(与资产通道的工作分片同一规则)。
	ControlFamilyAssetWake
	// ControlFamilyGuild:帮会缓存与它的代次键,身份是 guild_id。
	ControlFamilyGuild
	// ControlFamilyPlayerGuild:玩家所在帮会的缓存与它的代次键,身份是 player_id。
	ControlFamilyPlayerGuild
	// ControlFamilyGuildRank:帮会榜、同区的版本 HASH 与墓碑、维护锁,身份是 zone。
	ControlFamilyGuildRank
	// ControlFamilyPlayerLocator:player_locator 的 gate 索引控制键,身份固定是 gix(ControlPlayerLocatorIdentity)。
	ControlFamilyPlayerLocator
	// ControlFamilyFriend:好友私有键,身份是 player_id。
	ControlFamilyFriend
	// ControlFamilyFriendRequestQuota:好友申请配额,身份是 player_id。
	ControlFamilyFriendRequestQuota
	// ControlFamilyFriendDirectory:好友目录配额,身份是 player_id。
	ControlFamilyFriendDirectory
	// ControlFamilyGuildApply:帮会申请配额,身份是 player_id。
	ControlFamilyGuildApply

	controlFamilyCount
)

// controlFamilyPrefixes 与 ControlFamily 一一对应。f、rq、directory、apply 四族的键今天已是这个形状,不改名。
var controlFamilyPrefixes = [controlFamilyCount]string{
	"ctl",
	"q",
	"gm",
	"aw",
	"g",
	"pg",
	"gr",
	"pl",
	"f",
	"rq",
	"directory",
	"apply",
}

// ControlPlayerLocatorIdentity 是 player_locator 那一族的固定身份:它的 7 把控制键共用一个 tag,访问是 10 秒级的低频。
const ControlPlayerLocatorIdentity = "gix"

// Prefix 返回族前缀。越界的族是调用方的实现错误,panic。
func (f ControlFamily) Prefix() string {
	if f >= controlFamilyCount {
		panic("redisslot: unknown control family " + strconv.Itoa(int(f)))
	}
	return controlFamilyPrefixes[f]
}

// ControlTag 返回控制集群的 hash tag,例如 ControlTag(ControlFamilySceneControl, "7") = "{ctl:7}"。
// 身份不得为空,不得含花括号(花括号会截断 tag,键落到别的 slot);违反是调用方的实现错误,panic。
func ControlTag(f ControlFamily, identity string) string {
	if identity == "" || strings.ContainsAny(identity, "{}") {
		panic("redisslot: invalid control tag identity " + strconv.Quote(identity))
	}
	return "{" + f.Prefix() + ":" + identity + "}"
}

// ZoneControlTag 是按 zone 打 tag 的族(ctl、q、gm、gr)的简写,例如 ZoneControlTag(ControlFamilyLoginQueue, 3) = "{q:3}"。
func ZoneControlTag(f ControlFamily, zone uint32) string {
	return ControlTag(f, strconv.FormatUint(uint64(zone), 10))
}

// HashTag 返回键的 hash tag 内容(不含花括号)与键是否带非空 tag,取法与 HashSlot 相同。
func HashTag(key string) (string, bool) {
	return hashTagContent(key)
}

// IsControlTag 报告 tag 内容(不含花括号)是不是控制集群登记过的族 tag:<族前缀>:<非空身份>。
func IsControlTag(tag string) bool {
	prefix, identity, ok := strings.Cut(tag, ":")
	if !ok || identity == "" {
		return false
	}
	for _, known := range controlFamilyPrefixes {
		if prefix == known {
			return true
		}
	}
	return false
}

// ControlFamilyPrefixes 按枚举顺序返回全部族前缀的副本,供契约测试核对。
func ControlFamilyPrefixes() []string {
	out := make([]string, len(controlFamilyPrefixes))
	copy(out, controlFamilyPrefixes[:])
	return out
}
