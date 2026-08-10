package callerauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

// testSecret 是一把只在测试内出现的密钥,同时实现 Signer 与 SecretVerifier。
type testSecret struct {
	keys [][]byte
}

func newTestSecret(keys ...string) *testSecret {
	s := &testSecret{}
	for _, k := range keys {
		s.keys = append(s.keys, []byte(k))
	}
	return s
}

func (s *testSecret) Sign(data []byte) []byte {
	if len(s.keys) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, s.keys[0])
	m.Write(data)
	return m.Sum(nil)
}

func (s *testSecret) Verify(mac, data []byte) bool {
	for _, k := range s.keys {
		m := hmac.New(sha256.New, k)
		m.Write(data)
		if hmac.Equal(mac, m.Sum(nil)) {
			return true
		}
	}
	return false
}

const (
	testMethod = "/login.ClientPlayerLogin/Disconnect"
	testCaller = "gate"
)

func mustSignedMD(t *testing.T, s Signer, caller, method string, subject []byte) metadata.MD {
	t.Helper()
	md, err := SignedMetadata(s, caller, method, subject)
	if err != nil {
		t.Fatalf("SignedMetadata 失败: %v", err)
	}
	return md
}

func TestVerify_签名正确时通过(t *testing.T) {
	secret := newTestSecret("unit-test-internal-auth-secret-0123456789")
	v := NewVerifier(Options{Secrets: secret, Enforce: true})

	subject := []byte("session-detail-bytes")
	md := mustSignedMD(t, secret, testCaller, testMethod, subject)

	caller, err := v.Verify(md, testMethod, subject)
	if err != nil {
		t.Fatalf("期望验签通过,得到: %v", err)
	}
	if caller != testCaller {
		t.Fatalf("caller 期望 %q,得到 %q", testCaller, caller)
	}
}

// 这是本次修复的核心断言:**没有签名的身份声明必须被拒**。
// 修复前 SessionInterceptor 根本不看签名,任何人递一段 SessionDetails 就被当成
// 可信身份 —— 把 Verify 换回"无条件返回 nil"这条用例立刻失败。
func TestVerify_没有签名一律拒绝(t *testing.T) {
	secret := newTestSecret("unit-test-internal-auth-secret-0123456789")
	v := NewVerifier(Options{Secrets: secret, Enforce: true})

	md := metadata.Pairs(MetaCaller, testCaller)
	if _, err := v.Verify(md, testMethod, []byte("whatever")); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("期望 ErrMissingSignature,得到: %v", err)
	}

	// 连调用方标识都没有的裸声明同样拒绝。
	if _, err := v.Verify(metadata.MD{}, testMethod, []byte("whatever")); !errors.Is(err, ErrMissingCaller) {
		t.Fatalf("期望 ErrMissingCaller,得到: %v", err)
	}
}

func TestVerify_密钥不对时拒绝(t *testing.T) {
	signer := newTestSecret("attacker-key-aaaaaaaaaaaaaaaaaaaaaaaaaaa")
	server := newTestSecret("unit-test-internal-auth-secret-0123456789")
	v := NewVerifier(Options{Secrets: server, Enforce: true})

	subject := []byte("session-detail-bytes")
	md := mustSignedMD(t, signer, testCaller, testMethod, subject)

	if _, err := v.Verify(md, testMethod, subject); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("期望 ErrBadSignature,得到: %v", err)
	}
}

// 签名必须绑死目标方法,否则攻击者可以抓一条 Disconnect 的合法签名
// 转手去调 EnterGame(跨方法重放)。
func TestVerify_换个方法调用会失败(t *testing.T) {
	secret := newTestSecret("unit-test-internal-auth-secret-0123456789")
	v := NewVerifier(Options{Secrets: secret, Enforce: true})

	subject := []byte("session-detail-bytes")
	md := mustSignedMD(t, secret, testCaller, testMethod, subject)

	if _, err := v.Verify(md, "/login.ClientPlayerLogin/EnterGame", subject); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("期望跨方法重放被 ErrBadSignature 拒绝,得到: %v", err)
	}
}

