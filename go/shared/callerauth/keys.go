package callerauth

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
)

// 内部身份密钥的环境变量(docs/design/gate-online-identity.md §4、§10.1)。
//
// 只有 gate 持私钥;login 与在线域服务只持公钥。公钥不是密钥,可以进 ConfigMap、日志与 -DryRun 输出;
// 私钥只走 Secret,不落盘、不进日志。C++ gate 的同名常量在 cpp/libs/engine/core/security/caller_auth.h。
const (
	// SigningKeyEnv 是 gate 的签名私钥:32 字节 Ed25519 种子的十六进制(64 个字符)。
	// Go 服务不读它;这里定义是为了让密钥工具、部署契约与 C++ 引用同一个名字。
	SigningKeyEnv = "MMORPG_INTERNAL_AUTH_SIGNING_KEY"
	// PublicKeysEnv 是验证方的公钥列表:逗号分隔,每项是 32 字节 Ed25519 公钥的十六进制。
	// 轮换期同时列新旧两把(先加新公钥滚动验证方,再换 gate 的种子,最后撤旧公钥)。
	// gate 也读它,自己的公钥不在列表里就拒绝启动。
	PublicKeysEnv = "MMORPG_INTERNAL_AUTH_PUBLIC_KEYS"
)

// ErrBadKey 表示密钥配置的形状不对。错误信息只点格式,绝不回显值。
var ErrBadKey = errors.New("callerauth: malformed internal identity key")

// PublicKeys 是验证方持有的 Ed25519 公钥集合,实现 KeySet。
type PublicKeys []ed25519.PublicKey

// Verify 依次用集合里的公钥核签名。签名长度不对直接拒绝,不进入椭圆曲线运算。
func (keys PublicKeys) Verify(signature, data []byte) bool {
	if len(signature) != ed25519.SignatureSize {
		return false
	}
	for _, key := range keys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, data, signature) {
			return true
		}
	}
	return false
}

// Ed25519Signer 用私钥签 canonical 串,实现 Signer。
//
// 生产签名方只有 C++ gate;这个类型给 Go 测试替身(callerauthtest)与密钥工具用。
type Ed25519Signer struct {
	key ed25519.PrivateKey
}

// NewEd25519Signer 由 32 字节种子构造签名器。
func NewEd25519Signer(seed []byte) (Ed25519Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return Ed25519Signer{}, ErrBadKey
	}
	return Ed25519Signer{key: ed25519.NewKeyFromSeed(seed)}, nil
}

// Sign 返回 64 字节签名;零值签名器返回 nil,SignCanonical 会把它当成「密钥未配置」拒绝。
func (s Ed25519Signer) Sign(data []byte) []byte {
	if len(s.key) != ed25519.PrivateKeySize {
		return nil
	}
	return ed25519.Sign(s.key, data)
}

// PublicKey 返回与种子配对的公钥;零值签名器返回 nil。
func (s Ed25519Signer) PublicKey() ed25519.PublicKey {
	if len(s.key) != ed25519.PrivateKeySize {
		return nil
	}
	return s.key.Public().(ed25519.PublicKey)
}

// ParseSeedHex 解析 SigningKeyEnv 的取值:恰好 64 个十六进制字符,不带空白。
func ParseSeedHex(value string) ([]byte, error) {
	return decodeFixedHex(value, ed25519.SeedSize)
}

// ParsePublicKeys 解析 PublicKeysEnv 的取值:逗号分隔,至少一项;每项恰好 64 个十六进制字符;
// 不许空项、首尾空白或重复。任何一项不合格整份拒绝 —— 验证方宁可拒启,也不能带着半份公钥上线。
func ParsePublicKeys(value string) (PublicKeys, error) {
	if value == "" {
		return nil, ErrBadKey
	}
	parts := strings.Split(value, ",")
	keys := make(PublicKeys, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		raw, err := decodeFixedHex(part, ed25519.PublicKeySize)
		if err != nil {
			return nil, err
		}
		normalized := string(raw)
		if _, dup := seen[normalized]; dup {
			return nil, ErrBadKey
		}
		seen[normalized] = struct{}{}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// decodeFixedHex 要求取值恰好是 size 字节的十六进制,且没有任何空白。
func decodeFixedHex(value string, size int) ([]byte, error) {
	if len(value) != size*2 || value != strings.TrimSpace(value) {
		return nil, ErrBadKey
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != size {
		return nil, ErrBadKey
	}
	return raw, nil
}
