// Package tablequery 提供配置表的通用投影查询(手写,**非生成产物**)。
//
// 为什么单独开一个包,而不是放进 shared/generated/table:
// CLAUDE.md §3 明确「不要手改 generated/ 或各语言 proto 输出树下的生成产物」,而
// go/shared/generated 整棵树是 tools/data_table_exporter 的 deploy 目标
// (exporter_config.yaml 的 languages.go.deploy)。虽然 md5_copy_dir 只覆盖同名文件、
// 不删多余文件,手写代码放进去仍然是在生成树里埋人肉资产,下一个人分不清哪些能改。
// 本包与 table 包零耦合——泛型只依赖行 message 的 GetId() 与调用方传进来的取值函数,
// 不 import table,也不 import pb。
//
// 为什么不进 templates/go_config.go.j2:
// 这几个函数只依赖「每行都有 uint32 主键」这一条全表共有的形状(20 张表的行 message
// 都有 GetId() uint32),泛型一次写完即覆盖全部表,加新表零成本。渲染进模板意味着 20 份
// 逐表副本、模板变长、每次改动都要全量重导表重部署,而且在出现第一个消费者之前先摊 20 份
// 死代码。生成器只该生成手写不出来的东西——索引结构与类型安全的按键查询。
//
// 与快照 / 热更的关系(**重要,和 Pandora 不同**):
// 本仓 Go 侧配置表**没有热更**——各服务在 internal/svc/servicecontext.go 构造时调用
// table.LoadTables() 一次,之后不再重载。所以 FindAll() 返回的行切片在进程生命周期内
// 恒定,本包返回的结果不存在「跟不跟随热更」的问题。
//
// 若将来要加热更,必须先修 TableManager 的快照切换:当前模板生成的是
// `m.snap = snap` 裸赋值(go_config.go.j2 的 Load),读侧 `m.snap.kvData[id]` 无任何同步。
// 今天不是竞态只因为 Load 发生在任何 goroutine 读表之前;一旦有第二次 Load,它立刻变成
// 真实 data race(go test -race 会报)。届时的正确做法是把 snap 换成 atomic.Pointer,
// **不是**在本包里补锁——本包只读调用方传进来的切片,补不了上游的洞。
//
// 所有权:返回值一律是**调用方独占的新切片**,不是表内部切片的别名,可随意 sort / 持有 /
// 修改,不会影响表。代价是每次一次分配;表规模是几十到几百行,量级微秒,且这些 API 都不在
// 每帧 / 每 tick 路径上(真正的热路径是 FindById 的 O(1) map 查,本包完全不碰)。
// 若哪天某个调用点进了逐帧路径,再改成 Load 期预计算——那时零分配才值钱,也才值得为它
// 承担共享切片的所有权风险。
package tablequery

import (
	"cmp"
	"slices"
)

// RowWithID 是全部配置表行 message 的公共形状(pb.<Sheet>Table 都满足)。
type RowWithID interface{ GetId() uint32 }

// IDs 按加载序取全部主键(与 FindAll() 一一对应)。
//
//	ids := tablequery.IDs(table.ItemTableManagerInstance.FindAll())
func IDs[R RowWithID](rows []R) []uint32 {
	return Values(rows, func(row R) uint32 { return row.GetId() })
}

// Values 按加载序取某列取值(投影)。**不去重**,长度与 rows 一致,要键集合用 DistinctValues。
//
//	classIDs := tablequery.Values(table.MonsterTableManagerInstance.FindAll(),
//		(*pb.MonsterTable).GetClassId)
func Values[R, V any](rows []R, get func(R) V) []V {
	out := make([]V, 0, len(rows))
	for _, row := range rows {
		out = append(out, get(row))
	}
	return out
}

// DistinctValues 取某列的去重取值(键集合),**按升序**返回。
//
// 升序而非加载序是有意的:键集合没有「表里的顺序」这一说,升序是唯一规范序,
// 也让结果与行的排列无关(同一批数据换个行序,键集合完全一致)。需要加载序用 Values。
func DistinctValues[R any, V cmp.Ordered](rows []R, get func(R) V) []V {
	out := Values(rows, get)
	slices.Sort(out)
	return slices.Compact(out)
}
