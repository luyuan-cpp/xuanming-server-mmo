// Package matchentry 保存匹配票占用的判定；不获取角色锁、不访问远端服务。
// 现在只有 match 自己用:别的服务不再跨库读票,要占用经 match 的 AcquireTrialOccupancy(docs/design/horizontal-scale-logical-db.md §4)。
package matchentry

import pb "proto/match"

// TicketOccupiedSQL用于已持护栏副本行锁的单表查询，不含表名、玩家条件或行锁。
// NULL一律拒绝；state4是精确终态，state0允许登记player/owner，其余15项业务值须全空。
const TicketOccupiedSQL = `(state IS NULL OR (state<>4 AND (state<>0
 OR ticket IS NULL OR ticket<>'' OR request_id IS NULL OR request_id<>''
 OR formation_id IS NULL OR formation_id<>0 OR mode IS NULL OR mode<>0
 OR config_id IS NULL OR config_id<>0 OR team_size IS NULL OR team_size<>0
 OR enqueued_ms IS NULL OR enqueued_ms<>0 OR not_before_ms IS NULL OR not_before_ms<>0
 OR zone_id IS NULL OR zone_id<>0 OR rating_centi IS NULL OR rating_centi<>0
 OR team_id IS NULL OR team_id<>0 OR network_profile IS NULL OR OCTET_LENGTH(network_profile)<>0
 OR expires_ms IS NULL OR expires_ms<>0 OR selected_region IS NULL OR selected_region<>0
 OR preferred_region IS NULL OR preferred_region<>0)))`

// TicketOccupied与SQL谓词共同约束三种安全情况：无行、完整空占位0、精确终态4。
// 过期queued票、待退观战的pending0及未知状态都保持占用。
func TicketOccupied(ticket *pb.MatchAuthorityTicketRecord) bool {
	if ticket == nil || ticket.State == 4 {
		return false
	}
	if ticket.State != 0 {
		return true
	}
	return ticket.Ticket != "" || ticket.RequestId != "" || ticket.FormationId != 0 || ticket.Mode != 0 || ticket.ConfigId != 0 || ticket.TeamSize != 0 || ticket.EnqueuedMs != 0 || ticket.NotBeforeMs != 0 || ticket.ZoneId != 0 || ticket.RatingCenti != 0 || ticket.TeamId != 0 || len(ticket.NetworkProfile) != 0 || ticket.ExpiresMs != 0 || ticket.SelectedRegion != 0 || ticket.PreferredRegion != 0
}
