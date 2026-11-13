// Command battlescriptreview 是运营审核玩家战斗剧本的小工具(docs/design/battle-script.md §6.3):
// 用 serviceauth 调用方 ops 签名后调 MatchInternal.ListPendingBattleScripts / ReviewBattleScript。
// 密钥只从环境变量读(与 spectatefeatured 同一把运营密钥),不进命令行,也不进日志。
//
//	MMORPG_MATCH_OPS_CALLER_SECRET=<secret> battlescriptreview -endpoint 127.0.0.1:50500 -list -limit 20
//	MMORPG_MATCH_OPS_CALLER_SECRET=<secret> battlescriptreview -endpoint 127.0.0.1:50500 -script 123 -revision 2 -approve -reviewer alice
//	MMORPG_MATCH_OPS_CALLER_SECRET=<secret> battlescriptreview -endpoint 127.0.0.1:50500 -script 123 -revision 2 -reviewer alice -reason "规则互相矛盾"
//
// -list 打印待审剧本的完整正文(protobuf JSON),审核员据此判断;审核按 (script, revision) 做 CAS,
// 作者在你看完之后又改了、删了,或别人先审了,会回 STALE,重新 -list 再审。
// 退出码:0 = 成功;1 = 被拒或调用失败;2 = 参数错。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"match/internal/config"

	matchpb "proto/match"
	"shared/serviceauth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
)

// opsCallerSecretEnv 与部署注入 match 配置 Spectate.OpsCallerSecret 用同一个环境变量名。
const opsCallerSecretEnv = "MMORPG_MATCH_OPS_CALLER_SECRET"

func main() {
	endpoint := flag.String("endpoint", "", "match 的 gRPC 地址(host:port)")
	list := flag.Bool("list", false, "列出待审剧本")
	limit := flag.Uint("limit", 20, "-list 一次列几份(1..50)")
	script := flag.Uint64("script", 0, "要审的 script_id")
	revision := flag.Uint("revision", 0, "要审的版本号(取自 -list 的输出)")
	approve := flag.Bool("approve", false, "true = 通过;不带 = 驳回(须写 -reason)")
	reviewer := flag.String("reviewer", "", "审核员名(进审计流水)")
	reason := flag.String("reason", "", "驳回原因(作者会看到)或通过备注")
	timeout := flag.Duration("timeout", 5*time.Second, "调用超时")
	flag.Parse()

	if *endpoint == "" || (*list == (*script != 0)) {
		fmt.Fprintln(os.Stderr, "-endpoint is required, and exactly one of -list or -script must be given")
		os.Exit(2)
	}
	secret := os.Getenv(opsCallerSecretEnv)
	conn, err := grpc.NewClient(*endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Dial failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := matchpb.NewMatchInternalClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *list {
		request, err := newListRequest(uint32(*limit), secret, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Signing failed (check environment variable %s, at least 32 bytes): %v\n", opsCallerSecretEnv, err)
			os.Exit(2)
		}
		reply, err := client.ListPendingBattleScripts(ctx, request)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Call failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(reportList(os.Stdout, os.Stderr, reply))
	}

	request, err := newReviewRequest(*script, uint32(*revision), *approve, *reviewer, *reason, secret, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Bad arguments or signing failed: %v\n", err)
		os.Exit(2)
	}
	reply, err := client.ReviewBattleScript(ctx, request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Call failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(reportReject(os.Stdout, os.Stderr, reply.GetReject()))
}

func newListRequest(limit uint32, secret string, now time.Time) (*matchpb.ListPendingBattleScriptsRequest, error) {
	request := &matchpb.ListPendingBattleScriptsRequest{Limit: limit}
	if err := serviceauth.Sign(matchpb.MatchInternal_ListPendingBattleScripts_FullMethodName, request, config.SpectateOpsCaller, secret, uint64(now.UnixMilli())); err != nil {
		return nil, err
	}
	return request, nil
}

// newReviewRequest 组审核请求并签名。now 显式传入:签名时间戳决定服务端是否在 5 分钟窗口内接受
func newReviewRequest(script uint64, revision uint32, approve bool, reviewer, reason, secret string, now time.Time) (*matchpb.ReviewBattleScriptRequest, error) {
	if script == 0 || revision == 0 || reviewer == "" {
		return nil, errors.New("-script, -revision and -reviewer are required")
	}
	if !approve && reason == "" {
		return nil, errors.New("rejecting needs -reason (the author will see it)")
	}
	request := &matchpb.ReviewBattleScriptRequest{ScriptId: script, Revision: revision, Approve: approve, Reviewer: reviewer, Reason: reason}
	if err := serviceauth.Sign(matchpb.MatchInternal_ReviewBattleScript_FullMethodName, request, config.SpectateOpsCaller, secret, uint64(now.UnixMilli())); err != nil {
		return nil, err
	}
	return request, nil
}

// reportList 打印待审剧本:每份一段,先摘要行,再完整正文
func reportList(stdout, stderr io.Writer, reply *matchpb.ListPendingBattleScriptsResponse) int {
	if code := reportReject(io.Discard, stderr, reply.GetReject()); code != 0 {
		return code
	}
	marshal := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}
	for _, pending := range reply.GetScripts() {
		fmt.Fprintf(stdout, "script=%d revision=%d owner=%d submitted_ms=%d previously_approved_revision=%d\n",
			pending.GetScriptId(), pending.GetRevision(), pending.GetOwnerPlayerId(), pending.GetSubmittedMs(), pending.GetApprovedRevision())
		body, err := marshal.Marshal(pending.GetContent())
		if err != nil {
			fmt.Fprintf(stderr, "Cannot render script %d: %v\n", pending.GetScriptId(), err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n\n", body)
	}
	fmt.Fprintf(stdout, "pending=%d\n", len(reply.GetScripts()))
	return 0
}

// reportReject 把带内拒绝翻成可排查的说明与退出码
func reportReject(stdout, stderr io.Writer, reject matchpb.BattleScriptReviewReject) int {
	fmt.Fprintf(stdout, "reject=%s\n", reject.String())
	switch reject {
	case matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_NONE:
		return 0
	case matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_DENIED:
		fmt.Fprintf(stderr, "match rejected this call: the ops entry is disabled or the signature is invalid. Check match config Spectate.OpsCallerSecret, environment variable %s, and the local clock (skew must not exceed 5 minutes)\n", opsCallerSecretEnv)
	case matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_STALE:
		fmt.Fprintln(stderr, "This revision is no longer pending (edited, deleted or already reviewed). Run -list again before reviewing.")
	case matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INVALID:
		fmt.Fprintln(stderr, "Invalid arguments, or the script no longer validates against the current tables and can only be rejected.")
	case matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INTERNAL:
		fmt.Fprintln(stderr, "match storage failed. A list can be retried; after a review failure, run -list to see whether it was applied.")
	}
	return 1
}
