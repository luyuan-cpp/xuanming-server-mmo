package metrics

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zeromicro/go-zero/core/logx/logtest"
)

// noConnDriver 是只为构造 *sql.DB 的假驱动:连接池指标只读 db.Stats(),不需要真连接;真去连就报错。
type noConnDriver struct{}

func (noConnDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("metrics test driver: no real connections")
}

const fakeDriverName = "player_inbox_metrics_test"

func init() { sql.Register(fakeDriverName, noConnDriver{}) }

// gatherSeries 从默认注册表收一次:指标族名 → 每条序列的标签(名 → 值)。
func gatherSeries(t *testing.T) map[string][]map[string]string {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string][]map[string]string, len(families))
	for _, f := range families {
		series := make([]map[string]string, 0, len(f.GetMetric()))
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			series = append(series, labels)
		}
		out[f.GetName()] = series
	}
	return out
}

// labelValues 是一族序列里某个标签出现过的全部取值。
func labelValues(series []map[string]string, name string) map[string]bool {
	out := map[string]bool{}
	for _, labels := range series {
		if v, ok := labels[name]; ok {
			out[v] = true
		}
	}
	return out
}

func hasAll(have map[string]bool, want ...string) bool {
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// TestRegisterExposesEveryMetric 钉住 Register(设计 §9.4):全部指标挂进默认注册表、带 player_inbox_ 前缀;
// 连接池指标带 db_name="player_inbox";critical / 结果类计数器预建零值序列;重复调用不 panic(只注册一次)。
// Register 进程内只生效一次,所以它的断言集中在这一个用例里;别的用例要调 Register 就传同一种假库
// (TestAlertRulesReferenceRegisteredMetrics),谁先跑结果都一样。
func TestRegisterExposesEveryMetric(t *testing.T) {
	logtest.Discard(t)
	db, err := sql.Open(fakeDriverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	Register(db)
	Register(db) // 第二次是空操作;MustRegister 重复注册会 panic
	// 带 label 的向量要有过一条样本才会出现在 Gather 里。
	ObserveEnqueue("gm_block_currency_gain", "created")
	ObserveFetch("scene_gm_sanction", "match", "locked")
	ObserveAck("scene_gm_sanction", "finalized")
	ObserveVoid("scene_gm_sanction", "cancelled", "direct")

	families := gatherSeries(t)
	for _, name := range []string{
		"player_inbox_enqueue_total", "player_inbox_fetch_total", "player_inbox_ack_total", "player_inbox_void_total",
		"player_inbox_notify_total", "player_inbox_cursor_ahead_total", "player_inbox_fence_regressed_total",
		"player_inbox_epoch_ahead_total", "player_inbox_reconcile_ledger_read_total",
		"player_inbox_reconcile_inconsistent_rows", "player_inbox_reconcile_truncated_total", "player_inbox_reconcile_rounds_total",
		"player_inbox_sweep_deleted_total", "player_inbox_fetch_scanned_rows",
		"player_inbox_delivered_unacked", "player_inbox_delivered_unacked_oldest_seconds", "player_inbox_open_items",
		"player_inbox_oldest_open_age_seconds", "player_inbox_max_delivery_count", "player_inbox_notify_backlog",
		"player_inbox_delivered_unacked_stale", "player_inbox_battle_producer_ready",
	} {
		if _, ok := families[name]; !ok {
			t.Errorf("默认注册表里没有 %s", name)
		}
	}

	// 零值序列:「从未发生」与「指标不存在」在告警规则里必须分得开。
	// 评审 L6 拆出 nodes_unsynced / node_ambiguous(会自愈、留给补发);评审 R3 再拆出 node_missing(镜像已同步却查不到
	// 节点,可能只是租约抖动,按退避重试),no_node 只剩「位置记录既无节点也无 epoch」这一种已落定的结局。
	if got := labelValues(families["player_inbox_notify_total"], "result"); !hasAll(got,
		"sent", "offline", "awaiting_placement", "no_node", "empty_uuid", "error", "nodes_unsynced", "node_ambiguous",
		"node_missing") {
		t.Errorf("notify_total 的预建序列 = %v", got)
	}
	if got := NotifyResultLabels(); len(got) != 9 {
		t.Errorf("NotifyResultLabels = %v, want 9 个(与 inbox.NotifyResult 逐一对应,见 TestNotifyResultLabelsArePreregistered)", got)
	}
	if got := labelValues(families["player_inbox_reconcile_ledger_read_total"], "result"); !hasAll(got, "found", "absent", "error", "invalid") {
		t.Errorf("reconcile_ledger_read_total 的预建序列 = %v", got)
	}
	// 对账存活告警按 rounds_total{result="ok"} 的增量取:从未跑过一轮也必须有 0 值序列(评审 L4)。
	if got := labelValues(families["player_inbox_reconcile_rounds_total"], "result"); !hasAll(got, "ok", "error", "timeout") {
		t.Errorf("reconcile_rounds_total 的预建序列 = %v", got)
	}
	if _, ok := families["player_inbox_reconcile_mismatch_total"]; ok {
		t.Error("reconcile_mismatch_total 已由 reconcile_inconsistent_rows gauge 取代(评审 L11),不应再注册")
	}

	pool, ok := families["go_sql_max_open_connections"]
	if !ok {
		t.Fatal("连接池指标没有注册(设计 §9.4:单独出池指标)")
	}
	if got := labelValues(pool, "db_name"); len(got) != 1 || !got["player_inbox"] {
		t.Errorf("连接池指标的 db_name = %v, want 只有 player_inbox", got)
	}

	// 标签里不得出现玩家维度(AGENTS §9:player_id 当 label 高基数会爆)。
	for name, series := range families {
		if !strings.HasPrefix(name, "player_inbox_") {
			continue
		}
		for _, labels := range series {
			for label := range labels {
				if strings.Contains(strings.ToLower(label), "player") {
					t.Errorf("%s 带了玩家维度的标签 %s", name, label)
				}
			}
		}
	}
}

// alertRulesFile 是收件箱的 Prometheus 告警规则,相对本包目录(go/player_inbox/internal/metrics,go test 的工作目录)。
var alertRulesFile = filepath.Join("..", "..", "..", "..", "deploy", "k8s", "player-inbox-alerts.yaml")

// alertMetricName 匹配告警文件里的收件箱指标名。\b 排除库名 mmorpg_player_inbox 这类词的后半截。
var alertMetricName = regexp.MustCompile(`\bplayer_inbox_[a-z0-9_]+`)

// TestAlertRulesReferenceRegisteredMetrics 是告警文件与指标注册之间的机械守卫(评审 L11 / A 批复核 R1):
// player-inbox-alerts.yaml 里凡是以 player_inbox_ 开头的词(表达式、注释、描述都算,文件头写明了这条约定)
// 都必须是 Register 之后默认注册表里真有的指标。
//
// 起因:L11 把 reconcile_mismatch_total 计数器换成 gauge,唯一引用它的 critical 告警没跟着改;
// increase() 对不存在的序列返回空集,告警静默失效,而全仓没有任何测试会红。
//
// 名字的对应:计数器在本包 Name 里就写成 xxx_total,Gather 给的族名本身带 _total,不用剥;直方图 / 摘要在表达式里
// 写的是 _bucket / _sum / _count 序列,按后缀还原成族名、并核对族的类型再比。
// 文件读不到、一个名字都抽不到都判红:这条守卫不能静默失效。
func TestAlertRulesReferenceRegisteredMetrics(t *testing.T) {
	logtest.Discard(t)
	raw, err := os.ReadFile(alertRulesFile)
	if err != nil {
		t.Fatalf("读告警规则 %s 失败(移动了文件就同步改这里): %v", alertRulesFile, err)
	}
	seen := map[string]bool{}
	for _, m := range alertMetricName.FindAll(raw, -1) {
		seen[string(m)] = true
	}
	if len(seen) == 0 {
		t.Fatalf("%s 里一个 player_inbox_* 名字都没抽到(改了写法就同步改这里的正则)", alertRulesFile)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)

	db, err := sql.Open(fakeDriverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Register 进程内只生效一次;本用例与 TestRegisterExposesEveryMetric 谁先跑都带上同一种假库,结果一样。
	Register(db)
	// 没有预置序列的向量要有过一条子序列才会出现在 Gather 里。只建子序列、不加数,不影响别的用例按增量断言。
	enqueueTotal.WithLabelValues("gm_block_currency_gain", "created")
	fetchTotal.WithLabelValues("scene_gm_sanction", "match", "locked")
	ackTotal.WithLabelValues("scene_gm_sanction", "finalized")
	voidTotal.WithLabelValues("scene_gm_sanction", "cancelled", "force")

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	kinds := make(map[string]string, len(families)) // 族名 → 类型(COUNTER / GAUGE / HISTOGRAM / SUMMARY …)
	for _, f := range families {
		kinds[f.GetName()] = f.GetType().String()
	}
	registered := func(name string) bool {
		if _, ok := kinds[name]; ok {
			return true
		}
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			base, ok := strings.CutSuffix(name, suffix)
			if !ok {
				continue
			}
			switch kinds[base] {
			case "HISTOGRAM":
				return true
			case "SUMMARY":
				if suffix != "_bucket" {
					return true
				}
			}
		}
		return false
	}
	for _, name := range names {
		if !registered(name) {
			t.Errorf("%s 引用了 %s,默认注册表里没有这个指标:要么指标已改名 / 删除(同步改告警文件),"+
				"要么它是没有预置序列的向量(在本用例开头给它建一条子序列)", alertRulesFile, name)
		}
	}
}

// TestScanGaugesSetAndReset:领导者写入一次扫描结果;卸任归零,免得 max() 聚合里留着卸任副本的旧值(设计 §9.4)。
// 陈旧的已投递未确认 gauge(评审 M4)走同一条写入与归零路径。
func TestScanGaugesSetAndReset(t *testing.T) {
	SetScanGauges(ScanGauges{
		DeliveredUnacked: 4, DeliveredUnackedStale: 3, DeliveredUnackedOldestSeconds: 7.5, OpenItems: 9,
		OldestOpenAgeSeconds: 60, MaxDeliveryCount: 6, NotifyBacklog: 2,
	})
	checks := []struct {
		name string
		g    prometheus.Gauge
		want float64
	}{
		{"delivered_unacked", deliveredUnacked, 4},
		{"delivered_unacked_stale", deliveredUnackedStale, 3},
		{"delivered_unacked_oldest_seconds", deliveredUnackedOldestSeconds, 7.5},
		{"open_items", openItems, 9},
		{"oldest_open_age_seconds", oldestOpenAgeSeconds, 60},
		{"max_delivery_count", maxDeliveryCount, 6},
		{"notify_backlog", notifyBacklog, 2},
	}
	for _, c := range checks {
		if got := testutil.ToFloat64(c.g); got != c.want {
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}
	ResetScanGauges()
	for _, c := range checks {
		if got := testutil.ToFloat64(c.g); got != 0 {
			t.Errorf("卸任后 %s = %v, want 0", c.name, got)
		}
	}
}

// TestReconcileInconsistentSetAndReset:不一致行数是「当前」读数 —— 每轮覆盖写、不累加(评审 L11);
// 卸任归零(同扫描型 gauge);负数钳到 0。
func TestReconcileInconsistentSetAndReset(t *testing.T) {
	SetReconcileInconsistent(3)
	SetReconcileInconsistent(3) // 同一处不一致下一轮再报一次:读数仍是 3,不是 6
	if got := testutil.ToFloat64(reconcileInconsistentRows); got != 3 {
		t.Fatalf("reconcile_inconsistent_rows = %v, want 3", got)
	}
	SetReconcileInconsistent(1)
	if got := testutil.ToFloat64(reconcileInconsistentRows); got != 1 {
		t.Fatalf("处置后下一轮只剩 1 处: %v", got)
	}
	SetReconcileInconsistent(-2)
	if got := testutil.ToFloat64(reconcileInconsistentRows); got != 0 {
		t.Fatalf("负数应钳到 0: %v", got)
	}
	SetReconcileInconsistent(4)
	ResetReconcileInconsistent()
	if got := testutil.ToFloat64(reconcileInconsistentRows); got != 0 {
		t.Fatalf("卸任后 = %v, want 0", got)
	}
}

// TestReconcileRoundAndTruncatedCounters:轮次按 result 计数,截断无 label。
func TestReconcileRoundAndTruncatedCounters(t *testing.T) {
	before := testutil.ToFloat64(reconcileRoundsTotal.WithLabelValues("timeout"))
	ObserveReconcileRound("timeout")
	if got := testutil.ToFloat64(reconcileRoundsTotal.WithLabelValues("timeout")) - before; got != 1 {
		t.Fatalf("rounds_total{result=timeout} 增量 = %v, want 1", got)
	}
	truncated := testutil.ToFloat64(reconcileTruncatedTotal)
	IncReconcileTruncated()
	if got := testutil.ToFloat64(reconcileTruncatedTotal) - truncated; got != 1 {
		t.Fatalf("truncated_total 增量 = %v, want 1", got)
	}
}

// TestBattleProducerReadyGauge:Match 结算生产面的装配状态只取 0 / 1(2026-10-30,缺密钥不再静默)。
func TestBattleProducerReadyGauge(t *testing.T) {
	SetBattleProducerReady(true)
	if got := testutil.ToFloat64(battleProducerReady); got != 1 {
		t.Fatalf("已装配应为 1,got %v", got)
	}
	SetBattleProducerReady(false)
	if got := testutil.ToFloat64(battleProducerReady); got != 0 {
		t.Fatalf("未装配应为 0,got %v", got)
	}
}

// TestAddSweepDeletedIgnoresNonPositive:清理一轮没删到行不该动计数器。
func TestAddSweepDeletedIgnoresNonPositive(t *testing.T) {
	before := testutil.ToFloat64(sweepDeletedTotal)
	AddSweepDeleted(0)
	AddSweepDeleted(-3)
	AddSweepDeleted(5)
	if got := testutil.ToFloat64(sweepDeletedTotal) - before; got != 5 {
		t.Fatalf("sweep_deleted_total 增量 = %v, want 5", got)
	}
}

// TestStartHTTPDisabledWhenAddrEmpty:MetricsListenAddr 留空 = 不开端点,stop 是空操作。
func TestStartHTTPDisabledWhenAddrEmpty(t *testing.T) {
	logtest.Discard(t)
	stop, bound := startHTTP("")
	if bound != "" {
		t.Fatalf("留空不该监听,got %q", bound)
	}
	stop()
	StartHTTP("")() // 公开入口同样是空操作
}

// TestStartHTTPServesMetricsAndStops:监听后 /metrics 回 200(默认注册表的内容);stop 之后端口不再接受连接。
func TestStartHTTPServesMetricsAndStops(t *testing.T) {
	logtest.Discard(t)
	stop, bound := startHTTP("127.0.0.1:0")
	if bound == "" {
		t.Fatal("应当监听成功并返回实际地址")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + bound + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "go_goroutines") {
		t.Fatalf("/metrics status=%d body 前 200 字节=%q", resp.StatusCode, firstBytes(body, 200))
	}
	if resp, err := client.Get("http://" + bound + "/other"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("只挂 /metrics,其它路径应 404,got %d", resp.StatusCode)
		}
	}

	stop()
	stop() // 重复调用无害
	if conn, err := net.DialTimeout("tcp", bound, time.Second); err == nil {
		conn.Close()
		t.Fatal("stop 之后端口仍在接受连接")
	}
}

// TestStartHTTPListenFailureDoesNotPanic:端口被占时只记错误、返回空操作的 stop(没有 /metrics 不拖垮收件箱)。
func TestStartHTTPListenFailureDoesNotPanic(t *testing.T) {
	logtest.Discard(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	stop, bound := startHTTP(occupied.Addr().String())
	if bound != "" {
		t.Fatalf("端口被占时不应返回监听地址,got %q", bound)
	}
	stop()
}

func firstBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
