// Command formationfix 是永久 UNKNOWN 全球成局的人工处置工具(2026-11-05,手册 docs/ops/match-formation-manual-abort-runbook.md,
// 设计 docs/design/cross-zone-matchmaking.md §13.3)。
//
// 只是事故出口,替代不了自动路径:只处置「过了截止加终局宽限、没有任何终局行、持久 Create 原件过不了终局核对、工作租约已到期」
// 的成局(形状不合格 expired_unrecoverable、original_mismatch)。原件合格的局归到期恢复 / 帮会取消自动裁决,工具一律拒绝。
//
// 工具不连数据库:全部读写经 match 服务的 MatchInternal.ListStrandedFormations / ResolveStrandedFormation 由运行中的副本执行
// (匹配库只归 match 服务,AGENTS.md §7.6),锁内复核、ABORT 与审计行都在服务端同一事务里完成(authority/manual_abort.go)。
// 调用方证明用 serviceauth 调用方 ops-formation 签名;密钥只从环境变量读(缺省 MMORPG_MATCH_FORMATION_FIX_SECRET,
// 与 match 配置 GlobalMatch.ManualResolutionSecretEnv 指名的变量同值),不进命令行,也不进输出。
//
//	formationfix -endpoint <match grpc> list [-after-retry 0 -after-battle 0] [-limit 50]
//	formationfix -endpoint <match grpc> show -battle <id>
//	formationfix -endpoint <match grpc> abort -battle <id> -expect-digest <hex> -expect-target <环境名> \
//	    -operator <名字> -reason <依据> -confirm <id> [-execute]
//
// abort 默认只在锁内演练(DRY_RUN,整段回滚),加 -execute 才写库。-expect-target 是工单上写明的环境名
// (match 配置 GlobalMatch.ManualResolutionEnvironment),不从 show 的输出照抄:连错环境时由服务端回 target_mismatch。
// 退出码:0 成功;1 执行出错、读库失败或提交结果未知;2 参数不合法、入口未开或签名被拒、前置条件不满足、没有这一局;
// 3 已被处理或成局已变,本次未改动。
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"match/internal/authority"
	"match/internal/config"
	pb "proto/match"
	"shared/serviceauth"
)

const (
	exitOK        = 0
	exitError     = 1
	exitRejected  = 2
	exitUnchanged = 3
)

const (
	defaultSecretEnv = "MMORPG_MATCH_FORMATION_FIX_SECRET"
	listDefaultLimit = 50
	defaultTimeout   = 10 * time.Second
)

const usageText = `formationfix:永久 UNKNOWN 全球成局的人工处置工具(手册 docs/ops/match-formation-manual-abort-runbook.md)

用法:
  formationfix -endpoint <match grpc> list [-after-retry 0 -after-battle 0] [-limit 50]
  formationfix -endpoint <match grpc> show -battle <id>
  formationfix -endpoint <match grpc> abort -battle <id> -expect-digest <hex> -expect-target <环境名> \
      -operator <名字> -reason <依据> -confirm <id> [-execute]

abort 默认只在锁内演练、不写库,加 -execute 才写 ABORT。-expect-digest 填 show 打印的 digest,成局在 show 之后变过就拒绝;
-expect-target 填工单上写明的环境名(不从 show 照抄),与服务端的环境名不符就拒绝。
签名密钥从环境变量读(缺省 MMORPG_MATCH_FORMATION_FIX_SECRET,可用 -secret-env 改名)。

退出码:0 成功;1 执行出错 / 提交结果未知;2 参数 / 前置条件不满足;3 已被处理或成局已变,未改动。
`

// opsClient 是工具对 match 的依赖面(生成的 MatchInternalClient 满足它),测试用假实现替换。
type opsClient interface {
	ListStrandedFormations(ctx context.Context, in *pb.ListStrandedFormationsRequest, opts ...grpc.CallOption) (*pb.ListStrandedFormationsResponse, error)
	ResolveStrandedFormation(ctx context.Context, in *pb.ResolveStrandedFormationRequest, opts ...grpc.CallOption) (*pb.ResolveStrandedFormationResponse, error)
}

