package constants

import "fmt"

const (
	RedisKeyPrefixAccountData = "account"
)

func GetAccountDataKey(account string) string {
	return fmt.Sprintf("%s:%s", RedisKeyPrefixAccountData, account)
}

// PlayerToAccountKey 是历史反向缓存键；当前归属只认全局 SQL 目录，不授权入场或自愈。
// Used by rollback orphan cleanup to find which account owns a character.
func PlayerToAccountKey(playerId uint64) string {
	return fmt.Sprintf("player_to_account:%d", playerId)
}
