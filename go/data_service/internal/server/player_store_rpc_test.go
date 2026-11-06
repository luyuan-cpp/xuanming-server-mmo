package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/svc"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"
)

const playerStoreRPCSecret = "0123456789abcdef0123456789abcdef"

type fakeHolderReleaser struct {
	err     error
	players []uint64
	epochs  []uint64
	// seen 记下每次比较并写带来的写权行;只按代次的释放记 nil。
	seen []*dspb.PlayerStoreHolder
	// holder 是 ReadPlayerStoreHolder 的回答(nil = 没有写权行),readErr 是它的错误。
	holder  *dspb.PlayerStoreHolder
	readErr error
}

func (f *fakeHolderReleaser) ReleaseDeadPlayerStoreHolder(_ context.Context, player, epoch uint64) error {
	f.players = append(f.players, player)
	f.epochs = append(f.epochs, epoch)
	f.seen = append(f.seen, nil)
	return f.err
}

func (f *fakeHolderReleaser) ReadPlayerStoreHolder(context.Context, uint64) (*dspb.PlayerStoreHolder, error) {
	return f.holder, f.readErr
}

func (f *fakeHolderReleaser) ReleaseDeadPlayerStoreHolderIfUnchanged(_ context.Context, player, epoch uint64, seen *dspb.PlayerStoreHolder) error {
	f.players = append(f.players, player)
	f.epochs = append(f.epochs, epoch)
	f.seen = append(f.seen, seen)
	return f.err
}

type fakeDisplayReader struct {
	players map[uint64]*dspb.PlayerDisplay
	err     error
}

func (f fakeDisplayReader) ReadPlayerDisplays(context.Context, []uint64) (map[uint64]*dspb.PlayerDisplay, error) {
	return f.players, f.err
}

func signedRelease(t *testing.T, caller, secret string) *dspb.ReleaseDeadPlayerStoreHolderRequest {
	t.Helper()
	in := &dspb.ReleaseDeadPlayerStoreHolderRequest{PlayerId: 7, OwnerEpoch: 3}
	require.NoError(t, serviceauth.Sign(accountdirectory.MethodPrefix+"ReleaseDeadPlayerStoreHolder", in, caller, secret, uint64(time.Now().UnixMilli())))
	return in
}

// 只有 scene_manager 的签名能释放写权:这一笔会把在线玩家的持有者踢掉。
func TestReleaseDeadPlayerStoreHolder_RequiresSceneManagerSignature(t *testing.T) {
	verifier, err := serviceauth.New(map[string]string{"scene_manager": playerStoreRPCSecret})
	require.NoError(t, err)
	releaser := &fakeHolderReleaser{}
	server := &DataServiceServer{svcCtx: &svc.ServiceContext{SceneManagerDirectoryAuth: verifier, PlayerStoreHolders: releaser}}
	ctx := context.Background()

	_, err = server.ReleaseDeadPlayerStoreHolder(ctx, &dspb.ReleaseDeadPlayerStoreHolderRequest{PlayerId: 7, OwnerEpoch: 3})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "没签名")
	_, err = server.ReleaseDeadPlayerStoreHolder(ctx, signedRelease(t, "scene_manager", "fedcba9876543210fedcba9876543210"))
	require.Equal(t, codes.PermissionDenied, status.Code(err), "签错")
	require.Empty(t, releaser.players)

	_, err = server.ReleaseDeadPlayerStoreHolder(ctx, signedRelease(t, "scene_manager", playerStoreRPCSecret))
	require.NoError(t, err)
	require.Equal(t, []uint64{7}, releaser.players)
	require.Equal(t, []uint64{3}, releaser.epochs)
}

