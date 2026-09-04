
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



// conditionSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type conditionSnapshot struct {
    data   []*pb.ConditionTable
    kvData map[uint32]*pb.ConditionTable
    idxCondition1 map[uint32][]*pb.ConditionTable
    idxCondition2 map[uint32][]*pb.ConditionTable
    idxCondition3 map[uint32][]*pb.ConditionTable
    idxCondition4 map[uint32][]*pb.ConditionTable
}

type ConditionTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[conditionSnapshot]
}

var ConditionTableManagerInstance = NewConditionTableManager()

func NewConditionTableManager() *ConditionTableManager {
    m := &ConditionTableManager{}
    m.snap.Store(&conditionSnapshot{
        kvData: make(map[uint32]*pb.ConditionTable),
        idxCondition1: make(map[uint32][]*pb.ConditionTable),
        idxCondition2: make(map[uint32][]*pb.ConditionTable),
        idxCondition3: make(map[uint32][]*pb.ConditionTable),
        idxCondition4: make(map[uint32][]*pb.ConditionTable),
    })
    return m
}

func (m *ConditionTableManager) Load(configDir string, useBinary bool) error {
    var container pb.ConditionTableData

    if useBinary {
        path := filepath.Join(configDir, "condition.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "condition.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &conditionSnapshot{
        kvData: make(map[uint32]*pb.ConditionTable, len(container.Data)),
        idxCondition1: make(map[uint32][]*pb.ConditionTable),
        idxCondition2: make(map[uint32][]*pb.ConditionTable),
        idxCondition3: make(map[uint32][]*pb.ConditionTable),
        idxCondition4: make(map[uint32][]*pb.ConditionTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        for _, elem := range row.Condition1 {
            snap.idxCondition1[elem] = append(snap.idxCondition1[elem], row)
        }
        for _, elem := range row.Condition2 {
            snap.idxCondition2[elem] = append(snap.idxCondition2[elem], row)
        }
        for _, elem := range row.Condition3 {
            snap.idxCondition3[elem] = append(snap.idxCondition3[elem], row)
        }
        for _, elem := range row.Condition4 {
            snap.idxCondition4[elem] = append(snap.idxCondition4[elem], row)
        }
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *ConditionTableManager) FindAll() []*pb.ConditionTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *ConditionTableManager) FindById(id uint32) (*pb.ConditionTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *ConditionTableManager) FindByCondition1Index(key uint32) []*pb.ConditionTable {
    snap := m.snap.Load()
    return snap.idxCondition1[key]
}


func (m *ConditionTableManager) FindByCondition2Index(key uint32) []*pb.ConditionTable {
    snap := m.snap.Load()
    return snap.idxCondition2[key]
}


func (m *ConditionTableManager) FindByCondition3Index(key uint32) []*pb.ConditionTable {
    snap := m.snap.Load()
    return snap.idxCondition3[key]
}


func (m *ConditionTableManager) FindByCondition4Index(key uint32) []*pb.ConditionTable {
    snap := m.snap.Load()
    return snap.idxCondition4[key]
}



// ---- Exists ----

func (m *ConditionTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *ConditionTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *ConditionTableManager) CountByCondition1Index(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxCondition1[key])
}


func (m *ConditionTableManager) CountByCondition2Index(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxCondition2[key])
}


func (m *ConditionTableManager) CountByCondition3Index(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxCondition3[key])
}


func (m *ConditionTableManager) CountByCondition4Index(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxCondition4[key])
}



// ---- FindByIds (IN) ----

func (m *ConditionTableManager) FindByIds(ids []uint32) []*pb.ConditionTable {
    snap := m.snap.Load()
    result := make([]*pb.ConditionTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *ConditionTableManager) RandOne() (*pb.ConditionTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *ConditionTableManager) Where(pred func(*pb.ConditionTable) bool) []*pb.ConditionTable {
    snap := m.snap.Load()
    var result []*pb.ConditionTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *ConditionTableManager) First(pred func(*pb.ConditionTable) bool) (*pb.ConditionTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

