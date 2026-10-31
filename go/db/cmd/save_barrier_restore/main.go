// save_barrier_restore 只在停写维护窗口运行；凭据只经环境变量输入，不写入计划和回执。
// 屏障恢复、提升、解除三种模式随旧存盘路径删除,只剩 -authority-phase(维护排空的阶段动作)。工具名没改:部署和排空脚本都按它固定。
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
	"strings"
	"syscall"
	"time"
)

type options struct {
	authorityPhase, authorityOutput, authorityRequest, authorityRequestSHA, authorityTicket, authorityTicketSHA string
	maintenancePlan, maintenanceDir, maintenanceSHA, authorityKafkaEnv                                          string
	localTarget, sqlTarget, dsnEnv                                                                              string
}

func main() {
	var o options
	flag.StringVar(&o.dsnEnv, "dsn-env", "SAVE_BARRIER_RESTORE_DSN", "SQL DSN 所在环境变量名，不传明文")
	flag.StringVar(&o.localTarget, "local-target-id", "", "审定maintenance-recovery SQL目标32hex nonce；历史参数名不授予调试写权限")
	flag.StringVar(&o.sqlTarget, "sql-target-address", "", "审定无凭据SQL host:port，逐字绑定DSN和永久维护标记")
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
	if o.authorityPhase == "" {
		fmt.Fprintln(errOut, "only -authority-phase is supported: the barrier restore, promotion and release modes were removed with the old save path")
		return 2
	}
	return runAuthorityPhase(ctx, o, out, errOut)
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
	backend, err := authoritysql.LoadBackend(os.Getenv)
	if err != nil {
		return fail(err)
	}
	db, err := authoritysql.Open(budget, os.Getenv(o.dsnEnv), runtime.Plan.AuthoritySchema, backend, 1, permission)
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
	receipt, err := barrierrestore.AuthorityPhase(budget, catalog, runtime, o.authorityPhase, request, ticket, barrierrestore.Restorer{DB: db, RoutingMode: barrierrestore.RoutingModeDBFirst, RouteSchema: runtime.Plan.AuthoritySchema, LocalTargetID: o.localTarget, MaintenanceSQLAddr: o.sqlTarget})
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
