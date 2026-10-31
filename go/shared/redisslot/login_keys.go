package redisslot

import "strconv"

// 登录域键的唯一构造点(docs/design/redis-player-store-cluster.md §10.2 第 11 批)。
//
// 登录域按两种身份分布,两种身份之间不同槽,同一段 Lua 只能碰其中一种:
//   - 连接:login_session 与会话栅栏,tag 是连接身份 "<gate_instance_id>:<session_incarnation>",一条连接的两个键必然同槽。
//   - 账号:设备集、账号索引、令牌、令牌家族,tag 是 AccountTag(account),一个账号的键必然同槽。
//
// 玩家键(player_locker、login_idempotent、login_generation)属于玩家域,见 player_keys.go。
// 键形只存在于本文件,别的包不要自己拼;缺构造函数就加在这里。

// AccountTag 返回账号的 hash tag 内容(不含花括号)。账号是用户输入,可能带花括号,不能直接当 tag:
// 先把账号哈希到 slot,再取这个 slot 的 SlotTag。SlotTag 是纯小写字母,没有解析歧义。
func AccountTag(account string) string {
	return SlotTag(CRC16(account) & (SlotCount - 1))
}

// ValidAccountTag 报告 tag 是不是某个 slot 的规范 SlotTag。令牌字符串里的 tag 来自客户端,拼键之前先过这一关:
// 不合法的 tag 不进键名,也不用为它多跑一次 Redis。
func ValidAccountTag(tag string) bool {
	if tag == "" || len(tag) > slotTagMaxLen {
		return false
	}
	for i := 0; i < len(tag); i++ {
		if tag[i] < 'a' || tag[i] > 'z' {
			return false
		}
	}
	return SlotTag(CRC16(tag)&(SlotCount-1)) == tag
}

// LoginConnectionID 返回一条 gate 连接的全局唯一身份。gate_instance_id 每次 gate 进程启动重新生成,
// session_incarnation 在进程内单调,两者合起来不会撞号;裸 session_id 在 gate 重启后会重新发出。
func LoginConnectionID(gateInstanceID string, incarnation uint64) string {
	return gateInstanceID + ":" + strconv.FormatUint(incarnation, 10)
}

// LoginSessionKey 是本连接登录的账号:login_session:{<连接身份>}。
func LoginSessionKey(connectionID string) string {
	return "login_session:{" + connectionID + "}"
}

// SessionFenceKey 是本连接的会话栅栏:login:sessfence:{<连接身份>}。
func SessionFenceKey(connectionID string) string {
	return "login:sessfence:{" + connectionID + "}"
}

// AccountLoginLockKey 是 Login 认证之后按账号互斥的锁。
func AccountLoginLockKey(account string) string {
	return "account_lock:login:{" + AccountTag(account) + "}:" + account
}

// AccountCreateLockKey 是建角按账号互斥的锁。
func AccountCreateLockKey(account string) string {
	return "account_lock:create:{" + AccountTag(account) + "}:" + account
}

// AccountDeviceSetKey 是账号的设备集,成员是连接身份。
func AccountDeviceSetKey(account string) string {
	return "login_account_sessions:{" + AccountTag(account) + "}:" + account
}

// AccountRefreshKey 是账号的 refresh token 索引。
func AccountRefreshKey(account string) string {
	return "account_refresh:{" + AccountTag(account) + "}:" + account
}

// AccessTokenKey 是一枚 access token 的键。tag 是令牌所属账号的 AccountTag,secret 是令牌的随机部分。
func AccessTokenKey(tag, secret string) string {
	return "access_token:{" + tag + "}:" + secret
}

// RefreshTokenKeyPrefix 是账号的 refresh token 键前缀。账号封顶淘汰的 Lua 从索引里读出 secret 后用它现拼键,
// 前缀带账号 tag,现拼出来的键与脚本声明的键同槽。
func RefreshTokenKeyPrefix(tag string) string {
	return "refresh_token:{" + tag + "}:"
}

// RefreshTokenKey 是一枚 refresh token 的键。
func RefreshTokenKey(tag, secret string) string {
	return RefreshTokenKeyPrefix(tag) + secret
}

// RefreshFamilyKeyPrefix 是账号的令牌家族键前缀,用途同 RefreshTokenKeyPrefix。
func RefreshFamilyKeyPrefix(tag string) string {
	return "refresh_family:{" + tag + "}:"
}

// RefreshFamilyKey 是一个令牌家族的键。
func RefreshFamilyKey(tag, familyID string) string {
	return RefreshFamilyKeyPrefix(tag) + familyID
}

// PasswordQuotaKey 是口令失败配额的键。fingerprint 是账号类型加账号摘要,不含账号原文。
func PasswordQuotaKey(fingerprint string) string {
	return "login:password:quota:v1:{" + fingerprint + "}"
}
