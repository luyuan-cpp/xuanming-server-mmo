package battlescript

import (
	"testing"

	"shared/generated/table"
	pbtable "shared/generated/pb/table"
)

// 内置官方配置对真实导出表(仓库根 generated/tables)整份校验:剧本引用的技能、Buff、怪物、副本都还在,
// 职业与目标阵营对得上。策划改表删了被剧本引用的行,这条先红,而不是等 match 上线时拒启。
// 用新建的表管理器读真表,不动全局管理器;读不到表判红,不跳过(跳过 = 绿色但没验)。
const realTableDir = "../../../../generated/tables"

type managerTables struct {
	skills   *table.SkillTableManager
	buffs    *table.BuffTableManager
	monsters *table.MonsterTableManager
	dungeons *table.DungeonTableManager
}

func (m managerTables) Skill(id uint32) (*pbtable.SkillTable, bool) { return m.skills.FindById(id) }

func (m managerTables) HasBuff(id uint32) bool {
	_, ok := m.buffs.FindById(id)
	return ok
}

func (m managerTables) HasMonster(id uint32) bool {
	_, ok := m.monsters.FindById(id)
	return ok
}

func (m managerTables) HasDungeon(id uint32) bool {
	_, ok := m.dungeons.FindById(id)
	return ok
}

func TestEmbeddedOfficialConfigValidatesAgainstExportedTables(t *testing.T) {
	tables := managerTables{
		skills:   table.NewSkillTableManager(),
		buffs:    table.NewBuffTableManager(),
		monsters: table.NewMonsterTableManager(),
		dungeons: table.NewDungeonTableManager(),
	}
	loads := map[string]func(string, bool) error{
		"Skill": tables.skills.Load, "Buff": tables.buffs.Load,
		"Monster": tables.monsters.Load, "Dungeon": tables.dungeons.Load,
	}
	for name, load := range loads {
		if err := load(realTableDir, false); err != nil {
			t.Fatalf("load %s from %s: %v", name, realTableDir, err)
		}
	}
	catalog, err := LoadOfficial(tables)
	if err != nil {
		t.Fatalf("embedded official_scripts.json no longer validates against the exported tables: %v", err)
	}
	if len(catalog.OfficialScripts()) == 0 {
		t.Fatal("official config must ship at least one script")
	}
}
