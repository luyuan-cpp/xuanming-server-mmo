
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



// equipaffixqualitySnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type equipaffixqualitySnapshot struct {
    data   []*pb.EquipAffixQualityTable
    kvData map[uint32]*pb.EquipAffixQualityTable
}

type EquipAffixQualityTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[equipaffixqualitySnapshot]
}

var EquipAffixQualityTableManagerInstance = NewEquipAffixQualityTableManager()

func NewEquipAffixQualityTableManager() *EquipAffixQualityTableManager {
    m := &EquipAffixQualityTableManager{}
    m.snap.Store(&equipaffixqualitySnapshot{
        kvData: make(map[uint32]*pb.EquipAffixQualityTable),
    })
    return m
}

func (m *EquipAffixQualityTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *EquipAffixQualityTableManager) swapSnapshot(snap *equipaffixqualitySnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *EquipAffixQualityTableManager) loadSnapshot(configDir string, useBinary bool) (*equipaffixqualitySnapshot, error) {
    var container pb.EquipAffixQualityTableData

    if useBinary {
        path := filepath.Join(configDir, "equipaffixquality.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "equipaffixquality.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &equipaffixqualitySnapshot{
        kvData: make(map[uint32]*pb.EquipAffixQualityTable, len(container.Data)),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
    }

    snap.data = container.Data
    return snap, nil
}

func (m *EquipAffixQualityTableManager) FindAll() []*pb.EquipAffixQualityTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *EquipAffixQualityTableManager) FindById(id uint32) (*pb.EquipAffixQualityTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}



// ---- Exists ----

func (m *EquipAffixQualityTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *EquipAffixQualityTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}



// ---- FindByIds (IN) ----

func (m *EquipAffixQualityTableManager) FindByIds(ids []uint32) []*pb.EquipAffixQualityTable {
    snap := m.snap.Load()
    result := make([]*pb.EquipAffixQualityTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *EquipAffixQualityTableManager) RandOne() (*pb.EquipAffixQualityTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *EquipAffixQualityTableManager) Where(pred func(*pb.EquipAffixQualityTable) bool) []*pb.EquipAffixQualityTable {
    snap := m.snap.Load()
    var result []*pb.EquipAffixQualityTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *EquipAffixQualityTableManager) First(pred func(*pb.EquipAffixQualityTable) bool) (*pb.EquipAffixQualityTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

