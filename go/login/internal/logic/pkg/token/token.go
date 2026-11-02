// Package token manages access/refresh token lifecycle in Redis.
//
// Token architecture (WeChat/QQ game login style):
//
//	Access Token:  opaque, 2h TTL, used for reconnect without re-auth from third-party.
//	Refresh Token: opaque, 30d TTL, used to obtain new access token after expiry.
//
// 令牌字符串是 <AccountTag>.<secret>:tag 决定令牌的键落在哪个槽。secret 是 32 字节的 base64url,前 16 字节是家族号,
// 后 16 字节是随机数,随机半段第一个字节的最高位是类型位(0 = refresh,1 = access)。对客户端是不透明串,客户端不解析。
// 校验时先解析再拼键,格式不对按无效令牌,不访问 Redis。
//
// 家族:一次完整认证(Issue)签出的那一对,加上由它逐次轮换出的全部后继。一台设备的一次登录就是一个家族。
// 任何一枚令牌,包括已作废的旧 refresh,都能从自身解出家族号和类型,所以重用检测不需要墓碑,内存只与活家族数有关;
// 类型位让「把 access 当 refresh 发来」在碰 Redis 之前就被普通拒绝,不会被当成重用。
//
// Redis keys(登录域,键形在 shared/redisslot/login_keys.go,一个账号的键全部同槽,每段脚本都是一次原子提交):
//
//	access_token:{<tag>}:<secret>       → JSON{account, auth_type, device_id, created_at, family_id}          TTL=AccessTokenTTL
//	refresh_token:{<tag>}:<secret>      → 同上                                                                 TTL=RefreshTokenTTL
//	refresh_family:{<tag>}:<id>         → HASH{account, head, prev, created_at, rotated_at}                    TTL=RefreshTokenTTL
//	refresh_rotated:{<tag>}:<旧 secret> → HASH{access, refresh, access_expire, refresh_expire, rotated_at}     TTL=RefreshReuseGrace
//	account_refresh:{<tag>}:<account>   → ZSET of active refresh secrets, score = refresh 过期 unix 秒
//	   (member 与其 refresh_token 键同生共死:键按 TTL 过期的同一刻,score 也越过
//	    now,可被 ZREMRANGEBYSCORE 一并清除。旧版是无 score 的 SET —— 成员不随对应键
//	    过期而消失,死成员无上界累积成 Redis 内存泄漏,客户端反复 Login 即可触发。)
//
// 索引成员与家族 head 存的是 secret,不是完整令牌:脚本按「前缀 + secret」现拼 refresh 键,前缀带账号 tag,
// 现拼出来的键与脚本声明的键同槽。
//
// 不变量:
//   - 家族记录是唯一的真相源。记录不在 = 已吊销或已结束:该家族的 refresh 不能轮换,宽限重放不返回,access 不能登录。
//   - 每个家族恰有一枚活 refresh,就是记录里的 head,只有 head 能轮换。
//   - 令牌与家族记录都带正 TTL。类型、归属、payload 对不上的一律拒绝,脚本的全部检查都在第一次写之前。
//
// 取舍与误伤分析见 docs/design/dual-token-authentication.md「轮换重试的宽限窗口」「令牌家族、重用检测与家族吊销」。
package token

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"shared/redisslot"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	tokenSeparator = "."

	tokenByteLen  = 32   // 256-bit, base64url encoded → 43 chars
	accessKindBit = 0x80 // 随机半段第一个字节的最高位:1 = access,0 = refresh

	// maxRefreshTokensPerAccount 封顶单账号活跃 refresh token 数。死成员由
	// ZREMRANGEBYSCORE 按过期 score 清,这个封顶再挡住"客户端高频 Login 在 30d
	// 窗口内把**活**成员刷到任意大"的病态情况:超出即淘汰最旧的,并删其 token 键。
	maxRefreshTokensPerAccount = 32

	// MaxRefreshReuseGrace 是重放宽限窗口的上限。窗口越长,偷到旧 refresh token 的人能搭车的时间越长;
	// 60s 足够覆盖「客户端超时 + 重连 + 重发」。它也是轮换记录 TTL 的硬上限:剩余 TTL 超过它的记录按损坏处理。
	MaxRefreshReuseGrace = 60 * time.Second

	// replayBudget 是重放判定与重用检测的时间预算,不随请求截止放弃。这次检测漏掉的话,合法客户端拿到拒绝后去重登,
	// 不会再拿那枚旧 token 回来,就再也等不到下一次检测。这段脚本不是会话内有序的写:宽限重放只读,吊销幂等。
	replayBudget = 3 * time.Second

	// replayAttempts / replayBackoff:脚本幂等,在 replayBudget 内有界重试;脚本的明确拒绝不重试。
	replayAttempts = 3
	replayBackoff  = 100 * time.Millisecond
)

