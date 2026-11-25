#include "scene_task_scope.h"
#include "engine/core/async/loop_task_scope.h"

#define BOOST_COBALT_NO_LIB 1
#include <boost/cobalt/task.hpp>
#include <cassert>
#include "muduo/net/EventLoop.h"

struct SceneTaskScope::State
{
    muduo::net::EventLoop& loop;
    LoopTaskScope gameplay;
    LoopTaskScope durable;
    explicit State(muduo::net::EventLoop& loop) : loop(loop), gameplay(loop), durable(loop) {}
};

thread_local std::weak_ptr<SceneTaskScope::State> SceneTaskScope::current;

SceneTaskScope::SceneTaskScope(muduo::net::EventLoop& loop) : state(std::make_shared<State>(loop))
{
    loop.assertInLoopThread();
    assert(current.expired());
    current = state;
}

SceneTaskScope::~SceneTaskScope()
{
    StopAll();
    current.reset();
}

void SceneTaskScope::Stop() { state->gameplay.Stop(); }
void SceneTaskScope::StopAll()
{
    state->gameplay.Stop();
    state->durable.Stop();
}

bool SceneTaskScope::IsAccepting()
{
    const auto state = current.lock();
    return state && state->gameplay.IsAccepting();
}

bool SceneTaskScope::IsDurableAccepting()
{
    const auto state = current.lock();
    return state && state->durable.IsAccepting();
}

bool SceneTaskScope::TryStart(boost::cobalt::task<void> task, const char* name)
{
    const auto state = current.lock();
    return state && state->gameplay.Start(std::move(task), name);
}

bool SceneTaskScope::TryStartDurable(boost::cobalt::task<void> task, const char* name)
{
    const auto state = current.lock();
    return state && state->durable.Start(std::move(task), name);
}
