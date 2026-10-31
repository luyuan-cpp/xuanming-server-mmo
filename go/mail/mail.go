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

	"github.com/redis/go-redis/v9"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"mail/internal/data"
	"mail/internal/maintenance"
	"mail/internal/metrics"
	"mail/internal/notify"
	"mail/internal/server"
	assetpb "proto/common/asset"
	base "proto/common/base"
	mailpb "proto/mail"
	"schemamigrate"
	"shared/assetop"
	"shared/authoritysql"
	"shared/buildinfo"
	"shared/domainhost"
	tables "shared/generated/table"
	"shared/kafkacmd"
	"shared/scenenode"
	"shared/serviceauth"
)

type config struct {
	domainhost.Config
	Mail                   data.Limits        `json:"Mail"`
	Sweep                  maintenance.Config `json:"Sweep"`
	SharedRedisAddr        string             `json:"SharedRedisAddr"`
	SharedRedisPasswordEnv string             `json:"SharedRedisPasswordEnv,optional"`
	AssetSecretEnv         string             `json:"AssetSecretEnv"`
	KafkaBrokers           []string           `json:"KafkaBrokers"`
	AccountRoleSchema      string             `json:"AccountRoleSchema"`
	GlobalLoginSessions    bool               `json:"GlobalLoginSessions"`
	LoginRedis             struct {
		Host        string `json:"Host"`
		DB          int    `json:"DB,default=0"`
		PasswordEnv string `json:"PasswordEnv,optional"`
	} `json:"LoginRedis"`
}

var errManualSchema = errors.New("领域结构需人工处理")

var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式 typed 排空 runtime 原件")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有排空原件目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime 原文件 SHA256")
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "同一 boot 接收排空请求的私有目录")

