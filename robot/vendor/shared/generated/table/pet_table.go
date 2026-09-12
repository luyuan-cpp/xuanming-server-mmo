
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



// petSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type petSnapshot struct {
    data   []*pb.PetTable
    kvData map[uint32]*pb.PetTable
    idxAptitudeMin map[uint32][]*pb.PetTable
    idxAptitudeMax map[uint32][]*pb.PetTable
    idxSkill map[uint32][]*pb.PetTable
}

type PetTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[petSnapshot]
}

var PetTableManagerInstance = NewPetTableManager()

func NewPetTableManager() *PetTableManager {
    m := &PetTableManager{}
    m.snap.Store(&petSnapshot{
        kvData: make(map[uint32]*pb.PetTable),
        idxAptitudeMin: make(map[uint32][]*pb.PetTable),
        idxAptitudeMax: make(map[uint32][]*pb.PetTable),
        idxSkill: make(map[uint32][]*pb.PetTable),
    })
    return m
}

func (m *PetTableManager) Load(configDir string, useBinary bool) error {
    var container pb.PetTableData

    if useBinary {
        path := filepath.Join(configDir, "pet.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "pet.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &petSnapshot{
        kvData: make(map[uint32]*pb.PetTable, len(container.Data)),
        idxAptitudeMin: make(map[uint32][]*pb.PetTable),
        idxAptitudeMax: make(map[uint32][]*pb.PetTable),
        idxSkill: make(map[uint32][]*pb.PetTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        for _, elem := range row.AptitudeMin {
            snap.idxAptitudeMin[elem] = append(snap.idxAptitudeMin[elem], row)
        }
        for _, elem := range row.AptitudeMax {
            snap.idxAptitudeMax[elem] = append(snap.idxAptitudeMax[elem], row)
        }
        for _, elem := range row.Skill {
            snap.idxSkill[elem] = append(snap.idxSkill[elem], row)
        }
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *PetTableManager) FindAll() []*pb.PetTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *PetTableManager) FindById(id uint32) (*pb.PetTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *PetTableManager) FindByAptitudeMinIndex(key uint32) []*pb.PetTable {
    snap := m.snap.Load()
    return snap.idxAptitudeMin[key]
}


func (m *PetTableManager) FindByAptitudeMaxIndex(key uint32) []*pb.PetTable {
    snap := m.snap.Load()
    return snap.idxAptitudeMax[key]
}


func (m *PetTableManager) FindBySkillIndex(key uint32) []*pb.PetTable {
    snap := m.snap.Load()
    return snap.idxSkill[key]
}



// ---- Exists ----

func (m *PetTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *PetTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *PetTableManager) CountByAptitudeMinIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxAptitudeMin[key])
}


func (m *PetTableManager) CountByAptitudeMaxIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxAptitudeMax[key])
}


func (m *PetTableManager) CountBySkillIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxSkill[key])
}



// ---- FindByIds (IN) ----

func (m *PetTableManager) FindByIds(ids []uint32) []*pb.PetTable {
    snap := m.snap.Load()
    result := make([]*pb.PetTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *PetTableManager) RandOne() (*pb.PetTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *PetTableManager) Where(pred func(*pb.PetTable) bool) []*pb.PetTable {
    snap := m.snap.Load()
    var result []*pb.PetTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *PetTableManager) First(pred func(*pb.PetTable) bool) (*pb.PetTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

