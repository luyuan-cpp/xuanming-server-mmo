// Command spectatefeatured 是运营把一场公开局标成 / 取消标成赛事局的小工具(docs/design/battle-spectate-tiered-fanout.md §15.6):
// 用 serviceauth 调用方 ops 签名后调 MatchInternal.SetSpectateFeatured。密钥只从环境变量读,不进命令行,也不进日志。
//
//	MMORPG_MATCH_OPS_CALLER_SECRET=<secret> spectatefeatured -endpoint 127.0.0.1:50500 -battle 123 -featured=true
//
// 退出码:0 = 成功(配额满时仍是成功,看输出的 tier);1 = 被拒或调用失败;2 = 参数错。
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
)

// opsCallerSecretEnv 与部署注入 match 配置 Spectate.OpsCallerSecret 用同一个环境变量名。
const opsCallerSecretEnv = "MMORPG_MATCH_OPS_CALLER_SECRET"

func main() {
	endpoint := flag.String("endpoint", "", "match 的 gRPC 地址(host:port)")
	battle := flag.Uint64("battle", 0, "battle_id")
	featured := flag.Bool("featured", true, "true = 标成赛事局,false = 取消标记")
	timeout := flag.Duration("timeout", 5*time.Second, "调用超时")
	flag.Parse()
	if *endpoint == "" || *battle == 0 {
		fmt.Fprintln(os.Stderr, "必须指定 -endpoint 与 -battle")
		os.Exit(2)
	}
	request, err := newSignedRequest(*battle, *featured, os.Getenv(opsCallerSecretEnv), time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "签名失败(检查环境变量 %s,至少 32 字节): %v\n", opsCallerSecretEnv, err)
		os.Exit(2)
	}
	reply, err := call(*endpoint, *timeout, request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "调用失败: %v\n", err)
		os.Exit(1)
	}
	if code := reportReply(os.Stdout, os.Stderr, reply); code != 0 {
		os.Exit(code)
	}
}

// reportReply 打印调用结果并返回退出码。验签被拒是业务拒绝,不是 gRPC 错误,这里要说清楚怎么排查,否则运营只看到一个枚举名。
func reportReply(stdout, stderr io.Writer, reply *matchpb.SetSpectateFeaturedResponse) int {
	fmt.Fprintf(stdout, "reject=%s tier=%s\n", reply.GetReject().String(), reply.GetTier().String())
	switch reply.GetReject() {
	case matchpb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE:
		return 0
	case matchpb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_DENIED:
		fmt.Fprintf(stderr, "match 拒绝了这次调用:运营入口未开启,或签名无效。检查 match 配置 Spectate.OpsCallerSecret、环境变量 %s,以及本机时钟(偏差不能超过 5 分钟)\n", opsCallerSecretEnv)
		return 1
	default:
		return 1
	}
}

// newSignedRequest 组请求并签名。now 显式传入:签名时间戳决定服务端是否在 5 分钟窗口内接受。
func newSignedRequest(battle uint64, featured bool, secret string, now time.Time) (*matchpb.SetSpectateFeaturedRequest, error) {
	if battle == 0 {
		return nil, errors.New("battle_id 不能为 0")
	}
	request := &matchpb.SetSpectateFeaturedRequest{BattleId: battle, Featured: featured}
	if err := serviceauth.Sign(matchpb.MatchInternal_SetSpectateFeatured_FullMethodName, request, config.SpectateOpsCaller, secret, uint64(now.UnixMilli())); err != nil {
		return nil, err
	}
	return request, nil
}

func call(endpoint string, timeout time.Duration, request *matchpb.SetSpectateFeaturedRequest) (*matchpb.SetSpectateFeaturedResponse, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return matchpb.NewMatchInternalClient(conn).SetSpectateFeatured(ctx, request)
}
