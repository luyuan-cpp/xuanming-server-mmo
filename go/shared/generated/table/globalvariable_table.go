
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



// globalvariableSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type globalvariableSnapshot struct {
    data   []*pb.GlobalVariableTable
    kvData map[uint32]*pb.GlobalVariableTable
}

type GlobalVariableTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[globalvariableSnapshot]
}

var GlobalVariableTableManagerInstance = NewGlobalVariableTableManager()

func NewGlobalVariableTableManager() *GlobalVariableTableManager {
    m := &GlobalVariableTableManager{}
    m.snap.Store(&globalvariableSnapshot{
        kvData: make(map[uint32]*pb.GlobalVariableTable),
    })
    return m
}

func (m *GlobalVariableTableManager) Load(configDir string, useBinary bool) error {
    var container pb.GlobalVariableTableData

    if useBinary {
        path := filepath.Join(configDir, "globalvariable.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "globalvariable.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &globalvariableSnapshot{
        kvData: make(map[uint32]*pb.GlobalVariableTable, len(container.Data)),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *GlobalVariableTableManager) FindAll() []*pb.GlobalVariableTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *GlobalVariableTableManager) FindById(id uint32) (*pb.GlobalVariableTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}



// ---- Exists ----

func (m *GlobalVariableTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *GlobalVariableTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}



// ---- FindByIds (IN) ----

func (m *GlobalVariableTableManager) FindByIds(ids []uint32) []*pb.GlobalVariableTable {
    snap := m.snap.Load()
    result := make([]*pb.GlobalVariableTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *GlobalVariableTableManager) RandOne() (*pb.GlobalVariableTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *GlobalVariableTableManager) Where(pred func(*pb.GlobalVariableTable) bool) []*pb.GlobalVariableTable {
    snap := m.snap.Load()
    var result []*pb.GlobalVariableTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *GlobalVariableTableManager) First(pred func(*pb.GlobalVariableTable) bool) (*pb.GlobalVariableTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

