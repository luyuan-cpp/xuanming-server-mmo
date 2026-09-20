
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



// guildlevelSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type guildlevelSnapshot struct {
    data   []*pb.GuildLevelTable
    kvData map[uint32]*pb.GuildLevelTable
}

type GuildLevelTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[guildlevelSnapshot]
}

var GuildLevelTableManagerInstance = NewGuildLevelTableManager()

func NewGuildLevelTableManager() *GuildLevelTableManager {
    m := &GuildLevelTableManager{}
    m.snap.Store(&guildlevelSnapshot{
        kvData: make(map[uint32]*pb.GuildLevelTable),
    })
    return m
}

func (m *GuildLevelTableManager) Load(configDir string, useBinary bool) error {
    var container pb.GuildLevelTableData

    if useBinary {
        path := filepath.Join(configDir, "guildlevel.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "guildlevel.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &guildlevelSnapshot{
        kvData: make(map[uint32]*pb.GuildLevelTable, len(container.Data)),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *GuildLevelTableManager) FindAll() []*pb.GuildLevelTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *GuildLevelTableManager) FindById(id uint32) (*pb.GuildLevelTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}



// ---- Exists ----

func (m *GuildLevelTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *GuildLevelTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}



// ---- FindByIds (IN) ----

func (m *GuildLevelTableManager) FindByIds(ids []uint32) []*pb.GuildLevelTable {
    snap := m.snap.Load()
    result := make([]*pb.GuildLevelTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *GuildLevelTableManager) RandOne() (*pb.GuildLevelTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *GuildLevelTableManager) Where(pred func(*pb.GuildLevelTable) bool) []*pb.GuildLevelTable {
    snap := m.snap.Load()
    var result []*pb.GuildLevelTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *GuildLevelTableManager) First(pred func(*pb.GuildLevelTable) bool) (*pb.GuildLevelTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}

// ---- Composite Key ----

