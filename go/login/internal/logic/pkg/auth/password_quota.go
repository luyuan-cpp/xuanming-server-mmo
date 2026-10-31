package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

const (
	PasswordQuotaBudget  = 250 * time.Millisecond
	PasswordAttemptLease = 30 * time.Second
)

type PasswordAccountKind string

const (
	KnownPasswordAccount   PasswordAccountKind = "known"
	UnknownPasswordAccount PasswordAccountKind = "unknown"
)

type PasswordAttemptOutcome string

const (
	PasswordMismatch PasswordAttemptOutcome = "mismatch"
	PasswordSuccess  PasswordAttemptOutcome = "success"
	PasswordAbort    PasswordAttemptOutcome = "abort"
)

var (
	ErrPasswordQuotaUnavailable = errors.New("password quota unavailable")
	ErrPasswordQuotaDenied      = errors.New("password quota denied")
	ErrPasswordQuotaObsolete    = errors.New("password attempt obsolete")
	ErrPasswordQuotaConflict    = errors.New("password attempt outcome conflict")
)

// PasswordAttempt 只由 Begin 创建；不包含账号原文，调用者不可替换 token 或 key。
type PasswordAttempt struct {
	key, token string
	// 从发送 Begin 前起算的单调期限比服务端租期保守；Redis TIME 仍是状态权威。
	deadline time.Time
}

// PasswordFailureQuota 隐藏身份散列、单账号租约、幂等完成与失败窗。Begin 不确定就不跑
// KDF；一旦尝试 Finish，结果未知不得改 outcome 或补 abort，交给有限租期恢复。
type PasswordFailureQuota interface {
	Begin(context.Context, PasswordAccountKind, string) (PasswordAttempt, error)
	Finish(context.Context, PasswordAttempt, PasswordAttemptOutcome) error
}

// 两段脚本共用固定格式。读全状态并校验后才写，损坏/混配不能删键假装恢复。
// 原子性以同一 Redis 主及状态未丢失为前提；不声称异步故障切换下写入不可回退。
const passwordQuotaStateScript = `
local key, fp = KEYS[1], ARGV[1]
local limit, window, lock = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local token = ARGV[5]
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local function integer(n) return n and n >= 0 and n <= 9007199254740991 and n == math.floor(n) end
local function validToken(t) return t == '' or (#t == 32 and string.match(t, '^[0-9a-f]+$')) end
if not integer(limit) or limit < 1 or limit > 100 or not integer(window) or window < 1000 or window > 3600000
  or not integer(lock) or lock < 1000 or lock > 3600000 or not validToken(token) or token == '' then return {5, 0} end
local exists = redis.call('EXISTS', key) == 1
local s = {n=0, w=0, l=0, t='', e=0, d='', r='', u=0}
if exists then
  if redis.call('TYPE', key).ok ~= 'hash' or redis.call('HLEN', key) ~= 10 or redis.call('PTTL', key) < 0 then return {5, 0} end
  local values = redis.call('HMGET', key, 'v', 'fp', 'n', 'w', 'l', 't', 'e', 'd', 'r', 'u')
  for i=1,10 do if values[i] == false then return {5, 0} end end
  if values[1] ~= '1' or values[2] ~= fp then return {5, 0} end
  s = {n=tonumber(values[3]), w=tonumber(values[4]), l=tonumber(values[5]), t=values[6],
       e=tonumber(values[7]), d=values[8], r=values[9], u=tonumber(values[10])}
  if not integer(s.n) or s.n >= limit or not integer(s.w) or not integer(s.l) or not integer(s.e) or not integer(s.u)
    or not validToken(s.t) or not validToken(s.d) or ((s.n == 0) ~= (s.w == 0))
    or (s.l > 0 and (s.n ~= 0 or s.w ~= 0)) or ((s.t == '') ~= (s.e == 0))
    or (s.t ~= '' and s.d ~= '') or ((s.d == '') ~= (s.u == 0))
    or (s.d == '' and s.r ~= '') or (s.d ~= '' and s.r ~= 'success' and s.r ~= 'mismatch' and s.r ~= 'abort') then return {5, 0} end
end
local function expireWindows()
  if s.w > 0 and s.w <= now then s.n=0; s.w=0 end
  if s.l > 0 and s.l <= now then s.l=0 end
end
local function save()
  local deadline = math.max(s.w, s.l, s.e, s.u)
  redis.call('HSET', key, 'v', '1', 'fp', fp, 'n', s.n, 'w', s.w, 'l', s.l,
    't', s.t, 'e', s.e, 'd', s.d, 'r', s.r, 'u', s.u)
  redis.call('PEXPIREAT', key, deadline)
end
`

