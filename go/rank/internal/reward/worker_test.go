package reward

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	mailpb "proto/mail"
	rankpb "proto/rank"
	"rank/internal/data"
	"shared/drainproof"
	table "shared/generated/pb/table"
	"shared/serviceauth"
)

// memoryStore 按 rank_mail_reward 的 SQL 口径模拟奖励行:只有待投、到期、租约已过的行可领;
// 领租约只带出 ClaimReward 那条 SELECT 读的列;落结果必须仍是待投且持本次租约。SQL 本身由 data 包的 TiDB 用例覆盖。
type memoryStore struct {
	mu   sync.Mutex
	rows map[uint64]*rankpb.RankMailRewardRecord
}

func newMemoryStore(rows ...*rankpb.RankMailRewardRecord) *memoryStore {
	s := &memoryStore{rows: map[uint64]*rankpb.RankMailRewardRecord{}}
	for _, row := range rows {
		s.rows[row.RewardId] = proto.Clone(row).(*rankpb.RankMailRewardRecord)
	}
	return s
}

func (s *memoryStore) DueRewards(_ context.Context, now uint64, limit int) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []*rankpb.RankMailRewardRecord
	for _, row := range s.rows {
		if row.Status == data.RewardPending && row.NextAttemptMs <= now && row.LeaseUntilMs < now {
			due = append(due, row)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].NextAttemptMs != due[j].NextAttemptMs {
			return due[i].NextAttemptMs < due[j].NextAttemptMs
		}
		return due[i].RewardId < due[j].RewardId
	})
	var ids []uint64
	for _, row := range due {
		if len(ids) == limit {
			break
		}
		ids = append(ids, row.RewardId)
	}
	return ids, nil
}

func (s *memoryStore) ClaimReward(_ context.Context, id, now, until, token uint64) (*rankpb.RankMailRewardRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.Status != data.RewardPending || row.LeaseUntilMs >= now {
		return nil, false, nil
	}
	row.LeaseUntilMs, row.LeaseToken = until, token
	return &rankpb.RankMailRewardRecord{RewardId: id, LeaseToken: token, SettlementId: row.SettlementId, PlayerId: row.PlayerId, Rank: row.Rank,
		Attachments: row.Attachments, Attempts: row.Attempts, CreatedMs: row.CreatedMs, ExpireMs: row.ExpireMs}, true, nil
}

func (s *memoryStore) settle(p *rankpb.RankMailRewardRecord, state uint32, mailID, next, now uint64, countedAttempts uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[p.RewardId]
	if !ok || row.Status != data.RewardPending || row.LeaseToken != p.LeaseToken {
		return data.ErrConflict
	}
	row.Status, row.MailId, row.NextAttemptMs, row.LeaseUntilMs, row.UpdatedMs = state, mailID, next, 0, now
	row.Attempts += countedAttempts
	return nil
}

func (s *memoryStore) SettleReward(_ context.Context, p *rankpb.RankMailRewardRecord, mailID, next, now uint64) error {
	state := data.RewardPending
	if mailID != 0 {
		state = data.RewardDelivered
	}
	return s.settle(p, state, mailID, next, now, 1)
}

func (s *memoryStore) AbandonReward(_ context.Context, p *rankpb.RankMailRewardRecord, now uint64) error {
	return s.settle(p, data.RewardAbandoned, 0, now, now, 1)
}

func (s *memoryStore) DeferReward(_ context.Context, p *rankpb.RankMailRewardRecord, next, now uint64) error {
	return s.settle(p, data.RewardPending, 0, next, now, 0)
}

func (s *memoryStore) row(id uint64) *rankpb.RankMailRewardRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.Clone(s.rows[id]).(*rankpb.RankMailRewardRecord)
}

// manualClock 是用例的业务时钟:只在用例里拨动,不读真实墙钟。
type manualClock struct{ ms uint64 }

func (c *manualClock) now() time.Time { return time.UnixMilli(int64(c.ms)) }

// scriptedMail 记下每次发件的请求与当时的业务时刻,按调用序号(从 1 起)回包。
type scriptedMail struct {
	clock    *manualClock
	requests []*mailpb.SendMailRequest
	sentAt   []uint64
	reply    func(call int) (*mailpb.SendMailResponse, error)
}

