package server

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"mail/internal/data"
	"mail/internal/metrics"
	base "proto/common/base"
	mailpb "proto/mail"
	"shared/assetop"
	"shared/dualcontrol"
	table "shared/generated/pb/table"
	"shared/rolefence"
	"shared/roleidentity"
	"shared/serviceauth"
)

// adminCaller 是后台(java/gm_admin)与运维工具签发件请求用的调用方。它的发件全是甲级动作
// (docs/design/admin-identity-and-dual-control.md §3),恒在要求批准记录的调用方里。
const adminCaller = "admin"

type Server struct {
	mailpb.UnimplementedClientPlayerMailServer
	mailpb.UnimplementedMailAdminServer
	repo     *data.Repo
	auth     *serviceauth.Verifier
	reminder Notifier
	// approvalRequired 是发件必须带双人复核批准记录的调用方,allowUnapproved 是 AdminAllowUnapproved。
	// New 给的缺省是只有 admin、不放行;ConfigureDualControl 在启动时按配置装配一次,之后只读。
	approvalRequired map[string]bool
	allowUnapproved  bool
}
type Notifier interface {
	Personal(context.Context, uint64, uint64)
}

func New(repo *data.Repo, auth *serviceauth.Verifier, reminders ...Notifier) *Server {
	s := &Server{repo: repo, auth: auth, approvalRequired: map[string]bool{adminCaller: true}}
	if len(reminders) > 0 {
		s.reminder = reminders[0]
	}
	return s
}

// ConfigureDualControl 按配置装配发件的双人复核要求(docs/design/admin-identity-and-dual-control.md §5.5、§5.7),启动时调一次。
// callers 是 ApprovalRequiredCallers;admin 恒在其中:后台的发件全是甲级,把它从表里拿掉等于绕开 AdminAllowUnapproved 的逐条告警。
// allowUnapproved 为 true 时接受表内调用方不带记录的发件,只给本机开发:启动打一行 ERROR,之后每一次都打 ERROR 并计数。
func (s *Server) ConfigureDualControl(callers []string, allowUnapproved bool) {
	s.approvalRequired = map[string]bool{adminCaller: true}
	for _, caller := range callers {
		s.approvalRequired[caller] = true
	}
	for caller := range s.approvalRequired {
		metrics.PrebuildAdminUnapprovedCommandSeries(caller)
	}
	s.allowUnapproved = allowUnapproved
	if allowUnapproved {
		logx.Errorf("[mail] AdminAllowUnapproved is on: sends from callers %v without a dual-control approval record are accepted; keep it off outside local development",
			slices.Sorted(maps.Keys(s.approvalRequired)))
	}
}

