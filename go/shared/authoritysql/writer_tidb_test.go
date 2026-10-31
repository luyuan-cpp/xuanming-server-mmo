package authoritysql_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"shared/drainproof"
)

func fixtureID(t *testing.T) []byte {
	t.Helper()
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return id
}

func authorityFixture(t *testing.T) (context.Context, *sqltest.Fixture, sqltest.Options) {
	t.Helper()
	dsn := os.Getenv("MMORPG_SQL_FIXTURE_ADMIN_DSN")
	if dsn == "" {
		if os.Getenv("MAIL_RANK_REQUIRE_TESTS") == "1" || os.Getenv("MAIL_RANK_TEST_TIDB") != "" {
			t.Fatal("真实权限夹具必须显式提供本任务MMORPG_SQL_FIXTURE_ADMIN_DSN")
		}
		t.Skip("需本任务独占TiDB及真实权限资格原件")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	schema := "authority_it_" + hex.EncodeToString(fixtureID(t)[:8])
	opts, err := sqltest.OptionsFromEnv(dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, fixture, opts
}

// 原物理池断言保持四条独占连接和全部强会话变量；DDL/主体清理由共享owned fixture负责。
func TestPhysicalTiDBConnectionsPinStrongSession(t *testing.T) {
	ctx, fixture, opts := authorityFixture(t)
	cfg, err := mysql.ParseDSN(fixture.DSN(opts.AuthoritySchema))
	if err != nil {
		t.Fatal("fixture连接配置无效")
	}
	cfg.Params = map[string]string{"tidb_read_consistency": "'weak'", "tidb_snapshot": "'2000-01-01 00:00:00'", "tidb_read_staleness": "-5", "tidb_replica_read": "'follower'", "transaction_isolation": "'REPEATABLE-READ'"}
	db, err := authoritysql.Open(ctx, cfg.FormatDSN(), opts.AuthoritySchema, authoritysql.BackendTiDB, 4, fixture.Permission)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	for i := 0; i < 4; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		var mode, isolation, replica, consistency, snapshot string
		var stale, low int64
		if err = conn.QueryRowContext(ctx, "SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot").Scan(&mode, &isolation, &stale, &low, &replica, &consistency, &snapshot); err != nil {
			t.Fatal(err)
		}
		if mode != "pessimistic" || isolation != "READ-COMMITTED" || stale != 0 || low != 0 || replica != "leader" || consistency != "strict" || snapshot != "" {
			t.Fatalf("物理连接%d未固定强会话", i)
		}
	}
}

// 仅验证SQL控制协议。唯一发行者由本测试同步拥有，无外部Kafka/Redis队列；不能充当生产排空证据。
func TestTiDBSameRunRetirementAndFreeze(t *testing.T) {
	for _, withRelay := range []bool{false, true} {
		name := "P1直接写者"
		if withRelay {
			name = "P3同boot中继"
		}
		t.Run(name, func(t *testing.T) {
			ctx, fixture, opts := authorityFixture(t)
			meta, err := fixture.Catalog.CurrentMeta(ctx)
			if err != nil {
				t.Fatal(err)
			}
			runID := fixtureID(t)
			root := drainproof.New()
			relay := drainproof.New()
			sourcePath := filepath.Join(t.TempDir(), "fixture-inventory.txt")
			source := []byte("本用例同步拥有唯一SQL发行者，不启动外部发布器或队列消费者。")
			if err = os.WriteFile(sourcePath, source, 0600); err != nil {
				t.Fatal(err)
			}
			sourceDigest := sha256.Sum256(source)
			seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: fixtureID(t), InstanceId: fixture.Config.InstanceID(), PhysicalWriterKey: fixture.Config.WriterKey(), LogicalRole: "SOURCE", PermanentRevivalFenceId: fixtureID(t), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: sourcePath, Sha256: sourceDigest[:]}}}
			plan := &pb.SqlWriterMaintenanceReceipt_Plan{PhysicalClusterIdentity: opts.PhysicalAuthoritySHA256, AuthoritySchema: opts.AuthoritySchema, ProtectedSchemas: opts.ProtectedSchemas, RunId: runID, BeforeEpoch: meta.WriterEpoch, CatalogRevision: meta.CatalogRevision, ActiveWriterKeys: [][]byte{fixture.Config.WriterKey()}, RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{seed}}
			var relaySeed *pb.SqlWriterMaintenanceReceipt_IssuerClosure
			if withRelay {
				relaySeed = proto.Clone(seed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
				relaySeed.IssuerId = fixtureID(t)
				relaySeed.LogicalRole = "RELAY"
				plan.QueueMutators = []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{relaySeed}
			}
			planRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			manifest := sha256.Sum256(planRaw)
			var previous *pb.SqlWriterMaintenanceReceipt
			request := func(phase pb.SqlWriterDrainRun_Phase, kind pb.SqlWriterMaintenanceReceipt_Kind) *pb.SqlWriterMaintenanceReceipt {
				m, e := fixture.Catalog.CurrentMeta(ctx)
				if e != nil {
					t.Fatal(e)
				}
				r := &pb.SqlWriterMaintenanceReceipt{RequestId: fixtureID(t), RunId: runID, Generation: fixture.Generation, BeforeEpoch: m.WriterEpoch, AfterEpoch: m.WriterEpoch, CatalogRevision: m.CatalogRevision, PlanProto: planRaw, Kind: kind, RunPhase: phase, AttemptRevision: 1, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: runID, ManifestDigest: manifest[:]}}
				if previous != nil {
					r.PriorReceiptId = previous.RequestId
					r.PriorReceiptDigest = previous.ReceiptDigest
				}
				return r
			}
			advance := func(r *pb.SqlWriterMaintenanceReceipt) {
				sealed, e := authoritysql.SealMaintenanceRequest(r)
				if e != nil {
					t.Fatal(e)
				}
				next := &pb.SqlWriterDrainRun{OperationId: runID, RequestDigest: sealed.RequestDigest, Generation: fixture.Generation, AttemptRevision: 1, ManifestProto: planRaw, ManifestDigest: manifest[:], Phase: r.RunPhase}
				previous, e = fixture.Catalog.Advance(ctx, next, sealed)
				if e != nil {
					t.Fatal(e)
				}
			}
			advance(request(pb.SqlWriterDrainRun_PLAN_FIXED, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT))
			// 本测试发行者只做同步元数据读取；Enter/完成/关闭用同一Session线性化。
			done, err := root.Enter()
			if err != nil {
				t.Fatal(err)
			}
			normal, err := fixture.Open(ctx, opts.AuthoritySchema, 2)
			if err != nil {
				t.Fatal(err)
			}
			var one int
			if err = normal.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
				root.Hold()
				t.Fatal(err)
			}
			done()
			root.CloseAdmission()
			closed, err := root.Closure(ctx, seed)
			if err != nil {
				t.Fatal(err)
			}
			rootFact := request(pb.SqlWriterDrainRun_ROOTS_CLOSED, pb.SqlWriterMaintenanceReceipt_ROLE_CLOSED)
			rootFact.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Publication{Publication: &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{closed}}}
			advance(rootFact)
			var registrationRaw []byte
			if err = fixture.Operator.QueryRowContext(ctx, "SELECT request_proto FROM writer_lifecycle_receipt WHERE kind=1 AND request_digest=(SELECT registration_digest FROM writer_registry WHERE writer_key=?)", fixture.Config.WriterKey()).Scan(&registrationRaw); err != nil {
				t.Fatal(err)
			}
			retire := &pb.SqlWriterLifecycleReceipt_Request{}
			if err = proto.Unmarshal(registrationRaw, retire); err != nil {
				t.Fatal(err)
			}
			retire.RequestId = fixtureID(t)
			retire.Kind = pb.SqlWriterLifecycleReceipt_RETIRE
			retire.MaintenanceRunId = runID
			retire.ExpectedEpoch = meta.WriterEpoch
			retire.ExpectedCatalogRevision = meta.CatalogRevision
			if withRelay {
				if _, err = fixture.Catalog.Retire(ctx, retire); err == nil {
					t.Fatal("P1不能提前退役兼任relay的同boot")
				}
				advance(request(pb.SqlWriterDrainRun_DRAINING, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT))
				relay.CloseAdmission()
				relayClosed, e := relay.Closure(ctx, relaySeed)
				if e != nil {
					t.Fatal(e)
				}
				relayFact := request(pb.SqlWriterDrainRun_DRAINING, pb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED)
				relayFact.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Publication{Publication: &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{relayClosed}}}
				advance(relayFact)
			}
			if err = normal.Close(); err != nil {
				t.Fatal(err)
			}
			fixture.Config.Seal()
			retired, err := fixture.Catalog.Retire(ctx, retire)
			if err != nil {
				t.Fatal(err)
			}
			if retired.CatalogRevision != meta.CatalogRevision+1 || retired.ResultingEpoch != meta.WriterEpoch {
				t.Fatal("退役只增加目录版本，不改变E")
			}
			if _, err = fixture.Open(ctx, opts.AuthoritySchema, 1); err == nil {
				t.Fatal("旧boot封闭后仍能新建业务池")
			}
			if !withRelay {
				advance(request(pb.SqlWriterDrainRun_DRAINING, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT))
			}
			closedFact := request(pb.SqlWriterDrainRun_NON_SQL_CLOSED, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT)
			closedFact.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_NonSqlClosure{NonSqlClosure: &pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence{}}
			advance(closedFact)
			nonSQL := previous
			ticketID := fixtureID(t)
			freeze := request(pb.SqlWriterDrainRun_SQL_FROZEN, pb.SqlWriterMaintenanceReceipt_FREEZE)
			freeze.AfterEpoch++
			freeze.Phase = pb.SqlWriterMaintenanceReceipt_FROZEN
			freeze.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Ticket{Ticket: &pb.SqlWriterMaintenanceReceipt_TicketEvidence{TicketId: ticketID, AuthorityOperationId: runID, BoundRequestId: freeze.RequestId}}
			freeze, err = authoritysql.SealMaintenanceRequest(freeze)
			if err != nil {
				t.Fatal(err)
			}
			var now uint64
			if err = fixture.Operator.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&now); err != nil {
				t.Fatal(err)
			}
			ticket := &pb.SqlWriterFreezeTicket{TicketId: ticketID, OperationId: runID, AttemptRevision: 1, Generation: fixture.Generation, ManifestDigest: manifest[:], NonSqlReceiptDigest: nonSQL.ReceiptDigest, FreezeRequestId: freeze.RequestId, FreezeRequestDigest: freeze.RequestDigest, NotBeforeMs: now, ExpiresMs: now + 30000, State: pb.SqlWriterFreezeTicket_AVAILABLE, AuthorizationKind: pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE}
			ticketDigest, err := authoritysql.SealFreezeTicketRequest(ticket)
			if err != nil {
				t.Fatal(err)
			}
			grant := request(pb.SqlWriterDrainRun_FREEZE_AUTHORIZED, pb.SqlWriterMaintenanceReceipt_TICKET_GRANTED)
			grant.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Ticket{Ticket: &pb.SqlWriterMaintenanceReceipt_TicketEvidence{TicketId: ticketID, AuthorityOperationId: runID, BoundRequestId: freeze.RequestId, BoundRequestDigest: freeze.RequestDigest, TicketRequestDigest: ticketDigest}}
			grant, err = authoritysql.SealMaintenanceRequest(grant)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = fixture.Catalog.GrantTicket(ctx, ticket, grant); err != nil {
				t.Fatal(err)
			}
			frozen, err := fixture.Catalog.Freeze(ctx, ticketID, freeze)
			if err != nil {
				t.Fatal(err)
			}
			if frozen.AfterEpoch != meta.WriterEpoch+1 || !bytes.Equal(frozen.PlanProto, planRaw) {
				t.Fatal("空ACTIVE冻结也应单调加E并保留P0原件")
			}
			current, err := fixture.Catalog.CurrentMeta(ctx)
			if err != nil || current.Writable != 0 || current.CatalogRevision != retired.CatalogRevision {
				t.Fatalf("冻结目录或开关错误: %v", err)
			}
			// 换ticket参数不能借已存在的同request成功收据冒认另一个动作。
			if _, err = fixture.Catalog.Freeze(ctx, fixtureID(t), freeze); err == nil {
				t.Fatal("冻结认重未绑定原ticket")
			}
			read, err := fixture.Catalog.MaintenanceReceipt(ctx, freeze.RequestId)
			if err != nil || !proto.Equal(read, frozen) {
				t.Fatalf("原收据强读不一致: %v", err)
			}
		})
	}
}

