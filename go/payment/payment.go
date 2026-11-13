// payment 服务:真钱支付的渠道适配(docs/design/payment-recharge.md §22)。
//
// 全局一份、多副本、进程无状态:数据在独占库 mmorpg_payment(port-decisions D-14)。客户端协议 ClientPlayerPayment 经 gate → 路由服到达
// (按 PaymentNodeService.rpc 前缀发现);渠道回调走独立的 HTTP 端口(Payment.NotifyListenAddr,经 Ingress 暴露到公网)。
// 订单在商城(java/mall_game),本服务以 payment 角色向它记账、同步修订、回执退款;发货与扣回由 trade 收敛。
//
// 两种运行形态:
//   - 常驻服务(默认):核对表结构 → 渠道 → 回调入口 → 后台任务(只在 stable 副本)→ gRPC → 注册 etcd;
//   - `-migrate`:只对 mmorpg_payment 跑一次 schemamigrate.Up,按 D-14 退出码退出(K8s payment-migrate Job 用)。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"

	"payment/internal/config"
	"payment/internal/core"
	"payment/internal/data"
	"payment/internal/mall"
	"payment/internal/notify"
	"payment/internal/server"
	base "proto/common/base"
	paymentpb "proto/payment"
	"schemamigrate"
	"shared/authoritysql"
	"shared/buildinfo"
	"shared/dbplacement"
	"shared/domainhost"
	"shared/drainproof"
	"shared/releasetrack"
	"shared/serverbase"
	"shared/sqlpoolstats"
)

var maintenancePlan = flag.String("maintenance-drain-plan", "", "typed drain runtime receipt")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "private drain receipt directory")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "SHA256 of the runtime receipt file")
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "private directory receiving drain requests for this boot")

func main() {
	file := flag.String("f", "etc/payment.yaml", "config file")
	migrate := flag.Bool("migrate", false, "run schemamigrate.Up on mmorpg_payment and exit (0 ok / 1 error / 3 lock busy / 4 manual)")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	fmt.Println(buildinfo.StartupLine("payment"))
	if *version {
		return
	}
	var c config.Config
	conf.MustLoad(*file, &c)
	if err := run(c, *migrate); err != nil {
		fmt.Fprintln(os.Stderr, "[payment]", err)
		os.Exit(exitCode(err))
	}
}

var errManualSchema = errors.New("payment schema needs manual action")

// exitCode 把 run 的错误映射成进程退出码(D-14,迁移 Job 的 podFailurePolicy 按它判):需人工 4,迁移锁忙 3(可重试),其余 1。
func exitCode(err error) int {
	if errors.Is(err, errManualSchema) {
		return schemamigrate.ExitManual
	}
	return schemamigrate.ExitCode(schemamigrate.Report{}, err)
}

// schemaGate 把迁移(-migrate)或只读核对(常驻启动)的报告判成能不能继续:需人工项、常驻启动还有待执行语句、缺普通索引都拒启
// (与 rank / mail 同一口径:schemamigrate 对已存在的表不补建索引,而推进与查单的扫描都靠索引)。
func schemaGate(report schemamigrate.Report, migrate bool) error {
	if len(report.Manual) > 0 {
		return fmt.Errorf("%w: %s", errManualSchema, strings.Join(report.Manual, ";"))
	}
	if !migrate && !report.Clean() {
		return errors.New("payment schema is not deployed; run payment -migrate first")
	}
	for _, warning := range report.Warnings {
		logx.Info(warning)
	}
	if missing := report.MissingIndexes(); len(missing) > 0 {
		return fmt.Errorf("%w: payment indexes are missing: %s", errManualSchema, strings.Join(schemamigrate.Messages(missing), ";"))
	}
	return nil
}

