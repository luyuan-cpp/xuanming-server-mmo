
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



// seasonsealSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type seasonsealSnapshot struct {
    data   []*pb.SeasonSealTable
    kvData map[uint32]*pb.SeasonSealTable
    idxSeasonId map[uint32][]*pb.SeasonSealTable
}

type SeasonSealTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[seasonsealSnapshot]
}

var SeasonSealTableManagerInstance = NewSeasonSealTableManager()

func NewSeasonSealTableManager() *SeasonSealTableManager {
    m := &SeasonSealTableManager{}
    m.snap.Store(&seasonsealSnapshot{
        kvData: make(map[uint32]*pb.SeasonSealTable),
        idxSeasonId: make(map[uint32][]*pb.SeasonSealTable),
    })
    return m
}

func (m *SeasonSealTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *SeasonSealTableManager) swapSnapshot(snap *seasonsealSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *SeasonSealTableManager) loadSnapshot(configDir string, useBinary bool) (*seasonsealSnapshot, error) {
    var container pb.SeasonSealTableData

    if useBinary {
        path := filepath.Join(configDir, "seasonseal.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "seasonseal.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &seasonsealSnapshot{
        kvData: make(map[uint32]*pb.SeasonSealTable, len(container.Data)),
        idxSeasonId: make(map[uint32][]*pb.SeasonSealTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxSeasonId[row.SeasonId] = append(snap.idxSeasonId[row.SeasonId], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *SeasonSealTableManager) FindAll() []*pb.SeasonSealTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *SeasonSealTableManager) FindById(id uint32) (*pb.SeasonSealTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *SeasonSealTableManager) GetBySeasonId(key uint32) []*pb.SeasonSealTable {
    snap := m.snap.Load()
    return snap.idxSeasonId[key]
}



// ---- Exists ----

func (m *SeasonSealTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *SeasonSealTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *SeasonSealTableManager) CountBySeasonIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxSeasonId[key])
}



// ---- FindByIds (IN) ----

func (m *SeasonSealTableManager) FindByIds(ids []uint32) []*pb.SeasonSealTable {
    snap := m.snap.Load()
    result := make([]*pb.SeasonSealTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *SeasonSealTableManager) RandOne() (*pb.SeasonSealTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *SeasonSealTableManager) Where(pred func(*pb.SeasonSealTable) bool) []*pb.SeasonSealTable {
    snap := m.snap.Load()
    var result []*pb.SeasonSealTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *SeasonSealTableManager) First(pred func(*pb.SeasonSealTable) bool) (*pb.SeasonSealTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: season_id → SeasonRule.id


// ---- Composite Key ----

