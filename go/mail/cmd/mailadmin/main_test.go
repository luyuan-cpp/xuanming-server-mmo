package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	mailpb "proto/mail"
	table "shared/generated/pb/table"
	tip "shared/generated/tip"
	"shared/serverbase"
	"shared/serviceauth"
)

// fakeMailAdmin 只记下收到的发件请求并按脚本回包;不验签(验签由 mail 服务自己的测试覆盖)。
// 回包脚本按调用序号(从 1 起)决定,用例因此不依赖时序。
type fakeMailAdmin struct {
	mailpb.UnimplementedMailAdminServer
	mu       sync.Mutex
	requests []*mailpb.SendMailRequest
	reply    func(call int) (*mailpb.SendMailResponse, error)
}

func (f *fakeMailAdmin) record(in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, proto.Clone(in).(*mailpb.SendMailRequest))
	call := len(f.requests)
	f.mu.Unlock()
	return f.reply(call)
}

func (f *fakeMailAdmin) SendPersonalMail(_ context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return f.record(in)
}

func (f *fakeMailAdmin) SendSystemMail(_ context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return f.record(in)
}

func (f *fakeMailAdmin) received() []*mailpb.SendMailRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*mailpb.SendMailRequest(nil), f.requests...)
}

// startFakeMailAdmin 在本机随机端口起一个只认 MailAdmin 的 gRPC 服务,用例结束即停。
func startFakeMailAdmin(t *testing.T, reply func(call int) (*mailpb.SendMailResponse, error)) (*fakeMailAdmin, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMailAdmin{reply: reply}
	server := grpc.NewServer()
	mailpb.RegisterMailAdminServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return fake, listener.Addr().String()
}

// lostFirstReply 模拟首投已到邮件、回包丢失:第一次回传输错误,之后回原邮件(判重命中)。
func lostFirstReply(call int) (*mailpb.SendMailResponse, error) {
	if call == 1 {
		return nil, status.Error(codes.Unavailable, "reply lost")
	}
	return &mailpb.SendMailResponse{MailId: 900, Duplicated: true}, nil
}

func delivered(int) (*mailpb.SendMailResponse, error) {
	return &mailpb.SendMailResponse{MailId: 900}, nil
}

func testEnv(name string) string {
	if name == "MAIL_PRODUCER_SECRET_ADMIN" {
		return strings.Repeat("s", 32)
	}
	return ""
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, testEnv, &out, &errOut)
	return code, out.String(), errOut.String()
}

// retryArgs 取 stderr 里打印的重试命令,按 POSIX sh 引号规则切词,去掉开头的 "mailadmin"。
func retryArgs(t *testing.T, stderr string) []string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mailadmin ") {
			return splitPOSIX(t, line)[1:]
		}
	}
	t.Fatalf("结果未知时必须打印完整重试命令, stderr=%q", stderr)
	return nil
}

