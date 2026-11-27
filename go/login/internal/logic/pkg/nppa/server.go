package nppa

import (
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// 国家系统那一侧对请求做的验签与解密。生产路径用不到(国家系统是对端);用它的是沙箱 mock
// (cmd/sandbox_mock/nppa.go)和本包单测:两边按同一份实现核 login 发出的报文,签名或加密布局写坏了都会红。

// ErrBadSignature:请求头里的 sign 与按规范重算的不一致。
var ErrBadSignature = errors.New("nppa: signature mismatch")

// maxRequestBytes 远大于一批 128 条上下线加密后的大小,只为不让一条坏请求读个没完。
const maxRequestBytes = 1 << 20

// ReadRequest 按规范核一条请求并解开报文体,返回明文;GET 没有报文体,返回 nil。
// 签名覆盖系统参数(appId、bizId、timestamps)、URL 参数与报文体原文,见 sign。
// 明文里有姓名和身份证号:调用方不要记日志,本函数的错误文案也不带报文内容。
func ReadRequest(secretKey string, r *http.Request) ([]byte, error) {
	gcm, err := newGCM(secretKey)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		return nil, fmt.Errorf("nppa: read request body: %w", err)
	}
	params := map[string]string{
		"appId": r.Header.Get("appId"), "bizId": r.Header.Get("bizId"), "timestamps": r.Header.Get("timestamps"),
	}
	for key, values := range r.URL.Query() {
		params[key] = values[0]
	}
	if r.Header.Get("sign") != sign(secretKey, params, string(body)) {
		return nil, ErrBadSignature
	}
	if len(body) == 0 {
		return nil, nil
	}
	var envelope struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errors.New(`nppa: request body is not {"data": ...}`)
	}
	return decrypt(gcm, envelope.Data)
}

// decrypt 是 Client.encrypt 的逆:Base64 解码后前 12 字节是 nonce,其余是密文加认证标签。
func decrypt(gcm cipher.AEAD, data string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(raw) < gcm.NonceSize() {
		return nil, errors.New("nppa: request data is not Base64(nonce || ciphertext || tag)")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("nppa: request data does not decrypt with this secret key")
	}
	return plain, nil
}
