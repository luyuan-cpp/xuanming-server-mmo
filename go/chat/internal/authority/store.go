package authority

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"chat/internal/constants"
	"chat/internal/textfilter"
	chatpb "proto/chat"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/rolefence"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

var (
	ErrOriginal = errors.New("聊天原会话角色归属已变化或被冻结")
	ErrConflict = errors.New("聊天原请求号已用于不同请求")
	ErrInvalid  = errors.New("聊天参数非法")
	ErrPolicy   = errors.New("聊天全局规则与已登记原件不一致")
)

const acceptedState uint32 = 1
const dbNow = "CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)"
const receiptColumns = "owner_account,owner_revision,request_digest,request_payload,message_payload,stream_key,state,accepted_ms,result_tip"

// 发送原件只增不改(运行账号没有 UPDATE、DELETE),所以判重用普通读,不加锁。
const receiptSelectSQL = "SELECT " + receiptColumns + " FROM chat_send_authority WHERE player_id=? AND request_id=?"

type Rules struct {
	MaxContentBytes     int
	RateLimitPerSecond  int
	HistoryMaxEntries   int
	HistoryTTLSeconds   int
	HistoryDefaultLimit int
	HistoryMaxLimit     int
	SensitiveWords      []string
	// ActorVerifyTTLSeconds 是身份校验结果的进程内缓存秒数,0 = 每次都校验(actor_verify.go)。
	// 刻意不进 policyBytes:它只影响本进程读权威行的频率,不改变已登记原件的判定,副本之间可以不同,滚动改它不该被当成规则漂移。
	ActorVerifyTTLSeconds int
	// WorldHistoryCacheMs 是世界频道近窗快照的最长使用时长(毫秒),0 = 每次拉取都直读 SQL(world_window.go)。
	// 同样不进 policyBytes,理由同 ActorVerifyTTLSeconds。
	WorldHistoryCacheMs int
	// WorldChannelCooldownMs 是同一名玩家相邻两条世界频道消息的最短间隔(毫秒),0 = 不限。
	// 世界频道一条消息要推给全服在线玩家,每秒条数(RateLimitPerSecond)只管单人突发,管不住全服总量;
	// 发言间隔把全服世界消息量压在「在线人数 ÷ 间隔」之内(docs/design/chat-world-channel-fanout.md §5)。
	// 不进 policyBytes:它是运营会调的值,滚动改它时各副本短暂按不同间隔判,只影响新请求放不放行,不改变已登记原件的判定。
	WorldChannelCooldownMs int
}

type SendResult struct {
	Tip       uint32
	Duplicate bool
	// Cooldown 表示这次是被世界频道发言间隔拒绝的(Tip 同为限流码),调用方只用它区分指标。
	Cooldown bool
	// Message 是本次首次提交的显示原件(与历史里那一份逐字相同),只在这次请求真正写入了新的一行时非 nil;
	// 重放、限流都为 nil。世界频道推送只推它,重放不再推第二次(docs/design/chat-world-channel-fanout.md §4)。
	Message *chatpb.ChatMessage
}

// Service是真实调用面与独立逻辑测试的同一接缝；nil依赖拒绝，不存在Redis授权回落。
type Service interface {
	Send(context.Context, *chatpb.SendChatRequest) (SendResult, error)
	Pull(context.Context, *chatpb.PullChatHistoryRequest, int) ([]*chatpb.ChatMessage, error)
}

type Store struct {
	db     *sql.DB
	states rolefence.StateReader
	rules  Rules
	filter *textfilter.Filter
	clock  func(context.Context, *sql.Tx) (uint64, error)
	actors *actorVerifyCache
	world  *worldWindow // nil = 世界频道直读
}

// receipt只描述SQL元数据；业务原件始终是原PB字节，不复制PB字段定义。
type receipt struct {
	account    []byte
	revision   uint64
	digest     []byte
	request    []byte
	message    []byte
	stream     []byte
	state      uint32
	acceptedMs uint64
	tip        uint32
}

type role struct {
	account  string
	revision uint64
}

