package assetop

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	assetpb "proto/common/asset"

	"shared/scenenode"
)

// canonicalGolden 与 C++ AssetOpAuthTest.CanonicalGolden 用**同一组输入**:
// player 42、stream 1、epoch 1700000000000、seq 7、corr 99、tx 24、货币 [(1,30)]、
// 无物品、rpc debit、caller guild、ts 1700000000123。
// 两边任何一侧改了拼串规则,这个字面量就会把它抓出来 —— 否则线上表现是「全部验签失败」。
const canonicalGolden = "mmorpg-asset-op/v1\nguild\ndebit\n42\n1\n1700000000000\n7\n99\n24\nc=1:30;i=;u=;p=0\n1700000000123"

func TestCanonicalGolden(t *testing.T) {
	req := testRequest()
	req.Auth = &assetpb.AssetOpAuth{Caller: "guild", TimestampMs: 1700000000123}

	got := string(Canonical(RPCDebit, req, 1700000000123))
	if got != canonicalGolden {
		t.Fatalf("canonical 串不匹配\n实际: %q\n期望: %q", got, canonicalGolden)
	}
}

// 空 bundle 的写法也是契约的一部分,单独钉住。
func TestCanonicalEmptyBundle(t *testing.T) {
	req := &assetpb.AssetOpRequest{
		PlayerId:    7,
		Stream:      assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		Seq:         1,
		StreamEpoch: 2,
		Auth:        &assetpb.AssetOpAuth{Caller: "guild"},
	}
	got := string(Canonical(RPCAbort, req, 5))
	want := "mmorpg-asset-op/v1\nguild\nabort_debit\n7\n1\n2\n1\n0\n0\nc=;i=;u=;p=0\n5"
	if got != want {
		t.Fatalf("空 bundle 的 canonical 串不匹配\n实际: %q\n期望: %q", got, want)
	}
}

// 按 guid 扣的物品与宝宝必须进签名串。这两个字段会真改玩家资产,而 scene 的 gRPC
// 是不安全凭据:不进串就等于「集群内任何进程都能改这条请求扣哪件装备」。
// 与 C++ AssetOpAuthTest.CanonicalCoversGuidAndPet 同一组输入。
func TestCanonicalCoversGuidAndPet(t *testing.T) {
	req := &assetpb.AssetOpRequest{
		PlayerId:    7,
		Stream:      assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT,
		Seq:         1,
		StreamEpoch: 2,
		TxType:      24,
		Bundle: &assetpb.AssetBundle{
			ItemUuids: []uint64{900000000000000001, 900000000000000002},
			PetId:     700000000000000003,
		},
		Auth: &assetpb.AssetOpAuth{Caller: "trade"},
	}
	got := string(Canonical(RPCDebit, req, 5))
	want := "mmorpg-asset-op/v1\ntrade\ndebit\n7\n3\n2\n1\n0\n24\n" +
		"c=;i=;u=900000000000000001,900000000000000002;p=700000000000000003\n5"
	if got != want {
		t.Fatalf("guid/pet 段的 canonical 串不匹配\n实际: %q\n期望: %q", got, want)
	}
}

// 这条才是上面那条的**意义**所在:改掉 item_uuids / pet_id 之后签名必须失效。
// 不加这条,将来有人把 u/p 两段从拼串里删掉,只有 golden 用例会红,
// 而 golden 看起来只是「字面量对不上」,很容易被顺手改字面量「修好」。
func TestSignatureCoversGuidAndPetTamper(t *testing.T) {
	signer, err := NewSigner("trade", testSecret)
	if err != nil {
		t.Fatalf("构造 Signer 失败: %v", err)
	}
	base := func() *assetpb.AssetOpRequest {
		return &assetpb.AssetOpRequest{
			PlayerId:    7,
			Stream:      assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT,
			Seq:         1,
			StreamEpoch: 2,
			TxType:      24,
			Bundle: &assetpb.AssetBundle{
				ItemUuids: []uint64{900000000000000001},
				PetId:     700000000000000003,
			},
		}
	}

	signed := base()
	signer.Sign(RPCDebit, signed, 1700000000123)
	original := signed.GetAuth().GetSignatureHex()
	if original == "" {
		t.Fatal("签名为空")
	}

	// 攻击者能做的正是这一步:拿到合法请求,只换载荷,签名原样带上。
	for _, c := range []struct {
		name  string
		mutate func(*assetpb.AssetOpRequest)
	}{
		{"换掉 item_uuid", func(r *assetpb.AssetOpRequest) { r.Bundle.ItemUuids = []uint64{900000000000000099} }},
		{"多加一个 item_uuid", func(r *assetpb.AssetOpRequest) {
			r.Bundle.ItemUuids = append(r.Bundle.ItemUuids, 900000000000000099)
		}},
		{"清空 item_uuids", func(r *assetpb.AssetOpRequest) { r.Bundle.ItemUuids = nil }},
		{"换掉 pet_id", func(r *assetpb.AssetOpRequest) { r.Bundle.PetId = 700000000000000099 }},
		{"清空 pet_id", func(r *assetpb.AssetOpRequest) { r.Bundle.PetId = 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			tampered := base()
			c.mutate(tampered)
			tampered.Auth = &assetpb.AssetOpAuth{Caller: "trade", TimestampMs: 1700000000123}

			mac := hmac.New(sha256.New, []byte(testSecret))
			mac.Write(Canonical(RPCDebit, tampered, 1700000000123))
			if hex.EncodeToString(mac.Sum(nil)) == original {
				t.Fatal("篡改载荷后签名不变 —— u/p 两段没有进 canonical 串")
			}
		})
	}
}

