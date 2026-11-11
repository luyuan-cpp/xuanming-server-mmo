package main

// 战报回放冒烟(docs/design/battle-report-replay.md §17 第 5 步):一局 PVP 打完之后,参战机器人经大厅连接
//   ListBattleReports → 本人列表里有这一局、has_replay = true、本人在参战者里;
//   GetBattleReport  → 外壳格式 1、gzip 解压长度等于 raw_bytes、解析出 BattleReplay、battle_id 一致、
//                       开战状态至少两个单位、没截断时回合数等于 total_rounds。
// 由 battle-smoke 的 cross_zone 子模式(1V1)在收齐双方终局之后调用;PVE 局不录回放,不在这里核对。
//
// 时序:列表行与回放正文和终局在 match 同一个事务里提交(设计 BR-4),而个人 BattleEnd 由 scene 在结算应用、
// SQL 确认之后才推,所以 WaitBattleEnd 返回时战报必然已在库里,不需要轮询。
// 限频:两条消息各有 MessageLimiter 额度(GetBattleReport 每秒 1 次、ListBattleReports 每 2 秒 1 次),每个机器人各调一次。

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/battle"
	"proto/match"
	"robot/generated/pb/game"
	"robot/metrics"
)

// battleReportSmokeTimeout 是单次战报查询的等待预算:大于路由服 ForwardTimeoutMs(5s)。
const battleReportSmokeTimeout = 8 * time.Second

// battleReplayMaxRawBytes 与服务端录制上限同值(kMaxBattleReplayRawBytes / maxBattleReplayRawBytes)。
const battleReplayMaxRawBytes = 1 << 20

// battleReportSmokeResult 是一侧核对通过后的摘要,只用于结果日志。
type battleReportSmokeResult struct {
	listed      int    // 本人列表条数
	rounds      int    // 回放里的回合帧数
	totalRounds uint32 // 引擎结算过的回合数
	truncated   bool
	rawBytes    uint32
	payload     int
	endReason   battle.EBattleReplayEndReason
}

// battleReportSmokeCheck 核对一名参战机器人能看到 battleId 这一局的战报与回放。任一项不符返回 error。
func battleReportSmokeCheck(bot *battleSmokeBot, stats *metrics.Stats, battleId uint64) (battleReportSmokeResult, error) {
	caller := newSmokeCaller(bot.gc, stats, bot.account, battleReportSmokeTimeout, nil)

	list := &match.ListBattleReportsResponse{}
	envelopeTip, err := caller.call(game.MatchServiceListBattleReportsMessageId, &match.ListBattleReportsRequest{}, list)
	if err != nil {
		return battleReportSmokeResult{}, fmt.Errorf("ListBattleReports: %w", err)
	}
	if envelopeTip != 0 {
		return battleReportSmokeResult{}, fmt.Errorf("ListBattleReports envelope rejected tip=%d", envelopeTip)
	}
	if tip := list.GetErrorMessage().GetId(); tip != 0 {
		return battleReportSmokeResult{}, fmt.Errorf("ListBattleReports rejected tip=%d params=%v", tip, list.GetErrorMessage().GetParameters())
	}
	var summary *match.BattleReportSummary
	for _, report := range list.GetReports() {
		if report.GetBattleId() == battleId {
			summary = report
			break
		}
	}
	if summary == nil {
		return battleReportSmokeResult{}, fmt.Errorf("battle_id=%d missing from the player's report list (%d entries)", battleId, len(list.GetReports()))
	}
	if !summary.GetHasReplay() {
		return battleReportSmokeResult{}, fmt.Errorf("battle_id=%d listed without replay (has_replay=false)", battleId)
	}
	selfListed := false
	for _, member := range summary.GetMembers() {
		if member.GetPlayerId() == bot.gc.PlayerId {
			selfListed = true
			break
		}
	}
	if !selfListed {
		return battleReportSmokeResult{}, fmt.Errorf("battle_id=%d summary does not list player_id=%d among %d members",
			battleId, bot.gc.PlayerId, len(summary.GetMembers()))
	}

	got := &match.GetBattleReportResponse{}
	envelopeTip, err = caller.call(game.MatchServiceGetBattleReportMessageId, &match.GetBattleReportRequest{BattleId: battleId}, got)
	if err != nil {
		return battleReportSmokeResult{}, fmt.Errorf("GetBattleReport: %w", err)
	}
	if envelopeTip != 0 {
		return battleReportSmokeResult{}, fmt.Errorf("GetBattleReport envelope rejected tip=%d", envelopeTip)
	}
	if tip := got.GetErrorMessage().GetId(); tip != 0 {
		return battleReportSmokeResult{}, fmt.Errorf("GetBattleReport rejected tip=%d params=%v", tip, got.GetErrorMessage().GetParameters())
	}
	replay, err := decodeBattleReplay(got.GetReplay())
	if err != nil {
		return battleReportSmokeResult{}, fmt.Errorf("decode replay of battle_id=%d: %w", battleId, err)
	}
	if replay.GetBattleId() != battleId {
		return battleReportSmokeResult{}, fmt.Errorf("replay battle_id=%d, requested %d", replay.GetBattleId(), battleId)
	}
	if n := len(replay.GetStartState().GetActors()); n < 2 {
		return battleReportSmokeResult{}, fmt.Errorf("replay start_state has %d actors, expected >= 2", n)
	}
	if !replay.GetTruncated() && len(replay.GetRounds()) != int(replay.GetTotalRounds()) {
		return battleReportSmokeResult{}, fmt.Errorf("replay has %d round frames but total_rounds=%d without truncation",
			len(replay.GetRounds()), replay.GetTotalRounds())
	}
	switch replay.GetEndReason() {
	case battle.EBattleReplayEndReason_BATTLE_REPLAY_END_FINISHED, battle.EBattleReplayEndReason_BATTLE_REPLAY_END_DEADLINE_DRAW:
	default:
		return battleReportSmokeResult{}, fmt.Errorf("replay end_reason=%s", replay.GetEndReason())
	}
	if replay.GetOutcome() != summary.GetOutcome() {
		return battleReportSmokeResult{}, fmt.Errorf("replay outcome=%s but list says %s", replay.GetOutcome(), summary.GetOutcome())
	}
	result := battleReportSmokeResult{
		listed:      len(list.GetReports()),
		rounds:      len(replay.GetRounds()),
		totalRounds: replay.GetTotalRounds(),
		truncated:   replay.GetTruncated(),
		rawBytes:    got.GetReplay().GetRawBytes(),
		payload:     len(got.GetReplay().GetPayload()),
		endReason:   replay.GetEndReason(),
	}
	zap.L().Info("[battle-report] replay verified",
		zap.String("account", bot.account), zap.Uint64("battle_id", battleId),
		zap.Int("listed", result.listed), zap.Int("rounds", result.rounds), zap.Uint32("total_rounds", result.totalRounds),
		zap.Bool("truncated", result.truncated), zap.Uint32("raw_bytes", result.rawBytes), zap.Int("payload_bytes", result.payload),
		zap.String("end_reason", result.endReason.String()))
	return result, nil
}

