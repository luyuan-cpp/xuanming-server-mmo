package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	dspb "proto/data_service"
	smpb "proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func revisionFixtureValue(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	value, err := mr.Get(key)
	require.NoError(t, err)
	return value
}

func TestEnterSceneCurrentSQLRevisionPrecedesOldDoneAndAllProjectionWrites(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "revision-test")
	sc.RoleDirectory = testRoleDirectory(2)
	const player = uint64(99001)
	key := redisslot.EnterSceneDedupKey(player, "old-success")
	mr.Set(key, "done:old-response")
	before := mr.Keys()
	out, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&smpb.EnterSceneRequest{PlayerId: player, OwnerRevision: 1, RequestId: "old-success"})
	require.Nil(t, out)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, before, mr.Keys(), "旧R不得先访问done或推进投影")
	require.Equal(t, "done:old-response", revisionFixtureValue(t, mr, key))
}

func TestEnterSceneUnknownOrFrozenCurrentRoleFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing-client", "sql-unknown", "listing"} {
		t.Run(mode, func(t *testing.T) {
			sc, mr := newTestSvcCtx(t, "revision-test")
			switch mode {
			case "missing-client":
				sc.RoleDirectory = nil
			case "sql-unknown":
				sc.RoleDirectory = roleDirectoryFunc(func(context.Context, uint64) (*dspb.AccountRoleOwnership, error) {
					return nil, errors.New("unknown SQL result")
				})
			case "listing":
				sc.RoleDirectory = roleDirectoryFunc(func(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
					out, err := testRoleDirectory(1).Lookup(ctx, player)
					out.TradeListingId = 9
					return out, err
				})
			}
			out, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&smpb.EnterSceneRequest{PlayerId: 99002, OwnerRevision: 1})
			require.Error(t, err)
			require.Nil(t, out)
			require.Empty(t, mr.Keys())
		})
	}
}

func TestLocationRevisionFloorUsesExactUint64AndBlocksDelayedOldPlacement(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "revision-test")
	const player = uint64(99003)
	const oldRevision = uint64(9007199254740992)
	const newRevision = uint64(9007199254740993)
	require.NoError(t, observeCurrentRevision(sc, player, oldRevision))
	old := placementGuard{mint: true, ownerRevision: oldRevision, expectLoc: expectObservedLocation("")}
	require.NoError(t, observeCurrentRevision(sc, player, newRevision))
	_, err := placePlayerLocation(sc, player, 100, "10", 1, old)
	require.ErrorIs(t, err, errOwnerEpochConflict)
	require.False(t, mr.Exists(getPlayerLocationKey(player)))
	_, err = placePlayerLocation(sc, player, 100, "10", 1, placementGuard{mint: true, ownerRevision: newRevision, expectLoc: expectObservedLocation("")})
	require.NoError(t, err)
	_, raw, tag, err := getPlayerLocationSnapshot(sc, player)
	require.NoError(t, err)
	r, err := taggedLocationRevision(tag, raw)
	require.NoError(t, err)
	require.Equal(t, newRevision, r)
	require.Error(t, observeCurrentRevision(sc, player, oldRevision))
	require.NoError(t, observeCurrentRevision(sc, player, ^uint64(0)))
	require.Equal(t, "18446744073709551615", revisionFixtureValue(t, mr, locationRevisionFloorKey(player)))
}

func TestLocationRevisionRepairRequiresExactRawAndTagAndKeepsEpochFloor(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "revision-test")
	const player, scene = uint64(99004), uint64(99404)
	raw := leaveReleaseSeed(t, mr, player, scene, "10", 9)
	oldTag := locationRevisionTag(1, raw)
	mr.Set(locationRevisionKey(player), oldTag)
	require.NoError(t, observeCurrentRevision(sc, player, 2))
	// 相同PB原文也不能让迟到旧tag删除更新归属的投影。
	mr.Set(locationRevisionKey(player), locationRevisionTag(2, raw))
	outcome, err := deletePlayerLocationGuardedWithTag(sc, player, raw, 1, scene, leaveDeleteModeAny, 0, oldTag, 2)
	require.NoError(t, err)
	require.Equal(t, leaveOutcomeSuperseded, outcome)
	require.Equal(t, raw, revisionFixtureValue(t, mr, getPlayerLocationKey(player)))
	mr.Set(locationRevisionKey(player), oldTag)
	loc, _, _, err := locationForRevision(sc, player, 2)
	require.NoError(t, err)
	require.Nil(t, loc)
	require.False(t, mr.Exists(locationRevisionKey(player)))
	require.Equal(t, "9", revisionFixtureValue(t, mr, ownerepoch.OwnerEpochKey(player)))
	require.Equal(t, revisionText(2), revisionFixtureValue(t, mr, locationRevisionFloorKey(player)))
	require.Equal(t, "1", sceneMemberCount(mr, scene))
}

func TestUnknownLocationRevisionIsNeverFilledFromCurrentDirectory(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "revision-test")
	raw := leaveReleaseSeed(t, mr, 99005, 99405, "10", 5)
	require.NoError(t, observeCurrentRevision(sc, 99005, 2))
	loc, observed, tag, err := locationForRevision(sc, 99005, 2)
	require.NoError(t, err)
	require.NotNil(t, loc)
	require.Equal(t, raw, observed)
	require.Empty(t, tag)
	require.False(t, mr.Exists(locationRevisionKey(99005)))
}

