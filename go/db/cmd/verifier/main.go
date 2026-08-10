// verifier compares the converged state of player_database rows against the
// "expected" map written by data_stress (or by an instrumented robot).
//
// The contract being verified, per (msg_type, player_id):
//
//  1. PERSISTENCE CONVERGENCE (I1)
//     MySQL row's seq field == expected_seq.
//
//  2. CACHE CONVERGENCE (I2)
//     Redis cache "<msg_type>:<player_id>" exists and its decoded seq field
//     == expected_seq. (May be absent if the row was never read since being
//     written; we treat that as a soft-pass and report it separately.)
//
//  3. KAFKA NO BACKLOG (I3)
//     Best-effort: we don't read consumer-group lag here; pair this with
//     `kafka-consumer-groups.sh --describe` for that signal.
//
// SEQ ENCODING
//
//	We read `player_database.stress_test_probe.test_seq` (matching
//	data_stress / robot data-stress mode) and re-validate
//	`stress_test_probe.test_sig` to detect torn / mis-routed payloads.
//
// USAGE
//
//	go run ./cmd/verifier \
//	  -config etc/db.yaml \
//	  -redis 127.0.0.1:6379 \
//	  -wait 60s -interval 2s
//
// Exits non-zero if any mismatch persists beyond the deadline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	db_config "db/internal/config"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/stresstest"

	dbpb "proto/common/database"

	"github.com/zeromicro/go-zero/core/conf"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// Prometheus metrics. Scraped during long-running verifier runs (chaos
// tests, soak tests) so a Grafana panel can plot convergence latency and
// per-round divergence over time.
var (
	mEnrolled = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "verify_enrolled_players",
		Help: "Number of players published to verify:enrolled:<msg_type>",
	})
	mChecked = promauto.NewCounter(prometheus.CounterOpts{
		Name: "verify_checks_total",
		Help: "Total per-player consistency checks performed (rounds * players)",
	})
	mDivergent = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "verify_divergent_players",
		Help: "Players currently failing consistency, broken down by failure kind",
	}, []string{"kind"})
	mSeqLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "verify_seq_lag",
		Help: "Per-store seq lag against expected (expected - observed). 0 means converged",
	}, []string{"store"})
	mRound = promauto.NewCounter(prometheus.CounterOpts{
		Name: "verify_rounds_total",
		Help: "Number of verifier rounds executed",
	})
	mConverged = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "verify_converged",
		Help: "1 if the most recent round saw zero mismatches, 0 otherwise",
	})
)

type mismatch struct {
	playerID    uint64
	expectedSeq uint64
	mysqlSeq    uint64
	cacheSeq    uint64
	cacheMissed bool
	mysqlMissed bool
	mysqlSigBad bool // mysql row has a probe but its test_sig is wrong
	cacheSigBad bool // redis cache has a probe but its test_sig is wrong
}

