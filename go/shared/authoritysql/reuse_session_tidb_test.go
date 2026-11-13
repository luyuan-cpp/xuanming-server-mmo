package authoritysql_test

import (
	"testing"
	"time"

	"shared/authoritysql"
)

// 连接复用时不重放会话设置(connector.revalidate)。它的前提之一「驱动复用连接时不重置服务端会话」只靠读
// go-sql-driver 源码确认,这里在真库上钉住:池上限压到 1,同一条物理连接反复借出、归还,
// 建连时固定的强读会话(AssertCurrentSession 读回的那一组)原样还在,连接也没有被重建。
// 最后一轮先空闲超过探测窗口再借出:在真库上走一次空闲探测,探测通过、仍是同一条连接。
// 驱动升级后开始在复用时重置会话,或者探测读回的列形状和这边认的不一样(好连接被判成坏连接),这条用例先红。
// 需要真实库与权限资格原件,门控同 twoLogicalDBFixture;MySQL 与 TiDB 各跑一轮。
func TestPinnedSessionSurvivesPoolReuse(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	db, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 开池时的核对可能同时占两条连接,所以开完再压到 1:此后每次借出都是同一条物理连接。
	db.SetMaxOpenConns(1)
	connectionID := func() uint64 {
		t.Helper()
		var id uint64
		if err := db.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := connectionID()
	const rounds = 4
	for round := 1; round <= rounds; round++ {
		if round == rounds {
			// 一次有上限的等待:让连接空闲超过探测窗口,下一次借出才会发探测。
			time.Sleep(authoritysql.IdleProbeAfterForTest + 200*time.Millisecond)
		}
		if err = bumpGateBusiness(ctx, db); err != nil {
			t.Fatalf("第 %d 轮写: %v", round, err)
		}
		if err = authoritysql.AssertCurrentSession(ctx, db, fixture.Backend()); err != nil {
			t.Fatalf("第 %d 轮复用之后强读会话变了: %v", round, err)
		}
		if id := connectionID(); id != first {
			t.Fatalf("第 %d 轮连接被重建了(%d -> %d):复用核对把好连接判成了坏连接,或者连接真的断过", round, first, id)
		}
	}
}
