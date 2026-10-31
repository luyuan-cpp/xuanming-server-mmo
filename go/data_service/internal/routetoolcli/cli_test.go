package routetoolcli

// route_tool 入口的参数解析、装配与退出码。只钉「失败一律退出码 2、不 panic、不往 stdout 写东西」与装配顺序;
// 各子命令本身的逻辑在 internal/routetool 的用例里。TestRun* 用注入的 deps,不连真库。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"data_service/internal/config"
	"data_service/internal/routetool"
	"data_service/internal/store"

	pb "proto/common/database"
	"shared/authoritysql"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx"
)

// lockedBuffer 是并发安全的 bytes.Buffer:run 会把 go-zero logx 改写到 stderr,
// go-zero 的后台 goroutine 可能在 run 返回之后仍往里写。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runCLI 走生产装配(Run → defaultDeps)。
func runCLI(args ...string) (int, string, string) {
	var stdout, stderr lockedBuffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// runCLIWith 走注入的装配(不连真库)。
func runCLIWith(d deps, args ...string) (int, string, string) {
	var stdout, stderr lockedBuffer
	code := run(context.Background(), withMaintenanceShape(args...), &stdout, &stderr, d)
	return code, stdout.String(), stderr.String()
}

// withMaintenanceShape 只用于显式注入依赖的装配测试:补上过得了参数校验的维护参数,原件文件不存在也不会被打开。
func withMaintenanceShape(args ...string) []string {
	out := append([]string(nil), args...)
	if len(args) < 3 || (args[2] != subFreeze && args[2] != subUnfreeze) {
		return out
	}
	return append(out, "-maintenance-request="+filepath.Join(os.TempDir(), "route-cli-unit-request.json"),
		"-maintenance-sha256="+strings.Repeat("a", 64), "-ticket-id="+strings.Repeat("01", 16))
}

// TestRouteToolBadArgsExitTwo:缺 -f、缺 / 未知子命令、子命令不认的参数、取值越界、多余参数,一律退出码 2 并打印用法。
func TestRouteToolBadArgsExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing -f", []string{"tidb-prepare"}, "missing -f"},
		{"missing subcommand", []string{"-f", "x.yaml"}, "missing subcommand"},
		{"unknown subcommand", []string{"-f", "x.yaml", "export"}, "unknown subcommand"},
		{"import is gone", []string{"-f", "x.yaml", "import-from-redis"}, "unknown subcommand"},
		{"audit is gone", []string{"-f", "x.yaml", "audit"}, "unknown subcommand"},
		{"reconcile is gone", []string{"-f", "x.yaml", "reconcile-from-cache"}, "unknown subcommand"},
		{"rewarm is gone", []string{"-f", "x.yaml", "rewarm"}, "unknown subcommand"},
		{"zero timeout", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "-timeout=0s"}, "-timeout=0s"},
		{"extra positional argument", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "extra"}, "unexpected arguments"},
		{"tidb-prepare missing zones", []string{"-f", "x.yaml", "tidb-prepare"}, "-zones"},
		{"tidb-prepare zone zero", []string{"-f", "x.yaml", "tidb-prepare", "-zones=0"}, "-zones"},
		{"tidb-prepare zone over the max", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1000000"}, "-zones"},
		{"tidb-prepare zone not a number", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1,a"}, "-zones"},
		{"tidb-prepare empty zone item", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1,"}, "-zones"},
		{"tidb-prepare storage zero", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "-storages=0"}, "-storages"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(c.args...)
			assert.Equal(t, routetool.ExitFailed, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, c.want)
			assert.Contains(t, stderr, "用法:")
		})
	}
}

// TestRouteToolHelpExitsClean:-h 打印用法,退出码 0。
func TestRouteToolHelpExitsClean(t *testing.T) {
	code, stdout, stderr := runCLI("-h")
	assert.Equal(t, routetool.ExitClean, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "用法:")
}

