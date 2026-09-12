
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



// attributeautoplanSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type attributeautoplanSnapshot struct {
    data   []*pb.AttributeAutoPlanTable
    kvData map[uint32]*pb.AttributeAutoPlanTable
    idxDimension map[uint32][]*pb.AttributeAutoPlanTable
    idxWeight map[uint32][]*pb.AttributeAutoPlanTable
    idxClassId map[uint32][]*pb.AttributeAutoPlanTable
    idxPoolId map[uint32][]*pb.AttributeAutoPlanTable
}

type AttributeAutoPlanTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[attributeautoplanSnapshot]
}

var AttributeAutoPlanTableManagerInstance = NewAttributeAutoPlanTableManager()

func NewAttributeAutoPlanTableManager() *AttributeAutoPlanTableManager {
    m := &AttributeAutoPlanTableManager{}
    m.snap.Store(&attributeautoplanSnapshot{
        kvData: make(map[uint32]*pb.AttributeAutoPlanTable),
        idxDimension: make(map[uint32][]*pb.AttributeAutoPlanTable),
        idxWeight: make(map[uint32][]*pb.AttributeAutoPlanTable),
        idxClassId: make(map[uint32][]*pb.AttributeAutoPlanTable),
        idxPoolId: make(map[uint32][]*pb.AttributeAutoPlanTable),
    })
    return m
}

func (m *AttributeAutoPlanTableManager) Load(configDir string, useBinary bool) error {
    var container pb.AttributeAutoPlanTableData

    if useBinary {
        path := filepath.Join(configDir, "attributeautoplan.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "attributeautoplan.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &attributeautoplanSnapshot{
        kvData: make(map[uint32]*pb.AttributeAutoPlanTable, len(container.Data)),
        idxDimension: make(map[uint32][]*pb.AttributeAutoPlanTable),
        idxWeight: make(map[uint32][]*pb.AttributeAutoPlanTable),
        idxClassId: make(map[uint32][]*pb.AttributeAutoPlanTable),
        idxPoolId: make(map[uint32][]*pb.AttributeAutoPlanTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        for _, elem := range row.Dimension {
            snap.idxDimension[elem] = append(snap.idxDimension[elem], row)
        }
        for _, elem := range row.Weight {
            snap.idxWeight[elem] = append(snap.idxWeight[elem], row)
        }
        snap.idxClassId[row.ClassId] = append(snap.idxClassId[row.ClassId], row)
        snap.idxPoolId[row.PoolId] = append(snap.idxPoolId[row.PoolId], row)
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *AttributeAutoPlanTableManager) FindAll() []*pb.AttributeAutoPlanTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *AttributeAutoPlanTableManager) FindById(id uint32) (*pb.AttributeAutoPlanTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *AttributeAutoPlanTableManager) FindByDimensionIndex(key uint32) []*pb.AttributeAutoPlanTable {
    snap := m.snap.Load()
    return snap.idxDimension[key]
}


func (m *AttributeAutoPlanTableManager) FindByWeightIndex(key uint32) []*pb.AttributeAutoPlanTable {
    snap := m.snap.Load()
    return snap.idxWeight[key]
}


func (m *AttributeAutoPlanTableManager) GetByClassId(key uint32) []*pb.AttributeAutoPlanTable {
    snap := m.snap.Load()
    return snap.idxClassId[key]
}


func (m *AttributeAutoPlanTableManager) GetByPoolId(key uint32) []*pb.AttributeAutoPlanTable {
    snap := m.snap.Load()
    return snap.idxPoolId[key]
}



// ---- Exists ----

func (m *AttributeAutoPlanTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *AttributeAutoPlanTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *AttributeAutoPlanTableManager) CountByDimensionIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxDimension[key])
}


func (m *AttributeAutoPlanTableManager) CountByWeightIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxWeight[key])
}


func (m *AttributeAutoPlanTableManager) CountByClassIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxClassId[key])
}


func (m *AttributeAutoPlanTableManager) CountByPoolIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxPoolId[key])
}



// ---- FindByIds (IN) ----

func (m *AttributeAutoPlanTableManager) FindByIds(ids []uint32) []*pb.AttributeAutoPlanTable {
    snap := m.snap.Load()
    result := make([]*pb.AttributeAutoPlanTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *AttributeAutoPlanTableManager) RandOne() (*pb.AttributeAutoPlanTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *AttributeAutoPlanTableManager) Where(pred func(*pb.AttributeAutoPlanTable) bool) []*pb.AttributeAutoPlanTable {
    snap := m.snap.Load()
    var result []*pb.AttributeAutoPlanTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *AttributeAutoPlanTableManager) First(pred func(*pb.AttributeAutoPlanTable) bool) (*pb.AttributeAutoPlanTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: pool_id → AttributePool.id

// FK: dimension → AttributeDimension.id


// ---- Composite Key ----

