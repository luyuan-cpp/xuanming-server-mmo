package logic

import (
	"context"
	"errors"
	"testing"

	"data_service/internal/store"
	"data_service/internal/svc"
	componentpb "proto/common/component"
	dbpb "proto/common/database"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetPlayerItemGainBlock 的契约(docs/design/player-inbox-service.md §2.2):语义表同两本账本,读的是资料行。

type fakePlayerStoreProfiles struct {
	row *store.PlayerProfileRow
	err error
}

func (f fakePlayerStoreProfiles) ReadPlayerStoreProfile(context.Context, uint64) (*store.PlayerProfileRow, error) {
	return f.row, f.err
}

func profileSvc(row *store.PlayerProfileRow, err error) *svc.ServiceContext {
	return &svc.ServiceContext{PlayerStoreProfiles: fakePlayerStoreProfiles{row: row, err: err}}
}

func profileRow(block *componentpb.PlayerItemGainBlockComp) *store.PlayerProfileRow {
	return &store.PlayerProfileRow{Player: &dbpb.PlayerDatabase{PlayerId: playerStoreLedgerPlayer, ItemGainBlock: block}}
}

// 存档里的封禁表原样返回,不排序、不去重:运维看到的就是 scene 下一次加载时拿到的那一份。
func TestGetPlayerItemGainBlock_ReturnsThePersistedList(t *testing.T) {
	stored := &componentpb.PlayerItemGainBlockComp{ItemConfigIds: []uint32{30045, 30046}}
	block, found, err := GetPlayerItemGainBlock(context.Background(), profileSvc(profileRow(stored), nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, []uint32{30045, 30046}, block.GetItemConfigIds())
}

// 写权行在、从未被封过:found=true + 空消息,不是「没有存档」。
func TestGetPlayerItemGainBlock_FoundWithoutBlocksIsNotAbsent(t *testing.T) {
	block, found, err := GetPlayerItemGainBlock(context.Background(), profileSvc(profileRow(nil), nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.True(t, found)
	require.NotNil(t, block)
	assert.Empty(t, block.GetItemConfigIds())
}

// 没有写权行:found=false,不是错误。
func TestGetPlayerItemGainBlock_NoOwnerRowIsAbsent(t *testing.T) {
	block, found, err := GetPlayerItemGainBlock(context.Background(), profileSvc(nil, nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, block)
}

// 读不成不能退化成 found=false:运维会把「没有」读成「没封」。调用方放弃不记成本服务故障。
func TestGetPlayerItemGainBlock_ReadErrorIsNotAbsent(t *testing.T) {
	ctx := context.Background()
	s := profileSvc(nil, errors.New("tikv down"))

	_, found, err := GetPlayerItemGainBlock(ctx, s, playerStoreLedgerPlayer)
	assert.False(t, found)
	assert.Equal(t, codes.Unavailable, status.Code(err))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = GetPlayerItemGainBlock(canceled, s, playerStoreLedgerPlayer)
	assert.Equal(t, codes.Canceled, status.Code(err))
}

// 资料行读取器没装配是 Unavailable;player_id = 0 是 InvalidArgument。只装了钱包行读取器也不能拿它顶替。
func TestGetPlayerItemGainBlock_NotAssembledIsUnavailable(t *testing.T) {
	_, _, err := GetPlayerItemGainBlock(context.Background(), &svc.ServiceContext{}, playerStoreLedgerPlayer)
	assert.Equal(t, codes.Unavailable, status.Code(err))

	_, _, err = GetPlayerItemGainBlock(context.Background(), directSvc(assetRow(true, nil), nil), playerStoreLedgerPlayer)
	assert.Equal(t, codes.Unavailable, status.Code(err))

	_, _, err = GetPlayerItemGainBlock(context.Background(), profileSvc(nil, nil), 0)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
