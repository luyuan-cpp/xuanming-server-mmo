// Command commercefix 是转人工的交易合同(trade_commerce.status=10)的人工处置工具
// (设计 docs/design/jubaozhai-market.md §15.7、§15.7.1,手册 docs/ops/trade-commerce-quarantine-runbook.md §4.2)。
//
// 六种动作,语义见 commerce.ManualAction:
//   - resume:回到转人工之前的阶段,由引擎重新判定(原因已经消除时用);
//   - reissue:给没到账的最终发放腿各补发一条,原样的资产、原样的收件人;
//   - refund:把已扣进托管的资产原样退回给扣出它的人(已部分成交的市场单只退剩余部分),合同按取消结案;
//   - partial:按人从证据里认定的缺口处理部分发放 / 部分扣出的腿(-part <腿名>=<缺口>,可重复);
//   - redirect:把已换主或已删的一方改绑到原账号名下的另一名角色(-party seller|buyer -to <player_id>);
//   - void-auction:作废有出价、成交不了的拍卖,拍品与它引用的出价一起退回(-linked <出价号> -linked-version <版本>)。
//
// 工具不连数据库:全部读写经 trade 的 TradeAdmin.ListManualContracts / ResolveManualContract 由运行中的副本执行,
// 锁内复核、版本条件与留痕都在服务端同一事务里完成(commerce.ResolveManual)。合同只改合同行,新腿由引擎发号投递。
// 调用方证明用 serviceauth 调用方 ops-commerce 签名;密钥只从环境变量读(缺省 MMORPG_TRADE_COMMERCE_FIX_SECRET,
// 与 trade 配置 Commerce.ManualResolutionSecretEnv 指名的变量同值),不进命令行,也不进输出。
//
//	commercefix -endpoint <trade grpc> list [-after-updated 0 -after-commerce 0] [-limit 50]
//	commercefix -endpoint <trade grpc> show -commerce <id>
//	commercefix -endpoint <trade grpc> resolve -commerce <id> -action <动作> -expect-version <版本> -expect-target <环境名> \
//	    -operator <名字> -reason <依据> -confirm <id> [动作参数] [-execute]
//
// resolve 默认只在锁内算计划(DRY_RUN,不写),加 -execute 才写库。-expect-target 是工单上写明的环境名
// (trade 配置 Commerce.ManualResolutionEnvironment),不从 show 的输出照抄:连错环境时服务端回 target_mismatch。
// 退出码:0 成功;1 执行出错、读库失败或提交结果未知;2 参数不合法、入口未开或签名被拒、前置条件不满足、合同不存在;
// 3 合同(或联动的出价)版本已变,本次未改动。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"shared/serviceauth"
	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/data"
)

const (
	exitOK             = 0
	exitError          = 1
	exitRejected       = 2
	exitVersionChanged = 3
)

const (
	defaultSecretEnv = "MMORPG_TRADE_COMMERCE_FIX_SECRET"
	listDefaultLimit = 50
	defaultTimeout   = 15 * time.Second
)

const usageText = `commercefix: manual handling of quarantined trade contracts (runbook docs/ops/trade-commerce-quarantine-runbook.md §4.2)

Usage:
  commercefix -endpoint <trade grpc> list [-after-updated 0 -after-commerce 0] [-limit 50]
  commercefix -endpoint <trade grpc> show -commerce <id>
  commercefix -endpoint <trade grpc> resolve -commerce <id> -action <action> -expect-version <version> -expect-target <env> \
      -operator <name> -reason <evidence> -confirm <id> [action parameters] [-execute]

Actions: resume, reissue, refund, partial, redirect, void-auction.
  partial       -part <leg>=<missing>  (repeatable; missing is instance:<guid>, pet, currency:<type>, comma separated)
  redirect      -party seller|buyer -to <player_id of a role owned by the original account>
  void-auction  -linked <bid commerce_id> -linked-version <bid version from show>

resolve only plans under the same locks by default (nothing written); add -execute to apply.
-expect-version is the version printed by show; -expect-target is the environment name on the ticket (not copied from show).
The signing secret is read from an environment variable (default MMORPG_TRADE_COMMERCE_FIX_SECRET, rename with -secret-env).

Exit codes: 0 success; 1 failure or commit outcome unknown; 2 invalid arguments, entry disabled, refused or not found;
3 the contract changed since show, nothing written.
`

