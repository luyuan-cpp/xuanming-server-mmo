// Package buildinfo 是 Go 服务二进制的版本三元组(version / commit / build_time)唯一落点。
//
// # 为什么需要它
//
// 事故复盘最先要回答的是"线上跑的到底是哪一版"。以前 deploy/k8s/Dockerfile.go-svc 用
// `-X main.buildVersion=…` 注入,但没有任何 main 包声明那几个变量,链接器静默忽略 ——
// 镜像 tag 上写着版本,二进制里什么都没有。现在所有注入口统一指向本包:
//
//	-ldflags "-X shared/buildinfo.Version=<v> -X shared/buildinfo.Commit=<sha12> -X shared/buildinfo.BuildTime=<UTC>"
//
// 注入方:deploy/k8s/Dockerfile.go-svc(镜像)与 tools/scripts/go_services.ps1 -Command build(本地 exe)。
// 两处必须保持同一组变量名;改名等于让注入重新变成空操作,而且编译不会报错。
//
// # 回落规则(Get)
//
//   - Commit 未注入("unknown" 或空串)时,才读 debug.ReadBuildInfo 的 vcs.revision(截 12 位,
//     与 tools/scripts/lib/release_common.ps1 Get-GitReleaseStamp 口径一致)/ vcs.time / vcs.modified。
//     覆盖在 git 工作区里裸 `go build` 的场景;取不到 vcs 信息时(测试二进制、build context 不含 .git
//     的镜像内编译)仍是 unknown —— 镜像内编译必须靠 -X 注入。
//   - 显式注入的 Commit 永远优先:脚本注入时已自带 -dirty 后缀,不再用 vcs.modified 二次判断。
//   - Version 没注入就保持 "dev",不从 vcs 推版本号 —— 发布版本号只来自发布流程(-Version / MMORPG_RELEASE_VERSION)。
//
// 本包只依赖标准库:版本行要在配置加载与 logx 初始化之前就能打(理由同 cpp/nodes/gate/gate_version.h 头注释)。
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
)

const (
	defaultVersion = "dev"
	unknownValue   = "unknown"
	// commitLength 与 release_common.ps1 的 `git rev-parse --short=12` 一致,镜像 tag / OCI label / 启动行三处可直接比对。
	commitLength = 12
	dirtySuffix  = "-dirty"
)

// 由链接器 -X 注入。必须保持为未经函数调用初始化的包级 string 变量,否则 -X 不生效。
var (
	Version   = defaultVersion
	Commit    = unknownValue
	BuildTime = unknownValue
)

// Info 是解析后的版本信息。Commit 不含 -dirty 后缀(除非注入值本身带),脏树由 Modified 表示。
type Info struct {
	Version   string
	Commit    string
	BuildTime string
	GoVersion string
	Modified  bool
}

// Get 返回当前二进制的版本信息。只在 Commit 未注入时读 debug.ReadBuildInfo。
func Get() Info {
	info := injected()
	if info.Commit != unknownValue {
		return info
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		info = applyVCSSettings(info, bi.Settings)
	}
	return info
}

// StartupLine 返回一行启动版本行,各服务 main 在 flag.Parse 之后直写 stdout。
// 格式是日志检索契约(grep "service starting"),改字段名 / 顺序前先查告警与运维文档。
func StartupLine(service string) string {
	return formatStartupLine(service, Get())
}

// injected 读包级变量并把空串归一成默认值:`-X shared/buildinfo.Commit=` 这种空注入
// 与没注入同义,不能让启动行出现 `commit=` 这种看不出是"没注入"还是"注入失败"的空值。
func injected() Info {
	return Info{
		Version:   valueOrDefault(Version, defaultVersion),
		Commit:    valueOrDefault(Commit, unknownValue),
		BuildTime: valueOrDefault(BuildTime, unknownValue),
		GoVersion: runtime.Version(),
	}
}

// applyVCSSettings 用 go build 自动嵌入的 vcs.* 设置补齐未注入的 Commit / BuildTime / Modified。
// 纯函数,便于单测;已注入(非 unknown)的字段不覆盖。
func applyVCSSettings(info Info, settings []debug.BuildSetting) Info {
	if info.Commit != unknownValue {
		return info
	}
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				info.Commit = shortCommit(s.Value)
			}
		case "vcs.time":
			if s.Value != "" && info.BuildTime == unknownValue {
				info.BuildTime = s.Value
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}

func formatStartupLine(service string, info Info) string {
	commit := info.Commit
	if info.Modified && !strings.HasSuffix(commit, dirtySuffix) {
		commit += dirtySuffix
	}
	return "service starting service=" + service +
		" version=" + info.Version +
		" commit=" + commit +
		" build_time=" + info.BuildTime +
		" go_version=" + info.GoVersion
}

func shortCommit(revision string) string {
	if len(revision) > commitLength {
		return revision[:commitLength]
	}
	return revision
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
