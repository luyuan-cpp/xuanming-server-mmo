package assetop

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	assetpb "proto/common/asset"
)

// 资产 RPC 的鉴权(规格 §4.32)。
//
// 为什么签名放在**请求体**里而不是 gRPC metadata:scene 的 gRPC 包装函数由生成器产出,
// context 参数被注释掉(`grpc::ServerContext* /*context*/`),守护段里根本读不到 metadata;
// 而 scene 的 gRPC 端口用的是明文不安全凭据,集群内任何进程都能连。签名是主防线。
//
// 为什么不要 nonce:同一 (player, stream, epoch, seq) 重放要么只读答复、要么就是那唯一一次
// 应用,Go 总以 scene 的结局为准。时间窗只用来限制截获包的寿命。

const (
	// CanonicalVersion 是待签名串的第一行。它是跨语言契约的一部分:
	// C++ 侧 AssetOpCanonical 用同一字面量,改它等于改协议。
	CanonicalVersion = "mmorpg-asset-op/v1"

	// MinSecretLen 是调用方密钥的最小字节数。去掉首尾空白后不足这么长视同**未配置**,
	// 一律拒绝签名 —— 资产路径没有 dev 放行,本地由脚本显式注入开发值(规格 §4.32)。
	MinSecretLen = 32

	// MaxClockSkewMs 是 scene 接受的签名时间窗,这里只作为文档与测试的共同出处;
	// 真正的判定在 scene(C++ kAssetOpAuthMaxSkewMs)。两边必须同值。
	MaxClockSkewMs = 300000
)

var (
	// ErrWeakSecret 密钥为空或过短。构造期就失败,免得跑起来才发现全部请求被 scene 拒。
	ErrWeakSecret = errors.New("assetop: caller secret shorter than 32 bytes")
	// ErrNoSigner Caller 没配 Signer。**不允许**不签名发包:scene 会回 UNKNOWN 不记账,
	// 行会一直卡着,不如在调用方当场失败。
	ErrNoSigner = errors.New("assetop: caller has no signer configured")
	// ErrEmptyCaller 调用方名字为空;它进 canonical 串,也是 scene 的流白名单键。
	ErrEmptyCaller = errors.New("assetop: caller name is empty")
)

// Signer 持有某个调用方("guild" / "trade")的密钥,并按 canonical 串签名。
// 构造后只读,可被多 goroutine 共享。
type Signer struct {
	caller string
	secret []byte
}

// NewSigner 校验并持有密钥。**密钥值绝不进日志、指标或错误文本**(AGENTS §11.3),
// 所以这里的错误只说"太短",不说实际长度以外的任何内容。
func NewSigner(caller, secret string) (*Signer, error) {
	name := strings.TrimSpace(caller)
	if name == "" {
		return nil, ErrEmptyCaller
	}
	key := strings.TrimSpace(secret)
	if len(key) < MinSecretLen {
		return nil, fmt.Errorf("%w (caller=%s)", ErrWeakSecret, name)
	}
	return &Signer{caller: name, secret: []byte(key)}, nil
}

// Caller 返回调用方名字(进 canonical 串、进 scene 的流白名单)。
func (s *Signer) Caller() string { return s.caller }

// Canonical 拼出待签名串:LF 分隔、末尾无换行、全部十进制。
// 签验两侧必须逐字节一致,所以这是本包**唯一**允许出现串格式知识的地方。
//
// caller 取自 req.Auth.Caller:Sign 会先把它写好再调本函数。
// rpc 进串,防止拿 Abort 的签名去调 Credit;bundle 进串,防止中途改金额。
func Canonical(rpc RPC, req *assetpb.AssetOpRequest, tsMs uint64) []byte {
	var b strings.Builder
	b.Grow(160)
	b.WriteString(CanonicalVersion)
	b.WriteByte('\n')
	b.WriteString(req.GetAuth().GetCaller())
	b.WriteByte('\n')
	b.WriteString(rpc.String())
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(req.GetPlayerId(), 10))
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(int64(req.GetStream()), 10))
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(req.GetStreamEpoch(), 10))
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(req.GetSeq(), 10))
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(req.GetCorrelationId(), 10))
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(uint64(req.GetTxType()), 10))
	b.WriteByte('\n')
	writeBundleCanonical(&b, req.GetBundle())
	b.WriteByte('\n')
	b.WriteString(strconv.FormatUint(tsMs, 10))
	return []byte(b.String())
}

// writeBundleCanonical 写 bundle 段:"c=" 货币按**请求顺序** "<type>:<amount>" 逗号分隔,
// ";i=" 物品同理,";u=" 按 guid 扣的物品实例,";p=" 宝宝;全空则是 "c=;i=;u=;p=0"。
// 顺序不排序:scene 按同一份请求重算,排序只会给两侧各留一个出错的机会。
//
// **u/p 两段是安全边界,不是可选装饰**(2026-09-19 补):scene 的 gRPC 用的是不安全凭据,
// 集群内任意进程可连可嗅。这两个字段会真改玩家资产(按 guid 扣装备 / 扣宝宝),不进签名串
// 的话,攻击者截下一条合法的 TRADE_DEBIT、只把 item_uuids 换成该玩家的其它装备再发出去,
// 这个 seq scene 没见过、签名照样通过,扣掉的就是被换的那件。seq 幂等与 300s 时间窗都挡不住
// 这种「同 seq 抢跑改载荷」。帮会走 GUILD_* 流、这两段恒为空,只多两段固定字面量。
//
// 改这里 = 改协议:C++ AssetOpAuth 的 AppendBundleCanonical 必须同批改,
// 两边的 golden 用例(auth_test.go 的 TestCanonicalGolden 与 asset_op_auth_test.cpp)也是。
func writeBundleCanonical(b *strings.Builder, bundle *assetpb.AssetBundle) {
	b.WriteString("c=")
	for i, c := range bundle.GetCurrencies() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(c.GetCurrencyType()), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(c.GetAmount(), 10))
	}
	b.WriteString(";i=")
	for i, it := range bundle.GetItems() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(it.GetConfigId()), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(uint64(it.GetCount()), 10))
	}
	b.WriteString(";u=")
	for i, uuid := range bundle.GetItemUuids() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uuid, 10))
	}
	// 单值,空缺写 0:宝宝 guid 恒非 0,所以 0 与「没带宝宝」不会混淆。
	b.WriteString(";p=")
	b.WriteString(strconv.FormatUint(bundle.GetPetId(), 10))
}

// Sign 就地把 auth 写进 req:caller、timestamp_ms、signature_hex。
//
// 调用方必须传**这一次实际发包**的时间:每次重试 / 重查都要重签,否则时间窗会过期。
// req 应当是调用方自己克隆出来的副本 —— Caller.invoke 正是这么做的。
func (s *Signer) Sign(rpc RPC, req *assetpb.AssetOpRequest, nowMs uint64) {
	if s == nil || req == nil {
		return
	}
	req.Auth = &assetpb.AssetOpAuth{
		Caller:      s.caller,
		TimestampMs: nowMs,
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(Canonical(rpc, req, nowMs))
	req.Auth.SignatureHex = hex.EncodeToString(mac.Sum(nil))
}
