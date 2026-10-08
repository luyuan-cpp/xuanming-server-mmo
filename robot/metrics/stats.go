package metrics

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Stats tracks aggregate load-test counters.
// All methods are goroutine-safe.
type Stats struct {
	loginOK         atomic.Int64
	loginFail       atomic.Int64
	loginStuck      atomic.Int64
	enterOK         atomic.Int64
	enterFail       atomic.Int64
	msgSent         atomic.Int64
	msgRecv         atomic.Int64
	skillSent       atomic.Int64
	sceneSwitchSent atomic.Int64
	connected       atomic.Int64
	disconnected    atomic.Int64

	// access_token reconnect observability
	accessReconnectOK       atomic.Int64 // successful access_token short-circuit
	accessReconnectFallback atomic.Int64 // access_token failed → fell back to primary auth
	tokenRefreshOK          atomic.Int64 // successful in-session RefreshToken RPC
	tokenRefreshFail        atomic.Int64 // RefreshToken RPC returned error

	// AssignGate queue observability (added with the 2026-05 login queue).
	// queueEntered      = robots that hit code=100 at least once during this session
	// queueAdmittedAfterWait = robots that were eventually admitted from the queue
	// queueExpiredToken     = robots that saw 410 (queue token TTL'd out before admit)
	// queueWaitTotalNs      = sum of (admitTime - firstQueueResponseTime) per robot
	// queueMaxRankObserved  = high-water-mark of any robot's reported rank
	queueEntered           atomic.Int64
	queueAdmittedAfterWait atomic.Int64
	queueExpiredToken      atomic.Int64
	queueWaitTotalNs       atomic.Int64
	queueWaitCount         atomic.Int64
	queueMaxRankObserved   atomic.Int64

	// gateTokenRetry: VerifyGateToken 失败时,robot 不再消耗 retry / 不算 LoginFail,
	// 而是关掉本次连接、重新走 AssignGate(下一次 attempt 会拿到全新 token)。这个
	// 计数器记录"被 token-expired 反弹"的次数,Round 19 用来确认 5min→10min 后
	// 该路径是否真正归零。详见 docs/stress/stress-1zone-45k-2026-06-04-round18.md §R2。
	gateTokenRetry atomic.Int64

	mu             sync.Mutex
	loginCount     int64 // guarded by mu
	loginTotalNs   int64 // guarded by mu — sum in nanoseconds
	loginMaxNs     int64 // guarded by mu
	reportStart    time.Time
	prevMsgSent    int64     // guarded by mu — snapshot for rate calc
	prevMsgRecv    int64     // guarded by mu
	prevReportTime time.Time // guarded by mu

	loginRecords    []LoginRecord    // guarded by mu — per-attempt records for analysis
	behaviorRecords []BehaviorRecord // guarded by mu — per-action records for LLM/ML analysis
}

func NewStats() *Stats {
	now := time.Now()
	return &Stats{reportStart: now, prevReportTime: now}
}

func (s *Stats) LoginOK(d time.Duration) {
	s.loginOK.Add(1)
	s.mu.Lock()
	s.loginCount++
	s.loginTotalNs += int64(d)
	if int64(d) > s.loginMaxNs {
		s.loginMaxNs = int64(d)
	}
	s.mu.Unlock()
}
func (s *Stats) LoginFail()       { s.loginFail.Add(1) }
func (s *Stats) LoginStuck()      { s.loginStuck.Add(1) }
func (s *Stats) EnterOK()         { s.enterOK.Add(1) }
func (s *Stats) EnterFail()       { s.enterFail.Add(1) }
func (s *Stats) MsgSent()         { s.msgSent.Add(1) }
func (s *Stats) MsgRecv()         { s.msgRecv.Add(1) }
func (s *Stats) SkillSent()       { s.skillSent.Add(1) }
func (s *Stats) SceneSwitchSent() { s.sceneSwitchSent.Add(1) }
func (s *Stats) Connected()       { s.connected.Add(1) }
func (s *Stats) Disconnected()    { s.disconnected.Add(1) }

func (s *Stats) AccessReconnectOK()       { s.accessReconnectOK.Add(1) }
func (s *Stats) AccessReconnectFallback() { s.accessReconnectFallback.Add(1) }
func (s *Stats) TokenRefreshOK()          { s.tokenRefreshOK.Add(1) }
func (s *Stats) TokenRefreshFail()        { s.tokenRefreshFail.Add(1) }

