package data

// sweep_test.go —— 过期清理按 mail_id 分片(缺口清单第 25 条第四批,docs/design/assetop-redelivery-sharding-20261110.md §14.2)
// 的 SQL 层回归:纯函数钉住条件的位置与占位符;TiDB 用例(门控同 repo_integration_test.go 的 MAIL_RANK_TEST_TIDB)钉住真库效果。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	mailpb "proto/mail"
	"shared/workshard"
)

// 候选读与无效行抽样都把分片条件放在 ORDER BY / LIMIT 之前;占位符个数与 Sweep 传的实参一致
// (候选读:cutoff、片号……、limit;抽样:片号……、limit);零值条件不过滤;两条都是普通读。
func TestSweepQueriesPutShardFilterBeforeLimit(t *testing.T) {
	mine := map[string]bool{workshard.ShardKey(SweepWorkKind, 33): true, workshard.ShardKey(SweepWorkKind, 9): true}
	owned := workshard.SQLFilter(func(key string) bool { return mine[key] }, SweepWorkKind, SweepShardColumn)
	if got := owned.SQL(); got != "MOD(m.mail_id,64) IN(?,?)" {
		t.Fatalf("分片条件应按 mail_id 取模: %q", got)
	}
	if got := fmt.Sprint(owned.Args()); got != "[9 33]" {
		t.Fatalf("片号应按升序只走绑定参数: %s", got)
	}
	candidates := sweepCandidateQuery(owned)
	if !strings.HasSuffix(candidates, " AND MOD(m.mail_id,64) IN(?,?) ORDER BY m.expire_ms,m.mail_id LIMIT ?") {
		t.Fatalf("候选读的分片条件必须在 ORDER BY / LIMIT 之前:\n  %s", candidates)
	}
	if got, want := strings.Count(candidates, "?"), 1+len(owned.Args())+1; got != want {
		t.Fatalf("候选读占位符 %d 个,Sweep 传 %d 个实参", got, want)
	}
	sample := sweepInvalidSampleQuery(owned)
	if !strings.Contains(sample, " WHERE MOD(m.mail_id,64) IN(?,?) ORDER BY m.mail_id LIMIT ?) sample WHERE expire_ms=0") {
		t.Fatalf("抽样的分片条件必须在 LIMIT 之前:\n  %s", sample)
	}
	if got, want := strings.Count(sample, "?"), len(owned.Args())+1; got != want {
		t.Fatalf("抽样占位符 %d 个,Sweep 传 %d 个实参", got, want)
	}
	for _, query := range []string{candidates, sample} {
		if strings.Contains(strings.ToUpper(query), "FOR UPDATE") {
			t.Fatalf("候选读与抽样都必须是普通读: %s", query)
		}
	}
	if zero := sweepCandidateQuery(workshard.Filter{}); !strings.HasSuffix(zero, " AND 1=1 ORDER BY m.expire_ms,m.mail_id LIMIT ?") {
		t.Fatalf("零值条件不过滤: %s", zero)
	}
}

// 两个副本按 mail_id 互补分片:各自只删自己片里的过期邮件(连同 state 行),合起来一封不漏;一片都不归本副本时什么都不读、不删。
func TestTiDBSweepDeletesOnlyOwnedShards(t *testing.T) {
	ctx, db, repo := mailFixture(t)
	var ids []uint64
	for request := uint64(1); request <= 6; request++ {
		sent, err := repo.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(1100+request, 7))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sent.MailId)
	}
	if _, err := db.ExecContext(ctx, "UPDATE mail_message SET expire_ms=1"); err != nil {
		t.Fatal(err)
	}
	// 副本 a 负责前三封所在的片,副本 b 负责其余全部片。
	aShards := map[int]bool{}
	for _, id := range ids[:3] {
		aShards[workshard.ShardOf(id)] = true
	}
	for _, id := range ids[3:] {
		if aShards[workshard.ShardOf(id)] {
			t.Fatalf("夹具前提:前三封与后三封不在同一片(mail_id %v)", ids)
		}
	}
	ownsA := func(key string) bool {
		for shard := range aShards {
			if key == workshard.ShardKey(SweepWorkKind, shard) {
				return true
			}
		}
		return false
	}
	ownsB := func(key string) bool { return !ownsA(key) }
	cutoff := uint64(time.Now().UnixMilli())
	remainingMail := func() string {
		rows, err := db.QueryContext(ctx, "SELECT mail_id FROM mail_message ORDER BY mail_id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var left []uint64
		for rows.Next() {
			var id uint64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			left = append(left, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(left)
	}

	none := workshard.SQLFilter(func(string) bool { return false }, SweepWorkKind, SweepShardColumn)
	report, err := repo.Sweep(ctx, cutoff, 100, true, none)
	if err != nil || report.Personal.Candidates != 0 || report.Personal.Deleted != 0 {
		t.Fatalf("一片都不归本副本时不该读到、删到任何邮件: %+v %v", report, err)
	}
	report, err = repo.Sweep(ctx, cutoff, 100, true, workshard.SQLFilter(ownsA, SweepWorkKind, SweepShardColumn))
	if err != nil || report.Personal.Candidates != 3 || report.Personal.Deleted != 3 || report.SystemState.Deleted != 3 {
		t.Fatalf("副本 a 应只删自己片里的 3 封(连同 3 行 state): %+v %v", report, err)
	}
	if got, want := remainingMail(), fmt.Sprint(ids[3:]); got != want {
		t.Fatalf("副本 a 删完之后应只剩副本 b 的邮件: 剩 %s,应为 %s", got, want)
	}
	report, err = repo.Sweep(ctx, cutoff, 100, true, workshard.SQLFilter(ownsB, SweepWorkKind, SweepShardColumn))
	if err != nil || report.Personal.Deleted != 3 {
		t.Fatalf("副本 b 应删掉其余 3 封: %+v %v", report, err)
	}
	if got := remainingMail(); got != "[]" {
		t.Fatalf("两个副本合起来应一封不漏: 剩 %s", got)
	}
	var states int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_state").Scan(&states); err != nil || states != 0 {
		t.Fatalf("state 行应随邮件一起删掉: %d %v", states, err)
	}
}
