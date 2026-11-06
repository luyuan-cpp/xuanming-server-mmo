// Command commercefix 是转人工的交易合同(trade_commerce.status=10)的人工恢复工具
// (docs/design/jubaozhai-market.md §15.7,手册 docs/ops/trade-commerce-quarantine-runbook.md §4.2)。
//
// 三种处理,语义见 commerce.ManualAction:
//   - resume:回到转人工之前的阶段,由引擎重新判定(原因已经消除时用);
//   - reissue:给没到账的最终发放腿各补发一条,原样的资产、原样的收件人;
//   - refund:把已扣进托管的资产原样退回给扣出它的人,合同按取消结案。
//
// 工具只改合同行(commerce.ResolveManual:合同行锁、版本条件、留痕),不碰 op 行,不发任何资产:
// 新腿由引擎下一轮推进时发号、写 outbox、投递,和玩家操作同一条写路径。卡死的单条资产指令先用 assetopfix 终结。
//
// 鉴权:写库要带写者身份(authoritysql.LoadWriterConfig 读的环境变量),和服务进程同一套;只能在部署允许的 Pod 里
// 连库。操作人、依据、二次确认、操作人看过的版本都必须给,默认只打印计划,加 -execute 才写库。
// 维护排空期间不要运行。
//
// 退出码:0 成功;1 执行出错;2 参数不合法、合同不存在或前置条件不满足;3 合同版本已变,本次未改动。
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

	tradepb "proto/trade"
	"shared/authoritysql"
	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/data"
	"trade/internal/svc"
)

const (
	exitOK             = 0
	exitError          = 1
	exitRejected       = 2
	exitVersionChanged = 3
)

const (
	listDefaultLimit = 50
	listMaxLimit     = 500
	opTimeout        = 15 * time.Second
	logServiceName   = "trade-commercefix"
)

const usageText = `commercefix:转人工的交易合同的人工恢复工具

用法:
  commercefix -f etc/trade.yaml list [-limit 50]
  commercefix -f etc/trade.yaml show -commerce <id>
  commercefix -f etc/trade.yaml resolve -commerce <id> -action resume|reissue|refund -expect-version <版本> \
      -operator <名字> -reason <依据> -confirm <id> [-execute]

resolve 默认只打印计划,加 -execute 才写库。-expect-version 填 show 里看到的版本;合同之后被推进或处理过就拒绝。
前置条件见 show 的「能否处理」一栏。维护排空期间不要运行。

退出码:0 成功;1 执行出错;2 参数 / 前置条件不满足;3 合同版本已变,未改动。
`

// contractStore 是工具对交易库的依赖面,测试用假实现替换。
type contractStore interface {
	ListManual(ctx context.Context, limit int) ([]*tradepb.TradeCommerceRecord, error)
	Get(ctx context.Context, id uint64) (*tradepb.TradeCommerceRecord, error)
	LegOutcome(ctx context.Context, opID uint64) (data.LegOutcome, error)
	Resolve(ctx context.Context, req commerce.ManualRequest, execute bool) (commerce.ManualResult, error)
}

// repoStore 是 contractStore 的生产实现。
type repoStore struct{ repo *data.CommerceRepo }

func (s repoStore) ListManual(ctx context.Context, limit int) ([]*tradepb.TradeCommerceRecord, error) {
	return s.repo.ListManual(ctx, limit)
}

func (s repoStore) Get(ctx context.Context, id uint64) (*tradepb.TradeCommerceRecord, error) {
	return s.repo.Get(ctx, id)
}

func (s repoStore) LegOutcome(ctx context.Context, opID uint64) (data.LegOutcome, error) {
	return s.repo.LegResult(ctx, s.repo.DB, opID)
}

func (s repoStore) Resolve(ctx context.Context, req commerce.ManualRequest, execute bool) (commerce.ManualResult, error) {
	return commerce.ResolveManual(ctx, s.repo, req, uint64(time.Now().UnixMilli()), execute)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是可测的入口:不调 os.Exit,返回退出码。
func run(args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("commercefix", flag.ContinueOnError)
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
		limit, ok := parseList(rest[1:], stderr)
		if !ok {
			return exitRejected
		}
		return withStore(*configFile, true, stderr, func(ctx context.Context, store contractStore) int {
			return runList(ctx, store, limit, stdout, stderr)
		})
	case "show":
		id, ok := parseShow(rest[1:], stderr)
		if !ok {
			return exitRejected
		}
		return withStore(*configFile, true, stderr, func(ctx context.Context, store contractStore) int {
			return runShow(ctx, store, id, stdout, stderr)
		})
	case "resolve":
		opts, ok := parseResolve(rest[1:], stderr)
		if !ok {
			return exitRejected
		}
		return withStore(*configFile, false, stderr, func(ctx context.Context, store contractStore) int {
			return runResolve(ctx, store, opts, stdout, stderr)
		})
	default:
		fmt.Fprintf(stderr, "Unknown subcommand %q\n\n", rest[0])
		global.Usage()
		return exitRejected
	}
}

