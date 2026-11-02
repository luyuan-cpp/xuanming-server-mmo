// Command assetopfix 是交易资产指令(mmorpg_trade.trade_asset_op)卡死行的人工终结工具,
// 口径与 go/guild/cmd/assetopfix 相同(docs/design/guild-phase2/04-asset-channel.md §4.38,
// 手册 docs/ops/trade-commerce-quarantine-runbook.md §4.2)。
//
// 为什么需要它:scene 对某个 seq 回 UNKNOWN(窗口已过 / 信封非法)时,重投循环走 Alert、反复重排、永不终结,
// 合同停在这条腿上,以前只能手改 SQL。UNKNOWN 的 seq,scene 以后也不会再应用,唯一的疑问是"以前有没有应用过" ——
// 只能由人查流水回答;查清之后人工终结是安全的。合同推进只看 status,下一轮 Tick 按终局继续:
// aborted 走与 scene 判拒同一条补偿;实例扣款判 applied 时没有托管原件,合同隔离为 Manual。
//
// 边界:
//   - 只连 MySQL(etcd 只在配了 DbPlacement 时读一次映射);不起 RPC、不连 Redis / Kafka / data_service。
//     终结只改这一行,没有缓存要失效。
//   - 人工终结**不置 durable**(那不是 scene 的确认),只开放 applied / aborted 两种结局。
//   - 走包级 assetop.ResolveManually,不造 Loop。
//   - 不参加维护排空的写者闭合,排空期间不要运行。
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

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"shared/assetop"
	"shared/authoritysql"
	"shared/dbplacement"
	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/svc"
)

const (
	exitOK         = 0
	exitError      = 1
	exitRejected   = 2
	exitNotPending = 3
)

const (
	// resolveMinAge:只终结创建早于 30 分钟前的行。更新的行循环还在正常重试,人工插手只会和它抢同一行。
	resolveMinAge = 30 * time.Minute

	// resolveMinAttempts:只处理"反复 UNKNOWN"的行。一两次 UNKNOWN 可能是瞬时的信封 / 纪元问题;
	// 其它原因(玩家离线、背包满、战斗中)循环自己会收口,不该由人判结局。-force 只跳过这一条。
	// 传输失败 / 超时按 RETRY、原因码 0 落库,不会让 last_outcome 变成 UNKNOWN。
	resolveMinAttempts = 10

	listDefaultMinAgeMin = 60
	listDefaultLimit     = 50
	listMaxLimit         = 500

	// opTimeout 是一次 list / resolve 的总预算;AssetOpRepo 每次调用另有 constants.StoreOpTimeout。
	opTimeout   = 15 * time.Second
	pingTimeout = 5 * time.Second

	logServiceName = "trade-assetopfix"
)

const usageText = `assetopfix:交易资产指令(trade_asset_op)卡死行的人工终结工具

用法:
  assetopfix -f etc/trade.yaml list [-min-age-min 60] [-limit 50]
  assetopfix -f etc/trade.yaml resolve -op <op_id> -as applied|aborted -operator <名字> -reason <依据> \
      -confirm <op_id> -txlog-checked [-force]

resolve 的前置条件(任一不满足即拒绝,不改任何数据):
  行存在且为 PENDING;创建早于 30 分钟前;
  last_outcome = UNKNOWN(0) 且 attempts >= 10(-force 只跳过这一条);
  -confirm 与 -op 相同(二次确认);
  带 -txlog-checked(不带则打印核对清单后退出)。

隔离之后仍 PENDING 的托管扣款不要终结:资产通道会用同一个 seq 改发 Abort,等它定论(手册 §4.1)。
维护排空期间不要运行。

退出码:0 成功;1 执行出错;2 参数 / 前置条件不满足;3 行已不在 PENDING,未做改动。
`

