package main

import (
	"strings"
	"testing"

	"data_service/cmd/debugutil"
)

// r2-ds#1(2026-10-22)回归:写前闸门二对取不到身份的一侧 fail-closed。修复前空身份直接跳过比较,
// 线上 TiDB 经 port-forward 混进配置时闸门全部放行。

// tidbExportSources 是从线上导出的来源:Redis 身份齐全;落点库与全局库是 TiDB,导出时取不到身份。
func tidbExportSources() debugutil.BundleSources {
	return debugutil.BundleSources{
		CoreRedis:      "127.0.0.1:16379/0",
		CoreRedisID:    "prod-core-run-id",
		MappingRedis:   "127.0.0.1:16379/0",
		MappingRedisID: "prod-core-run-id",
		ZoneMySQL:      "127.0.0.1:14000/zone_3_db",
		GlobalMySQL:    "127.0.0.1:14000/mmorpg_global",
	}
}

// mysqlExportSources 是来源全为 MySQL、身份齐全的导出包。
func mysqlExportSources() debugutil.BundleSources {
	return debugutil.BundleSources{
		CoreRedis:      "127.0.0.1:16379/0",
		CoreRedisID:    "prod-core-run-id",
		MappingRedis:   "127.0.0.1:16379/0",
		MappingRedisID: "prod-core-run-id",
		ZoneMySQL:      "127.0.0.1:13306/zone_3_db",
		ZoneMySQLID:    "prod-mysql-uuid",
		GlobalMySQL:    "127.0.0.1:13306/testdb",
		GlobalMySQLID:  "prod-mysql-uuid",
	}
}

// localRedisTargets 是本地 Redis 写目标:run_id 与来源不同,并且建了本地标记(Redis 只有标记能证明是本地,
// 见 TestJudgeImportTargetsRedisNeedsLocalMarker)。
func localRedisTargets() []importTarget {
	return []importTarget{
		{kind: instanceRedis, what: "核心 Redis", id: "local-run-id", localMarker: true},
		{kind: instanceRedis, what: "映射 Redis", id: "local-run-id", localMarker: true},
	}
}