func (m *scriptedMail) SendPersonalMail(_ context.Context, in *mailpb.SendMailRequest, _ ...grpc.CallOption) (*mailpb.SendMailResponse, error) {
	m.requests = append(m.requests, proto.Clone(in).(*mailpb.SendMailRequest))
	m.sentAt = append(m.sentAt, m.clock.ms)
	return m.reply(len(m.requests))
}

var errNotPersonal = errors.New("rank rewards only use SendPersonalMail")

func (m *scriptedMail) SendSystemMail(context.Context, *mailpb.SendMailRequest, ...grpc.CallOption) (*mailpb.SendMailResponse, error) {
	return nil, errNotPersonal
}

func (m *scriptedMail) SendGuildMail(context.Context, *mailpb.SendMailRequest, ...grpc.CallOption) (*mailpb.SendMailResponse, error) {
	return nil, errNotPersonal
}

func (m *scriptedMail) ImportMailRecipients(context.Context, *mailpb.ImportMailRecipientsRequest, ...grpc.CallOption) (*mailpb.ImportMailRecipientsResponse, error) {
	return nil, errNotPersonal
}

func (m *scriptedMail) SealMailCampaign(context.Context, *mailpb.SealMailCampaignRequest, ...grpc.CallOption) (*mailpb.SendMailResponse, error) {
	return nil, errNotPersonal
}

const (
	testSealedMs = uint64(1_000_000_000_000)
	minuteMs     = uint64(60 * 1000)
)

func rewardRow(id, expireMs uint64) *rankpb.RankMailRewardRecord {
	attachments, err := proto.Marshal(&mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{CurrencyType: 1, Amount: 10}}})
	if err != nil {
		panic(err)
	}
	return &rankpb.RankMailRewardRecord{RewardId: id, SettlementId: 50, PlayerId: 7, Rank: 1, Attachments: attachments, Status: data.RewardPending,
		NextAttemptMs: testSealedMs, CreatedMs: testSealedMs, UpdatedMs: testSealedMs, ExpireMs: expireMs}
}

func testWorker(store Store, mail *scriptedMail) *Worker {
	return &Worker{Repo: store, Mail: mail, Secret: strings.Repeat("k", 32), Now: mail.clock.now}
}