// adminClient 是工具对 trade 的依赖面(生成的 TradeAdminClient 满足它),测试用假实现替换。
type adminClient interface {
	ListManualContracts(ctx context.Context, in *tradepb.ListManualContractsRequest, opts ...grpc.CallOption) (*tradepb.ListManualContractsResponse, error)
	ResolveManualContract(ctx context.Context, in *tradepb.ResolveManualContractRequest, opts ...grpc.CallOption) (*tradepb.ResolveManualContractResponse, error)
}

// deps 是 run 的外部依赖:拨号、环境变量、时钟(签名时间戳)。全部显式传入,测试不碰网络与真实环境。
type deps struct {
	dial   func(endpoint string) (adminClient, func(), error)
	getenv func(string) string
	now    func() time.Time
}

func productionDeps() deps {
	return deps{
		dial: func(endpoint string) (adminClient, func(), error) {
			conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return nil, nil, err
			}
			return tradepb.NewTradeAdminClient(conn), func() { _ = conn.Close() }, nil
		},
		getenv: os.Getenv,
		now:    time.Now,
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, productionDeps()))
}

// command 是解析好的一条子命令:要签名的请求与它的方法,以及拿到客户端之后怎么调用、怎么报告。
type command struct {
	method  string
	request proto.Message
	call    func(ctx context.Context, client adminClient) int
}

// run 是可测的入口:不调 os.Exit,返回退出码。子命令参数先于签名与拨号解析,参数写错不去碰服务。
func run(args []string, stdout, stderr io.Writer, d deps) int {
	global := flag.NewFlagSet("commercefix", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usageText) }
	endpoint := global.String("endpoint", "", "trade gRPC address (host:port)")
	timeout := global.Duration("timeout", defaultTimeout, "timeout of one call")
	secretEnv := global.String("secret-env", defaultSecretEnv, "environment variable holding the signing secret")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitRejected
	}
	rest := global.Args()
	if *endpoint == "" || len(rest) == 0 || *timeout <= 0 {
		global.Usage()
		return exitRejected
	}
	var cmd command
	var ok bool
	switch rest[0] {
	case "list":
		cmd, ok = parseList(rest[1:], stdout, stderr)
	case "show":
		cmd, ok = parseShow(rest[1:], stdout, stderr)
	case "resolve":
		cmd, ok = parseResolve(rest[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Unknown subcommand %q\n\n", rest[0])
		global.Usage()
		return exitRejected
	}
	if !ok {
		return exitRejected
	}
	// 签名先于拨号:密钥缺失或太短(口径在 serviceauth)不去碰服务。报错只报变量名,不报值。
	if err := serviceauth.Sign(cmd.method, cmd.request, config.ManualResolutionCaller, d.getenv(*secretEnv), uint64(d.now().UnixMilli())); err != nil {
		fmt.Fprintf(stderr, "Signing failed: check environment variable %s (at least 32 bytes): %v\n", *secretEnv, err)
		return exitRejected
	}
	client, closeClient, err := d.dial(*endpoint)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to connect to %s: %v\n", *endpoint, err)
		return exitError
	}
	defer closeClient()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return cmd.call(ctx, client)
}

// rpcExitCode 把 gRPC 错误映射成退出码:签名被拒、入口未开、参数被拒是确定的拒绝(2);其余(含提交结果未知)是 1。
func rpcExitCode(stderr io.Writer, err error) int {
	switch status.Code(err) {
	case codes.PermissionDenied:
		fmt.Fprintf(stderr, "Refused by trade: %v (entry disabled, wrong secret, or local clock off by more than 5 minutes)\n", err)
		return exitRejected
	case codes.InvalidArgument:
		fmt.Fprintf(stderr, "Refused by trade: %v\n", err)
		return exitRejected
	default:
		fmt.Fprintf(stderr, "Call failed, outcome unknown: %v\nRun show -commerce <id> before doing anything else.\n", err)
		return exitError
	}
}

// ── list ────────────────────────────────────────────────────────

