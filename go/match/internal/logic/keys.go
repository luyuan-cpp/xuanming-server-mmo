package logic

import "fmt"

// Redis key 契约(设计文档 turn-based-battle-server.md §5.4 / §6):
//
//	match:queue:{mode}:{battle_config_id}  list  排队玩家 id,FIFO(Rpush 入队尾,Lpop 出队首)
//	match:ticket:{player_id}               hash  排队票据(状态机:queued/matched/ready)
//	match:matcher:lock:{mode}:{config}     锁    matcher 凑单临界区(多实例 SETNX)
//	battle:lock:{player_id}                锁    scene 写,match 只做咨询性读(权威在 InBattleComp)
//	challenge:{id}                         hash  切磋记录(TTL 60s)
//	challenge:target:{player_id}           锁    同一目标同时只挂一个待应答挑战

func matchQueueKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:queue:%d:%d", mode, battleConfigId)
}

func matchTicketKey(playerId uint64) string {
	return fmt.Sprintf("match:ticket:%d", playerId)
}

func matcherLockKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:matcher:lock:%d:%d", mode, battleConfigId)
}

func battleLockKey(playerId uint64) string {
	return fmt.Sprintf("battle:lock:%d", playerId)
}

func challengeKey(challengeId uint64) string {
	return fmt.Sprintf("challenge:%d", challengeId)
}

func challengeTargetKey(playerId uint64) string {
	return fmt.Sprintf("challenge:target:%d", playerId)
}

// getPlayerLocationKey 是 scene_manager 维护的玩家位置权威键
// (共享契约记录见设计文档 §5.4;写者:go/scene_manager changesceneutil.go)。
func getPlayerLocationKey(playerId uint64) string {
	return fmt.Sprintf("player:%d:location", playerId)
}

// playerSessionKey 是 player_locator 维护的会话键(PlayerSession proto),
// 挑战推送按它定位目标玩家的 gate(照 guild online_status_resolver 的读法)。
func playerSessionKey(playerId uint64) string {
	return fmt.Sprintf("player:session:%d", playerId)
}
