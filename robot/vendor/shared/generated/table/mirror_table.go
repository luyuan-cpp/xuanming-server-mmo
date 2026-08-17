
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



// mirrorSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type mirrorSnapshot struct {
    data   []*pb.MirrorTable
    kvData map[uint32]*pb.MirrorTable
    idxSceneId map[uint32][]*pb.MirrorTable
    idxMainSceneId map[uint32][]*pb.MirrorTable
}

type MirrorTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[mirrorSnapshot]
}

var MirrorTableManagerInstance = NewMirrorTableManager()

func NewMirrorTableManager() *MirrorTableManager {
    m := &MirrorTableManager{}
    m.snap.Store(&mirrorSnapshot{
        kvData: make(map[uint32]*pb.MirrorTable),
        idxSceneId: make(map[uint32][]*pb.MirrorTable),
        idxMainSceneId: make(map[uint32][]*pb.MirrorTable),
    })
    return m
}

func (m *MirrorTableManager) Load(configDir string, useBinary bool) error {
    var container pb.MirrorTableData

    if useBinary {
        path := filepath.Join(configDir, "mirror.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "mirror.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &mirrorSnapshot{
        kvData: make(map[uint32]*pb.MirrorTable, len(container.Data)),
        idxSceneId: make(map[uint32][]*pb.MirrorTable),
        idxMainSceneId: make(map[uint32][]*pb.MirrorTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxSceneId[row.SceneId] = append(snap.idxSceneId[row.SceneId], row)
        snap.idxMainSceneId[row.MainSceneId] = append(snap.idxMainSceneId[row.MainSceneId], row)
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *MirrorTableManager) FindAll() []*pb.MirrorTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *MirrorTableManager) FindById(id uint32) (*pb.MirrorTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *MirrorTableManager) GetBySceneId(key uint32) []*pb.MirrorTable {
    snap := m.snap.Load()
    return snap.idxSceneId[key]
}


func (m *MirrorTableManager) GetByMainSceneId(key uint32) []*pb.MirrorTable {
    snap := m.snap.Load()
    return snap.idxMainSceneId[key]
}



// ---- Exists ----

func (m *MirrorTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *MirrorTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *MirrorTableManager) CountBySceneIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxSceneId[key])
}


func (m *MirrorTableManager) CountByMainSceneIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxMainSceneId[key])
}



// ---- FindByIds (IN) ----

func (m *MirrorTableManager) FindByIds(ids []uint32) []*pb.MirrorTable {
    snap := m.snap.Load()
    result := make([]*pb.MirrorTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *MirrorTableManager) RandOne() (*pb.MirrorTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *MirrorTableManager) Where(pred func(*pb.MirrorTable) bool) []*pb.MirrorTable {
    snap := m.snap.Load()
    var result []*pb.MirrorTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *MirrorTableManager) First(pred func(*pb.MirrorTable) bool) (*pb.MirrorTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: scene_id → BaseScene.id

// FK: main_scene_id → World.id


// ---- Composite Key ----

