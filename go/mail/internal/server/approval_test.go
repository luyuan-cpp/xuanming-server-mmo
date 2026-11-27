package server

// approval_test.go —— MailAdmin 发件的双人复核准入(docs/design/admin-identity-and-dual-control.md §5.5、§5.7)。
// 被拒的用例用 nil 仓储:准入若放过了请求,Repo.Send 判重的第一次读就会 panic,所以拒绝必然发生在任何读库写库之前。
// 放行的用例用连不上的库:过了准入的请求走到判重的第一次读,回故障 tip,连库次数加一。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mail/internal/data"
	"mail/internal/metrics"
	base "proto/common/base"
	mailpb "proto/mail"
	table "shared/generated/pb/table"
	"shared/serviceauth"
)

const (
	approvalTestSecret = "mail-approval-unit-test-only-never-a-real-secret"
	approvalTestTitle  = "approval-test-title-7f3a"
)

// unreachableDatabase 是连不上的邮件库:每次取连接计一次再失败。
type unreachableDatabase struct{ dials atomic.Int64 }

func (d *unreachableDatabase) Connect(context.Context) (driver.Conn, error) {
	d.dials.Add(1)
	return nil, errors.New("test mail database is unreachable")
}
func (d *unreachableDatabase) Driver() driver.Driver { return d }
func (d *unreachableDatabase) Open(string) (driver.Conn, error) {
	return d.Connect(context.Background())
}

func unreachableRepo(t *testing.T) (*data.Repo, *unreachableDatabase) {
	t.Helper()
	database := &unreachableDatabase{}
	db := sql.OpenDB(database)
	t.Cleanup(func() { _ = db.Close() })
	unused := func(context.Context) (uint64, error) {
		return 0, errors.New("id issuer is not used before the first read")
	}
	return data.New(db, unused, unused, func(uint32) bool { return true }, data.GuildRPCDirectory{}), database
}

// approvalTestServer 认 admin 与四个系统发件方的签名(同一把测试密钥)。没调 ConfigureDualControl 时是 New 的缺省要求。
func approvalTestServer(t *testing.T, repo *data.Repo) *Server {
	t.Helper()
	secrets := map[string]string{}
	for _, caller := range []string{"admin", "rank", "guild", "battle", "match"} {
		secrets[caller] = approvalTestSecret
	}
	verifier, err := serviceauth.New(secrets)
	if err != nil {
		t.Fatal(err)
	}
	return New(repo, verifier)
}

