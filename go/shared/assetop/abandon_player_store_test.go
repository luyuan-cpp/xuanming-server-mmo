package assetop

import (
	"testing"
	"time"

	componentpb "proto/common/component"
)

// 条件 3、4、5:写权行的 released 和账本出自同一次读。
func TestJudgeWithPlayerStore(t *testing.T) {
	const nowMs = uint64(1_000_000_000)
	loop := &Loop{cfg: LoopConfig{AbandonSafetyGap: 10 * time.Minute}}
	op := Op{OpID: 1, Stream: 1, StreamEpoch: 1, Seq: 5, LastSendMs: nowMs - uint64(time.Hour/time.Millisecond)}
	empty := &componentpb.PlayerAssetOpLedgerComp{}

	cases := []struct {
		name     string
		snapshot StoreLedger
		op       Op
		want     abandonSkip
	}{
		{"没有写权行:说不清,不放弃", StoreLedger{}, op, abandonSkipLedgerUnknown},
		{"持有者还没释放:算在线", StoreLedger{Found: true, Ledger: empty}, op, abandonSkipLocated},
		{"发送还不够久", StoreLedger{Found: true, Released: true, Ledger: empty},
			Op{OpID: 1, Stream: 1, StreamEpoch: 1, Seq: 5, LastSendMs: nowMs - 1}, abandonSkipRecent},
		{"已释放、够久、账本未见:放弃", StoreLedger{Found: true, Released: true, Ledger: empty}, op, 0},
		{"读到了存档却没有账本:说不清", StoreLedger{Found: true, Released: true}, op, abandonSkipLedgerUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := loop.judgeWithPlayerStore(tc.op, tc.snapshot, nowMs)
			if got != tc.want {
				t.Fatalf("skip = %v, want %v", got, tc.want)
			}
		})
	}
}