// 签名必须绑死请求主体:改一个字节的 SessionDetails 就该验不过,
// 否则攻击者能拿别人的签名套上自己的 player_id。
func TestVerify_篡改主体会失败(t *testing.T) {
	secret := newTestSecret("unit-test-internal-auth-secret-0123456789")
	v := NewVerifier(Options{Secrets: secret, Enforce: true})

	subject := []byte("session-detail-bytes")
	md := mustSignedMD(t, secret, testCaller, testMethod, subject)

	if _, err := v.Verify(md, testMethod, []byte("session-detail-byteS")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("期望篡改主体被拒,得到: %v", err)
	}
}

func TestVerify_时间戳超窗拒绝(t *testing.T) {
	secret := newTestSecret("unit-test-internal-auth-secret-0123456789")
	base := time.Now()
	v := NewVerifier(Options{
		Secrets:      secret,
		Enforce:      true,
		MaxClockSkew: 5 * time.Second,
		Now:          func() time.Time { return base },
	})

	subject := []byte("session-detail-bytes")

	// 过旧
	old := signAt(t, secret, testCaller, testMethod, subject, base.Add(-time.Minute).UnixMilli())
	if _, err := v.Verify(old, testMethod, subject); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("期望过旧时间戳被 ErrClockSkew 拒绝,得到: %v", err)
	}

	// 过新(时钟往前拨也不许,否则等于把重放窗口无限延长)
	future := signAt(t, secret, testCaller, testMethod, subject, base.Add(time.Minute).UnixMilli())
	if _, err := v.Verify(future, testMethod, subject); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("期望未来时间戳被 ErrClockSkew 拒绝,得到: %v", err)
	}
}

func TestVerify_同一条签名不能重放(t *testing.T) {
	secret := newTestSecret("unit-test-internal-auth-secret-0123456789")
	v := NewVerifier(Options{Secrets: secret, Enforce: true})

	subject := []byte("session-detail-bytes")
	md := mustSignedMD(t, secret, testCaller, testMethod, subject)

	if _, err := v.Verify(md, testMethod, subject); err != nil {
		t.Fatalf("首次验签应当通过,得到: %v", err)
	}
	if _, err := v.Verify(md, testMethod, subject); !errors.Is(err, ErrReplay) {
		t.Fatalf("期望第二次被 ErrReplay 拒绝,得到: %v", err)
	}
}

// 验签失败的请求**不能**污染 nonce 表 —— 否则攻击者只要猜到/抢先用掉
// 某个 nonce,就能让随后那条合法请求被误判成重放,等于自带拒绝服务。
func TestVerify_验签失败不占用nonce(t *testing.T) {
	real := newTestSecret("unit-test-internal-auth-secret-0123456789")
	fake := newTestSecret("attacker-key-aaaaaaaaaaaaaaaaaaaaaaaaaaa")
	v := NewVerifier(Options{Secrets: real, Enforce: true})

	subject := []byte("session-detail-bytes")
	nonce := "0123456789abcdef0123456789abcdef"
	ts := time.Now().UnixMilli()

	// 攻击者先用同一个 nonce 递一条签名错误的请求。
	bad := buildMD(testCaller, ts, nonce, hexSig(fake, testCaller, testMethod, subject, ts, nonce), subject)
	if _, err := v.Verify(bad, testMethod, subject); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("期望 ErrBadSignature,得到: %v", err)
	}

	// 合法调用方随后用同一个 nonce 应当仍然通过。
	good := buildMD(testCaller, ts, nonce, hexSig(real, testCaller, testMethod, subject, ts, nonce), subject)
	if _, err := v.Verify(good, testMethod, subject); err != nil {
		t.Fatalf("合法请求不应被未认证流量顶掉 nonce,得到: %v", err)
	}
}

func TestVerify_白名单外的调用方被拒(t *testing.T) {
	secret := newTestSecret("unit-test-internal-auth-secret-0123456789")
	v := NewVerifier(Options{Secrets: secret, Enforce: true, AllowedCallers: []string{"gateway"}})

	subject := []byte("session-detail-bytes")
	md := mustSignedMD(t, secret, testCaller, testMethod, subject)

	if _, err := v.Verify(md, testMethod, subject); !errors.Is(err, ErrCallerNotAllowed) {
		t.Fatalf("期望 ErrCallerNotAllowed,得到: %v", err)
	}
}