// actor 取客户端请求的原角色。在线域原账号只认 gate 签名原件,不读 login Redis:入口准入(domainhost.Guard)
// 已把验签原件里的原账号、原修订与角色捕获进 ctx,未签名或缺账号 / 修订的请求在入口就被拒。
// 这里再核一次是业务层自己的一道,不依赖拦截器的装配顺序;返回的 ctx 带着原件,提交点按它复核。
func (s *Server) actor(ctx context.Context) (context.Context, uint64, bool) {
	owned, ok := roleidentity.Original(ctx)
	if !ok || owned.Role.GetPlayerId() == 0 {
		return ctx, 0, false
	}
	return ctx, owned.Role.GetPlayerId(), true
}
func rejection(err error) *base.TipInfoMessage {
	if err == nil {
		return nil
	}
	code := uint32(table.CommonError_kServiceUnavailable)
	switch {
	case errors.Is(err, data.ErrInvalid), errors.Is(err, data.ErrDenied):
		code = uint32(table.CommonError_kInvalidParameter)
	case errors.Is(err, data.ErrNotFound):
		code = uint32(table.MailError_kMailNotFound)
	case errors.Is(err, data.ErrExpired):
		code = uint32(table.MailError_kMailExpired)
	case errors.Is(err, data.ErrBoxFull):
		code = uint32(table.MailError_kMailRecipientBoxFull)
	case errors.Is(err, data.ErrRecipientDeleted):
		code = uint32(table.MailError_kMailRecipientDeleted)
	case errors.Is(err, data.ErrAttachment):
		code = uint32(table.MailError_kMailAttachmentInvalid)
	case errors.Is(err, data.ErrConflict):
		code = uint32(table.MailError_kMailRequestConflict)
	case errors.Is(err, data.ErrProtected):
		code = uint32(table.MailError_kMailAttachmentProtected)
	case errors.Is(err, assetop.ErrTooManyPending):
		code = uint32(table.MailError_kMailClaimPending)
	case errors.Is(err, rolefence.ErrUnavailable):
		// 自愈读不到目录:事务已回滚,没有结局未知。回故障 tip 的话排空证明会被打成 Hold(规格 §3.6)。目录里没有这名角色在 runGuarded 里已转成 ErrDenied。
		logx.Infof("[mail] role fence authority unavailable: %v", err)
		code = uint32(table.CommonError_kFeatureUnavailable)
	}
	if code == uint32(table.CommonError_kServiceUnavailable) {
		logx.Error("[mail] authoritative mail request failed")
	}
	return &base.TipInfoMessage{Id: code}
}
func (s *Server) caller(ctx context.Context, method string, in proto.Message) (string, error) {
	caller, err := s.auth.Verify(ctx, method, in)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "内部调用无权访问")
	}
	return caller, nil
}
func (s *Server) ListMails(ctx context.Context, in *mailpb.ListMailsRequest) (*mailpb.ListMailsResponse, error) {
	ctx, player, ok := s.actor(ctx)
	if !ok {
		return &mailpb.ListMailsResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	result, err := s.repo.List(ctx, player, in)
	if err != nil {
		return &mailpb.ListMailsResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) ReadMail(ctx context.Context, in *mailpb.ReadMailRequest) (*mailpb.ReadMailResponse, error) {
	ctx, player, ok := s.actor(ctx)
	if !ok {
		return &mailpb.ReadMailResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	view, err := s.repo.Read(ctx, player, in.GetMailId())
	return &mailpb.ReadMailResponse{ErrorMessage: rejection(err), Mail: view}, nil
}
func (s *Server) ClaimMailAttachments(ctx context.Context, in *mailpb.ClaimMailAttachmentsRequest) (*mailpb.ClaimMailAttachmentsResponse, error) {
	ctx, player, ok := s.actor(ctx)
	if !ok || len(in.GetMailIds()) == 0 || len(in.GetMailIds()) > 100 {
		return &mailpb.ClaimMailAttachmentsResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	result := &mailpb.ClaimMailAttachmentsResponse{}
	seen := map[uint64]bool{}
	for _, id := range in.MailIds {
		if seen[id] {
			continue
		}
		seen[id] = true
		action, err := s.repo.Claim(ctx, player, id)
		if err != nil {
			action = &mailpb.MailActionResult{MailId: id, ErrorMessage: rejection(err)}
		}
		result.Results = append(result.Results, action)
		if ctx.Err() != nil {
			break
		}
	}
	return result, nil
}
func (s *Server) DeleteMails(ctx context.Context, in *mailpb.DeleteMailsRequest) (*mailpb.DeleteMailsResponse, error) {
	ctx, player, ok := s.actor(ctx)
	if !ok {
		return &mailpb.DeleteMailsResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	deleted, protected, err := s.repo.Delete(ctx, player, in.GetMailIds(), false)
	return &mailpb.DeleteMailsResponse{ErrorMessage: rejection(err), DeletedMailIds: deleted, ProtectedMailIds: protected}, nil
}
func (s *Server) ClearReadMails(ctx context.Context, _ *mailpb.ClearReadMailsRequest) (*mailpb.ClearReadMailsResponse, error) {
	ctx, player, ok := s.actor(ctx)
	if !ok {
		return &mailpb.ClearReadMailsResponse{ErrorMessage: rejection(data.ErrInvalid)}, nil
	}
	deleted, protected, err := s.repo.Delete(ctx, player, nil, true)
	return &mailpb.ClearReadMailsResponse{ErrorMessage: rejection(err), DeletedMailIds: deleted, ProtectedMailIds: protected}, nil
}
func (s *Server) send(ctx context.Context, method string, kind mailpb.MailKind, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	methodLabel := metrics.MethodSendPersonal
	if kind == mailpb.MailKind_MAIL_KIND_SYSTEM {
		methodLabel = metrics.MethodSendSystem
	}
	if kind == mailpb.MailKind_MAIL_KIND_GUILD {
		methodLabel = metrics.MethodSendGuild
	}
	caller, err := s.caller(ctx, method, in)
	if err != nil {
		metrics.ObserveAdminSend(methodLabel, metrics.SenderUnknown, metrics.OutcomeDenied)
		return nil, err
	}
	sender := metrics.SenderUnknown
	switch caller {
	case "admin":
		sender = metrics.SenderOpsTool
	case "rank":
		sender = metrics.SenderRank
	case "guild":
		sender = metrics.SenderGuild
	case "trade":
		sender = metrics.SenderTrade
	case "battle":
		sender = metrics.SenderBattle
	case "match":
		sender = metrics.SenderMatch
	}
	if err := s.admitApproval(ctx, caller, method, methodLabel, in); err != nil {
		metrics.ObserveAdminSend(methodLabel, sender, metrics.OutcomeDenied)
		return nil, err
	}
	result, err := s.repo.Send(ctx, caller, kind, in)
	if err != nil {
		tip := rejection(err)
		outcome := metrics.OutcomeRejected
		if tip.GetId() == uint32(table.CommonError_kServiceUnavailable) {
			outcome = metrics.OutcomeFault
		}
		metrics.ObserveAdminSend(methodLabel, sender, outcome)
		return &mailpb.SendMailResponse{ErrorMessage: tip}, nil
	}
	outcome := metrics.OutcomeOK
	if result.Duplicated {
		outcome = metrics.OutcomeDuplicated
	}
	metrics.ObserveAdminSend(methodLabel, sender, outcome)
	// Repo.Send已经提交；提示只触发客户端重拉，不参与发件成功或领取结局。
	if kind == mailpb.MailKind_MAIL_KIND_PERSONAL && !result.Duplicated && s.reminder != nil {
		s.reminder.Personal(ctx, in.RecipientPlayerId, result.MailId)
	}
	return result, nil
}

// admitApproval 核对发件带的双人复核批准记录,在验过调用证明之后、任何读库写库(含判重)之前做。
// 拒绝回 PermissionDenied,后台把它当确定的拒绝;错误文本只点出哪一项不合格,不回显请求内容。
//   - 带了记录就必须合格,不论调用方、不论 AdminAllowUnapproved:批准号就是请求号(后台流水号同时是这里的判重键,
//     一次批准只发一次信);发件请求没有操作人字段,不比发起人;
//   - 不带记录:表外调用方(rank / guild / battle / match 这些系统发件方,不经后台)照常;表内调用方只有
//     AdminAllowUnapproved 打开才接受,每一次都打 ERROR 并计入 mail_admin_unapproved_command_total。
func (s *Server) admitApproval(ctx context.Context, caller, method, methodLabel string, in *mailpb.SendMailRequest) error {
	if approval := in.GetApproval(); approval != nil {
		// 发件请求没有操作人字段:显式传发起人,两人记在批准记录里。
		if err := dualcontrol.RequireForRequest(approval, approval.GetProposer(), in.GetRequestId()); err != nil {
			logx.WithContext(ctx).Errorf("[mail] Rejecting %s from caller %s request_id=%d: %v", method, caller, in.GetRequestId(), err)
			return status.Error(codes.PermissionDenied, err.Error())
		}
		return nil
	}
	if !s.approvalRequired[caller] {
		return nil
	}
	if !s.allowUnapproved {
		logx.WithContext(ctx).Errorf("[mail] Rejecting %s from caller %s request_id=%d: %v", method, caller, in.GetRequestId(), dualcontrol.ErrMissing)
		return status.Error(codes.PermissionDenied, dualcontrol.ErrMissing.Error())
	}
	metrics.ObserveAdminUnapprovedCommand(caller, methodLabel)
	logx.WithContext(ctx).Errorf("[mail] %s accepted without a dual-control approval record caller=%s request_id=%d (AdminAllowUnapproved is on)",
		method, caller, in.GetRequestId())
	return nil
}
func (s *Server) SendPersonalMail(ctx context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return s.send(ctx, mailpb.MailAdmin_SendPersonalMail_FullMethodName, mailpb.MailKind_MAIL_KIND_PERSONAL, in)
}
func (s *Server) SendSystemMail(ctx context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return s.send(ctx, mailpb.MailAdmin_SendSystemMail_FullMethodName, mailpb.MailKind_MAIL_KIND_SYSTEM, in)
}
func (s *Server) SendGuildMail(ctx context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return s.send(ctx, mailpb.MailAdmin_SendGuildMail_FullMethodName, mailpb.MailKind_MAIL_KIND_GUILD, in)
}

// ImportMailRecipients 与 SealMailCampaign 不要求双人复核批准记录:它们只能作用在一个已经建好的活动上(Repo.Import / Seal
// 要求活动存在、producer 就是本调用方),而建活动的那份发件请求已经过 admitApproval(admin 建的必带合格的批准记录)。
// 标题、正文、附件、过期时刻、人数、片数与名单摘要都在建活动时冻结;导入只往这个活动里填人,封存时按片摘要重算的名单摘要、
// 片数与人数必须与冻结值逐一相等才发布。导入了别的名单只会让这个活动封存不了,发不出批准之外的东西。
func (s *Server) ImportMailRecipients(ctx context.Context, in *mailpb.ImportMailRecipientsRequest) (*mailpb.ImportMailRecipientsResponse, error) {
	caller, err := s.caller(ctx, mailpb.MailAdmin_ImportMailRecipients_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Import(ctx, caller, in)
	if err != nil {
		return &mailpb.ImportMailRecipientsResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
func (s *Server) SealMailCampaign(ctx context.Context, in *mailpb.SealMailCampaignRequest) (*mailpb.SendMailResponse, error) {
	caller, err := s.caller(ctx, mailpb.MailAdmin_SealMailCampaign_FullMethodName, in)
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Seal(ctx, caller, in)
	if err != nil {
		return &mailpb.SendMailResponse{ErrorMessage: rejection(err)}, nil
	}
	return result, nil
}
