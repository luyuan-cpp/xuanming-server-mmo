package logic

import (
	"context"
	"errors"
	"unicode"
	"unicode/utf8"

	"match/internal/authority"
	"match/internal/battlescript"
	"match/internal/config"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 审核入口的参数口径(docs/design/battle-script.md §6.3)
const (
	maxPendingBattleScriptsPage      = 50
	maxBattleScriptReviewerBytes     = 64
	maxBattleScriptReviewReasonBytes = 191
)

// BattleScriptReviewLogic 是运营审核玩家剧本的两条内部协议(调用方只有运营工具 battlescriptreview):
//   - 准入与 SetSpectateFeatured 同一把运营密钥(serviceauth 调用方 ops):带会话(客户端来源)、入口未开启、
//     验签失败一律带内 DENIED,拒绝发生在任何读写之前;
//   - 审核按 (script_id, revision, 待审) 做 CAS:作者改了、删了、或已被别的审核员审过,一律 STALE,不会审错版本;
//   - 通过前按当前配表再校验一遍正文,配表改了以后已不合法的版本不能通过(回 INVALID,只能驳回);
//   - 审核流水与状态变更同一事务写入(authority.ReviewBattleScript)。
type BattleScriptReviewLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBattleScriptReviewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BattleScriptReviewLogic {
	return &BattleScriptReviewLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

func (l *BattleScriptReviewLogic) callerAllowed(method string, in proto.Message) bool {
	if details, client := ctxkeys.GetSessionDetails(l.ctx); client && details != nil {
		return false
	}
	if l.svcCtx == nil || l.svcCtx.SpectateOpsAuth == nil {
		return false
	}
	caller, err := l.svcCtx.SpectateOpsAuth.Verify(l.ctx, method, in)
	return err == nil && caller == config.SpectateOpsCaller
}

// ListPendingBattleScripts 列一页待审剧本,先提交的在前。只读,读失败可直接重试
func (l *BattleScriptReviewLogic) ListPendingBattleScripts(in *matchpb.ListPendingBattleScriptsRequest) (*matchpb.ListPendingBattleScriptsResponse, error) {
	if !l.callerAllowed(matchpb.MatchInternal_ListPendingBattleScripts_FullMethodName, in) {
		l.Errorf("[battle_script] ListPendingBattleScripts rejected: caller proof is invalid or the ops entry is disabled")
		metrics.ObserveBattleScriptRequest("review_list", "denied")
		return &matchpb.ListPendingBattleScriptsResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_DENIED}, nil
	}
	if in.GetLimit() < 1 || in.GetLimit() > maxPendingBattleScriptsPage {
		metrics.ObserveBattleScriptRequest("review_list", "rejected")
		return &matchpb.ListPendingBattleScriptsResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INVALID}, nil
	}
	records, err := l.svcCtx.Authority.PendingBattleScripts(l.ctx, in.GetLimit())
	if err != nil {
		l.Errorf("[battle_script] ListPendingBattleScripts read failed: %v", err)
		metrics.ObserveBattleScriptRequest("review_list", "error")
		return &matchpb.ListPendingBattleScriptsResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INTERNAL}, nil
	}
	resp := &matchpb.ListPendingBattleScriptsResponse{}
	for _, record := range records {
		content := &battlepb.BattleScript{}
		if err = proto.Unmarshal(record.GetContent(), content); err != nil {
			l.Errorf("[battle_script] ListPendingBattleScripts content unreadable script=%d revision=%d: %v", record.GetScriptId(), record.GetRevision(), err)
			metrics.ObserveBattleScriptRequest("review_list", "error")
			return &matchpb.ListPendingBattleScriptsResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INTERNAL}, nil
		}
		resp.Scripts = append(resp.Scripts, &matchpb.PendingBattleScript{
			ScriptId: record.GetScriptId(), OwnerPlayerId: record.GetOwnerPlayerId(), Revision: record.GetRevision(),
			Content: content, SubmittedMs: record.GetSubmittedMs(), ApprovedRevision: record.GetApprovedRevision(),
		})
	}
	metrics.ObserveBattleScriptRequest("review_list", "ok")
	return resp, nil
}