func run(c config.Config, migrate bool) (runErr error) {
	if err := c.Validate(); err != nil {
		return err
	}
	// 发布轨道(docs/design/release-track-gray-canary.md §2.5):canary 副本只处理客户端请求与回调,不跑全局后台任务。
	releaseTrack, err := releasetrack.FromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer logx.Close()
	if !migrate && c.AutoMigrate {
		return errors.New("the resident service refuses automatic DDL; run payment -migrate explicitly")
	}
	var permission authoritysql.Permission
	var sqlWriter authoritysql.WriterConfig
	var runtime *drainproof.Runtime
	var root, relay *drainproof.Session
	var poolOptions []authoritysql.OpenOption
	if migrate {
		if permission, err = authoritysql.LoadSchemaPermission(os.Getenv); err == nil {
			// 搬库期间(state=MOVING)拒绝 DDL;库搬走之后连当前集群。
			poolOptions, err = dbplacement.ForMigration(ctx, c.DbPlacement, data.DatabaseName)
		}
	} else {
		if runtime, root, relay, err = serverbase.LoadPublicationRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA); err != nil {
			return err
		}
		var placement *dbplacement.Watcher
		if placement, err = dbplacement.Start(ctx, c.DbPlacement); err != nil {
			return err
		}
		poolOptions = placement.OpenOptions()
		sqlWriter, err = authoritysql.LoadWriterConfig(os.Getenv)
		permission = authoritysql.Writer(sqlWriter)
	}
	if err != nil {
		return err
	}
	resourcesTransferred := false
	db, err := authoritysql.Open(ctx, os.Getenv(c.DSNEnv), data.DatabaseName, authoritysql.Backend(c.Backend), c.MaxOpenConns, permission, poolOptions...)
	if err != nil {
		return err
	}
	defer func() {
		if !resourcesTransferred {
			runErr = errors.Join(runErr, db.Close())
		}
	}()
	options := schemamigrate.Options{Database: data.DatabaseName, Tables: data.Tables(), Logf: logx.Infof}
	var report schemamigrate.Report
	if migrate {
		report, err = schemamigrate.Up(ctx, db, options)
		if err == nil {
			err = authoritysql.EnsureCatalogSchema(ctx, db, data.DatabaseName) // 闸门跟着逻辑库走:本库自带一份
		}
	} else {
		report, err = schemamigrate.Plan(ctx, db, options)
	}
	if err != nil {
		return err
	}
	if err = schemaGate(report, migrate); err != nil {
		return err
	}
	if migrate {
		return nil
	}

	channels, err := buildChannels(c, os.LookupEnv)
	if err != nil {
		return err
	}
	mallSecret, err := secretValue(os.LookupEnv, "Payment.Mall.PaymentSecretEnv", c.Payment.Mall.PaymentSecretEnv, 32)
	if err != nil {
		return err
	}
	mallClient, err := mall.New(c.Payment.Mall.BaseURL, c.Payment.Mall.PaymentSubject, mallSecret, time.Duration(c.Payment.Mall.TimeoutMs)*time.Millisecond)
	if err != nil {
		return err
	}
	platforms, err := platformChannels(c.Payment.Platforms)
	if err != nil {
		return err
	}
	svc, err := core.New(data.New(db), mallClient, channels, platforms, serviceOptions(c))
	if err != nil {
		return err
	}
	core.RegisterMetrics()
	// 连接池指标 mmorpg_sql_pool_*{sql_client="payment",pool="primary"}:只有支付库这一个池,抓取时现取 DBStats。
	sqlpoolstats.Register("payment", []string{sqlpoolstats.PrimaryPool}, sqlpoolstats.Single(sqlpoolstats.PrimaryPool, db))

	notifyServer := notify.New(c.Payment.NotifyListenAddr, svc, relay)
	if err := notifyServer.Start(); err != nil {
		return fmt.Errorf("payment notification listener on %s: %w", c.Payment.NotifyListenAddr, err)
	}
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	workersDone := make(chan struct{})
	if releaseTrack.RunsGlobalBackgroundWork() {
		go func() { defer close(workersDone); svc.Run(workerCtx, relay) }()
	} else {
		close(workersDone)
		logx.Infof("[payment] release_track=%s: background workers disabled (canary serves client RPCs and callbacks only)", releaseTrack)
	}
	resourcesTransferred = true

	var sourceMu, relayMu sync.Mutex
	sourceClosed, relayClosed := false, false
	var relayErr error
	closeSource := func(closeCtx context.Context) error {
		sourceMu.Lock()
		defer sourceMu.Unlock()
		if sourceClosed {
			return nil
		}
		root.CloseAdmission()
		if err := root.Wait(closeCtx); err != nil {
			return err
		}
		sourceClosed = true
		return nil
	}
	// closeRelay 的顺序:先关客户端入口;再停回调入口(不收新回调,在途的做完);再停后台任务;最后等 relay 收敛、封写者、关库。
	closeRelay := func(closeCtx context.Context) error {
		relayMu.Lock()
		defer relayMu.Unlock()
		if relayClosed {
			return relayErr
		}
		if err := closeSource(closeCtx); err != nil {
			return err
		}
		if err := notifyServer.Shutdown(closeCtx); err != nil {
			relay.Hold()
			return err
		}
		cancelWorkers()
		select {
		case <-workersDone:
		case <-closeCtx.Done():
			relay.Hold()
			return errors.Join(drainproof.ErrHold, closeCtx.Err())
		}
		if err := relay.WaitIdle(closeCtx); err != nil {
			return err
		}
		relay.CloseAdmission()
		if err := relay.Wait(closeCtx); err != nil {
			return err
		}
		sqlWriter.Seal()
		relayErr = db.Close()
		relayClosed = true
		if relayErr != nil {
			relay.Hold()
		}
		return relayErr
	}
	listening := make(chan struct{})
	controlCtx, cancelControl := context.WithCancel(ctx)
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		bind := func(r *drainproof.Runtime) error { return r.BindBoot(sqlWriter.InstanceID(), sqlWriter.WriterKey()) }
		prepared := func(prepareCtx context.Context) (*serverbase.PreparedPublication, error) {
			select {
			case <-listening:
			case <-prepareCtx.Done():
				return nil, prepareCtx.Err()
			}
			if root.Pending() != 0 {
				return nil, drainproof.ErrHold
			}
			return &serverbase.PreparedPublication{NoConsumers: true}, nil // 支付服务没有 Kafka 消费者
		}
		if err := serverbase.DriveDrainRuntime(controlCtx, runtime, *maintenanceControlDir, root, relay, bind, closeSource, closeRelay, prepared); err != nil && controlCtx.Err() == nil {
			logx.Errorf("[payment] drain HOLD: %v", err)
		}
	}()
	defer func() {
		root.CloseAdmission()
		cancelControl()
		<-controlDone
		closeCtx, release := context.WithTimeout(context.Background(), time.Minute)
		defer release()
		runErr = errors.Join(runErr, closeRelay(closeCtx))
	}()
	host := domainhost.Config{RpcServerConf: c.RpcServerConf, ZoneId: c.ZoneId, MetricsListenAddr: c.MetricsListenAddr,
		KillSwitchPrefix: c.KillSwitchPrefix, AdvertiseHost: c.AdvertiseHost}
	return domainhost.ServeWithReady(ctx, host, base.ENodeType_PaymentNodeService, func(s *grpc.Server) {
		paymentpb.RegisterClientPlayerPaymentServer(s, server.New(svc))
	}, func(string) { close(listening) }, root, relay)
}
