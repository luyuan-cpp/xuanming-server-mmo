#include "scene_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include "agones/agones_scene_lifecycle.h"
#include "muduo/base/Logging.h"
#include "spatial/system/aoi.h"
#include "spatial/system/scene_crowd.h"
///<<< END WRITING YOUR CODE
void SceneEventHandler::Register()
{
    tlsEcs.dispatcher.sink<OnSceneCreated>().connect<&SceneEventHandler::OnSceneCreatedHandler>();
    tlsEcs.dispatcher.sink<OnSceneDestroyed>().connect<&SceneEventHandler::OnSceneDestroyedHandler>();
    tlsEcs.dispatcher.sink<BeforeEnterScene>().connect<&SceneEventHandler::BeforeEnterSceneHandler>();
    tlsEcs.dispatcher.sink<AfterEnterScene>().connect<&SceneEventHandler::AfterEnterSceneHandler>();
    tlsEcs.dispatcher.sink<BeforeLeaveScene>().connect<&SceneEventHandler::BeforeLeaveSceneHandler>();
    tlsEcs.dispatcher.sink<AfterLeaveScene>().connect<&SceneEventHandler::AfterLeaveSceneHandler>();
    tlsEcs.dispatcher.sink<S2CEnterScene>().connect<&SceneEventHandler::S2CEnterSceneHandler>();
}

void SceneEventHandler::UnRegister()
{
    tlsEcs.dispatcher.sink<OnSceneCreated>().disconnect<&SceneEventHandler::OnSceneCreatedHandler>();
    tlsEcs.dispatcher.sink<OnSceneDestroyed>().disconnect<&SceneEventHandler::OnSceneDestroyedHandler>();
    tlsEcs.dispatcher.sink<BeforeEnterScene>().disconnect<&SceneEventHandler::BeforeEnterSceneHandler>();
    tlsEcs.dispatcher.sink<AfterEnterScene>().disconnect<&SceneEventHandler::AfterEnterSceneHandler>();
    tlsEcs.dispatcher.sink<BeforeLeaveScene>().disconnect<&SceneEventHandler::BeforeLeaveSceneHandler>();
    tlsEcs.dispatcher.sink<AfterLeaveScene>().disconnect<&SceneEventHandler::AfterLeaveSceneHandler>();
    tlsEcs.dispatcher.sink<S2CEnterScene>().disconnect<&SceneEventHandler::S2CEnterSceneHandler>();
}
void SceneEventHandler::OnSceneCreatedHandler(const OnSceneCreated& event)
{
///<<< BEGIN WRITING YOUR CODE
	// Single accounting point for "how many rooms does this process host".
	//
	// Hooked here rather than in the two CreateScene RPC handlers because both
	// the gRPC path (scene_node_service.cpp) and the legacy muduo RPC path
	// (scene_handler.cpp) fire this event ONLY after the entity really exists:
	// idempotent hits and validation failures return early. So this point gets
	// "duplicate CreateScene does not double count" and "failed create does not
	// increment" for free, without duplicating the counter in both handlers.
	// NOTE: comments in this file stay ASCII -- it carries codegen guard regions
	// and has no UTF-8 BOM (see .github/copilot-instructions.md, CP936).
	agones::SceneLifecycle::Instance().OnSceneCreated(event.entity());
///<<< END WRITING YOUR CODE
}
void SceneEventHandler::OnSceneDestroyedHandler(const OnSceneDestroyed& event)
{
///<<< BEGIN WRITING YOUR CODE
	// Same as above: both DestroyScene paths fire this event only when the
	// entity actually exists -- "destroy a scene that is not here" takes the
	// idempotent-OK branch and never reaches this handler. SceneLifecycle
	// additionally dedupes by key, so the count can never go negative.
	agones::SceneLifecycle::Instance().OnSceneDestroyed(event.entity());
///<<< END WRITING YOUR CODE
}
void SceneEventHandler::BeforeEnterSceneHandler(const BeforeEnterScene& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}
void SceneEventHandler::AfterEnterSceneHandler(const AfterEnterScene& event)
{
///<<< BEGIN WRITING YOUR CODE
	SceneCrowdSystem::AfterEnterSceneHandler(event);
///<<< END WRITING YOUR CODE
}
void SceneEventHandler::BeforeLeaveSceneHandler(const BeforeLeaveScene& event)
{
///<<< BEGIN WRITING YOUR CODE
	SceneCrowdSystem::BeforeLeaveSceneHandler(event);
	AoiSystem::BeforeLeaveSceneHandler(event);
///<<< END WRITING YOUR CODE
}
void SceneEventHandler::AfterLeaveSceneHandler(const AfterLeaveScene& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}
void SceneEventHandler::S2CEnterSceneHandler(const S2CEnterScene& event)
{
///<<< BEGIN WRITING YOUR CODE

///<<< END WRITING YOUR CODE
}
