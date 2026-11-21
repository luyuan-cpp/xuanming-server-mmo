package battlescript

import (
	"fmt"

	pbtable "shared/generated/pb/table"
	"shared/generated/table"
)

// LoadWithTables 读校验要查的四张配表(Skill / Buff / Monster / Dungeon,与读表的先后无关),
// 再解析并校验内置官方剧本。任何一步失败都返回错误,match 据此拒绝启动
func LoadWithTables(tableDir string) (*Catalog, error) {
	loads := []struct {
		name string
		load func(string, bool) error
	}{
		{"Skill", table.SkillTableManagerInstance.Load},
		{"Buff", table.BuffTableManagerInstance.Load},
		{"Monster", table.MonsterTableManagerInstance.Load},
		{"Dungeon", table.DungeonTableManagerInstance.Load},
	}
	for _, item := range loads {
		if err := item.load(tableDir, false); err != nil {
			return nil, fmt.Errorf("battlescript: load %s table: %w", item.name, err)
		}
	}
	return LoadOfficial(TableLookup{})
}

// LoadFromTableSet 用候选 TableSet 校验并编译内置官方剧本,不触碰全局单例(两阶段热更与 --validate-tables 专用)。
func LoadFromTableSet(ts *table.TableSet) (*Catalog, error) {
	if ts == nil || ts.Skill == nil || ts.Buff == nil || ts.Monster == nil || ts.Dungeon == nil {
		return nil, fmt.Errorf("battlescript: TableSet missing Skill/Buff/Monster/Dungeon tables")
	}
	return LoadOfficial(TableSetLookup{Tables: ts})
}

// TableSetLookup 在一套给定的 TableSet 内查表,保证一次跨表校验或解析看到的四张表来自同一版本。
type TableSetLookup struct {
	Tables *table.TableSet
}

func (l TableSetLookup) Skill(id uint32) (*pbtable.SkillTable, bool) {
	if l.Tables == nil || l.Tables.Skill == nil {
		return table.SkillTableManagerInstance.FindById(id)
	}
	return l.Tables.Skill.FindById(id)
}

func (l TableSetLookup) HasBuff(id uint32) bool {
	if l.Tables == nil || l.Tables.Buff == nil {
		_, ok := table.BuffTableManagerInstance.FindById(id)
		return ok
	}
	_, ok := l.Tables.Buff.FindById(id)
	return ok
}

func (l TableSetLookup) HasMonster(id uint32) bool {
	if l.Tables == nil || l.Tables.Monster == nil {
		_, ok := table.MonsterTableManagerInstance.FindById(id)
		return ok
	}
	_, ok := l.Tables.Monster.FindById(id)
	return ok
}

func (l TableSetLookup) HasDungeon(id uint32) bool {
	if l.Tables == nil || l.Tables.Dungeon == nil {
		_, ok := table.DungeonTableManagerInstance.FindById(id)
		return ok
	}
	_, ok := l.Tables.Dungeon.FindById(id)
	return ok
}

// TableLookup 是生产用的 Tables:读全局表管理器(match 启动时 table.LoadTables 已整批加载)。
// 不缓存行指针:每次现查,热更后自然读到新快照(data/AGENTS.md「只存 id」)
type TableLookup struct{}

func (TableLookup) Skill(id uint32) (*pbtable.SkillTable, bool) {
	return table.SkillTableManagerInstance.FindById(id)
}

func (TableLookup) HasBuff(id uint32) bool {
	_, ok := table.BuffTableManagerInstance.FindById(id)
	return ok
}

func (TableLookup) HasMonster(id uint32) bool {
	_, ok := table.MonsterTableManagerInstance.FindById(id)
	return ok
}

func (TableLookup) HasDungeon(id uint32) bool {
	_, ok := table.DungeonTableManagerInstance.FindById(id)
	return ok
}
