// Command mailadmin直连现役M2 MailAdmin；只从环境变量读取admin角色密钥，不接受旧共享token。
// 邮件按(sender_kind, request_id)判重并核全文摘要：带附件的发件必须显式给 -request-id 与 -expire-ms
// （或写进 -file），否则拒绝执行；结果未知（传输失败，或邮件回故障码、码表外的码）时打印钉死这两项的完整重试命令，
// 原样重跑只会拿回原邮件，不会再发一份附件。系统附件先用原冻结名单执行import/seal。
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	base "proto/common/base"
	mailpb "proto/mail"
	"shared/serverbase"
	"shared/serviceauth"
	"strconv"
	"strings"
	"time"
)

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitRejected = 3
)

type multiFlag []string

func (f *multiFlag) String() string         { return strings.Join(*f, ",") }
func (f *multiFlag) Set(value string) error { *f = append(*f, value); return nil }
func main()                                 { os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)) }
func run(args []string, getenv func(string) string, out, errOut io.Writer) int {
	if len(args) == 0 {
		return exitUsage
	}
	kind := args[0]
	methods := map[string]string{"personal": mailpb.MailAdmin_SendPersonalMail_FullMethodName, "system": mailpb.MailAdmin_SendSystemMail_FullMethodName, "import": mailpb.MailAdmin_ImportMailRecipients_FullMethodName, "seal": mailpb.MailAdmin_SealMailCampaign_FullMethodName}
	method, ok := methods[kind]
	if !ok {
		fmt.Fprintln(errOut, "用法: mailadmin personal|system|import|seal [参数]")
		return exitUsage
	}
	fs := flag.NewFlagSet("mailadmin "+kind, flag.ContinueOnError)
	fs.SetOutput(errOut)
	target := fs.String("target", "127.0.0.1:50900", "直连MailAdmin地址")
	secretEnv := fs.String("secret-env", "MAIL_PRODUCER_SECRET_ADMIN", "admin签名密钥的环境变量名，禁止传密钥值")
	file := fs.String("file", "", "现役请求proto JSON文件；冻结名单操作必须提供")
	to := fs.Uint64("to", 0, "个人收件人ID")
	title := fs.String("title", "", "标题")
	body := fs.String("body", "", "正文")
	requestID := fs.Uint64("request-id", 0, "重试必须使用首次请求号")
	expire := fs.Uint64("expire-ms", 0, "重试必须使用首次过期时刻")
	days := fs.Int("expire-days", 7, "未填expire-ms时的有效天数")
	timeout := fs.Duration("timeout", 10*time.Second, "本次调用预算")
	var currency, item multiFlag
	fs.Var(&currency, "currency", "type:amount，可重复")
	fs.Var(&item, "item", "config_id:count，可重复")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 || *timeout <= 0 || *timeout > time.Minute || *days < 1 || *days > 365 {
		return exitUsage
	}
	secret := strings.TrimSpace(getenv(*secretEnv))
	if len([]byte(secret)) < 32 {
		fmt.Fprintln(errOut, "admin签名密钥未注入或不足32字节")
		return exitUsage
	}
	var request proto.Message
	switch kind {
	case "import":
		request = &mailpb.ImportMailRecipientsRequest{}
	case "seal":
		request = &mailpb.SealMailCampaignRequest{}
	default:
		request = &mailpb.SendMailRequest{SenderKind: 1, RequestId: *requestID, RecipientPlayerId: *to, Title: *title, Body: *body, ExpireMs: *expire}
	}
	if *file != "" {
		source, err := os.Open(*file)
		if err != nil {
			fmt.Fprintln(errOut, "请求文件不可读")
			return exitUsage
		}
		bytes, err := io.ReadAll(io.LimitReader(source, (4<<20)+1))
		_ = source.Close()
		if err != nil {
			fmt.Fprintln(errOut, "请求文件不可读")
			return exitUsage
		}
		if len(bytes) > 4<<20 {
			fmt.Fprintln(errOut, "请求文件超限")
			return exitUsage
		}
		if err := protojson.Unmarshal(bytes, request); err != nil {
			fmt.Fprintln(errOut, "请求JSON不符合现役协议")
			return exitUsage
		}
	} else if kind == "import" || kind == "seal" {
		fmt.Fprintln(errOut, "名单导入/封存必须提供-file")
		return exitUsage
	}
	switch in := request.(type) {
	case *mailpb.SendMailRequest:
		if in.SenderKind != 0 && in.SenderKind != 1 {
			return exitUsage
		}
		in.SenderKind = 1
		// -file 会整体替换请求，显式给的请求号与过期时刻要补回去；文件里已有不同的值就拒绝，免得重跑时用错键。
		if !pinKey(&in.RequestId, *requestID) || !pinKey(&in.ExpireMs, *expire) {
			fmt.Fprintln(errOut, "-request-id/-expire-ms conflict with request_id/expire_ms in -file")
			return exitUsage
		}
		if len(currency) > 0 || len(item) > 0 {
			if in.Attachments == nil {
				in.Attachments = &mailpb.MailAttachments{}
			}
			for _, value := range currency {
				a, b, err := pair(value, 64)
				if err != nil {
					return exitUsage
				}
				in.Attachments.Currencies = append(in.Attachments.Currencies, &mailpb.MailCurrency{CurrencyType: uint32(a), Amount: b})
			}
			for _, value := range item {
				a, b, err := pair(value, 32)
				if err != nil {
					return exitUsage
				}
				in.Attachments.Items = append(in.Attachments.Items, &mailpb.MailItem{ConfigId: uint32(a), Count: uint32(b)})
			}
		}
		requestKey, expireMs := in.RequestId, in.ExpireMs
		if requestKey == 0 {
			id, err := randomRequestID()
			if err != nil {
				return exitError
			}
			requestKey = id
		}
		if expireMs == 0 {
			expireMs = uint64(time.Now().Add(time.Duration(*days) * 24 * time.Hour).UnixMilli())
		}
		// 带附件时两项必须由调用方钉死：缺省生成的随机号和按当前时间算的过期时刻，原样重跑同一条命令都会变，
		// 邮件把它当成新请求，结果未知后重跑就会再发一份附件。只给出可复制的建议值，不替调用方发出。
		if hasAttachments(in) && (in.RequestId == 0 || in.ExpireMs == 0) {
			fmt.Fprintf(errOut, "mail with attachments needs explicit -request-id and -expire-ms (or request_id/expire_ms in -file) so a retry replays the same request; e.g. -request-id=%d -expire-ms=%d\n", requestKey, expireMs)
			return exitUsage
		}
		in.RequestId, in.ExpireMs = requestKey, expireMs
		fmt.Fprintf(out, "request_id=%d expire_ms=%d（未知结果按相同请求原样重试）\n", in.RequestId, in.ExpireMs)
	case *mailpb.ImportMailRecipientsRequest:
		if in.SenderKind != 0 && in.SenderKind != 1 {
			return exitUsage
		}
		in.SenderKind = 1
	case *mailpb.SealMailCampaignRequest:
		if in.SenderKind != 0 && in.SenderKind != 1 {
			return exitUsage
		}
		in.SenderKind = 1
	}
	if err := serviceauth.Sign(method, request, "admin", secret, uint64(time.Now().UnixMilli())); err != nil {
		return exitUsage
	}
	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return exitError
	}
	defer conn.Close()
	client := mailpb.NewMailAdminClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var response proto.Message
	switch kind {
	case "personal":
		response, err = client.SendPersonalMail(ctx, request.(*mailpb.SendMailRequest))
	case "system":
		response, err = client.SendSystemMail(ctx, request.(*mailpb.SendMailRequest))
	case "import":
		response, err = client.ImportMailRecipients(ctx, request.(*mailpb.ImportMailRecipientsRequest))
	case "seal":
		response, err = client.SealMailCampaign(ctx, request.(*mailpb.SealMailCampaignRequest))
	}
	if err != nil {
		fmt.Fprintf(errOut, "call failed, outcome unknown (code=%s); retry with exactly this command, changing nothing (including the -file content):\n%s\n", status.Code(err), retryCommand(kind, fs, request))
		return exitError
	}
	reply, ok := response.(interface{ GetErrorMessage() *base.TipInfoMessage })
	if !ok {
		return exitError
	}
	// 按 tip 码表定性，与 rank 奖励工作器同一口径（serverbase.TipVerdict）：只有业务拒绝是确定结果（退出码 3，不打印重试命令）；
	// 故障码与码表外的码都是结果未知，退出码与传输失败相同——邮件提交结果未知时回的就是故障码；码表外的码说明对端跑着更新的码表、
	// 本工具定不了性，当成确定拒收会诱导运维换请求号重发，多发一份附件。
	code := reply.GetErrorMessage().GetId()
	switch verdict := serverbase.TipVerdict(code); verdict {
	case serverbase.VerdictOK:
	case serverbase.VerdictBizReject:
		fmt.Fprintf(errOut, "业务码=%d\n", code)
		return exitRejected
	default:
		fmt.Fprintf(errOut, "业务码=%d\n", code)
		fmt.Fprintf(errOut, "mail returned tip code %d (%s), outcome unknown; retry with exactly this command, changing nothing (including the -file content):\n%s\n", code, verdict, retryCommand(kind, fs, request))
		return exitError
	}
	encoded, err := protojson.Marshal(response)
	if err != nil {
		return exitError
	}
	fmt.Fprintln(out, string(encoded))
	return exitOK
}

