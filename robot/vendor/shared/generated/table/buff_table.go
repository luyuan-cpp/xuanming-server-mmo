
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



// buffSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type buffSnapshot struct {
    data   []*pb.BuffTable
    kvData map[uint32]*pb.BuffTable
    idxInterval_effect map[float64][]*pb.BuffTable
    idxSub_buff map[uint32][]*pb.BuffTable
    idxTarget_sub_buff map[uint32][]*pb.BuffTable
}

type BuffTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[buffSnapshot]
}

var BuffTableManagerInstance = NewBuffTableManager()

func NewBuffTableManager() *BuffTableManager {
    m := &BuffTableManager{}
    m.snap.Store(&buffSnapshot{
        kvData: make(map[uint32]*pb.BuffTable),
        idxInterval_effect: make(map[float64][]*pb.BuffTable),
        idxSub_buff: make(map[uint32][]*pb.BuffTable),
        idxTarget_sub_buff: make(map[uint32][]*pb.BuffTable),
    })
    return m
}

func (m *BuffTableManager) Load(configDir string, useBinary bool) error {
    var container pb.BuffTableData

    if useBinary {
        path := filepath.Join(configDir, "buff.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "buff.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &buffSnapshot{
        kvData: make(map[uint32]*pb.BuffTable, len(container.Data)),
        idxInterval_effect: make(map[float64][]*pb.BuffTable),
        idxSub_buff: make(map[uint32][]*pb.BuffTable),
        idxTarget_sub_buff: make(map[uint32][]*pb.BuffTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        for _, elem := range row.IntervalEffect {
            snap.idxInterval_effect[elem] = append(snap.idxInterval_effect[elem], row)
        }
        for _, elem := range row.SubBuff {
            snap.idxSub_buff[elem] = append(snap.idxSub_buff[elem], row)
        }
        for _, elem := range row.TargetSubBuff {
            snap.idxTarget_sub_buff[elem] = append(snap.idxTarget_sub_buff[elem], row)
        }
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *BuffTableManager) FindAll() []*pb.BuffTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *BuffTableManager) FindById(id uint32) (*pb.BuffTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *BuffTableManager) FindByInterval_effectIndex(key float64) []*pb.BuffTable {
    snap := m.snap.Load()
    return snap.idxInterval_effect[key]
}


func (m *BuffTableManager) FindBySub_buffIndex(key uint32) []*pb.BuffTable {
    snap := m.snap.Load()
    return snap.idxSub_buff[key]
}


func (m *BuffTableManager) FindByTarget_sub_buffIndex(key uint32) []*pb.BuffTable {
    snap := m.snap.Load()
    return snap.idxTarget_sub_buff[key]
}



// ---- Exists ----

func (m *BuffTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *BuffTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *BuffTableManager) CountByInterval_effectIndex(key float64) int {
    snap := m.snap.Load()
    return len(snap.idxInterval_effect[key])
}


func (m *BuffTableManager) CountBySub_buffIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxSub_buff[key])
}


func (m *BuffTableManager) CountByTarget_sub_buffIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxTarget_sub_buff[key])
}



// ---- FindByIds (IN) ----

func (m *BuffTableManager) FindByIds(ids []uint32) []*pb.BuffTable {
    snap := m.snap.Load()
    result := make([]*pb.BuffTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *BuffTableManager) RandOne() (*pb.BuffTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *BuffTableManager) Where(pred func(*pb.BuffTable) bool) []*pb.BuffTable {
    snap := m.snap.Load()
    var result []*pb.BuffTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *BuffTableManager) First(pred func(*pb.BuffTable) bool) (*pb.BuffTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

