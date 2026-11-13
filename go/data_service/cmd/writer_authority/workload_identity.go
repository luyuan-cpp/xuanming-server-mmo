package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 一代写者登记时记下「是谁登记的」(REGISTER 的 registered_by),缩容收尾只结束本集群登记的一代
// (docs/design/writer-roster-exhaustion-20261106.md §5.9)。
//
// 为什么要它:收尾是按「本集群里这个序号的 Pod 不在了」推断这一代没有进程了。同一份名册和注册账号被带到第二个集群时
// (集群级备份恢复、备用集群把写者缩到 0 却没停 CronJob、两个环境误共用名册),那边的 Pod 当然不在,不核归属就会把
// 正式集群在役的写者结束掉。和 K8s 垃圾回收按属主 UID、云控制器按集群标签只回收自己建的资源同一条规矩:
// 按「不在」做删除的自动化,只能动自己建的东西。换代不受这条约束:新持有者围栏旧的,不需要旧的归自己。
//
// 身份取登记进程的 ServiceAccount 连同这个对象的 UID。UID 由所在集群的 API server 在建对象时分配,按对象恢复、
// 克隆 namespace 得到的是新 UID,带不过去(整个控制面连 etcd 一起克隆除外,那已经是同一个集群的两份)。
// 注册器在运行期问 API server 得到它(SelfSubjectReview),不由部署脚本渲染:渲染进清单的值会跟着备份走。

// workloadIdentityPrefix 标明身份的来源:K8s 的 ServiceAccount。
const workloadIdentityPrefix = "k8s-sa:"

// serviceAccountUserPrefix 是 ServiceAccount 在 K8s 认证里的用户名前缀:system:serviceaccount:<namespace>:<名字>。
const serviceAccountUserPrefix = "system:serviceaccount:"

var (
	// serviceAccountNamePattern 是 ServiceAccount 名的形状(DNS-1123 subdomain,至多 253 个字符)。
	serviceAccountNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	// objectUIDPattern 是 K8s 对象 UID 的形状。
	objectUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// workloadIdentity 是登记一代的工作负载:哪个 namespace 的哪个 ServiceAccount,连同这个对象在它所在集群里的 UID。
type workloadIdentity struct {
	namespace, serviceAccount, uid string
}

// String 是写进 REGISTER.registered_by 的文本:k8s-sa:<namespace>/<ServiceAccount 名>/<UID>。
func (w workloadIdentity) String() string {
	return workloadIdentityPrefix + w.namespace + "/" + w.serviceAccount + "/" + w.uid
}

func (w workloadIdentity) valid() bool {
	return kubernetesNamePattern.MatchString(w.namespace) && serviceAccountNamePattern.MatchString(w.serviceAccount) && objectUIDPattern.MatchString(w.uid)
}

// parseWorkloadIdentity 解 registered_by。空串(集群外登记)和认不出的文本都回 false:归属说不清就当不是自己的。
func parseWorkloadIdentity(text string) (workloadIdentity, bool) {
	rest, ok := strings.CutPrefix(text, workloadIdentityPrefix)
	if !ok {
		return workloadIdentity{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return workloadIdentity{}, false
	}
	identity := workloadIdentity{namespace: parts[0], serviceAccount: parts[1], uid: parts[2]}
	return identity, identity.valid()
}

// serviceAccountIdentity 把 SelfSubjectReview 回的用户名与 UID 变成 workloadIdentity。不是 ServiceAccount、形状不对都拒绝。
func serviceAccountIdentity(username, uid string) (workloadIdentity, error) {
	rest, ok := strings.CutPrefix(username, serviceAccountUserPrefix)
	parts := strings.Split(rest, ":")
	if !ok || len(parts) != 2 {
		return workloadIdentity{}, fmt.Errorf("the API server identifies this pod as %q, not as a service account", username)
	}
	identity := workloadIdentity{namespace: parts[0], serviceAccount: parts[1], uid: uid}
	if !identity.valid() {
		return workloadIdentity{}, fmt.Errorf("the API server returned an unusable service account identity (user %q, uid %q)", username, uid)
	}
	return identity, nil
}

// registrarIdentity 是注册器登记一代时写进 registered_by 的值:在 K8s 里问 API server 得到本 Pod 的 ServiceAccount 身份,
// 集群外(本机脚本、调试栈)回空串 —— 集群外没有缩容收尾,空身份的一代不会被自动结束。
// 在 K8s 里读不到就报错、不登记:带着空身份登记的一代以后缩容时不会被自动结束,宁可现在就停在 init、让人看见原因。
func registrarIdentity(ctx context.Context, getenv func(string) string) (string, error) {
	if !inKubernetes(getenv) {
		return "", nil
	}
	client, err := inClusterClient(getenv)
	if err != nil {
		return "", fmt.Errorf("read the workload identity of this pod: %w", err)
	}
	identity, err := client.selfIdentity(ctx)
	if err != nil {
		return "", fmt.Errorf("read the workload identity of this pod: %w", err)
	}
	return identity.String(), nil
}

// errRegisteredElsewhere 标记「这一代不是本 namespace 的工作负载登记的」:缩容收尾不动它,这一轮算失败,让人来看。
var errRegisteredElsewhere = errors.New("registered by a workload outside this namespace of this cluster")
