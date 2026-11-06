package gatepick

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 停写判定(StaleAfter = 3 × ReportPeriod)的前提是三件 C++ 侧的事实,这里逐字核对源码把它们钉住:
//  1. gate 的人数上报定时器周期等于 ReportPeriod(cpp/nodes/gate/main.cpp 的 playerCountReportTimer.RunEvery);
//  2. 定时器回调每次都调 UpdateNodeInfo(),中间没有「人数没变就不写」之类的条件;
//  3. UpdateNodeInfo 走 EtcdHelper::PutServiceNodeInfo,后者发的是普通 Put(不是比较后写入):值不变也是一次新写,
//     ModRevision 照样前进。
//
// 任何一条变了(拉长上报周期、改成人数变了才写、改成比较后写入),login 的 gate 目录镜像与 scene_manager 的
// RevisionTracker 都会把健康的 gate(尤其是人数不变的空闲 gate)判成停写:gate 多的 zone,新玩家被挤到刚写过的
// 那几台上;小 zone 整区回落,误触发 LoginGatePickFallbackStale。全程不报错,所以改 C++ 时让这里先失败,
// 提醒同批改 ReportPeriod 与停写判定。
//
// 找不到 C++ 源码(只拷了 go/ 的构建环境)时跳过;验收时跳过算未通过。
func TestReportPeriodMatchesCppGateReporter(t *testing.T) {
	gateMain := readRepoFile(t, "cpp/nodes/gate/main.cpp")
	m := regexp.MustCompile(`playerCountReportTimer\.RunEvery\(\s*([0-9]+(?:\.[0-9]+)?)\s*,\s*\[[^\]]*\]\s*\{([^}]*)\}`).
		FindStringSubmatch(gateMain)
	if m == nil {
		t.Fatal("cpp/nodes/gate/main.cpp 里找不到 playerCountReportTimer.RunEvery(<秒>, [...] { ... }):" +
			"上报定时器换了写法,先核对停写判定的三条前提,再改这里")
	}
	seconds, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("上报周期 %q 不是数字: %v", m[1], err)
	}
	if got := time.Duration(seconds * float64(time.Second)); got != ReportPeriod {
		t.Fatalf("C++ gate 上报周期 = %s,gatepick.ReportPeriod = %s:两边要同批改(StaleAfter 与 login 镜像的时限都按它推)",
			got, ReportPeriod)
	}
	body := m[2]
	if !strings.Contains(body, "GetEtcdManager().UpdateNodeInfo()") {
		t.Fatalf("上报定时器回调不再调 UpdateNodeInfo(),gate 不会每个周期重写注册记录:\n%s", body)
	}
	if regexp.MustCompile(`\bif\s*\(|\breturn\b|\?`).MatchString(body) {
		t.Fatalf("上报定时器回调里出现了条件分支,不再是每个周期无条件重写一次注册记录:\n%s", body)
	}

	etcdManager := readRepoFile(t, "cpp/libs/engine/core/node/system/etcd/etcd_manager.cpp")
	update := cppFunctionBody(t, etcdManager, "void EtcdManager::UpdateNodeInfo()")
	if !strings.Contains(update, "EtcdHelper::PutServiceNodeInfo(") {
		t.Fatalf("EtcdManager::UpdateNodeInfo 不再走 EtcdHelper::PutServiceNodeInfo:\n%s", update)
	}

	etcdHelper := readRepoFile(t, "cpp/libs/engine/core/node/system/etcd/etcd_helper.cpp")
	put := cppFunctionBody(t, etcdHelper, "void EtcdHelper::PutServiceNodeInfo(")
	if !strings.Contains(put, "etcdserverpb::PutRequest request;") || !strings.Contains(put, "->Put(request)") {
		t.Fatalf("EtcdHelper::PutServiceNodeInfo 不再发普通 Put:\n%s", put)
	}
	if strings.Contains(put, "Txn") || strings.Contains(put, "Compare") {
		t.Fatalf("EtcdHelper::PutServiceNodeInfo 改成了比较后写入,值不变时可能不再产生新的 ModRevision:\n%s", put)
	}
}

// readRepoFile 读仓库根下的文件(本包在 go/shared/gatepick,仓库根在上三级)。找不到时跳过:只拷了 go/ 的构建环境。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过跨语言检查")
	}
	if err != nil {
		t.Fatalf("读 %s: %v", rel, err)
	}
	return string(data)
}

// cppFunctionBody 取 C++ 源码里从 signature 起、到第一个顶格 `}` 为止的函数体。本仓库 C++ 函数的右括号都顶格,
// 函数体里的块都有缩进,所以第一个顶格 `}` 就是函数结尾。找不到时让用例失败:函数改名或挪走了,要先核对前提。
func cppFunctionBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("C++ 源码里找不到 %q", signature)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("找不到 %q 的函数结尾", signature)
	}
	return rest[:end]
}
