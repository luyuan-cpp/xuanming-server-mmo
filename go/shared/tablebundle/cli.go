package tablebundle

import (
	"fmt"
	"os"
	"strings"

	"shared/generated/table"
)

// ValidateTablesDir 执行发布前第 2 道预检(--validate-tables <dir>,设计 §5.4):
// 核对 manifest 第 2 版与文件摘要、整批解析全部表、执行服务专属校验钩子,不修改现役表、不连任何外部依赖。
func ValidateTablesDir(dir string, useBinary bool, prepareHook PrepareHook) (*BundleManifest, error) {
	manifest, err := VerifyTableDirManifest(dir, useBinary, table.SchemaDigest)
	if err != nil {
		return nil, fmt.Errorf("manifest verification failed: %w", err)
	}
	ts, err := table.Prepare(dir, useBinary)
	if err != nil {
		return nil, fmt.Errorf("table parse failed: %w", err)
	}
	ts.Identity = table.BundleIdentity{
		SchemaDigest:  table.SchemaDigest,
		ContentDigest: manifest.ContentDigest,
		Version:       manifest.Version,
		Source:        "bundle",
	}
	if prepareHook != nil {
		if _, err := prepareHook(dir, ts); err != nil {
			return nil, fmt.Errorf("service validation failed: %w", err)
		}
	}
	return manifest, nil
}

// MaybeRunValidateTablesCLI 在命令行传入 -validate-tables <dir> 时执行预检并直接以 0/1 退出;
// 未传入时为空操作,进程按正常启动路径继续。
func MaybeRunValidateTablesCLI(serviceName, dir string, useBinary bool, prepareHook PrepareHook) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	manifest, err := ValidateTablesDir(dir, useBinary, prepareHook)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[TableBundle] validate-tables FAILED service=%s dir=%s err=%v\n", serviceName, dir, err)
		os.Exit(1)
	}
	fmt.Printf("[TableBundle] validate-tables OK service=%s schema_digest=%s content_digest=%s version=%d dir=%s\n",
		serviceName, manifest.SchemaDigest, shortDigest(manifest.ContentDigest), manifest.Version, dir)
	os.Exit(0)
}
