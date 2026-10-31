package businessdigest

import (
	"encoding/hex"
	"testing"
)

type column struct {
	name  string
	value []byte
	null  bool
}

func digestOf(cols []column) string {
	w := New()
	for _, c := range cols {
		w.Column(c.name, c.value, c.null)
	}
	return hex.EncodeToString(w.Sum())
}

// golden 值由独立实现按包注释的协议算出（Python hashlib + struct.pack(">Q")）。
// go/db 的维护计划与 data_service 的恢复证明都对已落库原件比对这一摘要；它变了，说明协议被改了。
func TestGoldenDigestIsStable(t *testing.T) {
	cols := []column{
		{"player_id", []byte("7"), false},
		{"fence_owner_epoch", []byte("8"), false},
		{"transform", []byte{}, false},
		{"currency", nil, true},
		{"fence_save_seq", []byte("90"), false},
		{"blob", []byte{0x00, 0x01, 0xff}, false},
		{"persist_schema_gen", []byte("1"), false},
	}
	const want = "d035d4bbe175a3c55d43a7ccaf4fdd5bd45d66578cfdb54497f3403d9907749a"
	if got := digestOf(cols); got != want {
		t.Fatalf("业务全列摘要协议漂移: got %s, want %s", got, want)
	}
}

func TestFenceColumnsDoNotAffectDigest(t *testing.T) {
	base := []column{{"player_id", []byte("7"), false}, {"fence_owner_epoch", []byte("8"), false}, {"fence_save_seq", []byte("90"), false}}
	promoted := []column{{"player_id", []byte("7"), false}, {"fence_owner_epoch", []byte("11"), false}, {"fence_save_seq", []byte("1"), false}}
	if digestOf(base) != digestOf(promoted) {
		t.Fatal("只抬围栏不得改变业务摘要")
	}
}

func TestNullEmptyAndBoundariesAreDistinct(t *testing.T) {
	null := digestOf([]column{{"a", nil, true}})
	empty := digestOf([]column{{"a", []byte{}, false}})
	if null == empty {
		t.Fatal("NULL 与空串必须得到不同摘要")
	}
	// 列名与取值边界移动不得撞出同一摘要。
	left := digestOf([]column{{"ab", []byte("c"), false}})
	right := digestOf([]column{{"a", []byte("bc"), false}})
	if left == right {
		t.Fatal("长度前缀必须区分字段边界")
	}
	if digestOf([]column{{"a", []byte("1"), false}, {"b", []byte("2"), false}}) == digestOf([]column{{"b", []byte("2"), false}, {"a", []byte("1"), false}}) {
		t.Fatal("物理列序属于协议，换序必须改变摘要")
	}
}
