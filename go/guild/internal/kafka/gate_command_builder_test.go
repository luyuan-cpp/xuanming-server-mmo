package kafka

// gate 推送命令构造的单测:只拆包断言,不连 Kafka。
// friend / match 的同名实现与本文件被测的实现逐字相同(三份合并另做);共享编码器本身的
// 跨语言对拍在 go/shared/kafkautil/gate_push_test.go,这里盯的是 builder 把四个目标字段接对了。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"guild/generated/pb/game"
	kafkapb "proto/contracts/kafka"
	"shared/kafkautil"
)

const (
	testGateNodeID     = uint32(5)
	testGateInstanceID = "gate-5-uuid"
)

// TestBuildPushCommandCarriesTargetPlayer:单播的 player_id 必须进 payload(gate 的身份栅栏只读它),
// 信封上的 PlayerId / SessionId 同步照填。
func TestBuildPushCommandCarriesTargetPlayer(t *testing.T) {
	const (
		sessionID = uint32(655361)
		playerID  = uint64(90001)
	)
	raw, err := NewGateCommandBuilder().BuildPushCommand(testGateNodeID, sessionID, playerID, testGateInstanceID,
		game.GuildServiceNotifyGuildChangedMessageId, []byte("body"))
	require.NoError(t, err)

	var cmd kafkapb.GateCommand
	require.NoError(t, proto.Unmarshal(raw, &cmd))
	assert.Equal(t, uint32(game.ContractsKafkaPushToPlayerEventEventId), cmd.GetEventId())
	assert.Equal(t, testGateNodeID, cmd.GetTargetGateId())
	assert.Equal(t, testGateInstanceID, cmd.GetTargetInstanceId())
	assert.Equal(t, playerID, cmd.GetPlayerId())
	assert.Equal(t, sessionID, cmd.GetSessionId())

	var event kafkapb.PushToPlayerEvent
	require.NoError(t, proto.Unmarshal(cmd.GetPayload(), &event))
	assert.Equal(t, sessionID, event.GetSessionId())
	assert.Equal(t, playerID, event.GetPlayerId(), "gate 的身份栅栏只读 payload 里的 player_id")
	assert.Equal(t, uint32(game.GuildServiceNotifyGuildChangedMessageId), event.GetMessageContent().GetMessageId())
	assert.Equal(t, []byte("body"), event.GetMessageContent().GetSerializedMessage())
}

// TestBuildBroadcastCommandAlignsPlayerListWithBitmapOrder:64 个密集目标逆序给出,强制 bitmap 形态;
// 按 gate 的解码顺序(字节序、字节内低位在前)逐个置位取 player_list 的下一项,每个 session 拿到的
// 都必须是它自己的玩家。player_id 取 session 序号的一个置换(不单调),独立排序 player_list 的实现会被发现。
func TestBuildBroadcastCommandAlignsPlayerListWithBitmapOrder(t *testing.T) {
	const (
		sessionBase = uint32(5<<17) + 1 // gate node 5 的会话段
		targetCount = 64
	)
	want := make(map[uint32]uint64, targetCount)
	targets := make([]kafkautil.BroadcastTarget, 0, targetCount)
	for i := targetCount - 1; i >= 0; i-- {
		session := sessionBase + uint32(i)
		player := 9_000_000 + uint64(i*37%targetCount) // 37 与 64 互质,i*37%64 是 0..63 的一个置换
		targets = append(targets, kafkautil.BroadcastTarget{SessionID: session, PlayerID: player})
		want[session] = player
	}

	raw, err := NewGateCommandBuilder().BuildBroadcastCommand(testGateNodeID, targets, testGateInstanceID,
		game.GuildServiceNotifyGuildChangedMessageId, []byte("body"))
	require.NoError(t, err)

	var cmd kafkapb.GateCommand
	require.NoError(t, proto.Unmarshal(raw, &cmd))
	assert.Equal(t, uint32(game.ContractsKafkaBroadcastToPlayersEventEventId), cmd.GetEventId())
	assert.Equal(t, testGateNodeID, cmd.GetTargetGateId())
	assert.Equal(t, testGateInstanceID, cmd.GetTargetInstanceId())

	var event kafkapb.BroadcastToPlayersEvent
	require.NoError(t, proto.Unmarshal(cmd.GetPayload(), &event))
	require.NotEmpty(t, event.GetSessionBitmap(), "64 个密集目标必须走 bitmap 形态")
	assert.Empty(t, event.GetSessionList(), "bitmap 与 session_list 二选一")
	assert.Equal(t, sessionBase, event.GetSessionBitmapBase())
	require.Len(t, event.GetPlayerList(), targetCount, "player_list 必须与目标数等长,否则 gate 整条拒绝")

	index := 0
	for byteIndex, b := range event.GetSessionBitmap() {
		for bit := 0; bit < 8; bit++ {
			if b&(1<<bit) == 0 {
				continue
			}
			session := event.GetSessionBitmapBase() + uint32(byteIndex*8+bit)
			require.Less(t, index, len(event.GetPlayerList()), "bitmap 置位数多于 player_list")
			assert.Equal(t, want[session], event.GetPlayerList()[index], "session %d 对到了别人的 player_id", session)
			index++
		}
	}
	assert.Equal(t, targetCount, index, "bitmap 置位数必须等于目标数")
	assert.Equal(t, uint32(game.GuildServiceNotifyGuildChangedMessageId), event.GetMessageContent().GetMessageId())
}

// TestBuildBroadcastCommandListFormKeepsAlignment:目标稀少时走 session_list,player_list 按同一升序逐项对应。
func TestBuildBroadcastCommandListFormKeepsAlignment(t *testing.T) {
	targets := []kafkautil.BroadcastTarget{
		{SessionID: 655400, PlayerID: 1001},
		{SessionID: 655362, PlayerID: 3003},
		{SessionID: 655380, PlayerID: 2002},
	}
	raw, err := NewGateCommandBuilder().BuildBroadcastCommand(testGateNodeID, targets, testGateInstanceID,
		game.GuildServiceNotifyGuildChangedMessageId, []byte("body"))
	require.NoError(t, err)

	var cmd kafkapb.GateCommand
	require.NoError(t, proto.Unmarshal(raw, &cmd))
	var event kafkapb.BroadcastToPlayersEvent
	require.NoError(t, proto.Unmarshal(cmd.GetPayload(), &event))
	assert.Empty(t, event.GetSessionBitmap(), "3 个目标不到 bitmap 阈值")
	assert.Equal(t, []uint32{655362, 655380, 655400}, event.GetSessionList())
	assert.Equal(t, []uint64{3003, 2002, 1001}, event.GetPlayerList())
}
