package logic

// 观战活跃集合 spectate:battles:active 的写入:
//   - 开局成功后入活跃集合(addSpectateActive,best-effort);落点已在 SQL,写不进去不影响开局;
//   - 可见性随建房原件冻结(§10 第 6 条):只收公开局。
// Redis 故障用 miniredis 前置钩子按命令 + key 精确注入。

import (
	"testing"

	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"
)

// registerSpectateBattle 是单测夹具:按开局成功的路径登记一场战斗的活跃集合成员,
// 可见性按 spectateVisibilityFor(mode, 无活动上下文)冻结,私密模式不入活跃集合。
func registerSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64, mode matchpb.MatchMode, createdAtMs uint64) {
	addSpectateActive(svcCtx, battleId, createdAtMs, spectateVisibilityFor(mode, nil))
}

// installSpectateZaddFault 让活跃集合的 ZADD 失败。钩子可能在 Lua 的 redis.call 里带锁重入,所以只读自身的谓词,绝不调 mr.*;
// 与 SetError 共用槽位,装了就别再用 SetError。单个 svcCtx 注入的失败控制在 5 次以内(go-zero Redis 客户端自带 Google SRE 熔断,protection=5)。
func installSpectateZaddFault(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if cmd != "ZADD" || len(args) == 0 || args[0] != spectateBattlesActiveKey {
			return false
		}
		c.WriteError("ERR injected spectate active zadd failure")
		return true
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
}

// 开局成功后的公开:公开局入活跃集合;私密局与漏填可见性一律不入(默认拒绝)。
func TestAddSpectateActiveFollowsFrozenVisibility(t *testing.T) {
	for _, c := range []struct {
		name       string
		visibility battlepb.ESpectateVisibility
		wantActive int
	}{
		{"公开局入活跃集合", visPublic, 1},
		{"私密局不入", visPrivate, 0},
		{"漏填可见性不入", battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_UNSPECIFIED, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, _ := newTestSvcCtx(t)

			addSpectateActive(svcCtx, 881010, nowMs(), c.visibility)

			require.Equal(t, c.wantActive, activeIndexSize(t, svcCtx))
		})
	}
}

// 入活跃集合失败仍 best-effort:只是不进观战列表。用公开局:私密局根本不会 ZADD,测不到这条失败路径。
func TestAddSpectateActiveSurvivesZaddFailure(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	installSpectateZaddFault(t, mr)

	addSpectateActive(svcCtx, 881012, nowMs(), visPublic)

	require.Zero(t, activeIndexSize(t, svcCtx), "ZADD 只服务观战列表,不能让它拖垮开局")
}