// 新路径没打开时不能假装释放了:scene_manager 那边会接着删 location。
func TestReleaseDeadPlayerStoreHolder_UnavailableOnOldPath(t *testing.T) {
	server := &DataServiceServer{svcCtx: &svc.ServiceContext{}}
	_, err := server.ReleaseDeadPlayerStoreHolder(context.Background(), &dspb.ReleaseDeadPlayerStoreHolderRequest{PlayerId: 7, OwnerEpoch: 3})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestReleaseDeadPlayerStoreHolder_StoreFailureIsRetryable(t *testing.T) {
	verifier, err := serviceauth.New(map[string]string{"scene_manager": playerStoreRPCSecret})
	require.NoError(t, err)
	server := &DataServiceServer{svcCtx: &svc.ServiceContext{SceneManagerDirectoryAuth: verifier,
		PlayerStoreHolders: &fakeHolderReleaser{err: errors.New("tikv down")}}}
	_, err = server.ReleaseDeadPlayerStoreHolder(context.Background(), signedRelease(t, "scene_manager", playerStoreRPCSecret))
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// LeaveScene 判死释放带着读到的写权行来:走比较并写,原样转交 seen;读之后有人认领过回 FailedPrecondition,
// scene_manager 据此保留 location、下一次重读重核(不是故障)。
func TestReleaseDeadPlayerStoreHolder_WithSeenComparesTheRow(t *testing.T) {
	verifier, err := serviceauth.New(map[string]string{"scene_manager": playerStoreRPCSecret})
	require.NoError(t, err)
	seen := &dspb.PlayerStoreHolder{OwnerEpoch: 3, HolderInstance: []byte("instance-a"), ClaimSeq: 4}
	signed := func() *dspb.ReleaseDeadPlayerStoreHolderRequest {
		in := &dspb.ReleaseDeadPlayerStoreHolderRequest{PlayerId: 7, OwnerEpoch: 3, Seen: seen}
		require.NoError(t, serviceauth.Sign(accountdirectory.MethodPrefix+"ReleaseDeadPlayerStoreHolder", in, "scene_manager", playerStoreRPCSecret, uint64(time.Now().UnixMilli())))
		return in
	}

	releaser := &fakeHolderReleaser{}
	server := &DataServiceServer{svcCtx: &svc.ServiceContext{SceneManagerDirectoryAuth: verifier, PlayerStoreHolders: releaser}}
	_, err = server.ReleaseDeadPlayerStoreHolder(context.Background(), signed())
	require.NoError(t, err)
	require.Equal(t, []uint64{7}, releaser.players)
	require.Equal(t, []uint64{3}, releaser.epochs)
	require.Len(t, releaser.seen, 1)
	require.True(t, proto.Equal(seen, releaser.seen[0]), "比较并写拿的是 scene_manager 读到的那一份")

	changed := &DataServiceServer{svcCtx: &svc.ServiceContext{SceneManagerDirectoryAuth: verifier,
		PlayerStoreHolders: &fakeHolderReleaser{err: accountdirectory.ErrConflict}}}
	_, err = changed.ReleaseDeadPlayerStoreHolder(context.Background(), signed())
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// 改了 seen 的请求签名对不上:seen 在签名覆盖范围内,中途篡改不能把释放引到别的持有者上。
	tampered := signed()
	tampered.Seen = &dspb.PlayerStoreHolder{OwnerEpoch: 3, HolderInstance: []byte("instance-b"), ClaimSeq: 4}
	_, err = server.ReleaseDeadPlayerStoreHolder(context.Background(), tampered)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Len(t, releaser.players, 1)
}

func signedReadHolder(t *testing.T, caller, secret string) *dspb.ReadPlayerStoreHolderRequest {
	t.Helper()
	in := &dspb.ReadPlayerStoreHolderRequest{PlayerId: 7}
	require.NoError(t, serviceauth.Sign(accountdirectory.MethodPrefix+"ReadPlayerStoreHolder", in, caller, secret, uint64(time.Now().UnixMilli())))
	return in
}

// 判死释放的第一步:只认 scene_manager 的签名;没有写权行回 found=false(不是错误);读失败回可重试的 Unavailable;
// 新路径没打开时回 Unavailable。
func TestReadPlayerStoreHolder(t *testing.T) {
	ctx := context.Background()
	verifier, err := serviceauth.New(map[string]string{"scene_manager": playerStoreRPCSecret})
	require.NoError(t, err)

	old := &DataServiceServer{svcCtx: &svc.ServiceContext{}}
	_, err = old.ReadPlayerStoreHolder(ctx, signedReadHolder(t, "scene_manager", playerStoreRPCSecret))
	require.Equal(t, codes.Unavailable, status.Code(err))

	holder := &dspb.PlayerStoreHolder{OwnerEpoch: 3, HolderInstance: []byte("instance-a"), ClaimSeq: 4}
	server := &DataServiceServer{svcCtx: &svc.ServiceContext{SceneManagerDirectoryAuth: verifier, PlayerStoreHolders: &fakeHolderReleaser{holder: holder}}}
	_, err = server.ReadPlayerStoreHolder(ctx, &dspb.ReadPlayerStoreHolderRequest{PlayerId: 7})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "没签名")
	_, err = server.ReadPlayerStoreHolder(ctx, signedReadHolder(t, "scene_manager", "fedcba9876543210fedcba9876543210"))
	require.Equal(t, codes.PermissionDenied, status.Code(err), "签错")

	resp, err := server.ReadPlayerStoreHolder(ctx, signedReadHolder(t, "scene_manager", playerStoreRPCSecret))
	require.NoError(t, err)
	require.True(t, resp.GetFound())
	require.True(t, proto.Equal(holder, resp.GetHolder()))

	missing := &DataServiceServer{svcCtx: &svc.ServiceContext{SceneManagerDirectoryAuth: verifier, PlayerStoreHolders: &fakeHolderReleaser{}}}
	resp, err = missing.ReadPlayerStoreHolder(ctx, signedReadHolder(t, "scene_manager", playerStoreRPCSecret))
	require.NoError(t, err)
	require.False(t, resp.GetFound())
	require.Nil(t, resp.GetHolder())

	failing := &DataServiceServer{svcCtx: &svc.ServiceContext{SceneManagerDirectoryAuth: verifier,
		PlayerStoreHolders: &fakeHolderReleaser{readErr: errors.New("tikv down")}}}
	_, err = failing.ReadPlayerStoreHolder(ctx, signedReadHolder(t, "scene_manager", playerStoreRPCSecret))
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestBatchGetPlayerDisplay(t *testing.T) {
	ctx := context.Background()

	old := &DataServiceServer{svcCtx: &svc.ServiceContext{}}
	_, err := old.BatchGetPlayerDisplay(ctx, &dspb.BatchGetPlayerDisplayRequest{PlayerIds: []uint64{7}})
	require.Equal(t, codes.Unavailable, status.Code(err), "旧路径上没有这份资料")

	direct := &DataServiceServer{svcCtx: &svc.ServiceContext{PlayerStoreDisplays: fakeDisplayReader{
		players: map[uint64]*dspb.PlayerDisplay{7: {Name: "青云"}},
	}}}
	resp, err := direct.BatchGetPlayerDisplay(ctx, &dspb.BatchGetPlayerDisplayRequest{PlayerIds: []uint64{7}})
	require.NoError(t, err)
	require.Equal(t, "青云", resp.GetPlayers()[7].GetName())

	_, err = direct.BatchGetPlayerDisplay(ctx, &dspb.BatchGetPlayerDisplayRequest{PlayerIds: make([]uint64, store.PlayerDisplayBatchLimit+1)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	failing := &DataServiceServer{svcCtx: &svc.ServiceContext{PlayerStoreDisplays: fakeDisplayReader{err: errors.New("tikv down")}}}
	_, err = failing.BatchGetPlayerDisplay(ctx, &dspb.BatchGetPlayerDisplayRequest{PlayerIds: []uint64{7}})
	require.Equal(t, codes.Unavailable, status.Code(err))
}
