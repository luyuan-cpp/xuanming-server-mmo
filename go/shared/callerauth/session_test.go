package callerauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCppSignatureCanonicalVector(t *testing.T) {
	m := hmac.New(sha256.New, []byte("0123456789abcdef0123456789abcdef"))
	m.Write(Canonical("gate", "/login.ClientPlayerLogin/Login", []byte{0, 1, 255, 10}, 1762300000000, "00112233445566778899aabbccddeeff"))
	if hex.EncodeToString(m.Sum(nil)) != "b06ac9d4ee1cdab3e9fef0f534b6ba8ba73cf73dd901d3d896f4329d6bfebd5e" {
		t.Fatal("C++/Go 内部签名契约不一致")
	}
}

func TestMissingGateSecretRefusesStartup(t *testing.T) {
	if _, err := GateInterceptor(func(string) string { return "" }); err == nil {
		t.Fatal("未注入内部密钥仍然允许启动")
	}
}
