package server

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"mail/internal/data"
	"mail/internal/metrics"
	base "proto/common/base"
	dspb "proto/data_service"
	mailpb "proto/mail"
	"shared/assetop"
	"shared/domainhost"
	table "shared/generated/pb/table"
	"shared/loginsession"
	"shared/rolefence"
	"shared/roleidentity"
	"shared/serviceauth"
)

type Server struct {
	mailpb.UnimplementedClientPlayerMailServer
	mailpb.UnimplementedMailAdminServer
	repo          *data.Repo
	auth          *serviceauth.Verifier
	reminder      Notifier
	loginSessions loginsession.Reader
}
type Notifier interface {
	Personal(context.Context, uint64, uint64)
}

func New(repo *data.Repo, auth *serviceauth.Verifier, reminders ...Notifier) *Server {
	s := &Server{repo: repo, auth: auth}
	if len(reminders) > 0 {
		s.reminder = reminders[0]
	}
	return s
}

// WithLoginSessions只绑定部署已声明的全局登录态只读连接；不授权写登录目录。
func (s *Server) WithLoginSessions(reader loginsession.Reader) *Server {
	s.loginSessions = reader
	return s
}

func (s *Server) actor(ctx context.Context) (context.Context, uint64, bool) {
	session := domainhost.Session(ctx)
	if session.GetPlayerId() == 0 || session.GetOwnerRevision() == 0 {
		return ctx, 0, false
	}
	account, err := loginsession.Account(ctx, s.loginSessions, session)
	if err != nil {
		return ctx, 0, false
	}
	bound, err := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{
		Account: account, OwnerRevision: session.OwnerRevision,
		Role: &base.AccountSimplePlayer{PlayerId: session.PlayerId},
	})
	return bound, session.PlayerId, err == nil
}
func ClientMethods() map[string]bool {
	return map[string]bool{mailpb.ClientPlayerMail_ListMails_FullMethodName: true, mailpb.ClientPlayerMail_ReadMail_FullMethodName: true, mailpb.ClientPlayerMail_ClaimMailAttachments_FullMethodName: true, mailpb.ClientPlayerMail_DeleteMails_FullMethodName: true, mailpb.ClientPlayerMail_ClearReadMails_FullMethodName: true}
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
		logx.Error("[mail] 权威邮件请求失败")
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
func (s *Server) SendPersonalMail(ctx context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return s.send(ctx, mailpb.MailAdmin_SendPersonalMail_FullMethodName, mailpb.MailKind_MAIL_KIND_PERSONAL, in)
}
func (s *Server) SendSystemMail(ctx context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return s.send(ctx, mailpb.MailAdmin_SendSystemMail_FullMethodName, mailpb.MailKind_MAIL_KIND_SYSTEM, in)
}
func (s *Server) SendGuildMail(ctx context.Context, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	return s.send(ctx, mailpb.MailAdmin_SendGuildMail_FullMethodName, mailpb.MailKind_MAIL_KIND_GUILD, in)
}
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