func New(ctx context.Context, db *sql.DB, states rolefence.StateReader, backend authoritysql.Backend, rules Rules) (*Store, error) {
	if db == nil || states == nil || rules.MaxContentBytes <= 0 || rules.RateLimitPerSecond <= 0 || rules.HistoryMaxEntries <= 0 || rules.HistoryDefaultLimit <= 0 || rules.HistoryMaxLimit < rules.HistoryDefaultLimit || rules.HistoryMaxLimit > constants.MaxHistoryPullLimit || rules.HistoryTTLSeconds <= 0 || uint64(rules.HistoryTTLSeconds) > math.MaxUint64/1000 || rules.ActorVerifyTTLSeconds < 0 || rules.ActorVerifyTTLSeconds > constants.MaxActorVerifyTTLSeconds || rules.WorldHistoryCacheMs < 0 || rules.WorldHistoryCacheMs > constants.MaxWorldHistoryCacheMs {
		return nil, ErrInvalid
	}
	if rules.WorldChannelCooldownMs < 0 || rules.WorldChannelCooldownMs > constants.MaxWorldChannelCooldownMs {
		return nil, ErrInvalid
	}
	// 原件到期由库删除(schema.go 的 TTL 属性):可见窗口必须短于保留期,否则还在窗口里的历史会被删掉。
	if uint64(rules.HistoryTTLSeconds) > constants.MaxHistoryTTLSeconds {
		return nil, ErrInvalid
	}
	if err := authoritysql.AssertCurrentSession(ctx, db, backend); err != nil {
		return nil, err
	}
	if err := CheckSchema(ctx, db); err != nil {
		return nil, err
	}
	filter, err := textfilter.New(rules.SensitiveWords)
	if err != nil {
		return nil, err
	}
	rules.SensitiveWords = append([]string(nil), rules.SensitiveWords...)
	s := &Store{db: db, states: states, rules: rules, filter: filter, clock: databaseNow,
		actors: newActorVerifyCache(time.Duration(rules.ActorVerifyTTLSeconds)*time.Second, actorVerifyMaxEntries, monotonicSinceStart())}
	if rules.WorldHistoryCacheMs > 0 {
		s.world = newWorldWindow(time.Duration(rules.WorldHistoryCacheMs)*time.Millisecond, monotonicSinceStart(), s.loadWorldSnapshot)
	}
	if err = s.ensurePolicy(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func databaseNow(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var now uint64
	err := tx.QueryRowContext(ctx, "SELECT "+dbNow).Scan(&now)
	return now, err
}

func (s *Store) ensurePolicy(ctx context.Context) error {
	raw := policyBytes(s.rules)
	digest := sha256.Sum256(raw)
	// 策略行写过一次就不再变,重启只读。启动撞上闸门冻结时读能过、写不能,写资格是一次性的,不能因为搬库窗口让进程起不来。
	var existing []byte
	err := s.db.QueryRowContext(ctx, "SELECT policy_hash FROM chat_policy WHERE id=1").Scan(&existing)
	if err == nil {
		if !bytes.Equal(existing, digest[:]) {
			return ErrPolicy
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 先中性插入再取锁，不在不存在行上取锁后追加插入。
	if _, err = tx.ExecContext(ctx, "INSERT INTO chat_policy(id,policy_hash) VALUES(1,?) ON DUPLICATE KEY UPDATE id=id", digest[:]); err != nil {
		return err
	}
	var stored []byte
	if err = tx.QueryRowContext(ctx, "SELECT policy_hash FROM chat_policy WHERE id=1 FOR UPDATE").Scan(&stored); err != nil {
		return err
	}
	if !bytes.Equal(stored, digest[:]) {
		return ErrPolicy
	}
	return tx.Commit()
}

// 本版固定字节序，避免序列化库键序/HTML转义变化造成同规则误拒；不是客户端协议。
// v2(2026-11-12)把两个存储布局常量也钉进来:世界频道的分段数与原件保留天数。它们写在代码里,同一版构建的副本必然一致;
// 钉进策略行是为了挡住以后的构建悄悄改它们:分段数变了旧消息读不到,保留天数变了表上的 TTL 属性不会跟着变,都必须显式迁移。
func policyBytes(r Rules) []byte {
	var out bytes.Buffer
	out.WriteString("chat-authority-v2")
	var number [8]byte
	put := func(n uint64) { binary.BigEndian.PutUint64(number[:], n); out.Write(number[:]) }
	for _, n := range []int{r.MaxContentBytes, r.RateLimitPerSecond, r.HistoryMaxEntries, r.HistoryTTLSeconds, r.HistoryDefaultLimit, r.HistoryMaxLimit, constants.WorldStreamShards, constants.OriginalRetentionDays} {
		put(uint64(n))
	}
	put(uint64(len(r.SensitiveWords)))
	for _, word := range r.SensitiveWords {
		put(uint64(len(word)))
		out.WriteString(word)
	}
	return out.Bytes()
}

// worldStreamKeys 是世界频道的全部流键,下标是分段号(docs/design/chat-world-channel-fanout.md §5)。
// 世界频道的每条消息都要进按 (stream_key, state, accepted_ms, …) 排的索引:只有一个流键时,全服的写入都追加在这个索引的同一处,
// TiDB 上是一个拆不开的热点 Region。按发送者分成 WorldStreamShards 段之后有这么多个追加点,能被拆到不同的 Region。
// 读最新一窗时每段各取最新 N 行再合并(readWorldRows)。
var worldStreamKeys = func() [constants.WorldStreamShards]string {
	var keys [constants.WorldStreamShards]string
	for shard := range keys {
		keys[shard] = fmt.Sprintf("world-v3:%d", shard)
	}
	return keys
}()

// worldStreamKey 返回一名玩家的世界频道消息所在的流键。一名玩家的世界消息只落在这一段。
func worldStreamKey(sender uint64) string {
	return worldStreamKeys[sender%constants.WorldStreamShards]
}

func streamKey(actor, peer uint64) string {
	if peer == 0 {
		return worldStreamKey(actor)
	}
	left, right := actor, peer
	if left > right {
		left, right = right, left
	}
	return fmt.Sprintf("private-v2:%d:%d", left, right)
}

func validate(in *chatpb.SendChatRequest, max int) error {
	if in == nil || in.Message == nil || len(in.RequestId) > 64 || proto.Size(in) > max+8192 || len(in.Message.Content) > max || strings.TrimSpace(in.Message.Content) == "" {
		return ErrInvalid
	}
	if in.Message.Channel != chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD && in.Message.Channel != chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE {
		return ErrInvalid
	}
	return nil
}

func scanReceipt(row *sql.Row) (*receipt, error) {
	r := &receipt{}
	err := row.Scan(&r.account, &r.revision, &r.digest, &r.request, &r.message, &r.stream, &r.state, &r.acceptedMs, &r.tip)
	if err != nil {
		return nil, err
	}
	if !accountdirectory.ValidAccount(string(r.account)) || r.revision == 0 || len(r.digest) != sha256.Size || len(r.request) == 0 || r.state != acceptedState || r.acceptedMs == 0 || r.acceptedMs > math.MaxInt64 || r.tip != 0 || len(r.message) == 0 || len(r.stream) == 0 {
		return nil, errors.New("聊天永久原件损坏")
	}
	return r, nil
}

func (s *Store) Send(ctx context.Context, in *chatpb.SendChatRequest) (SendResult, error) {
	if err := validate(in, s.rules.MaxContentBytes); err != nil {
		return SendResult{}, err
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(in)
	if err != nil {
		return SendResult{}, err
	}
	digest := sha256.Sum256(raw)
	requestID := in.RequestId
	if requestID == "" {
		requestID = "server:" + rand.Text()
	}
	peer := uint64(0)
	if in.Message.Channel == chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE {
		peer = in.Message.TargetPlayerId
		if peer == 0 {
			return SendResult{}, ErrInvalid
		}
	}
	// 身份校验在发送事务之外(actor_verify.go):事务里只剩聊天自己的表,不持角色锁。
	actor, owned, err := s.verifyActor(ctx)
	if err != nil {
		return SendResult{}, err
	}
	if peer == actor {
		return SendResult{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SendResult{}, err
	}
	defer tx.Rollback()
	replay := func(r *receipt) (SendResult, error) {
		if string(r.account) != owned.account || r.revision != owned.revision {
			return SendResult{}, ErrOriginal
		}
		if !bytes.Equal(r.digest, digest[:]) || !bytes.Equal(r.request, raw) {
			return SendResult{}, ErrConflict
		}
		message := &chatpb.ChatMessage{}
		// 重放只信首次已接受的显示原件，不重新过滤或刷新时间。
		if !bytes.Equal(r.stream, []byte(streamKey(actor, peer))) || proto.Unmarshal(r.message, message) != nil || message.SenderPlayerId != actor || message.TargetPlayerId != peer || message.Channel != in.Message.Channel || message.SendTimeMs != int64(r.acceptedMs) || strings.TrimSpace(message.Content) == "" {
			return SendResult{}, errors.New("聊天永久显示原件与原接受决定不符")
		}
		return SendResult{Tip: r.tip, Duplicate: true}, nil
	}
	r, err := scanReceipt(tx.QueryRowContext(ctx, receiptSelectSQL, actor, []byte(requestID)))
	if err == nil {
		return replay(r)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SendResult{}, err
	}
	now, err := s.clock(ctx, tx)
	if err != nil {
		return SendResult{}, err
	}
	if now == 0 || now > math.MaxInt64 {
		return SendResult{}, errors.New("聊天SQL时间非法")
	}
	// 计数和下面的插入之间没有锁,并发在途时可能略超:近似限流,理由见 actor_verify.go。
	var count uint64
	start := now - now%1000
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_send_authority WHERE player_id=? AND state=1 AND accepted_ms>=? AND accepted_ms<?", actor, start, start+1000).Scan(&count); err != nil {
		return SendResult{}, err
	}
	// limited 是两种限流共用的出口。计数或间隔查询看到的行里可能有同一 request_id 并发首发的赢家:它已提交就是重复,不是超限。
	// 否则客户端换新 id 重发会多出一条。
	limited := func(cooldown bool) (SendResult, error) {
		winner, readErr := scanReceipt(tx.QueryRowContext(ctx, receiptSelectSQL, actor, []byte(requestID)))
		if readErr == nil {
			return replay(winner)
		}
		if !errors.Is(readErr, sql.ErrNoRows) {
			return SendResult{}, readErr
		}
		return SendResult{Tip: constants.ErrRateLimited, Cooldown: cooldown}, nil
	}
	if count >= uint64(s.rules.RateLimitPerSecond) {
		return limited(false)
	}
	if peer == 0 && s.rules.WorldChannelCooldownMs > 0 {
		cooling, coolErr := worldCooldownActive(ctx, tx, actor, now, uint64(s.rules.WorldChannelCooldownMs))
		if coolErr != nil {
			return SendResult{}, coolErr
		}
		if cooling {
			return limited(true)
		}
	}
	var payload []byte
	message := &chatpb.ChatMessage{SenderPlayerId: actor, TargetPlayerId: peer, Channel: in.Message.Channel, Content: s.filter.Mask(strings.TrimSpace(in.Message.Content)), SendTimeMs: int64(now)}
	payload, err = (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return SendResult{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO chat_send_authority(player_id,request_id,"+receiptColumns+",accepted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", actor, []byte(requestID), []byte(owned.account), owned.revision, digest[:], raw, payload, []byte(streamKey(actor, peer)), acceptedState, now, uint32(0), acceptedAt(now)); err != nil {
		var duplicate *mysql.MySQLError
		if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
			return SendResult{}, err
		}
		// 同一 request_id 并发首发:主键裁决,输家转读赢家已提交的原件,走同一条重放判定。
		if r, err = scanReceipt(tx.QueryRowContext(ctx, receiptSelectSQL, actor, []byte(requestID))); err != nil {
			return SendResult{}, err
		}
		return replay(r)
	}
	// SQL提交是唯一可见写入；未知提交结果保留原ID由调用者重放，不在本函数换ID重试。
	if err = tx.Commit(); err != nil {
		return SendResult{}, err
	}
	if peer == 0 && s.world != nil {
		// 本副本上的下一次世界频道拉取必须看得到这条(读己之写);别的副本最多滞后 WorldHistoryCacheMs。
		s.world.invalidate()
	}
	return SendResult{Message: message}, nil
}

// worldCooldownActive 报告这名玩家是否还在世界频道的发言间隔里:最近 cooldownMs 毫秒内已有一条被接受的世界频道消息。
// 一名玩家的世界消息只落在他自己那一段(worldStreamKey)。查询沿 (player_id, state, accepted_ms) 的索引只看他最近这段时间的行,
// 行数不超过「间隔秒数 × 每秒条数上限」。
// 和每秒限流一样是近似的:查询与插入之间没有锁。同一会话的请求由 gate 的会话 lane 串行(client-request-ordering.md),
// 正常客户端不会并发;绕过 lane 的并发最多多放过在途的那几条。
func worldCooldownActive(ctx context.Context, tx *sql.Tx, actor, now, cooldownMs uint64) (bool, error) {
	since := uint64(0)
	if now > cooldownMs {
		since = now - cooldownMs
	}
	var found int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM chat_send_authority WHERE player_id=? AND state=1 AND accepted_ms>? AND stream_key=? LIMIT 1", actor, since, []byte(worldStreamKey(actor))).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// acceptedAt 把接受时刻(毫秒)写成保留期列 accepted_at 的值(schema.go 的 TTL 属性按它删过期原件)。
// DATETIME 不带时区,固定写 UTC:集群时区不是 UTC 时,到期最多早或晚十几个小时。可见窗口比保留期至少短一天
// (constants.MaxHistoryTTLSeconds),这点偏差不会删掉还看得见的历史。
func acceptedAt(ms uint64) string {
	return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02 15:04:05.000")
}

func (s *Store) Pull(ctx context.Context, in *chatpb.PullChatHistoryRequest, limit int) ([]*chatpb.ChatMessage, error) {
	if in == nil || limit <= 0 || (in.Channel != chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD && in.Channel != chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE) {
		return nil, ErrInvalid
	}
	peer := uint64(0)
	if in.Channel == chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE {
		peer = in.PeerPlayerId
		if peer == 0 {
			return nil, ErrInvalid
		}
	}
	actor, _, err := s.verifyActor(ctx)
	if err != nil {
		return nil, err
	}
	if actor == peer {
		return nil, ErrInvalid
	}
	limit = min(limit, s.maxPullRows())
	window := uint64(s.rules.HistoryTTLSeconds) * 1000
	if peer == 0 && s.world != nil {
		// 世界频道读本副本的近窗快照(world_window.go):不再每次拉取都读同一段索引尾部。
		snap, err := s.world.get(ctx)
		if err != nil {
			return nil, err
		}
		rows := snap.rows
		if len(rows) > limit {
			rows = rows[:limit]
		}
		return visibleHistory(rows, snap.nowMs(s.world.now()), window, in.Channel, actor, peer)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return nil, err
	}
	if now == 0 || now > math.MaxInt64 {
		return nil, errors.New("聊天SQL时间非法")
	}
	var rows []historyRow
	if peer == 0 {
		rows, err = readWorldRows(ctx, tx, limit)
	} else {
		rows, err = readHistoryRows(ctx, tx, streamKey(actor, peer), limit)
	}
	if err != nil {
		return nil, err
	}
	return visibleHistory(rows, now, window, in.Channel, actor, peer)
}

// maxPullRows 是一次拉取最多返回的条数,也是世界频道快照要存的行数。
func (s *Store) maxPullRows() int {
	return min(s.rules.HistoryMaxLimit, s.rules.HistoryMaxEntries, constants.MaxHistoryPullLimit)
}

// loadWorldSnapshot 读世界频道最新的 maxPullRows 行与当时的数据库时间,供 worldWindow 缓存。
func (s *Store) loadWorldSnapshot(ctx context.Context) (*worldSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return nil, err
	}
	if now == 0 || now > math.MaxInt64 {
		return nil, errors.New("聊天SQL时间非法")
	}
	rows, err := readWorldRows(ctx, tx, s.maxPullRows())
	if err != nil {
		return nil, err
	}
	return &worldSnapshot{rows: rows, dbNowMs: now}, nil
}

// readWorldRows 按新到旧读世界频道最新的 limit 行:每一段各取最新 limit 行,合并后取前 limit 行。
// 全服最新的 limit 行里,任何一行都在它自己那一段的最新 limit 行之内,所以每段取 limit 行不会漏。
// 各段在同一个事务里先后读,读已提交隔离下后读的段可能多看到刚提交的行:结果仍是那一刻各段都已提交的消息,下一次读会补齐。
func readWorldRows(ctx context.Context, tx *sql.Tx, limit int) ([]historyRow, error) {
	merged := make([]historyRow, 0, limit)
	for _, key := range worldStreamKeys {
		rows, err := readHistoryRows(ctx, tx, key, limit)
		if err != nil {
			return nil, err
		}
		merged = append(merged, rows...)
	}
	return newestRows(merged, limit), nil
}

// newestRows 把若干段的行按单段查询的同一个顺序(接受时刻、发送者、请求号都从大到小)排好,取前 limit 行。会重排入参。
func newestRows(rows []historyRow, limit int) []historyRow {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].acceptedMs != rows[j].acceptedMs {
			return rows[i].acceptedMs > rows[j].acceptedMs
		}
		if rows[i].sender != rows[j].sender {
			return rows[i].sender > rows[j].sender
		}
		return bytes.Compare(rows[i].requestID, rows[j].requestID) > 0
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

// historyRow 是一个频道流里的一行已提交原件,还没有校验。
type historyRow struct {
	sender     uint64
	requestID  []byte
	payload    []byte
	acceptedMs uint64
}

// readHistoryRows 按新到旧读一个频道流最多 limit 行已提交原件。Scan 到 []byte 会复制驱动的缓冲,结果可以长期持有。
func readHistoryRows(ctx context.Context, tx *sql.Tx, stream string, limit int) ([]historyRow, error) {
	rows, err := tx.QueryContext(ctx, "SELECT player_id,request_id,message_payload,accepted_ms FROM chat_send_authority WHERE stream_key=? AND state=1 ORDER BY accepted_ms DESC,player_id DESC,request_id DESC LIMIT ?", []byte(stream), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]historyRow, 0, limit)
	for rows.Next() {
		var row historyRow
		if err = rows.Scan(&row.sender, &row.requestID, &row.payload, &row.acceptedMs); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// visibleHistory 从新到旧挑出可见的原件并逐行校验,直读与世界频道快照共用这一份规则:
// 最新一条距 now 达到 TTL 就整窗不可见;相邻两条间隔达到 TTL 就截断(保持旧 LIST 闲置过期的语义)。
// 只校验遍历到的行,截断之后的行即使损坏也不报。rows 已按新到旧排好,调用方保证最多 limit 行。
func visibleHistory(rows []historyRow, now, window uint64, channel chatpb.ChatChannelType, actor, peer uint64) ([]*chatpb.ChatMessage, error) {
	result := make([]*chatpb.ChatMessage, 0, len(rows))
	var newer uint64
	for _, row := range rows {
		if row.acceptedMs == 0 || len(row.requestID) == 0 || row.acceptedMs > math.MaxInt64 {
			return nil, errors.New("聊天已提交历史原件损坏")
		}
		if newer == 0 {
			if now >= row.acceptedMs && now-row.acceptedMs >= window {
				return result, nil
			}
		} else if newer >= row.acceptedMs && newer-row.acceptedMs >= window {
			break
		}
		newer = row.acceptedMs
		message := &chatpb.ChatMessage{}
		if proto.Unmarshal(row.payload, message) != nil || message.SenderPlayerId != row.sender || message.SendTimeMs != int64(row.acceptedMs) || message.Channel != channel || strings.TrimSpace(message.Content) == "" {
			return nil, errors.New("聊天显示正文与SQL原决定不符")
		}
		if peer != 0 && !((row.sender == actor && message.TargetPlayerId == peer) || (row.sender == peer && message.TargetPlayerId == actor)) {
			return nil, errors.New("聊天私聊原件不属于已授权分区")
		}
		if peer == 0 && message.TargetPlayerId != 0 {
			return nil, errors.New("聊天世界频道原件损坏")
		}
		result = append(result, message)
	}
	return result, nil
}
