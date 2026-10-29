package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"data_service/cmd/debugutil"

	"github.com/redis/go-redis/v9"
)

// ── 写前闸门二:写目标不能是导出来源(2026-10-22,r2-ds#1)──────────────────────────
//
// 地址闸门(CheckLocalWriteTarget)只认回环地址,挡不住 port-forward 到 127.0.0.1 的线上实例;这道闸门按实例身份
// 再挡一次。旧实现把取不到的身份当成「不同」直接跳过,而 TiDB 取不到 @@server_uuid,闸门对 TiDB 等于没有:
// 线上 TiDB 经 port-forward 混进配置时,导入会把线上两张玩家行替换成导出时的旧状态并抬高行内围栏,
// 之后线上 scene 的存盘全被判 stale_owner。
//
// 现在每个写目标都必须**证明**自己不是导出来源,证明不了就拒绝(fail-closed)。三种证明,按顺序:
//  1. 身份比对(只对 MySQL 目标):目标身份取得到;导出包里与它同角色的来源确实读过(落点库目标对落点库来源,
//     全局库目标对全局库来源);导出包里每个 MySQL 来源的身份都取得到,且没有一个与目标相等;
//  2. 本地调试环境标记:MySQL / TiDB 目标库里有 debugutil.LocalTargetMarkerTable 那一行,Redis 目标(客户端选中的
//     DB)里有 debugutil.LocalRedisMarkerKey 那把键;
//  3. 操作者显式加 -allow-unverified-target-identity:放行并逐个打 WARNING。
//
// Redis 目标只认 2、3(2026-10-22 评审补充):run_id 只标识进程。线上 Redis 是 HA 形态(svc/redis 经 haproxy 指向
// 当前主,Pod 滚动时 prestop 主动 failover),单实例重启也会换 run_id;导出之后切一次主、重启一次,或导出时直连了
// 从库 Pod,线上 Redis 的 run_id 就和包里不同。「不同」因此证明不了写目标是本地,只有「相同」有判定价值。
//
// 身份相等一律拒绝,标记与显式放行都不能越过:那是唯一能被正面证明的「写回导出来源」。
//
// 旧 player / playerdb 导出文件不记来源实例身份,以「没有来源」走同一套判定:每个写目标都只能凭标记或显式放行。

// instanceKind 是实例身份的口径。MySQL 的 @@server_uuid 与 Redis 的 run_id 不是同一种值,只在同口径内比较,
// 「同口径来源身份未知」也只影响同口径的目标。
type instanceKind uint8

const (
	instanceMySQL instanceKind = iota
	instanceRedis
)

// storeRole 是 MySQL / TiDB 实例在这次导出 / 导入里装的是哪类库。身份比对按角色配对:落点库目标只能拿导出时读过的
// 落点库来源证明自己不同,全局库目标同理;同角色的来源导出时没读(-skip-global、落点畸形没读落点库、旧版导出文件),
// 就无从比对,不能当成「已证明不同」(2026-10-22 评审补充)。Redis 实例不按角色配对,一律 roleNone。
type storeRole uint8

const (
	roleNone storeRole = iota
	roleZoneDB
	roleGlobalDB
)

// exportedInstance 是导出包记录的一个来源实例;id 为空 = 导出时没取到身份(TiDB、旧版导出包、导出时连接失败)。
type exportedInstance struct {
	kind instanceKind
	role storeRole
	what string
	id   string
}

// importTarget 是导入要写的一个实例。id 为空 = 取不到身份;localMarker = 目标上有本地调试环境标记。
type importTarget struct {
	kind        instanceKind
	role        storeRole
	what        string
	id          string
	localMarker bool
}

// bundleExportInstances 列出导出包实际读过的来源实例。地址与身份都空 = 导出时没读这台实例(例如 -skip-global、
// 落点畸形没读落点库),不进列表;与它同角色的写目标因此无从比对(见 unverifiedReason)。
func bundleExportInstances(src debugutil.BundleSources) []exportedInstance {
	var out []exportedInstance
	add := func(kind instanceKind, role storeRole, what, addr, id string) {
		if strings.TrimSpace(addr) == "" && id == "" {
			return
		}
		out = append(out, exportedInstance{kind: kind, role: role, what: what, id: id})
	}
	add(instanceRedis, roleNone, "核心 Redis", src.CoreRedis, src.CoreRedisID)
	add(instanceRedis, roleNone, "映射 Redis", src.MappingRedis, src.MappingRedisID)
	add(instanceMySQL, roleZoneDB, "落点库", src.ZoneMySQL, src.ZoneMySQLID)
	add(instanceMySQL, roleGlobalDB, "全局库", src.GlobalMySQL, src.GlobalMySQLID)
	return out
}

// mysqlImportTarget 取一个 MySQL / TiDB 写目标的身份与本地标记;q 必须连在导入要写的那个库上。
func mysqlImportTarget(ctx context.Context, role storeRole, what string, q debugutil.Queryer) importTarget {
	return importTarget{
		kind:        instanceMySQL,
		role:        role,
		what:        what,
		id:          debugutil.MySQLServerIdentity(ctx, q),
		localMarker: debugutil.HasLocalTargetMarker(ctx, q),
	}
}

