package playerstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"db/internal/dbtest"

	storepb "proto/player_store"
	"shared/serviceauth"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func testServer(t *testing.T, db *sql.DB, resolve StorageResolver) *Server {
	t.Helper()
	server, err := NewServer(ServerOptions{
		Secret: testSecret,
		Open: func(_ context.Context, storageID uint32) (*sql.DB, error) {
			if storageID == 99 {
				return nil, errors.New("storage down")
			}
			return db, nil
		},
		Resolve:  resolve,
		KnownGen: testKnownGen,
		Limits:   DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func signed[T proto.Message](t *testing.T, method string, req T, caller, secret string) T {
	t.Helper()
	if err := serviceauth.Sign(method, req, caller, secret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	return req
}

func grpcCode(err error) codes.Code {
	return status.Code(err)
}

func TestNewServer_RejectsBadConfig(t *testing.T) {
	open := func(context.Context, uint32) (*sql.DB, error) { return nil, nil }
	resolve := func(context.Context, uint64) (uint32, error) { return 1, nil }
	if _, err := NewServer(ServerOptions{Secret: "short", Open: open, Resolve: resolve}); err == nil {
		t.Fatal("a short secret must be rejected")
	}
	if _, err := NewServer(ServerOptions{Secret: testSecret, Resolve: resolve}); err == nil {
		t.Fatal("a nil opener must be rejected")
	}
	if _, err := NewServer(ServerOptions{Secret: testSecret, Open: open}); err == nil {
		t.Fatal("a nil resolver must be rejected")
	}
}

// 没签名、签错、别的调用方,都不碰库。
func TestServer_DeniedCallersDoNotTouchTheDatabase(t *testing.T) {
	db, rec := dbtest.OpenTx(t, noQuery(t))
	server := testServer(t, db, func(context.Context, uint64) (uint32, error) { return 1, nil })
	ctx := context.Background()

	if _, err := server.SavePlayer(ctx, validSave()); grpcCode(err) != codes.PermissionDenied {
		t.Fatalf("unsigned save: %v", err)
	}
	badSecret := signed(t, methodSavePlayer, validSave(), sceneCaller, "fedcba9876543210fedcba9876543210")
	if _, err := server.SavePlayer(ctx, badSecret); grpcCode(err) != codes.PermissionDenied {
		t.Fatalf("wrong secret: %v", err)
	}
	wrongMethod := signed(t, methodLoadAndClaim, validSave(), sceneCaller, testSecret)
	if _, err := server.SavePlayer(ctx, wrongMethod); grpcCode(err) != codes.PermissionDenied {
		t.Fatalf("signature for another method: %v", err)
	}
	if _, err := server.LoadAndClaim(ctx, validClaim()); grpcCode(err) != codes.PermissionDenied {
		t.Fatalf("unsigned claim: %v", err)
	}
	if got := rec.Statements(); len(got) != 0 {
		t.Fatalf("statements = %v", got)
	}
}

// 落点在认领时查出来,写进响应;Save 用请求里的落点。
func TestServer_ClaimReturnsStorageAndSaveUsesIt(t *testing.T) {
	db, _ := dbtest.OpenTx(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		if strings.Contains(query, "FOR UPDATE") {
			return ownerValues(10, 5, 1, instanceA), nil
		}
		return nil, nil
	})
	var resolved uint64
	server := testServer(t, db, func(_ context.Context, player uint64) (uint32, error) {
		resolved = player
		return 42, nil
	})
	ctx := context.Background()
	claimReq := signed(t, methodLoadAndClaim, validClaim(), sceneCaller, testSecret)
	claimResp, err := server.LoadAndClaim(ctx, claimReq)
	if err != nil {
		t.Fatal(err)
	}
	if claimResp.GetStorageId() != 42 || resolved != 7 {
		t.Fatalf("storage=%d resolved=%d", claimResp.GetStorageId(), resolved)
	}
	saveReq := validSave()
	saveReq.StorageId = 42
	resp, err := server.SavePlayer(ctx, signed(t, methodSavePlayer, saveReq, sceneCaller, testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
		t.Fatalf("result = %v", resp.GetResult())
	}
}

// 实体已经没了的空行释放不带落点,由服务端查。
func TestServer_DetachedReleaseResolvesStorage(t *testing.T) {
	db, _ := dbtest.OpenTx(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		if strings.Contains(query, "FOR UPDATE") {
			return ownerValues(10, 5, 1, instanceA), nil
		}
		return nil, nil
	})
	detached := func() *storepb.SavePlayerRequest {
		req := validSave()
		req.StorageId = 0
		req.Rows = nil
		req.Release = true
		return signed(t, methodSavePlayer, req, sceneCaller, testSecret)
	}
	ctx := context.Background()

	var resolved uint64
	server := testServer(t, db, func(_ context.Context, player uint64) (uint32, error) {
		resolved = player
		return 42, nil
	})
	resp, err := server.SavePlayer(ctx, detached())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED || resolved != 7 {
		t.Fatalf("result=%v resolved=%d", resp.GetResult(), resolved)
	}

	// 查出来的落点确实被用上:99 号库打不开。
	down := testServer(t, db, func(context.Context, uint64) (uint32, error) { return 99, nil })
	if _, err := down.SavePlayer(ctx, detached()); grpcCode(err) != codes.Unavailable {
		t.Fatalf("resolved storage down: %v", err)
	}
	frozen := testServer(t, db, func(context.Context, uint64) (uint32, error) { return 0, ErrStorageFrozen })
	if _, err := frozen.SavePlayer(ctx, detached()); grpcCode(err) != codes.Unavailable {
		t.Fatalf("frozen: %v", err)
	}
}

// 带行的存盘不带落点,或者不是释放,都是状态机出错:拒绝,不查落点,不碰库。
func TestServer_MissingStorageIsOnlyForDetachedRelease(t *testing.T) {
	db, rec := dbtest.OpenTx(t, noQuery(t))
	server := testServer(t, db, func(context.Context, uint64) (uint32, error) {
		t.Fatal("must not resolve storage")
		return 0, nil
	})
	withRows := validSave()
	withRows.StorageId = 0
	withRows.Release = true
	notRelease := validSave()
	notRelease.StorageId = 0
	notRelease.Rows = nil
	for name, req := range map[string]*storepb.SavePlayerRequest{"with rows": withRows, "not a release": notRelease} {
		resp, err := server.SavePlayer(context.Background(), signed(t, methodSavePlayer, req, sceneCaller, testSecret))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_INVALID {
			t.Fatalf("%s: result = %v", name, resp.GetResult())
		}
	}
	if got := rec.Statements(); len(got) != 0 {
		t.Fatalf("statements = %v", got)
	}
}

// 库打不开、落点查不到、搬库冻结,都回 Unavailable,scene 退避重试。
func TestServer_UnavailableDependencies(t *testing.T) {
	db, rec := dbtest.OpenTx(t, noQuery(t))
	ctx := context.Background()
	frozen := testServer(t, db, func(context.Context, uint64) (uint32, error) { return 0, ErrStorageFrozen })
	if _, err := frozen.LoadAndClaim(ctx, signed(t, methodLoadAndClaim, validClaim(), sceneCaller, testSecret)); grpcCode(err) != codes.Unavailable {
		t.Fatalf("frozen: %v", err)
	}
	down := testServer(t, db, func(context.Context, uint64) (uint32, error) { return 99, nil })
	if _, err := down.LoadAndClaim(ctx, signed(t, methodLoadAndClaim, validClaim(), sceneCaller, testSecret)); grpcCode(err) != codes.Unavailable {
		t.Fatalf("storage down on claim: %v", err)
	}
	saveReq := validSave()
	saveReq.StorageId = 99
	if _, err := down.SavePlayer(ctx, signed(t, methodSavePlayer, saveReq, sceneCaller, testSecret)); grpcCode(err) != codes.Unavailable {
		t.Fatalf("storage down on save: %v", err)
	}
	if got := rec.Statements(); len(got) != 0 {
		t.Fatalf("statements = %v", got)
	}
}

// SQL 出错就是结果未知,回 Unavailable,不把明细带给调用方。
func TestServer_SqlErrorIsUnknownOutcome(t *testing.T) {
	db, _ := dbtest.OpenTx(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
		return nil, errors.New("Error 1146: secret table detail")
	})
	server := testServer(t, db, func(context.Context, uint64) (uint32, error) { return 1, nil })
	_, err := server.SavePlayer(context.Background(), signed(t, methodSavePlayer, validSave(), sceneCaller, testSecret))
	if grpcCode(err) != codes.Unavailable {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret table detail") {
		t.Fatalf("error details must not reach the caller: %v", err)
	}
}
