package spectatechat

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	matchpb "proto/match"

	redv9 "github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

const (
	// RingLimit 单场战斗观战交流室保留的最近消息条数上限（O(1) 有界内存/Redis）。
	RingLimit = 60
	// DefaultPullLimit after_seq=0 首次拉取默认返回条数。
	DefaultPullLimit = 20
	// MaxSenderNameRunes 发言者展示名最大字符数。
	MaxSenderNameRunes = 16
	// closedGraceTTL 终局关闭后保留消息供尾帧拉取的宽限期。
	closedGraceTTL = 120 * time.Second
	// memIdleTTLMs 内存兜底模式空闲房间自动清理窗口（2 小时）。
	memIdleTTLMs uint64 = 2 * 3600 * 1000
	// memCooldownPruneThreshold 内存兜底模式单房冷却表剪枝阈值。
	memCooldownPruneThreshold = 1024
)

// SendResult 是单次发送尝试的原子裁决结果。
type SendResult struct {
	Message         *matchpb.BattleSpectateChatMessage
	CooldownUntilMs uint64
	HitCooldown     bool
	HitRoomBusy     bool
	RoomClosed      bool
}

// PullResult 是增量拉取的结果快照。
type PullResult struct {
	Messages        []*matchpb.BattleSpectateChatMessage
	LatestSeq       uint64
	CooldownUntilMs uint64
	RoomClosed      bool
}

// sendLua 在 match 私有 Redis 集群的单槽上原子完成：
//   1. 检查终局关闭标记（KEYS[5]）；
//   2. 检查单人冷却（KEYS[3] HGET playerId > nowMs）；
//   3. 检查单房 1 秒窗口总流速上限（KEYS[4]）；
//   4. 递增单调序号（KEYS[1]）、追加有界 60 条环形队列（KEYS[2]）、刷新冷却与 TTL。
//
// 返回值：{status, seqOrCooldown}：
//   status=0 成功（val=新 seq）
//   status=1 命中个人冷却（val=cooldownUntilMs）
//   status=2 命中单房总流速上限（val=0）
//   status=3 房间已终局关闭（val=0）
var sendLua = redv9.NewScript(`
if redis.call('EXISTS', KEYS[5]) == 1 then
  return {3, 0}
end
local nowMs = tonumber(ARGV[1])
local cooldownMs = tonumber(ARGV[2])
local playerId = ARGV[3]
local roomLimit = tonumber(ARGV[4])
local ttlSec = tonumber(ARGV[5])
local ringLimit = tonumber(ARGV[6])
local payload = ARGV[7]

local prevUntil = tonumber(redis.call('HGET', KEYS[3], playerId) or '0')
if prevUntil > nowMs then
  return {1, prevUntil}
end

local nowSec = math.floor(nowMs / 1000)
local rateRaw = redis.call('GET', KEYS[4])
local rateSec = 0
local rateCount = 0
if rateRaw then
  local sep = string.find(rateRaw, ':', 1, true)
  if sep then
    rateSec = tonumber(string.sub(rateRaw, 1, sep - 1)) or 0
    rateCount = tonumber(string.sub(rateRaw, sep + 1)) or 0
  end
end
if rateSec == nowSec then
  if rateCount >= roomLimit then
    return {2, 0}
  end
  rateCount = rateCount + 1
else
  rateSec = nowSec
  rateCount = 1
end
redis.call('SET', KEYS[4], tostring(rateSec) .. ':' .. tostring(rateCount), 'EX', ttlSec)

local nextUntil = nowMs + cooldownMs
redis.call('HSET', KEYS[3], playerId, tostring(nextUntil))
redis.call('EXPIRE', KEYS[3], ttlSec)

local seq = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ttlSec)

redis.call('RPUSH', KEYS[2], tostring(seq) .. ':' .. payload)
redis.call('LTRIM', KEYS[2], -ringLimit, -1)
redis.call('EXPIRE', KEYS[2], ttlSec)
return {0, seq}
`)

