package playerstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"db/internal/dbtest"

	storepb "proto/player_store"
)

// ownerValues 是写权行的一行查询结果。
func ownerValues(e, q uint64, gen uint32, holder []byte) *dbtest.Rows {
	return &dbtest.Rows{
		Columns: []string{"owner_epoch", "save_seq", "schema_gen", "holder_instance"},
		Values:  [][]driver.Value{{int64(e), int64(q), int64(gen), holder}},
	}
}

func openStore(t *testing.T, h dbtest.Handler) (*Store, *dbtest.Recorder) {
	t.Helper()
	db, rec := dbtest.OpenTx(t, h)
	store, err := New(db, testKnownGen, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return store, rec
}

// noQuery 用于「不该碰库」的用例。
func noQuery(t *testing.T) dbtest.Handler {
	return func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		t.Errorf("unexpected statement: %s", query)
		return nil, nil
	}
}

func validSave() *storepb.SavePlayerRequest {
	return &storepb.SavePlayerRequest{
		PlayerId:   7,
		StorageId:  1,
		OwnerEpoch: 10,
		SaveSeq:    6,
		SchemaGen:  1,
		InstanceId: instanceA,
		Rows: []*storepb.PlayerStoreRow{
			{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT, Payload: []byte("hot")},
			{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET, Payload: nil},
		},
	}
}

// 进事务前的确定性拒绝不碰库。
func TestSave_RejectedBeforeTransaction(t *testing.T) {
	store, rec := openStore(t, noQuery(t))
	big := make([]byte, DefaultLimits().MaxRowBytes+1)
	row := DefaultLimits().MaxRowBytes
	cases := []struct {
		name string
		mut  func(*storepb.SavePlayerRequest)
		want storepb.SavePlayerResult
	}{
		{"玩家为 0", func(r *storepb.SavePlayerRequest) { r.PlayerId = 0 }, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"代次为 0", func(r *storepb.SavePlayerRequest) { r.OwnerEpoch = 0 }, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"序号为 0", func(r *storepb.SavePlayerRequest) { r.SaveSeq = 0 }, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"实例为空", func(r *storepb.SavePlayerRequest) { r.InstanceId = nil }, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"实例过长", func(r *storepb.SavePlayerRequest) { r.InstanceId = make([]byte, maxInstanceBytes+1) }, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"kind 为写权行", func(r *storepb.SavePlayerRequest) {
			r.Rows[0].Kind = storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER
		}, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"kind 未知", func(r *storepb.SavePlayerRequest) { r.Rows[0].Kind = storepb.PlayerStoreKind(99) }, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"kind 重复", func(r *storepb.SavePlayerRequest) { r.Rows[1].Kind = r.Rows[0].Kind }, storepb.SavePlayerResult_SAVE_PLAYER_INVALID},
		{"单行超预算", func(r *storepb.SavePlayerRequest) { r.Rows[0].Payload = big }, storepb.SavePlayerResult_SAVE_PLAYER_TOO_LARGE},
		{"合计超预算", func(r *storepb.SavePlayerRequest) {
			r.Rows = []*storepb.PlayerStoreRow{
				{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT, Payload: make([]byte, row)},
				{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET, Payload: make([]byte, row)},
				{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_GROWTH, Payload: make([]byte, row)},
				{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE, Payload: []byte{1}},
			}
		}, storepb.SavePlayerResult_SAVE_PLAYER_TOO_LARGE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validSave()
			tc.mut(req)
			resp, err := store.Save(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetResult() != tc.want {
				t.Fatalf("result = %v, want %v", resp.GetResult(), tc.want)
			}
		})
	}
	if got := rec.Statements(); len(got) != 0 {
		t.Fatalf("statements = %v", got)
	}
}

// 被拒绝和幂等成功都只读写权行,不写、不提交,并带回库里的值。
func TestSave_NoWriteWithoutCommitDecision(t *testing.T) {
	cases := []struct {
		name  string
		owner *dbtest.Rows
		want  storepb.SavePlayerResult
	}{
		{"被接管", ownerValues(20, 9, 1, instanceB), storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER},
		{"同序号", ownerValues(10, 6, 1, instanceA), storepb.SavePlayerResult_SAVE_PLAYER_EQUAL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, rec := openStore(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
				return tc.owner, nil
			})
			resp, err := store.Save(context.Background(), validSave())
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetResult() != tc.want {
				t.Fatalf("result = %v, want %v", resp.GetResult(), tc.want)
			}
			if resp.GetRowOwnerEpoch() != uint64(tc.owner.Values[0][0].(int64)) || resp.GetRowSaveSeq() != uint64(tc.owner.Values[0][1].(int64)) {
				t.Fatalf("response must carry the row values: %+v", resp)
			}
			if rec.Contains("INSERT") || rec.Contains(dbtest.StatementCommit) {
				t.Fatalf("must not write: %v", rec.Statements())
			}
			if !rec.Contains(dbtest.StatementRollback) {
				t.Fatalf("must release the row lock: %v", rec.Statements())
			}
		})
	}
}

