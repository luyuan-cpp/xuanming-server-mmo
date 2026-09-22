#pragma once

class AfterEnterScene;
class BeforeLeaveScene;

class SceneCrowdSystem
{
public:
    static void AfterEnterSceneHandler(const AfterEnterScene& message);
    static void BeforeLeaveSceneHandler(const BeforeLeaveScene& message);
};