type memRoom struct {
	nextSeq      uint64
	lastActiveMs uint64
	closed       bool
	rateSec      uint64
	rateCount    int
	messages     []*matchpb.BattleSpectateChatMessage
	cooldowns    map[uint64]uint64
}

// Hub 管理「局外人观战交流模式」的有界房间队列、个人冷却与单房总流速限流。
// 生产装配 MatchRedis 时走单槽 Lua 跨副本共享；单测未装配 Redis 时自动走同语义内存实现。
type Hub struct {
	redis redv9.UniversalClient

	mu    sync.Mutex
	rooms map[uint64]*memRoom
}

func NewHub(matchRedis redv9.UniversalClient) *Hub {
	return &Hub{
		redis: matchRedis,
		rooms: make(map[uint64]*memRoom),
	}
}

func keySeq(battleID uint64) string    { return fmt.Sprintf("spectate:chat:seq:{%d}", battleID) }
func keyMsgs(battleID uint64) string   { return fmt.Sprintf("spectate:chat:msgs:{%d}", battleID) }
func keyCD(battleID uint64) string     { return fmt.Sprintf("spectate:chat:cd:{%d}", battleID) }
func keyRate(battleID uint64) string   { return fmt.Sprintf("spectate:chat:rate:{%d}", battleID) }
func keyClosed(battleID uint64) string { return fmt.Sprintf("spectate:chat:closed:{%d}", battleID) }

// SanitizeSenderName 截断并兜底发言者公开展示名。
func SanitizeSenderName(raw string, role matchpb.BattleSpectateChatRole) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if role == matchpb.BattleSpectateChatRole_BATTLE_SPECTATE_CHAT_ROLE_PARTICIPANT {
			return "参战道友"
		}
		return "观战道友"
	}
	runes := []rune(trimmed)
	if len(runes) > MaxSenderNameRunes {
		return string(runes[:MaxSenderNameRunes])
	}
	return trimmed
}

// Send 原子校验冷却、单房流速并追加一条已通过 ContentGuard 审核的观战交流消息。
func (h *Hub) Send(
	ctx context.Context,
	battleID, playerID uint64,
	senderName string,
	role matchpb.BattleSpectateChatRole,
	content string,
	nowMs uint64,
	ttlSeconds int,
	cooldownMs uint64,
	roomRateLimit int,
) (SendResult, error) {
	if ttlSeconds < 60 {
		ttlSeconds = 60
	}
	if roomRateLimit <= 0 {
		roomRateLimit = 8
	}
	msg := &matchpb.BattleSpectateChatMessage{
		SenderPlayerId: playerID,
		SenderName:     SanitizeSenderName(senderName, role),
		Role:           role,
		Content:        content,
		CreatedMs:      nowMs,
	}
	if h.redis != nil {
		return h.sendRedis(ctx, battleID, playerID, msg, nowMs, ttlSeconds, cooldownMs, roomRateLimit)
	}
	return h.sendMemory(battleID, playerID, msg, nowMs, cooldownMs, roomRateLimit), nil
}

