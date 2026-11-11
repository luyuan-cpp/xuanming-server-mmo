package callerauth

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// 跨语言契约:C++ gate(caller_auth.h)拼出的 canonical 必须与这里逐字节相同。
// cpp/tests/routing_identity_test/session_details_builder_test.cpp 断言的是同一条字面量。
func TestCanonicalV2Vector(t *testing.T) {
	got := string(Canonical("gate", "/login.ClientPlayerLogin/Login", []byte{0, 1, 255, 10}, 1762300000000,
		"00112233445566778899aabbccddeeff"))
	want := "mmorpg-gate-identity/v2\n" +
		"gate\n" +
		"/login.ClientPlayerLogin/Login\n" +
		"a95803d91f2a2eb57d42e2a799bb0d42e2acc37c22aa9d433a4173e5db794f28\n" +
		"1762300000000\n" +
		"00112233445566778899aabbccddeeff"
	if got != want {
		t.Fatalf("C++/Go 内部签名契约不一致:\n got %q\nwant %q", got, want)
	}
}

// RFC 8032 §7.1 TEST 1:钉住「种子 → 公钥」的派生与签名原语。C++ 侧用同一组向量;
// 本机开发用的密钥对(tools/scripts/lib/gate_identity_environment.ps1)也取这一组,这条用例同时证明那一对是配对的。
const (
	rfc8032Test1Seed      = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	rfc8032Test1PublicKey = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	rfc8032Test1EmptySig  = "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"
)

func TestEd25519Rfc8032Vector(t *testing.T) {
	seed, err := ParseSeedHex(rfc8032Test1Seed)
	if err != nil {
		t.Fatalf("解析 RFC 种子失败: %v", err)
	}
	signer, err := NewEd25519Signer(seed)
	if err != nil {
		t.Fatalf("构造签名器失败: %v", err)
	}
	if got := hex.EncodeToString(signer.PublicKey()); got != rfc8032Test1PublicKey {
		t.Fatalf("种子派生的公钥与 RFC 8032 不符: %s", got)
	}
	sig := signer.Sign(nil)
	if got := hex.EncodeToString(sig); got != rfc8032Test1EmptySig {
		t.Fatalf("签名与 RFC 8032 不符: %s", got)
	}
	keys, err := ParsePublicKeys(rfc8032Test1PublicKey)
	if err != nil {
		t.Fatalf("解析 RFC 公钥失败: %v", err)
	}
	if !keys.Verify(sig, nil) {
		t.Fatal("RFC 8032 的签名没有通过验签")
	}
	if keys.Verify(sig, []byte{0}) {
		t.Fatal("换了正文的签名不应通过验签")
	}
}

func TestMissingPublicKeysRefusesStartup(t *testing.T) {
	if _, err := GateInterceptor(func(string) string { return "" }); err == nil {
		t.Fatal("未注入验签公钥仍然允许启动")
	}
}

func TestMalformedPublicKeysRefuseStartup(t *testing.T) {
	cases := map[string]string{
		"短了一个字符":      rfc8032Test1PublicKey[:63],
		"非十六进制":       strings.Repeat("zz", 32),
		"首部空白":        " " + rfc8032Test1PublicKey[1:],
		"空项":          rfc8032Test1PublicKey + ",",
		"重复":          rfc8032Test1PublicKey + "," + rfc8032Test1PublicKey,
		"误填成 64 字节私钥": rfc8032Test1Seed + rfc8032Test1PublicKey,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := GateInterceptor(func(key string) string {
				if key == PublicKeysEnv {
					return value
				}
				return ""
			}); err == nil {
				t.Fatalf("格式不对的公钥列表 %q 仍然允许启动", value)
			}
		})
	}
}

func TestPublicKeysRejectWrongLengthSignature(t *testing.T) {
	signer, err := NewEd25519Signer(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	keys := PublicKeys{signer.PublicKey()}
	data := []byte("canonical")
	sig := signer.Sign(data)
	if !keys.Verify(sig, data) {
		t.Fatal("正确签名应当通过")
	}
	// 32 字节是旧 HMAC-SHA256 的长度:v1 的签名在 v2 下一律验不过。
	if keys.Verify(sig[:32], data) || keys.Verify(append(sig, 0), data) {
		t.Fatal("长度不对的签名不应通过")
	}
}

func TestZeroSignerRefusesToSign(t *testing.T) {
	if _, err := SignCanonical(Ed25519Signer{}, "gate", "/a.B/C", nil, 1, "00112233445566778899aabbccddeeff"); err == nil {
		t.Fatal("没有私钥的签名器不应签出东西")
	}
}

// BenchmarkPublicKeysVerify 量验证方每个带会话请求多出的验签 CPU。2026-11-11 由 HMAC-SHA256 换成 Ed25519
// (gate-online-identity.md §10.1),这条基准给 Codex 出实测数,不预设结论。
func BenchmarkPublicKeysVerify(b *testing.B) {
	signer, err := NewEd25519Signer(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		b.Fatal(err)
	}
	keys := PublicKeys{signer.PublicKey()}
	data := Canonical("gate", "/guild.GuildService/GetGuild", bytes.Repeat([]byte{9}, 96), 1762300000000,
		"00112233445566778899aabbccddeeff")
	sig := signer.Sign(data)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !keys.Verify(sig, data) {
			b.Fatal("验签失败")
		}
	}
}
