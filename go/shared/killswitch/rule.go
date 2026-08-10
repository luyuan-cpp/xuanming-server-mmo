package killswitch

import (
	"encoding/json"
	"strings"

	"google.golang.org/grpc/codes"
)

// Rule 是一条关停规则,对应 etcd 里一个 key 的值。
type Rule struct {
	// Deny 为 true 才关停。缺省 false —— 这是 fail-open 的第一道保证:
	// 值解析出来是空对象、字段拼错、写成 {} 都不会误关停。
	Deny bool `json:"deny"`

	// Reason 关停原因,回给调用方并打进日志。运维填,便于事后追责。
	Reason string `json:"reason,omitempty"`

	// Code 关停时返回的 gRPC 状态码。0 或非法值一律用 Unavailable。
	// 想让客户端不重试可以填 FailedPrecondition(9)。
	Code uint32 `json:"code,omitempty"`
}

// StatusCode 返回本规则该用的 gRPC 状态码。
// 越界/未填一律 Unavailable —— "暂时不可用"是语义最保守的一档。
func (r Rule) StatusCode() codes.Code {
	c := codes.Code(r.Code)
	if r.Code == 0 || c > codes.Unauthenticated {
		return codes.Unavailable
	}
	return c
}

// ParseRule 解析 etcd 里的值。
//
// 第二个返回值为 false 表示**这条值不可信**,调用方必须整条丢弃
// —— 铁律 fail-open:宁可一条规则不生效,也不能因为运维手抖写坏了
// 一个值就把请求乱关一气。
//
// 接受两种写法:
//
//	JSON  {"deny":true,"reason":"db 过载临时降级","code":9}
//	裸值  true / 1 / on / deny / yes  以及它们的反面
//
// 裸值是给"半夜手忙脚乱敲 etcdctl put"准备的,不必记 JSON 结构。
func ParseRule(raw []byte) (Rule, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		// 空值 = 显式不关停(常见于先建 key 占位)。
		return Rule{}, true
	}

	if s[0] == '{' {
		var r Rule
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			return Rule{}, false
		}
		return r, true
	}

	switch strings.ToLower(s) {
	case "1", "true", "on", "yes", "deny":
		return Rule{Deny: true}, true
	case "0", "false", "off", "no", "allow":
		return Rule{}, true
	default:
		return Rule{}, false
	}
}

// PatternFromKey 把 etcd 的完整 key 还原成匹配模式。
//
//	prefix="/mmorpg/killswitch/"  key="/mmorpg/killswitch/login.LoginService/Login"
//	→ "login.LoginService/Login"
//
// 第二个返回值为 false 表示这个 key 不该被当成规则(前缀不符 / 模式为空)。
func PatternFromKey(prefix, key string) (string, bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	p := strings.TrimSpace(strings.TrimPrefix(key, prefix))
	// 容忍运维多敲的斜杠:.../killswitch//login.LoginService/Login
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return "", false
	}
	return p, true
}

// MatchKeys 按**从精确到宽泛**的顺序返回一个 gRPC 全方法名的候选匹配键。
//
//	"/login.LoginService/Login" →
//	    login.LoginService/Login   精确(全限定服务名)
//	    LoginService/Login         精确(短服务名,给人手写用)
//	    login.LoginService/*       整服务
//	    LoginService/*             整服务(短名)
//	    *                          全局
//
// 顺序即优先级:第一个命中的规则说了算,所以精确规则可以用
// {"deny":false} 把自己从 Service/* 或 * 里豁免出来。
func MatchKeys(fullMethod string) []string {
	m := strings.TrimPrefix(fullMethod, "/")
	if m == "" {
		return []string{globalPattern}
	}

	slash := strings.LastIndex(m, "/")
	if slash <= 0 || slash == len(m)-1 {
		// 不是 "服务/方法" 形状,只能整条精确匹配或走全局。
		return []string{m, globalPattern}
	}

	svcFull, method := m[:slash], m[slash+1:]
	svcShort := svcFull
	if dot := strings.LastIndex(svcFull, "."); dot >= 0 {
		svcShort = svcFull[dot+1:]
	}

	keys := make([]string, 0, 5)
	keys = append(keys, svcFull+"/"+method)
	if svcShort != svcFull && svcShort != "" {
		keys = append(keys, svcShort+"/"+method)
	}
	keys = append(keys, svcFull+"/*")
	if svcShort != svcFull && svcShort != "" {
		keys = append(keys, svcShort+"/*")
	}
	return append(keys, globalPattern)
}

const globalPattern = "*"
