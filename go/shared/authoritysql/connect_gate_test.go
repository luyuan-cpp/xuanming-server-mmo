package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
)

// ConnectGate 单测(docs/design/data-service-breaker-per-logical-db.md §7):
// 沿用 placement_test.go / backend_test.go / catalog_succession_test.go 的假驱动,不连真库。

func TestConnectGateRejectSkipsDialAndReturnsGateError(t *testing.T) {
	gateErr := errors.New("logical database mmorpg_global_01: circuit breaker is open")
	dialed := 0
	var gotDB string
	inner := &backendFakeConnector{conn: &backendFakeConn{answer: healthyMySQL().answer}}
	c := connector{
		inner:      inner,
		backend:    BackendMySQL,
		permission: Reader(),
		logicalDB:  "mmorpg_global_01",
		connectGate: func(_ context.Context, logicalDB string) (func(error), error) {
			gotDB = logicalDB
			return func(error) { t.Fatal("settle must not be called when the gate rejects") }, gateErr
		},
		placed: &placedDialer{
			template:  mysql.NewConfig(),
			logicalDB: "mmorpg_global_01",
			resolver:  &fakeResolver{addr: placementAddrA},
			newInner: func(*mysql.Config) (driver.Connector, error) {
				dialed++
				return inner, nil
			},
		},
	}
	conn, err := c.Connect(context.Background())
	if !errors.Is(err, gateErr) || conn != nil {
		t.Fatalf("Connect() = (%v, %v), want (nil, %v)", conn, err, gateErr)
	}
	if gotDB != "mmorpg_global_01" {
		t.Fatalf("gate logicalDB = %q, want %q", gotDB, "mmorpg_global_01")
	}
	if dialed != 0 || inner.connects != 0 {
		t.Fatalf("gate rejection must not dial: placed=%d inner=%d", dialed, inner.connects)
	}
}

func TestConnectGateSettlesSuccessAndFailures(t *testing.T) {
	t.Run("success settles nil", func(t *testing.T) {
		var settled []error
		inner := &backendFakeConnector{conn: &backendFakeConn{answer: healthyMySQL().answer}}
		c := connector{
			inner:      inner,
			backend:    BackendMySQL,
			permission: Reader(),
			logicalDB:  "mmorpg_global_00",
			connectGate: func(_ context.Context, logicalDB string) (func(error), error) {
				if logicalDB != "mmorpg_global_00" {
					t.Fatalf("logicalDB = %q", logicalDB)
				}
				return func(err error) { settled = append(settled, err) }, nil
			},
		}
		conn, err := c.Connect(context.Background())
		if err != nil || conn == nil {
			t.Fatalf("Connect() = (%v, %v), want success", conn, err)
		}
		if len(settled) != 1 || settled[0] != nil {
			t.Fatalf("settled = %v, want [nil]", settled)
		}
	})

	t.Run("placement error settles non-nil", func(t *testing.T) {
		var settled []error
		cfg := mysql.NewConfig()
		cfg.DBName = "mmorpg_global_02"
		c := connector{
			backend:    BackendMySQL,
			permission: Reader(),
			logicalDB:  cfg.DBName,
			placed: &placedDialer{
				template:  cfg,
				logicalDB: cfg.DBName,
				resolver:  &fakeResolver{err: errors.New("etcd unavailable")},
				newInner: func(*mysql.Config) (driver.Connector, error) {
					t.Fatal("newInner must not run after placement failure")
					return nil, nil
				},
			},
			connectGate: func(_ context.Context, _ string) (func(error), error) {
				return func(err error) { settled = append(settled, err) }, nil
			},
		}
		if _, err := c.Connect(context.Background()); err == nil {
			t.Fatal("expected placement error")
		}
		if len(settled) != 1 || settled[0] == nil {
			t.Fatalf("settled = %v, want one non-nil error", settled)
		}
	})

	t.Run("dial error settles non-nil", func(t *testing.T) {
		var settled []error
		dialErr := errors.New("dial tcp: connection refused")
		c := connector{
			inner:      failingConnector{err: dialErr},
			backend:    BackendMySQL,
			permission: Reader(),
			logicalDB:  "mmorpg_global_03",
			connectGate: func(_ context.Context, _ string) (func(error), error) {
				return func(err error) { settled = append(settled, err) }, nil
			},
		}
		if _, err := c.Connect(context.Background()); err == nil {
			t.Fatal("expected dial error")
		}
		if len(settled) != 1 || settled[0] == nil {
			t.Fatalf("settled = %v, want one non-nil error", settled)
		}
	})
}

