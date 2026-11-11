package workshard

import (
	"fmt"
	"testing"
	"time"
)

func TestOwnerDependsOnlyOnMemberSet(t *testing.T) {
	a := []string{"replica-a", "replica-b", "replica-c"}
	b := []string{"replica-c", "", "replica-a", "replica-b", "replica-a"}
	for shard := 0; shard < Shards; shard++ {
		key := ShardKey("trade-commerce", shard)
		if Owner(key, a) != Owner(key, b) {
			t.Fatalf("片 %d 的归属随成员顺序或空成员变了", shard)
		}
	}
	if Owner("any", nil) != "" || Owner("any", []string{""}) != "" {
		t.Fatal("没有成员时不应有负责者")
	}
}

// 固定的相邻副本名和短数字后缀也应分散到各副本，不能让一份前缀分数支配整组分片。
func TestOwnerSpreadsShortShardKeysAcrossMembers(t *testing.T) {
	members := []string{"uuid-00", "uuid-01", "uuid-02", "uuid-03", "uuid-new"}
	for _, prefix := range []string{"", "3:", "formation:", "team-launch:", "team-idle:", "ready-ticket:", "rank-source:3:", "trade-commerce:", "trade-receipt-ack:",
		// 缺口清单第 25 条:资产指令重投与终局发件箱的类别键。
		"trade-assetop:", "guild-assetop:", "outcome-result:", "outcome-settlement:"} {
		t.Run(prefix, func(t *testing.T) {
			counts := make(map[string]int, len(members))
			for shard := 0; shard < Shards; shard++ {
				counts[Owner(fmt.Sprintf("%s%d", prefix, shard), members)]++
			}
			t.Logf("分片分布: %v", counts)
			for _, member := range members {
				if counts[member] == 0 {
					t.Errorf("副本 %s 没有分到任何分片", member)
				}
			}
		})
	}
}

// 移除副本只重分配原属它的键，其余副本的已有工作不换主。
func TestRemovingAMemberOnlyMovesItsShards(t *testing.T) {
	before := []string{"uuid-00", "uuid-01", "uuid-02", "uuid-03", "uuid-new"}
	after := []string{"uuid-00", "uuid-01", "uuid-03", "uuid-new"}
	moved := 0
	for shard := 0; shard < Shards; shard++ {
		key := ShardKey("formation", shard)
		was, now := Owner(key, before), Owner(key, after)
		if now == "" || now == "uuid-02" {
			t.Fatalf("片 %d 没有归属到存活副本: %q", shard, now)
		}
		if was != now {
			if was != "uuid-02" {
				t.Fatalf("片 %d 从存活副本 %s 换到了 %s", shard, was, now)
			}
			moved++
		}
	}
	if moved == 0 || moved == Shards {
		t.Fatalf("移除成员后迁移 %d 片，应只有原属它的一部分片", moved)
	}
}

// 加一个成员只从原有成员手里挪走一部分片,其余片不动;每一片恰好一个负责者。
func TestAddingAMemberOnlyMovesItsShards(t *testing.T) {
	before := []string{"replica-a", "replica-b"}
	after := []string{"replica-a", "replica-b", "replica-c"}
	moved := 0
	for shard := 0; shard < Shards; shard++ {
		key := ShardKey("trade-commerce", shard)
		was, now := Owner(key, before), Owner(key, after)
		if was != now {
			if now != "replica-c" {
				t.Fatalf("片 %d 从 %s 挪到了 %s,只应挪给新成员", shard, was, now)
			}
			moved++
		}
		owners := 0
		for _, member := range after {
			if Owns(member, key, after) {
				owners++
			}
		}
		if owners != 1 {
			t.Fatalf("片 %d 有 %d 个负责者", shard, owners)
		}
	}
	if moved == 0 || moved == Shards {
		t.Fatalf("新成员拿到 %d 片,应只拿走一部分", moved)
	}
	if Owns("", ShardKey("trade-commerce", 0), after) || Owns("stranger", ShardKey("trade-commerce", 0), after) {
		t.Fatal("空身份或不在成员表里的副本不负责任何片")
	}
}

