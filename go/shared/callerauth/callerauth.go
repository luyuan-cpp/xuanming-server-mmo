// Package callerauth 给 login 的**内部调用方身份声明**加签名闸门。
//
// # 修的是什么洞
//
// login 的 SessionInterceptor 原本从 gRPC metadata 取 `x-session-detail-bin`,
// base64 解码 + proto.Unmarshal 成 SessionDetails 之后**直接写进 ctx 当可信
// 身份用**,全程没有任何校验。后果:任何能连到 login gRPC 端口的进程
// (同集群里任意 Pod、任意被攻陷的边车、任意能穿到内网的人)都可以自称是
// 任意会话的任意玩家,进而调 Disconnect / LeaveGame / EnterGame 操纵别人的档。
// 这不是「缺一层防御」,是**根本没有认证**。
//
// # 修法
//
// 调用方必须对身份声明附一条 HMAC-SHA256 签名。canonical 串把下面这些
// 全绑进去,少一样都会留下可利用的缝:
//
//	行1  版本前缀      钉死算法与串格式,将来换版本不会被降级重放
//	行2  调用方标识    知道是谁在声明(可选白名单)
//	行3  目标方法全名  防「拿 Disconnect 的签名去调 EnterGame」的跨方法重放
//	行4  主体摘要      sha256(SessionDetails 原始字节),绑死声明的是哪个会话/玩家
//	行5  毫秒时间戳    配合 MaxClockSkew 划出重放时间窗
//	行6  随机 nonce    时间窗内的重放由 nonce 表拦掉
//
// 具体串形(LF 分隔,**末尾无换行**):
//
//	mmorpg-login-caller-auth/v1\n
//	<caller>\n
//	<fullMethod>\n
//	<lowercase-hex(sha256(subject))>\n
//	<timestampMs 十进制>\n
//	<nonce>
//
//	signature = lowercase-hex( HMAC-SHA256(secret, canonical) )
//
// # 上游怎么接(cpp gate / Java gateway 照这个实现)
//
// 除既有的 `x-session-detail-bin` 外,再挂四个纯 ASCII 的 metadata:
//
//	x-caller-id     调用方标识,如 "gate"
//	x-caller-ts     毫秒时间戳,十进制
//	x-caller-nonce  每次调用现取的随机串(建议 16 字节随机数转 hex)
//	x-caller-sig    上面 canonical 串的 HMAC 十六进制小写
//
// ⚠️ `x-session-detail-bin` 的取值口径**保持不动**:仍然是
// base64(SessionDetails 序列化字节)。gRPC 对 `-bin` 后缀的 key 自己还会再套
// 一层 base64,所以线上实际是双层编码 —— 这是既有行为,别去「优化」它,
// 改了会和 cpp gate 对不上。canonical 里的 subject 用的是**解码后的原始
// proto 字节**,签验双方各自在自己这侧拿到的都是同一串字节。
//
// # 边界
//
// nonce 表是**进程内**的。login 是多副本部署,理论上同一条签名可以在时间窗内
// 重放到另一个副本。这里刻意不上 Redis:每个 RPC 加一次 Redis 往返会把
// 登录热路径的 P99 直接抬上去,而时间窗(默认 30s)+ 方法绑定 + 主体绑定
// 已经把可利用面压到「同一条签名在 30s 内对另一个副本重复同一个方法同一个
// 会话」。要彻底闭合需要共享 nonce 存储,那是独立一件事,记在这里别忘。
package callerauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// canonicalVersion 是 canonical 串的第一行,钉死串格式与算法。
// 将来要换算法(比如上 Ed25519)必须换这个前缀,让旧签名验不过去,
// 免得被降级重放。
const canonicalVersion = "mmorpg-login-caller-auth/v1"

// gRPC metadata key。全小写 —— gRPC 会把 key 统一小写,写成大写会取不到。
const (
	// MetaSessionDetail 是既有的身份声明载体,取值 = base64(SessionDetails 字节)。
	MetaSessionDetail = "x-session-detail-bin"
	// MetaCaller 是调用方标识。
	MetaCaller = "x-caller-id"
	// MetaTimestamp 是毫秒时间戳(十进制)。
	MetaTimestamp = "x-caller-ts"
	// MetaNonce 是本次调用的随机串。
	MetaNonce = "x-caller-nonce"
	// MetaSignature 是 canonical 串的 HMAC 十六进制小写。
	MetaSignature = "x-caller-sig"
)