// 闸门冻结发生在已建好的连接上(beforeBegin / admitRead),不在建连阶段:
// 事务因冻结回 ErrWriterFrozen 时,建连钩子只看到这一次建连成功(settle(nil)),绝不把冻结计成后端失联。
func TestConnectGateDoesNotCountWriterFreezeAsConnectFailure(t *testing.T) {
	req := successionRegisterForTest(t, 1)
	rawReq, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	w := req.Writer
	grant := &databasepb.SqlWriterBootGrant{
		WriterKey:               w.WriterKey,
		InstanceId:              w.InstanceId,
		Generation:              w.Generation,
		AuthoritySchema:         req.AuthoritySchema,
		ProtectedSchemas:        req.ProtectedSchemas,
		PhysicalAuthoritySha256: req.DeploymentDigest,
		DeploymentId:            req.DeploymentId,
		SqlPrincipal:            w.SqlPrincipal,
		RegistrationDigest:      w.RegistrationDigest,
		LockPrimitive:           req.LockPrimitive,
		LockQualificationSha256: req.LockQualificationSha256,
		MaintenanceRunId:        req.MaintenanceRunId,
	}
	writerCfg := WriterConfig{boot: &writerBoot{grant: grant}}
	server := healthyMySQL()
	conn := &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
		switch {
		case query == "SELECT CURRENT_USER(),DATABASE()":
			return fakeColumns(2), []driver.Value{[]byte("slot_0@%"), []byte(req.AuthoritySchema)}, nil
		case strings.Contains(query, "route_meta"):
			// writable=0 表示搬库冻结窗口:pin 只看世代和 writer_epoch>0(建连通过),beforeBegin 判 writable!=1 回 ErrWriterFrozen。
			return fakeColumns(5), []driver.Value{w.Generation, int64(0), int64(7), int64(1), []byte("run-1")}, nil
		case strings.Contains(query, "writer_registry"):
			return fakeColumns(5), []driver.Value{w.Generation, w.InstanceId, w.SqlPrincipal, w.RegistrationDigest, int64(databasepb.SqlWriterRegistry_ACTIVE)}, nil
		case strings.Contains(query, "writer_lifecycle_receipt"):
			return fakeColumns(1), []driver.Value{rawReq}, nil
		default:
			return server.answer(query)
		}
	}}
	var settled []error
	db := sql.OpenDB(connector{
		inner:      &backendFakeConnector{conn: conn},
		backend:    BackendMySQL,
		permission: Writer(writerCfg),
		logicalDB:  req.AuthoritySchema,
		connectGate: func(_ context.Context, logicalDB string) (func(error), error) {
			if logicalDB != req.AuthoritySchema {
				t.Fatalf("logicalDB = %q, want %q", logicalDB, req.AuthoritySchema)
			}
			return func(err error) { settled = append(settled, err) }, nil
		},
	})
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.BeginTx(context.Background(), nil)
	if !errors.Is(err, ErrWriterFrozen) {
		t.Fatalf("BeginTx() err = %v, want ErrWriterFrozen", err)
	}
	if len(settled) != 1 || settled[0] != nil {
		t.Fatalf("writer freeze must settle connect as success: got %v", settled)
	}
}

func TestWithConnectGateSetsOption(t *testing.T) {
	var opts openOptions
	called := false
	WithConnectGate(func(context.Context, string) (func(error), error) {
		called = true
		return nil, nil
	})(&opts)
	if opts.connectGate == nil {
		t.Fatal("WithConnectGate did not populate connectGate")
	}
	_, _ = opts.connectGate(context.Background(), "mmorpg_global")
	if !called {
		t.Fatal("configured connectGate was not invoked")
	}
}

type failingConnector struct{ err error }

func (f failingConnector) Connect(context.Context) (driver.Conn, error) { return nil, f.err }
func (failingConnector) Driver() driver.Driver                          { return opaqueDriver{} }
