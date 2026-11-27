package logic

// 群仙问道赛领奖的数据一致性(数据覆写审计 r2 第 4 轮,群仙问道赛设计 V1 第七节):
//   - 带附件的奖励没发出邮件就不落已领取(r2-tn#1);
//   - 同一条奖励每次重领发的是同一原件,邮件已建而已领取没落定时,重领回原 mail_id 而不是请求冲突(r2-tn#2);
//   - 一条失败不挡其余。
// 存储与邮件都换内存替身;邮件替身按 go/mail Repo.Send 的判重口径回包:同一 request_id 摘要相同回原 mail_id,不同回请求冲突。

import (
	"context"
	"errors"
	"testing"

	"match/internal/constants"
	"match/internal/tournament"

	base "proto/common/base"
	mailpb "proto/mail"
	matchpb "proto/match"
	pbtable "shared/generated/pb/table"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const tournamentClaimTestSecret = "0123456789abcdef0123456789abcdef"

type fakeTournamentRewardStore struct {
	rows    []*matchpb.MatchTournamentRewardRecord
	claimed map[uint64]uint64 // reward_id → 落已领取时记下的 mail_id
	markErr map[uint64]error  // reward_id → 下一次落已领取返回的错误(用一次即清)
}

func newFakeTournamentRewardStore(rows ...*matchpb.MatchTournamentRewardRecord) *fakeTournamentRewardStore {
	return &fakeTournamentRewardStore{rows: rows, claimed: map[uint64]uint64{}, markErr: map[uint64]error{}}
}

func (s *fakeTournamentRewardStore) ClaimableTournamentRewards(_ context.Context, playerID uint64, seasonID uint32) ([]*matchpb.MatchTournamentRewardRecord, error) {
	var out []*matchpb.MatchTournamentRewardRecord
	for _, r := range s.rows {
		if _, done := s.claimed[r.RewardId]; done || r.PlayerId != playerID || (seasonID != 0 && r.SeasonId != seasonID) {
			continue
		}
		out = append(out, proto.Clone(r).(*matchpb.MatchTournamentRewardRecord))
	}
	return out, nil
}

func (s *fakeTournamentRewardStore) MarkTournamentRewardClaimed(_ context.Context, seasonID uint32, playerID uint64, tierKind uint32, mailID uint64) (*matchpb.TournamentPlayerRewardView, error) {
	for _, r := range s.rows {
		if r.SeasonId != seasonID || r.PlayerId != playerID || r.TierKind != tierKind {
			continue
		}
		if err := s.markErr[r.RewardId]; err != nil {
			delete(s.markErr, r.RewardId)
			return nil, err
		}
		if _, done := s.claimed[r.RewardId]; !done {
			s.claimed[r.RewardId] = mailID
		}
		return &matchpb.TournamentPlayerRewardView{RewardId: r.RewardId, SeasonId: seasonID, MailId: s.claimed[r.RewardId]}, nil
	}
	return nil, errors.New("fixture: reward row not found")
}

type fakeTournamentMailEntry struct {
	digest string
	mailID uint64
}

type fakeTournamentMail struct {
	mailpb.MailAdminClient
	sent     []*mailpb.SendMailRequest
	written  map[uint64]fakeTournamentMailEntry // request_id → 首次落库的摘要与 mail_id
	lostNext map[uint64]error                   // request_id → 下一次「邮件已建、应答丢了」(超时 / 断连),用一次即清
}

func newFakeTournamentMail() *fakeTournamentMail {
	return &fakeTournamentMail{written: map[uint64]fakeTournamentMailEntry{}, lostNext: map[uint64]error{}}
}

func (m *fakeTournamentMail) SendPersonalMail(_ context.Context, in *mailpb.SendMailRequest, _ ...grpc.CallOption) (*mailpb.SendMailResponse, error) {
	m.sent = append(m.sent, proto.Clone(in).(*mailpb.SendMailRequest))
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, err
	}
	entry, seen := m.written[in.RequestId]
	if !seen {
		entry = fakeTournamentMailEntry{digest: digest, mailID: uint64(9000 + len(m.written) + 1)}
		m.written[in.RequestId] = entry
	}
	if err := m.lostNext[in.RequestId]; err != nil {
		delete(m.lostNext, in.RequestId)
		return nil, err
	}
	if entry.digest != digest {
		return &mailpb.SendMailResponse{ErrorMessage: &base.TipInfoMessage{Id: uint32(pbtable.MailError_kMailRequestConflict)}}, nil
	}
	return &mailpb.SendMailResponse{MailId: entry.mailID, Duplicated: seen}, nil
}

