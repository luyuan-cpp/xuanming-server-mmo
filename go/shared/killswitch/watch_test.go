package killswitch

import (
	"testing"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func putEvent(key, value string) *clientv3.Event {
	return &clientv3.Event{
		Type: clientv3.EventTypePut,
		Kv:   &mvccpb.KeyValue{Key: []byte(key), Value: []byte(value)},
	}
}

func deleteEvent(key string) *clientv3.Event {
	return &clientv3.Event{
		Type: clientv3.EventTypeDelete,
		Kv:   &mvccpb.KeyValue{Key: []byte(key)},
	}
}

func TestApplyEvents(t *testing.T) {
	const p = DefaultPrefix

	tests := []struct {
		name        string
		start       map[string]Rule
		events      []*clientv3.Event
		wantRules   map[string]Rule
		wantChanged bool
	}{
		{
			name:        "PUT 新增一条",
			start:       map[string]Rule{},
			events:      []*clientv3.Event{putEvent(p+"LoginService/*", `{"deny":true,"reason":"止血"}`)},
			wantRules:   map[string]Rule{"LoginService/*": {Deny: true, Reason: "止血"}},
			wantChanged: true,
		},
		{
			name:        "PUT 裸值同样生效",
			start:       map[string]Rule{},
			events:      []*clientv3.Event{putEvent(p+"*", "true")},
			wantRules:   map[string]Rule{"*": {Deny: true}},
			wantChanged: true,
		},
		{
			name:        "PUT 覆盖既有条目",
			start:       map[string]Rule{"LoginService/*": {Deny: true}},
			events:      []*clientv3.Event{putEvent(p+"LoginService/*", `{"deny":false}`)},
			wantRules:   map[string]Rule{"LoginService/*": {Deny: false}},
			wantChanged: true,
		},
		{
			name:        "PUT 内容没变则不算变化(避免无谓发布快照)",
			start:       map[string]Rule{"LoginService/*": {Deny: true}},
			events:      []*clientv3.Event{putEvent(p+"LoginService/*", `{"deny":true}`)},
			wantRules:   map[string]Rule{"LoginService/*": {Deny: true}},
			wantChanged: false,
		},
		{
			name:        "DELETE 移除条目",
			start:       map[string]Rule{"LoginService/*": {Deny: true}},
			events:      []*clientv3.Event{deleteEvent(p + "LoginService/*")},
			wantRules:   map[string]Rule{},
			wantChanged: true,
		},
		{
			name:        "DELETE 不存在的条目不算变化",
			start:       map[string]Rule{},
			events:      []*clientv3.Event{deleteEvent(p + "LoginService/*")},
			wantRules:   map[string]Rule{},
			wantChanged: false,
		},
		{
			// fail-open 的关键一条:值写坏了要当这条事件没发生,
			// **保留原有规则**,绝不能把它解析成"关停"或把好规则冲掉。
			name:        "PUT 坏值被忽略,原规则原样保留",
			start:       map[string]Rule{"LoginService/*": {Deny: true, Reason: "原值"}},
			events:      []*clientv3.Event{putEvent(p+"LoginService/*", `{"deny":tru`)},
			wantRules:   map[string]Rule{"LoginService/*": {Deny: true, Reason: "原值"}},
			wantChanged: false,
		},
		{
			name:        "前缀不符的 key 被忽略",
			start:       map[string]Rule{},
			events:      []*clientv3.Event{putEvent("/other/LoginService/*", "true")},
			wantRules:   map[string]Rule{},
			wantChanged: false,
		},
		{
			name:        "nil 事件与空 Kv 不炸",
			start:       map[string]Rule{},
			events:      []*clientv3.Event{nil, {Type: clientv3.EventTypePut}},
			wantRules:   map[string]Rule{},
			wantChanged: false,
		},
		{
			name:  "一批事件:新增两条 + 删一条",
			start: map[string]Rule{"old/*": {Deny: true}},
			events: []*clientv3.Event{
				putEvent(p+"LoginService/*", "true"),
				putEvent(p+"*", `{"deny":true,"code":9}`),
				deleteEvent(p + "old/*"),
			},
			wantRules: map[string]Rule{
				"LoginService/*": {Deny: true},
				"*":              {Deny: true, Code: 9},
			},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{})
			rules := make(map[string]Rule, len(tt.start))
			for k, v := range tt.start {
				rules[k] = v
			}

			changed := s.applyEvents(rules, tt.events)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, 期望 %v", changed, tt.wantChanged)
			}
			if len(rules) != len(tt.wantRules) {
				t.Fatalf("规则表 = %+v, 期望 %+v", rules, tt.wantRules)
			}
			for k, want := range tt.wantRules {
				got, ok := rules[k]
				if !ok {
					t.Fatalf("规则表缺少 %q: %+v", k, rules)
				}
				if got != want {
					t.Fatalf("规则 %q = %+v, 期望 %+v", k, got, want)
				}
			}
		})
	}
}

// TestApplyEventsThenLookup 走一遍"事件进来 → 发布快照 → 读判定"的全链,
// 只是把 etcd 那一段换成手造事件。
func TestApplyEventsThenLookup(t *testing.T) {
	s := New(Config{})
	rules := map[string]Rule{}

	// 关停整个 LoginService。
	if !s.applyEvents(rules, []*clientv3.Event{
		putEvent(DefaultPrefix+"login.LoginService/*", `{"deny":true,"reason":"止血"}`),
	}) {
		t.Fatal("PUT 事件应当产生变化")
	}
	s.SetRules(rules)

	if _, blocked := s.Lookup(testMethod); !blocked {
		t.Fatal("规则已下发,却没有关停")
	}

	// 撤掉规则。
	if !s.applyEvents(rules, []*clientv3.Event{
		deleteEvent(DefaultPrefix + "login.LoginService/*"),
	}) {
		t.Fatal("DELETE 事件应当产生变化")
	}
	s.SetRules(rules)

	if _, blocked := s.Lookup(testMethod); blocked {
		t.Fatal("规则已撤销,却仍在关停")
	}
}
