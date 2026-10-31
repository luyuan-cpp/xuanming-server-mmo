package playerbundle

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	comppb "proto/common/component"
	dbpb "proto/common/database"
	storepb "proto/player_store"
)

const assetKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET)

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
	if row.KindName != "PLAYER_STORE_KIND_ASSET" || row.DecodeNote != "" {
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
	unknown := Row{Kind: 99}
	unknown.Decode()
	if !strings.Contains(unknown.DecodeNote, "unknown kind") {
		t.Fatalf("unknown kind note: %+v", unknown)
	}
}

func TestAnalyzeReportsHeldAndFrozen(t *testing.T) {
	route := Route{FrozenRunID: "run-1"}
	rows := []Row{{Kind: ownerKind, SaveSeq: 7, SchemaGen: 3, HolderInstance: "scene-a"}}
	got := Analyze(route, rows)
	if !got.Online || !got.Frozen || got.SaveSeq != 7 || got.SchemaGen != 3 || len(got.Warnings) != 2 {
		t.Fatalf("analysis: %+v", got)
	}
	released := Analyze(Route{}, []Row{{Kind: ownerKind, HolderInstance: "scene-a", Released: 1}})
	if released.Online {
		t.Fatalf("released holder is not online: %+v", released)
	}
	missing := Analyze(Route{}, nil)
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
