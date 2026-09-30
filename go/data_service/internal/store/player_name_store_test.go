package store

import (
	"database/sql/driver"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TestPlayerNameAttemptsFloor 守住 Reserve / Release 两个尝试上限的下限 2(理由见各自的常量注释):
//   - playerNameReserveAttempts 调成 1 时,「残余」A(锁继承)与 B(同一 player_id 上的并发重名登记)
//     两类 InnoDB 固有情形里被牺牲的那一方会把 1213 直接当存储错误返回,玩家看到的是建角失败,
//     而不是"名字已被占用";迁移没跑过的库(主键还是 player_id)上,名字抢注本身也还会成环,
//     那个窗口里这次重来是唯一的兜底。
//   - playerNameReleaseAttempts 调成 1 时,Release 的「情形 3」(行在且落在窗口内却没删到 —— 在途 INSERT 在
//     DELETE 之后才提交)永远不会重删:循环只跑一轮就落到 exhausted 分支返回存储错误。而这正是 login 两次
//     补偿释放要覆盖的竞态,退化的后果是每撞上一次就留一条孤儿名字行,且是静默的(只有 orphan 计数会涨)。
func TestPlayerNameAttemptsFloor(t *testing.T) {
	const floor = 2
	if playerNameReserveAttempts < floor {
		t.Fatalf("playerNameReserveAttempts=%d < %d:锁冲突重试与\"撞键后行已消失\"的重插共用这一个预算,"+
			"至少要能重来一次,Reserve 才能在固有的 1213 之后收敛到 Taken", playerNameReserveAttempts, floor)
	}
	if playerNameReleaseAttempts < floor {
		t.Fatalf("playerNameReleaseAttempts=%d < %d:少了这一次重来,在途 INSERT 与 Release 交叉时必留孤儿名字行"+
			"(Release 的「情形 3」再也走不到重删,直接按存储错误返回)", playerNameReleaseAttempts, floor)
	}
}

// TestStoreDSNDoesNotSetClientFoundRows 钉住 Reserve 判定口径的前提(见 Reserve 的「前提 1」):
// 连接带 CLIENT_FOUND_ROWS 时,no-op 的 ODKU 也回 1 行,与"插入成功"无法区分 —— 撞名会被当成 Inserted,
// 而名字其实属于别人,且全程零报错。任何人给 buildDSN 加上 clientFoundRows=true,本用例立刻变红。
func TestStoreDSNDoesNotSetClientFoundRows(t *testing.T) {
	cfg, err := mysql.ParseDSN(buildDSN(MySQLConfig{Host: "127.0.0.1:3306", User: "u", Password: "p", DBName: "d"}))
	if err != nil {
		t.Fatalf("buildDSN 产出的 DSN 解析失败: %v", err)
	}
	if cfg.ClientFoundRows {
		t.Fatal("buildDSN 设了 clientFoundRows=true:PlayerNameStore.Reserve 靠 ODKU 的受影响行数(1=插入 / 0=撞键 no-op)判定终局," +
			"带了它 no-op 也回 1,撞名会被误判成插入成功")
	}
}

// TestReserveInsertedRowsAffectedContract:1 = 插入,0 = 撞键 no-op,其它一律报错(不猜)。
func TestReserveInsertedRowsAffectedContract(t *testing.T) {
	for _, tc := range []struct {
		affected int64
		inserted bool
		wantErr  bool
	}{
		{affected: 1, inserted: true},
		{affected: 0, inserted: false},
		{affected: 2, wantErr: true}, // ODKU 真的改了一行:更新子句不再是 no-op
	} {
		inserted, err := reserveInserted(driver.RowsAffected(tc.affected), 42)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("affected=%d 应 fail-closed 报错,got inserted=%v", tc.affected, inserted)
			}
			continue
		}
		if err != nil || inserted != tc.inserted {
			t.Fatalf("affected=%d:want inserted=%v,got inserted=%v err=%v", tc.affected, tc.inserted, inserted, err)
		}
	}
}

// TestPlayerNameLockRetryBackoff:退避落在区间内,且单次调用最坏多睡的时长不超过 login 单次预算(1s)的十分之一。
func TestPlayerNameLockRetryBackoff(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := jitteredBackoff(playerNameLockRetryBackoffMin, playerNameLockRetryBackoffMax)
		if d < playerNameLockRetryBackoffMin || d > playerNameLockRetryBackoffMax {
			t.Fatalf("第 %d 次退避 %v 超出 [%v, %v]", i, d, playerNameLockRetryBackoffMin, playerNameLockRetryBackoffMax)
		}
	}
	for _, attempts := range []int{playerNameReserveAttempts, playerNameReleaseAttempts} {
		if worst := time.Duration(attempts-1) * playerNameLockRetryBackoffMax; worst > 100*time.Millisecond {
			t.Fatalf("锁冲突重试最坏多睡 %v,超过 100ms:会吃掉 login 单次 Reserve/Release 1s 预算的一成以上", worst)
		}
	}
}
