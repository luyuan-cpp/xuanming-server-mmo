package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/generated/persistschema"
)

func proofRow(kind int32, epoch, seq uint64, payload string) playerStoreProofRow {
	return playerStoreProofRow{kind: kind, ownerEpoch: epoch, saveSeq: seq, schemaGen: 1, bytes: []byte(payload)}
}

// 只要有一行不再是建角时的版本,就不是初始存档。
func TestIsInitialPlayerStore(t *testing.T) {
	initial := []playerStoreProofRow{
		proofRow(playerStoreOwnerKind, accountdirectory.InitialOwnerEpoch, accountdirectory.InitialSaveSeq, ""),
		proofRow(1, accountdirectory.InitialOwnerEpoch, accountdirectory.InitialSaveSeq, "hot"),
	}
	require.True(t, isInitialPlayerStore(initial))
	saved := []playerStoreProofRow{
		proofRow(playerStoreOwnerKind, accountdirectory.InitialOwnerEpoch, accountdirectory.InitialSaveSeq, ""),
		proofRow(1, 7, 9, "hot"),
	}
	require.False(t, isInitialPlayerStore(saved))
}

// 摘要覆盖所有列和所有行:任何一处变化都要反映出来,否则冻结期间的改动查不出来。
func TestDigestPlayerStoreRowsCoversEveryColumn(t *testing.T) {
	base := func() []playerStoreProofRow {
		return []playerStoreProofRow{
			{kind: playerStoreOwnerKind, ownerEpoch: 7, saveSeq: 9, schemaGen: 1, released: 1, holderInstance: []byte("a")},
			{kind: 2, ownerEpoch: 7, saveSeq: 9, schemaGen: 1, bytes: []byte("asset")},
		}
	}
	want := digestPlayerStoreRows("zone_1_db", base())
	require.Equal(t, want, digestPlayerStoreRows("zone_1_db", base()), "同样的内容摘要相同")
	require.NotEqual(t, want, digestPlayerStoreRows("zone_2_db", base()), "库名")

	mutations := map[string]func([]playerStoreProofRow){
		"kind":     func(r []playerStoreProofRow) { r[1].kind = 3 },
		"epoch":    func(r []playerStoreProofRow) { r[0].ownerEpoch = 8 },
		"seq":      func(r []playerStoreProofRow) { r[0].saveSeq = 10 },
		"gen":      func(r []playerStoreProofRow) { r[1].schemaGen = 2 },
		"released": func(r []playerStoreProofRow) { r[0].released = 0 },
		"holder":   func(r []playerStoreProofRow) { r[0].holderInstance = []byte("b") },
		"payload":  func(r []playerStoreProofRow) { r[1].bytes = []byte("other") },
	}
	for name, mutate := range mutations {
		rows := base()
		mutate(rows)
		require.NotEqual(t, want, digestPlayerStoreRows("zone_1_db", rows), name)
	}
	// 相邻字段不能互相借位:holder 和 payload 分别写长度前缀。
	left := base()
	left[1].holderInstance, left[1].bytes = []byte("ab"), []byte("c")
	right := base()
	right[1].holderInstance, right[1].bytes = []byte("a"), []byte("bc")
	require.NotEqual(t, digestPlayerStoreRows("zone_1_db", left), digestPlayerStoreRows("zone_1_db", right))
}

// 证据候选在目录事务外读,判定是纯函数:读到的版本低于下限、世代超前、初始形态对不上都拒绝,不能落证据。
func TestDecideFrozenEvidence(t *testing.T) {
	const initial = accountdirectory.InitialOwnerEpoch
	const initialSeq = accountdirectory.InitialSaveSeq
	trade := func(epoch, seq uint64) *dbpb.AccountRoleTrade {
		return &dbpb.AccountRoleTrade{ListingId: 101, PlayerId: 11, SellerOwnerRevision: 1, RequiredOwnerEpoch: epoch, RequiredSaveSeq: seq}
	}
	freshRows := func() []playerStoreProofRow {
		return []playerStoreProofRow{proofRow(playerStoreOwnerKind, initial, initialSeq, ""), proofRow(playerStoreProfileKind, initial, initialSeq, "hot")}
	}
	savedRows := func(epoch, seq uint64) []playerStoreProofRow {
		return []playerStoreProofRow{proofRow(playerStoreOwnerKind, epoch, seq, ""), proofRow(playerStoreProfileKind, epoch, seq, "hot")}
	}

	t.Run("从没进过场_初始提交", func(t *testing.T) {
		e, err := decideFrozenEvidence(trade(0, 0), 0, "zone_1_db", freshRows())
		require.NoError(t, err)
		require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT, e.Kind)
		require.Equal(t, digestPlayerStoreRows("zone_1_db", freshRows()), e.PersistedDigest)
		require.EqualValues(t, 101, e.ListingId)
		require.EqualValues(t, 11, e.PlayerId)
		require.EqualValues(t, 1, e.OwnerRevision)
	})
	t.Run("初始存档上不该有持有者", func(t *testing.T) {
		_, err := decideFrozenEvidence(trade(0, 0), 1, "zone_1_db", freshRows())
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	})
	t.Run("写权行是初始值但别的行存过盘", func(t *testing.T) {
		rows := freshRows()
		rows[1].saveSeq = 2
		_, err := decideFrozenEvidence(trade(0, 0), 0, "zone_1_db", rows)
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	})
	t.Run("初始存档低于历史下限", func(t *testing.T) {
		_, err := decideFrozenEvidence(trade(17, 3), 0, "zone_1_db", freshRows())
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	})
	t.Run("存过盘_追上下限", func(t *testing.T) {
		e, err := decideFrozenEvidence(trade(17, 3), 1, "zone_1_db", savedRows(17, 3))
		require.NoError(t, err)
		require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE, e.Kind)
		require.Equal(t, []uint64{17, 3}, []uint64{e.OwnerEpoch, e.SaveSeq})
	})
	t.Run("最后那笔存盘还没提交_不能冻结", func(t *testing.T) {
		_, err := decideFrozenEvidence(trade(17, 3), 1, "zone_1_db", savedRows(17, 2))
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	})
	t.Run("世代超前_拒绝", func(t *testing.T) {
		rows := savedRows(17, 3)
		rows[0].schemaGen = uint32(persistschema.Gen) + 1
		_, err := decideFrozenEvidence(trade(17, 3), 1, "zone_1_db", rows)
		require.ErrorContains(t, err, "世代")
	})
	t.Run("写权行围栏非法_拒绝", func(t *testing.T) {
		_, err := decideFrozenEvidence(trade(0, 0), 0, "zone_1_db", savedRows(0, 3))
		require.Error(t, err)
	})
	t.Run("摘要绑定库名", func(t *testing.T) {
		a, err := decideFrozenEvidence(trade(0, 0), 0, "zone_1_db", freshRows())
		require.NoError(t, err)
		b, err := decideFrozenEvidence(trade(0, 0), 0, "zone_2_db", freshRows())
		require.NoError(t, err)
		require.NotEqual(t, a.PersistedDigest, b.PersistedDigest)
	})
}
