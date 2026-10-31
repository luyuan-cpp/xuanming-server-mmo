package routetoolcli

// route_tool 入口的参数解析、模式门禁、装配与退出码。
//
// 前五个用例(TestRouteTool*)2026-10-18 从 cmd/route_tool/main_test.go 原样迁来,**名字不变**:前几批的验证命令按
// `-run "Route|…"` 选用例,改名会让它们在旧口径下静默消失(提案 §11.2「第 1b-1 批」的更正注列了新旧位置的对应)。
// 只钉「失败一律退出码 2、不 panic、不往 stdout 写东西」;各子命令本身的逻辑在 internal/routetool 的用例里。
// TestRun* 是第 1b-1 批新增的:新子命令的参数作用域、模式门禁、装配顺序与 stdout 纪律,装配用注入的 deps(不连真库)。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/routetool"
	"data_service/internal/store"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
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

// TestRouteToolBadArgsExitTwo:缺 -f、缺 / 未知子命令、-batch 越界、子命令不认的参数、多余参数,一律退出码 2 并打印用法。
func TestRouteToolBadArgsExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing -f", []string{"audit"}, "missing -f"},
		{"missing subcommand", []string{"-f", "x.yaml"}, "missing subcommand"},
		{"unknown subcommand", []string{"-f", "x.yaml", "export"}, "unknown subcommand"},
		{"batch zero", []string{"-f", "x.yaml", "audit", "-batch=0"}, "-batch=0"},
		{"batch over the store limit", []string{"-f", "x.yaml", "import-from-redis", "-batch=1001"}, "-batch=1001"},
		{"negative max-report", []string{"-f", "x.yaml", "import-from-redis", "-max-report=-1"}, "-max-report=-1"},
		{"zero timeout", []string{"-f", "x.yaml", "audit", "-timeout=0s"}, "-timeout=0s"},
		{"dry-run is import only", []string{"-f", "x.yaml", "audit", "-dry-run=false"}, "dry-run"},
		{"recheck flags are audit only", []string{"-f", "x.yaml", "import-from-redis", "-max-recheck=1"}, "max-recheck"},
		{"allow-db-first is import only", []string{"-f", "x.yaml", "audit", "-allow-db-first"}, "allow-db-first"},
		{"extra positional argument", []string{"-f", "x.yaml", "audit", "extra"}, "unexpected arguments"},
		// tidb-prepare(G1a):-zones 必填、逐项校验;它不认 import / audit 的参数,别的子命令也不认它的参数。
		{"tidb-prepare missing zones", []string{"-f", "x.yaml", "tidb-prepare"}, "-zones"},
		{"tidb-prepare zone zero", []string{"-f", "x.yaml", "tidb-prepare", "-zones=0"}, "-zones"},
		{"tidb-prepare zone over the max", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1000000"}, "-zones"},
		{"tidb-prepare zone not a number", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1,a"}, "-zones"},
		{"tidb-prepare empty zone item", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1,"}, "-zones"},
		{"tidb-prepare storage zero", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "-storages=0"}, "-storages"},
		{"batch is not a tidb-prepare flag", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "-batch=10"}, "batch"},
		{"zones is tidb-prepare only", []string{"-f", "x.yaml", "audit", "-zones=1"}, "zones"},
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

// TestRouteToolMissingConfigExitsTwo:配置读不到 → 退出码 2(不是 conf.MustLoad 的 log.Fatalf 退出 1)。
func TestRouteToolMissingConfigExitsTwo(t *testing.T) {
	code, stdout, stderr := runCLI("-f", filepath.Join(t.TempDir(), "absent.yaml"), "audit")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "absent.yaml")
}

// TestRouteToolUnreachableRedisExitsTwoWithoutPanic:mapping Redis 不可达 → 退出码 2,不 panic、不退出进程
// (MustNewRedis 会经 logx.Must 直接退出),也不去碰全局库。
func TestRouteToolUnreachableRedisExitsTwoWithoutPanic(t *testing.T) {
	addr := closedLocalAddr(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, addr))

	code, stdout, stderr := runCLI("-f", path, "audit")

	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, addr)
	assert.Contains(t, stderr, "unreachable")
}

// TestRouteToolRealImportUnderDBFirstNeedsExplicitFlag:yaml 是 RouteStore.Mode=db_first 时,真导入默认拒绝
// (退出码 2,在连 Redis / 库之前就拦下)—— db_first 下库才是真源,从缓存回填库方向是反的,只有提案 §7 P3 那一次合法。
// -allow-db-first 放行、dry-run 不受影响:这两种都越过这道闸,走到连 Redis(这里地址不可达 → unreachable)。
func TestRouteToolRealImportUnderDBFirstNeedsExplicitFlag(t *testing.T) {
	addr := closedLocalAddr(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, addr)+dbFirstYaml)

	t.Run("refused without the flag", func(t *testing.T) {
		code, stdout, stderr := runCLI("-f", path, "import-from-redis", "-dry-run=false")
		assert.Equal(t, routetool.ExitFailed, code)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "-allow-db-first")
		assert.NotContains(t, stderr, "unreachable", "必须在连 Redis 之前就拒绝")
	})
	for _, c := range []struct {
		name string
		args []string
	}{
		{"real import with the flag passes the gate", []string{"-f", path, "import-from-redis", "-dry-run=false", "-allow-db-first"}},
		{"dry-run passes the gate", []string{"-f", path, "import-from-redis"}},
		{"audit passes the gate", []string{"-f", path, "audit"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := defaultDeps()
			if strings.Contains(strings.Join(c.args, " "), "-dry-run=false") {
				// 本例隔离权限解析，只核原模式门禁与 Redis 连接顺序；缺 grant 的真实入口另有断言。
				d.resolvePermission = func(*cliArgs) (authoritysql.Permission, error) { return authoritysql.Reader(), nil }
				d.openCommand = unitRouteCommand
			}
			code, stdout, stderr := runCLIWith(d, c.args...)
			assert.Equal(t, routetool.ExitFailed, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "unreachable", "越过 db_first 闸之后才会去连 Redis")
			assert.NotContains(t, stderr, "a real import is refused")
		})
	}
}

// TestRouteToolTiDBPrepareDoesNotTouchRedis:tidb-prepare 只碰全局库,不连 mapping Redis。两个地址都指向刚关掉的
// 不同端口:经原装配接口核实际全局库目标与调用次数；错误必须脱敏，不能靠泄露 MySQL 地址识别来源。
// SnapshotMySQL.Host 必须显式写:不写会回落默认 127.0.0.1:3306,本机可能真有 MySQL。
func TestRouteToolTiDBPrepareDoesNotTouchRedis(t *testing.T) {
	redisAddr, mysqlAddr := twoClosedLocalAddrs(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, redisAddr)+fmt.Sprintf("SnapshotMySQL:\n  Host: %s\n", mysqlAddr))

	globalOpens, redisOpens := 0, 0
	d := defaultDeps()
	openGlobal := d.openRouteStore
	d.openRouteStore = func(c config.Config, permission authoritysql.Permission, frozen *pb.SqlWriterMaintenanceReceipt) (routeStore, error) {
		globalOpens++
		assert.Equal(t, mysqlAddr, c.SnapshotMySQL.Host, "失败应来自本例指定的全局库")
		return openGlobal(c, permission, frozen)
	}
	d.connectRedis = func(context.Context, redis.RedisConf) (routeRedis, error) {
		redisOpens++
		return nil, errors.New("tidb-prepare 不应装配 mapping Redis")
	}
	code, stdout, stderr := runCLIWith(d, "-f", path, "tidb-prepare", "-zones=1,2")

	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Equal(t, 1, globalOpens, "必须真实进入全局库建池失败路径")
	assert.Zero(t, redisOpens, "tidb-prepare 不得尝试连接 mapping Redis")
	assert.Contains(t, stderr, "cannot open the route tables on the global DB", "失败应来自全局库")
	assert.NotContains(t, stderr, mysqlAddr, "权威SQL物理连接失败不得泄露目标地址")
	assert.NotContains(t, stderr, redisAddr, "tidb-prepare 不该去连 mapping Redis")
	assert.NotContains(t, stderr, "unreachable", "那是 mapping Redis 的文案")
}

// 建池失败时 stderr 括号里的提示按原因分流(store.OpenRouteStoreHint,三个建 route store 的入口共用),
// 纯函数用例在 store 包的 TestOpenRouteStoreHintByCause。

// ── 第 1b-1 批:freeze / reconcile-from-cache / unfreeze 与 audit -frozen-recovery ──────────────