// TestRunSubcommandFlagsAreScoped:子命令的参数互不串用(别的子命令的参数一律 flag provided but not defined),
// 取值越界同样退出码 2;-freeze-timeout 不得低于 store.RouteFreezeTimeoutMin。
func TestRunSubcommandFlagsAreScoped(t *testing.T) {
	maint := "-maintenance-request=" + filepath.Join(t.TempDir(), "request.json")
	sha := "-maintenance-sha256=" + strings.Repeat("a", 64)
	ticket := "-ticket-id=" + strings.Repeat("01", 16)
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"dry-run is not a freeze flag", []string{"-f", "x.yaml", "freeze", "-dry-run=false"}, "dry-run"},
		{"zones is tidb-prepare only", []string{"-f", "x.yaml", "unfreeze", "-zones=1"}, "zones"},
		{"reason is freeze only", []string{"-f", "x.yaml", "unfreeze", "-reason=x"}, "reason"},
		{"reason is not a tidb-prepare flag", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "-reason=x"}, "reason"},
		{"freeze-timeout is freeze / unfreeze only", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "-freeze-timeout=5s"}, "freeze-timeout"},
		{"maintenance-request is freeze / unfreeze only", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", maint}, "maintenance-request"},
		{"ticket-id is freeze / unfreeze only", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", ticket}, "ticket-id"},
		{"confirm-audited is gone", []string{"-f", "x.yaml", "unfreeze", "-confirm-audited"}, "confirm-audited"},
		{"acknowledged is gone", []string{"-f", "x.yaml", "unfreeze", "-cache-was-not-verified-acknowledged"}, "cache-was-not-verified-acknowledged"},
		{"freeze-timeout below the floor", []string{"-f", "x.yaml", "freeze", maint, sha, ticket, "-freeze-timeout=4s"}, "-freeze-timeout=4s"},
		{"unfreeze freeze-timeout below the floor", []string{"-f", "x.yaml", "unfreeze", maint, sha, ticket, "-freeze-timeout=1s"}, "-freeze-timeout=1s"},
		{"freeze ticket is not hex", []string{"-f", "x.yaml", "freeze", maint, sha, "-ticket-id=zz"}, "票据"},
		{"extra positional after freeze", []string{"-f", "x.yaml", "freeze", "now"}, "unexpected arguments"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(c.args...)
			assert.Equal(t, routetool.ExitFailed, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, c.want)
			assert.Contains(t, stderr, "用法:")
		})
	}
}

// TestRouteToolMissingConfigExitsTwo:配置读不到 → 退出码 2(不是 conf.MustLoad 的 log.Fatalf 退出 1)。
func TestRouteToolMissingConfigExitsTwo(t *testing.T) {
	code, stdout, stderr := runCLI("-f", filepath.Join(t.TempDir(), "absent.yaml"), "tidb-prepare", "-zones=1")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "absent.yaml")
}

// TestRouteToolInvalidRouteStoreExitsTwo:RouteStore 段按服务的口径校验,非法即退出码 2,不去开库。
func TestRouteToolInvalidRouteStoreExitsTwo(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t))+"RouteStore:\n  MergedZoneRefreshSec: -1\n")
	d := defaultDeps()
	d.openRouteStore = func(config.Config, authoritysql.Permission) (routeStore, error) {
		t.Fatal("an invalid config must not reach the global DB")
		return nil, nil
	}
	code, stdout, stderr := runCLIWith(d, "-f", path, "tidb-prepare", "-zones=1")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "invalid RouteStore config")
}

// TestRouteToolTiDBPrepareOnlyTouchesTheGlobalDB:两个地址都指向刚关掉的不同端口,经原装配接口核实际全局库目标与调用次数;
// 错误必须脱敏,不能靠泄露 MySQL 地址识别来源。名字缓存的 Redis 不连。
// SnapshotMySQL.Host 必须显式写:不写会回落默认 127.0.0.1:3306,本机可能真有 MySQL。
func TestRouteToolTiDBPrepareOnlyTouchesTheGlobalDB(t *testing.T) {
	redisAddr, mysqlAddr := twoClosedLocalAddrs(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, redisAddr)+fmt.Sprintf("SnapshotMySQL:\n  Host: %s\n", mysqlAddr))

	globalOpens := 0
	d := defaultDeps()
	openGlobal := d.openRouteStore
	d.openRouteStore = func(c config.Config, permission authoritysql.Permission) (routeStore, error) {
		globalOpens++
		assert.Equal(t, mysqlAddr, c.SnapshotMySQL.Host, "失败应来自本例指定的全局库")
		return openGlobal(c, permission)
	}
	code, stdout, stderr := runCLIWith(d, "-f", path, "tidb-prepare", "-zones=1,2")

	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Equal(t, 1, globalOpens, "必须真实进入全局库建池失败路径")
	assert.Contains(t, stderr, "cannot open the route tables on the global DB", "失败应来自全局库")
	assert.NotContains(t, stderr, mysqlAddr, "权威SQL物理连接失败不得泄露目标地址")
	assert.NotContains(t, stderr, redisAddr, "tidb-prepare 不该去连名字缓存的 Redis")
}