func main() {
	var (
		configFile    = flag.String("config", "etc/db.yaml", "db.yaml config file (for MySQL connection)")
		redisAddr     = flag.String("redis", "127.0.0.1:6379", "redis addr")
		redisPassword = flag.String("redis-password", "", "redis password")
		redisDB       = flag.Int("redis-db", 0, "redis DB number")
		wait          = flag.Duration("wait", 60*time.Second, "max time to wait for full convergence")
		interval      = flag.Duration("interval", 2*time.Second, "polling interval between rounds")
		concurrency   = flag.Int("concurrency", 32, "max concurrent player checks per round")
		jsonOut       = flag.Bool("json", false, "emit summary as JSON instead of text")
		mode          = flag.String("mode", "strict", "comparison mode: 'strict' (mysql/cache seq must equal expected — for L2 data_stress driver) or 'atleast' (mysql/cache seq must be >= expected — for L3 robot mode where Scene may stamp multiple times per round)")
		metricsAddr   = flag.String("metrics-addr", "", "if non-empty, expose Prometheus metrics on this addr (e.g. ':9091'). /metrics is the scrape endpoint.")
	)
	flag.Parse()
	if *mode != "strict" && *mode != "atleast" {
		log.Fatalf("--mode must be 'strict' or 'atleast', got %q", *mode)
	}

	conf.MustLoad(*configFile, &db_config.AppConfig)
	db_config.AppConfig.Normalize()
	if db_config.AppConfig.ZoneId == 0 {
		log.Fatalf("ZoneId must be set in %s", *configFile)
	}
	db_config.AppConfig.ServerConfig.Database.DBName = db_config.ZoneDBName(db_config.AppConfig.ZoneId)

	// Initialize MySQL access via the same proto2mysql layer the consumer uses.
	// 与业务服务同一条路径:只连接 + 注册表,不跑 DDL,并过库名白名单。
	if err := proto_sql.InitDB(); err != nil {
		log.Fatalf("database init rejected: %v", err)
	}

	rc := redis.NewClient(&redis.Options{
		Addr: *redisAddr, Password: *redisPassword, DB: *redisDB,
	})
	defer func() { _ = rc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *wait+10*time.Second)
	defer cancel()

	if err := rc.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		go func() {
			log.Printf("metrics listener on %s/metrics", *metricsAddr)
			if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
				log.Printf("metrics listener exited: %v", err)
			}
		}()
	}

	msgType := string((&dbpb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName())

	deadline := time.Now().Add(*wait)
	round := 0

	var lastMismatches []mismatch
	for {
		round++
		players, err := loadEnrolled(ctx, rc, msgType)
		if err != nil {
			log.Fatalf("load enrolled: %v", err)
		}
		if len(players) == 0 {
			log.Printf("round %d: no enrolled players (verify:enrolled:%s set is empty); waiting...", round, msgType)
			if !time.Now().Before(deadline) {
				exitWithFailure("no enrolled players found before deadline", *jsonOut, nil)
			}
			time.Sleep(*interval)
			continue
		}

		mEnrolled.Set(float64(len(players)))
		mismatches, totalChecked := runRound(ctx, rc, msgType, players, *concurrency, *mode)
		lastMismatches = mismatches
		converged := len(mismatches) == 0

		mRound.Inc()
		mChecked.Add(float64(totalChecked))
		updateDivergenceMetrics(mismatches)
		if converged {
			mConverged.Set(1)
		} else {
			mConverged.Set(0)
		}

		log.Printf("round %d: checked=%d mismatches=%d converged=%v",
			round, totalChecked, len(mismatches), converged)

		if converged {
			emitSummary("converged", round, totalChecked, nil, *jsonOut)
			return
		}

		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(*interval)
	}

	emitSummary("DIVERGENT", round, len(lastMismatches), lastMismatches, *jsonOut)
	os.Exit(2)
}