// TestRunSubcommandFlagsAreScoped:新子命令的参数互不串用(别的子命令的参数一律 flag provided but not defined),
// 取值越界同样退出码 2;-freeze-timeout 不得低于 store.RouteFreezeTimeoutMin。
func TestRunSubcommandFlagsAreScoped(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"dry-run is not a freeze flag", []string{"-f", "x.yaml", "freeze", "-dry-run=false"}, "dry-run"},
		{"batch is not a freeze flag", []string{"-f", "x.yaml", "freeze", "-batch=10"}, "batch"},
		{"resolve-ties is reconcile only", []string{"-f", "x.yaml", "audit", "-resolve-ties=cache"}, "resolve-ties"},
		{"freeze-timeout is freeze / unfreeze only", []string{"-f", "x.yaml", "audit", "-freeze-timeout=5s"}, "freeze-timeout"},
		{"frozen-recovery is audit only", []string{"-f", "x.yaml", "import-from-redis", "-frozen-recovery"}, "frozen-recovery"},
		{"reason is freeze only", []string{"-f", "x.yaml", "unfreeze", "-confirm-audited", "-reason=x"}, "reason"},
		{"freeze-grace is reconcile only", []string{"-f", "x.yaml", "unfreeze", "-confirm-audited", "-freeze-grace=1s"}, "freeze-grace"},
		{"confirm-audited is unfreeze only", []string{"-f", "x.yaml", "reconcile-from-cache", "-confirm-audited"}, "confirm-audited"},
		{"acknowledged is unfreeze only", []string{"-f", "x.yaml", "freeze", "-cache-was-not-verified-acknowledged"}, "cache-was-not-verified-acknowledged"},
		{"resolve-ties value", []string{"-f", "x.yaml", "reconcile-from-cache", "-resolve-ties=db"}, "-resolve-ties=db"},
		{"negative freeze-grace", []string{"-f", "x.yaml", "reconcile-from-cache", "-freeze-grace=-1s"}, "-freeze-grace=-1s"},
		{"freeze-timeout below the floor", []string{"-f", "x.yaml", "freeze", "-freeze-timeout=4s"}, "-freeze-timeout=4s"},
		{"unfreeze freeze-timeout below the floor", []string{"-f", "x.yaml", "unfreeze", "-confirm-audited", "-freeze-timeout=1s"}, "-freeze-timeout=1s"},
		{"reason is not a tidb-prepare flag", []string{"-f", "x.yaml", "tidb-prepare", "-zones=1", "-reason=x"}, "reason"},
		{"extra positional after freeze", []string{"-f", "x.yaml", "freeze", "now"}, "unexpected arguments"},
		// 2026-10-19:rewarm 在第 1b-2 批收尾时落地,原先这里钉的「rewarm is not a subcommand yet」改为钉它的参数作用域
		// (rewarm 自身的参数校验见 TestRunRewarmFlags)。
		{"wait is rewarm only", []string{"-f", "x.yaml", "audit", "-wait=1m"}, "wait"},
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

// TestRunReconcileAndUnfreezeRefusedInRedisMode:yaml 是 redis 模式时 reconcile-from-cache / audit -frozen-recovery
// 在连任何东西之前就退出码 2(库不是真源,没有可修的);freeze 允许,但打 WARN(redis 模式的 W1 不读 writable)。
// 2026-10-18 修复评审起 unfreeze 在 redis 模式下改为「没有 -cache-was-not-verified-acknowledged 才拒绝」(同样在连任何东西之前),
// 放行的那一半见 TestRunFreezeThenUnfreezeInRedisMode。
func TestRunReconcileAndUnfreezeRefusedInRedisMode(t *testing.T) {
	addr := closedLocalAddr(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, addr)) // 不写 RouteStore 段 = redis 模式
	for _, args := range [][]string{
		{"-f", path, "reconcile-from-cache"},
		{"-f", path, "reconcile-from-cache", "-dry-run=false"},
		{"-f", path, "unfreeze", "-confirm-audited"},
		{"-f", path, "audit", "-frozen-recovery"},
	} {
		t.Run(strings.Join(args[2:], " "), func(t *testing.T) {
			shaped := withMaintenanceShape(args...)
			if strings.Contains(strings.Join(args, " "), "-dry-run=false") {
				// 只补语法形状以到达原模式门禁；该门禁必须先于任何原件读取，不能制造票据或runtime收据。
				private := t.TempDir()
				shaped = append(shaped, "-drain-runtime-plan="+filepath.Join(private, "must-not-read-runtime.json"),
					"-drain-runtime-directory="+private, "-drain-runtime-sha256="+strings.Repeat("a", 64))
			}
			code, stdout, stderr := runCLI(shaped...)
			assert.Equal(t, routetool.ExitFailed, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "RouteStore.Mode=redis")
			assert.NotContains(t, stderr, "unreachable", "必须在连 Redis 之前就拒绝")
		})
	}

	t.Run("P6缺runtime先于模式门禁拒绝", func(t *testing.T) {
		code, stdout, stderr := runCLI(withMaintenanceShape("-f", path, "reconcile-from-cache", "-dry-run=false")...)
		assert.Equal(t, routetool.ExitFailed, code)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "P6恢复必须绑定P0预列runtime及同NORMAL票据")
		assert.NotContains(t, stderr, "RouteStore.Mode=redis", "缺P0原件不能越过更早的参数门禁")
		assert.NotContains(t, stderr, "unreachable", "缺P0原件不得尝试外部连接")
	})

	t.Run("freeze 允许_打WARN", func(t *testing.T) {
		st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 1}}
		code, stdout, stderr := runCLIWith(fakeDeps(st), "-f", path, "freeze", "-reason=drill")
		assert.Equal(t, routetool.ExitClean, code, stderr)
		assert.Equal(t, "route_freeze changed=true writable=0\n", stdout)
		assert.Contains(t, stderr, "WARN: RouteStore.Mode=redis")
		assert.Equal(t, 1, st.freezeCalls)
	})
}

// TestRunFreezeThenUnfreezeInRedisMode(2026-10-18 修复评审):redis 模式下误跑 / 演练过 freeze 之后,工具自己能撤销 ——
// import-from-redis 的 requireRouteWritable 与模式无关,没有这条路径就只能手写 UPDATE route_meta。
// 没有 -cache-was-not-verified-acknowledged 退出码 2、不解冻;加上之后解冻,最低水位 0;两次都不连 mapping Redis
// (地址不可达,连了就是 unreachable)、不读身份(fakeDeps 的身份读取会报错)。MappingRedis.Type 不是 node 也不挡(不读标记)。
func TestRunFreezeThenUnfreezeInRedisMode(t *testing.T) {
	addr := closedLocalAddr(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, addr)) // 不写 RouteStore 段 = redis 模式
	st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 1}}

	code, _, stderr := runCLIWith(fakeDeps(st), "-f", path, "freeze", "-reason=drill")
	require.Equal(t, routetool.ExitClean, code, stderr)
	require.Equal(t, uint64(0), st.meta.Writable)

	code, stdout, stderr := runCLIWith(fakeDeps(st), "-f", path, "unfreeze", "-confirm-audited")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Empty(t, st.unfreezeCalls, "没有显式确认不许解冻")
	assert.Contains(t, stderr, "RouteStore.Mode=redis")
	assert.Contains(t, stderr, "-cache-was-not-verified-acknowledged")
	assert.NotContains(t, stderr, "unreachable")

	code, stdout, stderr = runCLIWith(fakeDeps(st), "-f", path, "unfreeze", "-confirm-audited", "-cache-was-not-verified-acknowledged")
	assert.Equal(t, routetool.ExitClean, code, stderr)
	assert.Equal(t, []uint64{0}, st.unfreezeCalls)
	assert.Equal(t, uint64(1), st.meta.Writable)
	assert.Equal(t, "route_unfreeze changed=true already_writable=false marker=not_read_redis_mode generation_realigned=false min_watermark_ms=0\n", stdout)
	assert.Contains(t, stderr, "WARN: RouteStore.Mode=redis")
	assert.NotContains(t, stderr, "unreachable", "redis 模式的 unfreeze 不连 mapping Redis")
	assert.NotContains(t, stderr, "identity not scripted")

	clusterPath := writeYaml(t, strings.Replace(fmt.Sprintf(minimalYaml, addr), "Type: node", "Type: cluster", 1))
	st.meta.Writable = 0
	code, _, stderr = runCLIWith(fakeDeps(st), "-f", clusterPath, "unfreeze", "-confirm-audited", "-cache-was-not-verified-acknowledged")
	assert.Equal(t, routetool.ExitClean, code, stderr)
	assert.NotContains(t, stderr, "need a single instance")
}

// TestRunUnfreezeNeedsConfirmAudited:unfreeze 没有 -confirm-audited 就退出码 2(参数阶段,连 Redis 之前);
// 带上之后越过这道闸,走到连 Redis(这里地址不可达 → unreachable)。
func TestRunUnfreezeNeedsConfirmAudited(t *testing.T) {
	addr := closedLocalAddr(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, addr)+dbFirstYaml)

	code, stdout, stderr := runCLI("-f", path, "unfreeze")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "-confirm-audited")
	assert.Contains(t, stderr, "用法:")
	assert.NotContains(t, stderr, "unreachable")

	st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 0}}
	code, stdout, stderr = runCLIWith(fakeDeps(st), "-f", path, "unfreeze", "-confirm-audited")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "unreachable", "越过参数闸之后才会去连 Redis")
}

// TestRunFreezeUnreachableDBExitsTwo:freeze 只碰全局库、不连 mapping Redis(冻结是止血动作,Redis 坏了也要能冻);
// 全局库不可达 → 退出码 2,stderr 带 MySQL 的地址、不带 Redis 的。
func TestRunFreezeUnreachableDBExitsTwo(t *testing.T) {
	redisAddr, mysqlAddr := twoClosedLocalAddrs(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, redisAddr)+dbFirstYaml+fmt.Sprintf("SnapshotMySQL:\n  Host: %s\n", mysqlAddr))

	// 显式隔离 SQL 网络故障；正式凭据拒绝由独立默认入口测试覆盖。
	d := fakeDeps(&fakeStore{})
	d.openMaintenance = func(context.Context, config.Config, cliArgs) (*maintenanceInput, error) {
		return nil, fmt.Errorf("全局 SQL %s 连接失败", mysqlAddr)
	}
	code, stdout, stderr := runCLIWith(d, "-f", path, "freeze", "-reason=it")

	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, mysqlAddr, "失败应来自全局库")
	assert.NotContains(t, stderr, redisAddr, "freeze 不该去连 mapping Redis")
}