func (h *Hub) sendRedis(
	ctx context.Context,
	battleID, playerID uint64,
	msg *matchpb.BattleSpectateChatMessage,
	nowMs uint64,
	ttlSeconds int,
	cooldownMs uint64,
	roomRateLimit int,
) (SendResult, error) {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return SendResult{}, err
	}
	keys := []string{keySeq(battleID), keyMsgs(battleID), keyCD(battleID), keyRate(battleID), keyClosed(battleID)}
	res, err := sendLua.Run(ctx, h.redis, keys,
		strconv.FormatUint(nowMs, 10),
		strconv.FormatUint(cooldownMs, 10),
		strconv.FormatUint(playerID, 10),
		strconv.Itoa(roomRateLimit),
		strconv.Itoa(ttlSeconds),
		strconv.Itoa(RingLimit),
		string(raw),
	).Int64Slice()
	if err != nil {
		return SendResult{}, err
	}
	if len(res) < 2 {
		return SendResult{}, fmt.Errorf("spectatechat: unexpected lua reply len=%d", len(res))
	}
	switch res[0] {
	case 0:
		msg.Seq = uint64(res[1])
		return SendResult{
			Message:         msg,
			CooldownUntilMs: nowMs + cooldownMs,
		}, nil
	case 1:
		return SendResult{
			CooldownUntilMs: uint64(res[1]),
			HitCooldown:     true,
		}, nil
	case 2:
		return SendResult{HitRoomBusy: true}, nil
	case 3:
		return SendResult{RoomClosed: true}, nil
	default:
		return SendResult{}, fmt.Errorf("spectatechat: unknown lua status=%d", res[0])
	}
}

func (h *Hub) sendMemory(
	battleID, playerID uint64,
	msg *matchpb.BattleSpectateChatMessage,
	nowMs, cooldownMs uint64,
	roomRateLimit int,
) SendResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evictIdleLocked(nowMs)
	room := h.getOrCreateLocked(battleID, nowMs)
	if room.closed {
		return SendResult{RoomClosed: true}
	}
	room.lastActiveMs = nowMs
	if until := room.cooldowns[playerID]; until > nowMs {
		return SendResult{CooldownUntilMs: until, HitCooldown: true}
	}
	nowSec := nowMs / 1000
	if room.rateSec == nowSec {
		if room.rateCount >= roomRateLimit {
			return SendResult{HitRoomBusy: true}
		}
		room.rateCount++
	} else {
		room.rateSec = nowSec
		room.rateCount = 1
	}
	room.nextSeq++
	msg.Seq = room.nextSeq
	stored := cloneMessage(msg)
	room.messages = append(room.messages, stored)
	if len(room.messages) > RingLimit {
		room.messages = append([]*matchpb.BattleSpectateChatMessage(nil), room.messages[len(room.messages)-RingLimit:]...)
	}
	nextUntil := nowMs + cooldownMs
	room.cooldowns[playerID] = nextUntil
	if len(room.cooldowns) > memCooldownPruneThreshold {
		for pid, u := range room.cooldowns {
			if u <= nowMs {
				delete(room.cooldowns, pid)
			}
		}
	}
	return SendResult{
		Message:         cloneMessage(stored),
		CooldownUntilMs: nextUntil,
	}
}

// Pull 只读查询指定战斗的观战交流增量消息（seq > afterSeq）。
func (h *Hub) Pull(ctx context.Context, battleID, playerID, afterSeq uint64, limit int, nowMs uint64) (PullResult, error) {
	if limit <= 0 {
		if afterSeq == 0 {
			limit = DefaultPullLimit
		} else {
			limit = RingLimit
		}
	}
	if limit > RingLimit {
		limit = RingLimit
	}
	if h.redis != nil {
		return h.pullRedis(ctx, battleID, playerID, afterSeq, limit, nowMs)
	}
	return h.pullMemory(battleID, playerID, afterSeq, limit, nowMs), nil
}