// TestRouteToolTiDBPrepareRealRunNeedsSchemaPrincipal:真执行要独立的 Schema 主体,缺了在开池之前退出码 2。
func TestRouteToolTiDBPrepareRealRunNeedsSchemaPrincipal(t *testing.T) {
	t.Setenv("MMORPG_SQL_SCHEMA_PRINCIPAL", "")
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
	d := defaultDeps()
	d.openRouteStore = func(config.Config, authoritysql.Permission) (routeStore, error) {
		t.Fatal("a real run without the schema principal must not open the global DB")
		return nil, nil
	}
	code, stdout, stderr := runCLIWith(d, "-f", path, "tidb-prepare", "-zones=1", "-dry-run=false")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.NotEmpty(t, stderr)
}

// TestRunTiDBPrepareUsesReaderAndClosesThePool:缺省 dry-run 走只读主体,入参原样交给 store,stdout 一行统计,跑完关池。
func TestRunTiDBPrepareUsesReaderAndClosesThePool(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
	st := &fakeStore{rep: store.RouteTiDBPrepareReport{Engine: "mysql", Version: "8.0.36", Skipped: true}}
	var permission authoritysql.Permission
	d := fakeDeps(&fakeCatalog{}, &fakeCloser{})
	d.openRouteStore = func(_ config.Config, p authoritysql.Permission) (routeStore, error) {
		permission = p
		return st, nil
	}

	code, stdout, stderr := runCLIWith(d, "-f", path, "tidb-prepare", "-zones=1,2", "-storages=1000000")

	assert.Equal(t, routetool.ExitClean, code, stderr)
	assert.Equal(t, authoritysql.Reader(), permission)
	require.Len(t, st.reqs, 1)
	assert.Equal(t, []uint32{1, 2}, st.reqs[0].Zones)
	assert.Equal(t, []uint32{1000000}, st.reqs[0].Storages)
	assert.True(t, st.reqs[0].DryRun, "dry-run is the default")
	assert.Equal(t, 1, strings.Count(stdout, "\n"), stdout)
	assert.Contains(t, stdout, "route_tidb_prepare engine=mysql")
	assert.Equal(t, 1, st.closed)
}

// TestRunPoolCloseFailureExitsTwo:关不掉连接算整次失败,不能报成功。
func TestRunPoolCloseFailureExitsTwo(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
	closeErr := errors.New("close failed")

	st := &fakeStore{rep: store.RouteTiDBPrepareReport{Engine: "mysql", Skipped: true}, closeErr: closeErr}
	d := fakeDeps(&fakeCatalog{}, &fakeCloser{})
	d.openRouteStore = func(config.Config, authoritysql.Permission) (routeStore, error) { return st, nil }
	code, _, stderr := runCLIWith(d, "-f", path, "tidb-prepare", "-zones=1")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Contains(t, stderr, "close failed")

	code, _, stderr = runCLIWith(fakeDeps(&fakeCatalog{}, &fakeCloser{err: closeErr}), "-f", path, "freeze")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Contains(t, stderr, "close failed")
}