// deps 是 run 的外部依赖:拨号、环境变量、时钟(签名时间戳)。全部显式传入,测试不碰网络与真实环境。
type deps struct {
	dial   func(endpoint string) (opsClient, func(), error)
	getenv func(string) string
	now    func() time.Time
}

func productionDeps() deps {
	return deps{
		dial: func(endpoint string) (opsClient, func(), error) {
			conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return nil, nil, err
			}
			return pb.NewMatchInternalClient(conn), func() { _ = conn.Close() }, nil
		},
		getenv: os.Getenv,
		now:    time.Now,
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, productionDeps()))
}

// run 是可测的入口:不调 os.Exit,返回退出码。子命令参数先于签名与拨号解析,参数写错不去碰服务。
func run(args []string, stdout, stderr io.Writer, d deps) int {
	global := flag.NewFlagSet("formationfix", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usageText) }
	endpoint := global.String("endpoint", "", "match 的 gRPC 地址(host:port)")
	timeout := global.Duration("timeout", defaultTimeout, "单次调用超时")
	secretEnv := global.String("secret-env", defaultSecretEnv, "签名密钥所在的环境变量名")
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
	case "abort":
		cmd, ok = parseAbort(rest[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "Unknown subcommand %q\n\n", rest[0])
		global.Usage()
		return exitRejected
	}
	if !ok {
		return exitRejected
	}
	// 签名先于拨号:密钥缺失或太短(口径在 serviceauth)不去碰服务。报错只报变量名,不报值。
	if err := sign(cmd.method, cmd.request, d.getenv(*secretEnv), d.now()); err != nil {
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

// command 是解析好的一条子命令:要签名的请求与它的方法,以及拿到客户端之后怎么调用、怎么报告。
type command struct {
	method  string
	request proto.Message
	call    func(ctx context.Context, client opsClient) int
}

// sign 用 ops-formation 签名请求;签名绑定方法与整个请求,时间戳决定服务端是否在 5 分钟窗口内接受。
func sign(method string, request proto.Message, secret string, now time.Time) error {
	return serviceauth.Sign(method, request, config.ManualResolutionCaller, secret, uint64(now.UnixMilli()))
}

// rpcExitCode 把 gRPC 错误映射成退出码:签名被拒、入口未开、参数被拒是确定的拒绝(2);其余(含 ABORT 提交结果未知)是 1。
func rpcExitCode(stderr io.Writer, err error) int {
	switch status.Code(err) {
	case codes.PermissionDenied:
		fmt.Fprintf(stderr, "Refused by match: %v (entry disabled, wrong secret, or local clock off by more than 5 minutes)\n", err)
		return exitRejected
	case codes.InvalidArgument:
		fmt.Fprintf(stderr, "Refused by match: %v\n", err)
		return exitRejected
	default:
		fmt.Fprintf(stderr, "Call failed, outcome unknown: %v\nRun show -battle <id> before doing anything else.\n", err)
		return exitError
	}
}

// ── list ────────────────────────────────────────────────────────

func parseList(args []string, stdout, stderr io.Writer) (command, bool) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	afterRetry := fs.Uint64("after-retry", 0, "游标:上一页打印的 next -after-retry")
	afterBattle := fs.Uint64("after-battle", 0, "游标:上一页打印的 next -after-battle")
	limit := fs.Uint("limit", listDefaultLimit, fmt.Sprintf("最多列多少条(1..%d)", authority.StrandedFormationPageMax))
	if err := fs.Parse(args); err != nil {
		return command{}, false
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > authority.StrandedFormationPageMax {
		fmt.Fprintf(stderr, "list takes only -after-retry, -after-battle and -limit within [1, %d]\n", authority.StrandedFormationPageMax)
		return command{}, false
	}
	in := &pb.ListStrandedFormationsRequest{AfterRetryAtMs: *afterRetry, AfterBattleId: *afterBattle, Limit: uint32(*limit)}
	return command{method: pb.MatchInternal_ListStrandedFormations_FullMethodName, request: in, call: func(ctx context.Context, client opsClient) int {
		return runList(ctx, client, in, stdout, stderr)
	}}, true
}

func runList(ctx context.Context, client opsClient, in *pb.ListStrandedFormationsRequest, stdout, stderr io.Writer) int {
	resp, err := client.ListStrandedFormations(ctx, in)
	if err != nil {
		return rpcExitCode(stderr, err)
	}
	if resp.GetRetry() {
		fmt.Fprintln(stderr, "match could not read the formations this time; retry later")
		return exitError
	}
	fmt.Fprintf(stdout, "target=%s now_ms=%d\n", resp.GetTarget(), resp.GetNowMs())
	if len(resp.GetFormations()) == 0 {
		fmt.Fprintln(stdout, "No expired formations without a terminal outcome")
		return exitOK
	}
	// 只用 ASCII:tabwriter 按字符数对齐。
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "battle_id\tstate\tclass\tfailed_check\tmanual\tterminal\tactivity\tmembers\teffective_deadline_ms\tretry_at_ms")
	for _, f := range resp.GetFormations() {
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\t%t\t%d\t%d\t%d\n", f.GetBattleId(), f.GetState(), classLabel(f.GetFormationClass()),
			dash(f.GetFailedCheck()), manualLabel(f.GetRefusal()), terminalLabel(f), f.GetActivity(), f.GetMemberCount(),
			f.GetEffectiveDeadlineMs(), f.GetRetryAtMs())
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "Output failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "\n%d formations. Run show -battle <id> before aborting.\n", len(resp.GetFormations()))
	if resp.GetNextRetryAtMs() != 0 || resp.GetNextBattleId() != 0 {
		fmt.Fprintf(stdout, "next page: list -after-retry %d -after-battle %d\n", resp.GetNextRetryAtMs(), resp.GetNextBattleId())
	}
	return exitOK
}

