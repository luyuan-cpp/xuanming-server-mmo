package handler

import (
	"sync"

	"proto/chat"
	"robot/logic/gameobject"
)

// chatInboxMaxEntriesPerPlayer 是每个玩家在收件箱里最多保留的响应条数。
// 为什么要封顶:这两个 handler 不只服务 chat-smoke —— 压测模式的 AI(robot_ai.go 的 chat 动作)
// 也会发 SendChat,长跑数小时不封顶就是按玩家无限增长的内存。冒烟每步前都会 ResetChatInbox,
// 单步最多几条响应,64 条对冒烟绰绰有余,对压测只是个小常数。超出时丢最旧的。
const chatInboxMaxEntriesPerPlayer = 64

// chatInbox 暂存聊天 RPC 的响应,供 chat-smoke 场景按玩家轮询消费。
//
// 为什么不放在 gameobject.Player 上:聊天是全局 Go 服务的回包(经 gate → 路由服 → chat),
// 与场景实体状态无关;放在 handler 包级只需两个 handler 各自 append,不必给 Player 再加一组字段。
// 两个 handler 在 RecvLoop goroutine 里写,场景主流程在另一个 goroutine 里读,故用互斥锁保护。
type chatInbox struct {
	mu sync.Mutex
	// sendTips:SendChatResponse.error_message.id,0 = chat 受理成功。按到达顺序追加。
	sendTips map[uint64][]uint32
	// histories:PullChatHistoryResponse 原样保存(含 error_message 与 messages),按到达顺序追加。
	histories map[uint64][]*chat.PullChatHistoryResponse
}

var chatInboxStore = &chatInbox{
	sendTips:  make(map[uint64][]uint32),
	histories: make(map[uint64][]*chat.PullChatHistoryResponse),
}

func (b *chatInbox) appendSendTip(playerId uint64, tip uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tips := append(b.sendTips[playerId], tip)
	if len(tips) > chatInboxMaxEntriesPerPlayer {
		tips = tips[len(tips)-chatInboxMaxEntriesPerPlayer:]
	}
	b.sendTips[playerId] = tips
}

func (b *chatInbox) appendHistory(playerId uint64, response *chat.PullChatHistoryResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	histories := append(b.histories[playerId], response)
	if len(histories) > chatInboxMaxEntriesPerPlayer {
		histories = histories[len(histories)-chatInboxMaxEntriesPerPlayer:]
	}
	b.histories[playerId] = histories
}

// ChatSendTipsOf 返回该玩家自上次 ResetChatInbox 以来收到的 SendChat 响应 tip(副本,按到达顺序)。
// 0 表示 chat 受理成功;非 0 是 chat 在响应体里给的拒绝码。
// 注意:gate / 路由服级拒绝走的是信封 MessageContent.error_message(body 为空),
// 那种包若被分发到这里会被解成全零响应、看起来像 tip=0 —— 需要区分的调用方(chat-smoke)
// 必须在自己的 RecvLoop 回调里先拦信封错误,不要把它交给 MessageBodyHandler。
func ChatSendTipsOf(playerId uint64) []uint32 {
	chatInboxStore.mu.Lock()
	defer chatInboxStore.mu.Unlock()
	return append([]uint32(nil), chatInboxStore.sendTips[playerId]...)
}

// ChatHistoriesOf 返回该玩家自上次 ResetChatInbox 以来收到的 PullChatHistory 响应(切片副本,按到达顺序)。
// 响应对象本身是只读共享的,调用方不要修改。
func ChatHistoriesOf(playerId uint64) []*chat.PullChatHistoryResponse {
	chatInboxStore.mu.Lock()
	defer chatInboxStore.mu.Unlock()
	return append([]*chat.PullChatHistoryResponse(nil), chatInboxStore.histories[playerId]...)
}

// ResetChatInbox 清空该玩家的聊天收件箱。
// chat-smoke 每发一个请求前调用:上一步迟到的响应不能被下一步当成自己的结果消费(错位消费)。
func ResetChatInbox(playerId uint64) {
	chatInboxStore.mu.Lock()
	defer chatInboxStore.mu.Unlock()
	delete(chatInboxStore.sendTips, playerId)
	delete(chatInboxStore.histories, playerId)
}

func ClientPlayerChatSendChatHandler(player *gameobject.Player, response *chat.SendChatResponse) {
	// GetErrorMessage / GetId 对 nil 安全:无 error_message = 成功 = tip 0。
	chatInboxStore.appendSendTip(player.ID, response.GetErrorMessage().GetId())
}