var passwordQuotaBeginScript = redis.NewScript(passwordQuotaStateScript + `
expireWindows()
if s.l > now then return {2, 0} end
if s.t ~= '' then
  if s.t == token then
    if s.e > now then return {1, s.e} else return {4, 0} end
  elseif s.e > now then return {3, 0} end
end
if s.d == token then return {4, 0} end
s.t=token; s.e=now+30000
s.d=''; s.r=''; s.u=0
save()
return {1, s.e}
`)

var passwordQuotaFinishScript = redis.NewScript(passwordQuotaStateScript + `
local outcome = ARGV[6]
if outcome ~= 'success' and outcome ~= 'mismatch' and outcome ~= 'abort' then return {6, 0} end
if not exists then return {4, 0} end
if s.d == token then
  if s.u <= now then return {4, 0} end
  if s.r ~= outcome then return {6, 0} end
  return {1, 0}
end
if s.t ~= token or s.e <= now then return {4, 0} end
expireWindows()
if outcome == 'mismatch' then
  if s.n == 0 then s.w=now+window end
  s.n=s.n+1
  if s.n >= limit then s.n=0; s.w=0; s.l=now+lock end
elseif outcome == 'success' then s.n=0; s.w=0; s.l=0 end
s.d=token; s.r=outcome; s.u=s.e
s.t=''; s.e=0
save()
return {1, 0}
`)

var passwordQuotaTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login", Name: "password_quota_total", Help: "生产口令账号失败配额，故障一律拒绝认证。",
	Labels: []string{"operation", "result"},
})

type RedisPasswordFailureQuota struct {
	fingerprint  string
	limit        int
	window, lock time.Duration
	run          func(context.Context, *redis.Script, string, ...any) (any, error)
	now          func() time.Time
	warnMu       sync.Mutex
	lastWarn     time.Time
}

func NewRedisPasswordFailureQuota(rdb redis.UniversalClient, limit int, window, lock time.Duration) (*RedisPasswordFailureQuota, error) {
	if nilPasswordDependency(rdb) || limit < 1 || limit > 100 || window < time.Second || window > time.Hour || lock < time.Second || lock > time.Hour || window%time.Millisecond != 0 || lock%time.Millisecond != 0 {
		return nil, errors.New("口令配额要求Redis、1..100阈值及1s..1h的整毫秒窗口和锁窗")
	}
	return &RedisPasswordFailureQuota{
		limit: limit, window: window, lock: lock,
		fingerprint: fmt.Sprintf("v1:%d:%d:%d:30000", limit, window.Milliseconds(), lock.Milliseconds()),
		now:         time.Now,
		run: func(ctx context.Context, script *redis.Script, key string, args ...any) (any, error) {
			return script.Run(ctx, rdb, []string{key}, args...).Result()
		},
	}, nil
}

