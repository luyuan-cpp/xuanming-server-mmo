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

thread_local std::weak_ptr<SceneTaskScope::State> SceneTaskScope::current_;

SceneTaskScope::SceneTaskScope(muduo::net::EventLoop& loop) : state_(std::make_shared<State>(loop))
{
    loop.assertInLoopThread();
    assert(current_.expired());
    current_ = state_;
}

SceneTaskScope::~SceneTaskScope()
{
    StopAll();
    current_.reset();
}

void SceneTaskScope::Stop() { state_->gameplay.Stop(); }
void SceneTaskScope::StopAll()
{
    state_->gameplay.Stop();
    state_->durable.Stop();
}

bool SceneTaskScope::IsAccepting()
{
    const auto state = current_.lock();
    return state && state->gameplay.IsAccepting();
}

bool SceneTaskScope::IsDurableAccepting()
{
    const auto state = current_.lock();
    return state && state->durable.IsAccepting();
}

bool SceneTaskScope::TryStart(boost::cobalt::task<void> task, const char* name)
{
    const auto state = current_.lock();
    return state && state->gameplay.Start(std::move(task), name);
}

bool SceneTaskScope::TryStartDurable(boost::cobalt::task<void> task, const char* name)
{
    const auto state = current_.lock();
    return state && state->durable.Start(std::move(task), name);
}
