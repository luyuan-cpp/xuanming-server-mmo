package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	pb "proto/match"
	inboxpb "proto/player_inbox"
)

// 终局发件箱:match_battle_completion 的结果 outbox(整局结果发 Kafka)与 match_battle_settlement_source 的个人 outbox
// (个人结算原件入 Inbox、等 APPLIED)。本文件是一轮投递的批量、并发、计数与积压读数(2026-10-30,评审 B-6)。
//
// 认领、投递与回写的语义与之前逐行串行的实现相同:逐行以观察到的 next_retry_ms 做 CAS 认领(改成本轮租约),
// 只有认领者投递,回写再以同一租约值为条件。所以同一轮内并发处理不同行、多个副本同时巡检都只会有一方投递同一行;
// 认领者投递后来不及回写就崩溃时,租约到期由任一副本重投同一份原件(至少一次,下游按 battle_id / (player, battle) 去重)。

// 投递计数与积压 gauge 的 kind 标签(封闭集合)。
const (
	OutboxKindResult  = "result"  // 整局结果发 Kafka(match_battle_completion)
	OutboxKindFirst   = "first"   // 个人结算首投 Inbox(inbox_seq=0,EnqueueBattleSettlement)
	OutboxKindWaiting = "waiting" // 已入箱、等 APPLIED(inbox_seq>0,LookupBattleSettlement)
)

// 投递计数的 outcome 标签(封闭集合)。指标只用 kind / outcome 两个低基数维度,battle_id / player_id 一律不进。
const (
	OutboxPublished  = "published"   // 结果已发 Kafka 并标记
	OutboxPending    = "pending"     // 个人原件已在 Inbox、尚未 APPLIED,按退避(5 秒到 5 分钟)再查
	OutboxApplied    = "applied"     // 个人原件已 APPLIED,发件箱销账
	OutboxFailed     = "failed"      // 投递失败或回执不合法;行留在认领租约里,到期由任一副本重投原件
	OutboxCorrupt    = "corrupt"     // 存量原件与摘要不符,不投递
	OutboxClaimLost  = "claim_lost"  // 认领 CAS 输给了别的副本或别的轮次,本轮不投
	OutboxDeferred   = "deferred"    // 本轮认领预算用尽,没认领、没改行,留给下一轮
	OutboxStoreError = "store_error" // 认领或回写的 SQL 失败;本轮不再派发新行
)

// OutcomeOutboxOptions 是终局发件箱一轮投递的批量与并发参数,对应配置 GlobalMatch.OutcomeOutbox(装配在 svc/servicecontext.go,
// 投递节拍 IntervalMs 由那里的循环使用,不在本结构里)。
// 零值字段取默认值;默认值与 2026-10-30 之前写死的行为一致:结果 8 行、首投 4 行、等待 4 行、逐行串行。
type OutcomeOutboxOptions struct {
	ResultBatch  int // 每轮最多读出的结果行(decision=COMPLETE、未发布、已到期)
	FirstBatch   int // 每轮最多读出的个人首投行(inbox_seq=0);与等待行分开计,离线积压占不满首投页
	WaitingBatch int // 每轮最多读出的个人等待 APPLIED 行(inbox_seq>0)
	Concurrency  int // 已认领行的并发投递上限;1 = 逐行串行
	// Observe 记每一行的处理结局(kind 见 OutboxKind*,outcome 见 OutboxPublished 等);nil = 不计数。
	// 并发投递时会被多个 goroutine 同时调用,实现必须并发安全(Prometheus 计数器即是)。
	Observe func(kind, outcome string)
}

// 参数上界:批量受单轮认领预算(outcomeOutboxClaimBudget)约束,并发受 SQL 连接池与 Inbox 端压力约束。
// 配置校验应使用同样的上界;越界由 Normalized 拒绝,不悄悄夹紧。
const (
	MaxOutcomeOutboxBatch       = 256
	MaxOutcomeOutboxConcurrency = 32
)

// DefaultOutcomeOutboxOptions 返回与 2026-10-30 之前行为一致的默认参数。
func DefaultOutcomeOutboxOptions() OutcomeOutboxOptions {
	return OutcomeOutboxOptions{ResultBatch: 8, FirstBatch: 4, WaitingBatch: 4, Concurrency: 1}
}