// tournamentClaimRow 造一条待领奖励;gold 为 0 时没有附件(纯荣誉奖励)。文案是结算时冻结进行里的值。
func tournamentClaimRow(t *testing.T, rewardID uint64, tier matchpb.TournamentRewardTierKind, gold uint64) *matchpb.MatchTournamentRewardRecord {
	t.Helper()
	att := &mailpb.MailAttachments{}
	if gold > 0 {
		att.Currencies = []*mailpb.MailCurrency{{CurrencyType: tournament.GoldCurrencyType, Amount: gold}}
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(att)
	require.NoError(t, err)
	return &matchpb.MatchTournamentRewardRecord{
		SeasonId: 3, PlayerId: 1001, TierKind: uint32(tier), RewardId: rewardID, GoldAmount: gold, Attachments: raw,
		DeliveryState: uint32(matchpb.TournamentRewardDeliveryState_TOURNAMENT_REWARD_DELIVERY_CLAIMABLE),
		MailTitle:     "群仙问道赛·全服总冠军奖励",
		MailBody:      "恭喜你的战队夺得本赛季群仙问道赛全服总冠军！",
	}
}

func claimedRewardIDs(resp *matchpb.ClaimTournamentRewardResponse) []uint64 {
	ids := make([]uint64, 0, len(resp.GetClaimedRewards()))
	for _, v := range resp.GetClaimedRewards() {
		ids = append(ids, v.GetRewardId())
	}
	return ids
}

// r2-tn#1:发件没装配(没配 MailRpc,或没注入发件密钥)时,带附件的奖励回可重试错误、保持可领取;纯荣誉奖励照常落已领取、mail_id 为 0。
func TestClaimTournamentRewardsKeepsAttachmentRewardsClaimableWithoutMail(t *testing.T) {
	ctx := context.Background()
	unsent := newFakeTournamentMail()
	for _, wiring := range []struct {
		name   string
		mail   mailpb.MailAdminClient
		secret string
	}{
		{name: "no MailRpc", mail: nil, secret: tournamentClaimTestSecret},
		{name: "no mail producer secret", mail: unsent, secret: ""},
	} {
		store := newFakeTournamentRewardStore(
			tournamentClaimRow(t, 11, matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHAMPION, 120000),
			tournamentClaimRow(t, 12, matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PARTICIPANT, 0),
		)
		resp := claimTournamentRewards(ctx, store, wiring.mail, wiring.secret, 1001, 0)
		require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId(), "%s:带附件的那条要回可重试错误", wiring.name)
		require.Equal(t, []uint64{12}, claimedRewardIDs(resp), "%s:纯荣誉奖励照常领取", wiring.name)
		_, attachmentClaimed := store.claimed[11]
		require.False(t, attachmentClaimed, "%s:附件没发出去就落已领取,附件被吞、事后无从补发", wiring.name)
		mailID, honorClaimed := store.claimed[12]
		require.True(t, honorClaimed, wiring.name)
		require.Zero(t, mailID, "%s:纯荣誉奖励不发信,mail_id 为 0", wiring.name)
	}
	require.Empty(t, unsent.sent, "没有发件密钥时一封都不发")
}

