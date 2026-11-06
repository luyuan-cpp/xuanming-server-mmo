package logic

// LeaveScene 判死释放(dead_holder_release.go):读写权行 → etcd 线性一致读复核持有实例 → 按读到的那一份比较并写。
// 夹具沿用 leave_release_test.go(leaveReleaseSeed / fakePlayerStoreHolders);本文件的辅助标识符统一加 deadHolder 前缀。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	gproto "google.golang.org/protobuf/proto"

	dspb "proto/data_service"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
)

// deadHolderInstance 是判死节点 "10" 上那个进程的启动 UUID(写权行 holder_instance 里记的就是它)。
const deadHolderInstance = "33333333-3333-4333-8333-333333333333"

// stubDeadHolderRegistryForTest 替换 etcd 复核:返回 registered 里的实例;err 非 nil 时复核失败。返回调用计数。
func stubDeadHolderRegistryForTest(t *testing.T, err error, registered ...string) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	prev := freshRegistryInstancesOverride
	freshRegistryInstancesOverride = func(context.Context) (map[string]struct{}, error) {
		calls.Add(1)
		if err != nil {
			return nil, err
		}
		instances := map[string]struct{}{}
		for _, instance := range registered {
			instances[instance] = struct{}{}
		}
		return instances, nil
	}
	t.Cleanup(func() { freshRegistryInstancesOverride = prev })
	return calls
}

// seedDeadHolderLeave 摆出 LeaveScene 判 owner_dead 的现场:location 在节点 "10"、owner_epoch 3;本副本镜像已同步、
// 节点 "10" 不在册,负载集里也没有它(与 TestLeaveScene_OwnerDeadReleasesPlayerStoreBeforeDeleting 同一摆法)。
// 写权行由 holders 回答。返回 location 原文。
func seedDeadHolderLeave(t *testing.T, playerID, sceneID uint64, holders *fakePlayerStoreHolders) (*svc.ServiceContext, *miniredis.Miniredis, string) {
	t.Helper()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	if holders != nil {
		installFakePlayerStoreHolders(sc, holders)
	}
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	markKnownNodesSyncedForTest(t)
	mr.Del(nodeLoadKey(testZoneId))
	return sc, mr, raw
}

// deadHolderRow 是判死节点上那个进程在 location 这一代认领下的写权行。
func deadHolderRow() *dspb.PlayerStoreHolder {
	return &dspb.PlayerStoreHolder{OwnerEpoch: 3, HolderInstance: []byte(deadHolderInstance), ClaimSeq: 4}
}

// handover-single-writer-v#1 的新路径:判死看的是本副本镜像(可能滞后),判死之后持有进程又可能以同一 UUID 失租重注册、
// 玩家同节点重连复用实体。etcd 线性一致读看到它在册就不释放,location 一个字节不动,回 FailedPrecondition 让清理重试。
// 旧实现只比代次,照样释放并删 location,在线玩家被踢。
func TestLeaveScene_OwnerDeadHolderBackInEtcdKeepsLocation(t *testing.T) {
	const (
		playerID = uint64(8341)
		sceneID  = uint64(9441)
	)
	holders := &fakePlayerStoreHolders{holder: deadHolderRow()}
	sc, mr, raw := seedDeadHolderLeave(t, playerID, sceneID, holders)
	calls := stubDeadHolderRegistryForTest(t, nil, deadHolderInstance)
	forgetInstanceGoneObservationForTest(t, deadHolderInstance)
	noteInstanceGoneFromRegistry(deadHolderInstance)

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "持有进程回来了:location 不许删")
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Empty(t, holders.players, "不许释放:那会踢掉同节点重连的在线玩家")
	assert.Equal(t, 1, holders.reads)
	assert.EqualValues(t, 1, calls.Load(), "释放前必须向 etcd 复核一次")
	_, observed := instanceGoneObservedSince(deadHolderInstance)
	assert.False(t, observed, "复核看到它在册,旧的消失记录作废")
}

