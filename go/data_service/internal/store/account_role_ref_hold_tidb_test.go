package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	base "proto/common/base"
	"shared/dirshard"
)

// 真库上的「被留下的建角 ref 标成保留」(account_role_ref.go 的 SweepStaleRefs、committedEvidence)。
// 开关同 account_directory_shard_tidb_test.go(ACCOUNT_DIRECTORY_TEST_DSN),只建/删自己的随机库。

// 用例「同一角色挂在别的账号上的 ACTIVE ref 也是提交证据」:摆出玩家片恢复点早于提交(角色行没了),账号 buyer 上留着一条建角第 1 步
// 写的 PENDING ref、超过宽限;角色真正的账号 seller 上那条已是 ACTIVE(提交之后才写)。判陈旧按 player_id 查到那条 ACTIVE,
// 不写墓碑,把 buyer 上那条标成保留(原因 ref_active);下一轮扫描不再扫它,只在复核名额里重判。
func TestTiDBActiveRefOnAnotherAccountHoldsTheStaleCreateRef(t *testing.T) {
	f := newDirectoryShardFixture(t)
	d, _ := namedDirectoryForTest(t, f) // 名字登记表要接上:没接时证据算核不全,判不到 ref 那一步
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const player = uint64(12) // 第 0 片
	_, err := createInitialRoleForTest(d, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1})
	require.NoError(t, err)
	_, err = f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].ExecContext(ctx, "DELETE FROM account_role WHERE player_id=?", player)
	require.NoError(t, err)
	_, err = f.Raw[dirshard.SlotOfAccount(shardBuyer, shardFixtureK)].ExecContext(ctx,
		"INSERT INTO account_role_ref(account,player_id,state,tx_id,listing_id,home_zone,nonce,created_ms) VALUES (?,?,?,0,0,1,77,"+dbNowMsSQL+"-?)",
		[]byte(shardBuyer), player, refStatePending, refPendingGrace.Milliseconds()+1000)
	require.NoError(t, err)

	result, err := d.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Equal(t, SweepResult{Scanned: 1, Held: 1, HeldTotal: 1, HeldCounted: true}, result)
	var reason []byte
	require.NoError(t, f.Raw[dirshard.SlotOfAccount(shardBuyer, shardFixtureK)].QueryRowContext(ctx,
		"SELECT held_reason FROM account_role_ref WHERE account=? AND player_id=? AND held_ms>0", []byte(shardBuyer), player).Scan(&reason))
	require.Equal(t, committedEvidenceRefActive, string(reason))
	var rows int
	require.NoError(t, f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role WHERE player_id=?", player).Scan(&rows))
	require.Zero(t, rows, "有提交证据,不写墓碑")

	again, err := d.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Equal(t, SweepResult{Rechecked: 1, HeldTotal: 1, HeldCounted: true}, again, "标了保留的不再进扫描名额,也不重复计")
}
