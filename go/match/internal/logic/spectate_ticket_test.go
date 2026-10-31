package logic

// 观战凭证(spectate_ticket.go)的纯函数测试:签名可被独立 HMAC 校验(gate 用 C++ 校验,这里不复用签发代码,防止
// 两边一起错)、字段完整、有效期从注入的 now 起算、缺密钥或缺来源 fail-closed。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"match/internal/config"

	gatepb "proto/gate"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const testSpectateTicketSecret = "spectate-ticket-secret-for-unit-tests-000"

// verifiedTicketPayload 按 gate 的口径独立验签并解出 payload:signature = 小写 hex 的 HMAC-SHA256(secret, payload 字节)。
func verifiedTicketPayload(t *testing.T, raw []byte, secret string) *gatepb.SpectateTicketPayload {
	t.Helper()
	require.NotEmpty(t, raw, "LOBBY 回包必须带观战凭证")
	ticket := &gatepb.SpectateTicket{}
	require.NoError(t, proto.Unmarshal(raw, ticket))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(ticket.GetPayload())
	require.Equal(t, hex.EncodeToString(mac.Sum(nil)), string(ticket.GetSignature()), "签名必须是小写 hex 的 HMAC-SHA256(secret, payload 字节)")
	payload := &gatepb.SpectateTicketPayload{}
	require.NoError(t, proto.Unmarshal(ticket.GetPayload(), payload))
	return payload
}

func TestIssueSpectateTicketVerifiesWithIndependentHmac(t *testing.T) {
	conf := config.SpectateConf{TicketSecret: testSpectateTicketSecret, TicketTTLSeconds: 90}
	source := spectateSource{battleInstanceId: "battle-instance", deadlineMs: 5_000_000}

	raw, err := issueSpectateTicket(conf, 880001, 7001, source, 1_000_000)
	require.NoError(t, err)

	payload := verifiedTicketPayload(t, raw, testSpectateTicketSecret)
	require.Equal(t, uint64(880001), payload.GetBattleId())
	require.Equal(t, uint64(7001), payload.GetPlayerId())
	require.Equal(t, "battle-instance", payload.GetBattleInstanceId())
	require.Equal(t, uint64(5_000_000), payload.GetDeadlineMs())
	require.Equal(t, uint64(1_000_000+90_000), payload.GetExpireAtMs(), "过期时刻 = now + TTL")
}

// 签名绑定 payload 的每个字节,换密钥或改 payload 都验不过(gate 据此拒绝换 player / 换 battle / 延期的凭证)。
func TestSpectateTicketSignatureBindsPayloadAndSecret(t *testing.T) {
	conf := config.SpectateConf{TicketSecret: testSpectateTicketSecret}
	raw, err := issueSpectateTicket(conf, 880002, 7002, spectateSource{battleInstanceId: "i", deadlineMs: 1}, 1000)
	require.NoError(t, err)
	ticket := &gatepb.SpectateTicket{}
	require.NoError(t, proto.Unmarshal(raw, ticket))

	other := hmac.New(sha256.New, []byte(testSpectateTicketSecret+"x"))
	other.Write(ticket.GetPayload())
	require.NotEqual(t, hex.EncodeToString(other.Sum(nil)), string(ticket.GetSignature()), "换密钥验不过")

	tampered := hmac.New(sha256.New, []byte(testSpectateTicketSecret))
	tampered.Write(append(append([]byte(nil), ticket.GetPayload()...), 0x00))
	require.NotEqual(t, hex.EncodeToString(tampered.Sum(nil)), string(ticket.GetSignature()), "改 payload 验不过")
}

// 跨语言固定向量:C++ gate_spectate_ticket_test.cpp 用同一组常量验签,两边的算法与编码只要有一边变了就有一边红。
// 这些值是手工算好写死的,不要用被测代码重新生成。
const (
	spectateTicketVectorSecret = "spectate-ticket-cross-language-vector-secret-01"
	// SpectateTicketPayload{battle_id=880001, player_id=7001, battle_instance_id="5b0f3c1e-7a42-4d0e-9a51-2f6c8d9e0b17",
	// deadline_ms=4102444800000, expire_at_ms=1700000060000} 的序列化字节。
	spectateTicketVectorPayloadHex = "0881db3510d9361a2435623066336331652d376134322d346430652d396135312d3266366338643965306231372080b08fe6b27728e0a499ffbc31"
	// 小写 hex 的 HMAC-SHA256(secret, payload 字节)。
	spectateTicketVectorSignature = "d06fe173dcaeaf5662cfd16c6976d0e2329b52d04db87f109b7a315c5261938d"
)