func TestSave_NoOwner(t *testing.T) {
	store, rec := openStore(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
		return &dbtest.Rows{Columns: []string{"owner_epoch", "save_seq", "schema_gen", "holder_instance"}}, nil
	})
	resp, err := store.Save(context.Background(), validSave())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_NO_OWNER {
		t.Fatalf("result = %v", resp.GetResult())
	}
	if rec.Contains("INSERT") {
		t.Fatal("Save must never create the owner row")
	}
}

// 写权行在锁之下读,写权行和脏行一条语句提交;nil payload 绑定成非 nil 的空切片,否则驱动写 NULL。
func TestSave_CommitsOwnerAndRowsTogether(t *testing.T) {
	var insert []driver.NamedValue
	store, rec := openStore(t, func(_ context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		if strings.Contains(query, "FOR UPDATE") {
			return ownerValues(9, 5, 1, instanceA), nil
		}
		if strings.HasPrefix(query, "INSERT") {
			insert = args
		}
		return nil, nil
	})
	req := validSave()
	req.Release = true
	resp, err := store.Save(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
		t.Fatalf("result = %v", resp.GetResult())
	}
	if resp.GetRowOwnerEpoch() != 10 || resp.GetRowSaveSeq() != 6 {
		t.Fatalf("response = %+v, want the committed values", resp)
	}
	got := rec.Statements()
	if len(got) != 4 || got[0] != dbtest.StatementBegin || !strings.Contains(got[1], "FOR UPDATE") ||
		!strings.HasPrefix(got[2], "INSERT") || got[3] != dbtest.StatementCommit {
		t.Fatalf("statements = %v", got)
	}
	if len(insert) != 24 {
		t.Fatalf("insert args = %d, want 3 rows of 8", len(insert))
	}
	// 写权行:E 抬到请求值,released 按请求写,payload 为空而不是 NULL。
	if insert[1].Value != int64(0) || insert[2].Value != int64(10) || insert[3].Value != int64(6) || insert[6].Value != int64(1) {
		t.Fatalf("owner row args = %v", insert[:8])
	}
	if payload, ok := insert[7].Value.([]byte); !ok || payload == nil || len(payload) != 0 {
		t.Fatalf("owner payload = %#v, want a non-nil empty slice", insert[7].Value)
	}
	// 第二个数据行的 payload 是 nil,同样要归一。
	if payload, ok := insert[23].Value.([]byte); !ok || payload == nil || len(payload) != 0 {
		t.Fatalf("nil payload = %#v, want a non-nil empty slice", insert[23].Value)
	}
	if holder, ok := insert[13].Value.([]byte); !ok || holder == nil || len(holder) != 0 {
		t.Fatalf("data row holder = %#v, want a non-nil empty slice", insert[13].Value)
	}
}

// 空行存盘(下线快路径的 release)也要写写权行。
func TestSave_EmptyRowsStillWritesOwner(t *testing.T) {
	store, rec := openStore(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		if strings.Contains(query, "FOR UPDATE") {
			return ownerValues(10, 5, 1, instanceA), nil
		}
		return nil, nil
	})
	req := validSave()
	req.Rows = nil
	req.Release = true
	resp, err := store.Save(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
		t.Fatalf("result = %v", resp.GetResult())
	}
	if !rec.Contains("INSERT") || !rec.Contains(dbtest.StatementCommit) {
		t.Fatalf("statements = %v", rec.Statements())
	}
}

// 任何一步出错都返回错误(结果未知),不伪装成明确结局,也不提交。
func TestSave_ErrorsAreUnknownOutcome(t *testing.T) {
	boom := errors.New("connection lost")
	cases := []struct {
		name   string
		failOn string
	}{
		{"读写权行", "FOR UPDATE"},
		{"写入", "INSERT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, rec := openStore(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
				if strings.Contains(query, tc.failOn) {
					return nil, boom
				}
				if strings.Contains(query, "FOR UPDATE") {
					return ownerValues(10, 5, 1, instanceA), nil
				}
				return nil, nil
			})
			resp, err := store.Save(context.Background(), validSave())
			if !errors.Is(err, boom) || resp != nil {
				t.Fatalf("resp=%+v err=%v", resp, err)
			}
			if rec.Contains(dbtest.StatementCommit) {
				t.Fatalf("must not commit: %v", rec.Statements())
			}
		})
	}
}

func validClaim() *storepb.LoadAndClaimRequest {
	return &storepb.LoadAndClaimRequest{PlayerId: 7, OwnerEpoch: 11, InstanceId: instanceA, BinarySchemaGen: testKnownGen}
}