// driveUntilSettled 每轮把业务时钟拨到奖励的下次尝试时刻再跑一轮,直到奖励离开待投;超过轮数即失败。
func driveUntilSettled(t *testing.T, w *Worker, store *memoryStore, clock *manualClock, id uint64, maxRounds int) {
	t.Helper()
	for round := 0; round < maxRounds; round++ {
		row := store.row(id)
		if row.Status != data.RewardPending {
			return
		}
		if row.NextAttemptMs > clock.ms {
			clock.ms = row.NextAttemptMs
		}
		if err := w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("奖励 %d 在 %d 轮内没有离开待投: %v", id, maxRounds, store.row(id))
}

func requestDigest(t *testing.T, request *mailpb.SendMailRequest) string {
	t.Helper()
	value, err := serviceauth.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// 收件箱一直满:按 1、2、4…32 分钟再每小时一次退避重投,下次尝试不晚于截止点(过期前 24 小时);截止点那一投仍被拒就不再发件,
// 推迟到过期后 1 小时的判重探询,探询仍被拒才转放弃终态。截止点与探询之间一封都不发。
// 每次发出的载荷完全相同(含探询):请求号是 reward_id,过期时刻是封榜时写定的 expire_ms,不是 created_ms+30 天。
func TestWorkerRetriesBoxFullWithBackoffUntilExpiryThenAbandons(t *testing.T) {
	cutoff := testSealedMs + 180*minuteMs
	expire := cutoff + minClaimMs
	store := newMemoryStore(rewardRow(1, expire))
	clock := &manualClock{ms: testSealedMs}
	mail := &scriptedMail{clock: clock, reply: func(int) (*mailpb.SendMailResponse, error) {
		return tipReply(uint32(table.MailError_kMailRecipientBoxFull)), nil
	}}
	w := testWorker(store, mail)
	driveUntilSettled(t, w, store, clock, 1, 50)
	row := store.row(1)
	if row.Status != data.RewardAbandoned || row.MailId != 0 {
		t.Fatalf("过期后的探询仍被拒应转放弃终态: %v", row)
	}
	var offsets []uint64
	for _, at := range mail.sentAt {
		offsets = append(offsets, (at-testSealedMs)/minuteMs)
	}
	probeOffset := (expiredProbeMs(expire) - testSealedMs) / minuteMs
	if want := []uint64{0, 1, 3, 7, 15, 31, 63, 123, 180, probeOffset}; !slices.Equal(offsets, want) {
		t.Fatalf("重投时刻(封榜后分钟) %v, want %v", offsets, want)
	}
	if row.Attempts != uint32(len(mail.requests)) {
		t.Fatalf("尝试次数 %d 与实际发件 %d 次不符", row.Attempts, len(mail.requests))
	}
	first := requestDigest(t, mail.requests[0])
	for i, request := range mail.requests {
		if request.RequestId != 1 || request.ExpireMs != expire || request.SenderKind != 3 || request.RecipientPlayerId != 7 {
			t.Fatalf("第 %d 次发件的键不对: %v", i+1, request)
		}
		if requestDigest(t, request) != first {
			t.Fatalf("第 %d 次发件载荷变了, 邮件会判成冲突: %v", i+1, request)
		}
	}
	clock.ms = expire + 24*60*minuteMs
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mail.requests) != len(mail.sentAt) || len(mail.requests) != 10 {
		t.Fatalf("放弃之后工作器不得再发件: %d 次", len(mail.requests))
	}
}

// 截止点之后到过期后的判重探询之前一封都不发:那时发出的邮件剩余不足 24 小时,玩家多半领不到,却会被记成已投。
// 邮件一直结果未知也不转终态(之前的尝试可能已经写了邮件);探询时邮件回原邮件(判重命中),奖励记为已投。
func TestWorkerNeverSendsWithLessThanMinimumClaimWindowLeft(t *testing.T) {
	cutoff := testSealedMs + 10*minuteMs
	expire := cutoff + minClaimMs
	probe := expiredProbeMs(expire)
	store := newMemoryStore(rewardRow(6, expire))
	clock := &manualClock{ms: testSealedMs}
	mail := &scriptedMail{clock: clock}
	mail.reply = func(int) (*mailpb.SendMailResponse, error) {
		if clock.ms >= probe {
			return &mailpb.SendMailResponse{MailId: 66, Duplicated: true}, nil
		}
		return nil, status.Error(codes.Unavailable, "reply lost")
	}
	w := testWorker(store, mail)
	driveUntilSettled(t, w, store, clock, 6, 100)
	if got := store.row(6); got.Status != data.RewardDelivered || got.MailId != 66 {
		t.Fatalf("探询时邮件回原邮件应记为已投: %v", got)
	}
	for i, at := range mail.sentAt {
		if at > cutoff && at < probe {
			t.Fatalf("第 %d 次发件在封榜后 %d 毫秒, 剩余可领 %d 毫秒, 不足最短可领时长 %d", i+1, at-testSealedMs, int64(expire)-int64(at), minClaimMs)
		}
	}
	if last := mail.sentAt[len(mail.sentAt)-1]; len(mail.sentAt) < 2 || last != probe || mail.sentAt[len(mail.sentAt)-2] > cutoff {
		t.Fatalf("截止点之前照常重投, 之后只有过期后的那一次探询: %v", mail.sentAt)
	}
}

// 过了截止点才被领到的行(工作器停了一阵,或上一个领租约的副本发出后崩溃、租约到期):不发件、不计尝试次数,
// 推迟到过期后的判重探询;探询时邮件回参数非法(已过期,判重没命中 = 邮件确认没有这封信)才转放弃终态。
// 不发件的推迟与明确拒收都是确定的结局,不挂起排空证明。
func TestWorkerDefersRowFoundPastCutoffUntilExpiryCheck(t *testing.T) {
	cutoff := testSealedMs + 60*minuteMs
	expire := cutoff + minClaimMs
	row := rewardRow(7, expire)
	row.Attempts = 3
	store := newMemoryStore(row)
	clock := &manualClock{ms: cutoff + 1}
	mail := &scriptedMail{clock: clock, reply: func(int) (*mailpb.SendMailResponse, error) {
		return tipReply(uint32(table.CommonError_kInvalidParameter)), nil
	}}
	w := testWorker(store, mail)
	relay := drainproof.New()
	w.Relay = relay
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := store.row(7)
	if len(mail.requests) != 0 || got.Status != data.RewardPending || got.NextAttemptMs != expiredProbeMs(expire) || got.Attempts != 3 || got.LeaseUntilMs != 0 {
		t.Fatalf("过了截止点应不发件、不计次数、推迟到探询时刻: %v, 发件 %d 次", got, len(mail.requests))
	}
	clock.ms = expire
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mail.requests) != 0 {
		t.Fatalf("过期之后、探询之前不得发件: %d 次", len(mail.requests))
	}
	driveUntilSettled(t, w, store, clock, 7, 3)
	if got := store.row(7); got.Status != data.RewardAbandoned || got.Attempts != 4 || len(mail.requests) != 1 || mail.sentAt[0] != expiredProbeMs(expire) {
		t.Fatalf("探询被拒才转放弃, 且只发探询这一次: %v, 发件时刻 %v", got, mail.sentAt)
	}
	if request := mail.requests[0]; request.RequestId != 7 || request.ExpireMs != expire {
		t.Fatalf("探询必须沿用原请求号与写定的过期时刻: %v", request)
	}
	relay.CloseAdmission()
	if err := relay.Wait(context.Background()); err != nil {
		t.Fatalf("推迟与明确拒收都是确定结局, 不得挂起排空证明: %v", err)
	}
}