// opStore 是工具对 AssetOpRepo 的依赖面,测试用假实现替换。
type opStore interface {
	assetop.ManualResolver
	GetOp(ctx context.Context, opID uint64) (*tradepb.TradeAssetOpRecord, bool, error)
	ListStuck(ctx context.Context, createdBeforeMs uint64, limit int) ([]*tradepb.TradeAssetOpRecord, error)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是可测的入口:不调 os.Exit,返回退出码。
func run(args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("assetopfix", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usageText) }
	configFile := global.String("f", "etc/trade.yaml", "trade 服务的配置文件(与服务进程同一份)")
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
		return withStore(*configFile, true, stderr, func(ctx context.Context, store opStore) int {
			return runList(ctx, store, opts, stdout, stderr)
		})
	case "resolve":
		opts, ok := parseResolve(rest[1:], stderr)
		if !ok {
			return exitRejected
		}
		return withStore(*configFile, false, stderr, func(ctx context.Context, store opStore) int {
			return runResolve(ctx, store, opts, stdout, stderr)
		})
	default:
		fmt.Fprintf(stderr, "Unknown subcommand %q\n\n", rest[0])
		global.Usage()
		return exitRejected
	}
}

// withStore 加载配置、按配置初始化日志、连 MySQL,把 AssetOpRepo 交给 fn,返回 fn 的退出码。
func withStore(configPath string, readOnly bool, stderr io.Writer, fn func(ctx context.Context, store opStore) int) int {
	var c config.Config
	// conf.Load 会跑 config.Validate(与服务进程同一套校验);其错误文案不含口令。
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
	if !readOnly {
		writer, err := authoritysql.LoadWriterConfig(os.Getenv)
		if err != nil {
			fmt.Fprintln(stderr, "Invalid writer identity:", err)
			return exitError
		}
		defer writer.Seal()
		permission = authoritysql.Writer(writer)
	}
	placementOptions, err := dbplacement.ForOneShot(ctx, c.MySQL.DbPlacement, c.MySQL.DBName)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	db, err := authoritysql.Open(ctx, svc.BuildDSN(c.MySQL), c.MySQL.DBName, authoritysql.Backend(c.MySQL.Backend), 2, permission, placementOptions...)
	if err != nil {
		// 错误里可能带 DSN,DSN 含口令,不打印原错误。
		fmt.Fprintln(stderr, "Failed to open MySQL (DSN contains a password; the original error is not printed)")
		return exitError
	}
	defer func() { _ = db.Close() }()

	pingCtx, cancelPing := context.WithTimeout(ctx, pingTimeout)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		fmt.Fprintf(stderr, "Failed to connect to MySQL database %s: %v\n", c.MySQL.DBName, err)
		return exitError
	}
	repo, err := data.NewAssetOpRepo(db, constants.StoreOpTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to build AssetOpRepo: %v\n", err)
		return exitError
	}
	return fn(ctx, repo)
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

func runList(ctx context.Context, store opStore, o listOptions, stdout, stderr io.Writer) int {
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
	fmt.Fprintln(tw, "op_id\tplayer_id\tref_id\tstream\tepoch\tseq\tkind\tattempts\tlast_outcome\tlast_reason\tage_min\tresolvable")
	for _, r := range rows {
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%s\t%d\t%d\t%d\t%d\t%s\n",
			r.GetOpId(), r.GetPlayerId(), r.GetRefId(), r.GetStream(), r.GetStreamEpoch(), r.GetSeq(), r.GetKind(),
			r.GetAttempts(), r.GetLastOutcome(), r.GetLastReason(), ageMinutes(r.GetCreatedMs(), nowMs), resolvableLabel(r, nowMs))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "Output failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "\n%d rows total. resolvable: yes = all row-level resolve preconditions met; force = only missing "+
		"the repeated UNKNOWN condition, add -force when manual intervention is required; no = cannot finalize manually.\n", len(rows))
	return exitOK
}

