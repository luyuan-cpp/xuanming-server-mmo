package noderegistry

// 纯单测:不连 etcd。etcd 上的 CAS / 续租 / 重注册见 registry_integration_test.go。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/releasetrack"
)

const (
	unitPrefix   = "ChatNodeService.rpc"
	unitNodeType = uint32(9)
	unitZone     = uint32(2)
	unitUUID     = "uuid-unit"
)

func unitSpec() Spec {
	return Spec{
		Prefix:   unitPrefix,
		NodeType: unitNodeType,
		ZoneId:   unitZone,
		LeaseTTL: 60,
		BuildValue: func(uint32, string) ([]byte, error) {
			return nil, errors.New("unit tests never call BuildValue")
		},
	}
}

// 格式串必须与 C++ EtcdManager::MakeNodeEtcdKey / MakeNodeAllocationKey
// (cpp/libs/engine/core/node/system/etcd/etcd_manager.cpp)逐字一致:
// C++ 节点与路由服按这两个路径 watch / CAS。改这里就是改跨语言契约。
func TestKeyFormatsMatchCpp(t *testing.T) {
	if got, want := RpcPath(unitPrefix, 2, 9, 7), "ChatNodeService.rpc/zone/2/node_type/9/node_id/7"; got != want {
		t.Fatalf("RpcPath = %q, want %q", got, want)
	}
	if got, want := AllocationKey(unitPrefix, 9, 7), "ChatNodeService.rpc/allocated/node_type/9/node_id/7"; got != want {
		t.Fatalf("AllocationKey = %q, want %q", got, want)
	}
	// 两种 key 的尾段同形,扫描时必须都能解回 node_id。
	for _, k := range []string{RpcPath(unitPrefix, 2, 9, nodeIDMax), AllocationKey(unitPrefix, 9, nodeIDMax)} {
		if id, ok := nodeIDFromKey(k); !ok || id != nodeIDMax {
			t.Fatalf("nodeIDFromKey(%q) = (%d, %v), want (%d, true)", k, id, ok, nodeIDMax)
		}
	}
}

// node_id 区间与既有副本、C++ NodeAllocator 同口径:0 保留,上界 1<<17-1(= C++ kMaxNodeId)。
// node_id 不喂给任何发号器,上界不跟发号器的位宽走;改它要同步 guild / player_locator 的副本与 C++。
func TestNodeIDRangeMatchesCpp(t *testing.T) {
	if nodeIDMin != 1 {
		t.Fatalf("nodeIDMin = %d, want 1 (0 is reserved)", nodeIDMin)
	}
	if nodeIDMax != 131071 {
		t.Fatalf("nodeIDMax = %d, want 131071 (C++ NodeAllocator kMaxNodeId)", nodeIDMax)
	}
}