// withStore 加载配置、初始化日志、连 MySQL,把仓储交给 fn。resolve 即使只打印计划也锁合同行,所以要写者身份。
func withStore(configPath string, readOnly bool, stderr io.Writer, fn func(ctx context.Context, store contractStore) int) int {
	var c config.Config
	if err := conf.Load(configPath, &c); err != nil {
		fmt.Fprintf(stderr, "Failed to load configuration %s: %v\n", configPath, err)
		return exitError
	}
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
	db, err := svc.OpenToolMySQL(ctx, c.MySQL, permission)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer func() { _ = db.Close() }()
	return fn(ctx, repoStore{repo: &data.CommerceRepo{DB: db}})
}

// ── list ────────────────────────────────────────────────────────

func parseList(args []string, stderr io.Writer) (int, bool) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", listDefaultLimit, fmt.Sprintf("最多列多少条(1..%d)", listMaxLimit))
	if err := fs.Parse(args); err != nil {
		return 0, false
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > listMaxLimit {
		fmt.Fprintf(stderr, "list takes only -limit within [1, %d]\n", listMaxLimit)
		return 0, false
	}
	return *limit, true
}

func runList(ctx context.Context, store contractStore, limit int, stdout, stderr io.Writer) int {
	rows, err := store.ListManual(ctx, limit)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to list manual contracts: %v\n", err)
		return exitError
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No manual contracts")
		return exitOK
	}
	// 只用 ASCII:tabwriter 按字符数对齐。
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "commerce_id\tkind\tversion\tseller\tbuyer\tquarantine_stage\tquarantine_reason\tlegs")
	for _, r := range rows {
		stage, reason, legs := "?", "undecodable", 0
		if w, err := data.Workflow(r); err == nil {
			stage, reason, legs = w.GetQuarantine().GetStage(), w.GetQuarantine().GetReason(), len(w.Legs)
		}
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%s\t%s\t%d\n", r.CommerceId, r.Kind, r.Version, r.SellerPlayerId, r.BuyerPlayerId, stage, reason, legs)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "Output failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "\n%d contracts. Run show -commerce <id> before resolving.\n", len(rows))
	return exitOK
}

// ── show ────────────────────────────────────────────────────────

func parseShow(args []string, stderr io.Writer) (uint64, bool) {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.Uint64("commerce", 0, "合同 commerce_id")
	if err := fs.Parse(args); err != nil {
		return 0, false
	}
	if fs.NArg() != 0 || *id == 0 {
		fmt.Fprintln(stderr, "show takes only -commerce <id>")
		return 0, false
	}
	return *id, true
}