// TestRunStdoutCarriesOnlyTheSummary:stdout 只有一行统计;store 打的 logx 日志(这里由假库打)进 stderr,不进 stdout。
func TestRunStdoutCarriesOnlyTheSummary(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t))+dbFirstYaml)
	st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 1}, logOnFreeze: "fake store: freeze log line"}

	code, stdout, stderr := runCLIWith(fakeDeps(st), "-f", path, "freeze")

	assert.Equal(t, routetool.ExitClean, code, stderr)
	assert.Equal(t, "route_freeze changed=true writable=0\n", stdout, "stdout 恰好一行统计")
	assert.Contains(t, stderr, "fake store: freeze log line", "logx 改写到了 stderr")
}

// TestRunUnfreezeRefusesWithoutVerifiedMarker:经入口跑 unfreeze —— 没有就绪标记、或标记里的身份与当前 Redis 不符 → 退出码 2、
// 不解冻,stderr 指向 runbook §9.2 的清单重放与显式确认开关;加 -cache-was-not-verified-acknowledged 才放行(最低水位 0);
// 标记可信时不必确认,最低水位取标记的 watermark_ms。身份由注入的 deps 给出(miniredis 不支持 INFO server / replication)。
func TestRunUnfreezeRefusesWithoutVerifiedMarker(t *testing.T) {
	current := routetool.RedisIdentity{RunID: strings.Repeat("ab", 20), MasterReplID: strings.Repeat("cd", 20)}
	marker := func(t *testing.T, id routetool.RedisIdentity, wm uint64) string {
		t.Helper()
		raw, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{
			Generation:   testGen,
			RedisRunID:   id.RunID,
			MasterReplID: id.MasterReplID,
			SetAtMs:      wm + 1,
			WatermarkMs:  wm,
			Leader:       "route_leader@it:0a1b2c3d",
		})
		require.NoError(t, err)
		return raw
	}
	setup := func(t *testing.T) (*miniredis.Miniredis, string, *fakeStore, deps) {
		t.Helper()
		mr := miniredis.RunT(t)
		path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+dbFirstYaml)
		st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 0}}
		d := fakeDeps(st)
		d.redisIdentity = func(context.Context, routeRedis) (routetool.RedisIdentity, error) { return current, nil }
		return mr, path, st, d
	}

	t.Run("没有标记_拒绝", func(t *testing.T) {
		_, path, st, d := setup(t)
		code, stdout, stderr := runCLIWith(d, "-f", path, "unfreeze", "-confirm-audited")
		assert.Equal(t, routetool.ExitFailed, code)
		assert.Empty(t, stdout)
		assert.Empty(t, st.unfreezeCalls, "确认不了缓存就不许解冻")
		assert.Contains(t, stderr, "§9.2")
		assert.Contains(t, stderr, "-cache-was-not-verified-acknowledged")
	})
	t.Run("身份不符_拒绝", func(t *testing.T) {
		mr, path, st, d := setup(t)
		other := routetool.RedisIdentity{RunID: strings.Repeat("01", 20), MasterReplID: current.MasterReplID}
		require.NoError(t, mr.Set(placement.RouteReadyKey, marker(t, other, 42)))
		code, _, stderr := runCLIWith(d, "-f", path, "unfreeze", "-confirm-audited")
		assert.Equal(t, routetool.ExitFailed, code)
		assert.Empty(t, st.unfreezeCalls)
		assert.Contains(t, stderr, "another Redis instance")
	})
	t.Run("显式确认_放行_水位0", func(t *testing.T) {
		_, path, st, d := setup(t)
		code, stdout, stderr := runCLIWith(d, "-f", path, "unfreeze", "-confirm-audited", "-cache-was-not-verified-acknowledged")
		assert.Equal(t, routetool.ExitClean, code, stderr)
		assert.Equal(t, []uint64{0}, st.unfreezeCalls)
		assert.Contains(t, stdout, "marker=absent")
	})
	t.Run("标记可信_不必确认_水位取标记", func(t *testing.T) {
		mr, path, st, d := setup(t)
		require.NoError(t, mr.Set(placement.RouteReadyKey, marker(t, current, 1_760_000_000_000)))
		st.releaseFloor = 1_760_000_000_000
		code, stdout, stderr := runCLIWith(d, "-f", path, "unfreeze", "-confirm-audited")
		assert.Equal(t, routetool.ExitClean, code, stderr)
		assert.Equal(t, []uint64{1_760_000_000_000}, st.unfreezeCalls)
		assert.Contains(t, stdout, "marker=verified")
	})
}

// TestRunAuditFrozenRecoveryNeedsFrozenWriters:audit -frozen-recovery 在映射写者没冻结时退出码 2,而且在审计开始之前就拒绝
// (假库的扫描方法没实现,被调到会 panic);可写时库独有的行与「Redis 丢了已确认的发布」分不开,不能放过 db_only_unpublished。
func TestRunAuditFrozenRecoveryNeedsFrozenWriters(t *testing.T) {
	mr := miniredis.RunT(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+dbFirstYaml)
	st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 1}}

	code, stdout, stderr := runCLIWith(fakeDeps(st), "-f", path, "audit", "-frozen-recovery")

	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "-frozen-recovery only applies while the route writers are frozen")
}

// ── 第 1b-2 批收尾(2026-10-19):rewarm 与 audit -require-maprev ──────────────────────────────

// TestRunRewarmFlags:rewarm 的参数作用域与取值校验 —— 一律退出码 2、打印用法、不连任何东西。
// -wait 缺省 0;-wait > 0 时 -poll 必须为正、-wait 必须小于 -timeout(-timeout 缺省 30m)。
func TestRunRewarmFlags(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"negative wait", []string{"-f", "x.yaml", "rewarm", "-wait=-1s"}, "-wait=-1s"},
		{"zero poll while waiting", []string{"-f", "x.yaml", "rewarm", "-wait=1m", "-poll=0s"}, "-poll=0s"},
		{"wait not shorter than timeout", []string{"-f", "x.yaml", "rewarm", "-wait=30m"}, "-wait=30m0s"},
		{"batch is not a rewarm flag", []string{"-f", "x.yaml", "rewarm", "-batch=10"}, "batch"},
		{"confirm-audited is not a rewarm flag", []string{"-f", "x.yaml", "rewarm", "-confirm-audited"}, "confirm-audited"},
		{"require-maprev is audit only", []string{"-f", "x.yaml", "rewarm", "-require-maprev"}, "require-maprev"},
		{"poll is rewarm only", []string{"-f", "x.yaml", "unfreeze", "-confirm-audited", "-poll=1s"}, "poll"},
		{"extra positional after rewarm", []string{"-f", "x.yaml", "rewarm", "now"}, "unexpected arguments"},
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

// rewarmMarker 编一份 set_at_ms = setAtMs 的合规就绪标记。
func rewarmMarker(t *testing.T, setAtMs uint64) string {
	t.Helper()
	raw, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{
		Generation:   testGen,
		RedisRunID:   strings.Repeat("ab", 20),
		MasterReplID: strings.Repeat("cd", 20),
		SetAtMs:      setAtMs,
		WatermarkMs:  setAtMs - 1,
		Leader:       "route_leader@it:0a1b2c3d",
	})
	require.NoError(t, err)
	return raw
}

// presetControlKeys 写满三把控制键,外加一把不相干的映射键(rewarm 不许碰它)。
func presetControlKeys(t *testing.T, mr *miniredis.Miniredis, markerSetAtMs uint64) {
	t.Helper()
	wm, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: testGen, WatermarkMs: markerSetAtMs})
	require.NoError(t, err)
	require.NoError(t, mr.Set(placement.RouteReadyKey, rewarmMarker(t, markerSetAtMs)))
	require.NoError(t, mr.Set(placement.RouteWarmKey, strings.Repeat("0a", 16)))
	require.NoError(t, mr.Set(placement.RouteWatermarkKey, wm))
	require.NoError(t, mr.Set(placement.HomeZoneKey(1), "1"))
}