func TestNodeIDFromKey(t *testing.T) {
	cases := []struct {
		key    string
		wantID uint32
		wantOK bool
	}{
		{"P.rpc/zone/1/node_type/9/node_id/5", 5, true},
		{"P.rpc/allocated/node_type/9/node_id/12", 12, true},
		{"P.rpc/zone/1/node_type/9/node_id/131071", 131071, true},
		{"odd/node_id/7/node_id/8", 8, true}, // 只认最后一段
		{"P.rpc/zone/1/node_type/9/node_id/0", 0, false},
		{"P.rpc/allocated/node_type/9/node_id/0", 0, false},
		{"P.rpc/zone/1/node_type/9/node_id/", 0, false},
		{"P.rpc/zone/1/node_type/9/node_id/abc", 0, false},
		{"P.rpc/zone/1/node_type/9/node_id/-3", 0, false},
		{"P.rpc/zone/1/node_type/9/node_id/+5", 0, false},
		{"P.rpc/zone/1/node_type/9/node_id/131072", 0, false},     // 超出 NodeMask
		{"P.rpc/zone/1/node_type/9/node_id/4294967296", 0, false}, // 超出 uint32
		{"P.rpc/zone/1/node_type/9/node_id/5/extra", 0, false},
		{"P.rpc/zone/1/node_type/9", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		id, ok := nodeIDFromKey(c.key)
		if id != c.wantID || ok != c.wantOK {
			t.Errorf("nodeIDFromKey(%q) = (%d, %v), want (%d, %v)", c.key, id, ok, c.wantID, c.wantOK)
		}
	}
}

// 已占 id 从 key 路径取,两棵子树都算,跨 zone 也算;值是否可解析与此无关(这里根本没有值)。
func TestUsedNodeIDsFromKeys(t *testing.T) {
	keys := []string{
		RpcPath(unitPrefix, 1, unitNodeType, 1),
		RpcPath(unitPrefix, 2, unitNodeType, 3), // 另一个 zone 的实例同样占号(allocKey 不带 zone)
		AllocationKey(unitPrefix, unitNodeType, 5),
		AllocationKey(unitPrefix, unitNodeType, 0), // 0 忽略
		RpcPath(unitPrefix, 1, unitNodeType, 0),    // 0 忽略
		RpcPath(unitPrefix, 1, unitNodeType, 3),    // 重复
		unitPrefix + "/something/else",
		unitPrefix + "/zone/1/node_type/9/node_id/x",
	}
	used := usedNodeIDsFromKeys(keys)
	want := map[uint32]bool{1: true, 3: true, 5: true}
	if len(used) != len(want) {
		t.Fatalf("used = %v, want %v", used, want)
	}
	for id := range want {
		if !used[id] {
			t.Fatalf("used = %v, missing %d", used, id)
		}
	}
}

// goodInfo 是一份合格的 protojson NodeInfo(lowerCamelCase)。launchTime 是 uint64,
// protojson 编成字符串;镜像不声明它,不应影响校验。
func goodInfo() map[string]any {
	return map[string]any{
		"nodeId":       7,
		"nodeType":     unitNodeType,
		"zoneId":       unitZone,
		"nodeUuid":     unitUUID,
		"launchTime":   "1773446400",
		"endpoint":     map[string]any{"ip": "127.0.0.1", "port": 50700},
		"grpcEndpoint": map[string]any{"ip": "127.0.0.1", "port": 50700},
		"protocolType": 1,
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateValue(t *testing.T) {
	spec := unitSpec()

	ok := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"protocolType as number", func(map[string]any) {}},
		{"protocolType as enum name", func(m map[string]any) { m["protocolType"] = "PROTOCOL_GRPC" }},
	}
	for _, c := range ok {
		m := goodInfo()
		c.mutate(m)
		if err := validateValue(mustJSON(t, m), spec, 7, unitUUID); err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
	}

	bad := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"nodeId mismatch", func(m map[string]any) { m["nodeId"] = 8 }},
		{"nodeId missing", func(m map[string]any) { delete(m, "nodeId") }},
		{"nodeId as string", func(m map[string]any) { m["nodeId"] = "7" }},
		{"zoneId mismatch", func(m map[string]any) { m["zoneId"] = 1 }},
		{"zoneId missing", func(m map[string]any) { delete(m, "zoneId") }},
		{"nodeType mismatch", func(m map[string]any) { m["nodeType"] = 8 }},
		{"nodeUuid empty", func(m map[string]any) { m["nodeUuid"] = "" }},
		{"nodeUuid missing", func(m map[string]any) { delete(m, "nodeUuid") }},
		{"nodeUuid differs from registration", func(m map[string]any) { m["nodeUuid"] = "someone-else" }},
		{"endpoint.port zero", func(m map[string]any) { m["endpoint"] = map[string]any{"ip": "127.0.0.1", "port": 0} }},
		{"endpoint missing", func(m map[string]any) { delete(m, "endpoint") }},
		{"grpcEndpoint.port zero", func(m map[string]any) { m["grpcEndpoint"] = map[string]any{"ip": "127.0.0.1"} }},
		{"grpcEndpoint missing", func(m map[string]any) { delete(m, "grpcEndpoint") }},
		{"protocolType missing", func(m map[string]any) { delete(m, "protocolType") }},
		{"protocolType null", func(m map[string]any) { m["protocolType"] = nil }},
		{"protocolType other name", func(m map[string]any) { m["protocolType"] = "PROTOCOL_TCP" }},
		{"protocolType other number", func(m map[string]any) { m["protocolType"] = 2 }},
	}
	for _, c := range bad {
		m := goodInfo()
		c.mutate(m)
		if err := validateValue(mustJSON(t, m), spec, 7, unitUUID); !errors.Is(err, errInvalidValue) {
			t.Errorf("%s: want errInvalidValue, got %v", c.name, err)
		}
	}

	// 用了 proto 原名(UseProtoNames)的产物:C++ / 路由服都读不出来,必须拒。
	snake := map[string]any{
		"node_id": 7, "node_type": unitNodeType, "zone_id": unitZone, "node_uuid": unitUUID,
		"endpoint":      map[string]any{"port": 50700},
		"grpc_endpoint": map[string]any{"port": 50700},
		"protocol_type": 1,
	}
	if err := validateValue(mustJSON(t, snake), spec, 7, unitUUID); !errors.Is(err, errInvalidValue) {
		t.Errorf("snake_case names: want errInvalidValue, got %v", err)
	}
	if err := validateValue([]byte("not json"), spec, 7, unitUUID); !errors.Is(err, errInvalidValue) {
		t.Errorf("not json: want errInvalidValue, got %v", err)
	}
}