// TestRunFreezeThenUnfreeze:各提交一次、stdout 恰好一行统计、stderr 第一行是连接目标、维护连接跑完就关;
// freeze / unfreeze 都不开业务的 route store(fakeDeps 里一开就报错)。
func TestRunFreezeThenUnfreeze(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t))+"SnapshotMySQL:\n  Host: 127.0.0.1:3999\n  DBName: mmorpg\n")
	catalog := &fakeCatalog{}
	db := &fakeCloser{}

	code, stdout, stderr := runCLIWith(fakeDeps(catalog, db), "-f", path, "freeze", "-reason=drill")
	require.Equal(t, routetool.ExitClean, code, stderr)
	assert.Equal(t, "route_freeze changed=true writable=0\n", stdout)
	assert.True(t, strings.HasPrefix(stderr, "route_tool freeze: global_db=127.0.0.1:3999/mmorpg\n"), stderr)
	assert.Contains(t, stderr, `FREEZE reason="drill"`)
	assert.Equal(t, 1, catalog.freezeCalls)
	assert.Equal(t, 1, db.closed)

	code, stdout, stderr = runCLIWith(fakeDeps(catalog, db), "-f", path, "unfreeze")
	require.Equal(t, routetool.ExitClean, code, stderr)
	assert.Equal(t, "route_unfreeze changed=true writable=1\n", stdout)
	assert.True(t, strings.HasPrefix(stderr, "route_tool unfreeze: global_db=127.0.0.1:3999/mmorpg\n"), stderr)
	assert.Equal(t, 1, catalog.releaseCalls)
	assert.Equal(t, 2, db.closed)
}

// TestRunCatalogFailureExitsTwo:Catalog 拒绝 → 退出码 2、不出统计行、连接照样关。
func TestRunCatalogFailureExitsTwo(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
	for _, sub := range []string{subFreeze, subUnfreeze} {
		db := &fakeCloser{}
		code, stdout, stderr := runCLIWith(fakeDeps(&fakeCatalog{err: errors.New("catalog denied")}, db), "-f", path, sub)
		assert.Equal(t, routetool.ExitFailed, code, sub)
		assert.Empty(t, stdout, sub)
		assert.Contains(t, stderr, "catalog denied", sub)
		assert.Equal(t, 1, db.closed, sub)
	}
}

// TestRunFreezeUnreachableDBExitsTwo:维护连接开不起来 → 退出码 2、不出统计行。
func TestRunFreezeUnreachableDBExitsTwo(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
	d := fakeDeps(&fakeCatalog{}, &fakeCloser{})
	d.openMaintenance = func(context.Context, config.Config, cliArgs) (*maintenanceInput, error) {
		return nil, errors.New("global SQL unreachable")
	}
	code, stdout, stderr := runCLIWith(d, "-f", path, "freeze", "-reason=it")

	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "global SQL unreachable")
}

// TestRunStdoutCarriesOnlyTheSummary:stdout 只有一行统计;Catalog 打的 logx 日志进 stderr,不进 stdout。
func TestRunStdoutCarriesOnlyTheSummary(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
	catalog := &fakeCatalog{logOnFreeze: "fake catalog: freeze log line"}

	code, stdout, stderr := runCLIWith(fakeDeps(catalog, &fakeCloser{}), "-f", path, "freeze")

	assert.Equal(t, routetool.ExitClean, code, stderr)
	assert.Equal(t, "route_freeze changed=true writable=0\n", stdout, "stdout 恰好一行统计")
	assert.Contains(t, stderr, "fake catalog: freeze log line", "logx 改写到了 stderr")
}

// TestRunProductionMaintenanceRequiresOriginalBeforeConnecting:默认入口没有维护原件就在参数阶段退出码 2。
func TestRunProductionMaintenanceRequiresOriginalBeforeConnecting(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
	for _, sub := range []string{subFreeze, subUnfreeze} {
		code, stdout, stderr := runCLI("-f", path, sub)
		assert.Equal(t, routetool.ExitFailed, code, sub)
		assert.Empty(t, stdout, sub)
		assert.Contains(t, stderr, "维护操作必须指定原件", sub)
	}
}

// TestRunProductionMaintenanceRejectsWrongDigestBeforeConnecting:默认入口原件摘要对不上,在连库之前退出码 2
// (全局库地址是刚关掉的端口,走到连库就会是另一条错误)。
func TestRunProductionMaintenanceRejectsWrongDigestBeforeConnecting(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t))+fmt.Sprintf("SnapshotMySQL:\n  Host: %s\n", closedLocalAddr(t)))
	original := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(original, []byte("{}"), 0o600))
	for _, sub := range []string{subFreeze, subUnfreeze} {
		code, stdout, stderr := runCLI("-f", path, sub, "-maintenance-request="+original,
			"-maintenance-sha256="+strings.Repeat("a", 64), "-ticket-id="+strings.Repeat("01", 16))
		assert.Equal(t, routetool.ExitFailed, code, sub)
		assert.Empty(t, stdout, sub)
		assert.Contains(t, stderr, "维护原件、部署身份或当前目录状态不匹配", sub)
	}
}

