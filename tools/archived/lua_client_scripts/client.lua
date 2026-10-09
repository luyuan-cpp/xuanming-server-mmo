function LoginRequest:ReadyGo()
	self.account = ("luhailong"..PlayerId.player_id)
	PlayerId.player_id = PlayerId.player_id + 1
end

function ReadyGo()
	request = LoginRequest.new()
	request:ReadyGo()
	player:send(LoginServiceLoginMsgId, request)
	print(request:DebugString())
end

function Example()
	response = TestResponse.new()
	player = response:add_players()
	player.player_id = 1
	for i=0,response:players_size() - 1 do
		p = response:players(i)
		print(p.player_id)
	end
	response:mutable_error().error_no = 1
	print(response:error().error_no)
end

function EnterGame(player_id)
	enter_gs_request = EnterGameRequest.new()
	enter_gs_request.player_id = player_id
	player:send(LoginServiceEnterGameMsgId, enter_gs_request)
end

function LeaveGame()
	request = LeaveGameRequest.new()
	player:send(request)
end

function LoginServiceLoginHandler(request, response)
	print(response:DebugString())
	print(response:players_size())
	if response:players_size() == 0 then
		request = CreatePlayerRequest.new()
		player:send(LoginServiceCreatPlayerMsgId, request)
		return
	end
	EnterGame(response:players(0).player_id)
end

function LoginServiceEnterGameHandler(request, response)
	print("LoginServiceEnterGameHandler")
end

function LoginServiceCreatPlayerHandler(request, response)
	EnterGame(response:players(0).player_id)
end