func TestLeaveSceneInvalidOriginalRevisionSidecarCannotAcknowledgeCleanup(t *testing.T) {
	for _, mode := range []string{"invalid-format", "different-raw-hash"} {
		t.Run(mode, func(t *testing.T) {
			sc, mr := newTestSvcCtx(t, "revision-corrupt-cleanup")
			const player, scene = uint64(99007), uint64(99407)
			raw := leaveReleaseSeed(t, mr, player, scene, "10", 9)
			tag := "damaged-revision-sidecar"
			if mode == "different-raw-hash" {
				tag = locationRevisionTag(1, raw+"different-original")
			}
			mr.Set(locationRevisionKey(player), tag)
			beforeKeys := mr.Keys()
			beforeCount := sceneMemberCount(mr, scene)
			out, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&smpb.LeaveSceneRequest{PlayerId: player, SceneId: scene})
			require.Nil(t, out, "新标签损坏不是旧PB毒数据，不得成功ACK清理")
			require.ErrorIs(t, err, errLocationRevisionProjectionCorrupt)
			require.Equal(t, beforeKeys, mr.Keys())
			require.Equal(t, raw, revisionFixtureValue(t, mr, getPlayerLocationKey(player)))
			require.Equal(t, tag, revisionFixtureValue(t, mr, locationRevisionKey(player)))
			require.Equal(t, beforeCount, sceneMemberCount(mr, scene))
			require.Equal(t, "9", revisionFixtureValue(t, mr, ownerepoch.OwnerEpochKey(player)))
		})
	}
}

func revisionRollbackFixture(t *testing.T, known, mint bool) (*svc.ServiceContext, *miniredis.Miniredis, uint64, placedLocation, routeRollbackPlan) {
	t.Helper()
	sc, mr := newTestSvcCtx(t, "revision-rollback")
	const player = uint64(99006)
	raw := leaveReleaseSeed(t, mr, player, 99406, "10", 1)
	old := &smpb.PlayerLocation{}
	require.NoError(t, proto.Unmarshal([]byte(raw), old))
	tag := ""
	if known {
		tag = locationRevisionTag(1, raw)
		mr.Set(locationRevisionKey(player), tag)
	}
	require.NoError(t, observeCurrentRevision(sc, player, 1))
	marker := ""
	if mint {
		marker = "1:1700000000000"
		mr.Set(ownerepoch.HandoffKey(player), marker)
	}
	placed, err := placePlayerLocation(sc, player, 99407, "20", 1, placementGuard{observedEpoch: 1, mint: mint, requiredMarker: marker, ownerRevision: 1, observedTag: tag, expectLoc: expectObservedLocation(raw)})
	require.NoError(t, err)
	plan, err := planRouteRollback(old, raw, 1, placed, marker, false)
	require.NoError(t, err)
	return sc, mr, player, placed, plan
}

func TestLocationRevisionDelayedRollbackCannotTouchNewRevisionFloor(t *testing.T) {
	sc, mr, player, placed, plan := revisionRollbackFixture(t, true, true)
	require.NoError(t, observeCurrentRevision(sc, player, 2))
	restored, echo := rollbackPlayerPlacement(sc, NewEnterSceneLogic(context.Background(), sc).Logger, player, placed, plan)
	require.False(t, restored)
	require.Zero(t, echo)
	require.Equal(t, placed.raw, revisionFixtureValue(t, mr, getPlayerLocationKey(player)))
	require.Equal(t, placed.tag, revisionFixtureValue(t, mr, locationRevisionKey(player)))
	require.Equal(t, strconv.FormatUint(placed.epoch, 10), revisionFixtureValue(t, mr, ownerepoch.OwnerEpochKey(player)))
	require.Equal(t, revisionText(2), revisionFixtureValue(t, mr, locationRevisionFloorKey(player)))
}

func TestLocationRevisionRollbackRestoresOriginalTagAndNeverFillsUnknown(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, mint := range []bool{false, true} {
			t.Run(fmt.Sprintf("known=%v/mint=%v", known, mint), func(t *testing.T) {
				sc, mr, player, placed, plan := revisionRollbackFixture(t, known, mint)
				restored, _ := rollbackPlayerPlacement(sc, NewEnterSceneLogic(context.Background(), sc).Logger, player, placed, plan)
				require.True(t, restored)
				require.Equal(t, plan.restoredRaw, revisionFixtureValue(t, mr, getPlayerLocationKey(player)))
				if known {
					require.Equal(t, locationRevisionTag(1, plan.restoredRaw), revisionFixtureValue(t, mr, locationRevisionKey(player)))
				} else {
					require.False(t, mr.Exists(locationRevisionKey(player)), "unknown原件不能借回滚补当前R")
				}
				require.Equal(t, revisionText(1), revisionFixtureValue(t, mr, locationRevisionFloorKey(player)))
			})
		}
	}
}

func TestLocationRevisionRollbackReplayDoesNotRestoreWithdrawnHandoff(t *testing.T) {
	sc, mr, player, placed, plan := revisionRollbackFixture(t, true, true)
	logger := NewEnterSceneLogic(context.Background(), sc).Logger
	restored, _ := rollbackPlayerPlacement(sc, logger, player, placed, plan)
	require.True(t, restored)
	mr.Del(ownerepoch.HandoffKey(player))
	restored, _ = rollbackPlayerPlacement(sc, logger, player, placed, plan)
	require.True(t, restored)
	require.False(t, mr.Exists(ownerepoch.HandoffKey(player)))
	require.Equal(t, locationRevisionTag(1, plan.restoredRaw), revisionFixtureValue(t, mr, locationRevisionKey(player)))
	require.Equal(t, revisionText(1), revisionFixtureValue(t, mr, locationRevisionFloorKey(player)))
}