// rotateRefreshScript 的返回值。
const (
	refreshNotLive    = 0
	refreshRotated    = 1
	refreshFamilyGone = 2
	refreshNotHead    = 3
)

// replayRefreshScript 返回数组的第一项。
const (
	replayDamaged       = -1
	replaySuccessorGone = 0
	replaySamePair      = 1
	replayFamilyGone    = 2
	replayRejected      = 3
	replayReuseRevoked  = 4
	replayReuseObserved = 5
)

// tokenKind 是令牌的类型位。
type tokenKind int

const (
	refreshKind tokenKind = iota
	accessKind
)

// ErrRefreshRejected:refresh token 不能用于换新。格式不对、不是 refresh、未知、过期、已被轮换且不在宽限窗口内
// (含触发家族吊销的重用),或所属家族已吊销。与存储故障、数据损坏区分开,调用方据此决定日志级别。
var ErrRefreshRejected = errors.New("refresh token rejected")

// errMalformedToken:令牌字符串不是 <AccountTag>.<secret>,或类型与用途不符。没有访问 Redis,与「令牌不存在」对调用方是同一种无效令牌。
var errMalformedToken = errors.New("token malformed")

// errTokenNotFound:令牌的键不在(从未存在、过期、已被轮换或吊销)。
var errTokenNotFound = errors.New("token not found or expired")

// refreshTotal 按结果统计 Refresh:
//   - rotated:正常轮换,签了新的一对;
//   - replayed:宽限窗口内的重放,原样返回上次轮换的那一对;
//   - rejected:格式或类型不对、未知、过期、家族已结束、重放的就是家族头,或轮换记录比当前窗口长(窗口刚调小后的过渡期);
//   - rejected_successor_gone:窗口内重放,但新 refresh token 已过期或被外部删除;
//   - rejected_family_gone:拿已吊销或已结束家族的 refresh 来轮换或宽限重放;
//   - reuse_revoked:已轮换的旧成员被再次使用(窗口外,或窗口内而它的后继已被轮换走),已吊销该家族;
//   - reuse_observed:同上,但 ReuseObserveOnly 开着,只记录没吊销;
//   - abandoned:请求在提交前已截止或取消,什么都没写;
//   - error:存储故障或数据损坏。重放判定持续失败时重用检测漏掉这一次,所以这一项要告警。
var refreshTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "login_refresh_token_total",
	Help: "RefreshToken outcomes: rotated, replayed, rejected, rejected_successor_gone, rejected_family_gone, reuse_revoked, reuse_observed, abandoned, error.",
}, []string{"result"})

// reuseDelay 是重用距家族最近一次轮换的秒数。replayed=predecessor(重放的是头的前一枚)时恰好是
// 那枚 token 被轮换至今的时间,用来判断宽限窗口是否偏短;older 时只是下界。60s 以下的桶对应窗口的可配范围。
var reuseDelay = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "login_refresh_reuse_delay_seconds",
	Help:    "Seconds since the family's last rotation when a rotated refresh token was reused.",
	Buckets: []float64{1, 5, 10, 20, 30, 45, 60, 120, 300, 900, 3600, 21600, 86400, 604800, 2592000},
}, []string{"replayed"})