// splitPOSIX 只实现本用例需要的子集:空格分词、单引号内全为字面量、反斜杠转义下一个字节。
// 按字节处理对 UTF-8 安全:多字节序列里不会出现空格、引号或反斜杠。
func splitPOSIX(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted:
			if c == '\'' {
				quoted = false
			} else {
				word.WriteByte(c)
			}
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\' && i+1 < len(line):
			i++
			word.WriteByte(line[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if quoted {
		t.Fatalf("重试命令引号未闭合: %q", line)
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

func digest(t *testing.T, request *mailpb.SendMailRequest) string {
	t.Helper()
	value, err := serviceauth.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func writeRequestFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 带附件却没钉死请求号或过期时刻:原样重跑会换成新请求、再发一份附件,所以必须在发出之前拒绝。
func TestAttachmentMailWithoutExplicitKeysIsRefusedBeforeSending(t *testing.T) {
	fake, target := startFakeMailAdmin(t, delivered)
	file := writeRequestFile(t, `{"recipientPlayerId":"7","title":"补偿","attachments":{"currencies":[{"currencyType":1,"amount":"10"}]}}`)
	cases := []struct {
		name string
		args []string
	}{
		{"两项都缺", []string{"personal", "-target", target, "-to", "7", "-title", "补偿", "-currency", "1:10"}},
		{"缺过期时刻", []string{"personal", "-target", target, "-to", "7", "-title", "补偿", "-item", "100:1", "-request-id", "42"}},
		{"缺请求号", []string{"personal", "-target", target, "-to", "7", "-title", "补偿", "-currency", "1:10", "-expire-ms", "1893456000000"}},
		{"只给有效天数", []string{"personal", "-target", target, "-to", "7", "-title", "补偿", "-currency", "1:10", "-request-id", "42", "-expire-days", "3"}},
		{"文件带附件缺两项", []string{"personal", "-target", target, "-file", file}},
		{"系统邮件带附件", []string{"system", "-target", target, "-title", "补偿", "-currency", "1:10"}},
	}
	for _, c := range cases {
		code, _, stderr := runCLI(c.args...)
		if code != exitUsage {
			t.Fatalf("%s: exit=%d want %d stderr=%s", c.name, code, exitUsage, stderr)
		}
		if !strings.Contains(stderr, "-request-id") || !strings.Contains(stderr, "-expire-ms") {
			t.Fatalf("%s: 拒绝提示必须点名两个参数: %s", c.name, stderr)
		}
	}
	if n := len(fake.received()); n != 0 {
		t.Fatalf("被拒绝的请求不得到达邮件服务, 实收 %d 次", n)
	}
}

// 带附件、结果未知:打印的重试命令原样重跑,邮件收到的请求摘要与首投完全相同(邮件据此回原邮件,不再发附件)。
func TestUnknownOutcomeRetryCommandReplaysIdenticalAttachmentMail(t *testing.T) {
	fake, target := startFakeMailAdmin(t, lostFirstReply)
	code, _, stderr := runCLI("personal", "-target", target, "-to", "7", "-title", "it's 周赛 补偿", "-body", "第 1 名 奖励",
		"-currency", "1:10", "-currency", "2:5", "-item", "100:1", "-request-id", "42", "-expire-ms", "1893456000000")
	if code != exitError {
		t.Fatalf("结果未知应返回 %d, 实得 %d stderr=%s", exitError, code, stderr)
	}
	retry := retryArgs(t, stderr)
	for _, want := range []string{"-request-id=42", "-expire-ms=1893456000000"} {
		if !slices.Contains(retry, want) {
			t.Fatalf("重试命令缺 %s: %q", want, retry)
		}
	}
	if code, _, stderr = runCLI(retry...); code != exitOK {
		t.Fatalf("按重试命令重跑应成功, exit=%d stderr=%s", code, stderr)
	}
	requests := fake.received()
	if len(requests) != 2 {
		t.Fatalf("应恰好两次发件, 实得 %d", len(requests))
	}
	if digest(t, requests[0]) != digest(t, requests[1]) {
		t.Fatalf("重跑的请求与首投不同:\n首投 %v\n重跑 %v", requests[0], requests[1])
	}
	if requests[1].RequestId != 42 || requests[1].ExpireMs != 1893456000000 || len(requests[1].GetAttachments().GetCurrencies()) != 2 {
		t.Fatalf("重跑请求的键或附件不对: %v", requests[1])
	}
}

// 不带附件时请求号和过期时刻可以缺省生成,但结果未知后打印的命令必须钉死这两个生成值;
// 重跑时不再按当前时间重算过期时刻(命令里不再带 -expire-days)。
func TestUnknownOutcomeRetryCommandPinsGeneratedKeysForTextMail(t *testing.T) {
	fake, target := startFakeMailAdmin(t, lostFirstReply)
	code, _, stderr := runCLI("personal", "-target", target, "-to", "7", "-title", "维护通知", "-expire-days", "3")
	if code != exitError {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	first := fake.received()[0]
	if first.RequestId == 0 || first.ExpireMs == 0 {
		t.Fatalf("缺省时应生成请求号与过期时刻: %v", first)
	}
	retry := retryArgs(t, stderr)
	for _, want := range []string{"-request-id=" + strconv.FormatUint(first.RequestId, 10), "-expire-ms=" + strconv.FormatUint(first.ExpireMs, 10)} {
		if !slices.Contains(retry, want) {
			t.Fatalf("重试命令缺 %s: %q", want, retry)
		}
	}
	for _, word := range retry {
		if strings.HasPrefix(word, "-expire-days") {
			t.Fatalf("重试命令不能再按有效天数重算过期时刻: %q", retry)
		}
	}
	if code, _, stderr = runCLI(retry...); code != exitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	requests := fake.received()
	if len(requests) != 2 || digest(t, requests[0]) != digest(t, requests[1]) {
		t.Fatalf("重跑请求必须与首投同一摘要: %v", requests)
	}
}

// -file 会整体替换请求:显式给的请求号与过期时刻要补进文件请求,文件里已有不同的值就拒绝。
func TestFileRequestTakesExplicitKeysAndRejectsConflict(t *testing.T) {
	fake, target := startFakeMailAdmin(t, delivered)
	pinned := writeRequestFile(t, `{"recipientPlayerId":"7","title":"补偿","requestId":"42","expireMs":"1893456000000","attachments":{"items":[{"configId":100,"count":1}]}}`)
	if code, _, stderr := runCLI("personal", "-target", target, "-file", pinned, "-request-id", "43"); code != exitUsage {
		t.Fatalf("文件与命令行请求号冲突必须拒绝: exit=%d stderr=%s", code, stderr)
	}
	if code, _, stderr := runCLI("personal", "-target", target, "-file", pinned, "-expire-ms", "1893456000001"); code != exitUsage {
		t.Fatalf("文件与命令行过期时刻冲突必须拒绝: exit=%d stderr=%s", code, stderr)
	}
	if n := len(fake.received()); n != 0 {
		t.Fatalf("冲突的请求不得发出, 实收 %d 次", n)
	}
	bare := writeRequestFile(t, `{"recipientPlayerId":"7","title":"补偿","attachments":{"items":[{"configId":100,"count":1}]}}`)
	if code, _, stderr := runCLI("personal", "-target", target, "-file", bare, "-request-id", "42", "-expire-ms", "1893456000000"); code != exitOK {
		t.Fatalf("文件缺键时用命令行的值: exit=%d stderr=%s", code, stderr)
	}
	if code, _, stderr := runCLI("personal", "-target", target, "-file", pinned, "-request-id", "42"); code != exitOK {
		t.Fatalf("两边相同不算冲突: exit=%d stderr=%s", code, stderr)
	}
	requests := fake.received()
	if len(requests) != 2 || requests[0].RequestId != 42 || requests[0].ExpireMs != 1893456000000 || digest(t, requests[0]) != digest(t, requests[1]) {
		t.Fatalf("文件请求应带上显式键且两次同一摘要: %v", requests)
	}
}

// 邮件回故障码同样是结果未知(提交结果未知时邮件回故障码),要打印重试命令;正常业务拒绝不冒充结果未知。
func TestFaultCodeIsUnknownButRejectionIsNot(t *testing.T) {
	_, faultTarget := startFakeMailAdmin(t, func(int) (*mailpb.SendMailResponse, error) {
		return &mailpb.SendMailResponse{ErrorMessage: &base.TipInfoMessage{Id: uint32(table.CommonError_kServiceUnavailable)}}, nil
	})
	code, _, stderr := runCLI("personal", "-target", faultTarget, "-to", "7", "-title", "补偿", "-currency", "1:10", "-request-id", "42", "-expire-ms", "1893456000000")
	if code != exitError {
		t.Fatalf("故障码应返回 %d, 实得 %d", exitError, code)
	}
	if retry := retryArgs(t, stderr); !slices.Contains(retry, "-request-id=42") {
		t.Fatalf("故障码也要打印带原请求号的重试命令: %q", retry)
	}
	_, fullTarget := startFakeMailAdmin(t, func(int) (*mailpb.SendMailResponse, error) {
		return &mailpb.SendMailResponse{ErrorMessage: &base.TipInfoMessage{Id: uint32(table.MailError_kMailRecipientBoxFull)}}, nil
	})
	code, _, stderr = runCLI("personal", "-target", fullTarget, "-to", "7", "-title", "补偿", "-currency", "1:10", "-request-id", "42", "-expire-ms", "1893456000000")
	if code != exitRejected {
		t.Fatalf("业务拒绝应返回 %d, 实得 %d", exitRejected, code)
	}
	if strings.Contains(stderr, "outcome unknown") {
		t.Fatalf("业务拒绝是确定结果, 不能提示结果未知: %s", stderr)
	}
}

// 码表外的码与故障码同一口径(serverbase.TipVerdict,与 rank 奖励工作器一致):结果未知,退出码与传输失败相同,
// 打印钉死 -request-id 与 -expire-ms 的完整重试命令,原样重跑发出的请求与首投摘要相同。以前把它当确定拒收(退出码 3、
// 不打印重试命令),运维会换请求号重发,多发一份附件。两种码表外的码都试:不落在任何段里的,
// 以及落在邮件段里、高于本二进制已分配上界的(对端码表比本工具新)。
func TestUnrecognizedTipCodeIsUnknownLikeFault(t *testing.T) {
	var drift uint32
	for _, segment := range tip.Segments {
		if segment.Domain == "mail" {
			drift = segment.Hi + 1
		}
	}
	for _, code := range []uint32{999999, drift} {
		if code == 0 || serverbase.TipVerdict(code) != serverbase.VerdictUnknown {
			t.Fatalf("用例前提: 码 %d 应在码表外", code)
		}
		fake, target := startFakeMailAdmin(t, func(call int) (*mailpb.SendMailResponse, error) {
			if call == 1 {
				return &mailpb.SendMailResponse{ErrorMessage: &base.TipInfoMessage{Id: code}}, nil
			}
			return &mailpb.SendMailResponse{MailId: 900, Duplicated: true}, nil
		})
		exit, _, stderr := runCLI("personal", "-target", target, "-to", "7", "-title", "补偿", "-currency", "1:10", "-request-id", "42", "-expire-ms", "1893456000000")
		if exit != exitError || !strings.Contains(stderr, "outcome unknown") {
			t.Fatalf("码 %d 应按结果未知返回 %d, 实得 %d stderr=%s", code, exitError, exit, stderr)
		}
		retry := retryArgs(t, stderr)
		for _, want := range []string{"-request-id=42", "-expire-ms=1893456000000"} {
			if !slices.Contains(retry, want) {
				t.Fatalf("码 %d: 重试命令缺 %s: %q", code, want, retry)
			}
		}
		if exit, _, stderr = runCLI(retry...); exit != exitOK {
			t.Fatalf("码 %d: 按重试命令重跑应拿回原邮件, exit=%d stderr=%s", code, exit, stderr)
		}
		if requests := fake.received(); len(requests) != 2 || digest(t, requests[0]) != digest(t, requests[1]) {
			t.Fatalf("码 %d: 重跑的请求必须与首投同一摘要: %v", code, requests)
		}
	}
}

// 成功码按码表定性:0 与 kSuccess 都是成功,打印回包、退出码 0;以前只认 0,kSuccess 被当成拒收。
func TestSuccessTipCodeIsDelivered(t *testing.T) {
	_, target := startFakeMailAdmin(t, func(int) (*mailpb.SendMailResponse, error) {
		return &mailpb.SendMailResponse{MailId: 900, ErrorMessage: &base.TipInfoMessage{Id: uint32(table.CommonError_kSuccess)}}, nil
	})
	code, stdout, stderr := runCLI("personal", "-target", target, "-to", "7", "-title", "补偿", "-currency", "1:10", "-request-id", "42", "-expire-ms", "1893456000000")
	if code != exitOK || !strings.Contains(stdout, "900") {
		t.Fatalf("kSuccess 应按成功处理: exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

// 名单导入的幂等键在文件里:重试命令原样带上 -file,不附加发件才有的请求号。
func TestImportRetryCommandKeepsFileOnly(t *testing.T) {
	_, target := startFakeMailAdmin(t, delivered)
	file := writeRequestFile(t, `{"campaignId":"5","manifestGeneration":"1","shardIndex":0,"playerIds":["7","8"]}`)
	// 替身没实现 ImportMailRecipients,回 Unimplemented,对调用方就是传输层失败、结果未知。
	code, _, stderr := runCLI("import", "-target", target, "-file", file)
	if code != exitError {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	retry := retryArgs(t, stderr)
	if !slices.Contains(retry, "-file="+file) || slices.ContainsFunc(retry, func(word string) bool { return strings.HasPrefix(word, "-request-id") }) {
		t.Fatalf("导入的重试命令应只带原参数: %q", retry)
	}
}
