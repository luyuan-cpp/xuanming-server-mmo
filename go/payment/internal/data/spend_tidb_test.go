package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	paymentpb "proto/payment"
	"schemamigrate"
)

// 真库用例:额度行锁能不能真把同一个人的并发预占排成队,是限额闸正确与否的关键,只有真库能证明。
// PAYMENT_TEST_DSN 指向一套专用的 TiDB(或 MySQL)、带建库权限的账号;每次只建、删自己的随机库。没配就跳过,
// PAYMENT_REQUIRE_TIDB=1 时没配判失败。
func spendTestRepo(t *testing.T) *Repo {
	t.Helper()
	dsn := os.Getenv("PAYMENT_TEST_DSN")
	if dsn == "" {
		if os.Getenv("PAYMENT_REQUIRE_TIDB") == "1" {
			t.Fatal("PAYMENT_TEST_DSN is not set")
		}
		t.Skip("PAYMENT_TEST_DSN is not set")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse PAYMENT_TEST_DSN: %v", err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "payment_it_" + hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE `"+name+"`") })
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(16)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := schemamigrate.Up(ctx, db, schemamigrate.Options{Database: name, Tables: Tables()}); err != nil {
		t.Fatalf("schemamigrate.Up: %v", err)
	}
	return New(db)
}

const testMonthStart = 1_000_000

func minorIntentRow(hi, lo, spender uint64, amount uint64) *paymentpb.PaymentIntentRecord {
	return &paymentpb.PaymentIntentRecord{OrderHi: hi, OrderLo: lo, OrderId: "test", PlayerId: 1, Channel: 1, AmountMinor: amount, Currency: "CNY",
		State: IntentCreated, ExpireMs: 9_000_000, CreatedMs: 2_000_000, UpdatedMs: 2_000_000, SpenderHi: spender, SpenderLo: spender, MinorAge: 15}
}

func randomKey(t *testing.T) uint64 {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return binary.BigEndian.Uint64(raw[:]) | 1
}

// 同一个人并发建 8 个 50 元的意图,每月 200 元:恰好 4 个预占成功,其余回 ErrMonthlyLimit。
func TestTiDBReserveIntentSerializesOnePerson(t *testing.T) {
	repo := spendTestRepo(t)
	spender, orderHi := randomKey(t), randomKey(t)
	limit := SpendLimit{MonthlyFen: 20000, MonthStartMs: testMonthStart, DayStartMs: testMonthStart}
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, results[i] = repo.ReserveIntent(context.Background(), minorIntentRow(orderHi, uint64(i+1), spender, 5000), limit)
		}(i)
	}
	wg.Wait()
	reserved := 0
	for i, err := range results {
		switch {
		case err == nil:
			reserved++
		case errors.Is(err, ErrMonthlyLimit):
		default:
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	if reserved != 4 {
		t.Fatalf("reserved %d of 8 intents of 50 yuan against a 200 yuan monthly limit, want 4", reserved)
	}
}

// 同一订单并发建意图:只插入一行,两边拿到同一行;核对时不计这张订单自己,后到的不会被自己挡住。
func TestTiDBReserveIntentIsIdempotentPerOrder(t *testing.T) {
	repo := spendTestRepo(t)
	spender, orderHi := randomKey(t), randomKey(t)
	limit := SpendLimit{MonthlyFen: 20000, MonthStartMs: testMonthStart, DayStartMs: testMonthStart}
	var wg sync.WaitGroup
	inserted := make([]bool, 2)
	errs := make([]error, 2)
	for i := range inserted {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, inserted[i], errs[i] = repo.ReserveIntent(context.Background(), minorIntentRow(orderHi, 7, spender, 15000), limit)
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || inserted[0] == inserted[1] {
		t.Fatalf("concurrent reservations of one order: inserted=%v errs=%v, want exactly one insert and no error", inserted, errs)
	}
}

// 到账、关闭、迟到的钱:转已付计入 paid_ms 所在的月;关闭释放;迟到的钱超限时不改、不超限时转已付;已付的不重复计。
func TestTiDBPaidClosedAndRevivedIntents(t *testing.T) {
	repo := spendTestRepo(t)
	ctx := context.Background()
	spender, orderHi := randomKey(t), randomKey(t)
	limit := SpendLimit{MonthlyFen: 20000, MonthStartMs: testMonthStart, DayStartMs: testMonthStart}
	first := minorIntentRow(orderHi, 1, spender, 15000)
	if _, _, err := repo.ReserveIntent(ctx, first, limit); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkIntentPaid(ctx, orderHi, 1, 3_000_000); err != nil {
		t.Fatal(err)
	}
	paid, err := repo.Intent(ctx, orderHi, 1)
	if err != nil || paid.State != IntentPaid || paid.PaidMs != 3_000_000 {
		t.Fatalf("after MarkIntentPaid: %+v %v", paid, err)
	}
	if _, _, err := repo.ReserveIntent(ctx, minorIntentRow(orderHi, 2, spender, 5001), limit); !errors.Is(err, ErrMonthlyLimit) {
		t.Fatalf("a paid intent must keep its amount this month: %v", err)
	}
	// 下个月(窗口起点晚于 paid_ms):上个月付的不再算。
	next := SpendLimit{MonthlyFen: 20000, MonthStartMs: 4_000_000, DayStartMs: 4_000_000}
	second := minorIntentRow(orderHi, 2, spender, 15000)
	if _, _, err := repo.ReserveIntent(ctx, second, next); err != nil {
		t.Fatalf("last month's payment must not count: %v", err)
	}
	if closed, err := repo.CloseIntent(ctx, orderHi, 2, 5_000_000); err != nil || !closed {
		t.Fatalf("close: %v %v", closed, err)
	}
	// 关掉之后额度释放,第三单占满本月的 200 元。
	if _, _, err := repo.ReserveIntent(ctx, minorIntentRow(orderHi, 3, spender, 20000), next); err != nil {
		t.Fatalf("a closed intent must release its amount: %v", err)
	}
	within, err := repo.ReviveMinorIntent(ctx, second, next, 6_000_000, 6_000_000, false)
	if err != nil || within {
		t.Fatalf("a late payment over the limit must be refused: within=%v err=%v", within, err)
	}
	if still, _ := repo.Intent(ctx, orderHi, 2); still.State != IntentClosed {
		t.Fatalf("a refused late payment must leave the intent closed: %+v", still)
	}
	// 撤回退款(force):超限照样转已付,回 false。
	within, err = repo.ReviveMinorIntent(ctx, second, next, 6_000_000, 6_000_000, true)
	revived, _ := repo.Intent(ctx, orderHi, 2)
	if err != nil || within || revived.State != IntentPaid || revived.PaidMs != 6_000_000 {
		t.Fatalf("a forced recount: within=%v err=%v intent=%+v", within, err, revived)
	}
	// 已经是已付的:额度早已计入,回 true、不改。
	if within, err = repo.ReviveMinorIntent(ctx, second, next, 7_000_000, 7_000_000, false); err != nil || !within {
		t.Fatalf("reviving a paid intent again: within=%v err=%v", within, err)
	}
}