// signedPersonal 组一封由 caller 签名的个人文本邮件;批准记录在签名之前放进去,签名覆盖它。
func signedPersonal(t *testing.T, caller string, requestID uint64, approval *base.AdminApproval) *mailpb.SendMailRequest {
	t.Helper()
	in := &mailpb.SendMailRequest{SenderKind: data.SenderKind(caller), RequestId: requestID, RecipientPlayerId: 7, Title: approvalTestTitle, Approval: approval}
	if err := serviceauth.Sign(mailpb.MailAdmin_SendPersonalMail_FullMethodName, in, caller, approvalTestSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	return in
}

// validApproval 是后台为请求号 requestID 签发的合格批准记录:批准号就是请求号,两人不同。
func validApproval(requestID uint64) *base.AdminApproval {
	return &base.AdminApproval{ApprovalId: strconv.FormatUint(requestID, 10), Proposer: "alice", Approver: "bob", ApprovedAtMs: 1764115200000}
}

// expectDenied:在准入处以 PermissionDenied 拒绝,没有回包,错误文本不回显请求内容。
func expectDenied(t *testing.T, s *Server, in *mailpb.SendMailRequest) {
	t.Helper()
	response, err := s.SendPersonalMail(context.Background(), in)
	if status.Code(err) != codes.PermissionDenied || response != nil {
		t.Fatalf("应在准入处拒绝: err=%v response=%v", err, response)
	}
	if strings.Contains(status.Convert(err).Message(), approvalTestTitle) {
		t.Fatalf("错误文本不能回显请求内容: %v", err)
	}
}

// expectReachedRepository:过了准入、走到了邮件库(连不上的库回故障 tip,连库次数增加)。
func expectReachedRepository(t *testing.T, s *Server, database *unreachableDatabase, in *mailpb.SendMailRequest) {
	t.Helper()
	before := database.dials.Load()
	response, err := s.SendPersonalMail(context.Background(), in)
	if err != nil || response.GetErrorMessage().GetId() != uint32(table.CommonError_kServiceUnavailable) || database.dials.Load() == before {
		t.Fatalf("应过准入并走到邮件库: err=%v response=%v dials=%d", err, response, database.dials.Load())
	}
}

// unapprovedCommands 读 mail_admin_unapproved_command_total{caller,method};序列不存在时 found=false。
func unapprovedCommands(t *testing.T, caller, method string) (value float64, found bool) {
	t.Helper()
	metrics.Register()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "mail_admin_unapproved_command_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["caller"] == caller && labels["method"] == method {
				return metric.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// admin 不带批准记录:New 的缺省、按配置装配、配置里没写 admin,都在准入处拒绝,不碰库。
func TestAdminSendWithoutApprovalIsDeniedBeforeRepository(t *testing.T) {
	for name, configure := range map[string]func(*Server){
		"New的缺省":     func(*Server) {},
		"按配置装配":      func(s *Server) { s.ConfigureDualControl(nil, false) },
		"配置里没写admin": func(s *Server) { s.ConfigureDualControl([]string{"rank"}, false) },
	} {
		t.Run(name, func(t *testing.T) {
			s := approvalTestServer(t, nil)
			configure(s)
			expectDenied(t, s, signedPersonal(t, "admin", 42, nil))
		})
	}
}

// admin 带合格的批准记录:过准入,走到邮件库。
func TestAdminSendWithValidApprovalReachesRepository(t *testing.T) {
	repo, database := unreachableRepo(t)
	s := approvalTestServer(t, repo)
	s.ConfigureDualControl(nil, false)
	expectReachedRepository(t, s, database, signedPersonal(t, "admin", 42, validApproval(42)))
}

// 批准记录不合格在准入处拒绝、不碰库:请求号不是批准号(同一份批准换个请求号再发一封)、自己批自己。
func TestInvalidApprovalIsDeniedBeforeRepository(t *testing.T) {
	selfApproved := validApproval(42)
	selfApproved.Approver = selfApproved.Proposer
	for name, build := range map[string]func(*testing.T) *mailpb.SendMailRequest{
		"请求号不是批准号": func(t *testing.T) *mailpb.SendMailRequest { return signedPersonal(t, "admin", 43, validApproval(42)) },
		"自己批自己":    func(t *testing.T) *mailpb.SendMailRequest { return signedPersonal(t, "admin", 42, selfApproved) },
	} {
		t.Run(name, func(t *testing.T) {
			s := approvalTestServer(t, nil)
			s.ConfigureDualControl(nil, false)
			expectDenied(t, s, build(t))
		})
	}
}

// rank、guild、battle、match 是系统发件方,不在要求表里:不带批准记录照常走到邮件库。
func TestSystemSendersWithoutApprovalReachRepository(t *testing.T) {
	repo, database := unreachableRepo(t)
	s := approvalTestServer(t, repo)
	s.ConfigureDualControl(nil, false)
	for _, caller := range []string{"rank", "guild", "battle", "match"} {
		expectReachedRepository(t, s, database, signedPersonal(t, caller, 42, nil))
	}
}

// AdminAllowUnapproved 打开:admin 不带批准记录被接受、走到邮件库,计数加一;序列在装配时就预建好。
func TestAllowUnapprovedAcceptsAndCountsAdminSendWithoutApproval(t *testing.T) {
	repo, database := unreachableRepo(t)
	s := approvalTestServer(t, repo)
	s.ConfigureDualControl(nil, true)
	before, found := unapprovedCommands(t, "admin", metrics.MethodSendPersonal)
	if !found {
		t.Fatal("装配时应预建 admin 的 0 值序列")
	}
	expectReachedRepository(t, s, database, signedPersonal(t, "admin", 42, nil))
	if after, _ := unapprovedCommands(t, "admin", metrics.MethodSendPersonal); after != before+1 {
		t.Fatalf("不带批准记录被接受一次,计数应加一: before=%v after=%v", before, after)
	}
}

// 带了批准记录就必须合格:开关打开也一样,表外的系统发件方也一样。
func TestInvalidApprovalIsDeniedEvenWhenUnapprovedAllowed(t *testing.T) {
	s := approvalTestServer(t, nil)
	s.ConfigureDualControl(nil, true)
	for _, caller := range []string{"admin", "rank"} {
		expectDenied(t, s, signedPersonal(t, caller, 43, validApproval(42)))
	}
}
