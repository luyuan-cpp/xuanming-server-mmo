package handler

import (
	"proto/chat"
	"robot/logic/gameobject"
)

// ClientPlayerChatPullChatHistoryHandler 把拉历史的响应原样放进聊天收件箱(见 client_player_chat_send_chat.go 的 chatInbox)。
// 不在这里解读 error_message / messages:断言口径(nonce 是否可见、sender 是否被服务端覆盖、条数)属于场景。
func ClientPlayerChatPullChatHistoryHandler(player *gameobject.Player, response *chat.PullChatHistoryResponse) {
	chatInboxStore.appendHistory(player.ID, response)
}