// pinKey 把命令行显式给的值并进请求：请求里没有就用它；两边都有且不同返回 false。0 表示没给。
func pinKey(field *uint64, flagValue uint64) bool {
	switch {
	case flagValue == 0 || *field == flagValue:
		return true
	case *field == 0:
		*field = flagValue
		return true
	}
	return false
}

func hasAttachments(in *mailpb.SendMailRequest) bool {
	return len(in.GetAttachments().GetCurrencies()) > 0 || len(in.GetAttachments().GetItems()) > 0
}

// randomRequestID 只给不带附件的发件缺省生成请求号；结果未知时打印的重试命令会钉死它。
func randomRequestID() (uint64, error) {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return 0, err
	}
	if id := binary.BigEndian.Uint64(bytes[:]); id != 0 {
		return id, nil
	}
	return 1, nil
}

// retryCommand 拼出原样重跑本次请求的完整命令：保留调用方显式给的全部参数，发件的请求号与过期时刻换成本次实际发出的值，
// 并去掉 -expire-days（否则重跑会按新的当前时间重算过期时刻）。名单导入/封存的幂等键在 -file 里，原样保留即可。
func retryCommand(kind string, fs *flag.FlagSet, request proto.Message) string {
	words := []string{"mailadmin", kind}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "request-id", "expire-ms", "expire-days":
			return
		}
		values := []string{f.Value.String()}
		if repeated, ok := f.Value.(*multiFlag); ok {
			values = *repeated
		}
		for _, value := range values {
			words = append(words, shellQuote("-"+f.Name+"="+value))
		}
	})
	if in, ok := request.(*mailpb.SendMailRequest); ok {
		words = append(words, "-request-id="+strconv.FormatUint(in.RequestId, 10), "-expire-ms="+strconv.FormatUint(in.ExpireMs, 10))
	}
	return strings.Join(words, " ")
}

// shellQuote 按 POSIX sh 规则给参数加单引号，只含安全字符时原样返回。
// 单引号串在 PowerShell 里同样按字面量处理；值本身含单引号时只保证 POSIX sh 的写法正确。
func shellQuote(word string) string {
	unsafe := strings.IndexFunc(word, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_=.,:/@%+", r))
	})
	if word != "" && unsafe < 0 {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func pair(value string, bits int) (uint64, uint64, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, errors.New("需要type:amount")
	}
	a, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	b, err := strconv.ParseUint(parts[1], 10, bits)
	if err != nil || b == 0 {
		return 0, 0, errors.New("数量必须为正")
	}
	return a, b, nil
}