func parseList(args []string, stdout, stderr io.Writer) (command, bool) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	afterUpdated := fs.Uint64("after-updated", 0, "cursor: next -after-updated printed by the previous page")
	afterCommerce := fs.Uint64("after-commerce", 0, "cursor: next -after-commerce printed by the previous page")
	limit := fs.Uint("limit", listDefaultLimit, fmt.Sprintf("page size (1..%d)", commerce.ManualListPageMax))
	if err := fs.Parse(args); err != nil {
		return command{}, false
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > commerce.ManualListPageMax {
		fmt.Fprintf(stderr, "list takes only -after-updated, -after-commerce and -limit within [1, %d]\n", commerce.ManualListPageMax)
		return command{}, false
	}
	in := &tradepb.ListManualContractsRequest{AfterUpdatedMs: *afterUpdated, AfterCommerceId: *afterCommerce, Limit: uint32(*limit)}
	return command{method: tradepb.TradeAdmin_ListManualContracts_FullMethodName, request: in, call: func(ctx context.Context, client adminClient) int {
		return runList(ctx, client, in, stdout, stderr)
	}}, true
}

func runList(ctx context.Context, client adminClient, in *tradepb.ListManualContractsRequest, stdout, stderr io.Writer) int {
	resp, err := client.ListManualContracts(ctx, in)
	if err != nil {
		return rpcExitCode(stderr, err)
	}
	if resp.GetRetry() {
		fmt.Fprintln(stderr, "trade could not read the manual contracts this time; retry later")
		return exitError
	}
	fmt.Fprintf(stdout, "target=%s\n", resp.GetTarget())
	if len(resp.GetContracts()) == 0 {
		fmt.Fprintln(stdout, "No manual contracts")
		return exitOK
	}
	// 只用 ASCII:tabwriter 按字符数对齐。
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "commerce_id\tkind\tversion\tseller\tbuyer\tquarantine_stage\tquarantine_reason\tlegs\tupdated_ms")
	for _, r := range resp.GetContracts() {
		stage, reason, legs := "?", "undecodable", 0
		if w, err := data.Workflow(r); err == nil {
			stage, reason, legs = w.GetQuarantine().GetStage(), w.GetQuarantine().GetReason(), len(w.GetLegs())
		}
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%s\t%s\t%d\t%d\n", r.GetCommerceId(), r.GetKind(), r.GetVersion(), r.GetSellerPlayerId(), r.GetBuyerPlayerId(),
			dash(stage), dash(reason), legs, r.GetUpdatedMs())
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "Output failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "\n%d contracts. Run show -commerce <id> before resolving.\n", len(resp.GetContracts()))
	if resp.GetNextAfterUpdatedMs() != 0 || resp.GetNextAfterCommerceId() != 0 {
		fmt.Fprintf(stdout, "next page: list -after-updated %d -after-commerce %d\n", resp.GetNextAfterUpdatedMs(), resp.GetNextAfterCommerceId())
	}
	return exitOK
}

// ── show ────────────────────────────────────────────────────────

func parseShow(args []string, stdout, stderr io.Writer) (command, bool) {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.Uint64("commerce", 0, "contract commerce_id")
	if err := fs.Parse(args); err != nil {
		return command{}, false
	}
	if fs.NArg() != 0 || *id == 0 {
		fmt.Fprintln(stderr, "show takes only -commerce <id>")
		return command{}, false
	}
	in := &tradepb.ResolveManualContractRequest{CommerceId: *id, Mode: tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW}
	return command{method: tradepb.TradeAdmin_ResolveManualContract_FullMethodName, request: in, call: func(ctx context.Context, client adminClient) int {
		return runShow(ctx, client, in, stdout, stderr)
	}}, true
}

func runShow(ctx context.Context, client adminClient, in *tradepb.ResolveManualContractRequest, stdout, stderr io.Writer) int {
	resp, err := client.ResolveManualContract(ctx, in)
	if err != nil {
		return rpcExitCode(stderr, err)
	}
	switch resp.GetVerdict() {
	case tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK:
	case tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND:
		fmt.Fprintf(stderr, "No contract commerce_id=%d on target=%s\n", in.GetCommerceId(), resp.GetTarget())
		return exitRejected
	default:
		fmt.Fprintf(stderr, "trade could not read the contract this time (%s); retry later\n", dash(resp.GetRefusal()))
		return exitError
	}
	printReport(stdout, resp)
	return exitOK
}

