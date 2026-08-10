package killswitch

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testMethod = "/login.LoginService/Login"

func newUnaryInfo(fullMethod string) *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: fullMethod}
}

// okHandler 记录自己有没有被调用 —— 关停生效的判据是 handler **根本没跑**。
func okHandler(called *bool) grpc.UnaryHandler {
	return func(context.Context, any) (any, error) {
		*called = true
		return "ok", nil
	}
}

func TestLookupFailOpen(t *testing.T) {
	tests := []struct {
		name  string
		setup func() *Switch
	}{
		{
			name:  "从没同步过(New 之后什么都没做)",
			setup: func() *Switch { return New(Config{}) },
		},
		{
			name: "同步成功但前缀下没有任何规则",
			setup: func() *Switch {
				s := New(Config{})
				s.SetRules(map[string]Rule{})
				return s
			},
		},
		{
			name: "有规则但没命中当前方法",
			setup: func() *Switch {
				s := New(Config{})
				s.SetRules(map[string]Rule{"other.OtherService/Foo": {Deny: true}})
				return s
			},
		},
		{
			name: "命中的规则 deny=false",
			setup: func() *Switch {
				s := New(Config{})
				s.SetRules(map[string]Rule{"login.LoginService/Login": {Deny: false}})
				return s
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.setup()
			if _, blocked := s.Lookup(testMethod); blocked {
				t.Fatal("fail-open 被破坏:该放行却关停了")
			}

			called := false
			resp, err := s.UnaryServerInterceptor()(
				context.Background(), struct{}{}, newUnaryInfo(testMethod), okHandler(&called))
			if err != nil {
				t.Fatalf("拦截器返回了错误: %v", err)
			}
			if !called {
				t.Fatal("handler 没被调用")
			}
			if resp != "ok" {
				t.Fatalf("resp = %v, 期望原样透传", resp)
			}
		})
	}
}

func TestLookupMatchPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		rules       map[string]Rule
		method      string
		wantBlocked bool
		wantReason  string
	}{
		{
			name:        "精确全限定名命中",
			rules:       map[string]Rule{"login.LoginService/Login": {Deny: true, Reason: "精确"}},
			method:      testMethod,
			wantBlocked: true,
			wantReason:  "精确",
		},
		{
			name:        "精确短服务名命中(运维手写友好)",
			rules:       map[string]Rule{"LoginService/Login": {Deny: true, Reason: "短名"}},
			method:      testMethod,
			wantBlocked: true,
			wantReason:  "短名",
		},
		{
			name:        "整服务通配命中",
			rules:       map[string]Rule{"login.LoginService/*": {Deny: true, Reason: "整服务"}},
			method:      testMethod,
			wantBlocked: true,
			wantReason:  "整服务",
		},
		{
			name:        "整服务通配(短名)命中",
			rules:       map[string]Rule{"LoginService/*": {Deny: true, Reason: "整服务短名"}},
			method:      testMethod,
			wantBlocked: true,
			wantReason:  "整服务短名",
		},
		{
			name:        "全局通配命中",
			rules:       map[string]Rule{"*": {Deny: true, Reason: "全局"}},
			method:      testMethod,
			wantBlocked: true,
			wantReason:  "全局",
		},
		{
			name: "精确优先于整服务",
			rules: map[string]Rule{
				"login.LoginService/Login": {Deny: true, Reason: "精确赢"},
				"login.LoginService/*":     {Deny: true, Reason: "整服务输"},
			},
			method:      testMethod,
			wantBlocked: true,
			wantReason:  "精确赢",
		},
		{
			name: "整服务优先于全局",
			rules: map[string]Rule{
				"login.LoginService/*": {Deny: true, Reason: "整服务赢"},
				"*":                    {Deny: true, Reason: "全局输"},
			},
			method:      testMethod,
			wantBlocked: true,
			wantReason:  "整服务赢",
		},
		{
			// 关停全服务、单独豁免一个方法 —— 这是止血时最常用的形状。
			name: "精确 deny=false 可以从整服务关停里豁免自己",
			rules: map[string]Rule{
				"login.LoginService/Login": {Deny: false},
				"login.LoginService/*":     {Deny: true, Reason: "整服务"},
			},
			method:      testMethod,
			wantBlocked: false,
		},
		{
			name: "同一批规则下,没被豁免的方法照关",
			rules: map[string]Rule{
				"login.LoginService/Login": {Deny: false},
				"login.LoginService/*":     {Deny: true, Reason: "整服务"},
			},
			method:      "/login.LoginService/CreatePlayer",
			wantBlocked: true,
			wantReason:  "整服务",
		},
		{
			name:        "别的服务不受本服务规则影响",
			rules:       map[string]Rule{"login.LoginService/*": {Deny: true}},
			method:      "/scene_manager.SceneManagerService/EnterScene",
			wantBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{})
			s.SetRules(tt.rules)

			rule, blocked := s.Lookup(tt.method)
			if blocked != tt.wantBlocked {
				t.Fatalf("Lookup(%q) blocked = %v, 期望 %v", tt.method, blocked, tt.wantBlocked)
			}
			if blocked && rule.Reason != tt.wantReason {
				t.Fatalf("命中的规则 reason = %q, 期望 %q", rule.Reason, tt.wantReason)
			}
		})
	}
}

