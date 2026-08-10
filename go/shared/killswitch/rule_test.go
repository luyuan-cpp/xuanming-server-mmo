package killswitch

import (
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
)

func TestParseRule(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Rule
		wantOK  bool
		comment string
	}{
		{name: "空值 = 显式不关停", raw: "", want: Rule{}, wantOK: true},
		{name: "空白也是空值", raw: "  \n\t ", want: Rule{}, wantOK: true},

		{name: "JSON 开启", raw: `{"deny":true}`, want: Rule{Deny: true}, wantOK: true},
		{
			name:   "JSON 带原因与状态码",
			raw:    `{"deny":true,"reason":"db 过载临时降级","code":9}`,
			want:   Rule{Deny: true, Reason: "db 过载临时降级", Code: 9},
			wantOK: true,
		},
		{name: "JSON 关闭", raw: `{"deny":false}`, want: Rule{}, wantOK: true},
		{
			name:    "空 JSON 对象不关停",
			raw:     `{}`,
			want:    Rule{},
			wantOK:  true,
			comment: "fail-open:字段拼错/漏写都不该误关停",
		},
		{
			name:    "字段名写错也不关停",
			raw:     `{"denied":true}`,
			want:    Rule{},
			wantOK:  true,
			comment: "同上",
		},

		{name: "裸值 true", raw: "true", want: Rule{Deny: true}, wantOK: true},
		{name: "裸值 1", raw: "1", want: Rule{Deny: true}, wantOK: true},
		{name: "裸值 on 大小写不敏感", raw: "ON", want: Rule{Deny: true}, wantOK: true},
		{name: "裸值 deny", raw: "deny", want: Rule{Deny: true}, wantOK: true},
		{name: "裸值 yes", raw: "yes", want: Rule{Deny: true}, wantOK: true},
		{name: "裸值 false", raw: "false", want: Rule{}, wantOK: true},
		{name: "裸值 0", raw: "0", want: Rule{}, wantOK: true},
		{name: "裸值 off", raw: "off", want: Rule{}, wantOK: true},
		{name: "裸值两侧空白被忽略", raw: "  true\n", want: Rule{Deny: true}, wantOK: true},

		{
			name:    "坏 JSON 整条丢弃",
			raw:     `{"deny":tru`,
			wantOK:  false,
			comment: "fail-open:值坏了就当没这条规则",
		},
		{
			name:    "无法识别的裸值整条丢弃",
			raw:     "关掉它",
			wantOK:  false,
			comment: "同上",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRule([]byte(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ParseRule(%q) ok = %v, 期望 %v (%s)", tt.raw, ok, tt.wantOK, tt.comment)
			}
			if ok && got != tt.want {
				t.Fatalf("ParseRule(%q) = %+v, 期望 %+v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRuleStatusCode(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want codes.Code
	}{
		{"未填 → Unavailable", Rule{Deny: true}, codes.Unavailable},
		{"显式 FailedPrecondition", Rule{Deny: true, Code: uint32(codes.FailedPrecondition)}, codes.FailedPrecondition},
		{"显式 ResourceExhausted", Rule{Deny: true, Code: uint32(codes.ResourceExhausted)}, codes.ResourceExhausted},
		{"越界码退回 Unavailable", Rule{Deny: true, Code: 999}, codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rule.StatusCode(); got != tt.want {
				t.Fatalf("StatusCode() = %v, 期望 %v", got, tt.want)
			}
		})
	}
}

func TestPatternFromKey(t *testing.T) {
	const prefix = DefaultPrefix

	tests := []struct {
		name   string
		key    string
		want   string
		wantOK bool
	}{
		{"精确方法", prefix + "login.LoginService/Login", "login.LoginService/Login", true},
		{"整服务", prefix + "LoginService/*", "LoginService/*", true},
		{"全局", prefix + "*", "*", true},
		{"多敲的斜杠被吃掉", prefix + "//LoginService/*", "LoginService/*", true},
		{"两侧空白被吃掉", prefix + " LoginService/* ", "LoginService/*", true},
		{"前缀不符", "/other/LoginService/*", "", false},
		{"前缀本身(模式为空)", prefix, "", false},
		{"只有斜杠", prefix + "///", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PatternFromKey(prefix, tt.key)
			if ok != tt.wantOK {
				t.Fatalf("PatternFromKey(%q) ok = %v, 期望 %v", tt.key, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("PatternFromKey(%q) = %q, 期望 %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestMatchKeys(t *testing.T) {
	tests := []struct {
		name       string
		fullMethod string
		want       []string
	}{
		{
			name:       "标准全方法名:五级候选,从精确到全局",
			fullMethod: "/login.LoginService/Login",
			want: []string{
				"login.LoginService/Login",
				"LoginService/Login",
				"login.LoginService/*",
				"LoginService/*",
				"*",
			},
		},
		{
			name:       "服务名不带包路径时不重复候选",
			fullMethod: "/LoginService/Login",
			want: []string{
				"LoginService/Login",
				"LoginService/*",
				"*",
			},
		},
		{
			name:       "多级包路径只取最后一段做短名",
			fullMethod: "/a.b.c.SceneManagerService/EnterScene",
			want: []string{
				"a.b.c.SceneManagerService/EnterScene",
				"SceneManagerService/EnterScene",
				"a.b.c.SceneManagerService/*",
				"SceneManagerService/*",
				"*",
			},
		},
		{name: "空方法名只剩全局", fullMethod: "", want: []string{"*"}},
		{name: "只有斜杠只剩全局", fullMethod: "/", want: []string{"*"}},
		{name: "不是服务/方法形状", fullMethod: "/Ping", want: []string{"Ping", "*"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchKeys(tt.fullMethod)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("MatchKeys(%q) = %v, 期望 %v", tt.fullMethod, got, tt.want)
			}
		})
	}
}