// TestRunRewarmDropsKeysWithoutTouchingTheDB:-wait=0(缺省)只连 mapping Redis、一把全局库都不开(回退 redis 模式时全局库可能已不归它管),
// 两种模式都允许;三把控制键没了,不相干的映射键还在;stdout 恰好一行统计。
func TestRunRewarmDropsKeysWithoutTouchingTheDB(t *testing.T) {
	for _, mode := range []struct{ name, yamlTail string }{{"redis模式", ""}, {"db_first", dbFirstYaml}} {
		t.Run(mode.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			presetControlKeys(t, mr, 1_760_000_000_000)
			path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+mode.yamlTail)
			opened := 0
			d := fakeDeps(&fakeStore{})
			d.openRouteStore = func(config.Config, authoritysql.Permission, *pb.SqlWriterMaintenanceReceipt) (routeStore, error) {
				opened++
				return nil, fmt.Errorf("must not open the global DB")
			}

			code, stdout, stderr := runCLIWith(d, "-f", path, "rewarm")

			assert.Equal(t, routetool.ExitClean, code, stderr)
			assert.Zero(t, opened, "-wait=0 不开全局库")
			assert.Equal(t, "route_rewarm deleted=3 marker_was=decoded waited=false rearmed=false baseline_set_at_ms=0 new_set_at_ms=0\n", stdout)
			for _, k := range []string{placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey} {
				assert.False(t, mr.Exists(k), k)
			}
			assert.True(t, mr.Exists(placement.HomeZoneKey(1)), "映射键不归 rewarm 管")
			assert.Contains(t, stderr, "DROPPED ")
		})
	}
}

// TestRunRewarmWaitRefusedInRedisMode:redis 模式下 -wait > 0 在连任何东西之前退出码 2(没有 leader 重挂);
// MappingRedis 不是 node 时 rewarm 一律退出码 2(三把控制键一条 DEL,cluster 下会 CROSSSLOT)。
func TestRunRewarmWaitRefusedInRedisMode(t *testing.T) {
	addr := closedLocalAddr(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, addr)) // 不写 RouteStore 段 = redis 模式

	code, stdout, stderr := runCLI("-f", path, "rewarm", "-wait=1m")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "RouteStore.Mode=redis")
	assert.Contains(t, stderr, "-wait=0")
	assert.NotContains(t, stderr, "unreachable", "必须在连 Redis 之前就拒绝")

	clusterPath := writeYaml(t, strings.Replace(fmt.Sprintf(minimalYaml, addr), "Type: node", "Type: cluster", 1)+dbFirstYaml)
	code, stdout, stderr = runCLI("-f", clusterPath, "rewarm")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "single instance")
	assert.NotContains(t, stderr, "unreachable")
}

// TestRunRewarmWaitRefusedWhileFrozen:db_first 下 -wait > 0 而映射写者冻结着 → 退出码 2,三把键都不动(冻结期 leader 不预热,等下去必然超时)。
func TestRunRewarmWaitRefusedWhileFrozen(t *testing.T) {
	mr := miniredis.RunT(t)
	presetControlKeys(t, mr, 1_760_000_000_000)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+dbFirstYaml)
	st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 0}}

	code, stdout, stderr := runCLIWith(fakeDeps(st), "-f", path, "rewarm", "-wait=1m", "-poll=1s")

	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "frozen")
	for _, k := range []string{placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey} {
		assert.True(t, mr.Exists(k), k)
	}
}

// TestRunRewarmWaitSeesRearmedMarker:db_first 下 -wait > 0 —— 撤键之后「leader」(这里是一个 goroutine:看到令牌没了,
// 就挂一份 set_at_ms 更大的新标记)重挂,rewarm 退出码 0,stdout 记下基线与新标记的 set_at_ms;基线取被撤标记,不读库时钟。
func TestRunRewarmWaitSeesRearmedMarker(t *testing.T) {
	const oldSetAt = uint64(1_760_000_000_000)
	mr := miniredis.RunT(t)
	presetControlKeys(t, mr, oldSetAt)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+dbFirstYaml)
	st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 1}}

	rearmed := rewarmMarker(t, oldSetAt+1) // 在测试 goroutine 里编好:require 不能在别的 goroutine 里 FailNow
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		deadline := time.Now().Add(5 * time.Second)
		for mr.Exists(placement.RouteWarmKey) {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		_ = mr.Set(placement.RouteReadyKey, rearmed)
	}()

	code, stdout, stderr := runCLIWith(fakeDeps(st), "-f", path, "rewarm", "-wait=10s", "-poll=5ms", "-timeout=1m")
	<-leaderDone

	assert.Equal(t, routetool.ExitClean, code, stderr)
	assert.Equal(t, fmt.Sprintf("route_rewarm deleted=3 marker_was=decoded waited=true rearmed=true baseline_set_at_ms=%d new_set_at_ms=%d\n",
		oldSetAt, oldSetAt+1), stdout)
	assert.Zero(t, st.clockCalls, "被撤标记可解码时基线取它的 set_at_ms,不读库时钟")
	assert.Contains(t, stderr, "REARMED ")
}

// TestRunRewarmWaitTimesOutExitsOne:db_first 下没有 leader 重挂 → 到 -wait 退出码 1,stdout 照出统计行(rearmed=false),
// stderr 指向 ArmReadyMarker 与 leader 指标;被撤时没有标记 → 基线取库时钟(读一次)。
func TestRunRewarmWaitTimesOutExitsOne(t *testing.T) {
	mr := miniredis.RunT(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+dbFirstYaml)
	st := &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 1}, dbNowMs: 1_760_000_000_000}

	code, stdout, stderr := runCLIWith(fakeDeps(st), "-f", path, "rewarm", "-wait=30ms", "-poll=10ms")

	assert.Equal(t, routetool.ExitAttention, code, stderr)
	assert.Equal(t, "route_rewarm deleted=0 marker_was=absent waited=true rearmed=false baseline_set_at_ms=1760000000000 new_set_at_ms=0\n", stdout)
	assert.Equal(t, 1, st.clockCalls)
	assert.Contains(t, stderr, "TIMEOUT")
	assert.Contains(t, stderr, "RouteStore.Reconcile.ArmReadyMarker")
	assert.Contains(t, stderr, "data_service_route_reconcile_leader")
}

// TestRunAuditRequireMapRevFlag:audit -require-maprev 接到 AuditOptions.RequireMapRev —— 缓存 maprev 落后于库 rev 的行默认口径不算差异
// (退出码 0、统计行没有 redis_rev_behind),严格口径退出码 1、统计行末尾 redis_rev_behind=1;redis 模式下连任何东西之前退出码 2。
func TestRunAuditRequireMapRevFlag(t *testing.T) {
	t.Run("redis模式_拒绝", func(t *testing.T) {
		path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t)))
		code, stdout, stderr := runCLI("-f", path, "audit", "-require-maprev")
		assert.Equal(t, routetool.ExitFailed, code)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "RouteStore.Mode=redis")
		assert.NotContains(t, stderr, "unreachable")
	})

	mr := miniredis.RunT(t)
	require.NoError(t, mr.Set(placement.HomeZoneKey(1), "1"))
	require.NoError(t, mr.Set(placement.MapRevKey(1), "1"))
	path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+dbFirstYaml)
	ast := &auditFakeStore{
		fakeStore: &fakeStore{meta: store.RouteMeta{Generation: testGen, Writable: 1}},
		rows:      map[uint64]store.RouteRow{1: {PlayerID: 1, HomeZone: 1, Rev: 2}},
	}
	d := fakeDeps(ast.fakeStore)
	d.openRouteStore = func(config.Config, authoritysql.Permission, *pb.SqlWriterMaintenanceReceipt) (routeStore, error) {
		return ast, nil
	}

	code, stdout, stderr := runCLIWith(d, "-f", path, "audit", "-recheck-delay=0s")
	assert.Equal(t, routetool.ExitClean, code, stderr)
	assert.NotContains(t, stdout, "redis_rev_behind", "默认口径统计行逐字不变")

	code, stdout, stderr = runCLIWith(d, "-f", path, "audit", "-require-maprev", "-recheck-delay=0s")
	assert.Equal(t, routetool.ExitAttention, code, stderr)
	assert.True(t, strings.HasSuffix(stdout, " redis_rev_behind=1\n"), stdout)
	assert.Contains(t, stderr, "DIFF kind=redis_rev_behind player=1 ")
}

// auditFakeStore 在 fakeStore 之上补 audit 要的三个只读方法(fakeStore 本身不实现它们:被调到就 panic,
// TestRunAuditFrozenRecoveryNeedsFrozenWriters 靠这一点证明门禁拦在审计之前)。
type auditFakeStore struct {
	*fakeStore
	rows map[uint64]store.RouteRow
}

