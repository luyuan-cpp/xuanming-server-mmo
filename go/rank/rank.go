package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"shared/drainproof"
	"shared/serverbase"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	base "proto/common/base"
	mailpb "proto/mail"
	rankpb "proto/rank"
	"rank/internal/data"
	"rank/internal/metrics"
	"rank/internal/reward"
	"rank/internal/server"
	"schemamigrate"
	"shared/authoritysql"
	"shared/buildinfo"
	"shared/dbplacement"
	"shared/domainhost"
	tables "shared/generated/table"
	"shared/releasetrack"
	"shared/serviceauth"
	"shared/sqlpoolstats"
)

type config struct {
	domainhost.Config
	MailRpc       zrpc.RpcClientConf `json:"MailRpc"`
	MailSecretEnv string             `json:"MailSecretEnv"`

	// MailMaxPersonalMailTtlDays 必须与 mail 的 Mail.MaxPersonalMailTtlDays 同值:rank 读不到邮件配置,只能显式写一份
	// (两份源配置由 config_test.go 核对)。封榜写出的奖励邮件过期时刻 = 封榜 + min(30 天, 本值)。
	MailMaxPersonalMailTtlDays uint32 `json:"MailMaxPersonalMailTtlDays"`
}

// Validate 在加载配置(conf.MustLoad)与 run 入口各核一次;奖励邮件有效期越出邮件允许的范围就拒启,
// 不让封榜写出邮件必拒的过期时刻。
func (c config) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	_, err := data.RewardMailWindow(c.MailMaxPersonalMailTtlDays)
	return err
}

var errManualSchema = errors.New("领域结构需人工处理")

var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式 typed 排空 runtime 原件")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有排空原件目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime 原文件 SHA256")
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "同一 boot 接收排空请求的私有目录")

