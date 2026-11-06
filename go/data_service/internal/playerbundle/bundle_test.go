package playerbundle

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	comppb "proto/common/component"
	dbpb "proto/common/database"
	storepb "proto/player_store"
)

const assetKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_WALLET) // 货币与账本所在的行

func assetPayload(t *testing.T) []byte {
	t.Helper()
	raw, err := proto.Marshal(&dbpb.PlayerDatabase{Currency: &comppb.CurrencyComp{Values: []uint64{42}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeRowRendersPayloadAndKeepsBytes(t *testing.T) {
	payload := assetPayload(t)
	row := Row{Kind: assetKind, Payload: payload}
	row.Decode()
	if row.KindName != "PLAYER_STORE_KIND_WALLET" || row.DecodeNote != "" {
		t.Fatalf("decode: %+v", row)
	}
	if !strings.Contains(string(row.Decoded), "42") {
		t.Fatalf("decoded json misses the currency: %s", row.Decoded)
	}
	if string(row.Payload) != string(payload) {
		t.Fatal("payload bytes must stay untouched")
	}
}

func TestDecodeRowKeepsBundleUsableWhenPayloadIsBad(t *testing.T) {
	row := Row{Kind: assetKind, Payload: []byte{0xff}}
	row.Decode()
	if row.DecodeNote == "" || row.Decoded != nil {
		t.Fatalf("bad payload must only leave a note: %+v", row)
	}
	if !bytes.Equal(row.Payload, []byte{0xff}) {
		t.Fatal("无法解码的原始字节不得改变")
	}
	// 退役编号与未知编号都不能套用当前分行布局，导出包须保留原件供排查。
	payload := assetPayload(t)
	for _, tc := range []struct {
		name string
		kind int32
	}{
		{"未知编号", 99},
		{"退役热数据行", 1},
		{"退役资产行", 2},
		{"退役旧表行", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unknown := Row{Kind: tc.kind, Payload: bytes.Clone(payload)}
			unknown.Decode()
			if !strings.HasPrefix(unknown.DecodeNote, "unknown or retired kind:") || unknown.Decoded != nil {
				t.Fatalf("未知或退役编号只能留下诊断，不得按当前布局解码: %+v", unknown)
			}
			if !bytes.Equal(unknown.Payload, payload) {
				t.Fatal("未知或退役编号的原始字节不得改变")
			}
		})
	}
}

func TestAnalyzeReportsHeld(t *testing.T) {
	rows := []Row{{Kind: ownerKind, SaveSeq: 7, SchemaGen: 3, HolderInstance: "scene-a"}}
	got := Analyze(rows)
	if !got.Online || got.SaveSeq != 7 || got.SchemaGen != 3 || len(got.Warnings) != 1 {
		t.Fatalf("analysis: %+v", got)
	}
	released := Analyze([]Row{{Kind: ownerKind, HolderInstance: "scene-a", Released: 1}})
	if released.Online {
		t.Fatalf("released holder is not online: %+v", released)
	}
	missing := Analyze(nil)
	if len(missing.Warnings) != 1 {
		t.Fatalf("missing owner row must warn: %+v", missing)
	}
}

func TestBundleRoundTripKeepsSixtyFourBitIDs(t *testing.T) {
	const id = uint64(1<<63 + 5)
	raw, err := json.Marshal(&Bundle{Format: Format, PlayerID: id, Rows: []Row{{Kind: ownerKind, OwnerEpoch: id, SaveSeq: id, Payload: []byte{}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlayerID != id || got.Rows[0].OwnerEpoch != id || got.Rows[0].SaveSeq != id {
		t.Fatalf("ids changed: %+v", got)
	}
	if _, err := Parse([]byte(`{"format":"other"}`)); err == nil {
		t.Fatal("other format must be refused")
	}
}

func TestPlanImport(t *testing.T) {
	bundle := func(rows ...Row) *Bundle { return &Bundle{Format: Format, Rows: rows} }
	owner := Row{Kind: ownerKind, SaveSeq: 100, SchemaGen: 2}
	asset := Row{Kind: assetKind, Payload: []byte{1}}

	t.Run("keeps sequence and generation monotonic", func(t *testing.T) {
		plan, err := PlanImport(bundle(owner, asset), LocalOwner{SaveSeq: 500, SchemaGen: 1, Released: 1}, 3)
		if err != nil {
			t.Fatal(err)
		}
		if plan.SaveSeq != 500 || plan.SchemaGen != 2 || len(plan.Rows) != 1 || plan.Rows[0].Kind != assetKind {
			t.Fatalf("plan: %+v", plan)
		}
	})
	t.Run("never-claimed local role is importable", func(t *testing.T) {
		if _, err := PlanImport(bundle(owner), LocalOwner{Released: 1}, 2); err != nil {
			t.Fatal(err)
		}
	})

	refused := map[string]struct {
		bundle *Bundle
		local  LocalOwner
		gen    uint32
	}{
		"no owner row":     {bundle(asset), LocalOwner{Released: 1}, 3},
		"local role held":  {bundle(owner), LocalOwner{Holder: []byte("scene-a")}, 3},
		"bundle too new":   {bundle(owner), LocalOwner{Released: 1}, 1},
		"unknown row kind": {bundle(owner, Row{Kind: 99}), LocalOwner{Released: 1}, 3},
		"duplicate row":    {bundle(owner, asset, asset), LocalOwner{Released: 1}, 3},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := PlanImport(tc.bundle, tc.local, tc.gen); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
}
