package battlescript

import (
	"shared/generated/table"
	pbtable "shared/generated/pb/table"
)

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