func (h *Hub) pullRedis(ctx context.Context, battleID, playerID, afterSeq uint64, limit int, nowMs uint64) (PullResult, error) {
	var out PullResult
	rawList, err := h.redis.LRange(ctx, keyMsgs(battleID), 0, -1).Result()
	if err != nil && err != redv9.Nil {
		return out, err
	}
	closedCount, err := h.redis.Exists(ctx, keyClosed(battleID)).Result()
	if err != nil && err != redv9.Nil {
		return out, err
	}
	out.RoomClosed = closedCount > 0
	if playerID != 0 {
		if cdStr, cdErr := h.redis.HGet(ctx, keyCD(battleID), strconv.FormatUint(playerID, 10)).Result(); cdErr == nil {
			if until, parseErr := strconv.ParseUint(cdStr, 10, 64); parseErr == nil && until > nowMs {
				out.CooldownUntilMs = until
			}
		}
	}
	var filtered []*matchpb.BattleSpectateChatMessage
	for _, entry := range rawList {
		msg, ok := decodeEntry([]byte(entry))
		if !ok {
			continue
		}
		if msg.Seq > out.LatestSeq {
			out.LatestSeq = msg.Seq
		}
		if msg.Seq > afterSeq {
			filtered = append(filtered, msg)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	out.Messages = filtered
	return out, nil
}

func (h *Hub) pullMemory(battleID, playerID, afterSeq uint64, limit int, nowMs uint64) PullResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out PullResult
	room, ok := h.rooms[battleID]
	if !ok {
		return out
	}
	out.RoomClosed = room.closed
	out.LatestSeq = room.nextSeq
	if until := room.cooldowns[playerID]; until > nowMs {
		out.CooldownUntilMs = until
	}
	var filtered []*matchpb.BattleSpectateChatMessage
	for _, m := range room.messages {
		if m.Seq > afterSeq {
			filtered = append(filtered, cloneMessage(m))
		}
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	out.Messages = filtered
	return out
}

// MarkEnded 在战斗终局或回收时关闭该场观战交流室（拒收新发言，并缩短残留键 TTL）。
func (h *Hub) MarkEnded(ctx context.Context, battleID uint64) {
	if battleID == 0 {
		return
	}
	if h.redis != nil {
		_ = h.redis.Set(ctx, keyClosed(battleID), "1", closedGraceTTL).Err()
		_ = h.redis.Expire(ctx, keyMsgs(battleID), closedGraceTTL).Err()
		_ = h.redis.Expire(ctx, keySeq(battleID), closedGraceTTL).Err()
		_ = h.redis.Expire(ctx, keyCD(battleID), closedGraceTTL).Err()
		_ = h.redis.Expire(ctx, keyRate(battleID), closedGraceTTL).Err()
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if room, ok := h.rooms[battleID]; ok {
		room.closed = true
	}
}

func (h *Hub) getOrCreateLocked(battleID, nowMs uint64) *memRoom {
	if h.rooms == nil {
		h.rooms = make(map[uint64]*memRoom)
	}
	room, ok := h.rooms[battleID]
	if !ok {
		room = &memRoom{
			lastActiveMs: nowMs,
			cooldowns:    make(map[uint64]uint64),
		}
		h.rooms[battleID] = room
	}
	return room
}

func (h *Hub) evictIdleLocked(nowMs uint64) {
	if len(h.rooms) < 64 {
		return
	}
	for id, room := range h.rooms {
		if nowMs > room.lastActiveMs && nowMs-room.lastActiveMs >= memIdleTTLMs {
			delete(h.rooms, id)
		}
	}
}

func decodeEntry(raw []byte) (*matchpb.BattleSpectateChatMessage, bool) {
	sep := bytes.IndexByte(raw, ':')
	if sep <= 0 || sep >= len(raw)-1 {
		return nil, false
	}
	seq, err := strconv.ParseUint(string(raw[:sep]), 10, 64)
	if err != nil || seq == 0 {
		return nil, false
	}
	msg := &matchpb.BattleSpectateChatMessage{}
	if err := proto.Unmarshal(raw[sep+1:], msg); err != nil {
		return nil, false
	}
	msg.Seq = seq
	return msg, true
}

func cloneMessage(m *matchpb.BattleSpectateChatMessage) *matchpb.BattleSpectateChatMessage {
	if m == nil {
		return nil
	}
	return &matchpb.BattleSpectateChatMessage{
		Seq:            m.Seq,
		SenderPlayerId: m.SenderPlayerId,
		SenderName:     m.SenderName,
		Role:           m.Role,
		Content:        m.Content,
		CreatedMs:      m.CreatedMs,
	}
}