// 轮换期:上游已经切到新密钥、login 还把旧密钥留在候选里,两边都得验得过。
func TestVerify_轮换期新旧密钥都认(t *testing.T) {
	oldKey := "rotation-old-key-000000000000000000000000"
	newKey := "rotation-new-key-111111111111111111111111"
	server := newTestSecret(newKey, oldKey) // 主密钥=新,附加=旧

	subject := []byte("session-detail-bytes")
	for name, signer := range map[string]*testSecret{
		"上游还在用旧密钥": newTestSecret(oldKey),
		"上游已切新密钥":  newTestSecret(newKey),
	} {
		v := NewVerifier(Options{Secrets: server, Enforce: true})
		md := mustSignedMD(t, signer, testCaller, testMethod, subject)
		if _, err := v.Verify(md, testMethod, subject); err != nil {
			t.Fatalf("%s:期望验签通过,得到 %v", name, err)
		}
	}
}

func TestVerify_密钥没配时一律拒绝(t *testing.T) {
	// Secrets 为 nil = 密钥没配好。必须 fail-closed,绝不能当成"无需校验"。
	v := NewVerifier(Options{Secrets: nil, Enforce: true})
	signer := newTestSecret("unit-test-internal-auth-secret-0123456789")
	subject := []byte("session-detail-bytes")
	md := mustSignedMD(t, signer, testCaller, testMethod, subject)

	if _, err := v.Verify(md, testMethod, subject); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("期望密钥缺失时 fail-closed,得到: %v", err)
	}
}

func TestNonceSet_两代轮转保留至少一个窗口(t *testing.T) {
	base := time.Now()
	n := newNonceSet(time.Second, 1000, nil)

	if !n.admit("a", base) {
		t.Fatal("首次 admit 应当成功")
	}
	// 半个窗口后仍然记得
	if n.admit("a", base.Add(500*time.Millisecond)) {
		t.Fatal("窗口内应当判定为重放")
	}
	// 一个窗口后翻代:a 掉进 prev,仍然记得
	if n.admit("a", base.Add(1500*time.Millisecond)) {
		t.Fatal("翻一代后应当仍然判定为重放")
	}
	// 两个窗口后 a 被淘汰(这没问题:那个时间戳早就超出 clock skew 了)
	if !n.admit("a", base.Add(4*time.Second)) {
		t.Fatal("超过两个窗口后应当允许重新使用")
	}
}

func TestNonceSet_超上限强制翻代不放行错误签名(t *testing.T) {
	overflowed := 0
	n := newNonceSet(time.Hour, 2, func() { overflowed++ })
	now := time.Now()
	for _, s := range []string{"a", "b", "c", "d"} {
		if !n.admit(s, now) {
			t.Fatalf("nonce %s 首次 admit 不应失败", s)
		}
	}
	if overflowed == 0 {
		t.Fatal("超过上限时应当触发 onOverflow 回调")
	}
	if n.size() > 4 {
		t.Fatalf("翻代后条数应当被压下来,当前 %d", n.size())
	}
}

// ── 测试辅助 ──────────────────────────────────────────────────────────

func hexSig(s Signer, caller, method string, subject []byte, ts int64, nonce string) string {
	return hex.EncodeToString(s.Sign(Canonical(caller, method, subject, ts, nonce)))
}

func buildMD(caller string, ts int64, nonce, sig string, subject []byte) metadata.MD {
	return metadata.Pairs(
		MetaSessionDetail, encodeSubject(subject),
		MetaCaller, caller,
		MetaTimestamp, formatMillis(ts),
		MetaNonce, nonce,
		MetaSignature, sig,
	)
}

func signAt(t *testing.T, s Signer, caller, method string, subject []byte, ts int64) metadata.MD {
	t.Helper()
	nonce, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce 失败: %v", err)
	}
	return buildMD(caller, ts, nonce, hexSig(s, caller, method, subject, ts, nonce), subject)
}
