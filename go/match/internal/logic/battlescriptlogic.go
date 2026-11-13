package logic

import (
	"context"
	"errors"

	"match/internal/authority"
	"match/internal/battlescript"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 战斗剧本客户端协议的容量口径(docs/design/battle-script.md §7)
const (
	maxBattleScriptsPerPlayer     = 10
	maxBattleScriptLoadoutEntries = 32
	// maxPlayerBattleScriptBytes 是单份玩家剧本序列化后的上限:gate 对整个 ClientRequest 有 1KB 的硬上限
	// (client_session_handler.cpp CheckMessageSize,超限还计非法包),扣掉外层信封与请求里的其它字段后留 900 字节
	maxPlayerBattleScriptBytes = 900
)

// BattleScriptLogic 是战斗剧本的四条客户端协议:
//   - player_id 只取 gate 注入的会话身份,请求体里没有 player_id;没有会话身份即不可信来源,fail-closed;
//   - 校验在写库之前做完(battlescript.Normalize + 玩家方 CheckSide),库里只存合法、规范化后的正文;
//   - 写入都锁本人的 role_fence 并在锁内复核上限与版本(authority/battle_script.go),预检不代替提交点校验;
//   - 新建剧本的 id 在事务外向发号器领(与 battle_id 同一号段),领不到就整体失败;
//   - 限频由 gate 的按消息限频负责(MessageLimiter 缺省额度),这里不另做。
type BattleScriptLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBattleScriptLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BattleScriptLogic {
	return &BattleScriptLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

func (l *BattleScriptLogic) sessionPlayer(rpc string) (uint64, bool) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[battle_script] %s missing session identity", rpc)
		metrics.ObserveBattleScriptRequest(rpc, "error")
		return 0, false
	}
	return detail.GetPlayerId(), true
}

