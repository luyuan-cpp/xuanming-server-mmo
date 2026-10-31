package logic

// 观战可见性策略(spectate_visibility.go)的纯函数测试:建房时的单一决策表、表外值 fail-closed、
// 全局成局原件的可见性读取。执法点的行为另见:登记与公开 gather_spectate_index_test.go
// TestGatherFreezesSpectateVisibility;WatchBattle 与列表 spectate_test.go;全局路径
// global_spectator_verdict_test.go TestGlobalFormationWatchable。

import (
	"testing"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 用例里常用的两个可见性取值(全名太长,表驱动用例读不下去);同包其它观战用例也用。
const (
	visPublic  = battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC
	visPrivate = battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE
)

// unknownMatchMode 返回一个不在 MatchMode 枚举里的值(现有最大值 + 1),不随 proto 新增枚举而失效。
func unknownMatchMode() matchpb.MatchMode {
	var highest int32
	for value := range matchpb.MatchMode_name {
		highest = max(highest, value)
	}
	return matchpb.MatchMode(highest + 1)
}

// unknownActivityKind 返回一个不在 EBattleActivityKind 枚举里的值(现有最大值 + 1)。
func unknownActivityKind() battlepb.EBattleActivityKind {
	var highest int32
	for value := range battlepb.EBattleActivityKind_name {
		highest = max(highest, value)
	}
	return battlepb.EBattleActivityKind(highest + 1)
}

func TestSpectateVisibilityPolicy(t *testing.T) {
	guildTrial := &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL}
	cases := []struct {
		name     string
		mode     matchpb.MatchMode
		activity *battlepb.BattleActivityContext
		want     battlepb.ESpectateVisibility
	}{
		{"单人 PVE 公开", matchpb.MatchMode_MATCH_MODE_PVE_SOLO, nil, visPublic},
		{"组队 PVE 公开", matchpb.MatchMode_MATCH_MODE_PVE_TEAM, nil, visPublic},
		{"活动类型 NONE 不算活动局", matchpb.MatchMode_MATCH_MODE_PVE_TEAM, &battlepb.BattleActivityContext{}, visPublic},
		{"帮会历练私密,压过组队 PVE 的公开", matchpb.MatchMode_MATCH_MODE_PVE_TEAM, guildTrial, visPrivate},
		{"活动局不看模式", matchpb.MatchMode_MATCH_MODE_PVE_SOLO, guildTrial, visPrivate},
		{"未知活动类型按私密", matchpb.MatchMode_MATCH_MODE_PVE_TEAM,
			&battlepb.BattleActivityContext{Kind: unknownActivityKind()}, visPrivate},
		{"1V1 计分局私密", matchpb.MatchMode_MATCH_MODE_1V1, nil, visPrivate},
		{"5V5 计分局私密", matchpb.MatchMode_MATCH_MODE_5V5, nil, visPrivate},
		{"点名切磋私密", matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, nil, visPrivate},
		{"人数档 PVP 私密", matchpb.MatchMode_MATCH_MODE_PVP_TEAM, nil, visPrivate},
		{"3V3 未开放私密", matchpb.MatchMode_MATCH_MODE_3V3, nil, visPrivate},
		{"漏填模式私密", matchpb.MatchMode_MATCH_MODE_UNSPECIFIED, nil, visPrivate},
		{"表外模式值私密", unknownMatchMode(), nil, visPrivate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, spectateVisibilityFor(c.mode, c.activity))
		})
	}
}

// 每个 MatchMode 枚举值都必须在显式决策表里、且明确判为 PUBLIC 或 PRIVATE:proto 新增模式而
// spectateVisibilityOfMode 没加一行,这里即红,新模式不会被 default 静默归到哪一边。
func TestSpectateVisibilityDecidesEveryMatchMode(t *testing.T) {
	for value, name := range matchpb.MatchMode_name {
		visibility, decided := spectateVisibilityOfMode(matchpb.MatchMode(value))
		require.True(t, decided, "%s 没进可见性决策表:在 spectateVisibilityOfMode 里显式写一行", name)
		require.Contains(t, []battlepb.ESpectateVisibility{visPublic, visPrivate}, visibility,
			"%s 必须明确判为 PUBLIC 或 PRIVATE", name)
	}
	visibility, decided := spectateVisibilityOfMode(unknownMatchMode())
	require.False(t, decided, "表外的值不得算作已决策")
	require.Equal(t, visPrivate, visibility, "表外的值按私密")
}

// 全局成局持久的建房原件:只有可见性为 PUBLIC 才放行;未填 / 未知值 / 空 / 坏字节一律 fail-closed。
func TestFormationSpectatePublic(t *testing.T) {
	encode := func(visibility battlepb.ESpectateVisibility) []byte {
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&battlepb.CreateBattleRequest{
			BattleId:           880_900_001,
			MatchMode:          uint32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO),
			SpectateVisibility: visibility,
		})
		require.NoError(t, err)
		return raw
	}
	cases := []struct {
		name          string
		createRequest []byte
		want          bool
	}{
		{"公开", encode(visPublic), true},
		{"私密", encode(visPrivate), false},
		{"未填可见性", encode(battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_UNSPECIFIED), false},
		{"未知可见性取值", encode(battlepb.ESpectateVisibility(99)), false},
		{"空字节:原件尚未落库", nil, false},
		{"坏字节:截断的 varint", []byte{0x08}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, formationSpectatePublic(c.createRequest))
		})
	}
}
