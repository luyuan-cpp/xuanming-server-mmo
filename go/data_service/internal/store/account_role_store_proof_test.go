package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	"shared/savebarrier"
)

func proofRow(kind int32, epoch, seq uint64, payload string) playerStoreProofRow {
	return playerStoreProofRow{kind: kind, ownerEpoch: epoch, saveSeq: seq, schemaGen: 1, bytes: []byte(payload)}
}

// 只要有一行不再是建角时的版本,就不是初始存档。
func TestIsInitialPlayerStore(t *testing.T) {
	initial := []playerStoreProofRow{
		proofRow(playerStoreOwnerKind, savebarrier.InitialOwnerEpoch, savebarrier.InitialSaveSeq, ""),
		proofRow(1, savebarrier.InitialOwnerEpoch, savebarrier.InitialSaveSeq, "hot"),
	}
	require.True(t, isInitialPlayerStore(initial))
	saved := []playerStoreProofRow{
		proofRow(playerStoreOwnerKind, savebarrier.InitialOwnerEpoch, savebarrier.InitialSaveSeq, ""),
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