func TestSQLFilter(t *testing.T) {
	if f := SQLFilter(nil, "k", "commerce_id"); f.SQL() != "1=1" || len(f.Args()) != 0 {
		t.Fatalf("没装配分工应不过滤: %q %v", f.SQL(), f.Args())
	}
	if f := SQLFilter(func(string) bool { return true }, "k", "commerce_id"); f.SQL() != "1=1" || len(f.Args()) != 0 {
		t.Fatalf("全部片都归本副本应不过滤: %q", f.SQL())
	}
	if f := SQLFilter(func(string) bool { return false }, "k", "commerce_id"); f.SQL() != "1=0" || len(f.Args()) != 0 {
		t.Fatalf("一片都不归应回恒假: %q", f.SQL())
	}
	owned := map[string]bool{ShardKey("k", 3): true, ShardKey("k", 40): true}
	f := SQLFilter(func(key string) bool { return owned[key] }, "k", "commerce_id")
	if f.SQL() != "MOD(commerce_id,64) IN(?,?)" {
		t.Fatalf("部分片的条件不对: %q", f.SQL())
	}
	if fmt.Sprint(f.Args()) != "[3 40]" {
		t.Fatalf("片号应按升序只走绑定参数: %v", f.Args())
	}
	if (Filter{}).SQL() != "1=1" {
		t.Fatal("零值 Filter 应不过滤")
	}
	if ShardOf(64*7+40) != 40 {
		t.Fatal("ShardOf 应与 MOD(column,64) 同一个算法")
	}
}

func TestStableMembersSkipsNonMembers(t *testing.T) {
	regs := []Registration{
		{Key: "TradeNodeService.rpc/zone/1/node_type/9/node_id/2", Value: []byte(`{"nodeUuid":"uuid-b","releaseTrack":"stable"}`)},
		{Key: "TradeNodeService.rpc/zone/1/node_type/9/node_id/1", Value: []byte(`{"nodeUuid":"uuid-a"}`)}, // 没写轨道 = stable
		{Key: "TradeNodeService.rpc/zone/1/node_type/9/node_id/3", Value: []byte(`{"nodeUuid":"uuid-c","releaseTrack":"canary"}`)},
		{Key: "TradeNodeService.rpc/zone/1/node_type/9/node_id/4", Value: []byte(`{"nodeUuid":"uuid-d","releaseTrack":"beta"}`)},
		{Key: "TradeNodeService.rpc/zone/1/node_type/9/node_id/5", Value: []byte(`not json`)},
		{Key: "TradeNodeService.rpc/zone/1/node_type/9/node_id/6", Value: []byte(`{"nodeId":6}`)},
		{Key: "TradeNodeService.rpc/allocated/node_type/9/node_id/7", Value: []byte(`{"nodeUuid":"uuid-e"}`)},
		{Key: "TradeNodeService.rpc/zone/1/node_type/9/node_id/8", Value: []byte(`{"nodeUuid":"uuid-a","releaseTrack":""}`)}, // 重复
	}
	got := StableMembers(regs)
	if fmt.Sprint(got) != "[uuid-a uuid-b]" {
		t.Fatalf("成员表只应有 stable 节点、升序去重: %v", got)
	}
}

func TestMembershipOwnsRules(t *testing.T) {
	clock := time.UnixMilli(1_000_000)
	m := NewMembership("TradeNodeService.rpc/zone/1/")
	m.now = func() time.Time { return clock }
	key := ShardKey("trade-commerce", 5)

	if !m.Owns(key) {
		t.Fatal("还没读到成员表时应做全部片(保活优先)")
	}
	m.listed([]string{"uuid-a", "uuid-b"})
	if m.Owns(key) {
		t.Fatal("成员表不空、本副本还没注册时什么都不做")
	}
	m.SetSelf("uuid-a")
	if m.Owns(key) != Owns("uuid-a", key, []string{"uuid-a", "uuid-b"}) {
		t.Fatal("注册之后按最高随机权重哈希判")
	}
	m.listed([]string{})
	if !m.Owns(key) {
		t.Fatal("读到空成员表时应做全部片")
	}
	m.listed([]string{"uuid-b"})
	if m.Owns(key) {
		t.Fatal("本副本不在成员表里时什么都不做")
	}
	clock = clock.Add(StaleAfter + time.Millisecond)
	if m.Members() != nil || !m.Owns(key) {
		t.Fatal("成员表过期后应当作为空,做全部片")
	}
	var unwired *Membership
	if !unwired.Owns(key) || unwired.Members() != nil {
		t.Fatal("没装配成员表时全部归本副本")
	}
}
