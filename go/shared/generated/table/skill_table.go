
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



// skillSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type skillSnapshot struct {
    data   []*pb.SkillTable
    kvData map[uint32]*pb.SkillTable
    idxSkill_type map[uint32][]*pb.SkillTable
    idxTargeting_mode map[uint32][]*pb.SkillTable
    idxEffect map[uint32][]*pb.SkillTable
}

type SkillTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[skillSnapshot]
}

var SkillTableManagerInstance = NewSkillTableManager()

func NewSkillTableManager() *SkillTableManager {
    m := &SkillTableManager{}
    m.snap.Store(&skillSnapshot{
        kvData: make(map[uint32]*pb.SkillTable),
        idxSkill_type: make(map[uint32][]*pb.SkillTable),
        idxTargeting_mode: make(map[uint32][]*pb.SkillTable),
        idxEffect: make(map[uint32][]*pb.SkillTable),
    })
    return m
}

func (m *SkillTableManager) Load(configDir string, useBinary bool) error {
    var container pb.SkillTableData

    if useBinary {
        path := filepath.Join(configDir, "skill.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "skill.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &skillSnapshot{
        kvData: make(map[uint32]*pb.SkillTable, len(container.Data)),
        idxSkill_type: make(map[uint32][]*pb.SkillTable),
        idxTargeting_mode: make(map[uint32][]*pb.SkillTable),
        idxEffect: make(map[uint32][]*pb.SkillTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        for _, elem := range row.SkillType {
            snap.idxSkill_type[elem] = append(snap.idxSkill_type[elem], row)
        }
        for _, elem := range row.TargetingMode {
            snap.idxTargeting_mode[elem] = append(snap.idxTargeting_mode[elem], row)
        }
        for _, elem := range row.Effect {
            snap.idxEffect[elem] = append(snap.idxEffect[elem], row)
        }
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *SkillTableManager) FindAll() []*pb.SkillTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *SkillTableManager) FindById(id uint32) (*pb.SkillTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *SkillTableManager) FindBySkill_typeIndex(key uint32) []*pb.SkillTable {
    snap := m.snap.Load()
    return snap.idxSkill_type[key]
}


func (m *SkillTableManager) FindByTargeting_modeIndex(key uint32) []*pb.SkillTable {
    snap := m.snap.Load()
    return snap.idxTargeting_mode[key]
}


func (m *SkillTableManager) FindByEffectIndex(key uint32) []*pb.SkillTable {
    snap := m.snap.Load()
    return snap.idxEffect[key]
}



// ---- Exists ----

func (m *SkillTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *SkillTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *SkillTableManager) CountBySkill_typeIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxSkill_type[key])
}


func (m *SkillTableManager) CountByTargeting_modeIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxTargeting_mode[key])
}


func (m *SkillTableManager) CountByEffectIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxEffect[key])
}



// ---- FindByIds (IN) ----

func (m *SkillTableManager) FindByIds(ids []uint32) []*pb.SkillTable {
    snap := m.snap.Load()
    result := make([]*pb.SkillTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *SkillTableManager) RandOne() (*pb.SkillTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *SkillTableManager) Where(pred func(*pb.SkillTable) bool) []*pb.SkillTable {
    snap := m.snap.Load()
    var result []*pb.SkillTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *SkillTableManager) First(pred func(*pb.SkillTable) bool) (*pb.SkillTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

