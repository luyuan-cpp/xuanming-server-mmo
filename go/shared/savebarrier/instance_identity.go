package savebarrier

// VerifiedIdentityField 是屏障 hash 里的「身份戳」字段:值为最近一次登录核验时所在 Redis 实例的身份
// (LuaInstanceIdentity 的 identity)。只由 login 的核验提交与 save_barrier_restore 写;C++ 存盘、
// go/db 的 ACK 发布与补投都不写它 —— go/db 若写,会把一份回退过、尚未核验的副本误盖成可信
// (docs/design/zone-redis-regression-guard-20261022.md §4.1.1)。其余读者都按字段名读屏障,不受它影响。
const VerifiedIdentityField = "verified_identity"

// LuaInstanceIdentity 定义 Lua 函数 instance_identity(),返回本次脚本所在 Redis 实例的 identity 与 role:
//
//	identity = INFO server 的 run_id .. ':' .. INFO replication 的 master_replid(各 40 位十六进制)
//	role     = INFO replication 的 role
//
// 任一字段取不到时 identity 为空串,调用方必须按「身份未知」处理,不得当成没变。
//
// 为什么「身份没变」就说明副本没回退:run_id 每次进程启动都换,master_replid 在从库被提升为主时换;
// 崩溃丢尾、切主丢尾、从备份恢复都伴随其中之一。前提是关掉原地换数据的 DEBUG RELOAD
// (--enable-debug-command no,redis.yaml 与 redis-ha.yaml 都已显式写出)。字段口径与
// shared/placement.RouteReadyReadScript 相同。
//
// 用 redis.pcall:INFO 在兼容实现里不可用时得到错误表,按身份未知处理,不让整段脚本失败。
// 未核实:Redis 7.2 / 8.x 脚本里调 INFO(同 route_ready.go 的未核实项),由真 Redis 用例取证。
const LuaInstanceIdentity = `
local function instance_identity()
    local srv = redis.pcall('INFO', 'server')
    local repl = redis.pcall('INFO', 'replication')
    if type(srv) ~= 'string' or type(repl) ~= 'string' then return '', '' end
    local role = string.match(repl, 'role:(%a+)') or ''
    local run = string.match(srv, 'run_id:(%x+)')
    local replid = string.match(repl, 'master_replid:(%x+)')
    if not run or not replid or #run ~= 40 or #replid ~= 40 then return '', role end
    return run .. ':' .. replid, role
end
`
