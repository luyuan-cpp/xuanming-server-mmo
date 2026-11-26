// Package cellroute provides deterministic 4096-slot Region->Cell compute/ingress routing
// (ported from xuanming-server/pkg/cellroute, see docs/design/region-cell-routing-alignment.md).
package cellroute

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

const (
	// LogicalCellCount is the fixed number of logical routing slots.
	LogicalCellCount uint64 = 4096

	// LogicalRegionCount is the fixed upper bound for region-level coordination shards.
	LogicalRegionCount uint64 = 64
)

var (
	ErrInvalidPlayerID = errors.New("cellroute: player_id must be non-zero")
	ErrInvalidConfig   = errors.New("cellroute: invalid configuration")
)

// Entry maps a single logical slot to its physical (RegionID, CellID).
type Entry struct {
	RegionID uint32 `json:"region_id"`
	CellID   uint32 `json:"cell_id"`
}

// Location is the deterministic routing output for a player_id.
type Location struct {
	RegionID    uint32
	CellID      uint32
	LogicalCell uint32
}

// CellSpec declares a physical cell belonging to a region.
type CellSpec struct {
	RegionID uint32 `json:"RegionId"`
	CellID   uint32 `json:"CellId"`
}

// Conf configures optional cellular routing on stateless entry services (e.g. login).
type Conf struct {
	// Mode selects routing source: "" (disabled), "static" (balanced from Cells), or "etcd" (hot watch).
	Mode string `json:"Mode,optional"`
	// Cells provides static topology (required for "static"; optional bootstrap seed for "etcd").
	Cells []CellSpec `json:"Cells,optional"`
	// EtcdKey is the etcd key holding TablePayload JSON when Mode == "etcd".
	EtcdKey string `json:"EtcdKey,optional"`
	// ResyncInterval controls periodic full resync in etcd mode (0 uses etcdwatch.ResyncInterval).
	ResyncInterval time.Duration `json:"ResyncInterval,optional"`
	// SelfRegion / SelfCell identify the local deployment cell for ownership verification.
	// Both zero disables foreign-cell mismatch checks.
	SelfRegion uint32 `json:"SelfRegion,optional"`
	SelfCell   uint32 `json:"SelfCell,optional"`
	// EnforceOwnership rejects EnterGame requests owned by a foreign cell when true;
	// false observes and logs foreign-cell arrivals without blocking (safe shadow rollout).
	EnforceOwnership bool `json:"EnforceOwnership,optional"`
}

// Validate checks Conf invariants fail-fast at startup.
func (c Conf) Validate() error {
	switch c.Mode {
	case "":
		return nil
	case "static":
		if len(c.Cells) == 0 {
			return fmt.Errorf("%w: static mode requires non-empty Cells", ErrInvalidConfig)
		}
		_, _, err := BuildBalancedEntries(c.Cells)
		return err
	case "etcd":
		if c.EtcdKey == "" {
			return fmt.Errorf("%w: etcd mode requires non-empty EtcdKey", ErrInvalidConfig)
		}
		if len(c.Cells) > 0 {
			if _, _, err := BuildBalancedEntries(c.Cells); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported Mode %q (want empty, static, or etcd)", ErrInvalidConfig, c.Mode)
	}
}

// Table abstracts a read-only 4096-slot routing lookup.
type Table interface {
	Lookup(logicalCell uint32) (Entry, bool)
	Len() int
}

// StaticTable is an immutable 4096-entry slot table.
type StaticTable struct {
	entries []Entry
}

// NewStaticTable validates exact slot count and Region-Cell consistency before building an immutable table.
func NewStaticTable(entries []Entry, regionOfCell map[uint32]uint32) (*StaticTable, error) {
	if uint64(len(entries)) != LogicalCellCount {
		return nil, fmt.Errorf("cellroute: entries len %d != LogicalCellCount %d", len(entries), LogicalCellCount)
	}
	for lc, e := range entries {
		reg, ok := regionOfCell[e.CellID]
		if !ok {
			return nil, fmt.Errorf("cellroute: logical_cell %d -> cell %d not declared in regionOfCell", lc, e.CellID)
		}
		if reg != e.RegionID {
			return nil, fmt.Errorf("cellroute: logical_cell %d -> cell %d region mismatch: entry=%d topology=%d",
				lc, e.CellID, e.RegionID, reg)
		}
	}
	cp := make([]Entry, len(entries))
	copy(cp, entries)
	return &StaticTable{entries: cp}, nil
}

func (t *StaticTable) Lookup(logicalCell uint32) (Entry, bool) {
	if t == nil || uint64(logicalCell) >= uint64(len(t.entries)) {
		return Entry{}, false
	}
	return t.entries[logicalCell], true
}

func (t *StaticTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.entries)
}