// 目标身份取不到(TiDB 写目标:可能就是 port-forward 出来的线上集群)、没有本地标记、没加放行:拒绝。
func TestJudgeImportTargetsRejectsUnknownTargetIdentity(t *testing.T) {
	targets := append(localRedisTargets(),
		importTarget{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库"},
		importTarget{kind: instanceMySQL, role: roleGlobalDB, what: "全局账号目录SQL"},
	)
	warnings, err := judgeImportTargets(bundleExportInstances(tidbExportSources()), targets, false)
	if err == nil {
		t.Fatal("身份取不到又没有本地标记的写目标被放行")
	}
	for _, want := range []string{"MySQL 落点库", "全局账号目录SQL", debugutil.LocalTargetMarkerTable,
		debugutil.LocalRedisMarkerSetupCommand, "-allow-unverified-target-identity"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝信息缺少 %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "核心 Redis(") || strings.Contains(err.Error(), "映射 Redis(") {
		t.Fatalf("带本地标记的 Redis 不该出现在拒绝清单里: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("拒绝时不该有放行警告: %v", warnings)
	}
}

// 目标身份取得到,但导出包没记下 MySQL 来源的身份(来源是 TiDB、旧版导出包):无从比对,同样拒绝。
func TestJudgeImportTargetsRejectsUnknownSourceIdentity(t *testing.T) {
	targets := append(localRedisTargets(), importTarget{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库", id: "local-mysql-uuid"})
	_, err := judgeImportTargets(bundleExportInstances(tidbExportSources()), targets, false)
	if err == nil || !strings.Contains(err.Error(), "导出包没记下") {
		t.Fatalf("来源身份未知时必须拒绝: %v", err)
	}

	// 旧版导出包没有任何 *_id:读过落点库,却没记下它的身份。
	legacy := debugutil.BundleSources{CoreRedis: "127.0.0.1:16379/0", MappingRedis: "127.0.0.1:16379/0", ZoneMySQL: "127.0.0.1:13306/zone_3_db"}
	_, err = judgeImportTargets(bundleExportInstances(legacy), targets, false)
	if err == nil || !strings.Contains(err.Error(), "MySQL 落点库") || !strings.Contains(err.Error(), "导出包没记下") {
		t.Fatalf("旧版导出包的 MySQL 来源身份未知时必须拒绝: %v", err)
	}
}

// Redis 目标:run_id 不同证明不了是本地,只有本地标记(或显式放行)能证明;run_id 相同时标记与放行都越不过。
func TestJudgeImportTargetsRedisNeedsLocalMarker(t *testing.T) {
	// 导出时连的是线上主 redis-0;导出之后 Pod 滚动、哨兵切主,svc/redis 的 port-forward 现在打到 redis-1,
	// 线上实例的 run_id 已与包里不同。这台线上 Redis 没有本地标记,必须拒绝。
	failedOver := []importTarget{{kind: instanceRedis, what: "核心 Redis", id: "prod-redis-1-run-id"}}
	sources := bundleExportInstances(debugutil.BundleSources{CoreRedis: "127.0.0.1:16379/0", CoreRedisID: "prod-redis-0-run-id"})
	warnings, err := judgeImportTargets(sources, failedOver, false)
	if err == nil {
		t.Fatal("run_id 不同、没有本地标记的 Redis 写目标被放行(线上切主后的 port-forward 会被当成本地)")
	}
	for _, want := range []string{"核心 Redis", "run_id", debugutil.LocalRedisMarkerSetupCommand} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝信息缺少 %q: %v", want, err)
		}
	}
	if len(warnings) != 0 {
		t.Fatalf("拒绝时不该有放行警告: %v", warnings)
	}

	// 本地 Redis 建了标记:放行,无警告。
	marked := []importTarget{{kind: instanceRedis, what: "核心 Redis", id: "local-run-id", localMarker: true}}
	if warnings, err := judgeImportTargets(sources, marked, false); err != nil || len(warnings) != 0 {
		t.Fatalf("带本地标记的 Redis 应直接放行: warnings=%v err=%v", warnings, err)
	}

	// run_id 与来源相同:就是导出来源,带标记、加放行都要拒绝。
	same := []importTarget{{kind: instanceRedis, what: "核心 Redis", id: "prod-redis-0-run-id", localMarker: true}}
	if _, err := judgeImportTargets(sources, same, true); err == nil || !strings.Contains(err.Error(), "同一个实例") {
		t.Fatalf("run_id 与来源相同时必须拒绝: %v", err)
	}

	// 显式放行:没有标记的 Redis 照样写,但要打警告。
	if warnings, err := judgeImportTargets(sources, failedOver, true); err != nil || len(warnings) != 1 ||
		!strings.Contains(warnings[0], "核心 Redis") {
		t.Fatalf("显式放行应放行并对该 Redis 打一条警告: warnings=%v err=%v", warnings, err)
	}
}

// 显式放行:照样放行,但每个证明不了的目标都要有一条警告。
func TestJudgeImportTargetsAllowUnverifiedWarnsPerTarget(t *testing.T) {
	targets := append(localRedisTargets(),
		importTarget{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库"},
		importTarget{kind: instanceMySQL, role: roleGlobalDB, what: "全局账号目录SQL"},
	)
	warnings, err := judgeImportTargets(bundleExportInstances(tidbExportSources()), targets, true)
	if err != nil {
		t.Fatalf("显式放行仍被拒绝: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("应对两个证明不了的 MySQL 目标各打一条警告,得到 %d 条: %v", len(warnings), warnings)
	}
	for i, what := range []string{"MySQL 落点库", "全局账号目录SQL"} {
		if !strings.Contains(warnings[i], what) || !strings.Contains(warnings[i], "-allow-unverified-target-identity") {
			t.Fatalf("警告 %d 没写明目标或放行开关: %q", i, warnings[i])
		}
	}
}

// 目标库带本地调试环境标记:身份取不到也放行,而且不需要警告。
func TestJudgeImportTargetsLocalMarkerProvesLocal(t *testing.T) {
	targets := append(localRedisTargets(),
		importTarget{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库", localMarker: true},
		importTarget{kind: instanceMySQL, role: roleGlobalDB, what: "全局账号目录SQL", localMarker: true},
	)
	warnings, err := judgeImportTargets(bundleExportInstances(tidbExportSources()), targets, false)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("带本地标记的目标应直接放行: warnings=%v err=%v", warnings, err)
	}

	// 只有一个库建了标记:另一个照样拒绝(标记必须建在每个要写的库里)。
	targets[len(targets)-1].localMarker = false
	if _, err := judgeImportTargets(bundleExportInstances(tidbExportSources()), targets, false); err == nil ||
		!strings.Contains(err.Error(), "全局账号目录SQL") {
		t.Fatalf("没建标记的库必须拒绝: %v", err)
	}
}

// 目标就是导出来源:一律拒绝,本地标记与显式放行都越不过。
func TestJudgeImportTargetsRejectsExportSourceEvenWhenAllowed(t *testing.T) {
	sameRedis := []importTarget{{kind: instanceRedis, what: "核心 Redis", id: "prod-core-run-id", localMarker: true}}
	if _, err := judgeImportTargets(bundleExportInstances(mysqlExportSources()), sameRedis, true); err == nil ||
		!strings.Contains(err.Error(), "同一个实例") {
		t.Fatalf("写回导出来源 Redis 未被拒绝(标记与放行都不能越过): %v", err)
	}
	sameMySQL := []importTarget{{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库", id: "prod-mysql-uuid", localMarker: true}}
	if _, err := judgeImportTargets(bundleExportInstances(mysqlExportSources()), sameMySQL, true); err == nil ||
		!strings.Contains(err.Error(), "同一个实例") {
		t.Fatalf("写回导出来源 MySQL 未被拒绝(标记与放行都不能越过): %v", err)
	}
}

// localMySQLTargets 是本地 MySQL 写目标:身份取得到、与线上不同,没建本地标记(只靠身份比对证明)。
func localMySQLTargets() []importTarget {
	return []importTarget{
		{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库", id: "local-mysql-uuid"},
		{kind: instanceMySQL, role: roleGlobalDB, what: "全局账号目录SQL", id: "local-mysql-uuid"},
	}
}

// 两侧身份齐全且都不同:放行,无警告。
func TestJudgeImportTargetsProvenDistinct(t *testing.T) {
	targets := append(localRedisTargets(), localMySQLTargets()...)
	warnings, err := judgeImportTargets(bundleExportInstances(mysqlExportSources()), targets, false)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("身份齐全且不同的目标应放行: warnings=%v err=%v", warnings, err)
	}

	// 口径隔离:Redis run_id 与 MySQL server_uuid 不互比(「相同」才硬拒,跨口径的相同不算)。
	crossKind := []importTarget{
		{kind: instanceRedis, what: "核心 Redis", id: "prod-mysql-uuid", localMarker: true},
		{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库", id: "prod-core-run-id"},
	}
	if _, err := judgeImportTargets(bundleExportInstances(mysqlExportSources()), crossKind, false); err != nil {
		t.Fatalf("不同口径的身份不该互比: %v", err)
	}
}

// MySQL 身份比对按角色配对:同角色的来源导出时没读,就无从比对,不能当成「已证明不同」(2026-10-22 评审补充)。
func TestJudgeImportTargetsPairsMySQLSourcesByRole(t *testing.T) {
	// debug_fetch -skip-global:全局库目标没有来源可比对,落点库目标照常由落点库来源证明。
	skippedGlobal := mysqlExportSources()
	skippedGlobal.GlobalMySQL, skippedGlobal.GlobalMySQLID = "", ""
	_, err := judgeImportTargets(bundleExportInstances(skippedGlobal), localMySQLTargets(), false)
	if err == nil || !strings.Contains(err.Error(), "全局账号目录SQL(") || !strings.Contains(err.Error(), "同角色") {
		t.Fatalf("导出时没读全局库,全局库目标必须判为无从比对: %v", err)
	}
	if strings.Contains(err.Error(), "MySQL 落点库(") {
		t.Fatalf("落点库目标有落点库来源可比对,不该被拒: %v", err)
	}

	// 落点畸形没读落点库(全局库照常读了):落点库目标无从比对,全局库目标不受影响。
	malformedZone := mysqlExportSources()
	malformedZone.ZoneMySQL, malformedZone.ZoneMySQLID = "", ""
	_, err = judgeImportTargets(bundleExportInstances(malformedZone), localMySQLTargets(), false)
	if err == nil || !strings.Contains(err.Error(), "MySQL 落点库(") || strings.Contains(err.Error(), "全局账号目录SQL(") {
		t.Fatalf("导出时没读落点库,只有落点库目标该被拒: %v", err)
	}

	// 落点畸形再加 -skip-global:导出包一个 MySQL 来源都没有,两个目标都无从比对。
	noMySQL := mysqlExportSources()
	noMySQL.ZoneMySQL, noMySQL.ZoneMySQLID, noMySQL.GlobalMySQL, noMySQL.GlobalMySQLID = "", "", "", ""
	_, err = judgeImportTargets(bundleExportInstances(noMySQL), localMySQLTargets(), false)
	if err == nil || !strings.Contains(err.Error(), "MySQL 落点库(") || !strings.Contains(err.Error(), "全局账号目录SQL(") {
		t.Fatalf("导出包没有 MySQL 来源时两个 MySQL 目标都必须拒绝: %v", err)
	}

	// 无从比对的目标建了本地标记就放行。
	marked := localMySQLTargets()
	marked[0].localMarker, marked[1].localMarker = true, true
	if warnings, err := judgeImportTargets(bundleExportInstances(noMySQL), marked, false); err != nil || len(warnings) != 0 {
		t.Fatalf("带本地标记的目标应放行: warnings=%v err=%v", warnings, err)
	}
}

// 旧 player / playerdb 导出不记来源身份(sources 为空):MySQL 与 Redis 目标都只能凭本地标记或显式放行。
func TestJudgeImportTargetsWithoutSourcesNeedsMarkers(t *testing.T) {
	unmarked := []importTarget{
		{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库", id: "local-mysql-uuid"},
		{kind: instanceRedis, what: "data_service 数据 Redis", id: "local-run-id"},
	}
	_, err := judgeImportTargets(nil, unmarked, false)
	if err == nil || !strings.Contains(err.Error(), "MySQL 落点库(") || !strings.Contains(err.Error(), "data_service 数据 Redis(") {
		t.Fatalf("没有来源身份、也没有标记的写目标必须全部拒绝: %v", err)
	}
	warnings, err := judgeImportTargets(nil, unmarked, true)
	if err != nil || len(warnings) != 2 {
		t.Fatalf("显式放行应对每个目标各打一条警告: warnings=%v err=%v", warnings, err)
	}
	marked := []importTarget{
		{kind: instanceMySQL, role: roleZoneDB, what: "MySQL 落点库", id: "local-mysql-uuid", localMarker: true},
		{kind: instanceRedis, what: "data_service 数据 Redis", id: "local-run-id", localMarker: true},
	}
	if warnings, err := judgeImportTargets(nil, marked, false); err != nil || len(warnings) != 0 {
		t.Fatalf("带本地标记的目标应放行: warnings=%v err=%v", warnings, err)
	}
}