// printReport 打印 show 的核对清单。放行条件全部在服务端锁内判定,这里的 actionable 只是锁外试算。
func printReport(stdout io.Writer, resp *tradepb.ResolveManualContractResponse) {
	r := resp.GetRecord()
	fmt.Fprintf(stdout, "target=%s\n", resp.GetTarget())
	fmt.Fprintf(stdout, "commerce_id=%d kind=%d side=%d status=%d version=%d seller=%d buyer=%d quantity=%d filled=%d expires_ms=%d updated_ms=%d\n",
		r.GetCommerceId(), r.GetKind(), r.GetSide(), r.GetStatus(), r.GetVersion(), r.GetSellerPlayerId(), r.GetBuyerPlayerId(),
		r.GetQuantity(), r.GetFilledQuantity(), r.GetExpiresMs(), r.GetUpdatedMs())
	w, err := data.Workflow(r)
	if err != nil {
		fmt.Fprintf(stdout, "workflow undecodable (%v): no manual action is possible, collect evidence per runbook §3\n", err)
		return
	}
	if q := w.GetQuarantine(); q != nil {
		fmt.Fprintf(stdout, "quarantine: stage=%s status=%d reason=%s at_ms=%d detail=%q\n", q.GetStage(), q.GetStatus(), q.GetReason(), q.GetAtMs(), q.GetDetail())
	}
	if r.GetKind() == 5 {
		fmt.Fprintf(stdout, "auction: highest_bidder=%d highest_bid=%d winning_bid=%d\n", w.GetHighestBidder(), w.GetHighestBid(), w.GetWinningBidId())
	}
	if l := resp.GetLinked(); l != nil {
		fmt.Fprintf(stdout, "linked: commerce_id=%d kind=%d status=%d version=%d\n", l.GetCommerceId(), l.GetKind(), l.GetStatus(), l.GetVersion())
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "role\tplayer\tstream\top_id\toutcome\top_status\tfinal_ms\tsuperseded_by\tbundle")
	for _, l := range resp.GetLegs() {
		leg := l.GetLeg()
		opStatus := "-"
		switch {
		case l.GetReadFailed():
			opStatus = "read_error"
		case leg.GetOpId() != 0:
			opStatus = l.GetOpStatus().String()
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%d\t%s\t%s\n", leg.GetRole(), leg.GetPlayerId(), leg.GetStream(), leg.GetOpId(), leg.GetOutcome(),
			opStatus, l.GetFinalMs(), dash(leg.GetSupersededBy()), describeBundle(leg.GetBundle()))
	}
	_ = tw.Flush()
	if len(resp.GetParties()) > 0 {
		tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "party\tplayer\tcontract_rev\tcurrent_rev\tsame_account\tdeleted\tfrozen\tunknown\tchanged")
		for _, p := range resp.GetParties() {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%t\t%t\t%t\t%t\t%t\n", p.GetParty(), p.GetPlayerId(), p.GetContractOwnerRevision(), p.GetCurrentOwnerRevision(),
				p.GetSameAccount(), p.GetDeleted(), p.GetFrozen(), p.GetUnknown(), p.GetChanged())
		}
		_ = tw.Flush()
	}
	for _, a := range w.GetManualActions() {
		fmt.Fprintf(stdout, "history: action=%s operator=%q at_ms=%d version_before=%d new_legs=%v superseded=%v retargeted=%v party=%s from=%d to=%d linked=%d reason=%q\n",
			a.GetAction(), a.GetOperator(), a.GetAtMs(), a.GetVersionBefore(), a.GetNewLegs(), a.GetSupersededLegs(), a.GetRetargetedLegs(),
			dash(a.GetParty()), a.GetFromPlayerId(), a.GetToPlayerId(), a.GetLinkedCommerceId(), a.GetReason())
		for _, part := range a.GetPartialParts() {
			fmt.Fprintf(stdout, "  partial: leg=%s missing=%s\n", part.GetLeg(), describeBundle(part.GetMissing()))
		}
	}
	fmt.Fprintln(stdout, "actionable (pre-lock check; resolve without -execute is the authoritative in-lock plan):")
	for _, a := range resp.GetActions() {
		verdict := "no"
		if a.GetAllowed() {
			verdict = "yes"
		}
		fmt.Fprintf(stdout, "  %s: %s (%s)\n", a.GetAction(), verdict, a.GetDetail())
	}
}

