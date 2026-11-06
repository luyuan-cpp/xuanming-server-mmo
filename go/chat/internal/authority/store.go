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
}

type SendResult struct {
	Tip       uint32
	Duplicate bool
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
func policyBytes(r Rules) []byte {
	var out bytes.Buffer
	out.WriteString("chat-authority-v1")
	var number [8]byte
	put := func(n uint64) { binary.BigEndian.PutUint64(number[:], n); out.Write(number[:]) }
	for _, n := range []int{r.MaxContentBytes, r.RateLimitPerSecond, r.HistoryMaxEntries, r.HistoryTTLSeconds, r.HistoryDefaultLimit, r.HistoryMaxLimit} {
		put(uint64(n))
	}
	put(uint64(len(r.SensitiveWords)))
	for _, word := range r.SensitiveWords {
		put(uint64(len(word)))
		out.WriteString(word)
	}
	return out.Bytes()
}

// worldStreamKey 是世界频道全服唯一的流键。
const worldStreamKey = "world-v2"

func streamKey(actor, peer uint64) string {
	if peer == 0 {
		return worldStreamKey
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
	if count >= uint64(s.rules.RateLimitPerSecond) {
		// 计数里可能有同一 request_id 并发首发的赢家:它已提交就是重复,不是超限。否则客户端换新 id 重发会多出一条。
		if r, err = scanReceipt(tx.QueryRowContext(ctx, receiptSelectSQL, actor, []byte(requestID))); err == nil {
			return replay(r)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return SendResult{}, err
		}
		return SendResult{Tip: constants.ErrRateLimited}, nil
	}
	var payload []byte
	message := &chatpb.ChatMessage{SenderPlayerId: actor, TargetPlayerId: peer, Channel: in.Message.Channel, Content: s.filter.Mask(strings.TrimSpace(in.Message.Content)), SendTimeMs: int64(now)}
	payload, err = (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return SendResult{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO chat_send_authority(player_id,request_id,"+receiptColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?)", actor, []byte(requestID), []byte(owned.account), owned.revision, digest[:], raw, payload, []byte(streamKey(actor, peer)), acceptedState, now, uint32(0)); err != nil {
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
	rows, err := readHistoryRows(ctx, tx, streamKey(actor, peer), limit)
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
	rows, err := readHistoryRows(ctx, tx, worldStreamKey, s.maxPullRows())
	if err != nil {
		return nil, err
	}
	return &worldSnapshot{rows: rows, dbNowMs: now}, nil
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