func main() {
	file := flag.String("f", "etc/mail.yaml", "配置文件")
	migrate := flag.Bool("migrate", false, "仅按proto迁移领域库")
	version := flag.Bool("version", false, "打印版本")
	flag.Parse()
	fmt.Println(buildinfo.StartupLine("mail"))
	if *version {
		return
	}
	var c config
	conf.MustLoad(*file, &c)
	if err := run(c, *migrate); err != nil {
		fmt.Fprintln(os.Stderr, "[mail]", err)
		code := schemamigrate.ExitCode(schemamigrate.Report{}, err)
		if errors.Is(err, errManualSchema) {
			code = schemamigrate.ExitManual
		}
		os.Exit(code)
	}
}
func run(c config, migrate bool) (runErr error) {
	c.Mail = c.Mail.Defaults()
	c.Sweep = c.Sweep.Defaults()
	if err := c.Mail.Validate(); err != nil {
		return err
	}
	if err := c.Sweep.Validate(); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
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
	var err error
	if migrate {
		permission, err = authoritysql.LoadSchemaPermission(os.Getenv)
	} else {
		runtime, root, relay, err = serverbase.LoadPublicationRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
		if err != nil {
			return err
		}
		sqlWriter, err = authoritysql.LoadWriterConfig(os.Getenv)
		permission = authoritysql.Writer(sqlWriter)
	}
	if err != nil {
		return err
	}
	resourcesTransferred := false
	db, err := authoritysql.Open(ctx, os.Getenv(c.DSNEnv), data.DatabaseName, authoritysql.Backend(c.Backend), c.MaxOpenConns, permission)
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
		return errors.New("邮箱结构未部署，请先运行mail -migrate")
	}
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "索引") {
			return errors.New("邮箱必要索引缺失")
		}
		logx.Info(warning)
	}
	if migrate {
		return nil
	}
	if !c.GlobalLoginSessions || c.LoginRedis.Host == "" || c.LoginRedis.DB < 0 {
		return errors.New("邮件必须显式装配同SQL事务域角色权威与全局登录态Redis")
	}
	// 同一物理连接跨库只读；不同集群或未授SELECT权限会启动失败，不降级成缓存资格。
	rows, err := db.QueryContext(ctx, "SELECT player_id,guild_id FROM mmorpg_guild.guild_member LIMIT 0")
	if err != nil {
		return errors.New("邮箱必须能在同SQL集群只读权威guild_member")
	}
	_ = rows.Close()
	secrets, err := c.Secrets()
	if err != nil {
		return err
	}
	auth, err := serviceauth.New(secrets)
	if err != nil {
		return err
	}
	signer, err := assetop.NewSigner("mail", os.Getenv(c.AssetSecretEnv))
	if err != nil {
		return err
	}
	if c.SharedRedisAddr == "" {
		return errors.New("邮箱共享Redis地址未配置")
	}
	if err = tables.ItemTableManagerInstance.Load(c.TableDir, false); err != nil {
		return err
	}
	ids, err := domainhost.OpenIDs(ctx, c.DataServiceRpc, "mail", "mail_asset_op")
	if err != nil {
		return err
	}
	defer func() {
		if !resourcesTransferred {
			ids.Close()
		}
	}()
	repo := data.New(db, ids.IDs["mail"].Next, ids.IDs["mail_asset_op"].Next, func(id uint32) bool { _, ok := tables.ItemTableManagerInstance.FindById(id); return ok }, "mmorpg_guild")
	roleCheck, cancelRoleCheck := context.WithTimeout(ctx, 3*time.Second)
	err = repo.ConfigureRoleAuthority(roleCheck, c.AccountRoleSchema)
	cancelRoleCheck()
	if err != nil {
		return errors.New("邮件同集群角色目录不可用")
	}
	if err := repo.ConfigureLimits(c.Mail); err != nil {
		return err
	}
	metrics.Register()
	sharedRedis := redis.NewClient(&redis.Options{Addr: c.SharedRedisAddr, Password: os.Getenv(c.SharedRedisPasswordEnv), PoolSize: 8, MaxActiveConns: 8, MaxRetries: -1, DialerRetries: 1, DialTimeout: time.Second, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond, PoolTimeout: 300 * time.Millisecond, ContextTimeoutEnabled: true})
	defer func() {
		if !resourcesTransferred {
			runErr = errors.Join(runErr, sharedRedis.Close())
		}
	}()
	check, cancelCheck := context.WithTimeout(ctx, time.Second)
	err = sharedRedis.Ping(check).Err()
	cancelCheck()
	if err != nil {
		return errors.New("邮箱玩家位置Redis不可达")
	}
	if len(c.KafkaBrokers) == 0 {
		return errors.New("邮件提醒KafkaBrokers未配置")
	}
	writer := &kafkago.Writer{Addr: kafkago.TCP(c.KafkaBrokers...), Balancer: &kafkacmd.CommandPartitionBalancer{Fallback: &kafkago.Hash{}}, RequiredAcks: kafkago.RequireOne, Async: false, BatchTimeout: 5 * time.Millisecond, WriteTimeout: 400 * time.Millisecond, MaxAttempts: 1}
	writer.Completion = func(_ []kafkago.Message, err error) {
		if err != nil {
			relay.Hold()
		}
	}
	defer func() {
		if !resourcesTransferred {
			runErr = errors.Join(runErr, writer.Close())
		}
	}()
	loginRedis := redis.NewClient(&redis.Options{Addr: c.LoginRedis.Host, DB: c.LoginRedis.DB,
		Password: os.Getenv(c.LoginRedis.PasswordEnv), PoolSize: 8, MaxActiveConns: 8, MaxRetries: -1,
		DialerRetries: 1, DialTimeout: time.Second, ReadTimeout: 300 * time.Millisecond,
		WriteTimeout: 300 * time.Millisecond, PoolTimeout: 300 * time.Millisecond, ContextTimeoutEnabled: true})
	defer func() {
		if !resourcesTransferred {
			runErr = errors.Join(runErr, loginRedis.Close())
		}
	}()
	loginCheck, cancelLoginCheck := context.WithTimeout(ctx, time.Second)
	err = loginRedis.Ping(loginCheck).Err()
	cancelLoginCheck()
	if err != nil {
		return errors.New("邮件全局登录态Redis不可用")
	}
	handler := server.New(repo, auth, &notify.Notifier{Redis: sharedRedis, Publisher: &relayPublisher{gate: relay, writer: writer}}).WithLoginSessions(loginRedis)
	etcd, err := clientv3.New(clientv3.Config{Endpoints: c.Etcd.Hosts, DialTimeout: 3 * time.Second})
	if err != nil {
		return err
	}
	defer func() {
		if !resourcesTransferred {
			runErr = errors.Join(runErr, etcd.Close())
		}
	}()
	conns := scenenode.NewConnCache()
	defer func() {
		if !resourcesTransferred {
			conns.Close()
		}
	}()
	watcher := scenenode.NewWatcher("mail_scene", scenenode.SceneNodeRpcPrefix, nil, func(entry scenenode.NodeEntry) { conns.Remove(entry.Endpoint) })
	locator := &scenenode.Locator{Reader: scenenode.GoRedisReader{Client: sharedRedis}, Watcher: watcher, Conns: conns}
	loopConfig := assetop.DefaultLoopConfig()
	loopConfig.Streams = []assetpb.AssetOpStream{assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT}
	loopConfig.AbandonAfter = 0
	loopConfig.AuditInterval = 0
	loopConfig.OfflineAfterAttempts = 0
	// 邮件发奖的终结只认 SQL 提交证据(data.Repo.Finalize 对 SQLCommitted 强制),与 assetop 的开关缺省值无关:
	// 固定打开,只部署到已切 go/db dual、已上新版 C++ 的 zone(docs/design/db-write-fencing-mysql.md §6.6 / §8.7)。
	loopConfig.RequireSQLCommit = true
	loop, err := assetop.NewLoop(loopConfig, data.StoreAdapter{Repo: repo}, &assetop.Caller{Resolver: locator, Signer: signer}, nil, nil)
	if err != nil {
		return err
	}
	loop.Relay = relay
	loop.Ledger = &assetop.DataServiceLedger{Client: ids.Data, Timeout: 300 * time.Millisecond}
	if err := loop.CheckWiring(); err != nil {
		return err
	}
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	sweepCtx, cancelSweep := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	loopDone := make(chan struct{})
	sweepDone := make(chan struct{})
	go func() { defer close(watchDone); watcher.Run(workerCtx, etcd) }()
	go func() { defer close(loopDone); loop.Run(workerCtx) }()
	go func() { defer close(sweepDone); maintenance.Run(sweepCtx, repo, c.Sweep, root) }()
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
		cancelSweep()
		select {
		case <-sweepDone:
		case <-closeCtx.Done():
			root.Hold()
			return errors.Join(drainproof.ErrHold, closeCtx.Err())
		}
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
		cancelWorkers()
		for _, done := range []<-chan struct{}{watchDone, loopDone} {
			select {
			case <-done:
			case <-closeCtx.Done():
				relay.Hold()
				return errors.Join(drainproof.ErrHold, closeCtx.Err())
			}
		}
		// 持久邮件/资产待办留给 P9；这里证明所有本机后继及号段预取已经结束。
		for _, tag := range []string{"mail", "mail_asset_op"} {
			digest, err := ids.IDs[tag].DrainAndReport(closeCtx)
			if err != nil {
				relay.Hold()
				return err
			}
			if err = relay.MergeSettled(digest); err != nil {
				return err
			}
		}
		if err := writer.Close(); err != nil {
			relay.Hold()
			return err
		}
		if err := relay.WaitIdle(closeCtx); err != nil {
			return err
		}
		relay.CloseAdmission()
		if err := relay.Wait(closeCtx); err != nil {
			return err
		}
		sqlWriter.Seal()
		conns.Close()
		relayErr = errors.Join(ids.RPC.Conn().Close(), loginRedis.Close(), sharedRedis.Close(), etcd.Close(), db.Close())
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
			logx.Errorf("mail 排空 HOLD: %v", err)
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
	return domainhost.ServeWithReady(ctx, c.Config, base.ENodeType_MailNodeService, server.ClientMethods(), func(s *grpc.Server) {
		mailpb.RegisterClientPlayerMailServer(s, handler)
		mailpb.RegisterMailAdminServer(s, handler)
	}, func() { close(listening) }, root, relay, relayIngress)
}

// relayPublisher 覆盖通知实际发送；调用方即使只记录错误，未知发布仍留在同 boot 的 RELAY。
type relayPublisher struct {
	gate   *drainproof.Session
	writer *kafkago.Writer
}

func (p *relayPublisher) WriteMessages(ctx context.Context, messages ...kafkago.Message) (resultErr error) {
	finish, err := p.gate.Enter()
	if err != nil {
		return err
	}
	defer finish()
	defer func() {
		if recovered := recover(); recovered != nil {
			p.gate.Hold()
			panic(recovered)
		}
		if resultErr != nil {
			p.gate.Hold()
		}
	}()
	return p.writer.WriteMessages(ctx, messages...)
}