func resolvableLabel(r *tradepb.TradeAssetOpRecord, nowMs uint64) string {
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

// finalStatusOf 只开放两种人工结局:REJECTED 是 scene 的判定、APPLIED_PARTIAL 需要逐件核对,
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

func runResolve(ctx context.Context, store opStore, o resolveOptions, stdout, stderr io.Writer) int {
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
	// 返回 false。前置检查只为挡住误操作,不是并发控制。
	finalized, err := assetop.ResolveManually(ctx, store, assetop.ManualResolution{
		OpID:     o.opID,
		Final:    o.final,
		Operator: o.operator,
		Reason:   o.reason,
	}, nil, time.Now)

	// 审计行无论成败都打(ERROR 级确保进日志检索),并原样打到 stdout 供贴进运维工单。
	audit := fmt.Sprintf("[AssetOpManual] operator=%s op_id=%d player_id=%d stream=%d seq=%d kind=%d ref_id=%d as=%s reason=%q finalized=%v force=%v",
		o.operator, row.GetOpId(), row.GetPlayerId(), row.GetStream(), row.GetSeq(), row.GetKind(), row.GetRefId(), o.as, o.reason, finalized, o.force)
	logx.Error(audit)
	fmt.Fprintln(stdout, audit)

	if err != nil {
		logx.Errorf("[AssetOpManual] op_id=%d manual finalization failed: %v", o.opID, err)
		fmt.Fprintf(stderr, "Manual finalization failed op_id=%d: %v\n", o.opID, err)
		return exitError
	}
	if !finalized {
		fmt.Fprintf(stdout, "op_id=%d already automatically finalized / no longer PENDING; no changes made\n", o.opID)
		return exitNotPending
	}
	fmt.Fprintf(stdout, "op_id=%d manually finalized as %s. Attach the [AssetOpManual] audit line above to the operations ticket.\n", o.opID, o.as)
	return exitOK
}

// checkResolvable 是 resolve 的前置条件。force 只跳过"反复 UNKNOWN"这一条。
func checkResolvable(r *tradepb.TradeAssetOpRecord, nowMs uint64, force bool) error {
	if r.GetStatus() != tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING {
		return fmt.Errorf("status=%s,不是 PENDING:已终结的行不能再人工改", r.GetStatus())
	}
	if r.GetCreatedMs()+uint64(resolveMinAge/time.Millisecond) > nowMs {
		return fmt.Errorf("创建于 %d 分钟前,不足 %v:新行循环还在正常重试,人工插手只会和它抢",
			ageMinutes(r.GetCreatedMs(), nowMs), resolveMinAge)
	}
	if force {
		return nil
	}
	unknown := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN)
	if r.GetLastOutcome() != unknown || r.GetAttempts() < resolveMinAttempts {
		return fmt.Errorf("last_outcome=%d attempts=%d:只处理反复 UNKNOWN(last_outcome=%d 且 attempts >= %d)的行;"+
			"其它原因循环会自己收口,确需人工介入时加 -force",
			r.GetLastOutcome(), r.GetAttempts(), unknown, resolveMinAttempts)
	}
	return nil
}

// printChecklist 打印核对清单。它只读这一行已有的列,不查任何别的系统:结论必须由人给出。
func printChecklist(w io.Writer, r *tradepb.TradeAssetOpRecord) {
	fmt.Fprintln(w, "-txlog-checked missing; no changes made. Complete these checks first:")
	fmt.Fprintf(w, "  Row: op_id=%d player_id=%d ref_kind=%s ref_id=%d stream=%d epoch=%d seq=%d kind=%s attempts=%d last_outcome=%d last_reason=%d deadline_ms=%d\n",
		r.GetOpId(), r.GetPlayerId(), r.GetRefKind(), r.GetRefId(), r.GetStream(), r.GetStreamEpoch(), r.GetSeq(), r.GetKind(),
		r.GetAttempts(), r.GetLastOutcome(), r.GetLastReason(), r.GetDeadlineMs())
	fmt.Fprintf(w, "  0. Confirm scene actually returned UNKNOWN: the player's scene node is reachable and recent loop logs in Loki still report "+
		"\"[AssetOp] Scene returned UNKNOWN; not finalizing op_id=%d\". A contract in Manual with this debit still PENDING waits for the Abort; do not finalize it\n", r.GetOpId())
	fmt.Fprintf(w, "  1. Check trade [AssetOp] logs in Loki: op_id=%d / corr=%d (included in every delivery, reschedule, and warning)\n", r.GetOpId(), r.GetOpId())
	fmt.Fprintf(w, "  2. Check transaction ledger for correlation_id=%d: record present = scene applied this command\n", r.GetOpId())
	fmt.Fprintln(w, "  3. Conclusion: applied record found -> -as applied; confirmed never applied -> -as aborted")
	if r.GetStream() == uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT) {
		fmt.Fprintln(w, "  This row is an escrow debit: applied = the asset left the player and is held by the contract (an instance debit then goes to Manual because no escrow snapshot exists); aborted = the contract compensates as if scene had rejected it")
	} else {
		fmt.Fprintln(w, "  This row is a delivery: applied = the player received the asset; aborted = the asset stays in contract escrow and the contract handles it as a failed leg")
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