func (q *RedisPasswordFailureQuota) Begin(ctx context.Context, kind PasswordAccountKind, identity string) (PasswordAttempt, error) {
	if (kind != KnownPasswordAccount && kind != UnknownPasswordAccount) || identity == "" {
		return PasswordAttempt{}, q.failure("begin", "identity")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return PasswordAttempt{}, q.failure("begin", "random")
	}
	digest := sha256.Sum256([]byte(identity))
	attempt := PasswordAttempt{key: redisslot.PasswordQuotaKey(string(kind) + ":" + hex.EncodeToString(digest[:])), token: hex.EncodeToString(random[:]), deadline: q.now().Add(PasswordAttemptLease)}
	code, err := q.execute(ctx, "begin", passwordQuotaBeginScript, attempt, "")
	if err != nil {
		return PasswordAttempt{}, err
	}
	if code != 1 {
		return PasswordAttempt{}, q.rejected("begin", code)
	}
	if !q.now().Before(attempt.deadline) {
		return PasswordAttempt{}, q.rejected("begin", 4)
	}
	passwordQuotaTotal.Inc("begin", "allowed")
	return attempt, nil
}

func (q *RedisPasswordFailureQuota) Finish(ctx context.Context, attempt PasswordAttempt, outcome PasswordAttemptOutcome) error {
	if attempt.key == "" || attempt.token == "" || (outcome != PasswordSuccess && outcome != PasswordMismatch && outcome != PasswordAbort) {
		return q.failure("finish", "input")
	}
	if !q.now().Before(attempt.deadline) {
		return q.rejected("finish", 4)
	}
	code, err := q.execute(ctx, "finish", passwordQuotaFinishScript, attempt, string(outcome))
	if err != nil {
		return err
	}
	if code != 1 {
		return q.rejected("finish", code)
	}
	if !q.now().Before(attempt.deadline) {
		return q.rejected("finish", 4)
	}
	passwordQuotaTotal.Inc("finish", string(outcome))
	return nil
}

func (q *RedisPasswordFailureQuota) execute(ctx context.Context, operation string, script *redis.Script, attempt PasswordAttempt, outcome string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	budget, cancel := context.WithTimeout(ctx, PasswordQuotaBudget)
	defer cancel()
	value, err := q.run(budget, script, attempt.key, q.fingerprint, q.limit, q.window.Milliseconds(), q.lock.Milliseconds(), attempt.token, outcome)
	if parentErr := ctx.Err(); parentErr != nil {
		return 0, parentErr
	}
	if budget.Err() != nil {
		return 0, q.failure(operation, "budget")
	}
	if err != nil {
		return 0, q.failure(operation, "redis")
	}
	values, ok := value.([]any)
	if !ok || len(values) != 2 {
		return 0, q.failure(operation, "reply")
	}
	code, ok := values[0].(int64)
	if !ok {
		return 0, q.failure(operation, "reply")
	}
	if _, ok := values[1].(int64); !ok {
		return 0, q.failure(operation, "reply")
	}
	return code, nil
}

func (q *RedisPasswordFailureQuota) rejected(operation string, code int64) error {
	switch code {
	case 2:
		passwordQuotaTotal.Inc(operation, "locked")
		return ErrPasswordQuotaDenied
	case 3:
		passwordQuotaTotal.Inc(operation, "inflight")
		return ErrPasswordQuotaDenied
	case 4:
		passwordQuotaTotal.Inc(operation, "obsolete")
		return ErrPasswordQuotaObsolete
	case 6:
		passwordQuotaTotal.Inc(operation, "conflict")
		return ErrPasswordQuotaConflict
	default:
		return q.failure(operation, "state")
	}
}

func (q *RedisPasswordFailureQuota) failure(operation, reason string) error {
	passwordQuotaTotal.Inc(operation, "error")
	q.warnMu.Lock()
	defer q.warnMu.Unlock()
	if q.lastWarn.IsZero() || time.Since(q.lastWarn) >= time.Minute {
		q.lastWarn = time.Now()
		logx.Errorf("口令失败配额拒绝认证 operation=%s reason=%s", operation, reason)
	}
	return ErrPasswordQuotaUnavailable
}

func nilPasswordDependency(value any) bool {
	if value == nil {
		return true
	}
	ref := reflect.ValueOf(value)
	switch ref.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return ref.IsNil()
	}
	return false
}
