package battlescript

import (
	"fmt"

	"shared/generated/table"
	pbtable "shared/generated/pb/table"
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