func TestSpecValidate(t *testing.T) {
	if err := unitSpec().validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	exit := unitSpec()
	exit.OnReclaimFailed = ExitProcess
	if err := exit.validate(); err != nil {
		t.Fatalf("ExitProcess spec rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(s *Spec)
	}{
		{"empty prefix", func(s *Spec) { s.Prefix = "" }},
		{"prefix with trailing slash", func(s *Spec) { s.Prefix = unitPrefix + "/" }},
		{"prefix with inner slash", func(s *Spec) { s.Prefix = "Chat/NodeService.rpc" }},
		{"zone 0", func(s *Spec) { s.ZoneId = 0 }},
		{"lease ttl 0", func(s *Spec) { s.LeaseTTL = 0 }},
		{"lease ttl negative", func(s *Spec) { s.LeaseTTL = -1 }},
		{"nil BuildValue", func(s *Spec) { s.BuildValue = nil }},
		{"unknown policy", func(s *Spec) { s.OnReclaimFailed = ReclaimPolicy(2) }},
	}
	for _, c := range cases {
		s := unitSpec()
		c.mutate(&s)
		if err := s.validate(); !errors.Is(err, errInvalidSpec) {
			t.Errorf("%s: want errInvalidSpec, got %v", c.name, err)
		}
	}
}

// 零值策略必须是 ReallocateNewID(契约:默认,chat 用)。
func TestReclaimPolicyDefaults(t *testing.T) {
	var s Spec
	if s.OnReclaimFailed != ReallocateNewID {
		t.Fatalf("zero-value policy = %s, want ReallocateNewID", s.OnReclaimFailed)
	}
	if ReallocateNewID.String() != "ReallocateNewID" || ExitProcess.String() != "ExitProcess" {
		t.Fatalf("String(): %s / %s", ReallocateNewID, ExitProcess)
	}
}

// 配置错误在碰 etcd 之前、在等端口之前就要报出来。
func TestRegisterRejectsBadInputsBeforeAnyIO(t *testing.T) {
	if _, err := Register(context.Background(), nil, unitSpec()); !errors.Is(err, errInvalidSpec) {
		t.Fatalf("Register(nil client): want errInvalidSpec, got %v", err)
	}
	start := time.Now()
	if _, err := RegisterAfterListening(context.Background(), nil, "127.0.0.1:1", 10*time.Second, unitSpec()); !errors.Is(err, errInvalidSpec) {
		t.Fatalf("RegisterAfterListening(nil client): want errInvalidSpec, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("invalid input waited %v for the port instead of failing fast", elapsed)
	}
}

// 发布轨道的环境变量非法时同样在碰 etcd、等端口之前拒绝(包注释「发布轨道」)。
// 零值 client 只为越过 nil 检查;校验失败时不会用到它。
func TestRegisterRejectsInvalidReleaseTrackBeforeAnyIO(t *testing.T) {
	t.Setenv(releasetrack.EnvVar, "beta")
	cli := &clientv3.Client{}
	if _, err := Register(context.Background(), cli, unitSpec()); !errors.Is(err, errInvalidSpec) {
		t.Fatalf("Register with %s=beta: want errInvalidSpec, got %v", releasetrack.EnvVar, err)
	}
	start := time.Now()
	if _, err := RegisterAfterListening(context.Background(), cli, "127.0.0.1:1", 10*time.Second, unitSpec()); !errors.Is(err, errInvalidSpec) {
		t.Fatalf("RegisterAfterListening with %s=beta: want errInvalidSpec, got %v", releasetrack.EnvVar, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("invalid release track waited %v for the port instead of failing fast", elapsed)
	}
}

func releaseTrackOf(t *testing.T, raw []byte) any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("value is not a JSON object: %v", err)
	}
	return fields[releaseTrackField]
}

// 注册值里的 releaseTrack 由本包写:没写就补上,写了必须与进程轨道一致,空串等同没写。
func TestWithReleaseTrack(t *testing.T) {
	raw := []byte(`{"nodeId":7,"zoneId":2,"nodeUuid":"u"}`)
	out, err := withReleaseTrack(raw, releasetrack.Canary)
	if err != nil {
		t.Fatalf("withReleaseTrack: %v", err)
	}
	if got := releaseTrackOf(t, out); got != "canary" {
		t.Fatalf("releaseTrack = %v, want canary", got)
	}
	if m := decodeMirror(t, out); m.NodeId != 7 || m.ZoneId != 2 || m.NodeUuid != "u" {
		t.Fatalf("other fields changed: %+v", m)
	}
	again, err := withReleaseTrack(raw, releasetrack.Canary)
	if err != nil || string(again) != string(out) {
		t.Fatalf("output is not deterministic: %s vs %s (%v)", out, again, err)
	}

	same := []byte(`{"nodeId":7,"releaseTrack":"stable"}`)
	if out, err := withReleaseTrack(same, releasetrack.Stable); err != nil || string(out) != string(same) {
		t.Fatalf("matching releaseTrack must pass through unchanged: %s, %v", out, err)
	}

	empty := []byte(`{"nodeId":7,"releaseTrack":""}`)
	if out, err := withReleaseTrack(empty, releasetrack.Canary); err != nil || releaseTrackOf(t, out) != "canary" {
		t.Fatalf("empty releaseTrack must be filled in: %s, %v", out, err)
	}

	rejected := []string{
		`{"releaseTrack":"canary"}`, // 与进程轨道 stable 不一致
		`{"releaseTrack":1}`,        // 不是字符串
		`not json`,
		`null`,
		`[]`,
	}
	for _, value := range rejected {
		if _, err := withReleaseTrack([]byte(value), releasetrack.Stable); !errors.Is(err, errInvalidValue) {
			t.Fatalf("withReleaseTrack(%s): want errInvalidValue, got %v", value, err)
		}
	}
}

func decodeMirror(t *testing.T, raw []byte) nodeInfoMirror {
	t.Helper()
	var m nodeInfoMirror
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("value is not a JSON NodeInfo: %v", err)
	}
	return m
}

