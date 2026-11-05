package callerauth

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/grpc/metadata"
)

// Options 配置验签闸门。
type Options struct {
	// Secrets 是验签密钥集合(主密钥 + 轮换期的旧密钥)。
	// 为 nil 或未配置时,任何签名都验不过 —— fail-closed。
	Secrets SecretVerifier

	// Enforce 决定验签失败时是**拒绝请求**还是**放行并打 WARN**。
	// 它由运行模式推导(生产恒 true),不接受业务配置直接关掉。
	Enforce bool

	// MaxClockSkew 是允许的双向时间戳偏移。<=0 时取默认 30s。
	MaxClockSkew time.Duration

	// AllowedCallers 是调用方白名单,留空表示不限名字(签名仍必须验过)。
	AllowedCallers []string

	// MaxNonceEntries 是 nonce 表硬上限,<=0 取默认 500000。
	MaxNonceEntries int

	// Now 可注入时钟,只为单测;生产留 nil 用 time.Now。
	Now func() time.Time

	// OnNonceOverflow 在 nonce 表被迫提前翻代时回调(打日志/计数)。
	OnNonceOverflow func()
}

const defaultMaxClockSkew = 30 * time.Second

// Verifier 校验一条内部调用方身份声明。并发安全。
type Verifier struct {
	secrets  SecretVerifier
	enforce  bool
	skew     time.Duration
	allowed  map[string]struct{}
	nonces   *nonceSet
	nowFunc  func() time.Time
	callerOK bool // AllowedCallers 是否生效(空表示不限)
}

// NewVerifier 构造校验器。
func NewVerifier(opts Options) *Verifier {
	skew := opts.MaxClockSkew
	if skew <= 0 {
		skew = defaultMaxClockSkew
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	v := &Verifier{
		secrets: opts.Secrets,
		enforce: opts.Enforce,
		skew:    skew,
		nowFunc: now,
		// nonce 保留窗口取 2×skew:必须覆盖「时间戳还可能被接受」的整个跨度
		// (now-skew .. now+skew),否则窗口边缘会漏掉重放。
		nonces: newNonceSet(2*skew, opts.MaxNonceEntries, opts.OnNonceOverflow),
	}
	if len(opts.AllowedCallers) > 0 {
		v.allowed = make(map[string]struct{}, len(opts.AllowedCallers))
		for _, c := range opts.AllowedCallers {
			if c != "" {
				v.allowed[c] = struct{}{}
			}
		}
		v.callerOK = len(v.allowed) > 0
	}
	return v
}

// Enforcing 表示验签失败是否会导致请求被拒。
func (v *Verifier) Enforcing() bool { return v != nil && v.enforce }

// Verify 校验一条身份声明。
//
// subject 传 SessionDetails 的**原始序列化字节**(调用方已经把 base64 解掉)。
// 返回的 caller 只在成功时有意义。
//
// 检查顺序是刻意排的:**先验签名,后记 nonce**。反过来的话,未认证的流量
// 就能往 nonce 表里灌垃圾,既能撑爆内存,又能通过预占 nonce 让合法请求
// 被误判成重放 —— 那是自带的拒绝服务。
func (v *Verifier) Verify(md metadata.MD, fullMethod string, subject []byte) (string, error) {
	caller := mdFirst(md, MetaCaller)
	if caller == "" {
		return "", ErrMissingCaller
	}
	if v.callerOK {
		if _, ok := v.allowed[caller]; !ok {
			return "", fmt.Errorf("%w: caller=%s", ErrCallerNotAllowed, caller)
		}
	}

	sigHex := mdFirst(md, MetaSignature)
	if sigHex == "" {
		return "", ErrMissingSignature
	}
	mac, err := hex.DecodeString(sigHex)
	if err != nil || len(mac) == 0 {
		return "", ErrBadSignature
	}

	tsRaw := mdFirst(md, MetaTimestamp)
	if tsRaw == "" {
		return "", ErrBadTimestamp
	}
	tsMs, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return "", ErrBadTimestamp
	}
	now := v.nowFunc()
	delta := now.Sub(time.UnixMilli(tsMs))
	if delta < 0 {
		delta = -delta
	}
	if delta > v.skew {
		return "", fmt.Errorf("%w: |now-ts|=%s > %s", ErrClockSkew, delta, v.skew)
	}

	nonce := mdFirst(md, MetaNonce)
	if len(nonce) < minNonceLen || len(nonce) > maxNonceLen {
		return "", ErrBadNonce
	}

	if v.secrets == nil || !v.secrets.Verify(mac, Canonical(caller, fullMethod, subject, tsMs, nonce)) {
		return "", ErrBadSignature
	}

	if !v.nonces.admit(nonce, now) {
		return "", ErrReplay
	}
	return caller, nil
}

// verifyOnly 是 Verify 的丢弃 caller 版本。拦截器那层用不到 caller
// (指标不允许按 caller 打 label,日志也不打自报字符串),包一层省得
// 每个调用点写 `_, err :=`。
func (v *Verifier) verifyOnly(md metadata.MD, fullMethod string, subject []byte) error {
	_, err := v.Verify(md, fullMethod, subject)
	return err
}

// mdFirst 取 metadata 里某个 key 的第一个值。gRPC 允许同名多值,
// 这里只认第一个 —— 多值本身就是异常输入,不做拼接以免留下歧义。
func mdFirst(md metadata.MD, key string) string {
	vals := md.Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