// 判死节点还没认领(交接不 release,写权行停在上一代、仍然活着的交接源上):拿不准交接源此刻是否还持有这名玩家
// (交接回滚之后它就是现任),同样不释放。released 保持 0、location 留着,等玩家下次进场换掉它(已接受的残余)。
func TestLeaveScene_OwnerDeadHandoverSourceStillRegisteredKeepsLocation(t *testing.T) {
	const (
		playerID = uint64(8346)
		sceneID  = uint64(9446)
		source   = "55555555-5555-4555-8555-555555555555"
	)
	holders := &fakePlayerStoreHolders{holder: &dspb.PlayerStoreHolder{OwnerEpoch: 2, HolderInstance: []byte(source), ClaimSeq: 7}}
	sc, mr, raw := seedDeadHolderLeave(t, playerID, sceneID, holders)
	stubDeadHolderRegistryForTest(t, nil, source)

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Empty(t, holders.players)
}

// 复核确认持有进程仍不在册:按读到的那一份(E, H, claim_seq)比较并写,代次用 location 这一代,然后删 location。
func TestLeaveScene_OwnerDeadHolderGoneReleasesTheRowItRead(t *testing.T) {
	const (
		playerID = uint64(8342)
		sceneID  = uint64(9442)
	)
	holders := &fakePlayerStoreHolders{holder: deadHolderRow()}
	sc, mr, _ := seedDeadHolderLeave(t, playerID, sceneID, holders)
	calls := stubDeadHolderRegistryForTest(t, nil, "44444444-4444-4444-8444-444444444444")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.EqualValues(t, 1, calls.Load())
	assert.Equal(t, []uint64{playerID}, holders.players)
	assert.Equal(t, []uint64{3}, holders.epochs)
	require.Len(t, holders.seen, 1)
	assert.True(t, gproto.Equal(deadHolderRow(), holders.seen[0]), "比较并写拿的是复核之前读到的那一份")
}

// 复核做不成(etcd 超时 / 不可达)一律不释放:location 不动,回 Unavailable,下一次重试再复核。
func TestLeaveScene_OwnerDeadRegistryRecheckFailureKeepsLocation(t *testing.T) {
	const (
		playerID = uint64(8343)
		sceneID  = uint64(9443)
	)
	holders := &fakePlayerStoreHolders{holder: deadHolderRow()}
	sc, mr, raw := seedDeadHolderLeave(t, playerID, sceneID, holders)
	stubDeadHolderRegistryForTest(t, errors.New("etcd unreachable"))

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Empty(t, holders.players)
}

// 读之后、提交之前写权行变了(同实例同代次重登只有 claim_seq 变):data_service 回 FailedPrecondition,location 不动。
func TestLeaveScene_OwnerDeadHolderRowChangedKeepsLocation(t *testing.T) {
	const (
		playerID = uint64(8344)
		sceneID  = uint64(9444)
	)
	holders := &fakePlayerStoreHolders{holder: deadHolderRow(),
		err: status.Error(codes.FailedPrecondition, "player store holder row changed since it was read")}
	sc, mr, raw := seedDeadHolderLeave(t, playerID, sceneID, holders)
	stubDeadHolderRegistryForTest(t, nil)

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Len(t, holders.players, 1, "比较并写发了,只是没写成")
}

// 没有东西要释放的几种读法:没有写权行、代次已比 location 新(新的持有者认领过了)→ 不释放也不复核,照常删 location;
// 写权行没有持有者(建角初行、已被判死释放过)→ 没有要复核的进程,直接比较并写(挡住可能在途的认领);读失败 → 保留 location。
func TestLeaveScene_OwnerDeadReleaseFollowsTheRowItRead(t *testing.T) {
	cases := []struct {
		name         string
		holders      *fakePlayerStoreHolders
		wantDeleted  bool
		wantReleases int
	}{
		{name: "没有写权行", holders: &fakePlayerStoreHolders{missing: true}, wantDeleted: true},
		{name: "新的持有者已在更新的代次认领", holders: &fakePlayerStoreHolders{
			holder: &dspb.PlayerStoreHolder{OwnerEpoch: 4, HolderInstance: []byte(deadHolderInstance), ClaimSeq: 5}}, wantDeleted: true},
		{name: "没有持有者", holders: &fakePlayerStoreHolders{holder: &dspb.PlayerStoreHolder{OwnerEpoch: 1}},
			wantDeleted: true, wantReleases: 1},
		{name: "读写权行失败", holders: &fakePlayerStoreHolders{readErr: status.Error(codes.Unavailable, "data_service down")}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			playerID, sceneID := uint64(8350+i), uint64(9450+i)
			sc, mr, raw := seedDeadHolderLeave(t, playerID, sceneID, tc.holders)
			calls := stubDeadHolderRegistryForTest(t, errors.New("复核不该发生"))

			_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
			assert.Zero(t, calls.Load(), "没有要复核的持有进程")
			assert.Len(t, tc.holders.players, tc.wantReleases)
			if tc.wantDeleted {
				require.NoError(t, err)
				assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
				return
			}
			require.Error(t, err)
			assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
		})
	}
}