// Config holds token settings.
type Config struct {
	AccessTokenTTL  time.Duration // e.g. 2h
	RefreshTokenTTL time.Duration // e.g. 30d (720h)
	// RefreshReuseGrace 是旧 refresh token 被轮换后仍可重放的宽限窗口:窗口内同一旧 token 再来,
	// 返回同一对新令牌、不再签发。0 = 严格一次性。取值 [0, MaxRefreshReuseGrace],整毫秒。
	RefreshReuseGrace time.Duration
	// ReuseObserveOnly:重用照常检测、记指标与日志,但不吊销家族(客户端仍被拒绝)。
	// 零值 = 检测到即吊销,是安全缺省;误伤超出预期时不必回滚代码即可退回观测。
	ReuseObserveOnly bool
}

// Validate 检查配置;越界拒绝启动,不静默截断(窗口是安全参数,配错要第一时间暴露)。
func (c Config) Validate() error {
	if !c.ttlUsable() {
		return fmt.Errorf("token config: TTL must be at least 1ms (access=%s refresh=%s)", c.AccessTokenTTL, c.RefreshTokenTTL)
	}
	// 家族记录的寿命跟着 refresh 走;access 比它活得长,家族结束之后它的 access 会被提前判无效。
	if c.RefreshTokenTTL < c.AccessTokenTTL {
		return fmt.Errorf("token config: RefreshTokenTTL=%s shorter than AccessTokenTTL=%s", c.RefreshTokenTTL, c.AccessTokenTTL)
	}
	if c.RefreshReuseGrace < 0 || c.RefreshReuseGrace > MaxRefreshReuseGrace {
		return fmt.Errorf("token config: RefreshReuseGrace=%s out of range [0, %s]", c.RefreshReuseGrace, MaxRefreshReuseGrace)
	}
	// 脚本按毫秒算:亚毫秒的值(包括 yaml 里漏写单位、被解析成纳秒的数字)会让 Go 以为窗口开着,
	// 而脚本按 0 处理、不写轮换记录,实际变成窗口 0 加吊销。
	if c.RefreshReuseGrace%time.Millisecond != 0 {
		return fmt.Errorf("token config: RefreshReuseGrace=%s must be whole milliseconds", c.RefreshReuseGrace)
	}
	return nil
}

// ttlUsable:SET PX / PEXPIRE 要毫秒级正值;非正会报错或签出永不过期的 token。
func (c Config) ttlUsable() bool {
	return c.AccessTokenTTL.Milliseconds() > 0 && c.RefreshTokenTTL.Milliseconds() > 0
}

