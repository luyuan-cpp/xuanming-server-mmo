package releasetrack

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestNamesMatchCppHeader:环境变量名与 metadata 键名是 Go 与 C++ 共用的契约,两边各写一份字面量。
// C++ gate 直连模式写 kMetadataKey,Go 的路由服与 match 读 MetadataKey,拼错一个字母就会让金丝雀会话的战斗悄悄落到 stable,
// 而且全程没有任何报错(设计文档 §2.6)。这里直接读 release_track.h 核对。
func TestNamesMatchCppHeader(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "cpp", "libs", "engine", "core", "node", "system", "node", "release_track.h"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过跨语言检查")
	}
	if err != nil {
		t.Fatalf("读 release_track.h: %v", err)
	}
	header := string(data)
	for _, c := range []struct{ cppName, want string }{
		{cppName: "kEnvVar", want: EnvVar},
		{cppName: "kMetadataKey", want: MetadataKey},
	} {
		m := regexp.MustCompile(`inline constexpr const char \*` + c.cppName + `\s*=\s*"([^"]*)"`).FindStringSubmatch(header)
		if m == nil {
			t.Fatalf("release_track.h 里找不到 %s 的定义:写法变了要同步本用例", c.cppName)
		}
		if m[1] != c.want {
			t.Fatalf("C++ %s = %q,Go 侧是 %q:两边要同批改", c.cppName, m[1], c.want)
		}
	}
}