func runShow(ctx context.Context, store contractStore, id uint64, stdout, stderr io.Writer) int {
	r, err := store.Get(ctx, id)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to read commerce_id=%d: %v\n", id, err)
		return exitRejected
	}
	fmt.Fprintf(stdout, "commerce_id=%d kind=%d status=%d version=%d seller=%d buyer=%d expires_ms=%d updated_ms=%d\n",
		r.CommerceId, r.Kind, r.Status, r.Version, r.SellerPlayerId, r.BuyerPlayerId, r.ExpiresMs, r.UpdatedMs)
	w, err := data.Workflow(r)
	if err != nil {
		fmt.Fprintf(stdout, "workflow undecodable (%v): no manual action is possible, collect evidence per runbook §3\n", err)
		return exitOK
	}
	if q := w.GetQuarantine(); q != nil {
		fmt.Fprintf(stdout, "quarantine: stage=%s status=%d reason=%s at_ms=%d detail=%q\n", q.Stage, q.Status, q.Reason, q.AtMs, q.Detail)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "role\tplayer\tstream\top_id\toutcome\top_status\tsuperseded_by")
	for _, l := range w.Legs {
		opStatus := "-"
		if l.OpId != 0 && l.Outcome == 0 {
			if out, err := store.LegOutcome(ctx, l.OpId); err != nil {
				opStatus = "read_error"
			} else {
				opStatus = out.Status.String()
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n", l.Role, l.PlayerId, l.Stream, l.OpId, l.Outcome, opStatus, l.SupersededBy)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "Output failed: %v\n", err)
		return exitError
	}
	for _, a := range w.ManualActions {
		fmt.Fprintf(stdout, "history: action=%s operator=%s at_ms=%d version_before=%d new_legs=%v superseded=%v reason=%q\n",
			a.Action, a.Operator, a.AtMs, a.VersionBefore, a.NewLegs, a.SupersededLegs, a.Reason)
	}
	fmt.Fprintln(stdout, "能否处理:")
	for _, action := range []commerce.ManualAction{commerce.ManualResume, commerce.ManualReissue, commerce.ManualRefund} {
		plan, err := commerce.PlanManualReadOnly(ctx, r, store.LegOutcome, action)
		if err != nil {
			fmt.Fprintf(stdout, "  %s: no (%v)\n", action, err)
			continue
		}
		fmt.Fprintf(stdout, "  %s: yes -> stage=%s status=%d new_legs=%v superseded=%v\n", action, plan.Stage, plan.Status, plan.NewLegs, plan.SupersededLegs)
	}
	return exitOK
}

// ── resolve ─────────────────────────────────────────────────────

type resolveOptions struct {
	req     commerce.ManualRequest
	execute bool
}

func parseResolve(args []string, stderr io.Writer) (resolveOptions, bool) {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.Uint64("commerce", 0, "合同 commerce_id")
	action := fs.String("action", "", "resume、reissue 或 refund")
	version := fs.Uint64("expect-version", 0, "show 里看到的合同版本")
	operator := fs.String("operator", "", "操作人(≤64 字节)")
	reason := fs.String("reason", "", "依据(≤191 字节),例如工单号和核对结论")
	confirm := fs.Uint64("confirm", 0, "二次确认:必须与 -commerce 相同")
	execute := fs.Bool("execute", false, "写库;不带只打印计划")
	if err := fs.Parse(args); err != nil {
		return resolveOptions{}, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "resolve does not accept positional arguments: %v\n", fs.Args())
		return resolveOptions{}, false
	}
	parsed, ok := commerce.ParseManualAction(*action)
	switch {
	case *id == 0:
		fmt.Fprintln(stderr, "Missing -commerce")
	case !ok:
		fmt.Fprintf(stderr, "-action must be resume, reissue or refund (got %q)\n", *action)
	case *version == 0:
		fmt.Fprintln(stderr, "Missing -expect-version: run show first")
	case strings.TrimSpace(*operator) == "" || strings.IndexFunc(*operator, unicode.IsControl) >= 0:
		fmt.Fprintln(stderr, "-operator is required and must not contain control characters")
	case strings.TrimSpace(*reason) == "":
		fmt.Fprintln(stderr, "Missing -reason: manual handling must state its evidence")
	case *confirm != *id:
		fmt.Fprintf(stderr, "Confirmation failed: -confirm(%d) must equal -commerce(%d)\n", *confirm, *id)
	default:
		return resolveOptions{
			req: commerce.ManualRequest{
				CommerceID: *id, ExpectVersion: *version, Action: parsed, Operator: *operator, Reason: *reason,
			},
			execute: *execute,
		}, true
	}
	return resolveOptions{}, false
}

func runResolve(ctx context.Context, store contractStore, o resolveOptions, stdout, stderr io.Writer) int {
	result, err := store.Resolve(ctx, o.req, o.execute)
	if err != nil {
		switch {
		case errors.Is(err, commerce.ErrManualVersionChanged):
			fmt.Fprintf(stderr, "No change: %v\n", err)
			return exitVersionChanged
		case errors.Is(err, commerce.ErrManualNotAllowed), errors.Is(err, commerce.ErrManualNotFound):
			fmt.Fprintf(stderr, "Refused, no change: %v\n", err)
			return exitRejected
		default:
			fmt.Fprintf(stderr, "Manual handling failed commerce_id=%d: %v\n", o.req.CommerceID, err)
			return exitError
		}
	}
	audit := fmt.Sprintf("[CommerceManual] operator=%s commerce_id=%d action=%s version_before=%d quarantine_stage=%s quarantine_reason=%s stage=%s status=%d new_legs=%v superseded=%v reason=%q executed=%v",
		o.req.Operator, o.req.CommerceID, o.req.Action, result.VersionBefore, result.Quarantine.GetStage(), result.Quarantine.GetReason(),
		result.Stage, result.Status, result.NewLegs, result.SupersededLegs, o.req.Reason, o.execute)
	fmt.Fprintln(stdout, audit)
	if !o.execute {
		fmt.Fprintln(stdout, "Plan only, nothing written. Rerun with -execute to apply.")
		return exitOK
	}
	// 审计行打 ERROR 级,确保进日志检索;同一行贴进运维工单。
	logx.Error(audit)
	fmt.Fprintf(stdout, "commerce_id=%d updated; the engine picks it up in the next round. Attach the [CommerceManual] line to the ticket.\n", o.req.CommerceID)
	return exitOK
}