func TestLoadAndClaim_RejectedBeforeTransaction(t *testing.T) {
	store, rec := openStore(t, noQuery(t))
	for name, mut := range map[string]func(*storepb.LoadAndClaimRequest){
		"玩家为 0": func(r *storepb.LoadAndClaimRequest) { r.PlayerId = 0 },
		"代次为 0": func(r *storepb.LoadAndClaimRequest) { r.OwnerEpoch = 0 },
		"实例为空":  func(r *storepb.LoadAndClaimRequest) { r.InstanceId = nil },
	} {
		req := validClaim()
		mut(req)
		resp, err := store.LoadAndClaim(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetResult() != storepb.LoadAndClaimResult_LOAD_AND_CLAIM_INVALID {
			t.Fatalf("%s: result = %v", name, resp.GetResult())
		}
	}
	if got := rec.Statements(); len(got) != 0 {
		t.Fatalf("statements = %v", got)
	}
}

// 认领成功:先锁写权行,再抬代次,再读全部数据行,同一个事务。
func TestLoadAndClaim_ClaimsThenReadsInOneTransaction(t *testing.T) {
	var claimArgs []driver.NamedValue
	store, rec := openStore(t, func(_ context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "FOR UPDATE"):
			return ownerValues(10, 5, 2, instanceB), nil
		case strings.HasPrefix(query, "UPDATE"):
			claimArgs = args
			return nil, nil
		case strings.Contains(query, "kind>0"):
			return &dbtest.Rows{Columns: []string{"kind", "payload"}, Values: [][]driver.Value{
				{int64(storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET), []byte("asset")},
				{int64(storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT), []byte("hot")},
			}}, nil
		}
		return nil, nil
	})
	resp, err := store.LoadAndClaim(context.Background(), validClaim())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED {
		t.Fatalf("result = %v", resp.GetResult())
	}
	if resp.GetOwnerEpoch() != 11 || resp.GetSaveSeq() != 5 || resp.GetSchemaGen() != 2 {
		t.Fatalf("response = %+v", resp)
	}
	// 片段按 kind 排好序,调用方拼接的结果才确定。
	if len(resp.GetRows()) != 2 || resp.GetRows()[0].GetKind() != storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT ||
		string(resp.GetRows()[1].GetPayload()) != "asset" {
		t.Fatalf("rows = %v", resp.GetRows())
	}
	if claimArgs[0].Value != int64(11) || string(claimArgs[1].Value.([]byte)) != string(instanceA) {
		t.Fatalf("claim args = %v", claimArgs)
	}
	got := rec.Statements()
	if len(got) != 5 || got[0] != dbtest.StatementBegin || got[4] != dbtest.StatementCommit {
		t.Fatalf("statements = %v", got)
	}
}

// 被拒绝时不抬代次、不读数据。
func TestLoadAndClaim_RejectionDoesNotTouchRows(t *testing.T) {
	store, rec := openStore(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
		return ownerValues(20, 5, 1, instanceB), nil
	})
	resp, err := store.LoadAndClaim(context.Background(), validClaim())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.LoadAndClaimResult_LOAD_AND_CLAIM_STALE_OWNER || resp.GetOwnerEpoch() != 20 {
		t.Fatalf("response = %+v", resp)
	}
	if rec.Contains("UPDATE player_store") || rec.Contains("kind>0") || rec.Contains(dbtest.StatementCommit) {
		t.Fatalf("statements = %v", rec.Statements())
	}
}

func TestVerifySchema(t *testing.T) {
	columns := func(names ...string) dbtest.Handler {
		return func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
			out := &dbtest.Rows{Columns: []string{"COLUMN_NAME"}}
			for _, name := range names {
				out.Values = append(out.Values, []driver.Value{name})
			}
			return out, nil
		}
	}
	store, _ := openStore(t, columns("player_id", "kind", "owner_epoch", "save_seq", "schema_gen", "holder_instance", "released", "PAYLOAD"))
	if err := store.VerifySchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	missingColumn, _ := openStore(t, columns("player_id", "kind"))
	if err := missingColumn.VerifySchema(context.Background()); err == nil {
		t.Fatal("missing columns must be rejected")
	}
	noTable, _ := openStore(t, columns())
	if err := noTable.VerifySchema(context.Background()); err == nil {
		t.Fatal("missing table must be rejected")
	}
}

func TestNew_RejectsBadArguments(t *testing.T) {
	db, _ := dbtest.OpenTx(t, noQuery(t))
	if _, err := New(nil, 1, DefaultLimits()); err == nil {
		t.Fatal("nil db must be rejected")
	}
	if _, err := New(db, 1, Limits{}); err == nil {
		t.Fatal("zero limits must be rejected")
	}
	if _, err := New(db, 1, Limits{MaxRowBytes: 10, MaxRequestBytes: 5}); err == nil {
		t.Fatal("request budget below the row budget must be rejected")
	}
	if _, err := New(db, 1, Limits{MaxRowBytes: 2 << 20, MaxRequestBytes: 3 << 20}); err == nil {
		t.Fatal("a row budget whose claim response cannot fit must be rejected")
	}
}

// C++ 收端的上限和这里要同一个值:这里算得比那边大,scene 收不下,角色就再也进不来了。
func TestMaxClaimResponseBytes_MatchesCpp(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "cpp", "libs", "engine", "config", "player_store_path.h"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("kMaxClaimResponseBytes = %d * 1024 * 1024", MaxClaimResponseBytes>>20)
	if !strings.Contains(string(raw), want) {
		t.Fatalf("player_store_path.h must declare %q", want)
	}
}
