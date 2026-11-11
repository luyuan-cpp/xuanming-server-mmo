package handler

import (
	"slices"
	"testing"

	base "proto/common/base"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 信封错误不进类型化 handler(docs/design/client-request-ordering.md O6):gate 级失败一律是带请求号的错误信封,
// body 为空。通用分发若照样交给生成的 handler,ListSkills 会被解成一份全零响应、把已拥有技能清空,压测 AI 从此不再施法。
// 同一消息号的成功回包作对照:它必须照常交给 handler,证明守卫只拦信封、不拦这个消息号。
func TestMessageBodyHandlerDropsEnvelopeErrorBeforeTypedHandler(t *testing.T) {
	// 取一个别的用例不会用的玩家号:PlayerList 是包级全局表。
	const playerID uint64 = 0x7e57_e1e0_0001
	player := gameobject.NewPlayer(playerID)
	player.SetOwnedSkillIDs([]uint32{101, 102})
	gameobject.PlayerList.Set(playerID, player)
	t.Cleanup(func() { gameobject.PlayerList.Delete(playerID) })
	client := &pkg.GameClient{PlayerId: playerID}

	before := EnvelopeErrorsDropped()
	MessageBodyHandler(client, &base.MessageContent{
		MessageId:    game.SceneSkillClientPlayerListSkillsMessageId,
		ErrorMessage: &base.TipInfoMessage{Id: uint32(tiptable.CommonError_kServiceUnavailable)},
	})
	if got := player.GetOwnedSkillIDs(); !slices.Equal(got, []uint32{101, 102}) {
		t.Fatalf("envelope error reached the typed ListSkills handler: owned skills = %v, want [101 102]", got)
	}
	if got := EnvelopeErrorsDropped(); got != before+1 {
		t.Fatalf("EnvelopeErrorsDropped = %d, want %d", got, before+1)
	}

	// 对照:同一消息号、没有信封错误的回包(空技能表)照常交给 handler,已拥有技能按回包写成空。
	MessageBodyHandler(client, &base.MessageContent{MessageId: game.SceneSkillClientPlayerListSkillsMessageId})
	if got := player.GetOwnedSkillIDs(); len(got) != 0 {
		t.Fatalf("successful reply did not reach the typed ListSkills handler: owned skills = %v, want empty", got)
	}
	if got := EnvelopeErrorsDropped(); got != before+1 {
		t.Fatalf("successful reply counted as envelope error: EnvelopeErrorsDropped = %d, want %d", got, before+1)
	}
}

// 推送与成功回包没有 error_message;error_message 存在但 id 为 0 也不算信封错误(与 pkg.GameClient 认领回包的判据一致),
// 照常派发。
func TestMessageBodyHandlerTreatsZeroTipAsNoEnvelopeError(t *testing.T) {
	const playerID uint64 = 0x7e57_e1e0_0002
	player := gameobject.NewPlayer(playerID)
	player.SetOwnedSkillIDs([]uint32{201})
	gameobject.PlayerList.Set(playerID, player)
	t.Cleanup(func() { gameobject.PlayerList.Delete(playerID) })

	before := EnvelopeErrorsDropped()
	MessageBodyHandler(&pkg.GameClient{PlayerId: playerID}, &base.MessageContent{
		MessageId:    game.SceneSkillClientPlayerListSkillsMessageId,
		ErrorMessage: &base.TipInfoMessage{},
	})
	if got := player.GetOwnedSkillIDs(); len(got) != 0 {
		t.Fatalf("message with tip 0 was not dispatched: owned skills = %v, want empty", got)
	}
	if got := EnvelopeErrorsDropped(); got != before {
		t.Fatalf("tip 0 counted as envelope error: EnvelopeErrorsDropped = %d, want %d", got, before)
	}
}