func (f *auditFakeStore) ScanRoutes(_ context.Context, after store.RouteScanCursor, limit int) ([]store.RouteRow, error) {
	out := make([]store.RouteRow, 0, len(f.rows))
	for _, row := range f.rows {
		if after.Less(store.RouteScanCursorAt(row)) {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return store.RouteScanCursorAt(out[i]).Less(store.RouteScanCursorAt(out[j])) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *auditFakeStore) GetRoutes(_ context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	out := make(map[uint64]store.RouteRow)
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

func (f *auditFakeStore) ListMergedInto(context.Context) ([]store.MergedIntoRow, error) {
	return nil, nil
}

// ── 替身与夹具 ────────────────────────────────────────────────────────────────

const testGen = "0123456789abcdef0123456789abcdef"

// fakeStore 是 routeStore 的假实现:只实现本文件用到的方法;其余方法落到嵌入的 nil 接口上,被调到就 panic ——
// 说明装配走错了路(例如 frozen-recovery 的门禁没拦住,审计照样开扫)。
type fakeStore struct {
	routeStore

	meta          store.RouteMeta
	freezeCalls   int
	unfreezeCalls []uint64
	// logOnFreeze 非空时 FreezeRouteWriters 用 logx 打这一行(模拟 store 自己的日志)。
	logOnFreeze string
	// releaseFloor 是测试排列时固定的正式请求水位，不由被测工具回填。
	releaseFloor uint64
	// dbNowMs 是 DBNowMs 回的库时钟(rewarm -wait 的基线);clockCalls 记调用次数。
	dbNowMs    uint64
	clockCalls int
}

func (f *fakeStore) ReadRouteMeta(context.Context) (store.RouteMeta, error) { return f.meta, nil }

func (f *fakeStore) FreezeRouteWriters(context.Context) (bool, error) {
	f.freezeCalls++
	if f.logOnFreeze != "" {
		logx.Error(f.logOnFreeze)
	}
	changed := f.meta.Writable != 0
	f.meta.Writable = 0
	return changed, nil
}

func (f *fakeStore) UnfreezeRouteWriters(_ context.Context, minWatermarkMs uint64) (bool, error) {
	f.unfreezeCalls = append(f.unfreezeCalls, minWatermarkMs)
	f.meta.Writable = 1
	return true, nil
}

func (f *fakeStore) Close() error { return nil }

// DBNowMs 只给 rewarm -wait 取库时钟。dbNowMs 为 0 时按库时钟读不到回错(与真 store 同口径)。
func (f *fakeStore) DBNowMs(context.Context) (uint64, error) {
	f.clockCalls++
	if f.dbNowMs == 0 {
		return 0, fmt.Errorf("fake store: database clock read as 0")
	}
	return f.dbNowMs, nil
}

// fakeDeps 用真 connectMappingRedis(miniredis 或不可达地址都行)、假库,身份读取默认报错(用例按需替换)。
func fakeDeps(st *fakeStore) deps {
	return deps{
		connectRedis:      connectMappingRedis,
		resolvePermission: func(*cliArgs) (authoritysql.Permission, error) { return authoritysql.Reader(), nil }, // 无真实 SQL；只隔离装配依赖。
		openCommand:       unitRouteCommand,
		openMaintenance: func(_ context.Context, _ config.Config, a cliArgs) (*maintenanceInput, error) {
			return unitMaintenanceInput(st, a)
		},
		openRouteStore: func(config.Config, authoritysql.Permission, *pb.SqlWriterMaintenanceReceipt) (routeStore, error) {
			return st, nil
		},
		redisIdentity: func(context.Context, routeRedis) (routetool.RedisIdentity, error) {
			return routetool.RedisIdentity{}, fmt.Errorf("identity not scripted in this test")
		},
	}
}

// 单测只隔离SQL/P0装配；Redis客户端仍由真实owned factory创建和关闭，不制作假队列原件。
func unitRouteCommand(_ context.Context, _ config.Config, _ cliArgs, input *maintenanceInput) (*routeCommand, error) {
	c := &routeCommand{session: drainproof.New(), redisSession: drainproof.New()}
	if input != nil {
		c.catalog = input.db
	}
	return c, nil
}

type routeCommandCloser func() error

func (f routeCommandCloser) Close() error { return f() }

func TestRouteCommandClosesResourcesBeforeFormalClosure(t *testing.T) {
	ctx := context.Background()
	var events []string
	c, _ := unitRouteCommand(ctx, config.Config{}, cliArgs{}, nil)
	c.maintenance = true
	c.resources = []io.Closer{
		routeCommandCloser(func() error { events = append(events, "sql-close"); return nil }),
		routeCommandCloser(func() error { events = append(events, "redis-close"); return nil }),
	}
	c.check = func(context.Context) error { events = append(events, "frozen-recheck"); return nil }
	c.catalog = routeCommandCloser(func() error { events = append(events, "catalog-close"); return nil })
	c.write = func(role string, session *drainproof.Session) error {
		require.Equal(t, "MAINTENANCE", role)
		require.True(t, session.Closed())
		require.Zero(t, session.Pending())
		events = append(events, "formal-closure")
		return nil
	}
	done, err := c.session.Enter()
	require.NoError(t, err)
	done()
	require.NoError(t, c.finish(ctx))
	require.Equal(t, []string{"redis-close", "sql-close", "frozen-recheck", "catalog-close", "formal-closure"}, events)
}

func TestRouteCommandPipelineUnknownSurvivesLocalJoin(t *testing.T) {
	ctx := context.Background()
	for _, aggregate := range []bool{false, true} {
		t.Run(fmt.Sprint(aggregate), func(t *testing.T) {
			c, _ := unitRouteCommand(ctx, config.Config{}, cliArgs{}, nil)
			wrote := false
			c.maintenance = true
			c.write = func(string, *drainproof.Session) error { wrote = true; return nil }
			cmd := goredis.NewCmd(ctx, "SET", "fixture-key", "v")
			hook := routeRedisHook{command: c}.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error {
				if aggregate {
					return errors.New("整批结局未知")
				}
				cmd.SetErr(errors.New("单项结局未知"))
				return nil
			})
			_ = hook(ctx, []goredis.Cmder{cmd})
			require.Zero(t, c.redisSession.Pending(), "本机hook已经返回")
			require.ErrorIs(t, c.finish(ctx), drainproof.ErrHold)
			require.False(t, wrote, "本机join不能清业务UNKNOWN")
		})
	}
}

func TestRouteCommandClosedRedisRejectsBeforeNativeCall(t *testing.T) {
	ctx := context.Background()
	c, _ := unitRouteCommand(ctx, config.Config{}, cliArgs{}, nil)
	require.NoError(t, c.finish(ctx))
	calls := 0
	hook := routeRedisHook{command: c}.ProcessHook(func(context.Context, goredis.Cmder) error { calls++; return nil })
	require.ErrorIs(t, hook(ctx, goredis.NewCmd(ctx, "SET", "fixture-key", "v")), drainproof.ErrClosed)
	require.Zero(t, calls)
	require.Zero(t, c.redisSession.Pending())
	require.NoError(t, c.redisSession.Wait(ctx), "明确本地拒绝没有发布，不能凭空增加UNKNOWN")
}

func TestRouteCommandCloseFailurePreventsFormalClosure(t *testing.T) {
	ctx := context.Background()
	c, _ := unitRouteCommand(ctx, config.Config{}, cliArgs{}, nil)
	wrote := false
	c.maintenance = true
	c.write = func(string, *drainproof.Session) error { wrote = true; return nil }
	c.resources = []io.Closer{routeCommandCloser(func() error { return errors.New("连接关闭未确定") })}
	require.Error(t, c.finish(ctx))
	require.False(t, wrote)
}

func TestRunP6WithoutRuntimeRefusesBeforeRedisAndSQLWork(t *testing.T) {
	mr := miniredis.RunT(t)
	path := writeYaml(t, fmt.Sprintf(minimalYaml, mr.Addr())+dbFirstYaml)
	st := &fakeStore{}
	d := fakeDeps(st)
	d.openCommand = openRouteCommand
	connected, opened := 0, 0
	d.connectRedis = func(context.Context, redis.RedisConf) (routeRedis, error) {
		connected++
		return nil, errors.New("不应连接")
	}
	d.openRouteStore = func(config.Config, authoritysql.Permission, *pb.SqlWriterMaintenanceReceipt) (routeStore, error) {
		opened++
		return st, nil
	}
	code, out, errOut := runCLIWith(d, "-f", path, subReconcile, "-dry-run=false")
	require.Equal(t, routetool.ExitFailed, code)
	require.Empty(t, out)
	require.Contains(t, errOut, "P0预列runtime")
	require.Zero(t, connected)
	require.Zero(t, opened)
}

func TestOwnedRouteRedisRetainedHandleCannotPublishAfterFinish(t *testing.T) {
	ctx := context.Background()
	c, _ := unitRouteCommand(ctx, config.Config{}, cliArgs{}, nil)
	ctx = context.WithValue(ctx, routeCommandKey{}, c)
	mr := miniredis.RunT(t)
	rd, err := connectMappingRedis(ctx, redis.RedisConf{Host: mr.Addr(), Type: redis.NodeType})
	require.NoError(t, err)
	c.resources = append(c.resources, rd)
	_, err = rd.DelCtx(ctx, "fixture-before-close")
	require.NoError(t, err)
	require.NoError(t, c.finish(ctx))
	_, err = rd.SetnxCtx(context.Background(), "fixture-after-close", "v")
	require.Error(t, err)
	require.False(t, mr.Exists("fixture-after-close"))
}

func TestP6RewarmCannotWaitForFrozenLeader(t *testing.T) {
	path := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t))+dbFirstYaml)
	directory := t.TempDir()
	code, out, errOut := runCLI("-f", path, subRewarm, "-wait=1m", "-maintenance-request="+filepath.Join(directory, "frozen.json"), "-maintenance-sha256="+strings.Repeat("a", 64), "-ticket-id="+strings.Repeat("01", 16), "-drain-runtime-plan="+filepath.Join(directory, "runtime.json"), "-drain-runtime-directory="+directory, "-drain-runtime-sha256="+strings.Repeat("b", 64))
	require.Equal(t, routetool.ExitFailed, code)
	require.Empty(t, out)
	require.Contains(t, errOut, "P6冻结期只允许-wait=0")
}