// QueueEntered records that a robot hit code=100 (any source) at least once.
// Idempotent within a single robot's lifecycle is the caller's contract — we
// just count events so the report shows "this run had N queue events".
func (s *Stats) QueueEntered()               { s.queueEntered.Add(1) }
func (s *Stats) QueueAdmittedAfterWait()     { s.queueAdmittedAfterWait.Add(1) }
func (s *Stats) QueueExpiredToken()          { s.queueExpiredToken.Add(1) }

// QueueWait records the time a single robot spent waiting in the queue
// (from first code=100 response to final code=0 admit). Used to surface
// p50/p99 wait time in the report.
func (s *Stats) QueueWait(d time.Duration) {
	s.queueWaitTotalNs.Add(int64(d))
	s.queueWaitCount.Add(1)
}

// QueueRankSeen tracks the highest rank observed across all robots. A
// climbing high-water mark with no admits is the canonical signal of a
// stuck dispatcher.
func (s *Stats) QueueRankSeen(rank int64) {
	for {
		cur := s.queueMaxRankObserved.Load()
		if rank <= cur {
			return
		}
		if s.queueMaxRankObserved.CompareAndSwap(cur, rank) {
			return
		}
	}
}

// GateTokenRetry 记录一次 "VerifyGateToken 失败 → 不计 LoginFail / 不消耗 retry,
// 重新走 AssignGate" 的反弹。R17 R2 收尾的核心信号:Round 19 这个值应该接近 0
// (gateTokenTTL 拉长到 10min 后,robot polling→VerifyGateToken 链路延迟应当
// 完全覆盖掉 token TTL 窗口)。
func (s *Stats) GateTokenRetry() { s.gateTokenRetry.Add(1) }
func (s *Stats) GateTokenRetryCount() int64 { return s.gateTokenRetry.Load() }

// StartReporter prints a summary every interval until stop is closed.
func (s *Stats) StartReporter(interval time.Duration, stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.report()
			case <-stop:
				s.report()
				return
			}
		}
	}()
}

func (s *Stats) report() {
	now := time.Now()
	elapsed := now.Sub(s.reportStart).Truncate(time.Second)

	s.mu.Lock()
	count := s.loginCount
	totalNs := s.loginTotalNs
	maxNs := s.loginMaxNs
	prevSent := s.prevMsgSent
	prevRecv := s.prevMsgRecv
	prevTime := s.prevReportTime
	s.mu.Unlock()

	curSent := s.msgSent.Load()
	curRecv := s.msgRecv.Load()

	var sendRate, recvRate float64
	if dt := now.Sub(prevTime).Seconds(); dt > 0 {
		sendRate = float64(curSent-prevSent) / dt
		recvRate = float64(curRecv-prevRecv) / dt
	}

	s.mu.Lock()
	s.prevMsgSent = curSent
	s.prevMsgRecv = curRecv
	s.prevReportTime = now
	s.mu.Unlock()

	var avg, max time.Duration
	if count > 0 {
		avg = time.Duration(totalNs / count)
		max = time.Duration(maxNs)
	}

	zap.L().Info(fmt.Sprintf("[stats %s] conn=%d login_ok=%d login_fail=%d login_stuck=%d enter_ok=%d enter_fail=%d "+
		"msg_sent=%d(%.0f/s) msg_recv=%d(%.0f/s) skill=%d scene_switch=%d avg_login=%s max_login=%s "+
		"recon_ok=%d recon_fb=%d refresh_ok=%d refresh_fail=%d "+
		"q_entered=%d q_admitted=%d q_expired=%d q_avg_wait=%s q_max_rank=%d gate_token_retry=%d",
		elapsed,
		s.connected.Load(),
		s.loginOK.Load(), s.loginFail.Load(), s.loginStuck.Load(),
		s.enterOK.Load(), s.enterFail.Load(),
		curSent, sendRate, curRecv, recvRate,
		s.skillSent.Load(),
		s.sceneSwitchSent.Load(),
		avg.Truncate(time.Millisecond),
		max.Truncate(time.Millisecond),
		s.accessReconnectOK.Load(), s.accessReconnectFallback.Load(),
		s.tokenRefreshOK.Load(), s.tokenRefreshFail.Load(),
		s.queueEntered.Load(), s.queueAdmittedAfterWait.Load(), s.queueExpiredToken.Load(),
		s.avgQueueWait().Truncate(time.Millisecond), s.queueMaxRankObserved.Load(),
		s.gateTokenRetry.Load(),
	))
}