// ListBattleScripts 回官方剧本、本人的剧本与本人的选用。只读
func (l *BattleScriptLogic) ListBattleScripts(_ *matchpb.ListBattleScriptsRequest) (*matchpb.ListBattleScriptsResponse, error) {
	player, ok := l.sessionPlayer("list")
	if !ok {
		return &matchpb.ListBattleScriptsResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	failed := func(err error) (*matchpb.ListBattleScriptsResponse, error) {
		l.Errorf("[battle_script] ListBattleScripts read failed player=%d: %v", player, err)
		metrics.ObserveBattleScriptRequest("list", "error")
		return &matchpb.ListBattleScriptsResponse{ErrorMessage: tipErr(constants.ErrInternal, "剧本列表暂时读不到")}, nil
	}
	records, err := l.svcCtx.Authority.BattleScriptsOf(l.ctx, player)
	if err != nil {
		return failed(err)
	}
	loadout, err := l.svcCtx.Authority.BattleScriptLoadout(l.ctx, player)
	if err != nil {
		return failed(err)
	}
	resp := &matchpb.ListBattleScriptsResponse{
		MaxScripts:     maxBattleScriptsPerPlayer,
		MaxScriptBytes: maxPlayerBattleScriptBytes,
		Loadout:        loadoutEntriesOf(loadout),
	}
	if l.svcCtx.BattleScripts != nil {
		for _, script := range l.svcCtx.BattleScripts.OfficialScripts() {
			resp.OfficialScripts = append(resp.OfficialScripts, proto.Clone(script).(*battlepb.BattleScript))
		}
	}
	for _, record := range records {
		view, err := scriptViewOf(record)
		if err != nil {
			return failed(err)
		}
		resp.MyScripts = append(resp.MyScripts, view)
	}
	metrics.ObserveBattleScriptRequest("list", "ok")
	return resp, nil
}

// SubmitBattleScript 新建或改写本人的剧本:校验通过存为新版本、置为待审
func (l *BattleScriptLogic) SubmitBattleScript(in *matchpb.SubmitBattleScriptRequest) (*matchpb.SubmitBattleScriptResponse, error) {
	player, ok := l.sessionPlayer("submit")
	if !ok {
		return &matchpb.SubmitBattleScriptResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	reject := func(code uint32, text, detail string) (*matchpb.SubmitBattleScriptResponse, error) {
		metrics.ObserveBattleScriptRequest("submit", "rejected")
		return &matchpb.SubmitBattleScriptResponse{ErrorMessage: tipErr(code, text), InvalidDetail: detail}, nil
	}
	if in.GetScript() == nil {
		return reject(constants.ErrBattleScriptInvalid, "剧本为空", "script is missing")
	}
	if in.GetScriptId() == 0 && in.GetExpectedRevision() != 0 {
		return reject(constants.ErrBattleScriptRevisionConflict, "新建剧本不带版本号", "")
	}
	script := proto.Clone(in.GetScript()).(*battlepb.BattleScript)
	script.ScriptId = 0 // 玩家剧本的身份是 script_id 行,正文里的这一格恒为 0
	if proto.Size(script) > maxPlayerBattleScriptBytes {
		return reject(constants.ErrBattleScriptTooLarge, "剧本太长", "")
	}
	normalized, err := battlescript.Normalize(script, battlescript.TableLookup{})
	if err == nil {
		err = battlescript.CheckSide(normalized, battlepb.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER, battlescript.TableLookup{})
	}
	if err != nil {
		return reject(constants.ErrBattleScriptInvalid, "剧本内容不合法", err.Error())
	}
	content, err := proto.MarshalOptions{Deterministic: true}.Marshal(normalized)
	if err != nil {
		l.Errorf("[battle_script] SubmitBattleScript marshal failed player=%d: %v", player, err)
		metrics.ObserveBattleScriptRequest("submit", "error")
		return &matchpb.SubmitBattleScriptResponse{ErrorMessage: tipErr(constants.ErrInternal, "剧本暂时存不了")}, nil
	}

	submit := authority.BattleScriptSubmit{Player: player, ScriptID: in.GetScriptId(), ExpectedRevision: in.GetExpectedRevision(),
		Content: content, MaxScripts: maxBattleScriptsPerPlayer}
	if submit.ScriptID == 0 {
		// 发号是 RPC,只能在事务外;份数已满时这一号作废,号段本就允许跳号
		if submit.NewScriptID, err = l.svcCtx.BattleIDGen.Generate(); err != nil {
			l.Errorf("[battle_script] SubmitBattleScript id mint failed player=%d: %v", player, err)
			metrics.ObserveBattleScriptRequest("submit", "error")
			return &matchpb.SubmitBattleScriptResponse{ErrorMessage: tipErr(constants.ErrInternal, "剧本暂时存不了")}, nil
		}
	}
	record, err := l.svcCtx.Authority.SubmitBattleScript(l.ctx, submit)
	switch {
	case errors.Is(err, authority.ErrBattleScriptLimit):
		return reject(constants.ErrBattleScriptLimitReached, "剧本数量已达上限", "")
	case errors.Is(err, authority.ErrBattleScriptNotFound):
		return reject(constants.ErrBattleScriptNotFound, "剧本不存在", "")
	case errors.Is(err, authority.ErrBattleScriptRevision):
		return reject(constants.ErrBattleScriptRevisionConflict, "剧本已在别处修改", "")
	case err != nil:
		l.Errorf("[battle_script] SubmitBattleScript write failed player=%d script=%d: %v", player, in.GetScriptId(), err)
		metrics.ObserveBattleScriptRequest("submit", "error")
		return &matchpb.SubmitBattleScriptResponse{ErrorMessage: tipErr(constants.ErrInternal, "剧本暂时存不了")}, nil
	}
	view, err := scriptViewOf(record)
	if err != nil {
		l.Errorf("[battle_script] SubmitBattleScript view failed player=%d script=%d: %v", player, record.GetScriptId(), err)
		metrics.ObserveBattleScriptRequest("submit", "error")
		return &matchpb.SubmitBattleScriptResponse{ErrorMessage: tipErr(constants.ErrInternal, "剧本已保存,请刷新查看")}, nil
	}
	l.Infof("[battle_script] Submitted player=%d script=%d revision=%d bytes=%d", player, record.GetScriptId(), record.GetRevision(), len(content))
	metrics.ObserveBattleScriptRequest("submit", "ok")
	return &matchpb.SubmitBattleScriptResponse{Script: view}, nil
}

// DeleteBattleScript 删除本人的剧本,并清掉指向它的选用
func (l *BattleScriptLogic) DeleteBattleScript(in *matchpb.DeleteBattleScriptRequest) (*matchpb.DeleteBattleScriptResponse, error) {
	player, ok := l.sessionPlayer("delete")
	if !ok {
		return &matchpb.DeleteBattleScriptResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	if in.GetScriptId() == 0 {
		metrics.ObserveBattleScriptRequest("delete", "rejected")
		return &matchpb.DeleteBattleScriptResponse{ErrorMessage: tipErr(constants.ErrBattleScriptNotFound, "剧本不存在")}, nil
	}
	err := l.svcCtx.Authority.DeleteBattleScript(l.ctx, player, in.GetScriptId(), in.GetExpectedRevision())
	switch {
	case errors.Is(err, authority.ErrBattleScriptNotFound):
		metrics.ObserveBattleScriptRequest("delete", "rejected")
		return &matchpb.DeleteBattleScriptResponse{ErrorMessage: tipErr(constants.ErrBattleScriptNotFound, "剧本不存在")}, nil
	case errors.Is(err, authority.ErrBattleScriptRevision):
		metrics.ObserveBattleScriptRequest("delete", "rejected")
		return &matchpb.DeleteBattleScriptResponse{ErrorMessage: tipErr(constants.ErrBattleScriptRevisionConflict, "剧本已在别处修改")}, nil
	case err != nil:
		l.Errorf("[battle_script] DeleteBattleScript failed player=%d script=%d: %v", player, in.GetScriptId(), err)
		metrics.ObserveBattleScriptRequest("delete", "error")
		return &matchpb.DeleteBattleScriptResponse{ErrorMessage: tipErr(constants.ErrInternal, "剧本暂时删不了")}, nil
	}
	l.Infof("[battle_script] Deleted player=%d script=%d", player, in.GetScriptId())
	metrics.ObserveBattleScriptRequest("delete", "ok")
	return &matchpb.DeleteBattleScriptResponse{}, nil
}

// SetBattleScriptLoadout 为某种对局选用剧本或清除选用,回改完后的全部选用
func (l *BattleScriptLogic) SetBattleScriptLoadout(in *matchpb.SetBattleScriptLoadoutRequest) (*matchpb.SetBattleScriptLoadoutResponse, error) {
	player, ok := l.sessionPlayer("loadout")
	if !ok {
		return &matchpb.SetBattleScriptLoadoutResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	reject := func(code uint32, text string) (*matchpb.SetBattleScriptLoadoutResponse, error) {
		metrics.ObserveBattleScriptRequest("loadout", "rejected")
		return &matchpb.SetBattleScriptLoadoutResponse{ErrorMessage: tipErr(code, text)}, nil
	}
	choice, code, text := l.loadoutChoice(player, in.GetEntry())
	if code != 0 {
		return reject(code, text)
	}
	list, err := l.svcCtx.Authority.SetBattleScriptLoadout(l.ctx, choice)
	switch {
	case errors.Is(err, authority.ErrBattleScriptNotFound):
		return reject(constants.ErrBattleScriptNotFound, "剧本不存在")
	case errors.Is(err, authority.ErrBattleScriptNotApproved):
		return reject(constants.ErrBattleScriptNotApproved, "剧本还没有通过审核")
	case errors.Is(err, authority.ErrBattleScriptLoadoutFull):
		return reject(constants.ErrBattleScriptLoadoutFull, "选用条目已达上限")
	case err != nil:
		l.Errorf("[battle_script] SetBattleScriptLoadout failed player=%d mode=%d config=%d: %v", player, choice.MatchMode, choice.BattleConfigID, err)
		metrics.ObserveBattleScriptRequest("loadout", "error")
		return &matchpb.SetBattleScriptLoadoutResponse{ErrorMessage: tipErr(constants.ErrInternal, "选用暂时存不了")}, nil
	}
	metrics.ObserveBattleScriptRequest("loadout", "ok")
	return &matchpb.SetBattleScriptLoadoutResponse{Loadout: loadoutEntriesOf(list)}, nil
}

// loadoutChoice 把客户端的选用条目校验成一次写入;不合法回 (_, tip 码, 文案)。
// 官方剧本在这里对照内置目录核对存在、适用于玩家方;玩家剧本的归属与审核状态在写入事务里锁内核对
func (l *BattleScriptLogic) loadoutChoice(player uint64, entry *matchpb.BattleScriptLoadoutEntry) (authority.BattleScriptChoice, uint32, string) {
	choice := authority.BattleScriptChoice{Player: player, MaxEntries: maxBattleScriptLoadoutEntries}
	if entry == nil {
		return choice, constants.ErrBattleScriptLoadoutInvalid, "选用为空"
	}
	choice.MatchMode, choice.BattleConfigID, choice.Origin = entry.GetMatchMode(), entry.GetBattleConfigId(), entry.GetOrigin()
	if !battlescript.IsKnownMatchMode(choice.MatchMode) ||
		(choice.BattleConfigID != 0 && !(battlescript.TableLookup{}).HasDungeon(choice.BattleConfigID)) {
		return choice, constants.ErrBattleScriptLoadoutInvalid, "未知的玩法"
	}
	switch choice.Origin {
	case battlepb.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_UNSPECIFIED:
		return choice, 0, "" // 清除
	case battlepb.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL:
		script, ok := l.officialScript(entry.GetOfficialScriptId())
		if !ok || battlescript.CheckSide(script, battlepb.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER, battlescript.TableLookup{}) != nil {
			return choice, constants.ErrBattleScriptLoadoutInvalid, "官方剧本不存在或不能给玩家用"
		}
		choice.OfficialScriptID = entry.GetOfficialScriptId()
		return choice, 0, ""
	case battlepb.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER:
		if entry.GetPlayerScriptId() == 0 {
			return choice, constants.ErrBattleScriptNotFound, "剧本不存在"
		}
		// 评分局只开官方剧本:明确选评分模式的玩家剧本直接拒绝;「任意模式」的玩家剧本照收,建房时在评分局里跳过它
		if choice.MatchMode != 0 && isRatedMode(matchpb.MatchMode(choice.MatchMode)) {
			return choice, constants.ErrBattleScriptLoadoutInvalid, "评分对局只能选用官方剧本"
		}
		choice.PlayerScriptID = entry.GetPlayerScriptId()
		return choice, 0, ""
	default:
		return choice, constants.ErrBattleScriptLoadoutInvalid, "未知的剧本来源"
	}
}

func (l *BattleScriptLogic) officialScript(id uint32) (*battlepb.BattleScript, bool) {
	if l.svcCtx.BattleScripts == nil {
		return nil, false
	}
	return l.svcCtx.BattleScripts.Official(id)
}

func scriptViewOf(record *matchpb.MatchBattleScriptRecord) (*matchpb.PlayerBattleScriptView, error) {
	content := &battlepb.BattleScript{}
	if err := proto.Unmarshal(record.GetContent(), content); err != nil {
		return nil, err
	}
	return &matchpb.PlayerBattleScriptView{
		ScriptId:         record.GetScriptId(),
		Revision:         record.GetRevision(),
		Content:          content,
		ReviewState:      matchpb.BattleScriptReviewState(record.GetReviewState()),
		ApprovedRevision: record.GetApprovedRevision(),
		RejectReason:     record.GetRejectReason(),
		SubmittedMs:      record.GetSubmittedMs(),
	}, nil
}

func loadoutEntriesOf(records []*matchpb.MatchBattleScriptLoadoutRecord) []*matchpb.BattleScriptLoadoutEntry {
	entries := make([]*matchpb.BattleScriptLoadoutEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, &matchpb.BattleScriptLoadoutEntry{
			MatchMode:        record.GetMatchMode(),
			BattleConfigId:   record.GetBattleConfigId(),
			Origin:           battlepb.EBattleScriptOrigin(record.GetOrigin()),
			OfficialScriptId: record.GetOfficialScriptId(),
			PlayerScriptId:   record.GetPlayerScriptId(),
		})
	}
	return entries
}
