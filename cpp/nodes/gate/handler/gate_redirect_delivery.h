#pragma once

// 推 msg 124 RedirectToGate 的唯一收口(docs/design/gate-drain-session-migration.md §4.3)。两条入口共用:
//   - 跨 zone 传送:scene_manager 经 Kafka 发来的 RedirectToGateEvent(handler/event/gate_event_handler.cpp);
//   - 排空迁移:login 签回的迁移票据(handler/rpc/drain_migration.cpp,经 SetRedirectPusher 注入)。
//
// 只能在 gate 主 loop 线程调用(会话表是 thread_local)。

#include "handler/rpc/drain_migration.h"

namespace gate_redirect
{

// 依次:
//   1. 找会话;
//   2. 身份栅栏:票据玩家非 0 时必须等于会话当前绑定的玩家(R13,session_identity_fence.h ClassifyPush,与其他推送同一口径);
//   3. lock 连接;
//   4. 丢掉还没发出的路由请求,之后的路由请求回 1003(gate_lane::OnSessionRedirected,client-request-ordering.md O9);
//   5. 推 124;
//   6. 登记滞留关闭(gate_drain_migration::NoteRedirected)。
// 前 3 步任一不过就返回对应结果,不改任何状态。
gate_drain_migration::PushOutcome PushRedirect(SessionId sessionId, Guid ticketPlayerId, const RedirectToGateNotify &notify,
	gate_drain_migration::SteadyTime now);

} // namespace gate_redirect