func main() {
	file := flag.String("f", "etc/rank.yaml", "配置文件")
	migrate := flag.Bool("migrate", false, "仅按proto迁移领域库")
	version := flag.Bool("version", false, "打印版本")
	flag.Parse()
	fmt.Println(buildinfo.StartupLine("rank"))
	if *version {
		return
	}
	var c config
	conf.MustLoad(*file, &c)
	if err := run(c, *migrate); err != nil {
		fmt.Fprintln(os.Stderr, "[rank]", err)
		code := schemamigrate.ExitCode(schemamigrate.Report{}, err)
		if errors.Is(err, errManualSchema) {
			code = schemamigrate.ExitManual
		}
		os.Exit(code)
	}
}
func run(c config, migrate bool) (runErr error) {
	if err := c.Validate(); err != nil {
		return err
	}
	// 发布轨道(docs/design/release-track-gray-canary.md §2.5):canary 副本只处理客户端请求,不跑奖励投递这条全局后台。
	// 取值非法就拒启。错误另起名字:本函数后面有 var err error。
	releaseTrack, trackErr := releasetrack.FromEnv()
	if trackErr != nil {
		return trackErr
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer logx.Close()
	if !migrate && c.AutoMigrate {
		return errors.New("常驻服务禁止自动DDL，请显式运行 -migrate")
	}
	var permission authoritysql.Permission
	var sqlWriter authoritysql.WriterConfig
	var runtime *drainproof.Runtime
	var root, relay *drainproof.Session
	relayIngress := drainproof.New()
	var poolOptions []authoritysql.OpenOption
	var err error
	if migrate {
		permission, err = authoritysql.LoadSchemaPermission(os.Getenv)
		if err == nil {
			// 搬库期间(state=MOVING)拒绝 DDL;库搬走之后连当前集群。
			poolOptions, err = dbplacement.ForMigration(ctx, c.DbPlacement, data.DatabaseName)
		}
	} else {
		runtime, root, relay, err = serverbase.LoadPublicationRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
		if err != nil {
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
		// 闸门跟着逻辑库走:本库自带一份。
		if err == nil {
			err = authoritysql.EnsureCatalogSchema(ctx, db, data.DatabaseName)
		}
	} else {
		report, err = schemamigrate.Plan(ctx, db, options)
	}
	if err != nil {
		return err
	}
	if len(report.Manual) > 0 {
		return fmt.Errorf("%w: %s", errManualSchema, strings.Join(report.Manual, ";"))
	}
	if !migrate && !report.Clean() {
		return errors.New("排行榜结构未部署，请先运行rank -migrate")
	}
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "索引") {
			return errors.New("排行榜必要索引缺失")
		}
		logx.Info(warning)
	}
	if migrate {
		return nil
	}
	metrics.Register()
	// 连接池指标 mmorpg_sql_pool_*{sql_client="rank",pool="primary"}:只有排行库这一个池,抓取时现取 DBStats。
	sqlpoolstats.Register("rank", []string{sqlpoolstats.PrimaryPool}, sqlpoolstats.Single(sqlpoolstats.PrimaryPool, db))
	secrets, err := c.Secrets()
	if err != nil {
		return err
	}
	auth, err := serviceauth.New(secrets)
	if err != nil {
		return err
	}
	mailSecret := os.Getenv(c.MailSecretEnv)
	if len(strings.TrimSpace(mailSecret)) < 32 {
		return errors.New("排行榜邮件密钥未注入")
	}
	if err = tables.ItemTableManagerInstance.Load(c.TableDir, false); err != nil {
		return err
	}
	ids, err := domainhost.OpenIDs(ctx, c.DataServiceRpc, "rank_board", "rank_settlement", "rank_reward")
	if err != nil {
		return err
	}
	defer func() {
		if !resourcesTransferred {
			ids.Close()
		}
	}()
	repo := data.New(db, ids.IDs["rank_board"].Next, ids.IDs["rank_settlement"].Next, ids.IDs["rank_reward"].Next, func(id uint32) bool { _, ok := tables.ItemTableManagerInstance.FindById(id); return ok })
	rewardWindow, err := data.RewardMailWindow(c.MailMaxPersonalMailTtlDays)
	if err != nil {
		return err
	}
	if err = repo.ConfigureRewardMailWindow(rewardWindow); err != nil {
		return err
	}
	handler := server.New(repo, auth, ids.Data)
	mailRPC, err := zrpc.NewClient(c.MailRpc)
	if err != nil {
		return err
	}
	defer func() {
		if !resourcesTransferred {
			runErr = errors.Join(runErr, mailRPC.Conn().Close())
		}
	}()
	worker := &reward.Worker{Relay: relay, Repo: repo, Mail: mailpb.NewMailAdminClient(mailRPC.Conn()), Secret: mailSecret}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	finished := make(chan struct{})
	if releaseTrack.RunsGlobalBackgroundWork() {
		go func() { defer close(finished); worker.Run(workerCtx) }()
	} else {
		// canary 副本不投奖励,到期奖励由 stable 副本领租约投出。完成通道直接关掉,停机时 closeRelay 等它立即返回。
		close(finished)
		logx.Infof("[rank] release_track=%s: reward worker disabled (canary serves client RPCs only)", releaseTrack)
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
	closeRelay := func(closeCtx context.Context) error {
		relayMu.Lock()
		defer relayMu.Unlock()
		if relayClosed {
			return relayErr
		}
		if err := closeSource(closeCtx); err != nil {
			return err
		}
		relayIngress.CloseAdmission()
		if err := relayIngress.Wait(closeCtx); err != nil {
			return err
		}
		cancelWorker()
		select {
		case <-finished:
		case <-closeCtx.Done():
			relay.Hold()
			return errors.Join(drainproof.ErrHold, closeCtx.Err())
		}
		// 三个 Client 也包含异步预取；本机无未来调用后，等待实际 Allocate 原件再合入 RELAY。
		for _, tag := range []string{"rank_board", "rank_settlement", "rank_reward"} {
			digest, err := ids.IDs[tag].DrainAndReport(closeCtx)
			if err != nil {
				relay.Hold()
				return err
			}
			if err = relay.MergeSettled(digest); err != nil {
				return err
			}
		}
		if err := relay.WaitIdle(closeCtx); err != nil {
			return err
		}
		relay.CloseAdmission()
		if err := relay.Wait(closeCtx); err != nil {
			return err
		}
		sqlWriter.Seal()
		relayErr = errors.Join(ids.RPC.Conn().Close(), mailRPC.Conn().Close(), db.Close())
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
			return &serverbase.PreparedPublication{NoConsumers: true}, nil
		}
		if err := serverbase.DriveDrainRuntime(controlCtx, runtime, *maintenanceControlDir, root, relay, bind, closeSource, closeRelay, prepared); err != nil && controlCtx.Err() == nil {
			logx.Errorf("rank drain HOLD: %v", err)
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
	return domainhost.ServeWithReady(ctx, c.Config, base.ENodeType_RankNodeService, func(s *grpc.Server) {
		rankpb.RegisterClientPlayerRankServer(s, handler)
		rankpb.RegisterRankAdminServer(s, handler)
	}, func(string) { close(listening) }, root, relay, relayIngress)
}