// avgQueueWait returns the running mean of QueueWait observations.
// Zero when no robot has been admitted from the queue yet.
func (s *Stats) avgQueueWait() time.Duration {
	count := s.queueWaitCount.Load()
	if count == 0 {
		return 0
	}
	return time.Duration(s.queueWaitTotalNs.Load() / count)
}

// LoginRecord captures one login attempt for post-hoc analysis / ML training.
type LoginRecord struct {
	Timestamp  time.Time
	Account    string
	Scenario   string // test scenario name or "stress"
	LatencyMs  int64
	Success    bool
	Stuck      bool // true if the attempt timed out waiting for a response
	ErrorMsg   string
	Attempt    int // retry attempt number (1-based)
	Concurrent int // number of concurrent connections at the time
}

// BehaviorRecord captures robot action traces for gameplay-pressure and LLM/ML analysis.
type BehaviorRecord struct {
	Timestamp  time.Time
	Account    string
	Scenario   string
	Action     string
	Success    bool
	LatencyMs  int64
	Detail     string
	SceneID    uint64
	Concurrent int
}

// RecordLogin appends a login attempt record (goroutine-safe).
func (s *Stats) RecordLogin(r LoginRecord) {
	s.mu.Lock()
	s.loginRecords = append(s.loginRecords, r)
	s.mu.Unlock()
}

func (s *Stats) RecordBehavior(r BehaviorRecord) {
	s.mu.Lock()
	s.behaviorRecords = append(s.behaviorRecords, r)
	s.mu.Unlock()
}

func (s *Stats) BehaviorRecordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.behaviorRecords)
}

// ExportLoginCSV writes all recorded login attempts to a CSV file for ML analysis.
// Columns: timestamp, account, scenario, latency_ms, success, stuck, error, attempt, concurrent
func (s *Stats) ExportLoginCSV(path string) error {
	s.mu.Lock()
	records := make([]LoginRecord, len(s.loginRecords))
	copy(records, s.loginRecords)
	s.mu.Unlock()

	if len(records) == 0 {
		zap.L().Info("no login records to export")
		return nil
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	_ = w.Write([]string{
		"timestamp", "account", "scenario", "latency_ms",
		"success", "stuck", "error", "attempt", "concurrent",
	})

	for _, r := range records {
		_ = w.Write([]string{
			r.Timestamp.Format(time.RFC3339Nano),
			r.Account,
			r.Scenario,
			strconv.FormatInt(r.LatencyMs, 10),
			strconv.FormatBool(r.Success),
			strconv.FormatBool(r.Stuck),
			r.ErrorMsg,
			strconv.Itoa(r.Attempt),
			strconv.Itoa(r.Concurrent),
		})
	}

	zap.L().Info("exported login records", zap.String("path", path), zap.Int("count", len(records)))
	return nil
}

func (s *Stats) ExportBehaviorCSV(path string) error {
	s.mu.Lock()
	records := make([]BehaviorRecord, len(s.behaviorRecords))
	copy(records, s.behaviorRecords)
	s.mu.Unlock()

	if len(records) == 0 {
		zap.L().Info("no behavior records to export")
		return nil
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create behavior csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	_ = w.Write([]string{
		"timestamp", "account", "scenario", "action", "success",
		"latency_ms", "detail", "scene_id", "concurrent",
	})

	for _, r := range records {
		_ = w.Write([]string{
			r.Timestamp.Format(time.RFC3339Nano),
			r.Account,
			r.Scenario,
			r.Action,
			strconv.FormatBool(r.Success),
			strconv.FormatInt(r.LatencyMs, 10),
			r.Detail,
			strconv.FormatUint(r.SceneID, 10),
			strconv.Itoa(r.Concurrent),
		})
	}

	zap.L().Info("exported behavior records", zap.String("path", path), zap.Int("count", len(records)))
	return nil
}

func (s *Stats) ExportBehaviorJSONL(path string) error {
	s.mu.Lock()
	records := make([]BehaviorRecord, len(s.behaviorRecords))
	copy(records, s.behaviorRecords)
	s.mu.Unlock()

	if len(records) == 0 {
		zap.L().Info("no behavior records to export as jsonl")
		return nil
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create behavior jsonl: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("encode behavior jsonl: %w", err)
		}
	}

	zap.L().Info("exported behavior jsonl", zap.String("path", path), zap.Int("count", len(records)))
	return nil
}
