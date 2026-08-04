package constants

// Tip error IDs returned to client via TipInfoMessage.
const (
	ErrCannotAddSelf        uint32 = 1
	ErrAlreadyFriends       uint32 = 2
	ErrFriendListFull       uint32 = 3
	ErrRequestAlreadySent   uint32 = 4
	ErrTargetFriendListFull uint32 = 5
	ErrNoPendingRequest     uint32 = 6
)
