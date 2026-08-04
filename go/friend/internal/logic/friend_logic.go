package logic

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"

	"friend/internal/config"
	"friend/internal/constants"
	"friend/internal/data"
	base "proto/common/base"
	pb "proto/friend"
)

func tipErr(id uint32, msg string) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id, Parameters: []string{msg}}
}

type FriendLogic struct {
	repo *data.FriendRepo
}

func NewFriendLogic(repo *data.FriendRepo) *FriendLogic {
	return &FriendLogic{repo: repo}
}

func (l *FriendLogic) AddFriend(ctx context.Context, req *pb.AddFriendRequest) (*pb.AddFriendResponse, error) {
	if req.PlayerId == req.TargetPlayerId {
		return &pb.AddFriendResponse{
			ErrorMessage: tipErr(constants.ErrCannotAddSelf, "cannot add yourself"),
		}, nil
	}

	// Check if already friends
	already, err := l.repo.AreFriends(ctx, req.PlayerId, req.TargetPlayerId)
	if err != nil {
		return nil, fmt.Errorf("check friendship: %w", err)
	}
	if already {
		return &pb.AddFriendResponse{
			ErrorMessage: tipErr(constants.ErrAlreadyFriends, "already friends"),
		}, nil
	}

	// Check friend count limit
	friends, err := l.repo.GetFriendList(ctx, req.PlayerId)
	if err != nil {
		return nil, fmt.Errorf("get friend list: %w", err)
	}
	if uint32(len(friends)) >= config.AppConfig.Cache.MaxFriends {
		return &pb.AddFriendResponse{
			ErrorMessage: tipErr(constants.ErrFriendListFull, "friend list full"),
		}, nil
	}

	// Check duplicate pending request
	hasPending, err := l.repo.HasPendingRequest(ctx, req.PlayerId, req.TargetPlayerId)
	if err != nil {
		return nil, fmt.Errorf("check pending: %w", err)
	}
	if hasPending {
		return &pb.AddFriendResponse{
			ErrorMessage: tipErr(constants.ErrRequestAlreadySent, "request already sent"),
		}, nil
	}

	if err := l.repo.AddFriendRequest(ctx, req.PlayerId, req.TargetPlayerId); err != nil {
		return nil, fmt.Errorf("add friend request: %w", err)
	}
	logx.Infof("Player %d sent friend request to %d", req.PlayerId, req.TargetPlayerId)
	return &pb.AddFriendResponse{}, nil
}

func (l *FriendLogic) AcceptFriend(ctx context.Context, req *pb.AcceptFriendRequest) (*pb.AcceptFriendResponse, error) {
	err := l.repo.AcceptFriend(ctx, req.FromPlayerId, req.PlayerId, config.AppConfig.Cache.MaxFriends)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrSenderFriendsFull):
			return &pb.AcceptFriendResponse{
				ErrorMessage: tipErr(constants.ErrTargetFriendListFull, "sender's friend list full"),
			}, nil
		case errors.Is(err, data.ErrAcceptorFriendsFull):
			return &pb.AcceptFriendResponse{
				ErrorMessage: tipErr(constants.ErrFriendListFull, "your friend list full"),
			}, nil
		case errors.Is(err, data.ErrRequestNotFound):
			return &pb.AcceptFriendResponse{
				ErrorMessage: tipErr(constants.ErrNoPendingRequest, "no pending friend request"),
			}, nil
		default:
			return nil, fmt.Errorf("accept friend: %w", err)
		}
	}
	logx.Infof("Player %d accepted friend request from %d", req.PlayerId, req.FromPlayerId)
	return &pb.AcceptFriendResponse{}, nil
}

func (l *FriendLogic) RejectFriend(ctx context.Context, req *pb.RejectFriendRequest) (*pb.RejectFriendResponse, error) {
	if err := l.repo.RejectFriend(ctx, req.FromPlayerId, req.PlayerId); errors.Is(err, data.ErrRequestNotFound) {
		return &pb.RejectFriendResponse{
			ErrorMessage: tipErr(constants.ErrNoPendingRequest, "no pending friend request"),
		}, nil
	} else if err != nil {
		return nil, fmt.Errorf("reject friend: %w", err)
	}
	return &pb.RejectFriendResponse{}, nil
}

func (l *FriendLogic) RemoveFriend(ctx context.Context, req *pb.RemoveFriendRequest) (*pb.RemoveFriendResponse, error) {
	if err := l.repo.RemoveFriend(ctx, req.PlayerId, req.TargetPlayerId); err != nil {
		return nil, fmt.Errorf("remove friend: %w", err)
	}
	logx.Infof("Player %d removed friend %d", req.PlayerId, req.TargetPlayerId)
	return &pb.RemoveFriendResponse{}, nil
}

func (l *FriendLogic) GetFriendList(ctx context.Context, req *pb.GetFriendListRequest) (*pb.GetFriendListResponse, error) {
	friends, err := l.repo.GetFriendList(ctx, req.PlayerId)
	if err != nil {
		return nil, fmt.Errorf("get friend list: %w", err)
	}

	// Batch query online status via Redis pipeline
	friendIDs := make([]uint64, len(friends))
	for i, f := range friends {
		friendIDs[i] = f.FriendPlayerID
	}
	onlineMap, err := l.repo.BatchGetOnlineStatus(ctx, friendIDs)
	if err != nil {
		logx.Errorf("batch online status failed, continuing without: %v", err)
		onlineMap = map[uint64]bool{}
	}

	pbFriends := make([]*pb.FriendEntry, 0, len(friends))
	for _, f := range friends {
		pbFriends = append(pbFriends, &pb.FriendEntry{
			FriendPlayerId: f.FriendPlayerID,
			SinceMs:        f.SinceMs,
			LastActiveMs:   f.LastActiveMs,
			IsOnline:       onlineMap[f.FriendPlayerID],
		})
	}
	return &pb.GetFriendListResponse{Friends: pbFriends}, nil
}

func (l *FriendLogic) GetPendingRequests(ctx context.Context, req *pb.GetPendingRequestsRequest) (*pb.GetPendingRequestsResponse, error) {
	requests, err := l.repo.GetPendingRequests(ctx, req.PlayerId)
	if err != nil {
		return nil, fmt.Errorf("get pending requests: %w", err)
	}

	pbRequests := make([]*pb.FriendRequest, 0, len(requests))
	for _, r := range requests {
		pbRequests = append(pbRequests, &pb.FriendRequest{
			FromPlayerId:  r.FromPlayerID,
			ToPlayerId:    r.ToPlayerID,
			RequestTimeMs: r.RequestTimeMs,
			Status:        pb.FriendRequestStatus(r.Status),
		})
	}
	return &pb.GetPendingRequestsResponse{Requests: pbRequests}, nil
}

func (l *FriendLogic) NotifyOnline(ctx context.Context, req *pb.NotifyOnlineRequest) (*pb.NotifyOnlineResponse, error) {
	if err := l.repo.SetPlayerOnline(ctx, req.PlayerId, req.GateNodeId); err != nil {
		return nil, fmt.Errorf("set player online: %w", err)
	}
	return &pb.NotifyOnlineResponse{}, nil
}

func (l *FriendLogic) NotifyOffline(ctx context.Context, req *pb.NotifyOfflineRequest) (*pb.NotifyOfflineResponse, error) {
	if err := l.repo.SetPlayerOffline(ctx, req.PlayerId); err != nil {
		return nil, fmt.Errorf("set player offline: %w", err)
	}
	return &pb.NotifyOfflineResponse{}, nil
}