// TokenData is the payload stored in Redis for each token.
type TokenData struct {
	Account   string `json:"account"`
	AuthType  string `json:"auth_type"`
	DeviceID  string `json:"device_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
	// FamilyID:这枚令牌所属的家族,与令牌自身嵌的家族号相同(读出时核对,不一致按损坏拒绝)。
	FamilyID string `json:"family_id"`
}

// TokenPair is returned after issuing or refreshing tokens.
type TokenPair struct {
	AccessToken        string
	RefreshToken       string
	AccessTokenExpire  int64 // unix seconds
	RefreshTokenExpire int64 // unix seconds
}

// Manager handles token CRUD operations.
type Manager struct {
	rdb redis.UniversalClient
	cfg Config
}

// NewManager creates a token manager. rdb 是登录域句柄,cfg 应先经 Validate。
func NewManager(rdb redis.UniversalClient, cfg Config) *Manager {
	return &Manager{rdb: rdb, cfg: cfg}
}

// parsedToken 是解析过的令牌字符串,字段都校验过,可以进键名。
type parsedToken struct {
	tag      string
	secret   string
	familyID string
	kind     tokenKind
}

// formatToken 拼出发给客户端的令牌字符串。
func formatToken(tag, secret string) string {
	return tag + tokenSeparator + secret
}

// parseToken 拆开令牌字符串。令牌是客户端输入,tag 与 secret 都要校验过才能进键名。
// Go 的 base64 解码会跳过 CR/LF,所以拿重新编码的结果与原串比对:同一 32 字节只对应一个字符串,夹带换行的变体不会被当成重用。
func parseToken(raw string) (parsedToken, bool) {
	tag, secret, found := strings.Cut(raw, tokenSeparator)
	if !found || !redisslot.ValidAccountTag(tag) {
		return parsedToken{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != tokenByteLen || base64.RawURLEncoding.EncodeToString(decoded) != secret {
		return parsedToken{}, false
	}
	kind := refreshKind
	if decoded[familyByteLen]&accessKindBit != 0 {
		kind = accessKind
	}
	return parsedToken{tag: tag, secret: secret, familyID: base64.RawURLEncoding.EncodeToString(decoded[:familyByteLen]), kind: kind}, true
}

// newSecret 生成属于 familyID、类型为 kind 的令牌随机部分。对外人 256 bit 全部未知;对持有过同家族令牌的人未知部分是 127 bit,猜中当前的头不可行。
func newSecret(familyID string, kind tokenKind) (string, error) {
	fid, err := base64.RawURLEncoding.DecodeString(familyID)
	if err != nil || len(fid) != familyByteLen {
		return "", errors.New("malformed family id")
	}
	b := make([]byte, tokenByteLen)
	copy(b, fid)
	if _, err := rand.Read(b[familyByteLen:]); err != nil {
		return "", err
	}
	if kind == accessKind {
		b[familyByteLen] |= accessKindBit
	} else {
		b[familyByteLen] &^= accessKindBit
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// familyLogTag 只给日志串联同一家族的几行用。家族号原值是令牌的一半,不进日志。
func familyLogTag(familyID string) string {
	sum := sha256.Sum256([]byte(familyID))
	return hex.EncodeToString(sum[:4])
}

// Issue generates a new access+refresh token pair and stores them in Redis.
func (m *Manager) Issue(ctx context.Context, account, authType, deviceID string) (*TokenPair, error) {
	if !m.cfg.ttlUsable() {
		return nil, fmt.Errorf("token TTL 必须至少为 1ms")
	}
	if account == "" || authType == "" {
		return nil, fmt.Errorf("token 账号与认证类型不能为空")
	}
	now := time.Now()
	familyID, err := generateOpaque(familyByteLen)
	if err != nil {
		return nil, fmt.Errorf("生成 token family: %w", err)
	}
	accessSecret, err := newSecret(familyID, accessKind)
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}
	refreshSecret, err := newSecret(familyID, refreshKind)
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	dataBytes, err := json.Marshal(&TokenData{Account: account, AuthType: authType, DeviceID: deviceID, CreatedAt: now.Unix(), FamilyID: familyID})
	if err != nil {
		return nil, fmt.Errorf("marshal token data: %w", err)
	}
	accessExpire := now.Add(m.cfg.AccessTokenTTL)
	refreshExpire := now.Add(m.cfg.RefreshTokenTTL)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tag := redisslot.AccountTag(account)
	if _, err := issueFamilyScript.Run(ctx, m.rdb,
		[]string{redisslot.AccessTokenKey(tag, accessSecret), redisslot.RefreshTokenKey(tag, refreshSecret), redisslot.AccountRefreshKey(account), redisslot.RefreshFamilyKey(tag, familyID)},
		dataBytes, account, now.Unix(), m.cfg.AccessTokenTTL.Milliseconds(), m.cfg.RefreshTokenTTL.Milliseconds(), now.Unix()-1, refreshExpire.Unix(), refreshSecret,
	).Int64(); err != nil {
		return nil, fmt.Errorf("store token family: %w", err)
	}
	m.trimRefreshSet(ctx, account, refreshSecret)
	logx.Infof("[Token] Issued tokens for account=%s auth_type=%s access_ttl=%v refresh_ttl=%v", account, authType, m.cfg.AccessTokenTTL, m.cfg.RefreshTokenTTL)
	return &TokenPair{AccessToken: formatToken(tag, accessSecret), RefreshToken: formatToken(tag, refreshSecret), AccessTokenExpire: accessExpire.Unix(), RefreshTokenExpire: refreshExpire.Unix()}, nil
}

// ValidateAccess validates an access token, returns the account if valid.
// 家族被吊销(重用、封顶淘汰、RevokeAll)之后,它签出的 access 即刻无效,不必等 2h 过期。
func (m *Manager) ValidateAccess(ctx context.Context, accessToken string) (*TokenData, error) {
	t, ok := parseToken(accessToken)
	if !ok || t.kind != accessKind {
		return nil, errMalformedToken
	}
	key := redisslot.AccessTokenKey(t.tag, t.secret)
	raw, data, err := m.load(ctx, t, key)
	if err != nil {
		return nil, err
	}
	valid, err := validateFamilyAccessScript.Run(ctx, m.rdb, []string{key, redisslot.RefreshFamilyKey(t.tag, t.familyID), redisslot.AccountRefreshKey(data.Account)}, raw, data.Account, t.familyID, redisslot.RefreshTokenKeyPrefix(t.tag)).Int64()
	if err != nil {
		return nil, fmt.Errorf("validate token family: %w", err)
	}
	if valid != 1 {
		return nil, fmt.Errorf("token family 已失效或不匹配")
	}
	return data, nil
}

// Refresh 轮换 refresh token:作废旧的,签发新的一对。
//
// 作废与签发是同一次原子提交(rotateRefreshScript):要么旧的仍有效、新的没签,要么旧的作废、新的已签。
// 提交前先查 ctx:请求已截止或取消就不发脚本(截止之后 gate 已放行同一会话的下一条请求,见 client-request-ordering.md §6.1)。
//
// 重试语义(RefreshReuseGrace > 0):轮换时一并写下「旧 token → 新的一对」的轮换记录,TTL = 宽限窗口。
// 窗口内同一旧 token 再来(回包丢失后的重试,或同一请求并发发了两次),只要新的 refresh token 还没被用过、家族仍在,
// 就原样返回同一对,不再签发。窗口为 0 时严格一次性。
//
// 重用检测:已轮换的旧成员再次出现,而它的后继已经被轮换走(窗口内),或已经没有轮换记录(窗口外)、家族却还活着,
// 说明有两方在用同一条链。服务端分不清哪边是合法客户端,吊销整个家族(RFC 9700 §4.14.2)。
//
// 返回的错误不含令牌值,可以直接进日志。业务拒绝一律是 ErrRefreshRejected。
func (m *Manager) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	// 非正 TTL 会让 SET PX / PEXPIRE 报错或签出永不过期的 token,提交前显式拒绝(fail-closed)。
	if !m.cfg.ttlUsable() {
		refreshTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("token TTL must be positive (access=%v refresh=%v)", m.cfg.AccessTokenTTL, m.cfg.RefreshTokenTTL)
	}
	t, ok := parseToken(refreshToken)
	if !ok || t.kind != refreshKind {
		refreshTotal.WithLabelValues("rejected").Inc()
		return nil, ErrRefreshRejected
	}
	raw, data, err := m.load(ctx, t, redisslot.RefreshTokenKey(t.tag, t.secret))
	if errors.Is(err, errTokenNotFound) {
		// 旧 token 不在:从未存在、过期,或已被轮换走(可能就是本请求丢了回包的那一次)。
		return m.replay(ctx, t)
	}
	if err != nil {
		if ctx.Err() != nil {
			refreshTotal.WithLabelValues("abandoned").Inc()
		} else {
			refreshTotal.WithLabelValues("error").Inc()
		}
		return nil, err
	}
	pair, res, err := m.rotate(ctx, t, raw, data)
	if err != nil {
		return nil, err
	}
	if res == refreshNotLive {
		// 读到旧值之后、脚本执行之前,并发的另一路抢先轮换了它:按重放判定,拿那一路换出的同一对。
		return m.replay(ctx, t)
	}
	return pair, nil
}

// rotate 发出轮换脚本。旧 token 已不是读到的那一份时回 (nil, refreshNotLive, nil),什么都没写。
func (m *Manager) rotate(ctx context.Context, t parsedToken, raw []byte, data *TokenData) (*TokenPair, int64, error) {
	now := time.Now()
	newAccess, err := newSecret(t.familyID, accessKind)
	if err != nil {
		refreshTotal.WithLabelValues("error").Inc()
		return nil, 0, fmt.Errorf("generate access token: %w", err)
	}
	newRefresh, err := newSecret(t.familyID, refreshKind)
	if err != nil {
		refreshTotal.WithLabelValues("error").Inc()
		return nil, 0, fmt.Errorf("generate refresh token: %w", err)
	}
	payload, err := json.Marshal(&TokenData{
		Account:   data.Account,
		AuthType:  data.AuthType,
		DeviceID:  data.DeviceID,
		CreatedAt: now.Unix(),
		FamilyID:  t.familyID,
	})
	if err != nil {
		refreshTotal.WithLabelValues("error").Inc()
		return nil, 0, fmt.Errorf("marshal token data: %w", err)
	}
	accessExpire := now.Add(m.cfg.AccessTokenTTL)
	refreshExpire := now.Add(m.cfg.RefreshTokenTTL)

	if err := ctx.Err(); err != nil {
		refreshTotal.WithLabelValues("abandoned").Inc()
		return nil, 0, fmt.Errorf("request done before rotating refresh token, nothing written: %w", err)
	}
	res, err := rotateRefreshScript.Run(ctx, m.rdb,
		[]string{redisslot.RefreshTokenKey(t.tag, t.secret), redisslot.AccountRefreshKey(data.Account), redisslot.AccessTokenKey(t.tag, newAccess),
			redisslot.RefreshTokenKey(t.tag, newRefresh), redisslot.RefreshFamilyKey(t.tag, t.familyID), redisslot.RefreshRotatedKey(t.tag, t.secret)},
		raw, t.secret, payload,
		m.cfg.AccessTokenTTL.Milliseconds(), m.cfg.RefreshTokenTTL.Milliseconds(),
		now.Unix()-1, refreshExpire.Unix(), newRefresh, data.Account, now.Unix(),
		m.cfg.RefreshReuseGrace.Milliseconds(), newAccess, accessExpire.Unix(),
	).Int64()
	if err != nil {
		refreshTotal.WithLabelValues("error").Inc()
		return nil, 0, fmt.Errorf("rotate refresh token: %w", err)
	}
	switch res {
	case refreshRotated:
	case refreshNotLive:
		return nil, res, nil
	case refreshFamilyGone:
		refreshTotal.WithLabelValues("rejected_family_gone").Inc()
		logx.Infof("[Token] Refresh rejected, family revoked or ended: account=%s family=%s", data.Account, familyLogTag(t.familyID))
		return nil, res, ErrRefreshRejected
	case refreshNotHead:
		refreshTotal.WithLabelValues("error").Inc()
		return nil, res, fmt.Errorf("live refresh token is not its family head: account=%s family=%s", data.Account, familyLogTag(t.familyID))
	default:
		refreshTotal.WithLabelValues("error").Inc()
		return nil, res, fmt.Errorf("unexpected rotateRefreshScript result %d", res)
	}

	// 提交之后的封顶清理,与 Issue 相同是 best-effort:请求恰好此刻截止时这一次不清,集合最多暂时多出
	// 一枚活成员,下一次签发会清掉。
	m.trimRefreshSet(ctx, data.Account, newRefresh)

	refreshTotal.WithLabelValues("rotated").Inc()
	logx.Infof("[Token] Rotated refresh token for account=%s auth_type=%s access_ttl=%v refresh_ttl=%v reuse_grace=%v",
		data.Account, data.AuthType, m.cfg.AccessTokenTTL, m.cfg.RefreshTokenTTL, m.cfg.RefreshReuseGrace)

	return &TokenPair{
		AccessToken:        formatToken(t.tag, newAccess),
		RefreshToken:       formatToken(t.tag, newRefresh),
		AccessTokenExpire:  accessExpire.Unix(),
		RefreshTokenExpire: refreshExpire.Unix(),
	}, res, nil
}

// replay 判定一枚已不在的旧 refresh token(replayRefreshScript):宽限重放、重用吊销或普通拒绝。所有分支都回 ErrRefreshRejected 或存储错误,
// 只有宽限重放回令牌。
func (m *Manager) replay(ctx context.Context, t parsedToken) (*TokenPair, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replayBudget)
	defer cancel()
	reply, err := m.runReplayScript(ctx, t)
	if err != nil {
		refreshTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("refresh replay check: family=%s: %w", familyLogTag(t.familyID), err)
	}
	code, ok := reply[0].(int64)
	if !ok {
		code = replayDamaged
	}
	switch code {
	case replaySamePair:
		return m.samePair(t, reply)
	case replaySuccessorGone:
		refreshTotal.WithLabelValues("rejected_successor_gone").Inc()
		logx.Infof("[Token] Refresh replay rejected, successor expired or removed: family=%s", familyLogTag(t.familyID))
		return nil, ErrRefreshRejected
	case replayFamilyGone:
		refreshTotal.WithLabelValues("rejected_family_gone").Inc()
		logx.Infof("[Token] Refresh replay rejected, family revoked or ended: family=%s", familyLogTag(t.familyID))
		return nil, ErrRefreshRejected
	case replayRejected:
		refreshTotal.WithLabelValues("rejected").Inc()
		return nil, ErrRefreshRejected
	case replayReuseRevoked, replayReuseObserved:
		m.reportReuse(t, code, reply)
		return nil, ErrRefreshRejected
	default:
		refreshTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("refresh replay check: token family state damaged: family=%s", familyLogTag(t.familyID))
	}
}

// runReplayScript:脚本幂等,在 ctx 的预算内有界重试;脚本的明确拒绝不重试。
func (m *Manager) runReplayScript(ctx context.Context, t parsedToken) ([]interface{}, error) {
	observeOnly := "0"
	if m.cfg.ReuseObserveOnly {
		observeOnly = "1"
	}
	keys := []string{redisslot.RefreshFamilyKey(t.tag, t.familyID), redisslot.RefreshRotatedKey(t.tag, t.secret)}
	var err error
	for attempt := 0; attempt < replayAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, errors.Join(err, ctx.Err())
			case <-time.After(replayBackoff << (attempt - 1)):
			}
		}
		var reply []interface{}
		reply, err = replayRefreshScript.Run(ctx, m.rdb, keys,
			t.secret, m.cfg.RefreshReuseGrace.Milliseconds(), MaxRefreshReuseGrace.Milliseconds(),
			redisslot.RefreshTokenKeyPrefix(t.tag), redisslot.AccountRefreshKeyPrefix(t.tag), observeOnly,
		).Slice()
		if err == nil {
			if len(reply) == 0 {
				return nil, errors.New("empty replay script reply")
			}
			return reply, nil
		}
		var scriptErr redis.Error
		if errors.As(err, &scriptErr) {
			return nil, err
		}
		// 脚本可能已经执行、只是回包丢了。重试时家族已不在,reuse_revoked 和 SECURITY 日志就没有了,这一行留个线索。
		logx.Errorf("[Token] Refresh replay check attempt %d failed, result unknown: family=%s: %v", attempt+1, familyLogTag(t.familyID), err)
	}
	return nil, err
}

// samePair 把脚本回的轮换记录装成令牌对。缺字段说明记录被外部改过,按损坏拒绝。
func (m *Manager) samePair(t parsedToken, reply []interface{}) (*TokenPair, error) {
	if len(reply) == 7 {
		account, _ := reply[1].(string)
		access, _ := reply[2].(string)
		refresh, _ := reply[3].(string)
		accessExpire, accessErr := strconv.ParseInt(replyString(reply[4]), 10, 64)
		refreshExpire, refreshErr := strconv.ParseInt(replyString(reply[5]), 10, 64)
		if access != "" && refresh != "" && accessErr == nil && refreshErr == nil {
			refreshTotal.WithLabelValues("replayed").Inc()
			logx.Infof("[Token] Refresh replay within reuse grace, returning the same pair: account=%s rotated_at=%s", account, replyString(reply[6]))
			return &TokenPair{
				AccessToken:        formatToken(t.tag, access),
				RefreshToken:       formatToken(t.tag, refresh),
				AccessTokenExpire:  accessExpire,
				RefreshTokenExpire: refreshExpire,
			}, nil
		}
	}
	refreshTotal.WithLabelValues("error").Inc()
	return nil, fmt.Errorf("refresh rotation record damaged: family=%s", familyLogTag(t.familyID))
}

// reportReuse 记重用的指标和 SECURITY 日志。检测与吊销已在脚本里一次完成。
func (m *Manager) reportReuse(t parsedToken, code int64, reply []interface{}) {
	var account, replayed string
	var delay, familyAge int64
	if len(reply) == 5 {
		account = replyString(reply[1])
		replayed = replyString(reply[2])
		now := time.Now().Unix()
		rotatedAt, _ := strconv.ParseInt(replyString(reply[3]), 10, 64)
		createdAt, _ := strconv.ParseInt(replyString(reply[4]), 10, 64)
		delay = max(now-rotatedAt, 0)
		familyAge = max(now-createdAt, 0)
	}
	if replayed != "predecessor" {
		replayed = "older"
	}
	reuseDelay.WithLabelValues(replayed).Observe(float64(delay))
	result, revoked := "reuse_revoked", true
	if code == replayReuseObserved {
		result, revoked = "reuse_observed", false
	}
	refreshTotal.WithLabelValues(result).Inc()
	logx.Errorf("[Token] SECURITY refresh token reuse detected: account=%s family=%s replayed=%s delay=%ds family_age=%ds revoked=%t",
		account, familyLogTag(t.familyID), replayed, delay, familyAge, revoked)
}

// replyString:脚本回的字段是 bulk string;false(nil)和别的类型都当空串。
func replyString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// trimRefreshSet 在一个 Lua 提交内淘汰最旧成员及其尚未轮换的 family。
// 与原合同相同，提交后的容量整理为 best-effort；错误不伪装成整理成功。
// protect 是刚签发的 refresh secret,不淘汰:它的 score 不一定最大(TTL 调小之后、同一秒内多次登录),淘汰了就是把作废的令牌交给客户端。
func (m *Manager) trimRefreshSet(ctx context.Context, account, protect string) {
	if _, err := m.retire(ctx, account, maxRefreshTokensPerAccount, protect); err != nil {
		logx.Errorf("[Token] trim refresh families failed: %v", err)
	}
}

// RevokeAll 原子撤销本账号当前 refresh 及对应 family，使已有 family access 同步失效。
// 并发的新认证只能线性化在撤销之前或之后，不会因旧索引的分步删除误伤新 family。
func (m *Manager) RevokeAll(ctx context.Context, account string) error {
	count, err := m.retire(ctx, account, 0, "")
	if err != nil {
		return fmt.Errorf("revoke refresh families: %w", err)
	}
	logx.Infof("[Token] Revoked refresh families for account=%s count=%d", account, count)
	return nil
}

// retire 淘汰账号索引里超出 keep 的最旧成员。脚本现拼的 refresh / family 键都带账号 tag,与索引键同槽。
func (m *Manager) retire(ctx context.Context, account string, keep int, protect string) (int64, error) {
	tag := redisslot.AccountTag(account)
	return retireRefreshScript.Run(ctx, m.rdb, []string{redisslot.AccountRefreshKey(account)},
		account, keep, redisslot.RefreshTokenKeyPrefix(tag), redisslot.RefreshFamilyKeyPrefix(tag), protect).Int64()
}

// load 读出 token 键的原始字节并解析。Refresh 要原始字节交给轮换脚本做按值比对(见 rotateRefreshScript)。
// 令牌里的 tag 必须等于 payload 账号的 AccountTag:账号索引键按账号算槽,家族键按 tag 算槽,两者不一致的载荷进不了同一段脚本。
// 令牌里的家族号必须等于 payload 里的:不一致是 bug 或数据损坏,不猜哪一个对。
func (m *Manager) load(ctx context.Context, t parsedToken, key string) ([]byte, *TokenData, error) {
	val, err := m.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil, errTokenNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("Redis lookup: %w", err)
	}

	var data TokenData
	if err := json.Unmarshal(val, &data); err != nil {
		return nil, nil, fmt.Errorf("unmarshal token data: %w", err)
	}
	if data.Account == "" || data.AuthType == "" || data.CreatedAt <= 0 {
		return nil, nil, fmt.Errorf("token payload 身份字段损坏")
	}
	if data.FamilyID != t.familyID {
		return nil, nil, fmt.Errorf("token family 与令牌不符")
	}
	if redisslot.AccountTag(data.Account) != t.tag {
		return nil, nil, fmt.Errorf("token tag 与账号不匹配")
	}
	return val, &data, nil
}

func generateOpaque(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
