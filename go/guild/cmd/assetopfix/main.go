// Command assetopfix 是帮会资产指令(mmorpg_guild.guild_asset_op)卡死行的人工终结工具
// (docs/design/guild-phase2/05-economy.md §5.23;90-consistency.md D4:v1 只做 CLI,不开 RPC)。
//
// 为什么需要它:scene 对某个 seq 回 UNKNOWN(窗口已过 / 信封非法)时,重投循环走 Alert、按
// MaxBackoffMs 反复重排、永不终结;同一玩家同一条流累计 16 行未决后,新的捐献 / 兑换全被
// kGuildAssetPending 挡住。UNKNOWN 的 seq,scene 以后也不会再应用,唯一的疑问是"以前有没有
// 应用过" —— 只能由人查流水回答;查清之后人工终结是安全的。
//
// 边界:
//   - 只连 MySQL(DSN 经 data.WithLockWaitTimeout,与服务进程同一套锁等待上限)与缓存 Redis
//     (终结会改帮会资金 / 成员帮贡,必须失效缓存);不起 RPC、不 LoadTables、不连 Kafka /
//     data_service / etcd,因此不推送(GuildAssetStore.OnFinalized 保持 nil)。
//   - 人工终结**不置 durable**(那不是 scene 的确认),只开放 applied / aborted 两种结局(裁决 E);
//     对侧账与自动终结共用 GuildAssetStore 里的同一个函数。
//   - 走包级 assetop.ResolveManually,不造 Loop(那需要假 Applier 和假循环参数,造出来就会被人拿去真跑)。
//
// 退出码:0 成功;1 执行出错;2 参数不合法或前置条件不满足(含缺 -txlog-checked);
// 3 行已不在 PENDING(被自动终结或别人抢先),本次未做任何改动。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"

	"guild/internal/config"
	"guild/internal/data"
	assetpb "proto/common/asset"
	databasepb "proto/common/database"
	pb "proto/guild"
	"shared/assetop"
	"shared/authoritysql"
	"shared/drainproof"
)

const (
	exitOK         = 0
	exitError      = 1
	exitRejected   = 2
	exitNotPending = 3
)

const (
	// resolveMinAge:只终结创建早于 30 分钟前的行(§5.23 第 1 条)。更新的行循环还在正常重试,
	// 人工插手只会和它抢同一行。
	resolveMinAge = 30 * time.Minute

	// resolveMinAttempts:只处理"反复 UNKNOWN"的行(§5.23 第 1 条)。一两次 UNKNOWN 可能是
	// 瞬时的信封 / 纪元问题;其它原因(玩家离线、背包满、战斗中)循环自己会收口,不该由人判结局。
	// -force 只跳过这一条。
	//
	// **口径(落地恢复,docs/design/landing-recovery-sync-digest.md §6.2)**:传输失败 / 超时(没拿到 scene 任何答复)
	// 现在按 RETRY、原因码 0 落库(go/shared/assetop/reconcile.go 的 rescheduleTransportFailure),last_outcome = 0
	// 只剩"scene 回了 UNKNOWN / 坏流号 / 投过之后成了毒行"这类要人看的行。
	// **残余盲区**:升级前由旧版写下的行,传输失败仍记 0,直到下一次投递把它改写 —— 只有这类旧行的 last_outcome = 0
	// 才可能是传输失败(scene 可能已经应用过、只是回包丢了,恢复后循环会自己终结,恰恰不该由人判)。列上分不出新旧,
	// 所以 printChecklist 第 0 步仍要求先确认 scene 可达、且 Loki 里循环仍在报 UNKNOWN 告警。
	// 本工具的判定行为不随之改变:仍只认 last_outcome = 0 且 attempts >= 10。
	resolveMinAttempts = 10

	listDefaultMinAgeMin = 60
	listDefaultLimit     = 50
	listMaxLimit         = 500

	// opTimeout 是一次 list / resolve 的总预算;Store 内部每条 SQL 另有自己的子预算
	// (读 1000ms、人工终结事务 2000ms),这里只防止整条命令挂住。
	opTimeout   = 15 * time.Second
	pingTimeout = 5 * time.Second

	// logServiceName 让审计日志与服务进程的日志在检索时分得开。
	logServiceName = "guild-assetopfix"
)