// 次数上限先于过期到达(只会出在时钟或数据异常时,正常退避下窗口内到不了,见 TestAttemptCapNeverPreemptsExpiryWithinRewardWindow):
// 第 MaxAttempts 次仍被拒即放弃,不再等到过期。
func TestWorkerAbandonsAtAttemptCapBeforeExpiry(t *testing.T) {
	row := rewardRow(2, testSealedMs+30*24*60*minuteMs)
	row.Attempts = MaxAttempts - 1
	store := newMemoryStore(row)
	clock := &manualClock{ms: testSealedMs}
	mail := &scriptedMail{clock: clock, reply: func(int) (*mailpb.SendMailResponse, error) {
		return tipReply(uint32(table.MailError_kMailRecipientBoxFull)), nil
	}}
	if err := testWorker(store, mail).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.row(2); got.Status != data.RewardAbandoned || got.Attempts != MaxAttempts || len(mail.requests) != 1 {
		t.Fatalf("到次数上限应放弃: %v, 发件 %d 次", got, len(mail.requests))
	}
}

// 结果未知(传输失败、邮件故障码)即使过了过期时刻、超过次数上限也留在待投:邮件可能已经提交;
// 过期后的判重探询时邮件对同一请求只会回原邮件或明确拒收,重投必然收敛。这里邮件最终回原邮件,奖励记为已投。
func TestWorkerKeepsUnknownOutcomePendingPastExpiryAndCapUntilResolved(t *testing.T) {
	expire := testSealedMs + 60*minuteMs
	row := rewardRow(3, expire)
	row.Attempts = MaxAttempts + 3
	store := newMemoryStore(row)
	clock := &manualClock{ms: expiredProbeMs(expire)}
	mail := &scriptedMail{clock: clock, reply: func(call int) (*mailpb.SendMailResponse, error) {
		switch call {
		case 1:
			return nil, status.Error(codes.DeadlineExceeded, "reply lost")
		case 2:
			return tipReply(uint32(table.CommonError_kServiceUnavailable)), nil
		}
		return &mailpb.SendMailResponse{MailId: 77, Duplicated: true}, nil
	}}
	w := testWorker(store, mail)
	for call := 1; call <= 2; call++ {
		if row := store.row(3); row.NextAttemptMs > clock.ms {
			clock.ms = row.NextAttemptMs
		}
		if err := w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := store.row(3); got.Status != data.RewardPending {
			t.Fatalf("第 %d 次结果未知不得转终态: %v", call, got)
		}
	}
	driveUntilSettled(t, w, store, clock, 3, 5)
	if got := store.row(3); got.Status != data.RewardDelivered || got.MailId != 77 {
		t.Fatalf("邮件回原邮件后应记为已投: %v", got)
	}
	for i, request := range mail.requests {
		if request.ExpireMs != expire {
			t.Fatalf("第 %d 次发件必须沿用写定的过期时刻: %v", i+1, request)
		}
	}
}

