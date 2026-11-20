#!/usr/bin/env python3
# 未验签连接洪水:按固定速率建 TCP 连接、一个字节都不发,看 gate / battle 的客户端准入怎么处置它们。
# 用于 docs/design/gate-connection-admission-control.md §9.6 的验收与 §9.7 的阈值校准。§9.3 说明了为什么不用 robot:
# robot 走完整登录、连接都会验签,测不到未验签连接,建连速率也远不够。
#
# 每条连接落进四种结局之一:
#   connect_failed  TCP 握手没完成(拒绝、超时、没有路由;SYN 队列满被丢也落在这里,表现为超时)
#   reset           对端回 RST:准入在 accept 处拒绝了它
#   closed          对端发 FIN 关掉:握手期限到了被收割,或承压时被淘汰给新连接让位
#   held            到 --hold 仍然连着,由本工具关掉
# 只用标准库。真规模只在 Linux 上跑(先把 ulimit -n 调到大于 --connections;§9.3:Windows 源端口与 SO_REUSEADDR 都会误报),
# Windows 本机只跑 --self-test。只对自己运维的服务器用。输出是英文(AGENTS.md §2)。

import argparse
import asyncio
import errno
import json
import socket
import struct
import sys
import time

OUTCOMES = ("connect_failed", "reset", "closed", "held")


def new_result():
    result = {name: 0 for name in OUTCOMES}
    result.update(connect_errors={}, reset_after_ms=[], closed_after_ms=[], unexpected_data=0)
    return result


def record(result, outcome, elapsed_ms):
    result[outcome] += 1
    if outcome in ("reset", "closed"):
        result[outcome + "_after_ms"].append(elapsed_ms)


def error_name(error):
    if getattr(error, "errno", None):
        return errno.errorcode.get(error.errno, type(error).__name__)
    return type(error).__name__


async def one_connection(args, result):
    local_addr = (args.bind, 0) if args.bind else None
    started = time.monotonic()
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(args.host, args.port, local_addr=local_addr), args.connect_timeout)
    except (ConnectionResetError, ConnectionAbortedError):
        # 握手已经完成、对端随即 RST(准入在 accept 处拒绝)时,RST 可能赶在本端确认连上之前到达。
        record(result, "reset", (time.monotonic() - started) * 1000.0)
        return
    except (OSError, asyncio.TimeoutError) as error:
        name = error_name(error)
        result["connect_failed"] += 1
        result["connect_errors"][name] = result["connect_errors"].get(name, 0) + 1
        return

    opened_at = time.monotonic()
    try:
        data = await asyncio.wait_for(reader.read(1), args.hold)
        # 不发数据的连接收到了字节:对端不是我们以为的服务(端口或协议不对),照样按 closed 记,另外计数提示。
        if data:
            result["unexpected_data"] += 1
        outcome = "closed"
    except asyncio.TimeoutError:
        # 必须排在 OSError 前面:Python 3.11 起 asyncio.TimeoutError 就是内建 TimeoutError,它是 OSError 的子类。
        outcome = "held"
    except OSError:
        outcome = "reset"
    record(result, outcome, (time.monotonic() - opened_at) * 1000.0)
    writer.close()
    try:
        await writer.wait_closed()
    except OSError:
        pass


async def flood(args):
    result = new_result()
    started = time.monotonic()
    tasks = []
    for index in range(args.connections):
        if args.rate > 0:
            # 按起点排时间表,sleep 的误差不累积。
            delay = started + index / args.rate - time.monotonic()
            if delay > 0:
                await asyncio.sleep(delay)
        tasks.append(asyncio.ensure_future(one_connection(args, result)))
    dial_seconds = time.monotonic() - started
    await asyncio.gather(*tasks)
    return result, dial_seconds, time.monotonic() - started


def percentile(sorted_values, q):
    # 最近秩:只看量级,不插值。
    if not sorted_values:
        return None
    index = min(len(sorted_values) - 1, max(0, int(round(q * (len(sorted_values) - 1)))))
    return sorted_values[index]


def summary(args, result, dial_seconds, elapsed_seconds):
    out = {
        "target": f"{args.host}:{args.port}",
        "bind": args.bind or "",
        "connections": args.connections,
        "rate_per_second": args.rate,
        "hold_seconds": args.hold,
        "dial_seconds": round(dial_seconds, 3),
        "elapsed_seconds": round(elapsed_seconds, 3),
        "connect_errors": dict(sorted(result["connect_errors"].items())),
        "unexpected_data": result["unexpected_data"],
    }
    for name in OUTCOMES:
        out[name] = result[name]
    for name in ("reset", "closed"):
        values = sorted(result[name + "_after_ms"])
        out[name + "_after_ms"] = {
            "p50": percentile(values, 0.5),
            "p90": percentile(values, 0.9),
            "max": values[-1] if values else None,
        }
    return out


