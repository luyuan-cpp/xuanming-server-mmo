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

	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"
)

const playerStoreRPCSecret = "0123456789abcdef0123456789abcdef"

type fakeHolderReleaser struct {
	err     error
	players []uint64
	epochs  []uint64
}

func (f *fakeHolderReleaser) ReleaseDeadPlayerStoreHolder(_ context.Context, player, epoch uint64) error {
	f.players = append(f.players, player)
	f.epochs = append(f.epochs, epoch)
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