// 没有写定过期时刻的原件(缺列或坏行)没有可投窗口:不发件,直接放弃。
func TestWorkerAbandonsRowWithoutFixedExpireWithoutSending(t *testing.T) {
	store := newMemoryStore(rewardRow(4, 0))
	clock := &manualClock{ms: testSealedMs}
	mail := &scriptedMail{clock: clock, reply: func(int) (*mailpb.SendMailResponse, error) { return &mailpb.SendMailResponse{MailId: 1}, nil }}
	if err := testWorker(store, mail).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.row(4); got.Status != data.RewardAbandoned || len(mail.requests) != 0 {
		t.Fatalf("缺过期时刻应不发件直接放弃: %v, 发件 %d 次", got, len(mail.requests))
	}
}

// 附件原件解不开:不发件,截止点之前按拒收退避等受控修复,之后推迟到过期后的探询时刻,仍坏即放弃;从头到尾不发件。
func TestWorkerHoldsCorruptAttachmentsWithinWindowThenAbandons(t *testing.T) {
	expire := testSealedMs + 2*minuteMs + minClaimMs
	row := rewardRow(5, expire)
	row.Attachments = []byte{0xff, 0xff}
	store := newMemoryStore(row)
	clock := &manualClock{ms: testSealedMs}
	mail := &scriptedMail{clock: clock, reply: func(int) (*mailpb.SendMailResponse, error) { return &mailpb.SendMailResponse{MailId: 1}, nil }}
	w := testWorker(store, mail)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.row(5); got.Status != data.RewardPending || got.NextAttemptMs != testSealedMs+minuteMs {
		t.Fatalf("窗口内坏原件应按拒收退避留在待投: %v", got)
	}
	driveUntilSettled(t, w, store, clock, 5, 5)
	if got := store.row(5); got.Status != data.RewardAbandoned || len(mail.requests) != 0 || clock.ms != expiredProbeMs(expire) {
		t.Fatalf("坏原件到探询时刻应放弃且从不发件: %v, 发件 %d 次, 时刻 %d", got, len(mail.requests), clock.ms)
	}
}

// 结果未知的三种(邮件回故障码、传输失败、成功码却没有 mail_id)都可能已经写了邮件,一律挂起排空证明,
// 不能据同步返回宣称终局(邮件服务注释引的规格 §3.6:回故障 tip 即 Hold)。明确拒收与投到是确定的结局,不挂起。
// 旧实现只在传输失败、成功码没有 mail_id 时挂起,邮件回故障码时不挂,排空收据会把可能在途的邮件当成已结清。
func TestWorkerHoldsDrainProofOnEveryUnknownOutcome(t *testing.T) {
	cases := []struct {
		name     string
		reply    func() (*mailpb.SendMailResponse, error)
		wantHold bool
	}{
		{name: "邮件回故障码", wantHold: true, reply: func() (*mailpb.SendMailResponse, error) {
			return tipReply(uint32(table.CommonError_kServiceUnavailable)), nil
		}},
		{name: "传输失败", wantHold: true, reply: func() (*mailpb.SendMailResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "reply lost")
		}},
		{name: "成功码却没有 mail_id", wantHold: true, reply: func() (*mailpb.SendMailResponse, error) {
			return &mailpb.SendMailResponse{}, nil
		}},
		{name: "收件箱满(明确拒收)", reply: func() (*mailpb.SendMailResponse, error) {
			return tipReply(uint32(table.MailError_kMailRecipientBoxFull)), nil
		}},
		{name: "投到", reply: func() (*mailpb.SendMailResponse, error) {
			return &mailpb.SendMailResponse{MailId: 91}, nil
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := uint64(40 + i)
			store := newMemoryStore(rewardRow(id, testSealedMs+minClaimMs+180*minuteMs))
			clock := &manualClock{ms: testSealedMs}
			mail := &scriptedMail{clock: clock, reply: func(int) (*mailpb.SendMailResponse, error) { return tc.reply() }}
			w := testWorker(store, mail)
			relay := drainproof.New()
			w.Relay = relay
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(mail.requests) != 1 {
				t.Fatalf("应恰好发件一次, 实际 %d 次", len(mail.requests))
			}
			relay.CloseAdmission()
			err := relay.Wait(context.Background())
			if tc.wantHold && !errors.Is(err, drainproof.ErrHold) {
				t.Fatalf("结果未知必须挂起排空证明, Wait 返回 %v", err)
			}
			if !tc.wantHold && err != nil {
				t.Fatalf("确定的结局不得挂起排空证明, Wait 返回 %v", err)
			}
		})
	}
}
