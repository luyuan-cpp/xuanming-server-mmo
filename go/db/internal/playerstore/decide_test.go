package playerstore

import (
	"testing"

	storepb "proto/player_store"
)

var (
	instanceA = []byte("instance-a")
	instanceB = []byte("instance-b")
)

const testKnownGen = 5

// 逐格覆盖规格 §4 的 Save 判定表。
func TestDecideSave(t *testing.T) {
	row := func(e, q uint64, gen uint32, holder []byte) *ownerRow {
		return &ownerRow{ownerEpoch: e, saveSeq: q, schemaGen: gen, holder: holder}
	}
	cases := []struct {
		name     string
		row      *ownerRow
		e, q     uint64
		gen      uint32
		instance []byte
		want     storepb.SavePlayerResult
	}{
		{"写权行不存在", nil, 10, 1, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_NO_OWNER},
		{"代次低于库里", row(10, 5, 1, instanceA), 9, 6, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER},
		{"被接管后旧持有者的写", row(10, 5, 1, instanceB), 9, 6, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER},
		{"同代次但持有者是别人", row(10, 5, 1, instanceB), 10, 6, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_NOT_HOLDER},
		{"代次更高但持有者是别人:没认领过", row(10, 5, 1, instanceB), 11, 6, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_UNCLAIMED},
		{"新号初行没有持有者:不能跳过认领", row(1, 0, 1, []byte{}), 1, 1, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_NOT_HOLDER},
		{"代次更高且持有者是自己:放行", row(10, 5, 1, instanceA), 11, 6, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED},
		{"别的实例同序号不算幂等", row(10, 5, 1, instanceB), 10, 5, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_NOT_HOLDER},
		{"同序号:幂等成功", row(10, 5, 1, instanceA), 10, 5, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_EQUAL},
		{"同序号且代次更高:同内容已提交", row(10, 5, 1, instanceA), 11, 5, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_EQUAL},
		{"旧序号", row(10, 5, 1, instanceA), 10, 4, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_OLDER_SEQ},
		{"世代倒退", row(10, 5, 3, instanceA), 10, 6, 2, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_REGRESSED},
		{"世代高于 go/db 认识的", row(10, 5, 1, instanceA), 10, 6, testKnownGen + 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_TOO_NEW},
		{"世代等于 go/db 认识的", row(10, 5, 1, instanceA), 10, 6, testKnownGen, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED},
		{"正常存盘", row(10, 5, 1, instanceA), 10, 6, 1, instanceA, storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSave(tc.row, tc.e, tc.q, tc.gen, tc.instance, testKnownGen)
			if got != tc.want {
				t.Fatalf("decideSave = %v, want %v", got, tc.want)
			}
		})
	}
}

// 逐格覆盖规格 §4 的 LoadAndClaim 判定表。
func TestDecideClaim(t *testing.T) {
	row := func(e uint64, gen uint32, holder []byte) *ownerRow {
		return &ownerRow{ownerEpoch: e, schemaGen: gen, holder: holder}
	}
	cases := []struct {
		name     string
		row      *ownerRow
		e        uint64
		instance []byte
		want     storepb.LoadAndClaimResult
	}{
		{"写权行不存在", nil, 10, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_NO_OWNER},
		{"路由过期", row(10, 1, instanceA), 9, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_STALE_OWNER},
		{"同代次已被别的实例持有", row(10, 1, instanceB), 10, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_NOT_HOLDER},
		{"同实例重进:幂等", row(10, 1, instanceA), 10, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED},
		{"新号初行", row(1, 1, []byte{}), 1, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED},
		{"换持有者:抬代次", row(10, 1, instanceB), 11, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED},
		{"世代比本进程新", row(10, testKnownGen+1, instanceB), 11, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_SCHEMA_TOO_NEW},
		{"世代等于本进程", row(10, testKnownGen, instanceB), 11, instanceA, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideClaim(tc.row, tc.e, tc.instance, testKnownGen)
			if got != tc.want {
				t.Fatalf("decideClaim = %v, want %v", got, tc.want)
			}
		})
	}
}
