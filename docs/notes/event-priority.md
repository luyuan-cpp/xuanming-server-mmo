# Events
---
## 1. Priority
    Async event handling is used where decoupling is needed — e.g., when an instance-related action
    cannot be invoked immediately. Async events are categorized so that under high load only important
    messages are processed each frame; unimportant ones can be dropped. If a message needs to be
    persisted, it can be stored on the player entity and processed on next login.
    Async events must NOT depend on ordering; if strict ordering is required, use synchronous (direct-call) events.

## 2. Priority Categories
    0 — Critical: processed at tick-end; if unfinished, persisted to database
    1 — Normal: processed after load subsides
    2 — Discardable: non-essential (e.g., UI tip notifications)

## 3. Cross-Server Architecture
    See: docs/design/mmo_cross_server_architecture.md