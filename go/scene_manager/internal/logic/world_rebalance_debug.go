package logic

import (
	"encoding/json"
	"net/http"
	"strconv"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
)

// rebalanceDebugResponse is the JSON shape returned by
// /debug/rebalance-plan. Kept deliberately flat so jq / grep can slice
// it during incidents without a schema library.
type rebalanceDebugResponse struct {
	Zones []zoneRebalanceDebug `json:"zones"`
}

type zoneRebalanceDebug struct {
	ZoneID        uint32           `json:"zone_id"`
	Budget        int              `json:"budget"`
	Urgent        []migrationDebug `json:"urgent"`
	Opportunistic []migrationDebug `json:"opportunistic"`
}

type migrationDebug struct {
	ConfID  uint64 `json:"conf_id"`
	SceneID uint64 `json:"scene_id"`
	OldNode string `json:"old_node"`
	NewNode string `json:"new_node"`
	Reason  string `json:"reason"`
}

// RegisterRebalanceDebugHandler attaches /debug/rebalance-plan to the
// metrics HTTP server. The handler snapshots each active zone's current
// planner output and returns it as JSON without triggering any
// migrations — safe to curl from an on-call dashboard during an incident.
//
// Optional query param zone=<id> narrows the response to a single zone;
// omit it for all active zones. Unknown zones produce an empty response,
// not a 404, so ops-side tooling can poll proactively.
func RegisterRebalanceDebugHandler(svcCtx *svc.ServiceContext) {
	metrics.RegisterDebugHandler("/rebalance-plan", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var zones []uint32
		if q := r.URL.Query().Get("zone"); q != "" {
			z, err := strconv.ParseUint(q, 10, 32)
			if err != nil {
				http.Error(w, "invalid zone query param", http.StatusBadRequest)
				return
			}
			zones = []uint32{uint32(z)}
		} else {
			zones = GetActiveZones()
		}

		resp := rebalanceDebugResponse{Zones: make([]zoneRebalanceDebug, 0, len(zones))}
		wids := worldConfIds()

		for _, z := range zones {
			urgent, opportunistic, budget := PlanWorldChannelRebalance(svcCtx, z, wids)
			resp.Zones = append(resp.Zones, zoneRebalanceDebug{
				ZoneID:        z,
				Budget:        budget,
				Urgent:        toDebugMigrations(urgent),
				Opportunistic: toDebugMigrations(opportunistic),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	}))
}

func toDebugMigrations(list []channelMigration) []migrationDebug {
	out := make([]migrationDebug, 0, len(list))
	for _, m := range list {
		out = append(out, migrationDebug{
			ConfID:  m.ConfId,
			SceneID: m.SceneId,
			OldNode: m.OldNode,
			NewNode: m.NewNode,
			Reason:  string(m.Reason),
		})
	}
	return out
}
