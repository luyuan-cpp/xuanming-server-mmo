// Package dbguard 是 db 服务在**碰到玩家数据之前**必须过的两道闸:
//
//  1. 库名白名单(本文件)—— 实际连上的 DATABASE() 必须出现在一份**外部注入**
//     的允许清单里,不在就拒启;
//  2. 大字段三档闸(blob.go)—— pb blob 列的入库字节上限。
//
// 两道闸都刻意做成模块内自包含,不依赖任何跨模块共享库。
package dbguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// AllowedDatabasesEnv 是白名单的最高优先级来源。
//
// 之所以让环境变量压过 yaml:K8s 里 yaml 通常是镜像内的模板 + ConfigMap 覆盖,
// 而「这套实例被允许写哪些库」是部署面的事实,应该由 Deployment 直接钉死,
// 不经过任何模板渲染环节 —— 渲染环节正是 ZoneId 填错的地方。
const AllowedDatabasesEnv = "DB_ALLOWED_DATABASES"

// ErrDatabaseNotAllowed 表示实际连上的库不在白名单内。启动路径必须据此拒启。
var ErrDatabaseNotAllowed = errors.New("connected database is not in the injected allow list")

// ErrAllowlistMissing 表示生产模式下没有任何白名单来源。
var ErrAllowlistMissing = errors.New("database allow list is empty")

// AllowlistSpec 描述白名单的三个外部来源,按优先级解析。
type AllowlistSpec struct {
	// EnvName 环境变量名,留空则用 AllowedDatabasesEnv。值为逗号分隔。
	EnvName string
	// File 每行一个库名的文件(ConfigMap 挂载场景),`#` 开头与空行忽略。
	File string
	// Inline 是 yaml 内联清单,优先级最低。
	Inline []string
}

// Allowlist 是解析后的白名单,Source 记录它到底来自哪儿(启动日志要打出来,
// 否则「我明明配了」和「它明明没读」永远对不上账)。
type Allowlist struct {
	Names  []string
	Source string
}

// Empty 报告白名单是否为空。
func (a Allowlist) Empty() bool { return len(a.Names) == 0 }

// Contains 判断库名是否被允许。MySQL 在 Linux 上库名默认大小写敏感,
// 这里按原样比较,不做大小写折叠 —— 折叠会让 Zone_1_DB 和 zone_1_db 互通,
// 而它们在服务端是两个库。
func (a Allowlist) Contains(name string) bool {
	for _, n := range a.Names {
		if n == name {
			return true
		}
	}
	return false
}

// String 便于日志输出。
func (a Allowlist) String() string {
	return fmt.Sprintf("[%s] (source=%s)", strings.Join(a.Names, ","), a.Source)
}

// Resolve 按 env > file > inline 的优先级解析白名单。
//
// 只要更高优先级的来源**解析出了非空清单**就短路,不做合并 —— 合并会让
// 「我在 Deployment 里收紧了白名单」被镜像里的旧 yaml 悄悄放宽。
func (s AllowlistSpec) Resolve() (Allowlist, error) {
	envName := s.EnvName
	if envName == "" {
		envName = AllowedDatabasesEnv
	}
	if raw := strings.TrimSpace(os.Getenv(envName)); raw != "" {
		names := normalizeNames(strings.Split(raw, ","))
		if len(names) > 0 {
			return Allowlist{Names: names, Source: "env:" + envName}, nil
		}
	}

	if s.File != "" {
		data, err := os.ReadFile(s.File)
		if err != nil {
			// 显式配了文件却读不到,属于部署事故,不能静默回落到 yaml。
			return Allowlist{}, fmt.Errorf("read database allow list file %q: %w", s.File, err)
		}
		names := normalizeNames(strings.Split(string(data), "\n"))
		if len(names) > 0 {
			return Allowlist{Names: names, Source: "file:" + s.File}, nil
		}
		return Allowlist{}, fmt.Errorf("database allow list file %q contains no database name", s.File)
	}

	if names := normalizeNames(s.Inline); len(names) > 0 {
		return Allowlist{Names: names, Source: "config:ServerConfig.Database.AllowedDatabases"}, nil
	}
	return Allowlist{Source: "none"}, nil
}

// normalizeNames 去掉注释、空白与重复项,并排序以便日志稳定。
func normalizeNames(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if idx := strings.IndexByte(item, '#'); idx >= 0 {
			item = item[:idx]
		}
		item = strings.TrimSpace(strings.Trim(item, "\r"))
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// RowQuerier 是 *sql.DB / *sql.Conn / *sql.Tx 的公共子集,便于测试注入。
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AssertOptions 控制断言的严格程度。
type AssertOptions struct {
	// Expected 是本进程按 ZoneId 推导出来的库名。非空时要求它与服务端
	// 实际的 DATABASE() 完全一致 —— 这一条抓的是「连错实例 / DSN 被改」。
	Expected string
	// Allow 是外部注入的白名单,抓的是「ZoneId 本身就填错了」。
	Allow Allowlist
	// RelaxEmptyAllowlist 只在 dev/local 模式下为 true:白名单为空时放行并
	// 打一条显眼的 WARN,而不是拒启。生产必须为 false。
	RelaxEmptyAllowlist bool
	// Warnf 接收放行但可疑的情况(为空时丢弃)。
	Warnf func(format string, args ...any)
}

// AssertDatabase 在服务碰玩家数据之前断言「我到底连上了哪个库」。
//
// 顺序刻意是「先问服务端、再比对配置」:配置里的 DBName 只是我们**想**连的库,
// 真正决定数据落在哪儿的是服务端 DATABASE()。历史事故正是二者不一致却无人发现。
func AssertDatabase(ctx context.Context, q RowQuerier, opts AssertOptions) (string, error) {
	var actual sql.NullString
	if err := q.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&actual); err != nil {
		return "", fmt.Errorf("query connected database name: %w", err)
	}
	if !actual.Valid || actual.String == "" {
		return "", errors.New("connection has no default database selected; refusing to start")
	}
	name := actual.String

	if opts.Expected != "" && opts.Expected != name {
		return name, fmt.Errorf("%w: connected=%q but this process derived %q from its ZoneId",
			ErrDatabaseNotAllowed, name, opts.Expected)
	}

	if opts.Allow.Empty() {
		if !opts.RelaxEmptyAllowlist {
			return name, fmt.Errorf("%w: set %s (or ServerConfig.Database.AllowedDatabasesFile / AllowedDatabases) to the databases this deployment may write; refusing to start with connected=%q",
				ErrAllowlistMissing, AllowedDatabasesEnv, name)
		}
		if opts.Warnf != nil {
			opts.Warnf("DB-ALLOWLIST: 白名单为空,dev/test 模式下放行 connected=%q。生产必须注入 %s,否则 ZoneId 填错会静默写进新库",
				name, AllowedDatabasesEnv)
		}
		return name, nil
	}

	if !opts.Allow.Contains(name) {
		return name, fmt.Errorf("%w: connected=%q allowed=%s", ErrDatabaseNotAllowed, name, opts.Allow.String())
	}
	return name, nil
}