const usageText = `assetopfix:帮会资产指令(guild_asset_op)卡死行的人工终结工具(05-economy.md §5.23)

用法:
  assetopfix -f etc/guild.yaml list [-min-age-min 60] [-limit 50]
  assetopfix -f etc/guild.yaml resolve -op <op_id> -as applied|aborted -operator <名字> -reason <依据> \
      -confirm <op_id> -txlog-checked [-force]

resolve 的前置条件(任一不满足即拒绝,不改任何数据):
  行存在且为 PENDING;创建早于 30 分钟前;
  last_outcome = UNKNOWN(0) 且 attempts >= 10(-force 只跳过这一条);
  -confirm 与 -op 相同(二次确认);
  带 -txlog-checked(不带则打印核对清单后退出)。

注意:传输失败 / 超时按 RETRY 落库,last_outcome = 0 表示 scene 回了 UNKNOWN(或坏流号 / 毒行);
只有升级前旧版写下的行,last_outcome = 0 才可能是传输失败(scene 可能已应用、只是回包丢了)。list 里的 yes
只说明列上的前置满足,动手前仍须按核对清单第 0 步确认 scene 可达、循环仍在报 UNKNOWN 告警。

退出码:0 成功;1 执行出错;2 参数 / 前置条件不满足;3 行已不在 PENDING,未做改动。
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是可测的入口:不调 os.Exit,返回退出码。
func run(args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("assetopfix", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usageText) }
	configFile := global.String("f", "etc/guild.yaml", "guild 服务的配置文件(与服务进程同一份)")
	drainPlan := global.String("maintenance-drain-plan", "", "本boot正式排空runtime原件")
	drainDir := global.String("maintenance-drain-dir", "", "本boot固定私有排空目录")
	drainSHA := global.String("maintenance-drain-sha256", "", "runtime原件固定SHA256")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitRejected
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return exitRejected
	}

	// 子命令参数先于连库解析:参数写错不该先去碰数据库。
	switch rest[0] {
	case "list":
		opts, ok := parseList(rest[1:], stderr)
		if !ok {
			return exitRejected
		}
		return withStore(*configFile, true, *drainPlan, *drainDir, *drainSHA, stderr, func(ctx context.Context, store *data.GuildAssetStore) int {
			return runList(ctx, store, opts, stdout, stderr)
		})
	case "resolve":
		opts, ok := parseResolve(rest[1:], stderr)
		if !ok {
			return exitRejected
		}
		return withStore(*configFile, false, *drainPlan, *drainDir, *drainSHA, stderr, func(ctx context.Context, store *data.GuildAssetStore) int {
			return runResolve(ctx, store, opts, stdout, stderr)
		})
	default:
		fmt.Fprintf(stderr, "Unknown subcommand %q\n\n", rest[0])
		global.Usage()
		return exitRejected
	}
}

// withStore 加载配置、按配置初始化日志、连 MySQL 与缓存 Redis,把 Store 交给 fn,返回 fn 的退出码。
func withStore(configPath string, readOnly bool, plan, directory, digest string, stderr io.Writer, fn func(ctx context.Context, store *data.GuildAssetStore) int) (result int) {
	var c config.Config
	// conf.Load 会跑 config.Validate(与服务进程同一套校验);其错误文案不含 DSN 原文。
	if err := conf.Load(configPath, &c); err != nil {
		fmt.Fprintf(stderr, "Failed to load configuration %s: %v\n", configPath, err)
		return exitError
	}
	// 按服务 yaml 的 Log 段初始化:部署环境若把日志落文件供采集,审计行也跟着进同一条管道。
	logConf := c.Log
	logConf.ServiceName = logServiceName
	if err := logx.SetUp(logConf); err != nil {
		fmt.Fprintf(stderr, "Failed to initialize logging: %v\n", err)
		return exitError
	}
	defer func() { _ = logx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	permission := authoritysql.Reader()
	var writer authoritysql.WriterConfig
	if !readOnly {
		var err error
		writer, err = authoritysql.LoadWriterConfig(os.Getenv)
		if err != nil {
			fmt.Fprintln(stderr, "NORMAL工具启动身份无效:", err)
			return exitError
		}
		permission = authoritysql.Writer(writer)
		defer writer.Seal()
	}
	var runtime *drainproof.Runtime
	source, relay := drainproof.New(), drainproof.New()
	if plan != "" || directory != "" || digest != "" {
		if readOnly {
			fmt.Fprintln(stderr, "只读list不参加写者闭合发行")
			return exitRejected
		}
		var err error
		runtime, err = drainproof.LoadRuntime(plan, directory, digest)
		if err != nil || runtime.Request.RunPhase >= databasepb.SqlWriterDrainRun_SQL_FROZEN || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
			fmt.Fprintln(stderr, "有限NORMAL工具缺同boot SOURCE/RELAY原件")
			return exitError
		}
		if err = runtime.BindBoot(writer.InstanceID(), writer.WriterKey()); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
	}
	accepted, err := source.Enter()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	source.CloseAdmission()
	store, closeDeps, err := openStore(ctx, c, permission, relay)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitError
	}
	completed := false
	defer func() {
		if !completed || result == exitError {
			source.Hold()
		}
		accepted()
		// 命令返回后仍等真实缓存重试结束，不能拿即将退出进程代替闭合报告。
		closeCtx, cancelClose := context.WithTimeout(context.Background(), opTimeout)
		defer cancelClose()
		if err := closeDeps(closeCtx); err != nil {
			source.Hold()
			fmt.Fprintln(stderr, "工具资源闭合失败，保持HOLD:", err)
			result = exitError
			return
		}
		if runtime != nil {
			if err := runtime.Write("SOURCE", source); err != nil {
				fmt.Fprintln(stderr, err)
				result = exitError
				return
			}
			if err := runtime.Write("RELAY", relay); err != nil {
				fmt.Fprintln(stderr, err)
				result = exitError
			}
		}
	}()
	result = fn(ctx, store)
	completed = true
	return result
}

// openStore 只建 MySQL + 缓存 Redis → GuildRepo → GuildAssetStore。
//
// Redis 连不上就拒绝执行(list 也一样,只为少一条分支):终结会改帮会资金与成员帮贡,
// 缓存失效失败时服务会继续按旧缓存显示,直到 Cache.DefaultTTL 过期。
// 后台缓存重试由同boot角色持有，真实返回后才关闭连接池。
func openStore(ctx context.Context, c config.Config, permission authoritysql.Permission, relay *drainproof.Session) (*data.GuildAssetStore, func(context.Context) error, error) {
	dsn, err := data.WithLockWaitTimeout(c.MySQL.DataSource)
	if err != nil {
		return nil, nil, err // 该错误本身不含 DSN 原文(DSN 带口令)
	}
	db, err := authoritysql.Open(ctx, dsn, data.DatabaseName, c.MySQL.RequiresTiDB(), 2, permission)
	if err != nil {
		return nil, nil, errors.New("打开 MySQL 失败(DSN 含口令,不打印原错误)")
	}
	pingCtx, cancelPing := context.WithTimeout(ctx, pingTimeout)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("连接 MySQL(库 %s)失败: %w", data.DatabaseName, err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     c.RedisClient.Host,
		Password: c.RedisClient.Password,
		DB:       c.RedisClient.DB,
	})
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		_ = db.Close()
		return nil, nil, fmt.Errorf("连接缓存 Redis %s (DB %d) 失败,拒绝执行(终结后无法失效帮会 / 成员缓存): %w",
			c.RedisClient.Host, c.RedisClient.DB, err)
	}

	repo := data.NewGuildRepo(rdb, db, c.Cache.DefaultTTL)
	if err := repo.BindPublication(relay); err != nil {
		_ = rdb.Close()
		_ = db.Close()
		return nil, nil, err
	}
	store, err := data.NewGuildAssetStore(repo)
	if err != nil {
		_ = rdb.Close()
		_ = db.Close()
		return nil, nil, fmt.Errorf("构造 GuildAssetStore 失败: %w", err)
	}
	return store, func(closeCtx context.Context) error {
		frontier, publicationErr := repo.ClosePublication(closeCtx)
		if err := relay.WaitIdle(closeCtx); err != nil {
			return errors.Join(publicationErr, err)
		}
		if publicationErr != nil {
			relay.Hold()
		} else if err := relay.MergeSettled(frontier); err != nil {
			publicationErr = err
			relay.Hold()
		}
		redisErr, sqlErr := rdb.Close(), db.Close()
		if redisErr != nil || sqlErr != nil {
			relay.Hold()
		}
		relay.CloseAdmission()
		return errors.Join(publicationErr, redisErr, sqlErr, relay.Wait(closeCtx))
	}, nil
}

// ── list ────────────────────────────────────────────────────────

type listOptions struct {
	minAgeMin int
	limit     int
}

func parseList(args []string, stderr io.Writer) (listOptions, bool) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	minAge := fs.Int("min-age-min", listDefaultMinAgeMin, "只列创建早于这么多分钟之前的 PENDING 行")
	limit := fs.Int("limit", listDefaultLimit, fmt.Sprintf("最多列多少行(1..%d)", listMaxLimit))
	if err := fs.Parse(args); err != nil {
		return listOptions{}, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "list does not accept positional arguments: %v\n", fs.Args())
		return listOptions{}, false
	}
	if *minAge < 0 {
		fmt.Fprintf(stderr, "-min-age-min must not be negative (got %d)\n", *minAge)
		return listOptions{}, false
	}
	if *limit < 1 || *limit > listMaxLimit {
		fmt.Fprintf(stderr, "-limit must be within [1, %d] (got %d)\n", listMaxLimit, *limit)
		return listOptions{}, false
	}
	return listOptions{minAgeMin: *minAge, limit: *limit}, true
}

func runList(ctx context.Context, store *data.GuildAssetStore, o listOptions, stdout, stderr io.Writer) int {
	now := time.Now()
	nowMs := uint64(now.UnixMilli())
	createdBeforeMs := uint64(now.Add(-time.Duration(o.minAgeMin) * time.Minute).UnixMilli())
	rows, err := store.ListStuck(ctx, createdBeforeMs, o.limit)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to list unresolved rows: %v\n", err)
		return exitError
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "No PENDING rows created more than %d minutes ago\n", o.minAgeMin)
		return exitOK
	}

	// 表头与取值只用 ASCII:tabwriter 按字符数对齐,中文双宽字符会把列挤歪。
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "op_id\tplayer_id\tguild_id\tstream\tepoch\tseq\tkind\tattempts\tlast_outcome\tlast_reason\tage_min\tresolvable")
	for _, r := range rows {
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%s\t%d\t%d\t%d\t%d\t%s\n",
			r.OpID, r.PlayerID, r.GuildID, r.Stream, r.StreamEpoch, r.Seq, r.Kind,
			r.Attempts, r.LastOutcome, r.LastReason, ageMinutes(r.CreatedMs, nowMs), resolvableLabel(r, nowMs))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "Output failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "\n%d rows total. resolvable: yes = all row-level resolve preconditions met; force = only missing "+
		"the repeated UNKNOWN condition, add -force when manual intervention is required; no = cannot finalize manually.\n", len(rows))
	// 传输失败按 RETRY 落库;只有升级前旧版写下的行,last_outcome=0 才可能是传输失败,列上分不出新旧(见 resolveMinAttempts 注释)。
	fmt.Fprintln(stdout, "Note: transport failures / timeouts are stored as RETRY, so last_outcome=0 means scene returned UNKNOWN (or bad stream / poison row); "+
		"only rows last written before the upgrade may still carry 0 for a transport failure (scene may have applied the command but response was lost; loop will finalize after scene recovers). "+
		"yes does not mean manual finalization is appropriate: first confirm the scene node is reachable and Loki still reports \"Scene returned UNKNOWN; not finalizing ... op_id=<id>\".")
	return exitOK
}

func resolvableLabel(r data.AssetOpRow, nowMs uint64) string {
	switch {
	case checkResolvable(r, nowMs, false) == nil:
		return "yes"
	case checkResolvable(r, nowMs, true) == nil:
		return "force"
	default:
		return "no"
	}
}

// ── resolve ─────────────────────────────────────────────────────

type resolveOptions struct {
	opID         uint64
	as           string
	final        assetop.Status
	operator     string
	reason       string
	txlogChecked bool
	force        bool
}

func parseResolve(args []string, stderr io.Writer) (resolveOptions, bool) {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opID := fs.Uint64("op", 0, "要终结的 op_id")
	as := fs.String("as", "", "人工判定的结局:applied(scene 已应用)或 aborted(确认从未应用)")
	operator := fs.String("operator", "", "操作人(写进 resolved_by,≤64 字)")
	reason := fs.String("reason", "", "判定依据(写进 resolve_reason,≤191 字),例如工单号 + 流水核对结论")
	confirm := fs.Uint64("confirm", 0, "二次确认:必须与 -op 相同")
	txlogChecked := fs.Bool("txlog-checked", false, "已按核对清单查过 Loki 与交易流水")
	force := fs.Bool("force", false, "跳过\"last_outcome=UNKNOWN 且 attempts>=10\"这一条前置(其余前置照查)")
	if err := fs.Parse(args); err != nil {
		return resolveOptions{}, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "resolve does not accept positional arguments: %v\n", fs.Args())
		return resolveOptions{}, false
	}

	o := resolveOptions{
		opID:         *opID,
		as:           *as,
		operator:     *operator,
		reason:       *reason,
		txlogChecked: *txlogChecked,
		force:        *force,
	}
	if o.opID == 0 {
		fmt.Fprintln(stderr, "Missing -op")
		return resolveOptions{}, false
	}
	final, ok := finalStatusOf(o.as)
	if !ok {
		fmt.Fprintf(stderr, "-as must be applied or aborted (got %q)\n", o.as)
		return resolveOptions{}, false
	}
	o.final = final
	if strings.TrimSpace(o.operator) == "" {
		fmt.Fprintln(stderr, "Missing -operator: manual finalization must record the operator")
		return resolveOptions{}, false
	}
	// 操作人按 %s 进审计行:带换行 / 控制字符就能伪造出第二条 [AssetOpManual],所以直接拒绝。
	// 长度上限(64 / 191)由 assetop.ResolveManually 统一校验,这里不再抄一份。
	if hasControl(o.operator) {
		fmt.Fprintln(stderr, "-operator must not contain newlines or control characters")
		return resolveOptions{}, false
	}
	if strings.TrimSpace(o.reason) == "" {
		fmt.Fprintln(stderr, "Missing -reason: manual finalization must state its evidence")
		return resolveOptions{}, false
	}
	if *confirm != o.opID {
		fmt.Fprintf(stderr, "Confirmation failed: -confirm(%d) must equal -op(%d)\n", *confirm, o.opID)
		return resolveOptions{}, false
	}
	return o, true
}

// finalStatusOf 只开放两种人工结局(裁决 E):REJECTED 是 scene 的判定、APPLIED_PARTIAL 需要逐件核对,
// 都不是"查流水得出有 / 没有"能给出的结论。
func finalStatusOf(as string) (assetop.Status, bool) {
	switch as {
	case "applied":
		return assetop.StatusApplied, true
	case "aborted":
		return assetop.StatusAborted, true
	default:
		return 0, false
	}
}

func runResolve(ctx context.Context, store *data.GuildAssetStore, o resolveOptions, stdout, stderr io.Writer) int {
	row, found, err := store.GetOp(ctx, o.opID)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to read op_id=%d: %v\n", o.opID, err)
		return exitError
	}
	if !found {
		fmt.Fprintf(stderr, "op_id=%d does not exist\n", o.opID)
		return exitRejected
	}
	if err := checkResolvable(row, uint64(time.Now().UnixMilli()), o.force); err != nil {
		fmt.Fprintf(stderr, "Refusing to finalize op_id=%d: %v\n", o.opID, err)
		return exitRejected
	}
	if !o.txlogChecked {
		printChecklist(stderr, row)
		return exitRejected
	}

	// 前置检查与下面的 CAS 之间,循环可能已经把这一行终结了:CAS(WHERE status=PENDING)会落空、
	// 返回 false,不会重复入账。前置检查只为挡住误操作,不是并发控制。
	finalized, err := assetop.ResolveManually(ctx, store, assetop.ManualResolution{
		OpID:     o.opID,
		Final:    o.final,
		Operator: o.operator,
		Reason:   o.reason,
	}, nil, time.Now)

	// 审计行无论成败都打(ERROR 级确保进日志检索),并原样打到 stdout 供贴进运维工单。
	// 在 §5.23 的格式末尾追加 force:跳过了哪条前置,事后追责必须看得到。
	audit := fmt.Sprintf("[AssetOpManual] operator=%s op_id=%d player_id=%d stream=%d seq=%d kind=%d as=%s reason=%q finalized=%v force=%v",
		o.operator, row.OpID, row.PlayerID, row.Stream, row.Seq, row.Kind, o.as, o.reason, finalized, o.force)
	logx.Error(audit)
	fmt.Fprintln(stdout, audit)

	if err != nil {
		logx.Errorf("[AssetOpManual] op_id=%d manual finalization failed: %v", o.opID, err)
		fmt.Fprintf(stderr, "Manual finalization failed op_id=%d: %v\n", o.opID, err)
		return exitError
	}
	if !finalized {
		fmt.Fprintf(stdout, "op_id=%d already automatically finalized / no longer PENDING; no changes made (do not adjust the ledger manually)\n", o.opID)
		return exitNotPending
	}
	fmt.Fprintf(stdout, "op_id=%d manually finalized as %s. Attach the [AssetOpManual] audit line above to the operations ticket.\n", o.opID, o.as)
	return exitOK
}

// checkResolvable 是 resolve 的前置条件(§5.23 第 1 条)。force 只跳过"反复 UNKNOWN"这一条。
func checkResolvable(r data.AssetOpRow, nowMs uint64, force bool) error {
	if r.Status != pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING {
		return fmt.Errorf("status=%s,不是 PENDING:已终结的行不能再人工改", r.Status)
	}
	if r.CreatedMs+uint64(resolveMinAge/time.Millisecond) > nowMs {
		return fmt.Errorf("创建于 %d 分钟前,不足 %v:新行循环还在正常重试,人工插手只会和它抢",
			ageMinutes(r.CreatedMs, nowMs), resolveMinAge)
	}
	if force {
		return nil
	}
	unknown := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN)
	if r.LastOutcome != unknown || r.Attempts < resolveMinAttempts {
		return fmt.Errorf("last_outcome=%d attempts=%d:只处理反复 UNKNOWN(last_outcome=%d 且 attempts >= %d)的行;"+
			"其它原因(玩家离线、背包满、战斗中)循环会自己收口,确需人工介入时加 -force",
			r.LastOutcome, r.Attempts, unknown, resolveMinAttempts)
	}
	return nil
}

// printChecklist 打印核对清单。它只读这一行已有的列,不查任何别的系统:结论必须由人给出。
func printChecklist(w io.Writer, r data.AssetOpRow) {
	fmt.Fprintln(w, "-txlog-checked missing; no changes made. Complete these checks first:")
	fmt.Fprintf(w, "  Row: op_id=%d player_id=%d guild_id=%d stream=%d(%s) epoch=%d seq=%d kind=%s attempts=%d last_outcome=%d last_reason=%d\n",
		r.OpID, r.PlayerID, r.GuildID, r.Stream, r.Stream, r.StreamEpoch, r.Seq, r.Kind, r.Attempts, r.LastOutcome, r.LastReason)
	// 第 0 步必须排在查流水之前:传输失败已按 RETRY 落库,但升级前旧版写下的行 last_outcome=0 仍可能是传输失败 / 超时,
	// 列上分不出新旧(见 resolveMinAttempts 注释)。
	fmt.Fprintf(w, "  0. Confirm scene actually returned UNKNOWN: the player's scene node is reachable and recent loop logs in Loki still report "+
		"\"[AssetOp] Scene returned UNKNOWN; not finalizing op_id=%d\". Transport failures / timeouts are stored as RETRY, but a row last written "+
		"before the upgrade may still carry last_outcome=0 for one -- scene may have applied such rows but response was lost; "+
		"the loop will finalize after scene recovery, do not finalize manually\n", r.OpID)
	fmt.Fprintf(w, "  1. Check guild [AssetOp] logs in Loki: op_id=%d / corr=%d (included in every delivery, reschedule, and warning)\n", r.OpID, r.OpID)
	fmt.Fprintf(w, "  2. Check transaction ledger for correlation_id=%d: record present = scene applied this command\n", r.OpID)
	fmt.Fprintln(w, "  3. Conclusion: applied record found -> -as applied; confirmed never applied -> -as aborted")
	switch r.Kind {
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE:
		fmt.Fprintf(w, "  This row is a donation: applied = guild funds +%d, donor contribution +%d (only orphan count if guild dissolved); aborted = refund today's donation count\n",
			r.FundsDelta, r.ContributionDelta)
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP:
		fmt.Fprintf(w, "  This row is a shop exchange: applied = contribution unchanged (deducted on order); aborted = refund contribution %d and purchase-limit count\n",
			r.ContributionDelta)
	default:
		fmt.Fprintf(w, "  This row kind=%s: state only, no counterpart ledger update\n", r.Kind)
	}
	fmt.Fprintln(w, "After checking, rerun the same command with -txlog-checked.")
}

func ageMinutes(createdMs, nowMs uint64) uint64 {
	if nowMs <= createdMs {
		return 0
	}
	return (nowMs - createdMs) / uint64(time.Minute/time.Millisecond)
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
