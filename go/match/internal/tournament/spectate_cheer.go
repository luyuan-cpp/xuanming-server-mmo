package tournament

import (
	"strings"
	"sync"

	matchpb "proto/match"
)

const (
	// SpectateCheerCooldownMs 单观众观战预设喝彩冷却毫秒(5 秒防刷屏)。
	SpectateCheerCooldownMs uint64 = 5000
	// spectateCheerRingLimit 单场保留的近期喝彩环形上限(按递增 seq 读取,O(1) 有界内存)。
	spectateCheerRingLimit = 32
	// spectateCheerInitialReturn after_seq=0 时首次拉取返回最近条数上限。
	spectateCheerInitialReturn = 12
	// spectateCheerRoomIdleTTLMs 空闲房间自动回收窗口(2 小时)。
	spectateCheerRoomIdleTTLMs uint64 = 2 * 3600 * 1000
	// maxSenderNameRunes 喝彩展示名最大截断字符数。
	maxSenderNameRunes = 16
)

// SpectateCheerSnapshot 单次提交或拉取返回的权威观战喝彩状态。
type SpectateCheerSnapshot struct {
	BattleID        uint64
	CountA          uint64
	CountB          uint64
	CooldownUntilMs uint64
	RecentCheers    []*matchpb.TournamentSpectateCheerItem
	ServerTimeMs    uint64
}

type spectateCheerRoom struct {
	countA       uint64
	countB       uint64
	nextSeq      uint64
	lastActiveMs uint64
	recent       []*matchpb.TournamentSpectateCheerItem
	cooldowns    map[uint64]uint64
}

// SpectateCheerHub 管理锦标赛 L0/L1/L2 通用观战预设喝彩的权威双边计数、5 秒限流与有界近期队列。
// shortcut: 进程内聚合, 多 match 副本场景下升级为 Redis HINCRBY + Stream 聚合。
type SpectateCheerHub struct {
	mu    sync.Mutex
	rooms map[uint64]*spectateCheerRoom
}

func NewSpectateCheerHub() *SpectateCheerHub {
	return &SpectateCheerHub{
		rooms: make(map[uint64]*spectateCheerRoom),
	}
}

// Send 验证白名单与 5 秒个人冷却并记录一条观战预设喝彩。
// 返回 (snapshot, cooldownHit, invalidArg)。
func (h *SpectateCheerHub) Send(battleID, playerID uint64, senderName string, phraseID, side uint32, nowMs uint64) (SpectateCheerSnapshot, bool, bool) {
	if battleID == 0 || playerID == 0 || !IsValidSpectateCheerPhrase(phraseID) || !IsValidSpectateCheerSide(side) {
		return SpectateCheerSnapshot{BattleID: battleID, ServerTimeMs: nowMs}, false, true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evictIdleLocked(nowMs)
	room := h.getOrCreateLocked(battleID, nowMs)
	room.lastActiveMs = nowMs
	if until := room.cooldowns[playerID]; until > nowMs {
		return SpectateCheerSnapshot{
			BattleID:        battleID,
			CountA:          room.countA,
			CountB:          room.countB,
			CooldownUntilMs: until,
			RecentCheers:    collectCheersAfter(room.recent, 0, spectateCheerInitialReturn),
			ServerTimeMs:    nowMs,
		}, true, false
	}
	if side == 0 {
		room.countA++
	} else {
		room.countB++
	}
	room.nextSeq++
	item := &matchpb.TournamentSpectateCheerItem{
		Seq:        room.nextSeq,
		PhraseId:   phraseID,
		Side:       side,
		SenderName: sanitizeCheerSenderName(senderName),
		CreatedMs:  nowMs,
	}
	room.recent = append(room.recent, item)
	if len(room.recent) > spectateCheerRingLimit {
		room.recent = append([]*matchpb.TournamentSpectateCheerItem(nil), room.recent[len(room.recent)-spectateCheerRingLimit:]...)
	}
	until := nowMs + SpectateCheerCooldownMs
	room.cooldowns[playerID] = until
	return SpectateCheerSnapshot{
		BattleID:        battleID,
		CountA:          room.countA,
		CountB:          room.countB,
		CooldownUntilMs: until,
		RecentCheers:    []*matchpb.TournamentSpectateCheerItem{cloneCheerItem(item)},
		ServerTimeMs:    nowMs,
	}, false, false
}

// Snapshot 只读查询某场战斗当前观战人气与 seq > afterSeq 的新增喝彩。
func (h *SpectateCheerHub) Snapshot(battleID, playerID, afterSeq, nowMs uint64) SpectateCheerSnapshot {
	snap := SpectateCheerSnapshot{BattleID: battleID, ServerTimeMs: nowMs}
	if battleID == 0 {
		return snap
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[battleID]
	if !ok {
		return snap
	}
	snap.CountA = room.countA
	snap.CountB = room.countB
	if until := room.cooldowns[playerID]; until > nowMs {
		snap.CooldownUntilMs = until
	}
	limit := spectateCheerRingLimit
	if afterSeq == 0 {
		limit = spectateCheerInitialReturn
	}
	snap.RecentCheers = collectCheersAfter(room.recent, afterSeq, limit)
	return snap
}

func (h *SpectateCheerHub) getOrCreateLocked(battleID, nowMs uint64) *spectateCheerRoom {
	if h.rooms == nil {
		h.rooms = make(map[uint64]*spectateCheerRoom)
	}
	room, ok := h.rooms[battleID]
	if !ok {
		room = &spectateCheerRoom{
			lastActiveMs: nowMs,
			cooldowns:    make(map[uint64]uint64),
		}
		h.rooms[battleID] = room
	}
	return room
}

func (h *SpectateCheerHub) evictIdleLocked(nowMs uint64) {
	if len(h.rooms) < 64 {
		return
	}
	for id, room := range h.rooms {
		if nowMs > room.lastActiveMs && nowMs-room.lastActiveMs >= spectateCheerRoomIdleTTLMs {
			delete(h.rooms, id)
		}
	}
}

func collectCheersAfter(items []*matchpb.TournamentSpectateCheerItem, afterSeq uint64, limit int) []*matchpb.TournamentSpectateCheerItem {
	if len(items) == 0 {
		return nil
	}
	var out []*matchpb.TournamentSpectateCheerItem
	for _, it := range items {
		if it.Seq > afterSeq {
			out = append(out, cloneCheerItem(it))
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func cloneCheerItem(it *matchpb.TournamentSpectateCheerItem) *matchpb.TournamentSpectateCheerItem {
	if it == nil {
		return nil
	}
	return &matchpb.TournamentSpectateCheerItem{
		Seq:        it.Seq,
		PhraseId:   it.PhraseId,
		Side:       it.Side,
		SenderName: it.SenderName,
		CreatedMs:  it.CreatedMs,
	}
}

func sanitizeCheerSenderName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "观战道友"
	}
	runes := []rune(trimmed)
	if len(runes) > maxSenderNameRunes {
		return string(runes[:maxSenderNameRunes])
	}
	return trimmed
}
