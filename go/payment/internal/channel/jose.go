package channel

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
)

// 本文件是 JWT / JWS 的最小实现,只覆盖本服务用到的两种算法:
//   - ES256(App Store Server API 鉴权、App Store 签名数据);签名是固定长度的 R||S(各 32 字节),不是 DER;
//   - RS256(Google 服务账号换令牌、Pub/Sub 推送的 OIDC 令牌)。
// 不接受 alg=none,也不按令牌头里的算法挑密钥:算法由调用方指定,头里写的不一致就拒绝。

var b64url = base64.RawURLEncoding

// jwtSign 拼出 header.payload 的签名输入。
func jwtSign(header, claims any, sign func(input []byte) ([]byte, error)) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64url.EncodeToString(headerJSON) + "." + b64url.EncodeToString(claimsJSON)
	signature, err := sign([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + b64url.EncodeToString(signature), nil
}

func signES256(key *ecdsa.PrivateKey) func([]byte) ([]byte, error) {
	return func(input []byte) ([]byte, error) {
		digest := sha256.Sum256(input)
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			return nil, err
		}
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		s.FillBytes(out[32:])
		return out, nil
	}
}

func signRS256(key *rsa.PrivateKey) func([]byte) ([]byte, error) {
	return func(input []byte) ([]byte, error) {
		digest := sha256.Sum256(input)
		return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	}
}

// jwsParts 拆出紧凑序列化的三段,并解出头与载荷。
type jwsParts struct {
	header       []byte
	payload      []byte
	signingInput []byte
	signature    []byte
}

func splitJWS(token string) (jwsParts, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return jwsParts{}, errors.New("payment channel: token is not a compact JWS")
	}
	header, err := b64url.DecodeString(segments[0])
	if err != nil {
		return jwsParts{}, errors.New("payment channel: JWS header is not base64url")
	}
	payload, err := b64url.DecodeString(segments[1])
	if err != nil {
		return jwsParts{}, errors.New("payment channel: JWS payload is not base64url")
	}
	signature, err := b64url.DecodeString(segments[2])
	if err != nil {
		return jwsParts{}, errors.New("payment channel: JWS signature is not base64url")
	}
	return jwsParts{header: header, payload: payload, signingInput: []byte(segments[0] + "." + segments[1]), signature: signature}, nil
}

func verifyES256(key *ecdsa.PublicKey, input, signature []byte) error {
	if len(signature) != 64 {
		return errors.New("payment channel: ES256 signature is not 64 bytes")
	}
	digest := sha256.Sum256(input)
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(key, digest[:], r, s) {
		return errors.New("payment channel: ES256 signature rejected")
	}
	return nil
}

func verifyRS256(key *rsa.PublicKey, input, signature []byte) error {
	digest := sha256.Sum256(input)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return errors.New("payment channel: RS256 signature rejected")
	}
	return nil
}

// ParseECPrivateKey 读 App Store Connect 下载的 .p8(PKCS#8 PEM 的 P-256 私钥)。
func ParseECPrivateKey(text string) (*ecdsa.PrivateKey, error) {
	der, err := keyDER(text, "PRIVATE KEY", "EC PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		ecKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("payment channel: private key is not ECDSA")
		}
		return ecKey, nil
	}
	key, err := x509.ParseECPrivateKey(der)
	if err != nil {
		return nil, errors.New("payment channel: private key is neither PKCS#8 nor SEC1 ECDSA")
	}
	return key, nil
}

// ParseCertificates 读一组 PEM 证书(App Store 的根证书由运维从 Apple PKI 下载后配置,本服务不内置)。
func ParseCertificates(text string) ([]*x509.Certificate, error) {
	rest := []byte(strings.TrimSpace(text))
	var out []*x509.Certificate
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("payment channel: certificate PEM cannot be parsed")
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, errors.New("payment channel: no certificate in PEM")
	}
	return out, nil
}