// decodeBattleReplay 按客户端交接(docs/handoff/battle-report-replay-client.md §4)的同一口径解码外壳:
// 只认格式 1;gzip 解压时最多读 min(raw_bytes, 1 MiB) + 1 字节(防解压炸弹),解出长度必须等于 raw_bytes。
func decodeBattleReplay(envelope *battle.BattleReplayEnvelope) (*battle.BattleReplay, error) {
	if envelope == nil {
		return nil, fmt.Errorf("empty replay envelope")
	}
	if envelope.GetFormatVersion() != 1 {
		return nil, fmt.Errorf("unknown format_version=%d", envelope.GetFormatVersion())
	}
	limit := int64(envelope.GetRawBytes())
	if limit > battleReplayMaxRawBytes {
		limit = battleReplayMaxRawBytes
	}
	var raw []byte
	switch envelope.GetCodec() {
	case battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP:
		reader, err := gzip.NewReader(bytes.NewReader(envelope.GetPayload()))
		if err != nil {
			return nil, fmt.Errorf("gzip header: %w", err)
		}
		defer reader.Close()
		// 读到 EOF 时 gzip.Reader 才核 CRC32 与长度;多读 1 字节用来发现超出上限。
		raw, err = io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return nil, fmt.Errorf("gzip body: %w", err)
		}
	case battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_NONE:
		raw = envelope.GetPayload()
	default:
		return nil, fmt.Errorf("unknown codec=%s", envelope.GetCodec())
	}
	if int64(len(raw)) != int64(envelope.GetRawBytes()) || int64(len(raw)) > battleReplayMaxRawBytes {
		return nil, fmt.Errorf("decoded %d bytes, envelope raw_bytes=%d (limit %d)", len(raw), envelope.GetRawBytes(), battleReplayMaxRawBytes)
	}
	replay := &battle.BattleReplay{}
	if err := proto.Unmarshal(raw, replay); err != nil {
		return nil, fmt.Errorf("parse BattleReplay: %w", err)
	}
	return replay, nil
}
