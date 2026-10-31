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

// 账本读的契约(docs/design/player-save-direct-commit.md §6)。

const playerStoreLedgerPlayer = uint64(7001)

type fakePlayerStoreAssets struct {
	row *store.PlayerAssetRow
	err error
}

func (f fakePlayerStoreAssets) ReadPlayerStoreAsset(context.Context, uint64) (*store.PlayerAssetRow, error) {
	return f.row, f.err
}

func directSvc(row *store.PlayerAssetRow, err error) *svc.ServiceContext {
	return &svc.ServiceContext{PlayerStoreAssets: fakePlayerStoreAssets{row: row, err: err}}
}

func assetRow(released bool, ledger *componentpb.PlayerAssetOpLedgerComp) *store.PlayerAssetRow {
	return &store.PlayerAssetRow{OwnerEpoch: 7, SaveSeq: 9, Released: released,
		Player: &dbpb.PlayerDatabase{PlayerId: playerStoreLedgerPlayer, AssetOpLedger: ledger}}
}

// 写权行在、账本行不在:存档在、账本为空,不是「没有存档」。
func TestPlayerStoreLedger_FoundWithEmptyLedgerIsNotAbsent(t *testing.T) {
	snapshot, err := GetPlayerStoreLedger(context.Background(), directSvc(assetRow(true, nil), nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.True(t, snapshot.Found)
	assert.NotNil(t, snapshot.Ledger)

	inbox, found, err := GetPlayerInboxLedger(context.Background(), directSvc(assetRow(true, nil), nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.True(t, found)
	assert.NotNil(t, inbox)
}

// 没有写权行:found=false,不是错误。
func TestPlayerStoreLedger_NoOwnerRowIsAbsent(t *testing.T) {
	snapshot, err := GetPlayerStoreLedger(context.Background(), directSvc(nil, nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.False(t, snapshot.Found)

	_, found, err := GetPlayerInboxLedger(context.Background(), directSvc(nil, nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.False(t, found)
}

// 读不成不能退化成 found=false:调用方会把 absent 当成「再等等」,把故障藏起来。
func TestPlayerStoreLedger_ReadErrorIsNotAbsent(t *testing.T) {
	ctx := context.Background()
	s := directSvc(nil, errors.New("tikv down"))

	snapshot, err := GetPlayerStoreLedger(ctx, s, playerStoreLedgerPlayer)
	assert.False(t, snapshot.Found)
	assert.Equal(t, codes.Unavailable, status.Code(err))

	_, _, err = GetPlayerInboxLedger(ctx, s, playerStoreLedgerPlayer)
	assert.Equal(t, codes.Unavailable, status.Code(err))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = GetPlayerStoreLedger(canceled, s, playerStoreLedgerPlayer)
	assert.Equal(t, codes.Canceled, status.Code(err), "调用方放弃不记成本服务故障")
}

// 账本读没装配起来是 Unavailable,不是「没有这个角色」。
func TestPlayerStoreLedger_NotAssembledIsUnavailable(t *testing.T) {
	_, err := GetPlayerStoreLedger(context.Background(), &svc.ServiceContext{}, playerStoreLedgerPlayer)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	_, err = GetPlayerStoreLedger(context.Background(), directSvc(nil, nil), 0)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// 自动放弃读这一份:released 和账本出自同一次读。
func TestGetPlayerStoreLedger_CarriesReleased(t *testing.T) {
	ledger := &componentpb.PlayerAssetOpLedgerComp{}
	snapshot, err := GetPlayerStoreLedger(context.Background(), directSvc(assetRow(true, ledger), nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.True(t, snapshot.Found)
	assert.True(t, snapshot.Released)
	assert.NotNil(t, snapshot.Ledger)

	snapshot, err = GetPlayerStoreLedger(context.Background(), directSvc(assetRow(false, ledger), nil), playerStoreLedgerPlayer)
	require.NoError(t, err)
	assert.False(t, snapshot.Released)
}