// ReviewBattleScript 通过或驳回一版待审剧本
func (l *BattleScriptReviewLogic) ReviewBattleScript(in *matchpb.ReviewBattleScriptRequest) (*matchpb.ReviewBattleScriptResponse, error) {
	if !l.callerAllowed(matchpb.MatchInternal_ReviewBattleScript_FullMethodName, in) {
		l.Errorf("[battle_script] ReviewBattleScript rejected script=%d: caller proof is invalid or the ops entry is disabled", in.GetScriptId())
		metrics.ObserveBattleScriptRequest("review", "denied")
		return &matchpb.ReviewBattleScriptResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_DENIED}, nil
	}
	if !validReviewText(in.GetReviewer(), 1, maxBattleScriptReviewerBytes) ||
		!validReviewText(in.GetReason(), 0, maxBattleScriptReviewReasonBytes) ||
		in.GetScriptId() == 0 || in.GetRevision() == 0 || (!in.GetApprove() && in.GetReason() == "") {
		metrics.ObserveBattleScriptRequest("review", "rejected")
		return &matchpb.ReviewBattleScriptResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INVALID}, nil
	}
	review := authority.BattleScriptReview{ScriptID: in.GetScriptId(), Revision: in.GetRevision(), Approve: in.GetApprove(),
		Reviewer: in.GetReviewer(), Reason: in.GetReason(), Validate: validateApprovedContent}
	err := l.svcCtx.Authority.ReviewBattleScript(l.ctx, review)
	switch {
	case errors.Is(err, authority.ErrBattleScriptReviewStale):
		metrics.ObserveBattleScriptRequest("review", "rejected")
		return &matchpb.ReviewBattleScriptResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_STALE}, nil
	case errors.Is(err, authority.ErrBattleScriptReviewInvalid):
		l.Errorf("[battle_script] ReviewBattleScript approval refused script=%d revision=%d: content no longer validates against current tables", in.GetScriptId(), in.GetRevision())
		metrics.ObserveBattleScriptRequest("review", "rejected")
		return &matchpb.ReviewBattleScriptResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INVALID}, nil
	case err != nil:
		l.Errorf("[battle_script] ReviewBattleScript write failed script=%d revision=%d: %v", in.GetScriptId(), in.GetRevision(), err)
		metrics.ObserveBattleScriptRequest("review", "error")
		return &matchpb.ReviewBattleScriptResponse{Reject: matchpb.BattleScriptReviewReject_BATTLE_SCRIPT_REVIEW_REJECT_INTERNAL}, nil
	}
	l.Infof("[battle_script] Reviewed script=%d revision=%d approved=%t reviewer=%q", in.GetScriptId(), in.GetRevision(), in.GetApprove(), in.GetReviewer())
	metrics.ObserveBattleScriptRequest("review", "ok")
	return &matchpb.ReviewBattleScriptResponse{}, nil
}

// validateApprovedContent 在审核事务里对要通过的那一版正文按当前配表再校验(纯函数,不调 RPC)
func validateApprovedContent(content []byte) error {
	script := &battlepb.BattleScript{}
	if err := proto.Unmarshal(content, script); err != nil {
		return err
	}
	normalized, err := battlescript.Normalize(script, battlescript.TableLookup{})
	if err != nil {
		return err
	}
	return battlescript.CheckSide(normalized, battlepb.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER, battlescript.TableLookup{})
}

// validReviewText 是审核员名与原因的字符口径:合法 UTF-8、长度在区间内、不含控制字符(进审计行与日志不用转义)
func validReviewText(text string, minBytes, maxBytes int) bool {
	if len(text) < minBytes || len(text) > maxBytes || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