// Normalized 把零值字段换成默认值,并拒绝负数与超过上界的取值(配置错误 fail-closed)。
func (o OutcomeOutboxOptions) Normalized() (OutcomeOutboxOptions, error) {
	defaults := DefaultOutcomeOutboxOptions()
	for _, field := range []struct {
		name  string
		value *int
		def   int
		max   int
	}{
		{"ResultBatch", &o.ResultBatch, defaults.ResultBatch, MaxOutcomeOutboxBatch},
		{"FirstBatch", &o.FirstBatch, defaults.FirstBatch, MaxOutcomeOutboxBatch},
		{"WaitingBatch", &o.WaitingBatch, defaults.WaitingBatch, MaxOutcomeOutboxBatch},
		{"Concurrency", &o.Concurrency, defaults.Concurrency, MaxOutcomeOutboxConcurrency},
	} {
		switch {
		case *field.value == 0:
			*field.value = field.def
		case *field.value < 0 || *field.value > field.max:
			return OutcomeOutboxOptions{}, fmt.Errorf("终局发件箱参数%s=%d越界，须在1到%d之间", field.name, *field.value, field.max)
		}
	}
	return o, nil
}

const (
	// outcomeOutboxLeaseMs 是逐行认领租约(SQL 毫秒):到期仍未回写的行由任一副本重投同一份原件。
	outcomeOutboxLeaseMs uint64 = 30000
	// outcomeOutboxCallTimeout 是单行一次投递(Kafka 发布,或 Inbox Enqueue / Lookup)的上限。
	outcomeOutboxCallTimeout = 2 * time.Second
	// outcomeOutboxClaimBudget:一轮从读 SQL 时刻起超过它就不再认领新行,剩下的记 deferred 留给下一轮。租约按本轮
	// SQL 时刻 + 30 秒计,晚于 26 秒才认领的行在 2 秒投递与回写之前租约就可能过期,另一副本会并发认领同一行
	// (旧的逐行串行实现在 16 行全部 2 秒超时的最坏情况下也会走到这一步)。用本地单调时钟计。
	outcomeOutboxClaimBudget = time.Duration(outcomeOutboxLeaseMs)*time.Millisecond - 2*outcomeOutboxCallTimeout
)

// PublishBattleOutcomes 投递一轮终局发件箱:先结果、后个人(首投与等待各有独立名额)。两类 outbox 逐行以 SQL 截止租约
// 认领;失败项留在租约里推后,下一页不会永远被前几项占住。回执丢失只重签认证并重放原正文,SQL 写序列 / 原 owner
// 从不在投递时重算。返回本轮全部投递失败与第一处 SQL 失败的合并错误;调用方只记日志与 HoldRelay,下一轮照常重试。
func (s *Store) PublishBattleOutcomes(ctx context.Context, client func(context.Context) (inboxpb.PlayerInboxClient, error), secret string, publish func(context.Context, uint64, []byte) error, options OutcomeOutboxOptions) error {
	if client == nil || publish == nil {
		return errors.New("终局投递端口未装配")
	}
	o, err := options.Normalized()
	if err != nil {
		return err
	}
	// 认领预算从读 SQL 时刻之前起算:租约基准 stamp 只会更晚,预算因此偏保守。
	claimBy := time.Now().Add(outcomeOutboxClaimBudget)
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	lease := stamp + outcomeOutboxLeaseMs
	results, err := s.dueOutcomeResults(ctx, stamp, o.ResultBatch)
	if err != nil {
		return err
	}
	resultJobs := make([]outboxJob, 0, len(results))
	for _, r := range results {
		resultJobs = append(resultJobs, outboxJob{kind: OutboxKindResult, run: func(ctx context.Context) outboxRowResult {
			return s.publishOutcomeResult(ctx, r, lease, publish)
		}})
	}
	failures, fatal := runOutboxJobs(ctx, resultJobs, o.Concurrency, claimBy, o.Observe)
	if fatal != nil {
		return errors.Join(append(failures, fatal)...)
	}
	people, err := s.dueSettlementSources(ctx, stamp, o.FirstBatch, o.WaitingBatch)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	personJobs := make([]outboxJob, 0, len(people))
	for _, r := range people {
		kind := OutboxKindFirst
		if r.InboxSeq != 0 {
			kind = OutboxKindWaiting
		}
		personJobs = append(personJobs, outboxJob{kind: kind, run: func(ctx context.Context) outboxRowResult {
			return s.deliverSettlementSource(ctx, r, stamp, lease, client, secret)
		}})
	}
	more, fatal := runOutboxJobs(ctx, personJobs, o.Concurrency, claimBy, o.Observe)
	failures = append(failures, more...)
	if fatal != nil {
		failures = append(failures, fatal)
	}
	return errors.Join(failures...)
}