// ── show ────────────────────────────────────────────────────────

func parseShow(args []string, stdout, stderr io.Writer) (command, bool) {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	battle := fs.Uint64("battle", 0, "battle_id")
	if err := fs.Parse(args); err != nil {
		return command{}, false
	}
	if fs.NArg() != 0 || *battle == 0 {
		fmt.Fprintln(stderr, "show takes only -battle <id>")
		return command{}, false
	}
	in := &pb.ResolveStrandedFormationRequest{BattleId: *battle, Mode: pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW}
	return command{method: pb.MatchInternal_ResolveStrandedFormation_FullMethodName, request: in, call: func(ctx context.Context, client opsClient) int {
		return runShow(ctx, client, in, stdout, stderr)
	}}, true
}

func runShow(ctx context.Context, client opsClient, in *pb.ResolveStrandedFormationRequest, stdout, stderr io.Writer) int {
	resp, err := client.ResolveStrandedFormation(ctx, in)
	if err != nil {
		return rpcExitCode(stderr, err)
	}
	switch resp.GetVerdict() {
	case pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_NOT_FOUND:
		fmt.Fprintf(stderr, "No formation battle_id=%d on target=%s\n", in.GetBattleId(), resp.GetTarget())
		return exitRejected
	case pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RETRY:
		fmt.Fprintln(stderr, "match could not read the formation this time; retry later")
		return exitError
	}
	if resp.GetFormation() == nil {
		// 拒绝却没有核对清单(例如 formation_unreadable:成局行本身解不开):没有可打印的字段,只报原因码。
		fmt.Fprintf(stderr, "Refused: battle_id=%d on target=%s cannot be checked (%s)\n", in.GetBattleId(), resp.GetTarget(), dash(resp.GetRefusal()))
		return exitRejected
	}
	printFormation(stdout, resp)
	return exitOK
}

