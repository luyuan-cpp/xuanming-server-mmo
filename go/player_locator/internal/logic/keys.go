package logic

import "fmt"

const (
	locationKeyPrefix        = "player:location:"
	sessionKeyPrefix         = "player:session:"
	sceneLocationKeyPrefix   = "player:"
	LeaseZSetKey             = "player:leases"
	LeaseProcessingZSetKey   = "player:leases:processing"
	leaseClaimTokenHashKey   = "player:leases:claim_tokens"
	leaseClaimPayloadHashKey = "player:leases:claim_payloads"
)

func locationKey(uid int64) string {
	return fmt.Sprintf("%s%d", locationKeyPrefix, uid)
}

func sessionKey(playerID uint64) string {
	return fmt.Sprintf("%s%d", sessionKeyPrefix, playerID)
}

// sceneManagerLocationKey 是 SceneManager 的位置权威键。player_locator 自己的
// player:location:{id} 是历史 RPC 的另一套 key，生产入场链不会写它。
func sceneManagerLocationKey(playerID uint64) string {
	return fmt.Sprintf("%s%d:location", sceneLocationKeyPrefix, playerID)
}