// EntriesCopy returns a defensive copy of the 4096 entries.
func (t *StaticTable) EntriesCopy() []Entry {
	cp := make([]Entry, len(t.entries))
	copy(cp, t.entries)
	return cp
}

// AtomicTable holds a lock-free swappable *StaticTable pointer for hot updates.
type AtomicTable struct {
	ptr atomic.Pointer[StaticTable]
}

// NewAtomicTable wraps an initial valid StaticTable.
func NewAtomicTable(initial *StaticTable) (*AtomicTable, error) {
	if initial == nil || uint64(initial.Len()) != LogicalCellCount {
		return nil, errors.New("cellroute: initial StaticTable is nil or wrong length")
	}
	a := &AtomicTable{}
	a.ptr.Store(initial)
	return a, nil
}

func (a *AtomicTable) Lookup(logicalCell uint32) (Entry, bool) {
	t := a.ptr.Load()
	if t == nil {
		return Entry{}, false
	}
	return t.Lookup(logicalCell)
}

func (a *AtomicTable) Len() int {
	t := a.ptr.Load()
	if t == nil {
		return 0
	}
	return t.Len()
}

// Swap atomically installs a newly validated StaticTable.
func (a *AtomicTable) Swap(next *StaticTable) error {
	if next == nil || uint64(next.Len()) != LogicalCellCount {
		return errors.New("cellroute: cannot swap nil or invalid StaticTable")
	}
	a.ptr.Store(next)
	return nil
}

// LogicalCellOf computes the deterministic logical slot for playerID.
func LogicalCellOf(playerID uint64) uint32 {
	return uint32(playerID % LogicalCellCount)
}

// Router resolves playerID to physical (RegionID, CellID) without network I/O.
type Router struct {
	table Table
}

// NewRouter constructs a Router over a validated 4096-slot Table.
func NewRouter(table Table) (*Router, error) {
	if table == nil {
		return nil, errors.New("cellroute: nil table")
	}
	if uint64(table.Len()) != LogicalCellCount {
		return nil, fmt.Errorf("cellroute: table len %d != LogicalCellCount %d", table.Len(), LogicalCellCount)
	}
	return &Router{table: table}, nil
}

// Route returns the physical location for playerID (rejects playerID == 0).
func (r *Router) Route(playerID uint64) (Location, error) {
	if playerID == 0 {
		return Location{}, ErrInvalidPlayerID
	}
	lc := LogicalCellOf(playerID)
	e, ok := r.table.Lookup(lc)
	if !ok {
		return Location{}, fmt.Errorf("cellroute: logical_cell %d not mapped (player_id=%d)", lc, playerID)
	}
	return Location{RegionID: e.RegionID, CellID: e.CellID, LogicalCell: lc}, nil
}

// BuildBalancedEntries distributes 4096 contiguous slots evenly across cells.
func BuildBalancedEntries(cells []CellSpec) ([]Entry, map[uint32]uint32, error) {
	if len(cells) == 0 {
		return nil, nil, errors.New("cellroute: cells must not be empty")
	}
	if uint64(len(cells)) > LogicalCellCount {
		return nil, nil, fmt.Errorf("cellroute: cells len %d exceeds LogicalCellCount %d", len(cells), LogicalCellCount)
	}
	regionOfCell := make(map[uint32]uint32, len(cells))
	for _, c := range cells {
		if prev, exists := regionOfCell[c.CellID]; exists {
			if prev != c.RegionID {
				return nil, nil, fmt.Errorf("cellroute: cell %d declared in multiple regions (%d and %d)", c.CellID, prev, c.RegionID)
			}
			return nil, nil, fmt.Errorf("cellroute: duplicate cell %d in cells", c.CellID)
		}
		regionOfCell[c.CellID] = c.RegionID
	}

	entries := make([]Entry, LogicalCellCount)
	n := uint64(len(cells))
	base := LogicalCellCount / n
	rem := LogicalCellCount % n
	var pos uint64
	for i, c := range cells {
		width := base
		if uint64(i) < rem {
			width++
		}
		for k := uint64(0); k < width; k++ {
			entries[pos] = Entry{RegionID: c.RegionID, CellID: c.CellID}
			pos++
		}
	}
	return entries, regionOfCell, nil
}
