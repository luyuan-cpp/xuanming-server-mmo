package redisslot

import (
	"strconv"
	"strings"
)

// 玩家域键的唯一构造点(docs/design/redis-player-store-cluster.md §5.2)。
//
// 规则:键里的玩家 id 用 PlayerTag 包住,同一玩家的全部键落在同一个 slot,多键 Lua 才合法。
// 这里的字面量是跨语言契约,与 C++ 的 player_ownership_comp.h、battle_lock_rules.h、player_landing.h
// 逐字一致;两侧金样钉同一组值(player_keys_test.go、cpp/tests/currency_test 的
// PlayerDomainRedisKeysMatchGoContract),改任何一个都要两侧同批改,上线后不得改。
//
// 已有归属包的键不在这里:owner_epoch 与已落盘标记在 shared/ownerepoch,交接资格在 shared/handofffence,
// 同步摘要在 shared/playersync。别的包不要自己 Sprintf 这些键,缺构造函数就加在这里。

// playerKey 拼 player:{<id>}:<suffix>。
func playerKey(playerID uint64, suffix string) string {
	return "player:" + PlayerTag(playerID) + ":" + suffix
}

// LocationKey 是 scene_manager 维护的玩家位置权威键(值为 PlayerLocation protobuf)。
func LocationKey(playerID uint64) string { return playerKey(playerID, "location") }

// LocationRevisionKey 是位置的归属修订侧车(与 location 同一段 Lua 维护)。
func LocationRevisionKey(playerID uint64) string { return playerKey(playerID, "location_revision") }

// LocationRevisionFloorKey 是已观察到的归属修订下限,不随 location 删除。
func LocationRevisionFloorKey(playerID uint64) string {
	return playerKey(playerID, "location_revision_floor")
}

// LeaveReleaseWaitKey 是离场等待持有者释放的计时键。
func LeaveReleaseWaitKey(playerID uint64) string {
	return playerKey(playerID, "leave_release_wait")
}

// ChannelSwitchCooldownKey 是玩家主动切线的冷却键。
func ChannelSwitchCooldownKey(playerID uint64) string {
	return playerKey(playerID, "channel_switch_cooldown")
}

// EnterSceneDedupKey 是 EnterScene 的请求去重键。request_id 的唯一性只在玩家作用域内成立,所以键里带玩家。
func EnterSceneDedupKey(playerID uint64, requestID string) string {
	return "enter_scene:dedup:" + PlayerTag(playerID) + ":" + requestID
}

// LoginGenerationKey 是 login 铸造登录代次的键。
func LoginGenerationKey(playerID uint64) string { return playerKey(playerID, "login_generation") }

// PlayerLockerKey 是 login 的玩家互斥锁键。
func PlayerLockerKey(playerID uint64) string { return "player_locker:" + PlayerTag(playerID) }

// LoginIdempotentKey 是 login 的请求幂等键。
func LoginIdempotentKey(playerID uint64, requestID string) string {
	return "login_idempotent:" + PlayerTag(playerID) + ":" + requestID
}

// BattleLockKey 是 C++ scene 写的战斗串行化锁(值是十进制 battle_id),guild、match 只读。
func BattleLockKey(playerID uint64) string { return "battle:lock:" + PlayerTag(playerID) }

// BattleCtxKey 是战斗锁的伴生上下文,scene 私有;Go 不读写它,这里只为金样与同槽断言。
func BattleCtxKey(playerID uint64) string { return "battle:ctx:" + PlayerTag(playerID) }

// AfkPassKey 是挂机通行(月卡)键,值为到期 unix 秒。
func AfkPassKey(playerID uint64) string { return "player:afk_pass:" + PlayerTag(playerID) }

const (
	sessionKeyPrefix = "player:session:"
	// SessionKeyPattern 是扫全部会话键的 SCAN 模式。
	SessionKeyPattern = sessionKeyPrefix + "{*}"
)

// SessionKey 是 player_locator 维护的会话键(值为 PlayerSession protobuf)。
func SessionKey(playerID uint64) string { return sessionKeyPrefix + PlayerTag(playerID) }

// ParseSessionKey 是 SessionKey 的逆,给按 SessionKeyPattern 扫出来的键取回玩家 id。不是这个键形
// (含前导零、0、超过 uint64)时 ok=false。
func ParseSessionKey(key string) (playerID uint64, ok bool) {
	body, found := strings.CutPrefix(key, sessionKeyPrefix+"{")
	if !found {
		return 0, false
	}
	text, found := strings.CutSuffix(body, "}")
	if !found || text == "" || text[0] == '0' {
		return 0, false
	}
	id, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