func loadEnrolled(ctx context.Context, rc *redis.Client, msgType string) ([]uint64, error) {
	members, err := rc.SMembers(ctx, "verify:enrolled:"+msgType).Result()
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, len(members))
	for _, m := range members {
		var id uint64
		if _, err := fmt.Sscanf(m, "%d", &id); err == nil && id != 0 {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func runRound(
	ctx context.Context,
	rc *redis.Client,
	msgType string,
	players []uint64,
	concurrency int,
	mode string,
) (mismatches []mismatch, totalChecked int) {
	if concurrency < 1 {
		concurrency = 1
	}

	var (
		mu  sync.Mutex
		sem = make(chan struct{}, concurrency)
		wg  sync.WaitGroup
	)

	for _, pid := range players {
		sem <- struct{}{}
		wg.Add(1)
		go func(playerID uint64) {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := checkPlayer(ctx, rc, msgType, playerID, mode)
			if err != nil {
				log.Printf("check player %d: %v", playerID, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			totalChecked++
			if res != nil {
				mismatches = append(mismatches, *res)
			}
		}(pid)
	}
	wg.Wait()

	sort.Slice(mismatches, func(i, j int) bool { return mismatches[i].playerID < mismatches[j].playerID })
	return mismatches, totalChecked
}

// seqMatches returns true if observed satisfies the comparison rule for
// the given mode against expected. "strict" requires equality; "atleast"
// requires observed >= expected.
func seqMatches(observed, expected uint64, mode string) bool {
	if mode == "atleast" {
		return observed >= expected
	}
	return observed == expected
}

func checkPlayer(ctx context.Context, rc *redis.Client, msgType string, playerID uint64, mode string) (*mismatch, error) {
	expectedRaw, err := rc.Get(ctx, fmt.Sprintf("verify:expected:%s:%d", msgType, playerID)).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("missing expected key for player=%d", playerID)
		}
		return nil, err
	}
	var expected uint64
	if _, err := fmt.Sscanf(expectedRaw, "%d", &expected); err != nil {
		return nil, fmt.Errorf("parse expected for player=%d: %v", playerID, err)
	}

	mySQLSeq, mysqlOK, mysqlSigOK := readMySQLProbe(playerID, msgType)
	cacheSeq, cacheOK, cacheSigOK := readCacheProbe(ctx, rc, msgType, playerID)

	if mysqlOK && cacheOK &&
		seqMatches(mySQLSeq, expected, mode) &&
		seqMatches(cacheSeq, expected, mode) &&
		mysqlSigOK && cacheSigOK {
		return nil, nil
	}
	return &mismatch{
		playerID:    playerID,
		expectedSeq: expected,
		mysqlSeq:    mySQLSeq,
		cacheSeq:    cacheSeq,
		cacheMissed: !cacheOK,
		mysqlMissed: !mysqlOK,
		mysqlSigBad: mysqlOK && !mysqlSigOK,
		cacheSigBad: cacheOK && !cacheSigOK,
	}, nil
}

// readMySQLProbe loads the row's stress_test_probe. Returns
// (seq, rowExists, sigOK). A row that exists but has no probe sub-message
// reports seq=0, rowExists=true, sigOK=false (so the verifier flags it as
// "stamping never reached MySQL").
func readMySQLProbe(playerID uint64, msgType string) (uint64, bool, bool) {
	row := &dbpb.PlayerDatabase{}
	where := fmt.Sprintf("player_id=%d", playerID)
	if err := proto_sql.DB.SqlModel.FindOneByWhereClause(row, where); err != nil {
		return 0, false, false
	}
	probe := row.GetStressTestProbe()
	if probe == nil {
		return 0, true, false
	}
	seq, sigOK := stresstest.VerifyProbe(playerID, msgType, probe)
	return seq, true, sigOK
}

// readCacheProbe loads the cached row from Redis. Returns
// (seq, rowExists, sigOK). Same semantics as readMySQLProbe.
func readCacheProbe(ctx context.Context, rc *redis.Client, msgType string, playerID uint64) (uint64, bool, bool) {
	raw, err := rc.Get(ctx, fmt.Sprintf("%s:%d", msgType, playerID)).Bytes()
	if err != nil {
		return 0, false, false
	}
	row := &dbpb.PlayerDatabase{}
	if err := proto.Unmarshal(raw, row); err != nil {
		log.Printf("warning: cache decode failed for player=%d: %v", playerID, err)
		return 0, false, false
	}
	probe := row.GetStressTestProbe()
	if probe == nil {
		return 0, true, false
	}
	seq, sigOK := stresstest.VerifyProbe(playerID, msgType, probe)
	return seq, true, sigOK
}

// updateDivergenceMetrics resets per-kind gauges and recomputes them from
// the current round's mismatches. We use Set (not Inc) so the gauge always
// reflects "currently failing" rather than a cumulative count.
func updateDivergenceMetrics(mismatches []mismatch) {
	kinds := map[string]float64{
		"MYSQL_ROW_MISSING":     0,
		"CACHE_MISSING":         0,
		"BOTH_SIG_INVALID":      0,
		"MYSQL_SIG_INVALID":     0,
		"CACHE_SIG_INVALID":     0,
		"MYSQL_BEHIND_EXPECTED": 0,
		"CACHE_BEHIND_EXPECTED": 0,
		"BOTH_BEHIND_EXPECTED":  0,
		"MYSQL_DIVERGENT":       0,
		"CACHE_DIVERGENT":       0,
		"BOTH_DIVERGENT":        0,
	}
	var maxMysqlLag, maxCacheLag uint64
	for _, m := range mismatches {
		switch {
		case m.mysqlMissed:
			kinds["MYSQL_ROW_MISSING"]++
		case m.cacheMissed:
			kinds["CACHE_MISSING"]++
		case m.mysqlSigBad && m.cacheSigBad:
			kinds["BOTH_SIG_INVALID"]++
		case m.mysqlSigBad:
			kinds["MYSQL_SIG_INVALID"]++
		case m.cacheSigBad:
			kinds["CACHE_SIG_INVALID"]++
		case m.mysqlSeq < m.expectedSeq && m.cacheSeq < m.expectedSeq:
			kinds["BOTH_BEHIND_EXPECTED"]++
		case m.mysqlSeq < m.expectedSeq:
			kinds["MYSQL_BEHIND_EXPECTED"]++
		case m.cacheSeq < m.expectedSeq:
			kinds["CACHE_BEHIND_EXPECTED"]++
		case m.mysqlSeq != m.expectedSeq && m.cacheSeq != m.expectedSeq:
			kinds["BOTH_DIVERGENT"]++
		case m.mysqlSeq != m.expectedSeq:
			kinds["MYSQL_DIVERGENT"]++
		case m.cacheSeq != m.expectedSeq:
			kinds["CACHE_DIVERGENT"]++
		}
		if m.expectedSeq > m.mysqlSeq {
			if lag := m.expectedSeq - m.mysqlSeq; lag > maxMysqlLag {
				maxMysqlLag = lag
			}
		}
		if m.expectedSeq > m.cacheSeq {
			if lag := m.expectedSeq - m.cacheSeq; lag > maxCacheLag {
				maxCacheLag = lag
			}
		}
	}
	for k, v := range kinds {
		mDivergent.WithLabelValues(k).Set(v)
	}
	mSeqLag.WithLabelValues("mysql").Set(float64(maxMysqlLag))
	mSeqLag.WithLabelValues("cache").Set(float64(maxCacheLag))
}

func emitSummary(status string, rounds int, n int, mismatches []mismatch, asJSON bool) {
	if asJSON {
		fmt.Printf("{\"status\":%q,\"rounds\":%d,\"checked_or_mismatched\":%d,\"mismatches\":[", status, rounds, n)
		for i, m := range mismatches {
			if i > 0 {
				fmt.Print(",")
			}
			fmt.Printf("{\"player_id\":%d,\"expected\":%d,\"mysql\":%d,\"cache\":%d,\"mysql_missing\":%v,\"cache_missing\":%v}",
				m.playerID, m.expectedSeq, m.mysqlSeq, m.cacheSeq, m.mysqlMissed, m.cacheMissed)
		}
		fmt.Println("]}")
		return
	}
	fmt.Printf("\n=== verifier summary: %s ===\n", status)
	fmt.Printf("rounds=%d, mismatches=%d\n", rounds, len(mismatches))
	for _, m := range mismatches {
		var kind string
		switch {
		case m.mysqlMissed:
			kind = "MYSQL_ROW_MISSING"
		case m.cacheMissed:
			kind = "CACHE_MISSING"
		case m.mysqlSigBad && m.cacheSigBad:
			kind = "BOTH_SIG_INVALID"
		case m.mysqlSigBad:
			kind = "MYSQL_SIG_INVALID"
		case m.cacheSigBad:
			kind = "CACHE_SIG_INVALID"
		case m.mysqlSeq < m.expectedSeq && m.cacheSeq < m.expectedSeq:
			kind = "BOTH_BEHIND_EXPECTED"
		case m.mysqlSeq < m.expectedSeq:
			kind = "MYSQL_BEHIND_EXPECTED"
		case m.cacheSeq < m.expectedSeq:
			kind = "CACHE_BEHIND_EXPECTED"
		case m.mysqlSeq != m.expectedSeq && m.cacheSeq != m.expectedSeq:
			kind = "BOTH_DIVERGENT"
		case m.mysqlSeq != m.expectedSeq:
			kind = "MYSQL_DIVERGENT"
		case m.cacheSeq != m.expectedSeq:
			kind = "CACHE_DIVERGENT"
		default:
			kind = "OK"
		}
		fmt.Printf("  player=%d expected=%d mysql=%d cache=%d kind=%s\n",
			m.playerID, m.expectedSeq, m.mysqlSeq, m.cacheSeq, kind)
	}
}

func exitWithFailure(msg string, asJSON bool, mismatches []mismatch) {
	if asJSON {
		fmt.Printf(`{"status":"FAILURE","reason":%q}`+"\n", msg)
	} else {
		fmt.Println("FAILURE:", msg)
	}
	os.Exit(2)
}
