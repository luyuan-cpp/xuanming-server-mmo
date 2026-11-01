package svc

import (
	"context"
	"errors"
	"testing"

	"data_service/internal/config"
	"data_service/internal/store"

	componentpb "proto/common/component"
	dbpb "proto/common/database"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/rolefence"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubFenceStates struct {
	state *rfpb.RoleFenceState
	err   error
}

func (s stubFenceStates) RoleFenceState(context.Context, uint64) (*rfpb.RoleFenceState, error) {
	return s.state, s.err
}

// 目录里没有 ≠ 目录读不到:前者这一行终结,后者行留着稍后再来。映射错了要么卡死要么误判角色不存在。
func TestRecallFenceReader_DistinguishesMissingFromUnavailable(t *testing.T) {
	want := &rfpb.RoleFenceState{PlayerId: 9}
	state, err := recallFenceReader{states: stubFenceStates{state: want}}.GetRoleFenceState(context.Background(), 9)
	require.NoError(t, err)
	assert.Same(t, want, state)

	_, err = recallFenceReader{states: stubFenceStates{err: accountdirectory.ErrMissing}}.GetRoleFenceState(context.Background(), 9)
	require.ErrorIs(t, err, rolefence.ErrRoleNotFound)

	_, err = recallFenceReader{states: stubFenceStates{err: errors.New("库坏了")}}.GetRoleFenceState(context.Background(), 9)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.NotErrorIs(t, err, rolefence.ErrRoleNotFound)
}

type stubAssetReader struct {
	row *store.PlayerAssetRow
	err error
}

func (s stubAssetReader) ReadPlayerStoreAsset(context.Context, uint64) (*store.PlayerAssetRow, error) {
	return s.row, s.err
}

// 没有写权行回 nil,有写权行回非 nil:Loop 靠这个区分「没存档,继续等」和「账本为空」。
func TestRecallLedger_ContractMatchesDataServiceLedger(t *testing.T) {
	ledger, err := recallLedger{assets: stubAssetReader{}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.NoError(t, err)
	assert.Nil(t, ledger)

	ledger, err = recallLedger{assets: stubAssetReader{row: &store.PlayerAssetRow{Player: &dbpb.PlayerDatabase{}}}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.NoError(t, err)
	assert.NotNil(t, ledger, "有存档、账本为空是空消息,不是 nil")

	stored := &componentpb.PlayerAssetOpLedgerComp{}
	ledger, err = recallLedger{assets: stubAssetReader{row: &store.PlayerAssetRow{Player: &dbpb.PlayerDatabase{AssetOpLedger: stored}}}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.NoError(t, err)
	assert.Same(t, stored, ledger)

	_, err = recallLedger{assets: stubAssetReader{err: errors.New("读失败")}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.Error(t, err)
}

func recallEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// 通道装配失败要在连任何东西之前报出来:Enabled=true 时缺密钥、缺 Redis 地址都拒启。
func TestNewRecallChannel_RefusesIncompleteConfigBeforeConnecting(t *testing.T) {
	secret := map[string]string{RecallAssetSecretEnv: "0123456789abcdef0123456789abcdef"}
	enabled := config.Config{Recall: config.RecallConfig{Enabled: true}, SharedRedis: config.SharedRedisConf{Host: "127.0.0.1:1"}}
	enabled.Etcd.Hosts = []string{"127.0.0.1:1"}

	noHost := enabled
	noHost.SharedRedis.Host = ""
	_, err := newRecallChannel(noHost, nil, nil, nil, recallEnv(secret))
	require.ErrorContains(t, err, "SharedRedis.Host")

	_, err = newRecallChannel(enabled, nil, nil, nil, recallEnv(nil))
	require.ErrorContains(t, err, RecallAssetSecretEnv)

	_, err = newRecallChannel(enabled, nil, nil, nil, recallEnv(map[string]string{RecallAssetSecretEnv: "short"}))
	require.ErrorContains(t, err, RecallAssetSecretEnv)

	noEtcd := enabled
	noEtcd.Etcd.Hosts = nil
	_, err = newRecallChannel(noEtcd, nil, nil, nil, recallEnv(secret))
	require.ErrorContains(t, err, "Etcd.Hosts")
}

// 没有回收存储就不能接参与者:协调者据此不装配,否则漏问一个就是漏冻结。
func TestRecallFenceParticipant_RequiresTheStore(t *testing.T) {
	_, err := (&ServiceContext{}).recallFenceParticipant("0123456789abcdef0123456789abcdef")
	require.Error(t, err)
}

// 没配回收也要能停:关停路径不能因为这一段没起而报错。
func TestStopRecallChannel_NoopWhenNotAssembled(t *testing.T) {
	s := &ServiceContext{}
	s.StartRecallChannel()
	require.NoError(t, s.StopRecallChannel(context.Background()))
}
