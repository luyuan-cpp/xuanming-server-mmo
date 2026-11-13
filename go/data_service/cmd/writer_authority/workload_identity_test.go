package main

import (
	"strings"
	"testing"
)

// registered_by 的文本形状:k8s-sa:<namespace>/<ServiceAccount 名>/<UID>,写出去再解回来不变;
// 空串(集群外登记)和认不出的文本都解不出身份 —— 缩容收尾把解不出的当成「不是自己的」。
func TestWorkloadIdentityTextRoundTrips(t *testing.T) {
	identity := workloadIdentity{namespace: namespaceForTest, serviceAccount: serviceAccountForTest, uid: serviceAccountUIDForTest}
	text := identity.String()
	if text != "k8s-sa:mmorpg-zone-1/default/0f8fad5b-d9cb-469f-a165-70867728950e" {
		t.Fatalf("身份文本: %s", text)
	}
	if parsed, ok := parseWorkloadIdentity(text); !ok || parsed != identity {
		t.Fatalf("解回来要相同: %+v %v", parsed, ok)
	}
	dotted := workloadIdentity{namespace: "mmorpg-infra", serviceAccount: "data-service.v2", uid: serviceAccountUIDForTest}
	if parsed, ok := parseWorkloadIdentity(dotted.String()); !ok || parsed != dotted {
		t.Fatalf("ServiceAccount 名可以带点: %+v %v", parsed, ok)
	}
	for name, bad := range map[string]string{
		"空串":           "",
		"没有前缀":         "mmorpg-zone-1/default/" + serviceAccountUIDForTest,
		"别的来源":         "node:worker-1",
		"少一段":          "k8s-sa:mmorpg-zone-1/default",
		"多一段":          "k8s-sa:mmorpg-zone-1/default/" + serviceAccountUIDForTest + "/extra",
		"namespace 大写": "k8s-sa:Zone/default/" + serviceAccountUIDForTest,
		"名字为空":         "k8s-sa:mmorpg-zone-1//" + serviceAccountUIDForTest,
		"UID 不成形":      "k8s-sa:mmorpg-zone-1/default/not-a-uid",
		"UID 大写":       "k8s-sa:mmorpg-zone-1/default/" + strings.ToUpper(serviceAccountUIDForTest),
		"前后有空白":        " " + text,
	} {
		if _, ok := parseWorkloadIdentity(bad); ok {
			t.Fatalf("%s 不该解出身份: %q", name, bad)
		}
	}
}

// SelfSubjectReview 回的用户名是 system:serviceaccount:<namespace>:<名字>,UID 是这个 ServiceAccount 对象的 UID。
func TestServiceAccountIdentityFromAuthenticatedUser(t *testing.T) {
	identity, err := serviceAccountIdentity("system:serviceaccount:mmorpg-zone-1:default", serviceAccountUIDForTest)
	if err != nil || identity.String() != workloadIdentityForTest {
		t.Fatalf("ServiceAccount 身份: %q %v", identity.String(), err)
	}
	for name, user := range map[string][2]string{
		"普通用户":          {"kubernetes-admin", serviceAccountUIDForTest},
		"节点":            {"system:node:worker-1", serviceAccountUIDForTest},
		"缺名字":           {"system:serviceaccount:mmorpg-zone-1", serviceAccountUIDForTest},
		"多一段":           {"system:serviceaccount:mmorpg-zone-1:default:extra", serviceAccountUIDForTest},
		"没有 UID":        {"system:serviceaccount:mmorpg-zone-1:default", ""},
		"namespace 不合法": {"system:serviceaccount:Bad_Namespace:default", serviceAccountUIDForTest},
	} {
		if _, err := serviceAccountIdentity(user[0], user[1]); err == nil {
			t.Fatalf("%s 不是可用的 ServiceAccount 身份", name)
		}
	}
}