// nonce 长度约束。太短会让 nonce 空间小到能撞、也能被穷举预占;
// 太长白占 nonce 表内存。16~128 个可见字符是个宽松又够用的区间。
const (
	minNonceLen = 16
	maxNonceLen = 128
)

// 校验失败的原因。全部导出是为了让拦截器能按原因打不同的日志/指标,
// 也让测试能精确断言到底是哪一步拒的。
var (
	ErrMissingCaller    = errors.New("缺少调用方标识 x-caller-id")
	ErrCallerNotAllowed = errors.New("调用方不在 AllowedCallers 白名单内")
	ErrMissingSignature = errors.New("身份声明未附签名")
	ErrBadSignature     = errors.New("签名校验不通过")
	ErrBadTimestamp     = errors.New("时间戳格式非法")
	ErrClockSkew        = errors.New("时间戳超出允许的偏移窗口")
	ErrBadNonce         = errors.New("nonce 缺失或长度非法")
	ErrReplay           = errors.New("nonce 已被使用过(重放)")
	ErrBadPayload       = errors.New("身份声明无法解码")
)

// Reason 把一个错误压成**低基数**的指标 label。
// 绝不能把原始错误信息当 label(基数会炸,仓库 CLAUDE.md §9)。
func Reason(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrMissingCaller):
		return "missing_caller"
	case errors.Is(err, ErrCallerNotAllowed):
		return "caller_not_allowed"
	case errors.Is(err, ErrMissingSignature):
		return "missing_signature"
	case errors.Is(err, ErrBadSignature):
		return "bad_signature"
	case errors.Is(err, ErrBadTimestamp):
		return "bad_timestamp"
	case errors.Is(err, ErrClockSkew):
		return "clock_skew"
	case errors.Is(err, ErrBadNonce):
		return "bad_nonce"
	case errors.Is(err, ErrReplay):
		return "replay"
	case errors.Is(err, ErrBadPayload):
		return "bad_payload"
	default:
		return "other"
	}
}

// Signer 是签名侧只需要的能力(config.SecretSet 实现了它)。
// 这里用接口而不是直接依赖 config,是为了让本包能被单测独立驱动。
type Signer interface {
	Sign(data []byte) []byte
}

// Verifier 是验签侧只需要的能力:轮换期要能用「主密钥 + 若干旧密钥」逐一验。
type SecretVerifier interface {
	Verify(mac, data []byte) bool
}

// Canonical 拼出待签名串。签验两侧必须逐字节一致,所以它是这个包里
// **唯一**允许出现串格式知识的地方。
//
// subject 传 SessionDetails 的**原始序列化字节**(不是 base64 文本)。
func Canonical(caller, fullMethod string, subject []byte, timestampMs int64, nonce string) []byte {
	sum := sha256.Sum256(subject)
	var b strings.Builder
	b.Grow(len(canonicalVersion) + len(caller) + len(fullMethod) + len(nonce) + 96)
	b.WriteString(canonicalVersion)
	b.WriteByte('\n')
	b.WriteString(caller)
	b.WriteByte('\n')
	b.WriteString(fullMethod)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(sum[:]))
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(timestampMs, 10))
	b.WriteByte('\n')
	b.WriteString(nonce)
	return []byte(b.String())
}

// SignCanonical 算出可以直接放进 x-caller-sig 的十六进制小写签名。
func SignCanonical(s Signer, caller, fullMethod string, subject []byte, timestampMs int64, nonce string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("callerauth: 没有可用于签名的密钥")
	}
	mac := s.Sign(Canonical(caller, fullMethod, subject, timestampMs, nonce))
	if len(mac) == 0 {
		return "", fmt.Errorf("callerauth: 密钥未配置,拒绝用空密钥签名")
	}
	return hex.EncodeToString(mac), nil
}

// NewNonce 生成一个 32 字符的十六进制 nonce(16 字节随机)。
// 用 crypto/rand:nonce 可预测等于重放防线作废。
func NewNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("callerauth: 生成 nonce 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NowMillis 是签名时间戳的取值口径,单独抽出来免得两侧各写一份。
func NowMillis() int64 { return time.Now().UnixMilli() }

// encodeSubject 把 SessionDetails 原始字节编成 x-session-detail-bin 的取值。
// 口径必须与拦截器的解码侧完全对称(标准 base64,带 padding)。
func encodeSubject(subject []byte) string {
	return base64.StdEncoding.EncodeToString(subject)
}

// formatMillis 是 x-caller-ts 的取值口径:十进制、无前导零、无单位后缀。
func formatMillis(ms int64) string { return strconv.FormatInt(ms, 10) }