func TestNewSignerRejectsWeak(t *testing.T) {
	cases := []struct {
		name   string
		caller string
		secret string
	}{
		{"空密钥", "guild", ""},
		{"全空白", "guild", strings.Repeat(" ", 64)},
		{"去空白后不足 32 字节", "guild", "  short-secret-0000000000  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewSigner(c.caller, c.secret); !errors.Is(err, ErrWeakSecret) {
				t.Fatalf("应当报 ErrWeakSecret,实际: %v", err)
			}
		})
	}
	if _, err := NewSigner("  ", testSecret); !errors.Is(err, ErrEmptyCaller) {
		t.Fatalf("空调用方名应当报 ErrEmptyCaller,实际: %v", err)
	}
}

// 每一次实际发包(含 durable 重查)都要用**当时**的时间重签:
// 复用旧签名会在慢路径上撞上 scene 的 300s 时间窗。
func TestCallerSignsEachAttempt(t *testing.T) {
	scene, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, n int) (*assetpb.AssetOpResponse, error) {
		return applied(n >= 2), nil
	})

	// 注入时钟:每次取时间都往前走 1 秒,于是两次发包的时间戳必定不同。
	// 这里不依赖 invoke 内部调了几次 now(),免得实现一改测试就假绿。
	var ticks int
	base := time.UnixMilli(1700000000123)
	caller := newTestCaller(t, &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}},
		[]time.Duration{10 * time.Millisecond})
	caller.Now = func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * time.Second)
	}

	if _, err := caller.Do(context.Background(), RPCDebit, testRequest()); err != nil {
		t.Fatalf("Do 出错: %v", err)
	}

	calls := scene.recorded()
	if len(calls) != 2 {
		t.Fatalf("应当发 2 个包,实际 %d", len(calls))
	}
	first, second := calls[0].Req.GetAuth(), calls[1].Req.GetAuth()
	if first.GetTimestampMs() == second.GetTimestampMs() {
		t.Fatalf("两次发包的时间戳相同(%d):重查没有重签", first.GetTimestampMs())
	}
	if first.GetSignatureHex() == second.GetSignatureHex() {
		t.Fatal("两次发包的签名相同:重查没有重签")
	}
	for i, call := range calls {
		if call.Req.GetAuth().GetCaller() != "guild" {
			t.Fatalf("第 %d 个包的 caller 不对: %q", i+1, call.Req.GetAuth().GetCaller())
		}
		if !verifySignature(call.RPC, call.Req, testSecret) {
			t.Fatalf("第 %d 个包验签失败", i+1)
		}
	}
}

func TestCallerNoSigner(t *testing.T) {
	_, client := startFakeScene(t, func(_ RPC, _ *assetpb.AssetOpRequest, _ int) (*assetpb.AssetOpResponse, error) {
		return applied(true), nil
	})
	caller := &Caller{Resolver: &fakeResolver{targets: []scenenode.Target{targetOf("scene-a:9000", client)}}}

	if _, err := caller.Do(context.Background(), RPCDebit, testRequest()); !errors.Is(err, ErrNoSigner) {
		t.Fatalf("应当报 ErrNoSigner,实际: %v", err)
	}
}

// verifySignature 是 scene 侧验签的 Go 复刻,只用于测试:
// 它证明「我们签出来的东西,按同一份规则能验回去」。
func verifySignature(rpc RPC, req *assetpb.AssetOpRequest, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(Canonical(rpc, req, req.GetAuth().GetTimestampMs()))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(req.GetAuth().GetSignatureHex()))
}