// r2-tn#2:邮件已建而已领取没落定(TiDB 抖动、进程在两步之间重启)时重领,发的是逐字节同一原件:
// ExpireMs 为 0、文案取行里冻结的值;mail 判重回原 mail_id,已领取记的就是第一次建的那封。
func TestTournamentRewardMailRequestIsTheSameOriginalOnEveryRetry(t *testing.T) {
	ctx := context.Background()
	row := tournamentClaimRow(t, 21, matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHAMPION, 120000)
	store := newFakeTournamentRewardStore(row)
	mail := newFakeTournamentMail()
	store.markErr[21] = errors.New("fixture: TiDB commit timed out")

	first := claimTournamentRewards(ctx, store, mail, tournamentClaimTestSecret, 1001, 0)
	require.Equal(t, constants.ErrInternal, first.GetErrorMessage().GetId())
	require.Empty(t, first.GetClaimedRewards())

	second := claimTournamentRewards(ctx, store, mail, tournamentClaimTestSecret, 1001, 0)
	require.Zero(t, second.GetErrorMessage().GetId(), "重领不能撞成请求冲突")
	require.Equal(t, []uint64{21}, claimedRewardIDs(second))
	require.Equal(t, mail.written[21].mailID, store.claimed[21], "已领取记的是第一次建的那封邮件")

	require.Len(t, mail.sent, 2)
	firstDigest, err := serviceauth.Digest(mail.sent[0])
	require.NoError(t, err)
	secondDigest, err := serviceauth.Digest(mail.sent[1])
	require.NoError(t, err)
	require.Equal(t, firstDigest, secondDigest, "两次请求除签名外逐字节相同")
	for _, req := range mail.sent {
		require.Zero(t, req.GetExpireMs(), "有效期由 mail 首次落库时算,请求里不带当前时刻")
		require.Equal(t, row.MailTitle, req.GetTitle(), "标题取结算时冻结的值,不读领取时刻的配表")
		require.Equal(t, row.MailBody, req.GetBody())
		require.Equal(t, row.RewardId, req.GetRequestId())
		require.Equal(t, tournamentMailSenderKind, req.GetSenderKind())
		require.Equal(t, row.PlayerId, req.GetRecipientPlayerId())
		require.Equal(t, row.GoldAmount, req.GetAttachments().GetCurrencies()[0].GetAmount())
	}

	// 构造本身只看奖励行:同一行两次构造的字节相同。
	atts := &mailpb.MailAttachments{}
	require.NoError(t, proto.Unmarshal(row.Attachments, atts))
	once, err := proto.MarshalOptions{Deterministic: true}.Marshal(tournamentRewardMailRequest(row, atts))
	require.NoError(t, err)
	again, err := proto.MarshalOptions{Deterministic: true}.Marshal(tournamentRewardMailRequest(proto.Clone(row).(*matchpb.MatchTournamentRewardRecord), atts))
	require.NoError(t, err)
	require.Equal(t, once, again)
}

// 一条失败不挡其余:第一条的邮件应答丢了(邮件其实已建),其余两条照常领取;重领时第一条判重回原 mail_id,补落已领取。
func TestClaimTournamentRewardsOneFailureDoesNotBlockTheRest(t *testing.T) {
	ctx := context.Background()
	store := newFakeTournamentRewardStore(
		tournamentClaimRow(t, 31, matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHAMPION, 120000),
		tournamentClaimRow(t, 32, matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHEER, 5000),
		tournamentClaimRow(t, 33, matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PREDICTION, 12000),
	)
	mail := newFakeTournamentMail()
	mail.lostNext[31] = context.DeadlineExceeded

	resp := claimTournamentRewards(ctx, store, mail, tournamentClaimTestSecret, 1001, 0)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId(), "带回第一条失败的提示")
	require.Equal(t, []uint64{32, 33}, claimedRewardIDs(resp), "排在后面的奖励照常领取")
	_, firstClaimed := store.claimed[31]
	require.False(t, firstClaimed, "结果不明不当成已发送")

	retry := claimTournamentRewards(ctx, store, mail, tournamentClaimTestSecret, 1001, 0)
	require.Zero(t, retry.GetErrorMessage().GetId())
	require.Equal(t, []uint64{31}, claimedRewardIDs(retry), "只剩第一条可领")
	require.Equal(t, mail.written[31].mailID, store.claimed[31], "补落的是当初已建的那封,没有第二封")
	require.Len(t, mail.written, 3, "三条奖励各一封邮件")
}
