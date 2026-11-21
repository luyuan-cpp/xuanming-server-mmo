package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"chat/internal/constants"
	chatpb "proto/chat"
)

const (
	profileCacheMaxEntries = 1 << 16
	maxRepeatScanRows      = 64
	minSpamNormalizedRunes = 4
	maxRetractReasonBytes  = 128
	maxReportDetailBytes   = 200
)

// PlayerProfile 是防刷/等级门槛所需的玩家基础档案(由 data_service 提供并带短 TTL 缓存)。
type PlayerProfile struct {
	Level            uint32
	AccountCreatedMs uint64
}

// ProfileReader 按 player_id 读取角色等级与账号创建时间。
type ProfileReader interface {
	GetPlayerProfile(ctx context.Context, playerID uint64) (PlayerProfile, error)
}

// ProfileReaderFunc 函数适配器。
type ProfileReaderFunc func(ctx context.Context, playerID uint64) (PlayerProfile, error)

func (f ProfileReaderFunc) GetPlayerProfile(ctx context.Context, playerID uint64) (PlayerProfile, error) {
	return f(ctx, playerID)
}

// WithProfileReader 装配角色等级与新号创角时间查询器(来自 data_service)。
func WithProfileReader(reader ProfileReader) Option {
	return func(s *Store) {
		if reader != nil {
			s.profiles = reader
		}
	}
}

type cachedProfile struct {
	profile PlayerProfile
	expiry  time.Duration
}

type profileCache struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Duration

	mu      sync.Mutex
	entries map[uint64]cachedProfile
}

func newProfileCache(ttl time.Duration, maxEntries int, now func() time.Duration) *profileCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &profileCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        now,
		entries:    make(map[uint64]cachedProfile),
	}
}

func (c *profileCache) get(ctx context.Context, reader ProfileReader, playerID uint64) (PlayerProfile, error) {
	if c == nil || reader == nil {
		return PlayerProfile{}, nil
	}
	started := c.now()
	c.mu.Lock()
	if ent, ok := c.entries[playerID]; ok && started < ent.expiry {
		c.mu.Unlock()
		return ent.profile, nil
	}
	c.mu.Unlock()

	profile, err := reader.GetPlayerProfile(ctx, playerID)
	if err != nil {
		return PlayerProfile{}, err
	}
	c.mu.Lock()
	if _, exists := c.entries[playerID]; !exists && len(c.entries) >= c.maxEntries {
		clear(c.entries)
	}
	c.entries[playerID] = cachedProfile{profile: profile, expiry: started + c.ttl}
	c.mu.Unlock()
	return profile, nil
}

// loadPlayerProfile 根据当前规则决定是否需要读玩家档案(世界最低等级 / 新号私聊人数限制)。
func (s *Store) loadPlayerProfile(ctx context.Context, actor uint64, peer uint64) (PlayerProfile, bool, error) {
	needLevel := peer == 0 && s.rules.WorldMinLevel > 0
	needAge := peer != 0 && s.rules.NewAccountAgeHours > 0 && s.rules.NewAccountDailyPrivateLimit > 0
	if (!needLevel && !needAge) || s.profiles == nil {
		return PlayerProfile{}, false, nil
	}
	profile, err := s.profileCache.get(ctx, s.profiles, actor)
	if err != nil {
		return PlayerProfile{}, true, err
	}
	return profile, true, nil
}

// normalizeSpamHash 忽略空白、标点、全角/小写差异计算归一化内容摘要(用于跨副本重复刷屏判定)。
func normalizeSpamHash(content string) ([32]byte, bool) {
	var buf bytes.Buffer
	runes := 0
	for _, r := range content {
		if r >= 'Ａ' && r <= 'Ｚ' {
			r = 'a' + (r - 'Ａ')
		} else if r >= 'ａ' && r <= 'ｚ' {
			r = 'a' + (r - 'ａ')
		} else if r >= '０' && r <= '９' {
			r = '0' + (r - '０')
		} else if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsControl(r) {
			continue
		}
		buf.WriteRune(r)
		runes++
	}
	if runes < minSpamNormalizedRunes {
		return [32]byte{}, false
	}
	return sha256.Sum256(buf.Bytes()), true
}

