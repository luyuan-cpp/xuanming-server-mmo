
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



// actoractionstateSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type actoractionstateSnapshot struct {
    data   []*pb.ActorActionStateTable
    // 主键可重复:一个 id 对应多行
    kvData map[uint32][]*pb.ActorActionStateTable
}

type ActorActionStateTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[actoractionstateSnapshot]
}

var ActorActionStateTableManagerInstance = NewActorActionStateTableManager()

func NewActorActionStateTableManager() *ActorActionStateTableManager {
    m := &ActorActionStateTableManager{}
    m.snap.Store(&actoractionstateSnapshot{
        kvData: make(map[uint32][]*pb.ActorActionStateTable),
    })
    return m
}

func (m *ActorActionStateTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *ActorActionStateTableManager) swapSnapshot(snap *actoractionstateSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *ActorActionStateTableManager) loadSnapshot(configDir string, useBinary bool) (*actoractionstateSnapshot, error) {
    var container pb.ActorActionStateTableData

    if useBinary {
        path := filepath.Join(configDir, "actoractionstate.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "actoractionstate.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &actoractionstateSnapshot{
        kvData: make(map[uint32][]*pb.ActorActionStateTable, len(container.Data)),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = append(snap.kvData[row.Id], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *ActorActionStateTableManager) FindAll() []*pb.ActorActionStateTable {
    snap := m.snap.Load()
    return snap.data
}

// FindAllById 返回该 id 命中的**全部**行(表内顺序)。
// 这张表的主键声明了 (cfg_multi),所以不生成 FindById —— 把可重复主键当单值用
// 会在编译期断，而不是悄悄只看第一行。
func (m *ActorActionStateTableManager) FindAllById(id uint32) []*pb.ActorActionStateTable {
    snap := m.snap.Load()
    return snap.kvData[id]
}



// ---- Exists ----

func (m *ActorActionStateTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    return len(snap.kvData[id]) > 0
}



// ---- Count ----

func (m *ActorActionStateTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}



// ---- FindByIds (IN) ----

func (m *ActorActionStateTableManager) FindByIds(ids []uint32) []*pb.ActorActionStateTable {
    snap := m.snap.Load()
    result := make([]*pb.ActorActionStateTable, 0, len(ids))
    for _, id := range ids {
        result = append(result, snap.kvData[id]...)
    }
    return result
}

// ---- RandOne ----

func (m *ActorActionStateTableManager) RandOne() (*pb.ActorActionStateTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *ActorActionStateTableManager) Where(pred func(*pb.ActorActionStateTable) bool) []*pb.ActorActionStateTable {
    snap := m.snap.Load()
    var result []*pb.ActorActionStateTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *ActorActionStateTableManager) First(pred func(*pb.ActorActionStateTable) bool) (*pb.ActorActionStateTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

