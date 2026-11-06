package logic

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"match/internal/pkg/ctxkeys"
	pb "proto/match"
	"shared/serviceauth"
)

// scene 冻结到期时的 SQL 终局只读查询(MatchInternal.GetBattleTerminalDecision,2026-11-06,
// 数据覆写审计 R2 r2-v1#1 残余窗口;docs/design/turn-battle-gap-closure.md §11)。
//
// 为什么要有它:COMPLETE 不受截止约束(终局宽限只推迟 ABORT),Match 发件箱积压时结算可能晚于 scene 的冻结到期才到。
// scene 到期先问这里:COMPLETE 而本地还没应用、或有可裁决原件而终局还没落定(terminal_expected),就续冻;
// ABORT、或这一局根本没有可裁决的原件,照常解冻;问不到按未知续冻一拍(都有总上限)。
//
// 契约:
//   - 只读 match_battle_completion(终局唯一权威,readTerminal 同一条 SQL)与成局行(只为判原件可不可裁决,
//     authority.Store.BattleTerminalState),不读发件箱投影,不加锁、不写任何行;
//   - 鉴权与回包签名沿用 CommitBattleOutcome 那一套:请求由固定调用方 "scene" 签(与 TeamAuthority.ReadMembership
//     同一把场景密钥),回包由 "match" 以方法 + "/reply" 签,request_digest 绑定请求原件;
//   - 读失败与非法持久值一律带内 retry = true,不回 gRPC 错误:只读查询没有结局未知,回 Unavailable 会被发布准入
//     记成未知结局、拖住排空证明(serverbase.PublicationAdmission)。拒绝只用 PermissionDenied / InvalidArgument,
//     两者都是确定拒绝(没有任何写入)。

// sceneTerminalCaller 是唯一放行的调用方。
const sceneTerminalCaller = "scene"

// terminalDecisionReadBudget 是单次 SQL 读的预算。scene 的单次调用期限是 6 秒(PlayerBattleSystem::kTerminalQueryDeadlineMs),
// 服务端 zrpc Timeout 4 秒;读超出预算按 retry 带内回,scene 下一拍再问。
const terminalDecisionReadBudget = 2 * time.Second

// battleTerminalReader 是查询对成局权威的全部依赖,生产实现是 *authority.Store(BattleTerminalState 只读 SQL 终局行
// 与成局行)。收窄成接口只为测试隔离:验签、带内 retry、非法持久值、有无原件这几条不起 TiDB 也能确定性复现。
type battleTerminalReader interface {
	BattleTerminalState(ctx context.Context, battle uint64) (decision pb.BattleOutcomeDecision, terminalExpected bool, err error)
}

// BattleTerminalDecisionQuery 持有场景密钥的验签器与签名密钥。启动时装配一次,之后只读,可并发调用。
type BattleTerminalDecisionQuery struct {
	store    battleTerminalReader
	verifier *serviceauth.Verifier
	secret   string
	now      func() time.Time
}

// NewBattleTerminalDecisionQuery 用 scene 的场景密钥装配查询。密钥缺失或不足 32 字节返回错误,调用方据此保持未装配:
// 未装配的入口一律 PermissionDenied(fail-closed),scene 那边按「未知」续冻,到上限照现行规则解冻并告警。
func NewBattleTerminalDecisionQuery(store battleTerminalReader, sceneSecret string) (*BattleTerminalDecisionQuery, error) {
	if store == nil {
		return nil, errors.New("match: 终局只读查询缺少成局权威")
	}
	secret := strings.TrimSpace(sceneSecret)
	verifier, err := serviceauth.New(map[string]string{sceneTerminalCaller: secret})
	if err != nil {
		return nil, errors.New("match: 场景终局查询签名密钥缺失或不足32字节")
	}
	return &BattleTerminalDecisionQuery{store: store, verifier: verifier, secret: secret, now: time.Now}, nil
}

// GetBattleTerminalDecision 回 battle_id 这一局的 SQL 终局。q 为 nil(未装配)时拒绝。
func (q *BattleTerminalDecisionQuery) GetBattleTerminalDecision(ctx context.Context, in *pb.GetBattleTerminalDecisionRequest) (*pb.GetBattleTerminalDecisionResponse, error) {
	if details, client := ctxkeys.GetSessionDetails(ctx); client && details != nil {
		return nil, status.Error(codes.PermissionDenied, "客户端不能查询内部战斗终局")
	}
	if q == nil || q.verifier == nil {
		logx.WithContext(ctx).Error("[match] Rejecting GetBattleTerminalDecision: scene call secret is not configured")
		return nil, status.Error(codes.PermissionDenied, "场景终局查询签名未装配")
	}
	method := pb.MatchInternal_GetBattleTerminalDecision_FullMethodName
	caller, err := q.verifier.Verify(ctx, method, in)
	if err != nil || caller != sceneTerminalCaller {
		logx.WithContext(ctx).Errorf("[match] Rejecting GetBattleTerminalDecision: scene call proof is invalid battle_id=%d", in.GetBattleId())
		return nil, status.Error(codes.PermissionDenied, "场景终局查询证明无效")
	}
	if in.GetBattleId() == 0 {
		return nil, status.Error(codes.InvalidArgument, "终局查询缺少对局编号")
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "终局查询原件无效")
	}
	out := &pb.GetBattleTerminalDecisionResponse{RequestDigest: digest}
	out.Decision, out.TerminalExpected, out.Retry = q.readDecision(ctx, in.GetBattleId())
	if err = serviceauth.Sign(method+"/reply", out, "match", q.secret, uint64(q.now().UnixMilli())); err != nil {
		return nil, status.Error(codes.Internal, "终局查询回包无法签名")
	}
	return out, nil
}

// readDecision 读 SQL 终局与「终局会不会落定」。读失败或读到非法持久值 = retry(此时另两项无意义,回零值)。
func (q *BattleTerminalDecisionQuery) readDecision(ctx context.Context, battleID uint64) (decision pb.BattleOutcomeDecision, terminalExpected, retry bool) {
	readCtx, cancel := context.WithTimeout(ctx, terminalDecisionReadBudget)
	defer cancel()
	decision, terminalExpected, err := q.store.BattleTerminalState(readCtx, battleID)
	if err != nil {
		logx.WithContext(ctx).Errorf("[match] Battle terminal read for scene failed battle_id=%d: %v", battleID, err)
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, false, true
	}
	switch decision {
	case pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN,
		pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE,
		pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT:
		return decision, terminalExpected, false
	default:
		// 终局行只会由 CommitBattleOutcome(COMPLETE)、ABORT 的几个入口与 ArchiveCompletion(无决定)写入;别的值是数据损坏,
		// 不能替它猜结论。按 retry 回,scene 续冻到上限再告警解冻。
		logx.WithContext(ctx).Errorf("[match] Battle terminal row holds an invalid decision battle_id=%d decision=%d", battleID, decision)
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, false, true
	}
}