func TestDialAddrForListen(t *testing.T) {
	good := []struct{ in, want string }{
		{"0.0.0.0:50700", "127.0.0.1:50700"},
		{"[::]:50700", "127.0.0.1:50700"},
		{":50700", "127.0.0.1:50700"},
		{"127.0.0.1:50700", "127.0.0.1:50700"},
		{"10.0.0.5:50700", "10.0.0.5:50700"},
		{"localhost:50700", "localhost:50700"},
	}
	for _, c := range good {
		got, err := dialAddrForListen(c.in)
		if err != nil || got != c.want {
			t.Errorf("dialAddrForListen(%q) = (%q, %v), want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"50700", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:abc", "127.0.0.1:70000", ""} {
		if _, err := dialAddrForListen(in); !errors.Is(err, errInvalidSpec) {
			t.Errorf("dialAddrForListen(%q): want errInvalidSpec, got %v", in, err)
		}
	}
}

func TestWaitForListening_ReturnsOnceThePortAccepts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := waitForListening(context.Background(), ln.Addr().String(), 2*time.Second); err != nil {
		t.Fatalf("open port: %v", err)
	}
}

// closedLoopbackAddr 返回一个刚释放、大概率无人监听的 loopback 地址。
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestWaitForListening_TimesOut(t *testing.T) {
	addr := closedLoopbackAddr(t)
	start := time.Now()
	if err := waitForListening(context.Background(), addr, 300*time.Millisecond); err == nil {
		t.Skipf("port %s was taken by someone else in between; cannot exercise the timeout", addr)
	}
	// 上界 = timeout + 一次拨号超时 + 一次轮询间隔,留足余量。
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout 300ms but waited %v", elapsed)
	}
}