// dueOutcomeResults 读已到期、尚未发布的 COMPLETE 结果行(按 next_retry_ms, battle_id 排序,至多 limit 行)。
func (s *Store) dueOutcomeResults(ctx context.Context, stamp uint64, limit int) ([]*pb.MatchBattleCompletionRecord, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT battle_id,raw_payload,payload_digest,next_retry_ms FROM match_battle_completion WHERE decision=1 AND result_published=0 AND next_retry_ms<=? ORDER BY next_retry_ms,battle_id LIMIT ?", stamp, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchBattleCompletionRecord
	for rows.Next() {
		r := &pb.MatchBattleCompletionRecord{}
		if err := rows.Scan(&r.BattleId, &r.RawPayload, &r.PayloadDigest, &r.NextRetryMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// dueSettlementSources 读已到期的个人行:首投(inbox_seq=0)至多 firstLimit 行,等待 APPLIED(inbox_seq>0)至多
// waitingLimit 行,两页分开读,首投在前。
func (s *Store) dueSettlementSources(ctx context.Context, stamp uint64, firstLimit, waitingLimit int) ([]*pb.MatchBattleSettlementSourceRecord, error) {
	var out []*pb.MatchBattleSettlementSourceRecord
	for _, page := range []struct {
		seqCondition string
		limit        int
	}{{"=0", firstLimit}, {">0", waitingLimit}} {
		rows, err := s.DB.QueryContext(ctx, "SELECT battle_id,player_id,settlement_proto,payload_digest,owner_account,owner_revision,issued_at_ms,inbox_seq,next_retry_ms FROM match_battle_settlement_source WHERE dispatch_state=1 AND inbox_seq"+page.seqCondition+" AND next_retry_ms<=? ORDER BY next_retry_ms,battle_id,player_id LIMIT ?", stamp, page.limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r := &pb.MatchBattleSettlementSourceRecord{}
			if err = rows.Scan(&r.BattleId, &r.PlayerId, &r.SettlementProto, &r.PayloadDigest, &r.OwnerAccount, &r.OwnerRevision, &r.IssuedAtMs, &r.InboxSeq, &r.NextRetryMs); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// outboxJob 是一行的完整处理(认领 → 投递 → 回写都在 run 里);kind 只给计数用。
type outboxJob struct {
	kind string
	run  func(context.Context) outboxRowResult
}

// outboxRowResult 是一行的处理结局。err 非 nil 计入本轮失败;fatal(认领或回写的 SQL 失败)时本轮不再派发新行,
// 与旧实现遇到 SQL 错误即返回一致(已在途的行照常跑完)。
type outboxRowResult struct {
	outcome string
	err     error
	fatal   bool
}

// runOutboxJobs 以至多 concurrency 个 goroutine 处理 jobs,派发顺序就是读出顺序。先占到空位再派发,认领时刻尽量贴近投递;
// concurrency=1 时后一行在前一行结束之后才认领,与旧的逐行串行实现相同。
//   - 过了 claimBy 还没派发的行记 deferred,不认领、不改行;
//   - 某行返回 fatal、或 ctx 已结束,就不再派发新行,等在途的行结束;
//   - 某行 panic 时同样停派,等在途的行结束后在调用方 goroutine 里重新 panic(与旧实现一样交给调用方的 recover)。
//
// 返回非致命失败与第一处致命错误(SQL 失败或 ctx 结束)。
func runOutboxJobs(ctx context.Context, jobs []outboxJob, concurrency int, claimBy time.Time, observe func(kind, outcome string)) ([]error, error) {
	if observe == nil {
		observe = func(string, string) {}
	}
	var (
		mu       sync.Mutex
		failures []error
		fatal    error
		panicked any
		wg       sync.WaitGroup
	)
	halted := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if fatal == nil && panicked == nil && ctx.Err() != nil {
			fatal = ctx.Err()
		}
		return fatal != nil || panicked != nil
	}
	slots := make(chan struct{}, concurrency)
	for i, job := range jobs {
		slots <- struct{}{}
		if halted() {
			<-slots
			break
		}
		if !time.Now().Before(claimBy) {
			<-slots
			for _, rest := range jobs[i:] {
				observe(rest.kind, OutboxDeferred)
			}
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer func() {
				if recovered := recover(); recovered != nil {
					mu.Lock()
					if panicked == nil {
						panicked = recovered
					}
					mu.Unlock()
				}
			}()
			result := job.run(ctx)
			observe(job.kind, result.outcome)
			if result.err == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if result.fatal && fatal == nil {
				fatal = result.err
				return
			}
			failures = append(failures, result.err)
		}()
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return failures, fatal
}

// claimOutboxRow 以观察到的 next_retry_ms 为条件改成本轮租约;改到 0 行 = 别的副本或别的轮次先认领了。
func (s *Store) claimOutboxRow(ctx context.Context, query string, args ...any) (bool, error) {
	changed, err := s.DB.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := changed.RowsAffected()
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

// publishOutcomeResult 认领一行结果、发 Kafka、以同一租约为条件标记已发布。
func (s *Store) publishOutcomeResult(ctx context.Context, r *pb.MatchBattleCompletionRecord, lease uint64, publish func(context.Context, uint64, []byte) error) outboxRowResult {
	claimed, err := s.claimOutboxRow(ctx, "UPDATE match_battle_completion SET next_retry_ms=? WHERE battle_id=? AND decision=1 AND result_published=0 AND next_retry_ms=?", lease, r.BattleId, r.NextRetryMs)
	if err != nil {
		return outboxRowResult{outcome: OutboxStoreError, err: err, fatal: true}
	}
	if !claimed {
		return outboxRowResult{outcome: OutboxClaimLost}
	}
	if d := sha256.Sum256(r.RawPayload); !bytes.Equal(d[:], r.PayloadDigest) {
		return outboxRowResult{outcome: OutboxCorrupt, err: errors.New("结果outbox摘要损坏")}
	}
	call, cancel := context.WithTimeout(ctx, outcomeOutboxCallTimeout)
	err = publish(call, r.BattleId, r.RawPayload)
	cancel()
	if err != nil {
		return outboxRowResult{outcome: OutboxFailed, err: err}
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE match_battle_completion SET result_published=1 WHERE battle_id=? AND decision=1 AND result_published=0 AND next_retry_ms=?", r.BattleId, lease); err != nil {
		return outboxRowResult{outcome: OutboxStoreError, err: err, fatal: true}
	}
	return outboxRowResult{outcome: OutboxPublished}
}

// deliverSettlementSource 认领一行个人结算,首投 Enqueue 或到期 Lookup,按签名回执推进 inbox_seq / 状态与下次到期:
// APPLIED 销账(dispatch_state=2),PENDING 按原签发时间退避(pendingSettlementDelayMs)。
func (s *Store) deliverSettlementSource(ctx context.Context, r *pb.MatchBattleSettlementSourceRecord, stamp, lease uint64, client func(context.Context) (inboxpb.PlayerInboxClient, error), secret string) outboxRowResult {
	claimed, err := s.claimOutboxRow(ctx, "UPDATE match_battle_settlement_source SET next_retry_ms=? WHERE battle_id=? AND player_id=? AND dispatch_state=1 AND next_retry_ms=?", lease, r.BattleId, r.PlayerId, r.NextRetryMs)
	if err != nil {
		return outboxRowResult{outcome: OutboxStoreError, err: err, fatal: true}
	}
	if !claimed {
		return outboxRowResult{outcome: OutboxClaimLost}
	}
	if d := sha256.Sum256(r.SettlementProto); !bytes.Equal(d[:], r.PayloadDigest) {
		return outboxRowResult{outcome: OutboxCorrupt, err: errors.New("个人outbox摘要损坏")}
	}
	call, cancel := context.WithTimeout(ctx, outcomeOutboxCallTimeout)
	seq, state, err := deliverBattleOriginal(call, r, client, secret)
	cancel()
	if err != nil {
		return outboxRowResult{outcome: OutboxFailed, err: err}
	}
	dispatch, next, outcome := uint32(1), stamp+pendingSettlementDelayMs(r.IssuedAtMs, stamp), OutboxPending
	if state == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED {
		dispatch, next, outcome = 2, 0, OutboxApplied
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET inbox_seq=?,inbox_state=?,dispatch_state=?,next_retry_ms=? WHERE battle_id=? AND player_id=? AND dispatch_state=1 AND next_retry_ms=? AND (inbox_seq=0 OR inbox_seq=?)", seq, uint32(state), dispatch, next, r.BattleId, r.PlayerId, lease, seq); err != nil {
		return outboxRowResult{outcome: OutboxStoreError, err: err, fatal: true}
	}
	return outboxRowResult{outcome: outcome}
}

// SettlementLookupDue 报告这张 ready 票的个人结算是否到了可以向 Inbox Lookup 的时刻:只有「已入箱(inbox_seq>0)、
// 未销账、next_retry_ms 已到(SQL 时钟)」才是 true;还没入箱、已销账、原 owner 不符都是 false。
//
// ready 票巡检(logic/global_ready.go)先问它,不到期就不发 Lookup:巡检每 500ms 一轮,不问就会绕过发件箱给等待行定的
// 5 秒到 5 分钟退避,对每个离线玩家每轮查一次 Inbox。到期确认仍由发件箱(PublishBattleOutcomes)负责,巡检跳过
// 不影响释放。按需路径(JoinQueue / GetQueueStatus 经 RefreshBattleSettlementReceipt)不问它,玩家主动查询时立即确认。
// 只读;判定与真正 Lookup 之间的竞态最多多一次只读 Lookup,不改任何业务结论。
func (s *Store) SettlementLookupDue(ctx context.Context, ticket *pb.MatchAuthorityTicketRecord) (bool, error) {
	if ticket == nil || ticket.State != 3 || ticket.FormationId == 0 || ticket.OwnerAccount == "" || ticket.OwnerRevision == 0 {
		return false, nil
	}
	var due bool
	err := s.DB.QueryRowContext(ctx, "SELECT next_retry_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) FROM match_battle_settlement_source WHERE battle_id=? AND player_id=? AND owner_account=? AND owner_revision=? AND dispatch_state=1 AND inbox_seq>0", ticket.FormationId, ticket.PlayerId, ticket.OwnerAccount, ticket.OwnerRevision).Scan(&due)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return due, err
}

// OutcomeOutboxBacklog 是终局发件箱三类待投行的积压读数。只读,只给 gauge;不参与任何投递或释放判定。
type OutcomeOutboxBacklog struct {
	Result  OutboxBacklog // decision=COMPLETE、结果还没发到 Kafka
	First   OutboxBacklog // 个人结算还没入箱(dispatch_state=1、inbox_seq=0)
	Waiting OutboxBacklog // 已入箱、等玩家 APPLIED(dispatch_state=1、inbox_seq>0);离线玩家会长期停在这里
}

// OutboxBacklog 是一类待投行的条数与最老一行的年龄。
type OutboxBacklog struct {
	Rows             uint64
	OldestAgeSeconds float64 // 最老一行自归档(archived_ms)到 SQL 当前时刻的秒数;没有积压为 0
}

// ReadOutcomeOutboxBacklog 用 COUNT / MIN 读三类积压。由每分钟的巡检调用(logic/global_expired_audit.go);
// 每个副本各读一份,看板按 max() 聚合。
func (s *Store) ReadOutcomeOutboxBacklog(ctx context.Context) (OutcomeOutboxBacklog, error) {
	stamp, err := s.Now(ctx)
	if err != nil {
		return OutcomeOutboxBacklog{}, err
	}
	var out OutcomeOutboxBacklog
	for _, q := range []struct {
		into  *OutboxBacklog
		query string
	}{
		{&out.Result, "SELECT COUNT(*),COALESCE(MIN(archived_ms),0) FROM match_battle_completion WHERE decision=1 AND result_published=0"},
		{&out.First, "SELECT COUNT(*),COALESCE(MIN(archived_ms),0) FROM match_battle_settlement_source WHERE dispatch_state=1 AND inbox_seq=0"},
		{&out.Waiting, "SELECT COUNT(*),COALESCE(MIN(archived_ms),0) FROM match_battle_settlement_source WHERE dispatch_state=1 AND inbox_seq>0"},
	} {
		var oldest uint64
		if err := s.DB.QueryRowContext(ctx, q.query).Scan(&q.into.Rows, &oldest); err != nil {
			return OutcomeOutboxBacklog{}, err
		}
		if q.into.Rows > 0 && stamp > oldest {
			q.into.OldestAgeSeconds = float64(stamp-oldest) / 1000
		}
	}
	return out, nil
}