func TestSpectateTicketCrossLanguageVector(t *testing.T) {
	wantPayload, err := hex.DecodeString(spectateTicketVectorPayloadHex)
	require.NoError(t, err)

	raw, err := signSpectateTicket(spectateTicketVectorSecret, &gatepb.SpectateTicketPayload{
		BattleId:         880001,
		PlayerId:         7001,
		BattleInstanceId: "5b0f3c1e-7a42-4d0e-9a51-2f6c8d9e0b17",
		DeadlineMs:       4102444800000,
		ExpireAtMs:       1700000060000,
	})
	require.NoError(t, err)
	ticket := &gatepb.SpectateTicket{}
	require.NoError(t, proto.Unmarshal(raw, ticket))
	require.Equal(t, wantPayload, ticket.GetPayload(), "payload 的序列化字节必须与向量一致")
	require.Equal(t, spectateTicketVectorSignature, string(ticket.GetSignature()), "签名必须与向量一致")
}

func TestIssueSpectateTicketDefaultsTTL(t *testing.T) {
	raw, err := issueSpectateTicket(config.SpectateConf{TicketSecret: testSpectateTicketSecret}, 880003, 7003,
		spectateSource{battleInstanceId: "i", deadlineMs: 1}, 1000)
	require.NoError(t, err)
	require.Equal(t, uint64(1000+60_000), verifiedTicketPayload(t, raw, testSpectateTicketSecret).GetExpireAtMs(), "漏配 TTL 按默认 60 秒")
}

// 缺密钥或缺来源的凭证 gate 必然拒绝,当场报错,不发出去。
func TestIssueSpectateTicketFailsClosed(t *testing.T) {
	known := spectateSource{battleInstanceId: "i", deadlineMs: 1}
	_, err := issueSpectateTicket(config.SpectateConf{}, 880004, 7004, known, 1000)
	require.Error(t, err, "没有密钥")
	conf := config.SpectateConf{TicketSecret: testSpectateTicketSecret}
	_, err = issueSpectateTicket(conf, 880004, 7004, spectateSource{deadlineMs: 1}, 1000)
	require.Error(t, err, "没有实例")
	_, err = issueSpectateTicket(conf, 880004, 7004, spectateSource{battleInstanceId: "i"}, 1000)
	require.Error(t, err, "没有期限")
}

// 三版共用的最小向量:payload 字节按 protobuf 线上格式手写,期望值由 Python hmac 独立算出。
// C++ `gate_spectate_ticket_test.cpp` 的 CrossLanguageVectorVerifies 与 Java `SpectateTicketsTest` 用同一组常量。
func TestSpectateTicketSharedVectorAcrossAllVersions(t *testing.T) {
	const (
		secret     = "spectate-cross-language-vector-secret-0123456789"
		payloadHex = "080110021a016920032804"
		signature  = "81e49d087e684b9df52c0c7bde47b88afce761af48f37b8dda6bdc3f01c2b0d8"
	)
	raw, err := signSpectateTicket(secret, &gatepb.SpectateTicketPayload{
		BattleId:         1,
		PlayerId:         2,
		BattleInstanceId: "i",
		DeadlineMs:       3,
		ExpireAtMs:       4,
	})
	require.NoError(t, err)
	ticket := &gatepb.SpectateTicket{}
	require.NoError(t, proto.Unmarshal(raw, ticket))
	require.Equal(t, payloadHex, hex.EncodeToString(ticket.GetPayload()), "payload 字节必须与手写的线上格式一致")
	require.Equal(t, signature, string(ticket.GetSignature()), "签名必须与三版共用的向量一致")
}