// describeBundle 是资产包的一行摘要:按数量的物品、实例、宝宝、货币。只用 ASCII,数量、编号原样。
func describeBundle(b *assetpb.AssetBundle) string {
	if b == nil {
		return "-"
	}
	var parts []string
	for _, item := range b.GetItems() {
		parts = append(parts, fmt.Sprintf("item:%dx%d", item.GetConfigId(), item.GetCount()))
	}
	for _, id := range b.GetItemUuids() {
		parts = append(parts, "instance:"+strconv.FormatUint(id, 10))
	}
	for _, it := range b.GetItemInstances() {
		parts = append(parts, "instance:"+strconv.FormatUint(it.GetItemUuid(), 10))
	}
	if b.GetPetId() != 0 {
		parts = append(parts, "pet:"+strconv.FormatUint(b.GetPetId(), 10))
	}
	if p := b.GetPetInstance(); p != nil {
		parts = append(parts, "pet:"+strconv.FormatUint(p.GetPetId(), 10))
	}
	for _, c := range b.GetCurrencies() {
		parts = append(parts, fmt.Sprintf("currency:%d=%d", c.GetCurrencyType(), c.GetAmount()))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

// ── resolve ─────────────────────────────────────────────────────

// partFlags 收集可重复的 -part。
type partFlags []string

func (p *partFlags) String() string     { return strings.Join(*p, ";") }
func (p *partFlags) Set(v string) error { *p = append(*p, v); return nil }

type resolveOptions struct {
	request *tradepb.ResolveManualContractRequest
	execute bool
}

func parseResolve(args []string, stdout, stderr io.Writer) (command, bool) {
	o, ok := parseResolveOptions(args, stderr)
	if !ok {
		return command{}, false
	}
	return command{method: tradepb.TradeAdmin_ResolveManualContract_FullMethodName, request: o.request, call: func(ctx context.Context, client adminClient) int {
		return runResolve(ctx, client, o, stdout, stderr)
	}}, true
}

func parseResolveOptions(args []string, stderr io.Writer) (resolveOptions, bool) {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.Uint64("commerce", 0, "contract commerce_id")
	action := fs.String("action", "", "resume, reissue, refund, partial, redirect or void-auction")
	version := fs.Uint64("expect-version", 0, "contract version printed by show")
	target := fs.String("expect-target", "", "environment name written on the ticket (trade Commerce.ManualResolutionEnvironment, not copied from show)")
	operator := fs.String("operator", "", "operator name (<=64 bytes)")
	reason := fs.String("reason", "", "evidence (<=191 bytes): ticket number and conclusion")
	confirm := fs.Uint64("confirm", 0, "confirmation: must equal -commerce")
	var parts partFlags
	fs.Var(&parts, "part", "partial: <leg>=<missing>, repeatable; missing is instance:<guid>, pet, currency:<type>")
	party := fs.String("party", "", "redirect: seller or buyer")
	to := fs.Uint64("to", 0, "redirect: player_id of a role owned by the party's original account")
	linked := fs.Uint64("linked", 0, "void-auction: commerce_id of the bid the lot references")
	linkedVersion := fs.Uint64("linked-version", 0, "void-auction: bid version printed by show")
	execute := fs.Bool("execute", false, "write; without it only plan under the same locks")
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
		fmt.Fprintf(stderr, "-action must be one of resume, reissue, refund, partial, redirect, void-auction (got %q)\n", *action)
	case *version == 0:
		fmt.Fprintln(stderr, "Missing -expect-version: run show first")
	case strings.TrimSpace(*target) == "":
		fmt.Fprintln(stderr, "Missing -expect-target: the environment name written on the ticket")
	case strings.TrimSpace(*operator) == "" || strings.IndexFunc(*operator, unicode.IsControl) >= 0:
		fmt.Fprintln(stderr, "-operator is required and must not contain control characters")
	case strings.TrimSpace(*reason) == "" || strings.IndexFunc(*reason, unicode.IsControl) >= 0:
		fmt.Fprintln(stderr, "-reason is required (manual handling must state its evidence) and must not contain control characters")
	case *confirm != *id:
		fmt.Fprintf(stderr, "Confirmation failed: -confirm(%d) must equal -commerce(%d)\n", *confirm, *id)
	case (parsed == commerce.ManualPartial) != (len(parts) > 0):
		fmt.Fprintln(stderr, "-part is required for partial and only accepted by partial")
	case (parsed == commerce.ManualRedirect) != (*party != "" || *to != 0):
		fmt.Fprintln(stderr, "-party and -to are required for redirect and only accepted by redirect")
	case parsed == commerce.ManualRedirect && (*party != "seller" && *party != "buyer" || *to == 0):
		fmt.Fprintln(stderr, "redirect needs -party seller|buyer and -to <player_id>")
	case (parsed == commerce.ManualVoidAuction) != (*linked != 0 || *linkedVersion != 0):
		fmt.Fprintln(stderr, "-linked and -linked-version are required for void-auction and only accepted by void-auction")
	case parsed == commerce.ManualVoidAuction && (*linked == 0 || *linkedVersion == 0):
		fmt.Fprintln(stderr, "void-auction needs -linked <bid commerce_id> and -linked-version <bid version>")
	default:
		for _, raw := range parts {
			if _, err := commerce.ParsePartialPart(raw); err != nil {
				fmt.Fprintf(stderr, "Invalid -part %q: %v\n", raw, err)
				return resolveOptions{}, false
			}
		}
		mode := tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_DRY_RUN
		if *execute {
			mode = tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE
		}
		return resolveOptions{
			request: &tradepb.ResolveManualContractRequest{
				CommerceId: *id, Mode: mode, Action: string(parsed), ExpectVersion: *version, ExpectTarget: *target,
				Operator: *operator, Reason: *reason, Parts: append([]string(nil), parts...),
				Party: *party, ToPlayerId: *to, LinkedCommerceId: *linked, LinkedVersion: *linkedVersion,
			},
			execute: *execute,
		}, true
	}
	return resolveOptions{}, false
}

func runResolve(ctx context.Context, client adminClient, o resolveOptions, stdout, stderr io.Writer) int {
	resp, err := client.ResolveManualContract(ctx, o.request)
	if err != nil {
		return rpcExitCode(stderr, err)
	}
	if line := resp.GetAuditLine(); line != "" {
		fmt.Fprintln(stdout, line)
	}
	id := o.request.GetCommerceId()
	switch resp.GetVerdict() {
	case tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK:
		printPlan(stdout, resp)
		if !o.execute {
			fmt.Fprintln(stdout, "Plan only, nothing written (checked under the same locks and rules as the real run). Rerun with -execute to apply.")
			return exitOK
		}
		fmt.Fprintf(stdout, "commerce_id=%d updated; the engine issues the new legs in its next round. Attach the [CommerceManual] line to the ticket.\n", id)
		return exitOK
	case tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED:
		fmt.Fprintf(stderr, "Refused, no change: %s\n", resp.GetRefusal())
		return exitRejected
	case tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND:
		fmt.Fprintf(stderr, "Refused, no change: no contract commerce_id=%d on target=%s\n", id, resp.GetTarget())
		return exitRejected
	case tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_VERSION_CHANGED:
		fmt.Fprintf(stderr, "No change: %s; run show again\n", resp.GetRefusal())
		return exitVersionChanged
	default:
		fmt.Fprintf(stderr, "trade could not complete the check this time (nothing written: %s); retry later\n", dash(resp.GetRefusal()))
		return exitError
	}
}

// printPlan 打印计划(或已执行的结果):新腿、被取代的腿、改投的腿、改绑与联动的合同、缺口。
func printPlan(stdout io.Writer, resp *tradepb.ResolveManualContractResponse) {
	plan := resp.GetPlan()
	fmt.Fprintf(stdout, "plan: stage=%s status=%d version_before=%d new_legs=%v superseded=%v retargeted=%v\n",
		resp.GetStage(), resp.GetStatus(), plan.GetVersionBefore(), plan.GetNewLegs(), plan.GetSupersededLegs(), plan.GetRetargetedLegs())
	if plan.GetParty() != "" {
		fmt.Fprintf(stdout, "redirect: party=%s from=%d to=%d owner_revision %d -> %d\n", plan.GetParty(), plan.GetFromPlayerId(), plan.GetToPlayerId(),
			plan.GetIdentityBefore().GetOwnerRevision(), plan.GetIdentityAfter().GetOwnerRevision())
	}
	parts := append([]*tradepb.TradeCommercePartialPart(nil), plan.GetPartialParts()...)
	sort.Slice(parts, func(i, j int) bool { return parts[i].GetLeg() < parts[j].GetLeg() })
	for _, part := range parts {
		fmt.Fprintf(stdout, "partial: leg=%s missing=%s\n", part.GetLeg(), describeBundle(part.GetMissing()))
	}
	if l := resp.GetLinkedPlan(); l != nil {
		fmt.Fprintf(stdout, "linked: commerce_id=%d version_before=%d (the bid moves to bid_refund with the lot)\n", plan.GetLinkedCommerceId(), l.GetVersionBefore())
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