// withMaintenanceShape 只用于显式注入依赖的装配测试；不创建凭据文件、不改变生产身份解析。
func withMaintenanceShape(args ...string) []string {
	out := append([]string(nil), args...)
	if len(args) < 3 {
		return out
	}
	sub := args[2]
	maint := sub == subFreeze || sub == subUnfreeze || sub == subReconcile
	for _, arg := range args[3:] {
		if arg == "-frozen-recovery" {
			maint = true
		}
	}
	if !maint {
		return out
	}
	out = append(out, "-maintenance-request="+filepath.Join(os.TempDir(), "route-cli-unit-request.json"), "-maintenance-sha256="+strings.Repeat("a", 64))
	if sub == subFreeze || sub == subUnfreeze || sub == subReconcile {
		out = append(out, "-ticket-id="+strings.Repeat("01", 16))
	}
	return out
}

// unitCatalog 隔离真正的 SQL 控制器。它只驱动既有业务装配替身，不证明权限、锁或永久收据。
type unitCatalog struct{ st *fakeStore }

func (c *unitCatalog) Freeze(ctx context.Context, ticket []byte, request *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if len(ticket) != 16 || request == nil || request.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE {
		return nil, fmt.Errorf("冻结请求不符")
	}
	if _, err := c.st.FreezeRouteWriters(ctx); err != nil {
		return nil, err
	}
	return proto.Clone(request).(*pb.SqlWriterMaintenanceReceipt), nil
}
func (c *unitCatalog) Release(ctx context.Context, ticket []byte, request *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if len(ticket) != 16 || request == nil || request.Kind != pb.SqlWriterMaintenanceReceipt_RELEASE || request.GetEvidence().GetReleaseReadiness() == nil {
		return nil, fmt.Errorf("释放请求不符")
	}
	if _, err := c.st.UnfreezeRouteWriters(ctx, request.GetEvidence().GetReleaseReadiness().RouteWatermarkFloorMs); err != nil {
		return nil, err
	}
	return proto.Clone(request).(*pb.SqlWriterMaintenanceReceipt), nil
}
func (c *unitCatalog) MaintenanceReceipt(context.Context, []byte) (*pb.SqlWriterMaintenanceReceipt, error) {
	return nil, fmt.Errorf("本装配测试未提供永久 SQL 收据")
}
func (c *unitCatalog) CurrentMeta(context.Context) (*pb.RouteMeta, error) {
	return nil, fmt.Errorf("本装配测试未提供 SQL 控制快照")
}
func unitMaintenanceInput(st *fakeStore, a cliArgs) (*maintenanceInput, error) {
	kind := pb.SqlWriterMaintenanceReceipt_FREEZE
	if a.sub == subUnfreeze {
		kind = pb.SqlWriterMaintenanceReceipt_RELEASE
	}
	run := bytes.Repeat([]byte{1}, 16)
	plan, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_Plan{RunId: run, BeforeEpoch: 1, CatalogRevision: 1})
	if err != nil {
		return nil, err
	}
	manifest := sha256.Sum256(plan)
	request := &pb.SqlWriterMaintenanceReceipt{RequestId: bytes.Repeat([]byte{2}, 16), RunId: run, Generation: testGen, BeforeEpoch: 1, CatalogRevision: 1, AttemptRevision: 1, Kind: kind, PlanProto: plan, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: run, ManifestDigest: manifest[:]}}
	if kind == pb.SqlWriterMaintenanceReceipt_RELEASE {
		request.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{ReleaseReadiness: &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{OriginalFreezeReceiptId: bytes.Repeat([]byte{3}, 16), OriginalFreezeReceiptDigest: bytes.Repeat([]byte{4}, 32), RouteWatermarkFloorMs: st.releaseFloor}}
	}
	request, err = authoritysql.SealMaintenanceRequest(request)
	if err != nil {
		return nil, err
	}
	return &maintenanceInput{db: st, catalog: &unitCatalog{st: st}, request: request, ticketID: bytes.Repeat([]byte{1}, 16)}, nil
}

// minimalYaml 是能被 conf.Load 接受的最小配置(同 internal/config 的 minimalYaml),MappingRedis.Host 由用例填。
const minimalYaml = `
Name: dataservice.rpc
ListenOn: 127.0.0.1:0
MappingRedis:
  Host: %s
  Type: node
Regions:
  - Id: 1
    Zones: [1]
    Redis:
      Addrs:
        - 127.0.0.1:7001
`

// dbFirstYaml 追加在 minimalYaml 之后,把 RouteStore 切成 db_first。
const dbFirstYaml = "RouteStore:\n  Mode: db_first\n"

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

// 默认入口不能从模式开关、审计确认或可达地址推导正式权限。
func TestRunProductionMaintenanceRequiresOriginalBeforeConnecting(t *testing.T) {
	address := closedLocalAddr(t)
	configPath := writeYaml(t, fmt.Sprintf(minimalYaml, address)+dbFirstYaml)
	for _, sub := range []string{"freeze", "unfreeze", "reconcile-from-cache"} {
		args := []string{"-f", configPath, sub}
		if sub == "unfreeze" {
			args = append(args, "-confirm-audited")
		}
		code, stdout, stderr := runCLI(args...)
		assert.Equal(t, routetool.ExitFailed, code)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "维护操作必须指定原件")
		assert.NotContains(t, stderr, "unreachable")
	}
}

func TestRunProductionRealImportRejectsMissingGrant(t *testing.T) {
	t.Setenv("MMORPG_SQL_WRITER_GRANT_FILE", "")
	t.Setenv("MMORPG_SQL_WRITER_GRANT_SHA256", "")
	configPath := writeYaml(t, fmt.Sprintf(minimalYaml, closedLocalAddr(t))+dbFirstYaml)
	code, stdout, stderr := runCLI("-f", configPath, "import-from-redis", "-dry-run=false", "-allow-db-first")
	assert.Equal(t, routetool.ExitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "SQL writer启动grant缺失")
	assert.NotContains(t, stderr, "unreachable")
}

func TestNormalRouteRuntimeRejectsReleasedBeforeConsumingBoot(t *testing.T) {
	for _, phase := range []pb.SqlWriterDrainRun_Phase{pb.SqlWriterDrainRun_PLAN_FIXED, pb.SqlWriterDrainRun_SQL_FROZEN, pb.SqlWriterDrainRun_SQL_RELEASED} {
		directory := t.TempDir()
		pin := routeRecoveryTestPin(t, filepath.Join(directory, "source.json"), []byte("fixed source"))
		seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16), PhysicalWriterKey: bytes.Repeat([]byte{3}, 16), LogicalRole: "SOURCE", PermanentRevivalFenceId: bytes.Repeat([]byte{4}, 16), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{pin}}
		plan := &pb.SqlWriterMaintenanceReceipt_Plan{RunId: bytes.Repeat([]byte{5}, 16), AuthoritySchema: "fixture", RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{seed}}
		current := proto.Clone(seed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
		role := "SOURCE"
		if phase == pb.SqlWriterDrainRun_SQL_RELEASED {
			current.InstanceId = bytes.Repeat([]byte{6}, 16)
			current.PhysicalWriterKey = bytes.Repeat([]byte{7}, 16)
		}
		if phase == pb.SqlWriterDrainRun_SQL_FROZEN {
			current.LogicalRole = "MAINTENANCE"
			current.PhysicalWriterKey = nil
			plan.MaintenanceIssuers = []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{current}
			role = "MAINTENANCE"
		}
		planRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
		require.NoError(t, err)
		digest := sha256.Sum256(planRaw)
		runtime := &drainproof.Runtime{Directory: directory, Plan: plan, Request: &pb.SqlWriterMaintenanceReceipt{RunId: plan.RunId, Generation: "abc", RunPhase: phase, PlanProto: planRaw, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: plan.RunId, ManifestDigest: digest[:], Detail: &pb.SqlWriterMaintenanceReceipt_Evidence_Publication{Publication: &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{current}}}}}}
		require.NotNil(t, runtime.Seed(role), "夹具必须是有效的同boot正式角色，不能靠空Plan在BindBoot中碰巧失败")
		marker := filepath.Join(directory, "observable-bind.consumed")
		bound, worked := 0, 0
		// 仅隔离BindBoot的私有文件副作用；生产传真实Runtime.BindBoot，不绕过任何正式身份验证。
		err = bindNormalRouteRuntime(runtime, func() error { bound++; return os.WriteFile(marker, []byte("binding reached"), 0600) })
		if err == nil {
			worked++
		}
		if phase == pb.SqlWriterDrainRun_PLAN_FIXED {
			require.NoError(t, err)
			require.Equal(t, 1, bound)
			require.Equal(t, 1, worked)
			_, err = os.Stat(marker)
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, drainproof.ErrHold)
			require.Zero(t, bound)
			require.Zero(t, worked)
			_, err = os.Stat(marker)
			require.True(t, errors.Is(err, os.ErrNotExist), "阶段拒绝必须先于实际binding副作用")
		}
	}
}

func TestRouteRecoveryExportRequiresExplicitCompleteReadOnlyShape(t *testing.T) {
	directory := t.TempDir()
	valid := []string{"-f", "fixture.yaml", subAudit, "-frozen-recovery", "-maintenance-request=" + filepath.Join(directory, "frozen.json"), "-maintenance-sha256=" + strings.Repeat("a", 64), "-maintenance-publication=" + filepath.Join(directory, "closed.json"), "-maintenance-publication-sha256=" + strings.Repeat("b", 64), "-recovery-audit-output=" + filepath.Join(directory, "audit.json")}
	args, err := parseArgs(valid, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, args.recoveryAuditOutput)
	for _, extra := range [][]string{{"-require-maprev"}, {"-timeout=31m"}, {"-maintenance-publication-sha256=bad"}, {"-recovery-audit-output=relative"}, {"-frozen-recovery=false"}} {
		_, err := parseArgs(append(append([]string(nil), valid...), extra...), io.Discard)
		require.Error(t, err)
	}
	ordinary, err := parseArgs([]string{"-f", "fixture.yaml", subAudit}, io.Discard)
	require.NoError(t, err)
	require.Empty(t, ordinary.recoveryAuditOutput)
}