// 只按代次的释放器装了、判死释放的两步没装(装配错):fail-closed,保留 location,不退回只比代次的旧释放。
func TestLeaveScene_OwnerDeadWithoutDeadHolderFenceKeepsLocation(t *testing.T) {
	const (
		playerID = uint64(8345)
		sceneID  = uint64(9445)
	)
	sc, mr, raw := seedDeadHolderLeave(t, playerID, sceneID, nil)
	legacy := &fakePlayerStoreHolders{}
	sc.PlayerStoreHolders = legacy

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Empty(t, legacy.players)
}

// 每条路径的结果标签(dead_holder_release_total{result})与错误:「还不能释放」的两种(在册 / 行已变)是 FailedPrecondition,
// 调用方按 Info 记;其余错误才是故障。标签是封闭集合,取值以 metrics.DeadHolderRelease* 为准。
func TestReleaseDeadPlayerStoreHolder_ResultLabelPerPath(t *testing.T) {
	rowChanged := status.Error(codes.FailedPrecondition, "player store holder row changed since it was read")
	cases := []struct {
		name        string
		holders     *fakePlayerStoreHolders
		registry    error
		registered  []string
		wantResult  string
		wantCode    codes.Code
		wantKeepsOK bool
	}{
		{name: "没有写权行", holders: &fakePlayerStoreHolders{missing: true}, wantResult: metrics.DeadHolderReleaseNoRow, wantCode: codes.OK},
		{name: "新持有者已认领", holders: &fakePlayerStoreHolders{
			holder: &dspb.PlayerStoreHolder{OwnerEpoch: 4, HolderInstance: []byte(deadHolderInstance), ClaimSeq: 5}},
			wantResult: metrics.DeadHolderReleaseNewerOwner, wantCode: codes.OK},
		{name: "持有实例在册", holders: &fakePlayerStoreHolders{holder: deadHolderRow()}, registered: []string{deadHolderInstance},
			wantResult: metrics.DeadHolderReleaseHolderRegistered, wantCode: codes.FailedPrecondition, wantKeepsOK: true},
		{name: "etcd 复核失败", holders: &fakePlayerStoreHolders{holder: deadHolderRow()}, registry: errors.New("etcd unreachable"),
			wantResult: metrics.DeadHolderReleaseRegistryUnavailable, wantCode: codes.Unavailable},
		{name: "写权行已变", holders: &fakePlayerStoreHolders{holder: deadHolderRow(), err: rowChanged},
			wantResult: metrics.DeadHolderReleaseRowChanged, wantCode: codes.FailedPrecondition, wantKeepsOK: true},
		{name: "读写权行失败", holders: &fakePlayerStoreHolders{readErr: status.Error(codes.Unavailable, "data_service down")},
			wantResult: metrics.DeadHolderReleaseError, wantCode: codes.Unavailable},
		{name: "已释放", holders: &fakePlayerStoreHolders{holder: deadHolderRow()},
			wantResult: metrics.DeadHolderReleaseReleased, wantCode: codes.OK},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, _, _ := seedDeadHolderLeave(t, uint64(8370+i), uint64(9470+i), tc.holders)
			stubDeadHolderRegistryForTest(t, tc.registry, tc.registered...)

			result, err := releaseDeadPlayerStoreHolderOnce(context.Background(), sc, uint64(8370+i), 3)
			assert.Equal(t, tc.wantResult, result)
			assert.Equal(t, tc.wantCode, status.Code(err))
			assert.Equal(t, tc.wantKeepsOK, err != nil && deadHolderReleaseKeepsLocation(err))
		})
	}
}
