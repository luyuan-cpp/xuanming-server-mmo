package killswitch

import (
	"reflect"
	"testing"
)

// 范围规则与受保护方法(docs/design/rpc-kill-switch.md §4、§5、§8),与 C++ kill_switch_test.cpp 同一组口径。

func TestSplitScope(t *testing.T) {
	tests := []struct {
		pattern    string
		wantKind   scopeKind
		wantValue  string
		wantTarget string
		wantOK     bool
	}{
		{"login.LoginService/Login", scopeAll, "", "login.LoginService/Login", true},
		{"*", scopeAll, "", "*", true},
		{"type/LoginNodeService/*", scopeNodeType, "LoginNodeService", "*", true},
		{"type/LoginNodeService/LoginService/Login", scopeNodeType, "LoginNodeService", "LoginService/Login", true},
		{"instance/uuid-1/LoginService/*", scopeInstance, "uuid-1", "LoginService/*", true},
		{"type/", scopeNodeType, "", "", false},
		{"type/LoginNodeService", scopeNodeType, "", "", false},
		{"type/LoginNodeService/", scopeNodeType, "", "", false},
		{"instance//*", scopeInstance, "", "", false},
		{"", scopeAll, "", "", false},
	}
	for _, tt := range tests {
		kind, value, target, ok := splitScope(tt.pattern)
		if ok != tt.wantOK {
			t.Fatalf("splitScope(%q) ok = %v, 期望 %v", tt.pattern, ok, tt.wantOK)
		}
		if !ok {
			continue
		}
		if kind != tt.wantKind || value != tt.wantValue || target != tt.wantTarget {
			t.Fatalf("splitScope(%q) = (%v, %q, %q), 期望 (%v, %q, %q)",
				tt.pattern, kind, value, target, tt.wantKind, tt.wantValue, tt.wantTarget)
		}
	}
}

func TestExactKeys(t *testing.T) {
	if got, want := ExactKeys("/login.LoginService/Login"), []string{"login.LoginService/Login", "LoginService/Login"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ExactKeys = %v, 期望 %v", got, want)
	}
	if got, want := ExactKeys("/Scene/CreateScene"), []string{"Scene/CreateScene"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ExactKeys = %v, 期望 %v", got, want)
	}
}

func scopedSwitch(rules map[string]Rule) *Switch {
	s := New(Config{NodeTypes: []string{"LoginNodeService"}})
	s.AddInstance("uuid-a")
	s.SetRules(rules)
	return s
}

func TestLookupScopePrecedence(t *testing.T) {
	tests := []struct {
		name        string
		rules       map[string]Rule
		wantBlocked bool
	}{
		{"类型整体关停", map[string]Rule{"type/LoginNodeService/*": {Deny: true}}, true},
		{"实例整体关停", map[string]Rule{"instance/uuid-a/*": {Deny: true}}, true},
		{"类型关停、本实例豁免", map[string]Rule{
			"type/LoginNodeService/*": {Deny: true},
			"instance/uuid-a/*":       {Deny: false},
		}, false},
		{"实例关停先于全局的方法豁免", map[string]Rule{
			"instance/uuid-a/*":        {Deny: true},
			"login.LoginService/Login": {Deny: false},
		}, true},
		{"类型范围的方法豁免先于全局关停", map[string]Rule{
			"type/LoginNodeService/LoginService/Login": {Deny: false},
			"*": {Deny: true},
		}, false},
		{"别的节点类型的规则不认", map[string]Rule{"type/ChatNodeService/*": {Deny: true}}, false},
		{"别的进程的规则不认", map[string]Rule{"instance/uuid-other/*": {Deny: true}}, false},
		{"范围段残缺的键不是规则", map[string]Rule{"type/LoginNodeService": {Deny: true}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, blocked := scopedSwitch(tt.rules).Lookup(testMethod); blocked != tt.wantBlocked {
				t.Fatalf("blocked = %v, 期望 %v", blocked, tt.wantBlocked)
			}
		})
	}
}

func TestAddInstanceActivatesInstanceRules(t *testing.T) {
	s := New(Config{})
	s.SetRules(map[string]Rule{"instance/uuid-a/*": {Deny: true}})
	if _, blocked := s.Lookup(testMethod); blocked {
		t.Fatal("还没登记 uuid,实例规则不该生效")
	}
	if got := s.applicableCount(s.Rules()); got != 0 {
		t.Fatalf("applicableCount = %d, 期望 0", got)
	}
	s.AddInstance("uuid-a")
	s.AddInstance("uuid-a") // 重复登记无副作用
	s.AddInstance("")       // 空串忽略
	if _, blocked := s.Lookup(testMethod); !blocked {
		t.Fatal("登记 uuid 之后实例规则应当生效")
	}
	if got := s.applicableCount(s.Rules()); got != 1 {
		t.Fatalf("applicableCount = %d, 期望 1", got)
	}
}

func TestSameLayerConflictPrefersDeny(t *testing.T) {
	s := New(Config{NodeTypes: []string{"MatchNodeService", "TeamNodeService"}})
	s.SetRules(map[string]Rule{
		"type/MatchNodeService/LoginService/*": {Deny: false},
		"type/TeamNodeService/LoginService/*":  {Deny: true},
	})
	if _, blocked := s.Lookup(testMethod); !blocked {
		t.Fatal("同一进程的两个身份写了相反的规则时应当取关停")
	}
}

func TestProtectedMethodsIgnoreWildcards(t *testing.T) {
	s := New(Config{NodeTypes: []string{"LoginNodeService"}, Protected: []string{testMethod}})
	s.SetRules(map[string]Rule{"*": {Deny: true}, "type/LoginNodeService/LoginService/*": {Deny: true}})
	if _, blocked := s.Lookup(testMethod); blocked {
		t.Fatal("受保护的方法不该被通配规则关掉")
	}
	if _, blocked := s.Lookup("/login.LoginService/Other"); !blocked {
		t.Fatal("同一服务里不受保护的方法照常被通配规则关掉")
	}
	s.SetRules(map[string]Rule{"type/LoginNodeService/LoginService/Login": {Deny: true}})
	if _, blocked := s.Lookup(testMethod); !blocked {
		t.Fatal("精确到方法的规则应当能关掉受保护的方法")
	}
}

func TestDenyMessageFormat(t *testing.T) {
	if got, want := denyMessage(testMethod, Rule{Deny: true}), "killswitch: method "+testMethod+" is disabled"; got != want {
		t.Fatalf("denyMessage = %q, 期望 %q", got, want)
	}
	if got, want := denyMessage(testMethod, Rule{Deny: true, Reason: "db overload"}),
		"killswitch: method "+testMethod+" is disabled, reason: db overload"; got != want {
		t.Fatalf("denyMessage = %q, 期望 %q", got, want)
	}
}