func TestInterceptorShortCircuits(t *testing.T) {
	s := New(Config{})
	s.SetRules(map[string]Rule{
		"login.LoginService/Login": {Deny: true, Reason: "db 过载临时降级"},
	})

	called := false
	resp, err := s.UnaryServerInterceptor()(
		context.Background(), struct{}{}, newUnaryInfo(testMethod), okHandler(&called))

	if called {
		t.Fatal("关停命中时 handler 绝不能被调用")
	}
	if resp != nil {
		t.Fatalf("resp = %v, 期望 nil", resp)
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("返回的不是 gRPC status: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Fatalf("status code = %v, 期望 Unavailable", st.Code())
	}
	for _, want := range []string{"killswitch", testMethod, "db 过载临时降级"} {
		if !strings.Contains(st.Message(), want) {
			t.Errorf("错误文本里缺少 %q: %s", want, st.Message())
		}
	}
}

func TestInterceptorCustomStatusCode(t *testing.T) {
	s := New(Config{})
	s.SetRules(map[string]Rule{
		"*": {Deny: true, Code: uint32(codes.FailedPrecondition)},
	})

	called := false
	_, err := s.UnaryServerInterceptor()(
		context.Background(), struct{}{}, newUnaryInfo(testMethod), okHandler(&called))

	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code = %v, 期望 FailedPrecondition", status.Code(err))
	}
}

// TestStaleSnapshotFailsOpen 守住铁律的最后一段:
// 与 etcd 长时间失联后,本地规则必须自动作废、退回全放行。
func TestStaleSnapshotFailsOpen(t *testing.T) {
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		staleAfter  time.Duration
		elapsed     time.Duration
		wantBlocked bool
	}{
		{"刚同步完:规则生效", time.Minute, 0, true},
		{"宽限期内:规则仍生效(不因抖动松阀)", time.Minute, 59 * time.Second, true},
		{"超过宽限期:作废退回放行", time.Minute, 61 * time.Second, false},
		{"负数表示永不作废", -1, 24 * time.Hour, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			now := base

			s := New(Config{StaleAfter: tt.staleAfter})
			s.now = func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}

			s.SetRules(map[string]Rule{"*": {Deny: true}})

			mu.Lock()
			now = base.Add(tt.elapsed)
			mu.Unlock()

			if _, blocked := s.Lookup(testMethod); blocked != tt.wantBlocked {
				t.Fatalf("经过 %v 后 blocked = %v, 期望 %v", tt.elapsed, blocked, tt.wantBlocked)
			}
		})
	}
}

// TestStartWithNilClientFailsOpen:没接 etcd 是合法配置,必须放行且不阻塞。
func TestStartWithNilClientFailsOpen(t *testing.T) {
	s := New(Config{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Start(context.Background(), nil)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start 阻塞了 —— 它必须立刻返回")
	}

	if _, blocked := s.Lookup(testMethod); blocked {
		t.Fatal("没接 etcd 却关停了请求")
	}
}

// TestParseKVDropsBadValue:值坏了要整条丢弃,不能落进快照。
func TestParseKVDropsBadValue(t *testing.T) {
	s := New(Config{})

	tests := []struct {
		name   string
		key    string
		value  string
		wantOK bool
	}{
		{"正常值", DefaultPrefix + "LoginService/*", `{"deny":true}`, true},
		{"坏 JSON", DefaultPrefix + "LoginService/*", `{"deny":`, false},
		{"无法识别的裸值", DefaultPrefix + "LoginService/*", `关掉`, false},
		{"前缀不符", "/somewhere/else", `{"deny":true}`, false},
		{"模式为空", DefaultPrefix, `{"deny":true}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, ok := s.parseKV(tt.key, []byte(tt.value))
			if ok != tt.wantOK {
				t.Fatalf("parseKV ok = %v, 期望 %v", ok, tt.wantOK)
			}
		})
	}
}

func TestRulesSnapshotIsCopy(t *testing.T) {
	s := New(Config{})
	orig := map[string]Rule{"*": {Deny: true}}
	s.SetRules(orig)

	// 改调用方手里的原 map 不该影响已发布的快照。
	orig["*"] = Rule{Deny: false}
	if _, blocked := s.Lookup(testMethod); !blocked {
		t.Fatal("SetRules 没有对入参做拷贝")
	}

	// 改 Rules() 拿到的拷贝同样不该影响快照。
	got := s.Rules()
	delete(got, "*")
	if _, blocked := s.Lookup(testMethod); !blocked {
		t.Fatal("Rules() 返回的不是拷贝")
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}
	if c.prefix() != DefaultPrefix {
		t.Errorf("prefix() = %q, 期望 %q", c.prefix(), DefaultPrefix)
	}
	if c.staleAfter() != defaultStaleAfter {
		t.Errorf("staleAfter() = %v, 期望 %v", c.staleAfter(), defaultStaleAfter)
	}
	if c.resyncBackoff() != defaultResyncBackoff {
		t.Errorf("resyncBackoff() = %v, 期望 %v", c.resyncBackoff(), defaultResyncBackoff)
	}

	custom := Config{Prefix: "/x/", StaleAfter: time.Second, ResyncBackoff: 2 * time.Second}
	if custom.prefix() != "/x/" || custom.staleAfter() != time.Second ||
		custom.resyncBackoff() != 2*time.Second {
		t.Error("自定义配置没有被采纳")
	}
}

// TestLookupConcurrentWithSetRules 确认读路径与热更新可以并发跑
// (快照是 atomic.Pointer + 不可变 map,读侧零锁)。
// 用 -race 跑才有意义。
func TestLookupConcurrentWithSetRules(t *testing.T) {
	s := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			if i%2 == 0 {
				s.SetRules(map[string]Rule{"*": {Deny: true}})
			} else {
				s.SetRules(map[string]Rule{})
			}
		}
	}()

	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			s.Lookup(testMethod)
		}
	}()

	wg.Wait()
}
