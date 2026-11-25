package cellroute

import (
	"encoding/json"
	"fmt"
)

// TablePayload is the JSON wire schema stored in etcd for hot-swappable routing tables.
type TablePayload struct {
	Version      uint64            `json:"version"`
	RegionOfCell map[uint32]uint32 `json:"region_of_cell"`
	Entries      []Entry           `json:"entries"`
}

// MarshalTableJSON serializes a validated StaticTable and its cell-to-region map.
func MarshalTableJSON(version uint64, t *StaticTable, regionOfCell map[uint32]uint32) ([]byte, error) {
	if t == nil {
		return nil, fmt.Errorf("cellroute: cannot marshal nil table")
	}
	return json.Marshal(TablePayload{
		Version:      version,
		RegionOfCell: regionOfCell,
		Entries:      t.EntriesCopy(),
	})
}

// ParseTableJSON decodes and validates a TablePayload into an immutable StaticTable.
func ParseTableJSON(raw []byte) (*StaticTable, uint64, error) {
	var p TablePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, 0, fmt.Errorf("cellroute: invalid table json: %w", err)
	}
	tbl, err := NewStaticTable(p.Entries, p.RegionOfCell)
	if err != nil {
		return nil, 0, err
	}
	return tbl, p.Version, nil
}
