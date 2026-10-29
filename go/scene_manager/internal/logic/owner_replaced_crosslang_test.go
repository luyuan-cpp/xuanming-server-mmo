package logic

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"shared/savebarrier"
)

// GO-3(cross-zone-scene-travel.md §13.10)判「持有进程已被顶替」的前提是三件跨语言的事实,这里把它们钉住:
//  1. 交接资格存在 save_barrier hash 的 handoff_fence 字段(C++ handoff_sql_barrier::kFenceField);
//  2. 资格原文第二段写的是本进程的 NodeInfo.node_uuid(role_admission_system.cpp 的 ReadLocalNode 取
//     GetNodeInfo().node_uuid(),登记候选按 {ownerEpoch, local.instanceId, ...} 编码);
//  3. 同一个 node_uuid 以 nodeUuid 出现在 etcd 注册 JSON 里(NodeInfo 的 MessageToJsonString,字段 node_uuid)。
//
// 任何一条变了(换字段名、实例号改用别的来源、注册不再带 UUID),判定会静默退化成「永远没有证据」或误判,
// 所以改动时这里先失败。找不到 C++ 源码(只拷了 go/ 的构建环境)时跳过。
func TestOwnerReplacedEvidenceMatchesCppFenceStamp(t *testing.T) {
	read := func(rel string) string {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(crossLangRepoRoot, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过跨语言检查")
		}
		require.NoError(t, err)
		return string(src)
	}
	barrier := read("cpp/libs/services/scene/player/system/handoff_sql_barrier.h")
	require.Regexp(t, regexp.MustCompile(`kFenceField\s*=\s*"`+regexp.QuoteMeta(savebarrier.HandoffFenceField)+`"`), barrier,
		"C++ 交接资格字段名与 savebarrier.HandoffFenceField 不一致")

	admission := read("cpp/libs/services/scene/player/system/role_admission_system.cpp")
	require.Regexp(t, regexp.MustCompile(`return\s*\{\s*GetNodeInfo\(\)\.node_uuid\(\)\s*,`), admission,
		"ReadLocalNode 的实例号不再取 NodeInfo.node_uuid:资格章与 etcd 注册的实例号对不上")
	require.Regexp(t, regexp.MustCompile(`EncodeFence\(\{\s*context\.ownerEpoch\s*,\s*local\.instanceId\s*,`), admission,
		"登记候选不再把本进程实例号写在资格原文第二段")

	common := read("proto/common/base/common.proto")
	require.Regexp(t, regexp.MustCompile(`(?m)^\s*string\s+node_uuid\s*=\s*8\s*;`), common,
		"NodeInfo.node_uuid 变了:etcd 注册 JSON 的 nodeUuid 不再是进程实例号")
}
