#pragma once

// AFK (Away From Keyboard) component.
// Attached to a player entity when they have been idle (no client messages)
// for kAfkInactivityFrames. Movement systems exclude entities with this tag
// so AFK players skip expensive per-tick position updates.
struct AfkComp
{
    uint32_t afkStartFrame{0};   // Frame when AFK state began
    uint32_t reducedTickCount{0}; // Number of reduced ticks processed
};

// Tracks the last frame a player entity received a client message.
// Checked by AfkSystem each tick to detect inactivity.
struct LastActiveFrameComp
{
    uint32_t frame{0};
};