type routeRecoveryEmptyStore struct {
	fakeStore
	scans int
}

func (s *routeRecoveryEmptyStore) ScanRoutes(context.Context, store.RouteScanCursor, int) ([]store.RouteRow, error) {
	s.scans++
	return nil, nil
}
func (s *routeRecoveryEmptyStore) GetRoutes(context.Context, []uint64) (map[uint64]store.RouteRow, error) {
	return map[uint64]store.RouteRow{}, nil
}
func (s *routeRecoveryEmptyStore) ListMergedInto(context.Context) ([]store.MergedIntoRow, error) {
	return nil, nil
}

type routeRecoveryEmptyRedis struct {
	routeRedis
	scans int
}

func (r *routeRecoveryEmptyRedis) ScanCtx(context.Context, uint64, string, int64) ([]string, uint64, error) {
	r.scans++
	return nil, 0, nil
}
func (r *routeRecoveryEmptyRedis) GetCtx(context.Context, string) (string, error) { return "", nil }
func (r *routeRecoveryEmptyRedis) MgetCtx(_ context.Context, keys ...string) ([]string, error) {
	return make([]string, len(keys)), nil
}

func TestRouteRecoveryStatsUseActualAuditAndRejectEveryBlockingObservation(t *testing.T) {
	rd, st := &routeRecoveryEmptyRedis{}, &routeRecoveryEmptyStore{}
	report, err := routetool.Audit(context.Background(), rd, st, routetool.AuditOptions{Batch: 10, MaxReport: 10, MaxRecheck: 10, FrozenRecovery: true, Stdout: io.Discard, Stderr: io.Discard})
	require.NoError(t, err)
	require.Positive(t, rd.scans)
	require.Positive(t, st.scans)
	stats, err := routeRecoveryStats(report)
	require.NoError(t, err)
	require.True(t, stats.FrozenRecovery)
	require.False(t, stats.RequireMapRev)
	require.False(t, stats.Completed, "资源尚未关闭不能宣告完成")
	unpublished := report
	unpublished.DBRows = 7
	unpublished.RedisPlayers = 5
	unpublished.DBOnlyUnpublished = 2
	stats, err = routeRecoveryStats(unpublished)
	require.NoError(t, err)
	require.Equal(t, uint64(7), stats.DbRows)
	require.Equal(t, uint64(5), stats.RedisPlayers)
	require.Equal(t, uint64(2), stats.DbOnlyUnpublished)
	for _, change := range []func(*routetool.AuditReport){
		func(r *routetool.AuditReport) { r.DBRows = -1 }, func(r *routetool.AuditReport) { r.RedisPlayers = -1 }, func(r *routetool.AuditReport) { r.DBOnlyUnpublished = -1 },
		func(r *routetool.AuditReport) { r.DBOnlyUnpublished = 1 }, func(r *routetool.AuditReport) { r.MissingInRedis = 1 }, func(r *routetool.AuditReport) { r.MissingInDB = 1 },
		func(r *routetool.AuditReport) { r.ValueMismatch = 1 }, func(r *routetool.AuditReport) { r.RedisMalformed = 1 }, func(r *routetool.AuditReport) { r.DBInvalid = 1 },
		func(r *routetool.AuditReport) { r.RedisRevAhead = 1 }, func(r *routetool.AuditReport) { r.RedisRevBehind = 1 }, func(r *routetool.AuditReport) { r.MergedIntoMissingInRedis = 1 },
		func(r *routetool.AuditReport) { r.MergedIntoMissingInDB = 1 }, func(r *routetool.AuditReport) { r.MergedIntoMismatch = 1 },
		func(r *routetool.AuditReport) { r.TransientCleared = 1 }, func(r *routetool.AuditReport) { r.RequireMapRev = true }, func(r *routetool.AuditReport) { r.FrozenRecovery = false },
	} {
		bad := report
		change(&bad)
		_, err := routeRecoveryStats(bad)
		require.Error(t, err)
	}
}

func routeRecoveryTestPin(t *testing.T, file string, raw []byte) *pb.SqlWriterMaintenanceReceipt_SourcePin {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0700))
	require.NoError(t, os.WriteFile(file, raw, 0600))
	sum := sha256.Sum256(raw)
	return &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: file, Sha256: sum[:]}
}
func TestRouteRecoveryPublicationBindsEveryRealToolOriginal(t *testing.T) {
	directory := t.TempDir()
	source := routeRecoveryTestPin(t, filepath.Join(directory, "source.json"), []byte("actual-source-fixture"))
	seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16), LogicalRole: "MAINTENANCE", PermanentRevivalFenceId: bytes.Repeat([]byte{3}, 16), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{source}}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{MaintenanceIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{seed}}
	closed := proto.Clone(seed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	closed.AcceptingClosedMs = 100
	closed.AcceptedSettledMs = 101
	closed.AcceptedFrontierDigest = bytes.Repeat([]byte{4}, 32)
	raw, err := (protojson.MarshalOptions{}).Marshal(closed)
	require.NoError(t, err)
	original := routeRecoveryTestPin(t, filepath.Join(directory, "maintenance.issuer.json"), raw)
	inventory, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: plan.MaintenanceIssuers})
	require.NoError(t, err)
	digest := sha256.Sum256(inventory)
	publication := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{closed}, SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{original}, IssuerInventoryDigest: digest[:]}
	frozen := &pb.SqlWriterMaintenanceReceipt{CommittedMs: 99}
	require.NoError(t, verifyRouteMaintenancePublication(plan, frozen, publication))
	for _, change := range []func(*pb.SqlWriterMaintenanceReceipt_PublicationEvidence){
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.LogicalRoles = nil }, func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.SourceEvidence = nil },
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) {
			p.LogicalRoles[0].InstanceId = bytes.Repeat([]byte{9}, 16)
		},
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) {
			p.LogicalRoles[0].UnresolvedPublications = 1
		},
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.LogicalRoles[0].AcceptingClosedMs = 98 },
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.LogicalRoles[0].AcceptedSettledMs++ },
	} {
		bad := proto.Clone(publication).(*pb.SqlWriterMaintenanceReceipt_PublicationEvidence)
		change(bad)
		require.Error(t, verifyRouteMaintenancePublication(plan, frozen, bad))
	}
	require.NoError(t, os.WriteFile(original.AbsolutePath, []byte("changed"), 0600))
	require.Error(t, verifyRouteMaintenancePublication(plan, frozen, publication))
}

func TestRouteRecoveryQuerySourcesMustAllExistInImmutableP0(t *testing.T) {
	directory := t.TempDir()
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	for _, rel := range []string{"go/data_service/internal/routetoolcli/cli.go", "go/data_service/internal/routetool/auditor.go", "go/data_service/internal/routetool/redisread.go", "go/data_service/internal/store/route_store.go"} {
		plan.SourcePins = append(plan.SourcePins, routeRecoveryTestPin(t, filepath.Join(directory, filepath.FromSlash(rel)), []byte(rel)))
	}
	pins, err := routeRecoveryQueryPins(plan)
	require.NoError(t, err)
	require.Len(t, pins, 4)
	bad := proto.Clone(plan).(*pb.SqlWriterMaintenanceReceipt_Plan)
	bad.SourcePins = bad.SourcePins[:3]
	_, err = routeRecoveryQueryPins(bad)
	require.Error(t, err)
	bad = proto.Clone(plan).(*pb.SqlWriterMaintenanceReceipt_Plan)
	bad.SourcePins = append(bad.SourcePins, bad.SourcePins[0])
	_, err = routeRecoveryQueryPins(bad)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(plan.SourcePins[0].AbsolutePath, []byte("changed source"), 0600))
	_, err = routeRecoveryQueryPins(plan)
	require.Error(t, err)
}