// 只在测试驱动接缝建立屏障，不改生产围栏。RR快照固定在slot等待之前，
// 再由真实TiDB锁等待表确认阻塞；控制事务模拟freeze/release的E+2提交，不宣称完整维护验收。
type epochWaitProbe struct {
	schema        string
	snapshotReady chan struct{}
	slotSent      chan struct{}
	snapshotOnce  sync.Once
	slotOnce      sync.Once
	businessSent  atomic.Int64
}
type epochWaitConnector struct {
	inner driver.Connector
	probe *epochWaitProbe
}

func (c epochWaitConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c epochWaitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &epochWaitConn{Conn: raw, probe: c.probe}, nil
}

type epochWaitConn struct {
	driver.Conn
	probe *epochWaitProbe
}

func (c *epochWaitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, "SELECT writer_epoch FROM "+"`"+c.probe.schema+"\x60.route_meta WHERE id=1", nil)
	if err == nil {
		values := make([]driver.Value, len(rows.Columns()))
		err = rows.Next(values)
		if err == io.EOF {
			err = errors.New("缺route_meta")
		}
		err = errors.Join(err, rows.Close())
	}
	if err != nil {
		return nil, errors.Join(err, tx.Rollback())
	}
	c.probe.snapshotOnce.Do(func() { close(c.probe.snapshotReady) })
	return tx, nil
}
func (c *epochWaitConn) observe(query string) {
	if strings.Contains(query, "writer_slot") && (strings.HasPrefix(query, "UPDATE ") || strings.HasSuffix(query, "FOR UPDATE")) {
		c.probe.slotOnce.Do(func() { close(c.probe.slotSent) })
	}
	if strings.Contains(query, "race_business") && (strings.HasPrefix(query, "UPDATE ") || strings.HasPrefix(query, "INSERT ") || strings.HasPrefix(query, "DELETE ")) {
		c.probe.businessSent.Add(1)
	}
}
func (c *epochWaitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.observe(query)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *epochWaitConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.observe(query)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestTiDBOldEpochWaitingSlotCannotSendBusinessSQL(t *testing.T) {
	for _, primitive := range []pb.SqlWriterBootGrant_LockPrimitive{pb.SqlWriterBootGrant_LOCK_TOUCH, pb.SqlWriterBootGrant_SELECT_ONLY} {
		t.Run(primitive.String(), func(t *testing.T) {
			dsn := os.Getenv("MMORPG_SQL_FIXTURE_ADMIN_DSN")
			required := os.Getenv("MAIL_RANK_REQUIRE_TESTS") == "1" || os.Getenv("MAIL_RANK_TEST_TIDB") != ""
			if dsn == "" {
				if required {
					t.Fatal("缺本任务真实TiDB fixture")
				}
				t.Skip("需真实TiDB权限资格原件")
			}
			schema := "epoch_wait_" + hex.EncodeToString(fixtureID(t)[:8])
			opts, err := sqltest.OptionsFromEnv(dsn, schema)
			if err != nil {
				t.Fatal(err)
			}
			// 每个原语必须绑定它自己的真实最小权限证明，不能挪用另一种原语的资格摘要。
			qualification := os.Getenv("MMORPG_SQL_FIXTURE_" + primitive.String() + "_QUALIFICATION_SHA256")
			if qualification != "" {
				opts.LockQualificationSHA256, err = hex.DecodeString(qualification)
				if err != nil || len(opts.LockQualificationSHA256) != 32 {
					t.Fatal("锁原语资格摘要无效")
				}
			} else if opts.LockPrimitive != primitive {
				if required {
					t.Fatal("缺该锁原语的真实权限资格原件")
				}
				t.Skip("该锁原语尚无独立资格原件")
			}
			opts.LockPrimitive = primitive
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f, err := sqltest.New(ctx, t, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Schema.ExecContext(ctx, "CREATE TABLE race_business(id BIGINT PRIMARY KEY,value BIGINT NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			if _, err = f.Schema.ExecContext(ctx, "INSERT INTO race_business(id,value) VALUES(1,0)"); err != nil {
				t.Fatal(err)
			}
			if err = f.GrantTables(ctx, schema, "race_business"); err != nil {
				t.Fatal(err)
			}
			cfg, err := mysql.ParseDSN(f.DSN(schema))
			if err != nil {
				t.Fatal("fixture连接配置无效")
			}
			cfg.ReadTimeout = 20 * time.Second
			cfg.WriteTimeout = 20 * time.Second
			if cfg.Params == nil {
				cfg.Params = map[string]string{}
			}
			cfg.Params["innodb_lock_wait_timeout"] = "15"
			inner, err := mysql.NewConnector(cfg)
			if err != nil {
				t.Fatal(err)
			}
			probe := &epochWaitProbe{schema: schema, snapshotReady: make(chan struct{}), slotSent: make(chan struct{})}
			guarded, err := authoritysql.PinCurrentSession(epochWaitConnector{inner: inner, probe: probe}, authoritysql.BackendTiDB, f.Permission)
			if err != nil {
				t.Fatal(err)
			}
			business := sql.OpenDB(guarded)
			business.SetMaxOpenConns(1)
			defer business.Close()
			if err = business.PingContext(ctx); err != nil {
				t.Fatal(err)
			}
			meta, err := f.Catalog.CurrentMeta(ctx)
			if err != nil {
				t.Fatal(err)
			}
			controller, err := f.Operator.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer controller.Rollback()
			locked, err := controller.QueryContext(ctx, "SELECT slot FROM writer_slot WHERE writer_key=? ORDER BY slot FOR UPDATE", f.Config.WriterKey())
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for locked.Next() {
				var slot uint32
				if err = locked.Scan(&slot); err != nil {
					locked.Close()
					t.Fatal(err)
				}
				count++
			}
			err = errors.Join(locked.Err(), locked.Close())
			if err != nil || count != 64 {
				t.Fatalf("未锁全64槽: %d %v", count, err)
			}
			done := make(chan error, 1)
			go func() {
				tx, e := business.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
				if e == nil {
					_, e = tx.ExecContext(ctx, "UPDATE race_business SET value=value+1 WHERE id=1")
					e = errors.Join(e, tx.Rollback())
				}
				done <- e
			}()
			for _, ready := range []<-chan struct{}{probe.snapshotReady, probe.slotSent} {
				select {
				case <-ready:
				case e := <-done:
					t.Fatalf("尚未进入受控等待就终结: %v", e)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			// SQL往返就是观察屏障；不以sleep时长推定已经卡住。仅匹配本用例随机schema。
			observed := false
			for !observed {
				var waits int
				if err = f.Schema.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.DATA_LOCK_WAITS WHERE KEY_INFO LIKE ?", "%"+schema+"%").Scan(&waits); err != nil {
					t.Fatalf("无法核验本次真实TiDB锁等待: %v", err)
				}
				observed = waits > 0
				if !observed {
					select {
					case e := <-done:
						t.Fatalf("未观测到真实锁等待: %v", e)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					default:
					}
				}
			}
			if _, err = controller.ExecContext(ctx, "UPDATE writer_slot SET writer_epoch=? WHERE writer_key=?", meta.WriterEpoch+2, f.Config.WriterKey()); err != nil {
				t.Fatal(err)
			}
			if _, err = controller.ExecContext(ctx, "UPDATE route_meta SET writer_epoch=?,writable=1 WHERE id=1", meta.WriterEpoch+2); err != nil {
				t.Fatal(err)
			}
			if err = controller.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if !errors.Is(err, authoritysql.ErrWriterDenied) {
					t.Fatalf("旧E事务未被围栏拒绝: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if probe.businessSent.Load() != 0 {
				t.Fatalf("业务SQL已发送%d次", probe.businessSent.Load())
			}
			var value uint64
			if err = f.Operator.QueryRowContext(ctx, "SELECT value FROM race_business WHERE id=1").Scan(&value); err != nil || value != 0 {
				t.Fatalf("业务行发生变化: %d %v", value, err)
			}
		})
	}
}
