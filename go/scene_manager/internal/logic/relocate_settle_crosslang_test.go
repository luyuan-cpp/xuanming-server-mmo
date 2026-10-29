package logic

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// CPP-3(docs/design/cross-zone-scene-travel.md §13.8)的跨配置契约。
//
// C++ relocate_confirm.h 的 kSettleAfterCallDeadline 估计的是:C++ 侧 gRPC deadline 到期之后,scene_manager 的
// EnterScene handler 最多还会再跑多久(go-zero 的超时拦截器只是提前返回、不等 handler,推路由又用
// context.Background(),所以最多再做一次 Kafka 同步写与失败回滚)。这个上限就是本服务的 KafkaWriteTimeoutSeconds。
// 那边只有一句注释「调大 KafkaWriteTimeoutSeconds 必须同步本常量」;这里把它变成可执行的检查:
// 有效的 KafkaWriteTimeoutSeconds(etc 里写了就用它,没写取 config.go 的缺省值)不得大于 C++ 常量。
// 调小 C++ 常量、或调大这边的超时而没同步,settle 窗口就会短于 handler 还可能改 location 的时间,
// 待确认表会在落点还没定的时候去核实,误踢玩家。
//
// K8s 的 scene-manager ConfigMap 从同一份 etc 镜像这个值(tools/scripts/k8s_deploy.ps1 的
// Get-AuthoritativeScalar),不另查。找不到 C++ 源码(只拷了 go/ 的构建环境)时跳过。
//
// 本检查守的是"数字没被单边调大",不证明 settle 窗口一定盖住 handler:kafka-go 在冷连接池或 topic 元数据还没缓存时,
// 元数据查询不受 WriteTimeout 约束;相等时也没给失败回滚的 EVAL 留余量。这两条记在 relocate_confirm.h 的已知残余里。
func TestRelocateSettleWindowCoversKafkaWriteTimeout(t *testing.T) {
	header, err := os.ReadFile(filepath.Join(crossLangRepoRoot,
		filepath.FromSlash("cpp/libs/services/scene/player/system/relocate_confirm.h")))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过跨配置检查")
	}
	require.NoError(t, err)
	// 连类型一起钉住:哪天改成 milliseconds,这里会按设计报"找不到",不会拿毫秒数和秒数比而误绿。
	settleSeconds := firstIntSubmatchForTest(t, `std::chrono::seconds\s+kSettleAfterCallDeadline\{(\d+)\}`, string(header),
		"relocate_confirm.h 里以 std::chrono::seconds 声明的 kSettleAfterCallDeadline")

	configSrc, err := os.ReadFile(filepath.Join("..", "config", "config.go"))
	require.NoError(t, err)
	kafkaWriteTimeout := firstIntSubmatchForTest(t, `KafkaWriteTimeoutSeconds\s+int64\s+`+"`"+`json:",default=(\d+)"`+"`",
		string(configSrc), "config.go 里 KafkaWriteTimeoutSeconds 的缺省值")

	etc, err := os.ReadFile(filepath.Join("..", "..", "etc", "scene_manager_service.yaml"))
	require.NoError(t, err)
	// go-zero 读配置时键名不区分大小写:先按不区分大小写判断键在不在,在的话必须是能解析的裸十进制整数,
	// 否则用例失败 —— 不能悄悄退回缺省值,拿缺省值去比会误绿。
	if regexp.MustCompile(`(?im)^\s*KafkaWriteTimeoutSeconds\s*:`).MatchString(string(etc)) {
		m := regexp.MustCompile(`(?im)^\s*KafkaWriteTimeoutSeconds\s*:\s*(\d+)\s*(#.*)?$`).FindStringSubmatch(string(etc))
		require.NotNil(t, m, "scene_manager_service.yaml 的 KafkaWriteTimeoutSeconds 无法解析,请写成裸十进制整数")
		v, convErr := strconv.Atoi(m[1])
		require.NoError(t, convErr)
		kafkaWriteTimeout = v
	}

	require.Positive(t, kafkaWriteTimeout)
	require.LessOrEqualf(t, kafkaWriteTimeout, settleSeconds,
		"KafkaWriteTimeoutSeconds=%d 大于 C++ relocate_confirm::kSettleAfterCallDeadline=%ds:两边必须同步调整",
		kafkaWriteTimeout, settleSeconds)
}

// firstIntSubmatchForTest 返回 pattern 在 src 里第一处匹配的第一个捕获组(十进制整数)。找不到时让用例失败:
// 常量改名或改写法也要被发现,不能悄悄变成「没东西可比」。
func firstIntSubmatchForTest(t *testing.T, pattern, src, what string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	require.NotNilf(t, m, "找不到 %s(模式 %s)", what, pattern)
	v, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	return v
}