def print_summary(out, as_json):
    if as_json:
        print(json.dumps(out, sort_keys=True))
        return
    print(f"preauth_flood target={out['target']} bind={out['bind'] or '-'} connections={out['connections']} "
          f"rate={out['rate_per_second']:g}/s hold={out['hold_seconds']:g}s dial={out['dial_seconds']:.1f}s "
          f"elapsed={out['elapsed_seconds']:.1f}s")
    print("  " + " ".join(f"{name}={out[name]}" for name in OUTCOMES))
    for name in ("reset", "closed"):
        stats = out[name + "_after_ms"]
        cells = " ".join(f"{key}={'-' if value is None else f'{value:.0f}'}" for key, value in stats.items())
        print(f"  {name}_after_ms {cells}")
    if out["connect_errors"]:
        print("  connect_errors " + " ".join(f"{key}={value}" for key, value in out["connect_errors"].items()))
    if out["unexpected_data"]:
        print(f"  unexpected_data={out['unexpected_data']} (the server sent bytes to a silent client: wrong port or protocol?)")


def warn_if_fd_limit_too_low(connections):
    try:
        import resource
    except ImportError:  # Windows 没有 resource 模块
        return
    soft, _ = resource.getrlimit(resource.RLIMIT_NOFILE)
    if soft != resource.RLIM_INFINITY and connections + 64 > soft:
        print(f"warning: --connections {connections} needs more file descriptors than ulimit -n {soft}; "
              "connections beyond it fail with EMFILE and show up as connect_failed", file=sys.stderr)


async def self_test():
    # 本进程起一个服务端:第 0、3、6 条接上就 RST,第 1、4、7 条 200ms 后 FIN,其余一直挂着、等客户端先关。
    accepted = 0

    async def handle(reader, writer):
        nonlocal accepted
        index = accepted
        accepted += 1
        if index % 3 == 0:
            linger = struct.pack("hh" if sys.platform == "win32" else "ii", 1, 0)
            writer.get_extra_info("socket").setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, linger)
            writer.transport.abort()
            return
        if index % 3 == 1:
            await asyncio.sleep(0.2)
        else:
            await reader.read(1)
        writer.close()

    server = await asyncio.start_server(handle, "127.0.0.1", 0)
    port = server.sockets[0].getsockname()[1]
    args = argparse.Namespace(host="127.0.0.1", port=port, connections=9, rate=0.0, hold=1.0, bind=None, connect_timeout=3.0)
    result, _, _ = await flood(args)
    server.close()
    await server.wait_closed()
    actual = {name: result[name] for name in OUTCOMES}
    expected = {"connect_failed": 0, "reset": 3, "closed": 3, "held": 3}
    problems = []
    if actual != expected:
        problems.append(f"outcomes {actual}, expected {expected}")
    if not all(ms < 1000 for ms in result["reset_after_ms"]):
        problems.append(f"reset_after_ms {result['reset_after_ms']} should all be < 1000")
    if not all(150 <= ms < 1000 for ms in result["closed_after_ms"]):
        problems.append(f"closed_after_ms {result['closed_after_ms']} should all be in [150, 1000)")
    if problems:
        print("preauth_flood self-test FAILED: " + "; ".join(problems), file=sys.stderr)
        return 1
    print("preauth_flood self-test OK: " + json.dumps(actual, sort_keys=True))
    return 0


def parse_args(argv):
    parser = argparse.ArgumentParser(
        description="Open TCP connections at a fixed rate and never send a byte (an unauthenticated client), "
                    "then report how the server ended each one: connect_failed / reset (RST at accept) / "
                    "closed (FIN: reaped or evicted) / held. Point it only at servers you operate.")
    parser.add_argument("--host", help="gate / battle client address")
    parser.add_argument("--port", type=int, help="gate / battle client port")
    parser.add_argument("--connections", type=int, default=100, help="connections to open (default 100)")
    parser.add_argument("--rate", type=float, default=0.0, help="connections per second; 0 = as fast as possible (default 0)")
    parser.add_argument("--hold", type=float, default=15.0,
                        help="seconds to keep each connection open before closing it ourselves (default 15, above the 10 s handshake deadline)")
    parser.add_argument("--bind", default=None, help="local source address (e.g. one address per /24 for the per-source tests)")
    parser.add_argument("--connect-timeout", type=float, default=3.0, help="seconds to wait for the TCP handshake (default 3)")
    parser.add_argument("--json", action="store_true", help="print the summary as one JSON line")
    parser.add_argument("--self-test", action="store_true", help="run against an in-process server and check the classification")
    args = parser.parse_args(argv)
    if args.self_test:
        return args
    if not args.host or args.port is None:
        parser.error("--host and --port are required (or use --self-test)")
    if not 1 <= args.port <= 65535:
        parser.error("--port must be 1..65535")
    if args.connections < 1:
        parser.error("--connections must be at least 1")
    if args.rate < 0:
        parser.error("--rate must not be negative")
    if args.hold <= 0 or args.connect_timeout <= 0:
        parser.error("--hold and --connect-timeout must be positive")
    return args


def main(argv):
    args = parse_args(argv)
    if args.self_test:
        return asyncio.run(self_test())
    warn_if_fd_limit_too_low(args.connections)
    result, dial_seconds, elapsed_seconds = asyncio.run(flood(args))
    print_summary(summary(args, result, dial_seconds, elapsed_seconds), args.json)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
