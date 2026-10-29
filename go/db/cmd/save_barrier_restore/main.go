// save_barrier_restore 只在停写维护窗口运行；凭据只经环境变量输入，不写入计划和回执。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"db/internal/barrierrestore"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/kafkautil"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

type options struct {
	authorityPhase, authorityOutput, authorityRequest, authorityRequestSHA, authorityTicket, authorityTicketSHA string
	maintenancePlan, maintenanceDir, maintenanceSHA, authorityKafkaEnv                                          string
	fenceOnlyPromotion                                                                                          bool
	auditFile, auditSHA, localTarget, observationFile, sqlTarget                                                string
	auditedOwnerUpperBound                                                                                      string
	mode, schema, players, maintenanceID, planFile, planSHA, redisAddr, dsnEnv, redisPasswordEnv                string
	zone                                                                                                        uint
	redisDB                                                                                                     int
	routingMode                                                                                                 string
	writersStopped, queuesDrained, allowLegacy, localMySQL, allowHomeFallback                                   bool
	mappingAddr, mappingPasswordEnv, routeSchema                                                                string
	mappingDB                                                                                                   int
	releaseMembers, releaseMembersSHA, releaseReview, releaseReviewSHA, releaseAttempt, releaseAttemptSHA       string
}

func main() {
	var o options
	flag.StringVar(&o.mode, "mode", "plan", "guard | plan | apply | release-plan | release-apply；旧模式均不自动放流")
	flag.StringVar(&o.schema, "schema", "", "实际玩家 SQL 库名，必须显式指定")
	flag.UintVar(&o.zone, "zone", 0, "home zone，必须显式指定")
	flag.StringVar(&o.players, "players", "", "逗号分隔的玩家号，每批最多1000名")
	flag.StringVar(&o.maintenanceID, "maintenance-id", "", "已备案的停写维护工单号；须与永久 Redis 标记一致")
	flag.StringVar(&o.planFile, "plan", "", "计划文件；plan 新建，apply 只读")
	flag.StringVar(&o.planSHA, "plan-sha256", "", "apply 必填：已审查的原始计划文件 SHA-256")
	flag.StringVar(&o.redisAddr, "redis", "", "父缓存 Redis host:port；不支持 Cluster")
	flag.IntVar(&o.redisDB, "redis-db", 0, "父缓存逻辑 DB；映射仍固定 DB0")
	flag.StringVar(&o.routingMode, "routing-mode", "", "redis-same-instance | redis-split | db_first，必须与真实部署一致")
	flag.StringVar(&o.mappingAddr, "mapping-redis", "", "独立映射 Redis 地址；省略时复用父缓存实例 DB0")
	flag.IntVar(&o.mappingDB, "mapping-redis-db", 0, "映射固定 DB0")
	flag.StringVar(&o.mappingPasswordEnv, "mapping-password-env", "SAVE_BARRIER_RESTORE_MAPPING_PASSWORD", "独立映射 Redis 密码的环境变量名")
	flag.StringVar(&o.routeSchema, "route-schema", "", "db_first 的 SQL 路由库；必须与资产库同一 TiDB 实例")
	flag.StringVar(&o.auditedOwnerUpperBound, "audited-owner-upper-bound", "", "v2 必填：独立保留并审定的旧 owner 上界；当前计数器须严格高于它，不得猜测")
	flag.StringVar(&o.dsnEnv, "dsn-env", "SAVE_BARRIER_RESTORE_DSN", "SQL DSN 所在环境变量名，不传明文")
	flag.StringVar(&o.redisPasswordEnv, "redis-password-env", "SAVE_BARRIER_RESTORE_REDIS_PASSWORD", "Redis 密码所在环境变量名")
	flag.BoolVar(&o.writersStopped, "writers-stopped", false, "声明 scene/login/DB、scene_manager等归属发号者、路由/回填/迁移工具均已停且不能复活")
	flag.BoolVar(&o.queuesDrained, "queues-drained", false, "声明全部 Kafka 世代、retry、processing、dead 已审定排空")
	flag.BoolVar(&o.allowLegacy, "allow-legacy", false, "仅 plan：允许将历史零围栏双表元数据迁到现有 owner/seq1")
	flag.BoolVar(&o.allowHomeFallback, "allow-home-fallback", false, "仅 plan：允许无落点记录、home 等于 -zone 的玩家按本区 zone 库恢复(copy 合服/撤销后)；写进计划")
	flag.BoolVar(&o.localMySQL, "local-mysql", false, "仅本地 MySQL 验证；默认必须是 TiDB 强读会话")
	flag.BoolVar(&o.fenceOnlyPromotion, "fence-only-promotion", false, "显式单玩家维护提升；稳定R撤销后，仅将两SQL围栏提升到max(原counter,U)+1/q1")
	flag.StringVar(&o.auditFile, "audit-evidence", "", "独立审核的停写/队列/旧可发owner上界原件文件")
	flag.StringVar(&o.auditSHA, "audit-evidence-sha256", "", "该审核原件的已审SHA-256；仅摘要进入计划")
	flag.StringVar(&o.localTarget, "local-target-id", "", "审定maintenance-recovery SQL目标32hex nonce；历史参数名不授予调试写权限")
	flag.StringVar(&o.sqlTarget, "sql-target-address", "", "审定无凭据SQL host:port，逐字绑定DSN和永久维护标记")
	flag.StringVar(&o.observationFile, "target-observation", "", "新promotion每次执行的新建目标观测文件；run_id/replid只记录当次观测")
	flag.StringVar(&o.releaseMembers, "release-members", "", "仅release-plan：完整成员输入原件")
	flag.StringVar(&o.releaseMembersSHA, "release-members-sha256", "", "完整成员输入的已审原文件SHA")
	flag.StringVar(&o.releaseReview, "release-review", "", "显式解除的不可变producer审核原件")
	flag.StringVar(&o.releaseReviewSHA, "release-review-sha256", "", "不可变审核原件SHA")
	flag.StringVar(&o.releaseAttempt, "release-attempt", "", "本次新短排他维护窗原件；不得改原plan续期")
	flag.StringVar(&o.releaseAttemptSHA, "release-attempt-sha256", "", "本次新维护窗原件SHA")
	flag.StringVar(&o.authorityPhase, "authority-phase", "", "collect-kafka-catalog | collect-kafka-baseline | collect-kafka | seal-request | advance | grant-ticket | revoke-ticket | freeze | release | read")
	flag.StringVar(&o.authorityOutput, "authority-phase-output", "", "本次新建正式typed结果文件")
	flag.StringVar(&o.authorityRequest, "authority-request", "", "完整正式阶段Receipt请求原件")
	flag.StringVar(&o.authorityRequestSHA, "authority-request-sha256", "", "正式阶段请求原bytes SHA")
	flag.StringVar(&o.authorityTicket, "authority-ticket", "", "原正式freeze ticket文件")
	flag.StringVar(&o.authorityTicketSHA, "authority-ticket-sha256", "", "原ticket原bytes SHA")
	flag.StringVar(&o.maintenancePlan, "maintenance-drain-plan", "", "正式维护runtime Receipt文件")
	flag.StringVar(&o.maintenanceDir, "maintenance-drain-dir", "", "已固定私有stop/report目录")
	flag.StringVar(&o.maintenanceSHA, "maintenance-drain-sha256", "", "runtime Receipt原文件SHA")
	flag.StringVar(&o.authorityKafkaEnv, "authority-kafka-brokers-env", "MMORPG_MAINTENANCE_KAFKA_BROKERS", "真实Kafka broker列表环境变量名")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, o, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, o options, out, errOut io.Writer) int {
	if o.authorityPhase != "" {
		return runAuthorityPhase(ctx, o, out, errOut)
	}
	if err := validateOptions(o); err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	var plan barrierrestore.Plan
	if o.mode == "apply" && !o.fenceOnlyPromotion {
		var err error
		plan, err = readPlan(o.planFile, o.planSHA)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 2
		}
		target := o.target()
		if plan.Schema != o.schema || uint(plan.Zone) != o.zone || plan.MaintenanceID != o.maintenanceID || plan.RedisAddr != o.redisAddr || plan.RedisDB != o.redisDB || plan.RoutingMode != o.routingMode || plan.LocalMySQL != o.localMySQL || plan.RouteSchema != o.routeSchema || (plan.Format == barrierrestore.FormatV2 && (plan.MappingAddr != target.MappingAddr || plan.MappingDB != target.MappingDB)) {
			fmt.Fprintln(errOut, "计划目标与执行参数不符")
			return 2
		}
		if plan.AuditedOwnerUpperBound != o.auditedOwnerUpperBound {
			fmt.Fprintln(errOut, "独立审核旧代上界与审定计划不符")
			return 2
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rc := redis.NewClient(&redis.Options{Addr: o.redisAddr, DB: o.redisDB, Password: os.Getenv(o.redisPasswordEnv), MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
	defer rc.Close()
	r := barrierrestore.Restorer{Redis: rc, RoutingMode: o.routingMode, LocalMySQL: o.localMySQL, RouteSchema: o.routeSchema}
	r.AuditedOwnerUpperBound = o.auditedOwnerUpperBound
	r.AllowHomeFallback = o.allowHomeFallback // 只影响 plan;apply 以计划为准(validateOptions 拒绝 apply 带它)
	if o.mappingAddr != "" || o.redisDB != 0 {
		addr, password := o.mappingAddr, os.Getenv(o.mappingPasswordEnv)
		if addr == "" {
			addr, password = o.redisAddr, os.Getenv(o.redisPasswordEnv)
		}
		mapping := redis.NewClient(&redis.Options{Addr: addr, DB: o.mappingDB, Password: password, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
		defer mapping.Close()
		r.MappingRedis = mapping
	}
	if o.mode == "guard" {
		if err := r.Guard(ctx, o.target()); err != nil {
			fmt.Fprintln(errOut, "维护闸建立失败，保留已建标记，修复后核查重入")
			return 1
		}
		fmt.Fprintln(out, "永久维护与路由围栏已建立；不会自动停业务或冻结 SQL 路由，不得放流")
		return 0
	}
	permission, err := authoritysql.LoadOperatorPermission(os.Getenv)
	if err != nil {
		fmt.Fprintln(errOut, "维护SQL operator主体装配缺失")
		return 2
	}
	db, err := authoritysql.Open(ctx, os.Getenv(o.dsnEnv), o.schema, !o.localMySQL, 1, permission)
	if err != nil {
		fmt.Fprintln(errOut, "权威 SQL 连接或强读会话检查失败")
		return 2
	}
	defer db.Close()
	r.DB = db
	r.LocalTargetID = o.localTarget
	r.MaintenanceSQLAddr = o.sqlTarget
	if o.mode == "release-plan" || o.mode == "release-apply" {
		return runMaintenanceRelease(ctx, o, r, out, errOut)
	}
	if o.fenceOnlyPromotion {
		return runPromotion(ctx, o, r, out, errOut)
	}
	if o.mode == "plan" {
		plan, err = r.Prepare(ctx, o.schema, uint32(o.zone), o.maintenanceID, strings.Split(o.players, ","), o.allowLegacy)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		body, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "计划序列化失败")
			return 2
		}
		body = append(body, '\n')
		if err := writeNew(o.planFile, body); err != nil {
			fmt.Fprintln(errOut, "计划文件创建失败，已有文件不会覆盖")
			return 2
		}
		h := sha256.Sum256(body)
		fmt.Fprintf(out, "plan_sha256=%s players=%d；计划未写业务数据，维护标记保留\n", hex.EncodeToString(h[:]), len(plan.Players))
		return 0
	}
	enc := json.NewEncoder(out)
	for i, row := range plan.Players {
		err := r.ApplyOne(ctx, plan, i)
		status := "committed"
		if err != nil {
			status = "failed"
		}
		if encodeErr := enc.Encode(struct {
			PlayerID string `json:"player_id"`
			Status   string `json:"status"`
		}{row.ID, status}); encodeErr != nil {
			fmt.Fprintln(errOut, "回执写入失败，保持维护；按同计划重跑核查")
			return 1
		}
		if err != nil {
			fmt.Fprintf(errOut, "玩家 %s 恢复失败: %v；已完成前缀不回退，保持维护并按同计划重跑\n", row.ID, err)
			return 1
		}
	}
	fmt.Fprintln(errOut, "全部计划玩家屏障已恢复；维护标记保留，冷加载与资产核对通过前不得放流")
	return 0
}

func validateOptions(o options) error {
	if o.mode == "release-plan" || o.mode == "release-apply" {
		return validateMaintenanceReleaseOptions(o)
	}
	if o.releaseMembers != "" || o.releaseMembersSHA != "" || o.releaseReview != "" || o.releaseReviewSHA != "" || o.releaseAttempt != "" || o.releaseAttemptSHA != "" {
		return errors.New("release专用原件只能用于显式release-plan或release-apply")
	}
	if o.mode != "plan" && o.mode != "apply" && o.mode != "guard" {
		return errors.New("mode 必须是 guard、plan 或 apply")
	}
	if o.zone == 0 || o.zone > 999999 || o.target().ValidateTarget() != nil || o.maintenanceID == "" || len(o.maintenanceID) > 128 || o.redisAddr == "" || o.dsnEnv == "" || o.redisPasswordEnv == "" {
		return errors.New("必须显式提供目标库、home zone、维护号、计划和连接环境变量名")
	}
	if !o.writersStopped || !o.queuesDrained {
		return errors.New("必须先停旧写者、阻止自动复活并审定全部 Kafka/retry/dead 排空；工具无法代替此停写门禁")
	}
	if o.fenceOnlyPromotion {
		if o.mode == "guard" || o.routingMode != barrierrestore.RoutingModeDBFirst || o.localMySQL || o.allowLegacy || o.allowHomeFallback || o.auditFile == "" || len(o.auditSHA) != 64 || len(o.localTarget) != 32 || strings.Trim(o.localTarget, "0123456789abcdef") != "" || o.sqlTarget == "" || o.observationFile == "" || o.mode == "plan" && strings.Contains(o.players, ",") {
			return errors.New("fence-only promotion仅支持显式单玩家db_first/TiDB、独立维护目标marker及独立审核原件，不接受隐式降级选项")
		}
	} else if o.auditFile != "" || o.auditSHA != "" || o.localTarget != "" || o.sqlTarget != "" || o.observationFile != "" {
		return errors.New("审核原件和实例选项仅属于显式fence-only promotion")
	}
	if o.mode == "guard" {
		if o.routingMode == barrierrestore.RoutingModeRedis || o.planFile != "" || o.planSHA != "" || o.players != "" || o.allowLegacy || o.allowHomeFallback {
			return errors.New("guard 仅建立跨接缝维护闸，不接受计划或资产选项")
		}
		return nil
	}
	if o.planFile == "" {
		return errors.New("必须指定计划文件")
	}
	if o.routingMode != barrierrestore.RoutingModeRedis || o.schema != fmt.Sprintf("zone_%d_db", o.zone) || o.mappingAddr != "" || o.redisDB != 0 || o.auditedOwnerUpperBound != "" {
		n, err := strconv.ParseUint(o.auditedOwnerUpperBound, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != o.auditedOwnerUpperBound {
			return errors.New("v2 必须指定独立审核旧 owner 上界，不能从回退 SQL 猜测")
		}
	}
	if o.mode == "plan" && (o.players == "" || o.planSHA != "") {
		return errors.New("plan 必须指定玩家集合，不接受旧计划摘要")
	}
	if o.mode == "apply" && (o.planSHA == "" || o.players != "" || o.allowLegacy || o.allowHomeFallback) {
		return errors.New("apply 必须提供计划摘要，玩家与迁移选项只能取自审定计划")
	}
	return nil
}

func releaseDigestShape(value string, n int) bool {
	return len(value) == n && strings.Trim(value, "0123456789abcdef") == "" && value != strings.Repeat("0", n)
}

func validateMaintenanceReleaseOptions(o options) error {
	if o.mode != "release-plan" && o.mode != "release-apply" {
		return errors.New("解除入口必须显式指定release-plan或release-apply")
	}
	if o.zone == 0 || o.zone > 999999 || o.target().ValidateTarget() != nil || o.maintenanceID == "" || len(o.maintenanceID) > 128 || o.planFile == "" || o.schema == "" || o.redisAddr == "" || o.dsnEnv == "" || o.redisPasswordEnv == "" {
		return errors.New("显式解除必须提供完整受审SQL/Redis目标、zone、维护号与计划文件")
	}
	if o.routingMode != barrierrestore.RoutingModeDBFirst || o.localMySQL || o.fenceOnlyPromotion || o.allowLegacy || o.allowHomeFallback || o.players != "" || o.auditedOwnerUpperBound != "" || o.auditFile != "" || o.auditSHA != "" {
		return errors.New("显式解除只支持原TiDB/db_first，不接受资产恢复、promotion或旧迁移参数")
	}
	if !o.writersStopped || !o.queuesDrained || !releaseDigestShape(o.localTarget, 32) || o.sqlTarget == "" || o.observationFile == "" || o.releaseReview == "" || !releaseDigestShape(o.releaseReviewSHA, 64) || o.releaseAttempt == "" || !releaseDigestShape(o.releaseAttemptSHA, 64) {
		return errors.New("显式解除需要原实例nonce、独立审核原件、本次短排他窗与新的观测文件；声明不能代替真实原件")
	}

	if o.mode == "release-plan" {
		if o.planSHA != "" || o.releaseMembers == "" || !releaseDigestShape(o.releaseMembersSHA, 64) {
			return errors.New("release-plan只读完整成员输入，不接受旧plan摘要")
		}
	} else if !releaseDigestShape(o.planSHA, 64) || o.releaseMembers != "" || o.releaseMembersSHA != "" {
		return errors.New("release-apply只接受完整原planSHA，成员只能取自不可变计划")
	}
	return nil
}

// 显式解除只负责原guard与永久解除收据，不解冻SQL路由，不启动任何业务进程。
func runMaintenanceRelease(ctx context.Context, o options, r barrierrestore.Restorer, out, errOut io.Writer) int {
	expectedSHA := ""
	if o.mode == "release-apply" {
		expectedSHA = o.planSHA
	}
	permit, err := barrierrestore.ReadMaintenanceReleasePermit(ctx, barrierrestore.ReleaseFile{Path: o.releaseReview, SHA256: o.releaseReviewSHA}, barrierrestore.ReleaseFile{Path: o.releaseAttempt, SHA256: o.releaseAttemptSHA}, expectedSHA)
	if err != nil {
		fmt.Fprintln(errOut, "显式解除的不可变审核原件或本次排他窗无效")
		return 2
	}
	bindings, err := readPromotionBindings(ctx, o, r)
	if err != nil {
		fmt.Fprintln(errOut, "显式解除SQL/Redis维护目标核验失败")
		return 2
	}
	r.RedisTargetID = bindings.RedisTarget
	r.MappingTargetID = bindings.MappingTarget
	var p barrierrestore.MaintenanceReleasePlan
	if o.mode == "release-plan" {
		input, e := barrierrestore.ReadMaintenanceReleaseMembers(barrierrestore.ReleaseFile{Path: o.releaseMembers, SHA256: o.releaseMembersSHA})
		if e != nil || input.Zone != uint32(o.zone) || input.MaintenanceID != o.maintenanceID {
			fmt.Fprintln(errOut, "完整成员输入与本次zone/维护号不符")
			return 2
		}
		p, err = r.PrepareMaintenanceRelease(ctx, uint32(o.zone), o.maintenanceID, input.Members, permit)
	} else {
		body, e := readVerifiedFile(o.planFile, o.planSHA, 16<<20)
		if e != nil {
			fmt.Fprintln(errOut, "原解除计划文件或完整SHA不符")
			return 2
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&p) != nil || decoder.Decode(&extra) != io.EOF || p.Zone != uint32(o.zone) || p.MaintenanceID != o.maintenanceID || p.LocalTarget != o.localTarget || p.RouteSchema != o.routeSchema || p.Review.Path != o.releaseReview || p.Review.SHA256 != o.releaseReviewSHA {
			fmt.Fprintln(errOut, "解除计划格式、固定目标或原审核原件不符")
			return 2
		}
		err = r.ApplyMaintenanceRelease(ctx, p, o.planSHA, permit)
	}
	// 已提交前缀或丢回包均保留；不以写观测失败掩盖领域提交的不确定结果。
	status := "prepared"
	if o.mode == "release-apply" {
		status = "release-completed"
	}
	if err != nil {
		status = "failed-or-unknown"
	}
	after, identityErr := readPromotionBindings(ctx, o, r)
	if identityErr != nil || !samePromotionTargets(bindings, after) || bindings.RedisObservedIdentity != after.RedisObservedIdentity || bindings.MappingObservedIdentity != after.MappingObservedIdentity {
		status = "failed-or-unknown"
		if err == nil {
			err = errors.New("显式解除执行期间实例身份变化或尾核失败")
		}
	}
	observation, encodeErr := json.MarshalIndent(struct {
		Format                   string           `json:"format"`
		Mode                     string           `json:"mode"`
		Status                   string           `json:"status"`
		PlanSHA                  string           `json:"plan_sha256,omitempty"`
		Before                   promotionCLIPlan `json:"before"`
		After                    promotionCLIPlan `json:"after"`
		AutomaticUnfreeze        bool             `json:"automatic_unfreeze"`
		BusinessProcessesStarted bool             `json:"business_processes_started"`
	}{"maintenance-release-local-observation-v1", o.mode, status, o.planSHA, bindings, after, false, false}, "", "  ")
	if encodeErr != nil || writeNew(o.observationFile, append(observation, '\n')) != nil {
		fmt.Fprintln(errOut, "解除观测落盘失败；保留原plan及全部已提交前缀，禁止自动放流")
		return 1
	}
	if err != nil {
		fmt.Fprintln(errOut, "显式解除失败或结果未知；保留原plan、永久收据及已提交前缀，禁止自动解冻或重启")
		return 1
	}
	if o.mode == "release-plan" {
		body, e := json.MarshalIndent(p, "", "  ")
		if e != nil {
			fmt.Fprintln(errOut, "解除计划序列化失败")
			return 2
		}
		body = append(body, '\n')
		if writeNew(o.planFile, body) != nil {
			fmt.Fprintln(errOut, "解除计划创建失败；已有文件不会覆盖")
			return 2
		}
		sum := sha256.Sum256(body)
		if _, e := fmt.Fprintf(out, "plan_sha256=%s members=%d；只读计划，维护仍关闭\n", hex.EncodeToString(sum[:]), len(p.Members)); e != nil {
			fmt.Fprintln(errOut, "计划摘要输出失败；保留原件")
			return 1
		}
	} else if _, e := fmt.Fprintln(out, "原维护guard已按同一计划核对解除；SQL路由仍需另行正式audit/unfreeze，本命令不重启SM/Scene"); e != nil {
		fmt.Fprintln(errOut, "解除回执输出失败；按原plan及永久收据消歧")
		return 1
	}
	return 0
}

type promotionCLIPlan struct {
	Format                  string          `json:"format"`
	LocalTarget             string          `json:"local_target_id"`
	SQLTarget               string          `json:"physical_sql_addr"`
	RedisTarget             string          `json:"redis_target_id"`
	MappingTarget           string          `json:"mapping_target_id"`
	RedisAddr               string          `json:"redis_addr"`
	RedisDB                 int             `json:"redis_db"`
	MappingAddr             string          `json:"mapping_addr"`
	MappingDB               int             `json:"mapping_db"`
	RedisObservedIdentity   string          `json:"redis_observed_identity"`
	MappingObservedIdentity string          `json:"mapping_observed_identity"`
	Plan                    json.RawMessage `json:"plan"`
}

func samePromotionTargets(a, b promotionCLIPlan) bool {
	return a.Format == b.Format && a.LocalTarget == b.LocalTarget && a.SQLTarget == b.SQLTarget && a.RedisTarget == b.RedisTarget && a.MappingTarget == b.MappingTarget && a.RedisAddr == b.RedisAddr && a.RedisDB == b.RedisDB && a.MappingAddr == b.MappingAddr && a.MappingDB == b.MappingDB
}

func runPromotion(ctx context.Context, o options, r barrierrestore.Restorer, out, errOut io.Writer) int {
	evidence, err := readVerifiedFile(o.auditFile, o.auditSHA, 1<<20)
	if err != nil {
		fmt.Fprintln(errOut, "独立审核证据原件不符")
		return 2
	}
	sum := sha256.Sum256(evidence)
	bindings, err := readPromotionBindings(ctx, o, r)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	r.RedisTargetID = bindings.RedisTarget
	r.MappingTargetID = bindings.MappingTarget
	observation, err := json.Marshal(bindings)
	if err != nil || writeNew(o.observationFile, append(observation, '\n')) != nil {
		fmt.Fprintln(errOut, "目标当次观测必须新建留存，未进行提升")
		return 2
	}
	if o.mode == "plan" {
		plan, err := r.PreparePromotion(ctx, o.schema, uint32(o.zone), o.maintenanceID, o.players, sum[:])
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		inner, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(plan)
		if err != nil {
			fmt.Fprintln(errOut, "提升计划编码失败")
			return 2
		}
		bindings.Plan = inner
		after, err := readPromotionBindings(ctx, o, r)
		if err != nil || !samePromotionTargets(after, bindings) {
			fmt.Fprintln(errOut, "只读计划期间目标实例变化")
			return 1
		}
		body, err := json.MarshalIndent(bindings, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "目标封套编码失败")
			return 2
		}
		body = append(body, '\n')
		if err := writeNew(o.planFile, body); err != nil {
			fmt.Fprintln(errOut, "提升计划创建失败，原文件不覆盖")
			return 2
		}
		hash := sha256.Sum256(body)
		fmt.Fprintf(out, "plan_sha256=%s；仅只读计划，未改业务数据或围栏\n", hex.EncodeToString(hash[:]))
		return 0
	}
	body, err := readVerifiedFile(o.planFile, o.planSHA, 32768)
	if err != nil {
		fmt.Fprintln(errOut, "审定提升计划原bytes不符")
		return 2
	}
	var envelope promotionCLIPlan
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil {
		fmt.Fprintln(errOut, "提升目标封套格式不符")
		return 2
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || !samePromotionTargets(envelope, bindings) {
		fmt.Fprintln(errOut, "提升目标SQL/Redis实例偏离审定封套")
		return 2
	}
	plan := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan{}
	if protojson.Unmarshal(envelope.Plan, plan) != nil || barrierrestore.ValidatePromotion(plan) != nil || plan.RowSchema != o.schema || uint(plan.HomeZone) != o.zone || plan.MaintenanceId != o.maintenanceID || !bytes.Equal(plan.AuditEvidenceSha256, sum[:]) {
		fmt.Fprintln(errOut, "提升计划契约、目标或审核证据不符")
		return 2
	}
	receipt, err := r.ApplyPromotion(ctx, plan)
	if err != nil {
		fmt.Fprintln(errOut, err)
		fmt.Fprintln(errOut, "保持全部维护闸，不修改原计划/nonce，仅同计划重读未知阶段")
		return 1
	}
	body, err = (protojson.MarshalOptions{UseProtoNames: true}).Marshal(receipt)
	if err != nil {
		fmt.Fprintln(errOut, "维护收据编码失败，保持维护")
		return 1
	}
	if _, err = fmt.Fprintln(out, string(body)); err != nil {
		fmt.Fprintln(errOut, "维护收据输出失败，仅同计划重读")
		return 1
	}
	return 0
}

func readPromotionBindings(ctx context.Context, o options, r barrierrestore.Restorer) (promotionCLIPlan, error) {
	result := promotionCLIPlan{Format: "fence-only-promotion-cli-plan-v2", LocalTarget: o.localTarget, SQLTarget: o.sqlTarget, RedisAddr: r.Redis.Options().Addr, RedisDB: r.Redis.Options().DB, MappingAddr: o.target().MappingAddr, MappingDB: o.target().MappingDB}
	dsn, err := mysql.ParseDSN(os.Getenv(o.dsnEnv))
	if err != nil || dsn.Net != "tcp" || dsn.Addr != o.sqlTarget || r.MaintenanceSQLAddr != o.sqlTarget {
		return result, errors.New("维护SQL物理地址偏离显式审定目标")
	}
	purpose, scopeSchema := "promotion", o.schema
	if o.mode == "release-plan" || o.mode == "release-apply" {
		purpose, scopeSchema = "release", o.routeSchema
	}
	read := func(client *redis.Client, role string) (string, string, error) {
		marker, err := barrierrestore.ReadMaintenanceRedisMarker(ctx, client, purpose, role, uint32(o.zone), o.routeSchema, scopeSchema)
		if err != nil {
			return "", "", err
		}
		server, err := client.Info(ctx, "server").Result()
		if err != nil {
			return "", "", err
		}
		repl, err := client.Info(ctx, "replication").Result()
		if err != nil {
			return "", "", err
		}
		field := func(raw, key string) string {
			for _, line := range strings.Split(raw, "\n") {
				name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
				if ok && name == key {
					return value
				}
			}
			return ""
		}
		run, replica := field(server, "run_id"), field(repl, "master_replid")
		if len(run) != 40 || len(replica) != 40 || strings.Trim(run+replica, "0123456789abcdef") != "" || field(repl, "role") != "master" {
			return "", "", errors.New("Redis实例身份未知或不是主")
		}
		return marker.Nonce, run + ":" + replica, nil
	}
	result.RedisTarget, result.RedisObservedIdentity, err = read(r.Redis, "core")
	if err != nil {
		return result, err
	}
	mapping := r.MappingRedis
	if mapping == nil {
		mapping = r.Redis
	}
	result.MappingTarget, result.MappingObservedIdentity, err = read(mapping, "mapping")
	if err == nil && r.Redis.Options().Addr == mapping.Options().Addr && r.Redis.Options().DB == mapping.Options().DB && result.RedisTarget != result.MappingTarget {
		return result, errors.New("同物理Redis目标nonce不一致")
	}
	return result, err
}

func readVerifiedFile(path, digest string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("原件大小或读取失败")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("原件SHA不符")
	}
	return body, nil
}

func (o options) target() barrierrestore.Plan {
	addr := o.mappingAddr
	if addr == "" {
		addr = o.redisAddr
	}
	return barrierrestore.Plan{Format: barrierrestore.FormatV2, Schema: o.schema, Zone: uint32(o.zone), RedisAddr: o.redisAddr, RedisDB: o.redisDB, RoutingMode: o.routingMode, MappingAddr: addr, MappingDB: o.mappingDB, RouteSchema: o.routeSchema, MaintenanceID: o.maintenanceID, LocalMySQL: o.localMySQL, AuditedOwnerUpperBound: o.auditedOwnerUpperBound}
}

func readPlan(path, digest string) (barrierrestore.Plan, error) {
	var p barrierrestore.Plan
	f, err := os.Open(path)
	if err != nil {
		return p, errors.New("无法读取审定计划")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if err != nil || len(b) > 2*1024*1024 {
		return p, errors.New("计划过大或读取失败")
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != digest {
		return p, errors.New("审定计划 SHA-256 不符")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, errors.New("计划格式无效")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, errors.New("计划包含额外内容")
	}
	return p, p.Validate()
}

func writeNew(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(body)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func runAuthorityPhase(ctx context.Context, o options, out, errOut io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(errOut, err); return 2 }
	if !filepath.IsAbs(o.authorityOutput) {
		return fail(errors.New("authority结果必须为独立绝对新路径"))
	}
	if o.authorityPhase == "collect-kafka-catalog" {
		budget, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		brokers := strings.Split(os.Getenv(o.authorityKafkaEnv), ",")
		for i := range brokers {
			brokers[i] = strings.TrimSpace(brokers[i])
			if brokers[i] == "" {
				return fail(errors.New("Kafka broker目录缺失"))
			}
		}
		prober, err := kafkautil.NewGroupLagProber(brokers)
		if err != nil {
			return fail(err)
		}
		raw, err := prober.CaptureDrainCatalog(budget)
		closeErr := prober.Close()
		if err != nil || closeErr != nil {
			return fail(errors.Join(err, closeErr))
		}
		if err = writeNew(o.authorityOutput, raw); err != nil {
			return fail(err)
		}
		return 0
	}
	runtime, err := drainproof.LoadRuntime(o.maintenancePlan, o.maintenanceDir, o.maintenanceSHA)
	if err != nil {
		return fail(err)
	}
	if o.authorityPhase != "collect-kafka-baseline" {
		if err = barrierrestore.ValidateAuthorityPlan(runtime.Plan); err != nil {
			return fail(err)
		}
	}
	budget, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if o.authorityPhase == "collect-kafka-baseline" {
		brokers := strings.Split(os.Getenv(o.authorityKafkaEnv), ",")
		for i := range brokers {
			brokers[i] = strings.TrimSpace(brokers[i])
			if brokers[i] == "" {
				return fail(errors.New("Kafka broker目录缺失"))
			}
		}
		prober, err := kafkautil.NewGroupLagProber(brokers)
		if err != nil {
			return fail(err)
		}
		rows, baseline, catalogRaw, err := prober.CaptureDrainBaseline(budget, runtime.Plan.KafkaPartitions)
		var manualBaseline, manualCatalog []byte
		if err == nil && len(runtime.Plan.ManualAssignments) > 0 {
			_, manualBaseline, manualCatalog, err = prober.CaptureManualBaseline(budget, runtime.Plan.ManualAssignments)
		}
		closeErr := prober.Close()
		if err != nil || closeErr != nil {
			return fail(errors.Join(err, closeErr))
		}
		catalogSum := sha256.Sum256(catalogRaw)
		catalogPath := o.authorityOutput + ".catalog.json"
		if err = writeNew(catalogPath, catalogRaw); err != nil {
			return fail(err)
		}
		baselinePath := o.authorityOutput + ".baseline.json"
		if err = writeNew(baselinePath, baseline); err != nil {
			return fail(err)
		}
		baselineSum := sha256.Sum256(baseline)
		plan := proto.Clone(runtime.Plan).(*databasepb.SqlWriterMaintenanceReceipt_Plan)
		plan.KafkaPartitions = rows
		pins := []*databasepb.SqlWriterMaintenanceReceipt_SourcePin{}
		for _, pin := range plan.SourcePins {
			raw, readErr := drainproof.ReadPin(pin, 16<<20)
			if readErr != nil {
				return fail(readErr)
			}
			var marker struct {
				Schema string `json:"schema"`
			}
			if json.Unmarshal(raw, &marker) == nil && (marker.Schema == "SQL_DRAIN_KAFKA_CATALOG_V2" || marker.Schema == "SQL_DRAIN_KAFKA_BASELINE_V2") {
				continue
			}
			pins = append(pins, pin)
		}
		plan.SourcePins = append(pins, &databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: catalogPath, Sha256: catalogSum[:]}, &databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: baselinePath, Sha256: baselineSum[:]})
		if len(manualBaseline) > 0 {
			manualCatalogPath := o.authorityOutput + ".manual.catalog.json"
			if err = writeNew(manualCatalogPath, manualCatalog); err != nil {
				return fail(err)
			}
			manualCatalogSum := sha256.Sum256(manualCatalog)
			var captured kafkautil.DrainManualKafkaBaseline
			if err = json.Unmarshal(manualBaseline, &captured); err != nil {
				return fail(err)
			}
			captured.CatalogSource = &databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: manualCatalogPath, Sha256: manualCatalogSum[:]}
			manualBaseline, err = json.Marshal(&captured)
			if err != nil {
				return fail(err)
			}
			manualPath := o.authorityOutput + ".manual.baseline.json"
			if err = writeNew(manualPath, manualBaseline); err != nil {
				return fail(err)
			}
			manualSum := sha256.Sum256(manualBaseline)
			plan.SourcePins = append(plan.SourcePins, &databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: manualPath, Sha256: manualSum[:]})
		}
		if err = barrierrestore.ValidateAuthorityPlan(plan); err != nil {
			return fail(err)
		}
		planRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
		if err != nil {
			return fail(err)
		}
		sum := sha256.Sum256(planRaw)
		request := proto.Clone(runtime.Request).(*databasepb.SqlWriterMaintenanceReceipt)
		request.PlanProto = planRaw
		request.Evidence.ManifestDigest = sum[:]
		request, err = authoritysql.SealMaintenanceRequest(request)
		if err != nil {
			return fail(err)
		}
		raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(request)
		if err != nil {
			return fail(err)
		}
		if err = writeNew(o.authorityOutput, raw); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "P0完整typed原件已绑定实际C0/L0；未执行SQL阶段变更")
		return 0
	}
	if o.authorityPhase == "collect-kafka" {
		brokers := strings.Split(os.Getenv(o.authorityKafkaEnv), ",")
		for i := range brokers {
			brokers[i] = strings.TrimSpace(brokers[i])
			if brokers[i] == "" {
				return fail(errors.New("Kafka broker目录缺失"))
			}
		}
		start := uint64(time.Now().UnixMilli())
		prober, err := kafkautil.NewGroupLagProber(brokers)
		if err != nil {
			return fail(err)
		}
		rows, before, after, err := prober.DrainPartitions(budget, runtime.Plan.KafkaPartitions)
		var manual []*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence
		var leaderRaw, manualBefore, manualAfter []byte
		if err == nil && len(runtime.Plan.ManualAssignments) > 0 {
			input := &databasepb.SqlWriterMaintenanceReceipt{}
			err = barrierrestore.ReadAuthorityProto(barrierrestore.ReleaseFile{Path: o.authorityRequest, SHA256: o.authorityRequestSHA}, input)
			manifestSum := sha256.Sum256(runtime.Request.PlanProto)
			if err == nil && (input.Evidence == nil || input.Evidence.GetNonSqlClosure() == nil || !bytes.Equal(input.RunId, runtime.Plan.RunId) || !bytes.Equal(input.PlanProto, runtime.Request.PlanProto) || !bytes.Equal(input.Evidence.ManifestDigest, manifestSum[:])) {
				err = errors.New("manual P3原件输入没有绑定同run原manifest")
			}
			if err == nil {
				manual, leaderRaw, manualBefore, manualAfter, err = prober.DrainManualConsumers(budget, runtime.Plan.ManualAssignments, input.Evidence.GetNonSqlClosure().SourceEvidence)
			}
		}
		closeErr := prober.Close()
		if err != nil || closeErr != nil {
			return fail(errors.Join(err, closeErr))
		}
		beforePath, afterPath := o.authorityOutput+".catalog.before.json", o.authorityOutput+".catalog.after.json"
		if err = writeNew(beforePath, before); err != nil {
			return fail(err)
		}
		if err = writeNew(afterPath, after); err != nil {
			return fail(err)
		}
		a, b := sha256.Sum256(before), sha256.Sum256(after)
		historyDigest, err := kafkautil.StableDrainCatalogDigest(before)
		if err != nil {
			return fail(err)
		}
		closure := &databasepb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence{HistoricalInventoryDigest: historyDigest, KafkaPartitions: rows, ObservedStartMs: start, ObservedEndMs: uint64(time.Now().UnixMilli()), SourceEvidence: []*databasepb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: beforePath, Sha256: a[:]}, {AbsolutePath: afterPath, Sha256: b[:]}}}
		if len(manual) > 0 {
			leaderPath := o.authorityOutput + ".manual.leader.json"
			if err = writeNew(leaderPath, leaderRaw); err != nil {
				return fail(err)
			}
			leaderSum := sha256.Sum256(leaderRaw)
			leaderPin := &databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: leaderPath, Sha256: leaderSum[:]}
			closure.SourceEvidence = append(closure.SourceEvidence, leaderPin)
			for _, captured := range []struct {
				suffix string
				raw    []byte
			}{{".manual.catalog.before.json", manualBefore}, {".manual.catalog.after.json", manualAfter}} {
				path := o.authorityOutput + captured.suffix
				if err = writeNew(path, captured.raw); err != nil {
					return fail(err)
				}
				sum := sha256.Sum256(captured.raw)
				closure.SourceEvidence = append(closure.SourceEvidence, &databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: path, Sha256: sum[:]})
			}
			for _, report := range manual {
				report.SourcePins = append(report.SourcePins, proto.Clone(leaderPin).(*databasepb.SqlWriterMaintenanceReceipt_SourcePin))
			}
			closure.ManualConsumers = manual
		}
		raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(closure)
		if err != nil {
			return fail(err)
		}
		if err = writeNew(o.authorityOutput, raw); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Kafka正式原件已采集；仍须同owner完整nonSQL核验")
		return 0
	}
	request := &databasepb.SqlWriterMaintenanceReceipt{}
	if err = barrierrestore.ReadAuthorityProto(barrierrestore.ReleaseFile{Path: o.authorityRequest, SHA256: o.authorityRequestSHA}, request); err != nil {
		return fail(err)
	}
	var ticket *databasepb.SqlWriterFreezeTicket
	if o.authorityTicket != "" {
		ticket = &databasepb.SqlWriterFreezeTicket{}
		if err = barrierrestore.ReadAuthorityProto(barrierrestore.ReleaseFile{Path: o.authorityTicket, SHA256: o.authorityTicketSHA}, ticket); err != nil {
			return fail(err)
		}
	}
	if o.authorityPhase == "seal-request" {
		if request.Evidence == nil || !bytes.Equal(request.RunId, runtime.Plan.RunId) || !bytes.Equal(request.PlanProto, runtime.Request.PlanProto) {
			return fail(errors.New("待封请求未绑定原完整run/manifest"))
		}
		sum := sha256.Sum256(request.PlanProto)
		if !bytes.Equal(request.Evidence.ManifestDigest, sum[:]) || !bytes.Equal(request.Evidence.OperationId, request.RunId) {
			return fail(errors.New("待封请求evidence身份不符"))
		}
		if publication := request.Evidence.GetPublication(); publication != nil && request.RunPhase != databasepb.SqlWriterDrainRun_PLAN_FIXED {
			seeds := runtime.Plan.RootIssuers
			if request.Kind == databasepb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED {
				seeds = runtime.Plan.QueueMutators
			}
			seedRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&databasepb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: seeds})
			if err != nil {
				return fail(err)
			}
			inventory := sha256.Sum256(seedRaw)
			publication.IssuerInventoryDigest = inventory[:]
			if err = barrierrestore.VerifyAuthorityEvidence(budget, runtime.Plan, runtime.Request.PlanProto, request, nil, nil); err != nil {
				return fail(err)
			}
		}
		request, err = authoritysql.SealMaintenanceRequest(request)
		if err != nil {
			return fail(err)
		}
		raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(request)
		if err != nil {
			return fail(err)
		}
		if err = writeNew(o.authorityOutput, raw); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "正式请求已确定性封装；未执行SQL变更")
		return 0
	}
	permission, err := authoritysql.LoadOperatorPermission(os.Getenv)
	if err != nil {
		return fail(err)
	}
	db, err := authoritysql.Open(budget, os.Getenv(o.dsnEnv), runtime.Plan.AuthoritySchema, !o.localMySQL, 1, permission)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{AuthoritySchema: runtime.Plan.AuthoritySchema, Generation: runtime.Request.Generation, ProtectedSchemas: runtime.Plan.ProtectedSchemas, PhysicalAuthoritySHA256: runtime.Plan.PhysicalClusterIdentity, Principal: os.Getenv("MMORPG_SQL_OPERATOR_PRINCIPAL"), RequireReconcile: true})
	if err != nil {
		return fail(err)
	}
	if o.authorityPhase == "read" {
		var ticketID []byte
		if ticket != nil {
			ticketID = ticket.TicketId
		}
		receipt, run, ticket, err := barrierrestore.ReadAuthorityOutcome(budget, catalog, request.RequestId, request.RunId, ticketID)
		if err != nil {
			return fail(err)
		}
		// 三份分别按正式proto保存，缺任何原件都不把UNKNOWN改为已完成。
		for _, item := range []struct {
			suffix  string
			message proto.Message
		}{{".receipt.json", receipt}, {".run.json", run}} {
			raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(item.message)
			if err != nil {
				return fail(err)
			}
			if err = writeNew(o.authorityOutput+item.suffix, raw); err != nil {
				return fail(err)
			}
		}
		if ticket != nil {
			raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(ticket)
			if err != nil {
				return fail(err)
			}
			if err = writeNew(o.authorityOutput+".ticket.json", raw); err != nil {
				return fail(err)
			}
		}
		fmt.Fprintln(out, "原request/run/ticket已强读；未执行变更")
		return 0
	}
	// attempt文件在SQL前持久CreateNew；重复调用只允许read，不能以无收据推断旧事务未执行。
	attempt := filepath.Join(runtime.Directory, "attempt-"+hex.EncodeToString(request.RequestId)+".started")
	if len(request.RequestId) != 16 {
		return fail(errors.New("authority request_id非法"))
	}
	if err = writeNew(attempt, []byte(hex.EncodeToString(request.RequestDigest)+"\n")); err != nil {
		return fail(errors.New("原动作已尝试或标记未知；仅可authority-phase read"))
	}
	receipt, err := barrierrestore.AuthorityPhase(budget, catalog, runtime, o.authorityPhase, request, ticket, barrierrestore.Restorer{DB: db, RoutingMode: barrierrestore.RoutingModeDBFirst, RouteSchema: runtime.Plan.AuthoritySchema, LocalMySQL: o.localMySQL, LocalTargetID: o.localTarget, MaintenanceSQLAddr: o.sqlTarget})
	if err != nil {
		fmt.Fprintln(errOut, "authority动作未获完整确认；保持所有维护闸，按原ID只读核验")
		return 1
	}
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(receipt)
	if err != nil {
		return fail(err)
	}
	if err = writeNew(o.authorityOutput, raw); err != nil {
		return fail(err)
	}
	fmt.Fprintln(out, "原authority阶段收据已保存；不自动放流")
	return 0
}
