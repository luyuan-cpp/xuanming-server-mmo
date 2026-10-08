---<<< BEGIN WRITING YOUR CODE
local own_server_scene1 = 4294967298
local own_server_scene2 = 4294967297
local cross_server_scene1 = 1
local cross_server_scene2 = 2
local times = 0

function ChangeGsScene(response,  to_scene)
	local message = EnterSceneC2SRequest.new()
	message:mutable_scene_info().scene_id = to_scene
	player:send(ClientPlayerSceneServiceEnterSceneC2SMsgId, message)
end
---<<< END WRITING YOUR CODE

---<<<rpc begin
function ClientPlayerSceneServiceEnterSceneC2SHandler(request, response)
---<<< BEGIN WRITING YOUR CODE 
---<<< END WRITING YOUR CODE
end
function ClientPlayerSceneServicePushEnterSceneS2CHandler(request, response)
---<<< BEGIN WRITING YOUR CODE 
	--连续切换有问题
	LogInfo('1------------------------>')
	ChangeGsScene(response,  own_server_scene2)
	times = times + 1
	LogInfo('<------------------------'..times)
	if times == 3 then
		LeaveGame()
		LogInfo("leave game ")
	end
---<<< END WRITING YOUR CODE
end

function ClientPlayerSceneServicePushSceneInfoS2CHandler(request, response)
---<<< BEGIN WRITING YOUR CODE
	local message = EnterSceneC2SRequest.new()
	message:mutable_scene_info().scene_id = own_server_scene1
	player:send(ClientPlayerSceneServiceEnterSceneC2SMsgId, message)

---<<< END WRITING YOUR CODE
end
---<<<rpc end