// printFormation 打印 show 与 abort 共用的核对清单。放行条件全部在服务端按 SQL 可核的事实判定,
// 下面的「另查」只作提示,不作放行条件。
func printFormation(stdout io.Writer, resp *pb.ResolveStrandedFormationResponse) {
	f := resp.GetFormation()
	fmt.Fprintf(stdout, "target=%s replica=%s now_ms=%d\n", resp.GetTarget(), resp.GetReplica(), resp.GetNowMs())
	fmt.Fprintf(stdout, "battle_id=%d state=%d match_mode=%d config_id=%d members=%d activity=%t\n",
		f.GetBattleId(), f.GetState(), f.GetMatchMode(), f.GetConfigId(), f.GetMemberCount(), f.GetActivity())
	fmt.Fprintf(stdout, "operation_id=%s target_instance_id=%s preflight_instance_id=%s\n", f.GetOperationId(), f.GetTargetInstanceId(), f.GetPreflightInstanceId())
	fmt.Fprintf(stdout, "created_ms=%d deadline_ms=%d retry_at_ms=%d effective_deadline_ms=%d\n", f.GetCreatedMs(), f.GetDeadlineMs(), f.GetRetryAtMs(), f.GetEffectiveDeadlineMs())
	fmt.Fprintf(stdout, "outcome_protocol=%d preflight_supported=%d adopted_observed=%t adopted_protocol=%d\n",
		f.GetOutcomeProtocolVersion(), f.GetPreflightSupportedVersion(), f.GetAdoptedOutcomeObserved(), f.GetAdoptedOutcomeProtocolVersion())
	if f.GetCreateDecodable() {
		fmt.Fprintf(stdout, "create: bytes=%d created_at_ms=%d deadline_ms=%d admit_before_ms=%d players=%d\n",
			f.GetCreateRequestBytes(), f.GetCreateCreatedAtMs(), f.GetCreateDeadlineMs(), f.GetCreateAdmitBeforeMs(), f.GetCreatePlayerCount())
	} else {
		fmt.Fprintf(stdout, "create: bytes=%d undecodable\n", f.GetCreateRequestBytes())
	}
	fmt.Fprintf(stdout, "terminal: %s\n", terminalLabel(f))
	for _, t := range f.GetTickets() {
		fmt.Fprintf(stdout, "ticket: player_id=%d state=%d formation_id=%d owner_revision=%d\n", t.GetPlayerId(), t.GetState(), t.GetFormationId(), t.GetOwnerRevision())
	}
	fmt.Fprintf(stdout, "class=%s failed_check=%s\n", classLabel(f.GetFormationClass()), dash(f.GetFailedCheck()))
	fmt.Fprintf(stdout, "digest=%s\n", f.GetDigest())
	if code := f.GetRefusal(); code != "" {
		fmt.Fprintf(stdout, "能否处理: no (%s)\n", code)
	} else {
		fmt.Fprintln(stdout, "能否处理: yes (锁外预检;以 abort 不带 -execute 的锁内演练为准)")
	}
	fmt.Fprintln(stdout, "另查(只作提示,不作放行条件):")
	fmt.Fprintf(stdout, "  - battle 日志里 battle_id=%d 的 ErrBattleOutcomeOriginal / battle_sql_outcome_unflushed_at_shutdown\n", f.GetBattleId())
	if f.GetFailedCheck() == "adopted_legacy" {
		fmt.Fprintln(stdout, "  - battle 明确采用了旧协议:另查这一局是否已按旧路径发过结算")
	}
	if f.GetActivity() {
		fmt.Fprintln(stdout, "  - 帮会历练局:ABORT 之后到 guild 日志确认 [GuildTrial] 恢复报错消失")
	}
}

// ── abort ───────────────────────────────────────────────────────

type abortOptions struct {
	request *pb.ResolveStrandedFormationRequest
	execute bool
}

func parseAbort(args []string, stdout, stderr io.Writer) (command, bool) {
	o, ok := parseAbortOptions(args, stderr)
	if !ok {
		return command{}, false
	}
	return command{method: pb.MatchInternal_ResolveStrandedFormation_FullMethodName, request: o.request, call: func(ctx context.Context, client opsClient) int {
		return runAbort(ctx, client, o, stdout, stderr)
	}}, true
}