func (s *Store) checkAndRecordRepeatSpam(ctx context.Context, tx *sql.Tx, actor, now uint64, content string) (uint32, error) {
	if s.rules.DuplicateSpamWindowMs <= 0 || (s.rules.DuplicateSpamMaxSenders <= 0 && s.rules.DuplicateSpamMaxPerSender <= 0) {
		return 0, nil
	}
	hash, ok := normalizeSpamHash(content)
	if !ok {
		return 0, nil
	}
	since := uint64(0)
	if win := uint64(s.rules.DuplicateSpamWindowMs); now > win {
		since = now - win
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT sender_player_id FROM chat_repeat_event WHERE content_hash=? AND accepted_ms>=? LIMIT ?",
		hash[:], since, maxRepeatScanRows,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	others := make(map[uint64]struct{})
	selfCount := 0
	for rows.Next() {
		var sender uint64
		if scanErr := rows.Scan(&sender); scanErr != nil {
			return 0, scanErr
		}
		if sender == actor {
			selfCount++
		} else {
			others[sender] = struct{}{}
		}
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if s.rules.DuplicateSpamMaxSenders > 0 && len(others) >= s.rules.DuplicateSpamMaxSenders {
		return constants.ErrDuplicateSpam, nil
	}
	if s.rules.DuplicateSpamMaxPerSender > 0 && selfCount >= s.rules.DuplicateSpamMaxPerSender {
		return constants.ErrDuplicateSpam, nil
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT IGNORE INTO chat_repeat_event(content_hash,accepted_ms,sender_player_id,accepted_at) VALUES (?,?,?,?)",
		hash[:], now, actor, acceptedAt(now),
	); err != nil {
		return 0, err
	}
	return 0, nil
}

func utcDayYMD(ms uint64) uint32 {
	t := time.UnixMilli(int64(ms)).UTC()
	return uint32(t.Year()*10000 + int(t.Month())*100 + t.Day())
}

func (s *Store) checkAndRecordPrivateTarget(ctx context.Context, tx *sql.Tx, actor, peer, now uint64, profile PlayerProfile, hasProfile bool) (uint32, error) {
	if peer == 0 || s.rules.NewAccountAgeHours <= 0 || s.rules.NewAccountDailyPrivateLimit <= 0 || !hasProfile {
		return 0, nil
	}
	ageLimitMs := uint64(s.rules.NewAccountAgeHours) * 3600 * 1000
	if profile.AccountCreatedMs > 0 && now > profile.AccountCreatedMs && (now-profile.AccountCreatedMs) >= ageLimitMs {
		return 0, nil
	}
	day := utcDayYMD(now)
	var exists int
	err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM chat_private_daily_target WHERE sender_player_id=? AND day_ymd=? AND target_player_id=? LIMIT 1",
		actor, day, peer,
	).Scan(&exists)
	if err == nil {
		return 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var distinctCount int
	if err = tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM chat_private_daily_target WHERE sender_player_id=? AND day_ymd=?",
		actor, day,
	).Scan(&distinctCount); err != nil {
		return 0, err
	}
	if distinctCount >= s.rules.NewAccountDailyPrivateLimit {
		return constants.ErrPrivateTargetLimit, nil
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT IGNORE INTO chat_private_daily_target(sender_player_id,day_ymd,target_player_id,accepted_at) VALUES (?,?,?,?)",
		actor, day, peer, acceptedAt(now),
	); err != nil {
		return 0, err
	}
	return 0, nil
}

type retractedKey struct {
	sender     uint64
	acceptedMs uint64
}

func loadRetractedWorldKeys(ctx context.Context, tx *sql.Tx, minMs uint64) (map[retractedKey]struct{}, error) {
	rows, err := tx.QueryContext(ctx, "SELECT sender_player_id,accepted_ms FROM chat_world_retraction WHERE accepted_ms>=?", minMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[retractedKey]struct{}{}
	for rows.Next() {
		var k retractedKey
		if err = rows.Scan(&k.sender, &k.acceptedMs); err != nil {
			return nil, err
		}
		out[k] = struct{}{}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func filterRetractedRows(rows []historyRow, retracted map[retractedKey]struct{}) []historyRow {
	if len(retracted) == 0 || len(rows) == 0 {
		return rows
	}
	filtered := rows[:0]
	for _, r := range rows {
		if _, gone := retracted[retractedKey{sender: r.sender, acceptedMs: r.acceptedMs}]; gone {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// RetractWorld 追加写一条世界频道撤回记录并失效本地近窗快照;运行账号无 UPDATE/DELETE,不改 chat_send_authority。
func (s *Store) RetractWorld(ctx context.Context, senderPlayerID uint64, acceptedMs int64, reason string) (bool, uint32, error) {
	if senderPlayerID == 0 || acceptedMs <= 0 || len(reason) > maxRetractReasonBytes || !utf8.ValidString(reason) {
		return false, 0, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()

	var found int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM chat_send_authority WHERE player_id=? AND state=1 AND accepted_ms=? AND stream_key=? LIMIT 1",
		senderPlayerID, uint64(acceptedMs), []byte(worldStreamKey(senderPlayerID)),
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, constants.ErrMessageNotFound, nil
	}
	if err != nil {
		return false, 0, err
	}
	now, err := s.clock(ctx, tx)
	if err != nil {
		return false, 0, err
	}
	res, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO chat_world_retraction(sender_player_id,accepted_ms,retracted_ms,reason,accepted_at) VALUES (?,?,?,?,?)",
		senderPlayerID, uint64(acceptedMs), now, []byte(reason), acceptedAt(uint64(acceptedMs)),
	)
	if err != nil {
		return false, 0, err
	}
	if err = tx.Commit(); err != nil {
		return false, 0, err
	}
	affected, _ := res.RowsAffected()
	if s.world != nil {
		s.world.invalidate()
	}
	return affected > 0, 0, nil
}

// ReportMessage 核对原消息存在且对当前举报人可见后,将举报写入追加审计表 chat_report。
func (s *Store) ReportMessage(ctx context.Context, in *chatpb.ReportChatRequest) (uint32, error) {
	if in == nil || in.GetTargetSenderPlayerId() == 0 || in.GetSendTimeMs() <= 0 ||
		in.GetReason() == 0 || in.GetReason() > 4 || len(in.GetDetail()) > maxReportDetailBytes || !utf8.ValidString(in.GetDetail()) {
		return 0, ErrInvalid
	}
	if in.GetChannel() != chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD &&
		in.GetChannel() != chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE {
		return 0, ErrInvalid
	}
	reporter, _, err := s.verifyActor(ctx)
	if err != nil {
		return 0, err
	}
	if in.GetChannel() == chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE && reporter == in.GetTargetSenderPlayerId() {
		return 0, ErrInvalid
	}

	expectedStream := worldStreamKey(in.GetTargetSenderPlayerId())
	if in.GetChannel() == chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE {
		expectedStream = streamKey(reporter, in.GetTargetSenderPlayerId())
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM chat_send_authority WHERE player_id=? AND state=1 AND accepted_ms=? AND stream_key=? LIMIT 1",
		in.GetTargetSenderPlayerId(), uint64(in.GetSendTimeMs()), []byte(expectedStream),
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return constants.ErrMessageNotFound, nil
	}
	if err != nil {
		return 0, err
	}
	now, err := s.clock(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT IGNORE INTO chat_report(reporter_player_id,channel,target_sender_player_id,send_time_ms,reason,detail,reported_ms,accepted_at) VALUES (?,?,?,?,?,?,?,?)",
		reporter, uint8(in.GetChannel()), in.GetTargetSenderPlayerId(), uint64(in.GetSendTimeMs()), in.GetReason(), []byte(strings.TrimSpace(in.GetDetail())), now, acceptedAt(now),
	); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return 0, nil
}
