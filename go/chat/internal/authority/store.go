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
	"regexp"
	"strings"
	"time"

	"chat/internal/constants"
	"chat/internal/textfilter"
	chatpb "proto/chat"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/placement"

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
}

type SendResult struct {
	Tip       uint32
	Duplicate bool
}

// Service是真实调用面与独立逻辑测试的同一接缝；nil依赖拒绝，不存在Redis授权回落。
type Service interface {
	Send(context.Context, *chatpb.SendChatRequest) (SendResult, error)
	Pull(context.Context, *chatpb.PullChatHistoryRequest, int) ([]*chatpb.ChatMessage, error)
}

type Store struct {
	db         *sql.DB
	roleSchema string
	rules      Rules
	filter     *textfilter.Filter
	clock      func(context.Context, *sql.Tx) (uint64, error)
	actors     *actorVerifyCache
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

func New(ctx context.Context, db *sql.DB, schema string, backend authoritysql.Backend, rules Rules) (*Store, error) {
	if db == nil || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`).MatchString(schema) || rules.MaxContentBytes <= 0 || rules.RateLimitPerSecond <= 0 || rules.HistoryMaxEntries <= 0 || rules.HistoryDefaultLimit <= 0 || rules.HistoryMaxLimit < rules.HistoryDefaultLimit || rules.HistoryMaxLimit > constants.MaxHistoryPullLimit || rules.HistoryTTLSeconds <= 0 || uint64(rules.HistoryTTLSeconds) > math.MaxUint64/1000 || rules.ActorVerifyTTLSeconds < 0 || rules.ActorVerifyTTLSeconds > constants.MaxActorVerifyTTLSeconds {
		return nil, ErrInvalid
	}
	if err := authoritysql.AssertCurrentSession(ctx, db, backend); err != nil {
		return nil, err
	}
	if err := CheckSchema(ctx, db); err != nil {
		return nil, err
	}
	if err := placement.CheckRoleRouteContract(ctx, db, schema); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT player_id,account,owner_revision,trade_listing_id FROM `"+schema+"`.account_role LIMIT 0")
	if err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	filter, err := textfilter.New(rules.SensitiveWords)
	if err != nil {
		return nil, err
	}
	rules.SensitiveWords = append([]string(nil), rules.SensitiveWords...)
	s := &Store{db: db, roleSchema: schema, rules: rules, filter: filter, clock: databaseNow,
		actors: newActorVerifyCache(time.Duration(rules.ActorVerifyTTLSeconds)*time.Second, actorVerifyMaxEntries, monotonicSinceStart())}
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

func streamKey(actor, peer uint64) string {
	if peer == 0 {
		return "world-v2"
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
	return SendResult{}, nil
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
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	limit = min(limit, s.rules.HistoryMaxLimit, s.rules.HistoryMaxEntries, constants.MaxHistoryPullLimit)
	now, err := s.clock(ctx, tx)
	if err != nil {
		return nil, err
	}
	if now == 0 || now > math.MaxInt64 {
		return nil, errors.New("聊天SQL时间非法")
	}
	rows, err := tx.QueryContext(ctx, "SELECT player_id,request_id,message_payload,accepted_ms FROM chat_send_authority WHERE stream_key=? AND state=1 ORDER BY accepted_ms DESC,player_id DESC,request_id DESC LIMIT ?", []byte(streamKey(actor, peer)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	window := uint64(s.rules.HistoryTTLSeconds) * 1000
	var newer uint64
	result := make([]*chatpb.ChatMessage, 0, limit)
	for rows.Next() {
		var sender, accepted uint64
		var id, payload []byte
		if err = rows.Scan(&sender, &id, &payload, &accepted); err != nil {
			return nil, err
		}
		if accepted == 0 || len(id) == 0 || accepted > math.MaxInt64 {
			return nil, errors.New("聊天已提交历史原件损坏")
		}
		if newer == 0 {
			if now >= accepted && now-accepted >= window {
				return result, nil
			}
		} else if newer >= accepted && newer-accepted >= window {
			break
		}
		newer = accepted
		message := &chatpb.ChatMessage{}
		if proto.Unmarshal(payload, message) != nil || message.SenderPlayerId != sender || message.SendTimeMs != int64(accepted) || message.Channel != in.Channel || strings.TrimSpace(message.Content) == "" {
			return nil, errors.New("聊天显示正文与SQL原决定不符")
		}
		if peer != 0 && !((sender == actor && message.TargetPlayerId == peer) || (sender == peer && message.TargetPlayerId == actor)) {
			return nil, errors.New("聊天私聊原件不属于已授权分区")
		}
		if peer == 0 && message.TargetPlayerId != 0 {
			return nil, errors.New("聊天世界频道原件损坏")
		}
		if len(result) < limit {
			result = append(result, message)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