func TestWaitForListening_StopsOnContextCancel(t *testing.T) {
	addr := closedLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitForListening(ctx, addr, 10*time.Second)
	if err == nil {
		t.Skipf("port %s was taken by someone else in between", addr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancelled ctx but waited %v", elapsed)
	}
}

func TestWaitForListening_RejectsNonPositiveTimeout(t *testing.T) {
	if err := waitForListening(context.Background(), "127.0.0.1:1", 0); !errors.Is(err, errInvalidSpec) {
		t.Fatalf("want errInvalidSpec, got %v", err)
	}
}

// ---- 退避(R18)-------------------------------------------------------------------------

// 退避抖 ±20%:落在 [0.8d, 1.2d],而且确实在变(不变就等于没抖,多实例仍同拍重试)。
func TestJitteredStaysWithinTwentyPercent(t *testing.T) {
	for _, d := range []time.Duration{backoffMin, 7 * time.Second, backoffMax} {
		lo, hi := d-d/5, d+d/5
		seen := make(map[time.Duration]bool)
		for i := 0; i < 1000; i++ {
			got := jittered(d)
			if got < lo || got > hi {
				t.Fatalf("jittered(%v) = %v, want within [%v, %v]", d, got, lo, hi)
			}
			seen[got] = true
		}
		if len(seen) < 2 {
			t.Fatalf("jittered(%v) returned the same value 1000 times", d)
		}
	}
	for _, d := range []time.Duration{0, 4 * time.Nanosecond} {
		if got := jittered(d); got != d {
			t.Fatalf("jittered(%v) = %v, want it unchanged", d, got)
		}
	}
}

// 退避等待挂在句柄的 ctx 上:Close 的第一步就是取消它,等待必须立刻返回 false,不等满退避。
func TestSleepReturnsAsSoonAsTheRegistrationCloses(t *testing.T) {
	r := newRegistration(nil, unitSpec(), unitUUID, releasetrack.Stable)
	time.AfterFunc(50*time.Millisecond, r.cancel)
	start := time.Now()
	if r.sleep(backoffMax) {
		t.Fatal("sleep must report false once the registration is closing")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("sleep ignored cancellation for %v", elapsed)
	}
}

// ---- 指标(R17)-------------------------------------------------------------------------
// 每个用例用自己的 Prefix,序列互不干扰。这里只走不碰 etcd 的状态切换;Close 与真实失租在集成测试里核对。

var metricsPrefixSeq atomic.Int64

// metricsPrefix 给指标用例一个本次运行独有的前缀:指标是进程级全局状态,-count=N 重跑时不能撞上上一轮的序列。
func metricsPrefix(name string) string {
	return fmt.Sprintf("%s%d_%d.rpc", name, time.Now().UnixNano(), metricsPrefixSeq.Add(1))
}

func metricsSpec(prefix string) Spec {
	s := unitSpec()
	s.Prefix = prefix
	return s
}

func registeredValue(prefix string) float64 {
	return testutil.ToFloat64(registeredGauge.WithLabelValues(prefix))
}

// 预置:注册成功时就把本前缀的全部序列建好,第一次失租 / 失败之前序列已存在,increase() 不会漏掉第一次。
func TestPrimeMetricsCreatesEverySeries(t *testing.T) {
	prefix := metricsPrefix("NoderegistryPrimeUnit")
	counts := func() []int {
		return []int{
			testutil.CollectAndCount(leaseLostTotal),
			testutil.CollectAndCount(reRegisterSuccessTotal),
			testutil.CollectAndCount(reRegisterFailureTotal),
			testutil.CollectAndCount(nodeIDReallocatedTotal),
			testutil.CollectAndCount(registeredGauge),
		}
	}
	before := counts()
	primeMetrics(prefix)
	primeMetrics(prefix) // 幂等:不重复建序列
	after := counts()
	want := []int{1, 1, len(reRegisterFailReasons), 1, 1}
	for i := range want {
		if got := after[i] - before[i]; got != want[i] {
			t.Fatalf("collector #%d: priming added %d series, want %d", i, got, want[i])
		}
	}
	if registeredValue(prefix) != 0 {
		t.Fatalf("primed registered gauge = %v, want 0 until the first commit", registeredValue(prefix))
	}
}

// registered 跟着「提交 / 判失租」走,同一句柄重复提交、重复判失租不多加多减;失租次数逐次累加。
func TestRegisteredGaugeFollowsCommitAndLeaseLoss(t *testing.T) {
	prefix := metricsPrefix("NoderegistryGaugeUnit")
	r := newRegistration(nil, metricsSpec(prefix), unitUUID, releasetrack.Stable)
	defer r.cancel()
	primeMetrics(prefix)
	leaseLost := func() float64 { return testutil.ToFloat64(leaseLostTotal.WithLabelValues(prefix)) }

	if !r.commit(1, 7, "{}") || registeredValue(prefix) != 1 {
		t.Fatalf("after first commit: registered = %v, want 1", registeredValue(prefix))
	}
	r.commit(2, 7, "{}") // 重夺原号:仍是同一份注册
	if registeredValue(prefix) != 1 {
		t.Fatalf("re-commit on the same handle: registered = %v, want 1", registeredValue(prefix))
	}

	r.markLeaseLost()
	r.markLeaseLost()
	if registeredValue(prefix) != 0 || leaseLost() != 2 {
		t.Fatalf("after two lease losses: registered = %v (want 0), lease_lost = %v (want 2)", registeredValue(prefix), leaseLost())
	}

	// 同一前缀第二份注册(测试与多实例进程里会有):gauge 是持有份数,不是 0/1 开关。
	other := newRegistration(nil, metricsSpec(prefix), "uuid-other", releasetrack.Stable)
	defer other.cancel()
	r.commit(3, 8, "{}")
	other.commit(4, 9, "{}")
	if registeredValue(prefix) != 2 {
		t.Fatalf("two handles holding: registered = %v, want 2", registeredValue(prefix))
	}
}

// 失败按原因计数;句柄关闭(ctx 取消)之后的失败是停机造成的,不计。
func TestReRegisterFailuresAreCountedByReasonUntilClose(t *testing.T) {
	prefix := metricsPrefix("NoderegistryFailureUnit")
	r := newRegistration(nil, metricsSpec(prefix), unitUUID, releasetrack.Stable)
	primeMetrics(prefix)
	failures := func(reason string) float64 {
		return testutil.ToFloat64(reRegisterFailureTotal.WithLabelValues(prefix, reason))
	}

	r.countFailedAttempt(reRegisterFailGrant)
	r.countFailedAttempt(reRegisterFailGrant)
	r.countFailedAttempt(reRegisterFailAllocate)
	if failures(reRegisterFailGrant) != 2 || failures(reRegisterFailAllocate) != 1 || failures(reRegisterFailReclaim) != 0 {
		t.Fatalf("failures grant=%v allocate=%v reclaim=%v, want 2/1/0",
			failures(reRegisterFailGrant), failures(reRegisterFailAllocate), failures(reRegisterFailReclaim))
	}

	r.cancel() // Close 的第一步
	r.countFailedAttempt(reRegisterFailGrant)
	if failures(reRegisterFailGrant) != 2 {
		t.Fatalf("failure after close was counted: grant=%v, want 2", failures(reRegisterFailGrant))
	}
}