func parseAbortOptions(args []string, stderr io.Writer) (abortOptions, bool) {
	fs := flag.NewFlagSet("abort", flag.ContinueOnError)
	fs.SetOutput(stderr)
	battle := fs.Uint64("battle", 0, "battle_id")
	digest := fs.String("expect-digest", "", "show 打印的 digest")
	target := fs.String("expect-target", "", "工单上写明的目标环境名(与 match 的 GlobalMatch.ManualResolutionEnvironment 相同,不从 show 照抄)")
	operator := fs.String("operator", "", fmt.Sprintf("操作人(≤%d 字节)", authority.ManualOperatorMaxBytes))
	reason := fs.String("reason", "", fmt.Sprintf("依据(≤%d 字节),写工单号与核对结论", authority.ManualReasonMaxBytes))
	confirm := fs.Uint64("confirm", 0, "二次确认:必须与 -battle 相同")
	execute := fs.Bool("execute", false, "写 ABORT;不带只在锁内演练、不写库")
	if err := fs.Parse(args); err != nil {
		return abortOptions{}, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "abort does not accept positional arguments: %v\n", fs.Args())
		return abortOptions{}, false
	}
	auditErr := authority.ValidateManualAudit(*operator, *reason)
	switch {
	case *battle == 0:
		fmt.Fprintln(stderr, "Missing -battle")
	case !validDigest(*digest):
		fmt.Fprintln(stderr, "-expect-digest must be the 64-char lowercase hex digest printed by show")
	case strings.TrimSpace(*target) == "":
		fmt.Fprintln(stderr, "Missing -expect-target: the environment name written on the incident ticket")
	case auditErr != nil:
		fmt.Fprintf(stderr, "Invalid -operator / -reason: %v\n", auditErr)
	case *confirm != *battle:
		fmt.Fprintf(stderr, "Confirmation failed: -confirm(%d) must equal -battle(%d)\n", *confirm, *battle)
	default:
		mode := pb.StrandedFormationMode_STRANDED_FORMATION_MODE_DRY_RUN
		if *execute {
			mode = pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT
		}
		return abortOptions{
			request: &pb.ResolveStrandedFormationRequest{BattleId: *battle, Mode: mode, ExpectDigest: *digest, ExpectTarget: *target, OperatorName: *operator, Reason: *reason},
			execute: *execute,
		}, true
	}
	return abortOptions{}, false
}

func validDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func runAbort(ctx context.Context, client opsClient, o abortOptions, stdout, stderr io.Writer) int {
	resp, err := client.ResolveStrandedFormation(ctx, o.request)
	if err != nil {
		return rpcExitCode(stderr, err)
	}
	if line := resp.GetAuditLine(); line != "" {
		fmt.Fprintln(stdout, line)
	}
	battle := o.request.GetBattleId()
	switch resp.GetVerdict() {
	case pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE:
		if !o.execute {
			fmt.Fprintln(stdout, "Plan only, nothing written (checked under the same locks and rules as the real abort). Rerun with -execute to apply.")
			return exitOK
		}
		fmt.Fprintf(stdout, "battle_id=%d aborted: every member's ticket is released and the battle result is void. Guild trial lobbies release on the next recovery round. Attach the [FormationManual] line to the ticket.\n", battle)
		return exitOK
	case pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED:
		fmt.Fprintf(stderr, "Refused, no change: %s\n", resp.GetRefusal())
		return exitRejected
	case pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_NOT_FOUND:
		fmt.Fprintf(stderr, "Refused, no change: no formation battle_id=%d on target=%s\n", battle, resp.GetTarget())
		return exitRejected
	case pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_CHANGED:
		fmt.Fprintln(stderr, "No change: the formation changed after show; run show again")
		return exitUnchanged
	case pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RESOLVED:
		fmt.Fprintf(stderr, "No change: battle_id=%d already has a terminal outcome (%s)\n", battle, terminalLabel(resp.GetFormation()))
		return exitUnchanged
	default:
		fmt.Fprintln(stderr, "match could not complete the check this time (nothing written); retry later")
		return exitError
	}
}

// ── 展示 ────────────────────────────────────────────────────────

func classLabel(c pb.StrandedFormationClass) string {
	switch c {
	case pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_AUTO:
		return "auto"
	case pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH:
		return "original_mismatch"
	case pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID:
		return "shape_invalid"
	default:
		return "-"
	}
}

func manualLabel(refusal string) string {
	if refusal == "" {
		return "eligible"
	}
	return "no:" + refusal
}

func terminalLabel(f *pb.StrandedFormation) string {
	if !f.GetHasTerminalRow() {
		return "none"
	}
	return fmt.Sprintf("decision=%s source_kind=%s", f.GetTerminalDecision(), pb.MatchBattleCompletionSourceKind(f.GetTerminalSourceKind()))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