func TestRouteRecoveryPublishesOnlyAfterEveryResourceCloses(t *testing.T) {
	for _, failClose := range []bool{false, true} {
		events := []string{}
		audit := &pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit{Stats: &pb.SqlWriterMaintenanceReceipt_RouteRecoveryStats{FrozenRecovery: true}}
		resources := []io.Closer{routeCommandCloser(func() error { events = append(events, "catalog"); return nil }), routeCommandCloser(func() error {
			events = append(events, "redis")
			if failClose {
				return errors.New("close unknown")
			}
			return nil
		}), routeCommandCloser(func() error { events = append(events, "store"); return nil })}
		wrote := false
		err := publishRouteRecoveryAudit(context.Background(), "fixture", audit, resources, func(_ string, raw []byte) error {
			events = append(events, "write")
			wrote = true
			decoded := &pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit{}
			require.NoError(t, (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, decoded))
			require.True(t, decoded.Stats.Completed)
			return nil
		})
		if failClose {
			require.Error(t, err)
			require.False(t, wrote)
			require.False(t, audit.Stats.Completed)
			require.Equal(t, []string{"store", "redis", "catalog"}, events)
		} else {
			require.NoError(t, err)
			require.True(t, wrote)
			require.Equal(t, []string{"store", "redis", "catalog", "write"}, events)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	require.Error(t, publishRouteRecoveryAudit(ctx, "fixture", &pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit{Stats: &pb.SqlWriterMaintenanceReceipt_RouteRecoveryStats{}}, nil, func(string, []byte) error { called = true; return nil }))
	require.False(t, called)
}

type routeRecoveryAtomicRedis struct {
	routeRedis
	reply    any
	err      error
	script   string
	keys     []string
	argCount int
}

func (r *routeRecoveryAtomicRedis) EvalCtx(_ context.Context, script string, keys []string, args ...any) (any, error) {
	r.script = script
	r.keys = append([]string(nil), keys...)
	r.argCount = len(args)
	return r.reply, r.err
}
func TestRouteRecoveryUsesOneAtomicSnapshotAndActualWatermarkFloor(t *testing.T) {
	runID := bytes.Repeat([]byte{1}, 16)
	target := &pb.SqlWriterMaintenanceReceipt_RedisTargetClosure{Role: "mapping", Address: "127.0.0.1:6379", ServerRunId: []byte(strings.Repeat("a", 40)), MaintenanceNonce: bytes.Repeat([]byte{2}, 16)}
	marker, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{Generation: "abc", RedisRunID: string(target.ServerRunId), MasterReplID: strings.Repeat("b", 40), SetAtMs: 50, WatermarkMs: 80, Leader: "fixture"})
	require.NoError(t, err)
	watermark, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: "abc", WatermarkMs: 90})
	require.NoError(t, err)
	reply := []any{"run_id:" + string(target.ServerRunId) + "\r\nuptime_in_seconds:1\r\n", "role:master\r\nmaster_replid:" + strings.Repeat("b", 40) + "\r\n", marker, watermark, strings.Repeat("02", 16)}
	rd := &routeRecoveryAtomicRedis{reply: reply}
	before, err := captureRouteRecoveryRedis(context.Background(), rd, target, runID, "abc")
	require.NoError(t, err)
	require.Equal(t, uint64(90), before.RouteWatermarkFloorMs)
	key, err := drainproof.RedisMaintenanceNonceKey(runID)
	require.NoError(t, err)
	require.Equal(t, drainproof.RouteRecoveryReadScript, rd.script)
	require.Equal(t, []string{key, placement.RouteReadyKey, placement.RouteWatermarkKey}, rd.keys)
	require.Zero(t, rd.argCount)
	reply[0] = "run_id:" + string(target.ServerRunId) + "\r\nuptime_in_seconds:2\r\n"
	after, err := captureRouteRecoveryRedis(context.Background(), rd, target, runID, "abc")
	require.NoError(t, err)
	require.True(t, sameRouteRecoveryRedis(before, after), "INFO时间变化不代表物理身份变化")
	after.WatermarkRaw = []byte("changed")
	require.False(t, sameRouteRecoveryRedis(before, after))
	reply[3] = nil
	without, err := captureRouteRecoveryRedis(context.Background(), rd, target, runID, "abc")
	require.NoError(t, err)
	require.Equal(t, uint64(80), without.RouteWatermarkFloorMs)
	for _, tc := range []struct {
		index int
		value any
	}{{0, "run_id:" + strings.Repeat("c", 40)}, {1, "role:slave\r\nmaster_replid:" + strings.Repeat("b", 40)}, {2, nil}, {2, "malformed"}, {3, "malformed"}, {4, strings.Repeat("03", 16)}} {
		changed := append([]any(nil), reply...)
		changed[tc.index] = tc.value
		rd.reply = changed
		_, err := captureRouteRecoveryRedis(context.Background(), rd, target, runID, "abc")
		require.Error(t, err)
	}
	rd.reply = reply
	rd.err = errors.New("maintenance nonce missing or expiring")
	_, err = captureRouteRecoveryRedis(context.Background(), rd, target, runID, "abc")
	require.Error(t, err)
}

type routeRecoveryCatalogFixture struct {
	frozen *pb.SqlWriterMaintenanceReceipt
	run    *pb.SqlWriterDrainRun
	meta   *pb.RouteMeta
}

func (c *routeRecoveryCatalogFixture) MaintenanceReceipt(context.Context, []byte) (*pb.SqlWriterMaintenanceReceipt, error) {
	return c.frozen, nil
}
func (c *routeRecoveryCatalogFixture) DrainRun(context.Context, []byte) (*pb.SqlWriterDrainRun, error) {
	return c.run, nil
}
func (c *routeRecoveryCatalogFixture) CurrentMeta(context.Context) (*pb.RouteMeta, error) {
	return c.meta, nil
}
func TestRouteRecoveryRechecksPermanentFrozenRunAndEpoch(t *testing.T) {
	runID := bytes.Repeat([]byte{1}, 16)
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{RunId: runID}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	require.NoError(t, err)
	digest := sha256.Sum256(raw)
	frozen := &pb.SqlWriterMaintenanceReceipt{Kind: pb.SqlWriterMaintenanceReceipt_FREEZE, Phase: pb.SqlWriterMaintenanceReceipt_FROZEN, RunPhase: pb.SqlWriterDrainRun_SQL_FROZEN, RequestId: bytes.Repeat([]byte{2}, 16), RunId: runID, Generation: "abc", AttemptRevision: 1, AfterEpoch: 3, CatalogRevision: 7, PlanProto: raw, CommittedMs: 100}
	catalog := &routeRecoveryCatalogFixture{frozen: proto.Clone(frozen).(*pb.SqlWriterMaintenanceReceipt), run: &pb.SqlWriterDrainRun{Phase: pb.SqlWriterDrainRun_SQL_FROZEN, Generation: "abc", AttemptRevision: 1, ManifestProto: raw, ManifestDigest: digest[:], FrozenRequestId: frozen.RequestId}, meta: &pb.RouteMeta{Generation: "abc", WriterEpoch: 3, CatalogRevision: 7, FreezeRunId: runID}}
	snapshot, err := readRouteRecoveryFrozen(context.Background(), catalog, plan, frozen)
	require.NoError(t, err)
	require.NoError(t, snapshot.verifyCurrent(context.Background(), catalog, plan, frozen))
	for _, change := range []func(*routeRecoveryCatalogFixture){func(c *routeRecoveryCatalogFixture) { c.meta.Writable = 1 }, func(c *routeRecoveryCatalogFixture) { c.meta.WriterEpoch++ }, func(c *routeRecoveryCatalogFixture) { c.meta.CatalogRevision++ }, func(c *routeRecoveryCatalogFixture) { c.run.AttemptRevision++ }, func(c *routeRecoveryCatalogFixture) { c.run.Phase = pb.SqlWriterDrainRun_SQL_RELEASED }, func(c *routeRecoveryCatalogFixture) { c.frozen.CommittedMs++ }} {
		bad := &routeRecoveryCatalogFixture{frozen: proto.Clone(catalog.frozen).(*pb.SqlWriterMaintenanceReceipt), run: proto.Clone(catalog.run).(*pb.SqlWriterDrainRun), meta: proto.Clone(catalog.meta).(*pb.RouteMeta)}
		change(bad)
		_, err := readRouteRecoveryFrozen(context.Background(), bad, plan, frozen)
		require.Error(t, err)
	}
	// 这些变化单独看仍满足FROZEN/RELEASE_READY集合及原epoch，必须由首尾全文比较拒绝。
	for _, change := range []func(*routeRecoveryCatalogFixture){
		func(c *routeRecoveryCatalogFixture) { c.run.Phase = pb.SqlWriterDrainRun_RELEASE_READY },
		func(c *routeRecoveryCatalogFixture) { c.run.LatestReceiptId = bytes.Repeat([]byte{8}, 16) },
		func(c *routeRecoveryCatalogFixture) { c.run.LatestReceiptDigest = bytes.Repeat([]byte{9}, 32) },
		func(c *routeRecoveryCatalogFixture) { c.run.UpdatedMs++ },
		func(c *routeRecoveryCatalogFixture) { c.meta.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01}) },
	} {
		active := &routeRecoveryCatalogFixture{frozen: proto.Clone(catalog.frozen).(*pb.SqlWriterMaintenanceReceipt), run: proto.Clone(catalog.run).(*pb.SqlWriterDrainRun), meta: proto.Clone(catalog.meta).(*pb.RouteMeta)}
		initial, err := readRouteRecoveryFrozen(context.Background(), active, plan, frozen)
		require.NoError(t, err)
		change(active)
		_, err = readRouteRecoveryFrozen(context.Background(), active, plan, frozen)
		require.NoError(t, err, "尾读单独合法，不能把这个分支当作原字段校验的负例")
		wrote := false
		if err = initial.verifyCurrent(context.Background(), active, plan, frozen); err == nil {
			err = publishRouteRecoveryAudit(context.Background(), "fixture", &pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit{Stats: &pb.SqlWriterMaintenanceReceipt_RouteRecoveryStats{}}, nil, func(string, []byte) error { wrote = true; return nil })
		}
		require.Error(t, err)
		require.False(t, wrote, "SQL窗口漂移不得进入正式写入口")
	}

}
