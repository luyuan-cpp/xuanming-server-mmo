package logic

import (
	"context"
	"testing"

	"guild/internal/constants"
	cgpb "proto/content_guard"
	pb "proto/guild"
	"shared/contentguard"
	tablepb "shared/generated/pb/table"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRenameGuildRequiresSessionAndValidatesInputBeforeRepo(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil)

	// 无会话拒绝。
	_, err := l.RenameGuild(context.Background(), &pb.RenameGuildRequest{GuildId: 1, Name: "逍遥帮"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// 空帮会 id。
	resp, err := l.RenameGuild(clientCtx(101), &pb.RenameGuildRequest{GuildId: 0, Name: "逍遥帮"})
	require.NoError(t, err)
	require.Equal(t, constants.ErrGuildNotFound, resp.GetErrorMessage().GetId())

	// 空名字 / 非法控制字符。
	resp, err = l.RenameGuild(clientCtx(101), &pb.RenameGuildRequest{GuildId: 1, Name: "   "})
	require.NoError(t, err)
	require.Equal(t, constants.ErrGuildNameInvalid, resp.GetErrorMessage().GetId())
}

func TestRenameGuildEnforcesContentGuard(t *testing.T) {
	zones := &fakeHomeZones{zone: 1}
	for _, tc := range []struct {
		name    string
		verdict contentguard.Verdict
		wantTip uint32
	}{
		{"sensitive", contentguard.Reject, constants.ErrGuildNameSensitive},
		{"unavailable", contentguard.Unavailable, constants.ErrTextCheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotScene cgpb.TextScene
			l := NewGuildLogic(nil, nil, nil, zones, WithContentGuard(contentguard.Func(
				func(_ context.Context, scene cgpb.TextScene, _ uint64, _ string) contentguard.Result {
					gotScene = scene
					return contentguard.Result{Verdict: tc.verdict}
				},
			)))
			resp, err := l.RenameGuild(clientCtx(101), &pb.RenameGuildRequest{GuildId: 9, Name: "新帮名"})
			require.NoError(t, err)
			require.Equal(t, cgpb.TextScene_TEXT_SCENE_GUILD_NAME, gotScene)
			require.Equal(t, tc.wantTip, resp.GetErrorMessage().GetId())
		})
	}
}

func TestValidateGuildTablesChecksRenameRuleBounds(t *testing.T) {
	badCooldown := legalGuildRule()
	badCooldown.RenameCooldownSeconds = maxGuildRenameDurationSeconds + 1
	require.Error(t, validateGuildTables(badCooldown, legalGuildLevels()))

	badHold := legalGuildRule()
	badHold.NameHoldSeconds = maxGuildRenameDurationSeconds + 1
	require.Error(t, validateGuildTables(badHold, legalGuildLevels()))

	badCost := legalGuildRule()
	badCost.RenameCostFunds = maxGuildRenameCostFunds + 1
	require.Error(t, validateGuildTables(badCost, legalGuildLevels()))

	okRule := &tablepb.GuildRuleTable{
		Id:                              guildRuleRowID,
		ApplicationExpireHours:          72,
		MaxPendingApplicationsPerPlayer: 3,
		MaxPendingApplicationsPerGuild:  50,
		RenameCooldownSeconds:           604800,
		NameHoldSeconds:                 604800,
		RenameCostFunds:                 1000,
	}
	require.NoError(t, validateGuildTables(okRule, legalGuildLevels()))
}
