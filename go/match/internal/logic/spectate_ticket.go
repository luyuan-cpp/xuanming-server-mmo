package logic

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"match/internal/config"

	gatepb "proto/gate"

	"google.golang.org/protobuf/proto"
)

// 观战订阅凭证(battle-spectate-tiered-fanout.md §15.2,D98):L1 / L2 观众凭它在 gate 本地订阅,每个观众零条 Kafka
// 控制面消息。match 只签,gate 只验;客户端原样转交,不解析、不持久。凭证绑定玩家与 battle 实例,不能转让,
// 也不能拿去收其它实例的帧。

// spectateSource 是观战帧的来源:battle 实例与战斗期限。gate 只接受该实例发出的帧,过了期限 + 宽限就本地收尾。
type spectateSource struct {
	battleInstanceId string
	deadlineMs       uint64
}

// known 判定两项都已知。缺任何一项的凭证都没有用:gate 按实例过滤帧,实例为空的凭证收不到任何帧。
func (s spectateSource) known() bool {
	return s.battleInstanceId != "" && s.deadlineMs != 0
}

// issueSpectateTicket 按当前配置给一名观众签一张凭证,有效期从 nowMs 起算。secret 为空或来源未知一律报错(fail-closed):
// 那样的凭证 gate 必然拒绝,不如当场让调用方回失败。
func issueSpectateTicket(conf config.SpectateConf, battleId, playerId uint64, source spectateSource, nowMs uint64) ([]byte, error) {
	if !source.known() {
		return nil, fmt.Errorf("观战帧来源未知 battle=%d", battleId)
	}
	return signSpectateTicket(conf.TicketSecret, &gatepb.SpectateTicketPayload{
		BattleId:         battleId,
		PlayerId:         playerId,
		BattleInstanceId: source.battleInstanceId,
		DeadlineMs:       source.deadlineMs,
		ExpireAtMs:       nowMs + uint64(conf.TicketTTL().Milliseconds()),
	})
}

// signSpectateTicket 组 SpectateTicket 并序列化。signature = 小写 hex 的 HMAC-SHA256(secret, payload 字节),与 battle 直连票据、
// gate 登录令牌同口径。验签对象是 payload 的序列化字节本身,gate 验过再反序列化,所以两边不必对 pb 的编码达成一致。
func signSpectateTicket(secret string, payload *gatepb.SpectateTicketPayload) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("观战凭证密钥未配置")
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化观战凭证失败: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	ticket, err := proto.Marshal(&gatepb.SpectateTicket{Payload: raw, Signature: []byte(hex.EncodeToString(mac.Sum(nil)))})
	if err != nil {
		return nil, fmt.Errorf("序列化观战凭证失败: %w", err)
	}
	return ticket, nil
}
