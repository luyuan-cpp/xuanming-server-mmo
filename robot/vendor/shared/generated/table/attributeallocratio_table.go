
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



// attributeallocratioSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type attributeallocratioSnapshot struct {
    data   []*pb.AttributeAllocRatioTable
    kvData map[uint32]*pb.AttributeAllocRatioTable
    idxDimensionId map[uint32][]*pb.AttributeAllocRatioTable
    idxClassId map[uint32][]*pb.AttributeAllocRatioTable
}

type AttributeAllocRatioTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[attributeallocratioSnapshot]
}

var AttributeAllocRatioTableManagerInstance = NewAttributeAllocRatioTableManager()

func NewAttributeAllocRatioTableManager() *AttributeAllocRatioTableManager {
    m := &AttributeAllocRatioTableManager{}
    m.snap.Store(&attributeallocratioSnapshot{
        kvData: make(map[uint32]*pb.AttributeAllocRatioTable),
        idxDimensionId: make(map[uint32][]*pb.AttributeAllocRatioTable),
        idxClassId: make(map[uint32][]*pb.AttributeAllocRatioTable),
    })
    return m
}

func (m *AttributeAllocRatioTableManager) Load(configDir string, useBinary bool) error {
    var container pb.AttributeAllocRatioTableData

    if useBinary {
        path := filepath.Join(configDir, "attributeallocratio.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "attributeallocratio.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &attributeallocratioSnapshot{
        kvData: make(map[uint32]*pb.AttributeAllocRatioTable, len(container.Data)),
        idxDimensionId: make(map[uint32][]*pb.AttributeAllocRatioTable),
        idxClassId: make(map[uint32][]*pb.AttributeAllocRatioTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxDimensionId[row.DimensionId] = append(snap.idxDimensionId[row.DimensionId], row)
        snap.idxClassId[row.ClassId] = append(snap.idxClassId[row.ClassId], row)
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *AttributeAllocRatioTableManager) FindAll() []*pb.AttributeAllocRatioTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *AttributeAllocRatioTableManager) FindById(id uint32) (*pb.AttributeAllocRatioTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *AttributeAllocRatioTableManager) GetByDimensionId(key uint32) []*pb.AttributeAllocRatioTable {
    snap := m.snap.Load()
    return snap.idxDimensionId[key]
}


func (m *AttributeAllocRatioTableManager) GetByClassId(key uint32) []*pb.AttributeAllocRatioTable {
    snap := m.snap.Load()
    return snap.idxClassId[key]
}



// ---- Exists ----

func (m *AttributeAllocRatioTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *AttributeAllocRatioTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *AttributeAllocRatioTableManager) CountByDimensionIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxDimensionId[key])
}


func (m *AttributeAllocRatioTableManager) CountByClassIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxClassId[key])
}



// ---- FindByIds (IN) ----

func (m *AttributeAllocRatioTableManager) FindByIds(ids []uint32) []*pb.AttributeAllocRatioTable {
    snap := m.snap.Load()
    result := make([]*pb.AttributeAllocRatioTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *AttributeAllocRatioTableManager) RandOne() (*pb.AttributeAllocRatioTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *AttributeAllocRatioTableManager) Where(pred func(*pb.AttributeAllocRatioTable) bool) []*pb.AttributeAllocRatioTable {
    snap := m.snap.Load()
    var result []*pb.AttributeAllocRatioTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *AttributeAllocRatioTableManager) First(pred func(*pb.AttributeAllocRatioTable) bool) (*pb.AttributeAllocRatioTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: dimension_id → AttributeDimension.id


// ---- Composite Key ----