// ── 替身 ──────────────────────────────────────────────────────────────────────

// fakeStore 是 routeStore 的假实现。
type fakeStore struct {
	rep      store.RouteTiDBPrepareReport
	reqs     []store.RouteTiDBPrepareRequest
	closed   int
	closeErr error
}

func (f *fakeStore) PrepareTiDB(_ context.Context, req store.RouteTiDBPrepareRequest) (store.RouteTiDBPrepareReport, error) {
	f.reqs = append(f.reqs, req)
	return f.rep, nil
}

func (f *fakeStore) Close() error {
	f.closed++
	return f.closeErr
}

// fakeCatalog 隔离真正的 SQL 控制器。它只驱动装配,不证明权限、锁或永久收据。
type fakeCatalog struct {
	err          error
	freezeCalls  int
	releaseCalls int
	// logOnFreeze 非空时 Freeze 用 logx 打这一行(模拟 SQL 层自己的日志)。
	logOnFreeze string
}

func (c *fakeCatalog) Freeze(_ context.Context, _ []byte, request *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	c.freezeCalls++
	if c.logOnFreeze != "" {
		logx.Error(c.logOnFreeze)
	}
	return request, c.err
}

func (c *fakeCatalog) Release(_ context.Context, _ []byte, request *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	c.releaseCalls++
	return request, c.err
}

// fakeCloser 是维护连接的替身,记下关闭次数。
type fakeCloser struct {
	err    error
	closed int
}

func (c *fakeCloser) Close() error {
	c.closed++
	return c.err
}

// fakeDeps 用假 Catalog 与假连接跑 freeze / unfreeze;它们不该开业务的 route store,开了就报错。
func fakeDeps(catalog *fakeCatalog, db *fakeCloser) deps {
	return deps{
		openMaintenance: func(_ context.Context, _ config.Config, a cliArgs) (*maintenanceInput, error) {
			return &maintenanceInput{db: db, catalog: catalog, request: unitMaintenanceRequest(a.sub), ticketID: bytes.Repeat([]byte{1}, 16)}, nil
		},
		openRouteStore: func(config.Config, authoritysql.Permission) (routeStore, error) {
			return nil, errors.New("this test does not open the route store")
		},
	}
}

// unitMaintenanceRequest 造一份过得了 routetool 入参校验的请求;摘要与票据的核对在 openMaintenance / Catalog 里。
func unitMaintenanceRequest(sub string) *pb.SqlWriterMaintenanceReceipt {
	if sub == subUnfreeze {
		return &pb.SqlWriterMaintenanceReceipt{
			Kind: pb.SqlWriterMaintenanceReceipt_RELEASE,
			Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{Detail: &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{
				ReleaseReadiness: &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{},
			}},
		}
	}
	return &pb.SqlWriterMaintenanceReceipt{Kind: pb.SqlWriterMaintenanceReceipt_FREEZE}
}

// minimalYaml 是能被 conf.Load 接受的最小配置(同 internal/config 的 minimalYaml),NameCacheRedis.Host 由用例填。
const minimalYaml = `
Name: dataservice.rpc
ListenOn: 127.0.0.1:0
NameCacheRedis:
  Host: %s
  Type: node
`

func writeYaml(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data_service.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// closedLocalAddr 返回一个刚关掉的本机端口:连它必然被拒绝。
func closedLocalAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// twoClosedLocalAddrs 返回两个不同的、刚关掉的本机端口(两个监听同时开着才保证端口不同,拿到地址后一起关掉)。
func twoClosedLocalAddrs(t *testing.T) (string, string) {
	t.Helper()
	a, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	b, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	first, second := a.Addr().String(), b.Addr().String()
	require.NoError(t, a.Close())
	require.NoError(t, b.Close())
	return first, second
}