// redisImportTarget 取一个 Redis 写目标的 run_id(只用来拒绝「相同」)与本地标记;client 必须连在导入要写的
// 那个实例、那个 DB 上。
func redisImportTarget(ctx context.Context, what string, client redis.Cmdable) importTarget {
	return importTarget{
		kind:        instanceRedis,
		role:        roleNone,
		what:        what,
		id:          debugutil.RedisServerIdentity(ctx, client),
		localMarker: debugutil.HasLocalRedisMarker(ctx, client),
	}
}

// bundleWriteTargets 是 player-bundle 导入要写的全部实例的连接,这里只用来取身份与本地标记。
// dataServiceMapping 为 nil = data_service 没配 MappingRedis。
type bundleWriteTargets struct {
	zoneDB, globalDB                            debugutil.Queryer
	coreRedis, mappingRedis, dataServiceMapping redis.Cmdable
}

// checkBundleWriteTargets 是 player-bundle 的写前闸门二:取每个写目标的身份与本地标记,按导出包记录的来源判定。
func checkBundleWriteTargets(ctx context.Context, src debugutil.BundleSources, w bundleWriteTargets, allowUnverified bool) error {
	targets := []importTarget{
		mysqlImportTarget(ctx, roleZoneDB, "MySQL 落点库", w.zoneDB),
		redisImportTarget(ctx, "核心 Redis", w.coreRedis),
		redisImportTarget(ctx, "映射 Redis", w.mappingRedis),
	}
	if w.dataServiceMapping != nil {
		targets = append(targets, redisImportTarget(ctx, "data_service MappingRedis", w.dataServiceMapping))
	}
	targets = append(targets, mysqlImportTarget(ctx, roleGlobalDB, "全局账号目录SQL", w.globalDB))
	return admitWriteTargets(bundleExportInstances(src), targets, allowUnverified)
}

// admitWriteTargets 判定全部写目标,凭显式放行通过的目标逐条打 WARNING;返回 error 时一个都不许写。
// 旧 player / playerdb 模式以 sources=nil 调用:旧导出文件不记来源身份,每个目标都只能凭标记或显式放行。
func admitWriteTargets(sources []exportedInstance, targets []importTarget, allowUnverified bool) error {
	warnings, err := judgeImportTargets(sources, targets, allowUnverified)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", warning)
	}
	return nil
}

// judgeImportTargets 判定全部写目标。返回的 warnings 是凭显式放行通过的目标(调用方逐条打印);
// err 非 nil 时一个都不许写。纯函数,不连任何实例。
func judgeImportTargets(sources []exportedInstance, targets []importTarget, allowUnverified bool) (warnings []string, err error) {
	for _, target := range targets {
		if target.id == "" {
			continue
		}
		for _, source := range sources {
			if source.kind == target.kind && source.id == target.id {
				return nil, fmt.Errorf("%s 与导出来源%s是同一个实例(身份 %s):导入只写隔离环境,检查是不是把线上 port-forward 当成了本地",
					target.what, source.what, target.id)
			}
		}
	}

	var unverified []string
	for _, target := range targets {
		if reason := unverifiedReason(sources, target); reason != "" {
			unverified = append(unverified, target.what+"("+reason+")")
		}
	}
	if len(unverified) == 0 {
		return nil, nil
	}
	if !allowUnverified {
		return nil, fmt.Errorf("以下写目标无法证明不是导出来源,拒绝写入: %s。确认它们都是本地隔离环境后二选一:"+
			"建本地标记——MySQL / TiDB 目标在每个要写的库(落点库 zone_<N>_db、全局库)执行 %s,"+
			"Redis 目标在每个要写的实例上、按配置里的 DB 号执行 %s(不设 TTL,FLUSHALL 后要重建);"+
			"或加 -allow-unverified-target-identity 显式放行",
			strings.Join(unverified, "; "), debugutil.LocalTargetMarkerSetupSQL, debugutil.LocalRedisMarkerSetupCommand)
	}
	for _, item := range unverified {
		warnings = append(warnings, fmt.Sprintf("write target %s cannot be proven distinct from the export source; proceeding only because -allow-unverified-target-identity is set", item))
	}
	return warnings, nil
}

// unverifiedReason 返回目标无法证明「不是导出来源」的原因;能证明时返回空串。调用前 judgeImportTargets 已拒掉
// 身份与任一来源相等的目标。
func unverifiedReason(sources []exportedInstance, target importTarget) string {
	if target.localMarker {
		return ""
	}
	if target.kind == instanceRedis {
		return "Redis run_id 只标识进程,线上切主、重启或导出时连的是从库都会让它不同,证明不了是本地;目标实例也没有本地调试环境标记"
	}
	if target.id == "" {
		return "目标实例身份取不到(TiDB 一律取不到),目标库也没有本地调试环境标记"
	}
	pairedSourceRead := false
	for _, source := range sources {
		if source.kind != instanceMySQL {
			continue
		}
		// 任一 MySQL 来源身份未知,目标都可能就是它(同一实例上既有全局库也有各 zone 库),无从排除。
		if source.id == "" {
			return "导出包没记下" + source.what + "的实例身份(来源是 TiDB、旧版导出包或导出时连接失败),无从比对"
		}
		if source.role == target.role {
			pairedSourceRead = true
		}
	}
	if !pairedSourceRead {
		return "导出文件里没有同角色的来源可比对(导出时没读这个库,如 -skip-global、落点畸形;或是不记来源身份的旧 player / playerdb 导出),目标库也没有本地调试环境标记"
	}
	return ""
}
