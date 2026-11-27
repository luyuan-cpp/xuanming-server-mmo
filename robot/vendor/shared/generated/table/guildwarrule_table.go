
package table

import (
    "fmt"
    "math/rand/v2"
    "os"
    "path/filepath"
    "sync/atomic"

    "google.golang.org/protobuf/encoding/protojson"
    "google.golang.org/protobuf/proto"
    pb "shared/generated/pb/table"
)



// guildwarruleSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type guildwarruleSnapshot struct {
    data   []*pb.GuildWarRuleTable
    kvData map[uint32]*pb.GuildWarRuleTable
    idxBattleConfigId map[uint32][]*pb.GuildWarRuleTable
}

type GuildWarRuleTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[guildwarruleSnapshot]
}

var GuildWarRuleTableManagerInstance = NewGuildWarRuleTableManager()

func NewGuildWarRuleTableManager() *GuildWarRuleTableManager {
    m := &GuildWarRuleTableManager{}
    m.snap.Store(&guildwarruleSnapshot{
        kvData: make(map[uint32]*pb.GuildWarRuleTable),
        idxBattleConfigId: make(map[uint32][]*pb.GuildWarRuleTable),
    })
    return m
}

func (m *GuildWarRuleTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *GuildWarRuleTableManager) swapSnapshot(snap *guildwarruleSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *GuildWarRuleTableManager) loadSnapshot(configDir string, useBinary bool) (*guildwarruleSnapshot, error) {
    var container pb.GuildWarRuleTableData

    if useBinary {
        path := filepath.Join(configDir, "guildwarrule.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "guildwarrule.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &guildwarruleSnapshot{
        kvData: make(map[uint32]*pb.GuildWarRuleTable, len(container.Data)),
        idxBattleConfigId: make(map[uint32][]*pb.GuildWarRuleTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxBattleConfigId[row.BattleConfigId] = append(snap.idxBattleConfigId[row.BattleConfigId], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *GuildWarRuleTableManager) FindAll() []*pb.GuildWarRuleTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *GuildWarRuleTableManager) FindById(id uint32) (*pb.GuildWarRuleTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *GuildWarRuleTableManager) GetByBattleConfigId(key uint32) []*pb.GuildWarRuleTable {
    snap := m.snap.Load()
    return snap.idxBattleConfigId[key]
}



// ---- Exists ----

func (m *GuildWarRuleTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *GuildWarRuleTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *GuildWarRuleTableManager) CountByBattleConfigIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxBattleConfigId[key])
}



// ---- FindByIds (IN) ----

func (m *GuildWarRuleTableManager) FindByIds(ids []uint32) []*pb.GuildWarRuleTable {
    snap := m.snap.Load()
    result := make([]*pb.GuildWarRuleTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *GuildWarRuleTableManager) RandOne() (*pb.GuildWarRuleTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *GuildWarRuleTableManager) Where(pred func(*pb.GuildWarRuleTable) bool) []*pb.GuildWarRuleTable {
    snap := m.snap.Load()
    var result []*pb.GuildWarRuleTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *GuildWarRuleTableManager) First(pred func(*pb.GuildWarRuleTable) bool) (*pb.GuildWarRuleTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: battle_config_id → Dungeon.id


// ---- Composite Key ----

