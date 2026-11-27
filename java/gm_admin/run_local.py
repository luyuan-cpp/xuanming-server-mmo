"""独立 GM 的本机准备和启动入口；密钥仅进入私有文件、环境与内存。"""
from __future__ import annotations
import argparse
import csv
import ctypes
from contextlib import contextmanager
import datetime as dt
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
RUNTIME = ROOT / "build/gm-admin-runtime"
PRIVATE = RUNTIME / "private.json"
UPSTREAM = ROOT / "build/gm-admin-upstream"
COMPOSE = ROOT / "deploy/gm-admin/compose.yml"
JAVA = Path("D:/luyuan/tools/jdk23/jdk-23.0.2+7/bin/java.exe")
HIDDEN = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0
DOCKER = ["docker", "--context", "desktop-linux"]
SECRET_FIELDS = ("GM_MYSQL_ROOT_PASSWORD", "GM_DB_PASSWORD", "GM_REDIS_PASSWORD", "GM_BOOTSTRAP_PASSWORD")
WEB_PORTS = {"backend": 18090, "frontend": 3101}
WEB_URLS = {"backend": "http://127.0.0.1:18090/jeecg-boot", "frontend": "http://127.0.0.1:3101"}
WEB_TIMEOUTS = {"backend": 180, "frontend": 60}
SYSTEM_QUERY_TIMEOUT = 20


def read_private():
    """启动检查只读取已有凭据，不创建、改权限或修复配置。"""
    try:
        values = json.loads(PRIVATE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise RuntimeError("Prepared GM private credentials are missing or invalid") from None
    if not isinstance(values, dict) or any(not isinstance(values.get(name), str)
            or not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", values[name]) for name in SECRET_FIELDS):
        raise RuntimeError("Prepared GM private credentials are invalid")
    return values


def execute(args, *, env=None, check=True, timeout=30):
    result = subprocess.run(args, env=env, capture_output=True, text=True, encoding="utf-8",
                            errors="replace", timeout=timeout, creationflags=HIDDEN)
    if check and result.returncode:
        if RUNTIME.exists():
            detail = result.stdout + result.stderr
            for key, value in (env or {}).items():
                if value and any(word in key.upper() for word in ("PASSWORD", "SECRET", "TOKEN", "KEY")):
                    detail = detail.replace(value, "[redacted]")
            (RUNTIME / "last-command-failure.log").write_text(detail, encoding="utf-8")
        raise RuntimeError(f"Command failed (exit {result.returncode}): {Path(args[0]).name}")
    return result


def protect(path):
    if os.name == "nt":
        identity = execute(["whoami", "/user", "/fo", "csv", "/nh"]).stdout.strip()
        sid = next(csv.reader([identity]))[-1]
        execute(["icacls", str(path), "/inheritance:r", "/grant:r", f"*{sid}:(F)", "*S-1-5-18:(F)"])
    else:
        path.chmod(0o700 if path.is_dir() else 0o600)


def atomic_json(path, value):
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.touch(exist_ok=True)
    protect(temporary)
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8")
    temporary.replace(path)


def materials():
    RUNTIME.mkdir(parents=True, exist_ok=True)
    if not PRIVATE.exists():
        for volume in ("xuanming-gm-admin-mysql-data", "xuanming-gm-admin-redis-data"):
            if execute(DOCKER + ["volume", "inspect", volume], check=False).returncode == 0:
                raise RuntimeError("GM data exists but private.json is missing; restore original credentials first")
        values = {name: secrets.token_urlsafe(36) for name in SECRET_FIELDS}
        values["adminUsername"] = "admin"
        atomic_json(PRIVATE, values)
    values = json.loads(PRIVATE.read_text(encoding="utf-8"))
    for name in SECRET_FIELDS:
        if not isinstance(values.get(name), str) or not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", values[name]):
            raise RuntimeError(f"Invalid private credential field: {name}")
    protect(PRIVATE)
    redis_file = RUNTIME / "redis.conf"
    config = "bind 0.0.0.0\nprotected-mode yes\nappendonly yes\nrequirepass " + values["GM_REDIS_PASSWORD"] + "\n"
    if not redis_file.exists() or redis_file.read_text(encoding="utf-8") != config:
        redis_file.touch(exist_ok=True)
        protect(redis_file)
        redis_file.write_text(config, encoding="utf-8")
    (RUNTIME / "logs").mkdir(exist_ok=True)
    (RUNTIME / "uploads").mkdir(exist_ok=True)
    return values


def gateway_key():
    if os.environ.get("GM_GATEWAY_ADMIN_KEY"):
        return os.environ["GM_GATEWAY_ADMIN_KEY"]
    try:
        inspected = json.loads(execute(DOCKER + ["inspect", "xuanming-debug-current-gateway-1"]).stdout)[0]
        env = dict(item.split("=", 1) for item in inspected["Config"]["Env"])
        if env.get("ADMIN_API_KEY"):
            return env["ADMIN_API_KEY"]
        content = execute(DOCKER + ["exec", "xuanming-debug-current-gateway-1", "cat", "/app/config/application.yaml"]).stdout
        match = re.search(r"(?m)^admin:\s*\n\s+api-key:\s*([^\r\n]+)", content)
        if not match:
            return ""
        value = match.group(1).strip()
        if value.startswith('"'):
            value = json.loads(value)
        elif value.startswith("'") and value.endswith("'"):
            value = value[1:-1].replace("''", "'")
        return "" if "${" in value or value == "change-me-in-production" else value
    except (RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired):
        return ""


def environment(values):
    env = os.environ.copy()
    env.pop("GM_MYSQL_ROOT_PASSWORD", None)
    env.update({name: values[name] for name in SECRET_FIELDS if name != "GM_MYSQL_ROOT_PASSWORD"})
    env.update({"GM_DB_USER": "gm_admin", "GM_JDBC_URL": "jdbc:mysql://127.0.0.1:13391/gm_admin?"
                "useUnicode=true&characterEncoding=UTF-8&serverTimezone=UTC&useSSL=false&allowPublicKeyRetrieval=true",
                "GM_REDIS_PORT": "16391", "GM_HTTP_PORT": "18090", "GM_UPLOAD_PATH": str(RUNTIME / "uploads"),
                "GM_GATEWAY_ADMIN_KEY": gateway_key(), "GM_GAME_PLAINTEXT": "true"})
    # 游戏写通道只认本任务部署后的真实就绪回执；外层进程遗留变量不能跳过验收。
    # 交易管理通道本机还没有经过验收的转发,外层遗留变量同样不能打开它。
    for key in ("GM_DATA_TARGET", "GM_LOCATOR_TARGET", "GM_SCENE_ADMIN_SECRET", "GM_SCENE_TARGETS_JSON", "GM_SCENE_ETCD_URL", "GM_MAIL_TARGET", "GM_MAIL_ADMIN_SECRET",
                "GM_TRADE_TARGET", "GM_TRADE_ADMIN_SECRET"):
        env.pop(key, None)
    env.update(verified_read_runtime())
    env.update(verified_game_runtime())
    return env


def verified_read_runtime():
    try:
        scripts = str(ROOT / "tools/scripts")
        if scripts not in sys.path:
            sys.path.insert(0, scripts)
        import gm_scene_promotion
        return gm_scene_promotion.verified_read_bridge(ROOT)
    except Exception:
        return {}


def verified_game_runtime():
    try:
        scripts = str(ROOT / "tools/scripts")
        if scripts not in sys.path:
            sys.path.insert(0, scripts)
        import gm_scene_promotion
        import start_docker_debug
        def process_environment(identity):
            raw = start_docker_debug.run(DOCKER + ["exec", identity, "cat", "/proc/1/environ"])
            return dict(value.split("=", 1) for value in raw.split("\0") if "=" in value)
        return gm_scene_promotion.ready_runtime(ROOT, start_docker_debug.preflight,
            start_docker_debug.inspect, process_environment, verified_read_runtime)
    except Exception:
        # 缺少、陈旧或被改动的回执都关闭写通道；不可打印异常原文或私有内容。
        return {}


def infra(values):
    sql = RUNTIME / "sql/001-jeecg.sql"
    if not sql.is_file() or sql.stat().st_size == 0:
        raise RuntimeError("Prepared Jeecg SQL is missing; run prepare.py first")
    env = os.environ.copy()
    env.update({name: values[name] for name in SECRET_FIELDS})
    execute(DOCKER + ["compose", "--project-name", "xuanming-gm-admin", "-f", str(COMPOSE),
                     "up", "-d", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "180"], env=env, timeout=210)
    print("GM MySQL and Redis are healthy (loopback 13391 / 16391)")


def process_alive(pid):
    if os.name == "nt":
        kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel.OpenProcess.restype = ctypes.c_void_p
        handle = kernel.OpenProcess(0x1000, False, pid)
        if not handle:
            return False
        code = ctypes.c_ulong()
        try:
            return bool(kernel.GetExitCodeProcess(ctypes.c_void_p(handle), ctypes.byref(code))) and code.value == 259
        finally:
            kernel.CloseHandle(ctypes.c_void_p(handle))
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


def command(kind):
    if kind not in WEB_PORTS:
        raise RuntimeError("Unknown GM web service")
    if kind == "backend":
        jar = UPSTREAM / "jeecg-boot/jeecg-module-system/jeecg-system-start/target/jeecg-system-start-3.9.6.jar"
        if not JAVA.is_file() or not jar.is_file():
            raise RuntimeError("GM Java runtime or built backend JAR is missing")
        return [str(JAVA), "-Xms256m", "-Xmx2g", "-jar", str(jar), "--spring.profiles.active=dev,gm"], jar.parent
    frontend = UPSTREAM / "jeecgboot-vue3"
    vite = frontend / "node_modules/vite/bin/vite.js"
    node = shutil.which("node")
    if not node or not vite.is_file() or not (frontend / ".env.gm").is_file():
        raise RuntimeError("Prepared GM frontend or installed Vite is missing")
    return [node, str(vite), "--mode", "gm", "--host", "127.0.0.1", "--port", "3101", "--strictPort"], frontend


def windows_arguments(command_line):
    shell = ctypes.WinDLL("shell32", use_last_error=True)
    shell.CommandLineToArgvW.argtypes = [ctypes.c_wchar_p, ctypes.POINTER(ctypes.c_int)]
    shell.CommandLineToArgvW.restype = ctypes.POINTER(ctypes.c_wchar_p)
    count = ctypes.c_int()
    pointer = shell.CommandLineToArgvW(command_line, ctypes.byref(count))
    if not pointer:
        raise RuntimeError("Cannot inspect GM process arguments")
    try:
        return [pointer[index] for index in range(count.value)]
    finally:
        kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel.LocalFree.argtypes = [ctypes.c_void_p]
        kernel.LocalFree(ctypes.cast(pointer, ctypes.c_void_p))


def system_query(script, *, kind="process ownership"):
    # 查询结果可能含其他进程的参数，只在内存比较，失败不写入通用命令日志。
    try:
        script = "[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false); " + script
        result = subprocess.run(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script],
            capture_output=True, text=True, encoding="utf-8", errors="replace",
            timeout=SYSTEM_QUERY_TIMEOUT, creationflags=HIDDEN)
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"GM {kind} query timed out after {SYSTEM_QUERY_TIMEOUT} seconds") from None
    except OSError:
        raise RuntimeError(f"GM {kind} query could not start") from None
    if result.returncode:
        raise RuntimeError(f"GM {kind} query failed (exit {result.returncode})")
    try:
        return json.loads(result.stdout) if result.stdout.strip() else None
    except ValueError:
        raise RuntimeError(f"GM {kind} query returned invalid JSON") from None


def process_identity(pid):
    if type(pid) is not int or pid <= 0:
        raise RuntimeError("Invalid GM process receipt PID")
    if os.name == "nt":
        row = system_query("$ErrorActionPreference='Stop'; "
            f"$p=Get-CimInstance Win32_Process -Filter 'ProcessId = {pid}'; "
            "if ($null -ne $p) { [pscustomobject]@{pid=[int]$p.ProcessId; "
            "created=$p.CreationDate.ToUniversalTime().ToString('o'); executable=$p.ExecutablePath; "
            "commandLine=$p.CommandLine} | ConvertTo-Json -Compress }", kind="process identity")
        if row is None:
            return None
        if not row.get("created") or not row.get("executable") or not row.get("commandLine"):
            raise RuntimeError("GM process identity is not readable")
        return {"pid": pid, "created": row["created"], "executable": row["executable"],
                "argv": windows_arguments(row["commandLine"])}
    directory = Path("/proc") / str(pid)
    try:
        fields = (directory / "stat").read_text().rsplit(")", 1)[1].split()
        if fields[0] == "Z":
            return None
        boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
        return {"pid": pid, "created": boot + ":" + fields[19],
                "executable": str((directory / "exe").resolve(strict=True)),
                "argv": [os.fsdecode(value) for value in (directory / "cmdline").read_bytes().split(b"\0") if value]}
    except FileNotFoundError:
        return None
    except (OSError, IndexError):
        raise RuntimeError("Cannot inspect local GM process identity") from None


def listener_owners(port):
    if os.name == "nt":
        rows = system_query("$ErrorActionPreference='Stop'; @("
            f"Get-NetTCPConnection -State Listen -ErrorAction Stop | Where-Object LocalPort -eq {port} "
            "| ForEach-Object { [pscustomobject]@{pid=[int]$_.OwningProcess; address=$_.LocalAddress} }) "
            "| ConvertTo-Json -Compress", kind="TCP listener ownership")
        if rows is None:
            return []
        return rows if isinstance(rows, list) else [rows]
    # Linux 本机脚本沿用 /proc，只接受精确回环监听；不借助端口可连通推断进程身份。
    sockets = {}
    for table in (Path("/proc/net/tcp"), Path("/proc/net/tcp6")):
        for line in table.read_text().splitlines()[1:]:
            fields = line.split()
            address, number = fields[1].split(":")
            if int(number, 16) == port and fields[3] == "0A":
                sockets[fields[9]] = "127.0.0.1" if address == "0100007F" else address
    result = []
    for process in Path("/proc").iterdir():
        if not process.name.isdigit():
            continue
        try:
            for descriptor in (process / "fd").iterdir():
                match = re.fullmatch(r"socket:\[(\d+)\]", os.readlink(descriptor))
                if match and match[1] in sockets:
                    result.append({"pid": int(process.name), "address": sockets[match[1]]})
        except (FileNotFoundError, PermissionError):
            continue
    if sockets and not result:
        raise RuntimeError("GM listener ownership is not readable")
    return result


def normalized_argument(value):
    return os.path.normcase(os.path.realpath(value)) if os.path.isabs(value) else value


def matching_process(actual, expected, recorded=None):
    if not actual:
        return False
    if (normalized_argument(actual["executable"]) != normalized_argument(expected[0])
            or [normalized_argument(value) for value in actual["argv"]]
            != [normalized_argument(value) for value in expected]):
        return False
    return recorded is None or actual == recorded


def worker_command(kind, log):
    return [sys.executable, str(Path(__file__).resolve()), kind, "--worker", "--log", str(log)]


def load_process_receipt(kind, suffix="process"):
    path = RUNTIME / f"{kind}-{suffix}.json"
    if not path.exists():
        return None
    try:
        record = json.loads(path.read_text(encoding="utf-8"))
        log = Path(record["log"])
        if (record["kind"] != kind or type(record["workerPid"]) is not int or record["workerPid"] <= 0
                or not log.is_absolute() or log.resolve() != log or not log.is_relative_to(RUNTIME / "logs")):
            raise ValueError()
        if suffix == "process" and (type(record["childPid"]) is not int or record["childPid"] <= 0):
            raise ValueError()
        return record
    except (OSError, ValueError, KeyError, TypeError):
        raise RuntimeError(f"GM {kind} process receipt is invalid") from None


def http_ready(kind):
    connection = http.client.HTTPConnection("127.0.0.1", WEB_PORTS[kind], timeout=2)
    try:
        connection.request("GET", "/jeecg-boot/gm/overview" if kind == "backend" else "/")
        response = connection.getresponse()
        body = response.read(65537)
        if len(body) > 65536:
            return False
        content_type = response.getheader("Content-Type", "").lower()
        if kind == "backend":
            if response.status != 401 or "application/json" not in content_type:
                return False
            try:
                value = json.loads(body)
            except (ValueError, UnicodeError):
                return False
            return isinstance(value, dict) and value.get("code") == 401 and value.get("success") is False
        text = body.decode("utf-8", errors="replace")
        return (response.status == 200 and "text/html" in content_type and "<html" in text.lower()
                and re.search(r'<script\b[^>]*\bsrc=[\'\"]/\@vite/client[\'\"]', text) is not None
                and re.search(r'<script\b[^>]*\bsrc=[\'\"]/src/main\.ts(?:\?[^\'\"]*)?[\'\"]', text) is not None)
    except (OSError, http.client.HTTPException):
        return False
    finally:
        connection.close()


def web_state(kind, *, _refreshes=0):
    def refresh_snapshot():
        if _refreshes >= 2:
            raise RuntimeError(f"GM {kind} process receipt changed repeatedly during identity inspection")
        return web_state(kind, _refreshes=_refreshes + 1)

    args, _ = command(kind)
    record = load_process_receipt(kind)
    child = process_identity(record["childPid"]) if record else None
    child_matches = matching_process(child, args, record.get("childIdentity") if record else None)
    worker = process_identity(record["workerPid"]) if record else None
    worker_matches = bool(record) and matching_process(worker, worker_command(kind, record["log"]), record.get("workerIdentity"))
    owners = listener_owners(WEB_PORTS[kind])
    # 慢速系统查询期间 supervisor 可能发布新回执；旧 PID 与新端口不能混成同一快照。
    if load_process_receipt(kind) != record:
        return refresh_snapshot()
    if not child_matches:
        pending = load_process_receipt(kind, "launch")
        if pending and isinstance(pending.get("workerIdentity"), dict):
            identity = process_identity(pending["workerPid"])
            if load_process_receipt(kind) != record or load_process_receipt(kind, "launch") != pending:
                return refresh_snapshot()
            if (matching_process(identity, worker_command(kind, pending["log"]), pending["workerIdentity"])
                    and (record is None or record["workerPid"] != pending["workerPid"]
                         or record.get("workerIdentity") != pending["workerIdentity"])):
                # Vite 可先监听，再由 worker 写入完整子进程身份；这里只等待，不认可未知监听已就绪。
                return {"state": "starting", "record": pending, "worker": identity, "child": None}
    if owners and (not child_matches or any(owner["pid"] != child["pid"]
            or owner["address"] not in ("127.0.0.1", "::ffff:127.0.0.1") for owner in owners)):
        raise RuntimeError(f"GM {kind} loopback port is owned by an unverified process or address")
    if child_matches:
        ready = bool(owners) and http_ready(kind)
        if ready and process_identity(child["pid"]) != child:
            raise RuntimeError(f"GM {kind} process identity changed during readiness check")
        return {"state": "ready" if ready else "starting", "record": record, "child": child,
                "worker": worker if worker_matches else None}
    if worker_matches:
        raise RuntimeError(f"GM {kind} supervised process has exited")
    # PID 已被复用的旧回执仅视为陈旧证据；不接管、不终止未知进程。
    with socket.socket() as probe:
        try:
            probe.bind(("127.0.0.1", WEB_PORTS[kind]))
        except OSError:
            raise RuntimeError(f"GM {kind} loopback port is unavailable") from None
    return {"state": "stopped", "record": None, "worker": None, "child": None}


def web_preflight():
    read_private()
    if not (RUNTIME / "logs").is_dir() or not (RUNTIME / "uploads").is_dir():
        raise RuntimeError("Prepared GM runtime directories are missing")
    services = {}
    for kind in WEB_PORTS:
        services[kind] = {"state": web_state(kind)["state"], "url": WEB_URLS[kind]}
    return {"schema": 1, "services": services}


@contextmanager
def web_start_lock():
    # 此锁独立于游戏启动锁，允许一键入口在持有游戏锁时串行确保 GM 网页。
    with (RUNTIME / ".gm-web-start.lock").open("a+b") as stream:
        stream.seek(0, 2)
        if stream.tell() == 0:
            stream.write(b"0")
            stream.flush()
        stream.seek(0)
        try:
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise RuntimeError("Another GM web startup is active") from None
        try:
            yield
        finally:
            stream.seek(0)
            if os.name == "nt":
                msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(stream.fileno(), fcntl.LOCK_UN)


def launch_locked(kind):
    state = web_state(kind)
    if state["state"] != "stopped":
        return False
    log = RUNTIME / "logs" / f"{kind}-{dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')}.log"
    child = subprocess.Popen(worker_command(kind, log), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL, creationflags=HIDDEN, close_fds=True)
    identity = process_identity(child.pid)
    if not matching_process(identity, worker_command(kind, log)):
        raise RuntimeError(f"GM {kind} supervisor failed before its identity could be verified")
    # 独立意图文件避免父进程覆盖已快速写出的运行回执；等待期间也能拒绝重复启动。
    atomic_json(RUNTIME / f"{kind}-launch.json", {"kind": kind, "workerPid": child.pid,
        "workerIdentity": identity, "log": str(log)})
    return True


def launch(kind):
    read_private()
    with web_start_lock():
        started = launch_locked(kind)
    print(f"GM {kind} startup requested" if started else f"GM {kind} process is already active")


def wait_web_ready(kind):
    deadline = time.monotonic() + WEB_TIMEOUTS[kind]
    while True:
        state = web_state(kind)
        if state["state"] == "ready":
            record = dict(state["record"])
            # 旧回执必须先通过完整命令、真实监听 PID 与 HTTP 验证，再补充创建时间。
            record["childIdentity"] = state["child"]
            if state["worker"]:
                record["workerIdentity"] = state["worker"]
            if record != state["record"]:
                atomic_json(RUNTIME / f"{kind}-process.json", record)
            return
        if state["state"] == "stopped":
            raise RuntimeError(f"GM {kind} startup process exited before HTTP readiness")
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError(f"GM {kind} did not become HTTP-ready within {WEB_TIMEOUTS[kind]} seconds")
        time.sleep(min(0.5, remaining))


def ensure_web_started():
    read_private()
    with web_start_lock():
        web_preflight()
        services = {}
        for kind in WEB_PORTS:
            started = launch_locked(kind)
            wait_web_ready(kind)
            services[kind] = {"state": "ready", "action": "started" if started else "already-running", "url": WEB_URLS[kind]}
        return {"schema": 1, "services": services}


def worker(kind, log, values):
    env = environment(values) if kind == "backend" else os.environ.copy()
    if kind == "frontend":
        env = {key: value for key, value in env.items() if not key.startswith("GM_")}
    args, cwd = command(kind)
    hidden_values = sorted({value for key, value in env.items()
                            if key.startswith("GM_") and any(term in key for term in ("PASSWORD", "SECRET", "KEY"))
                            and value}, key=len, reverse=True)
    receipt = RUNTIME / f"{kind}-process.json"
    with log.open("w", encoding="utf-8") as output:
        proc = subprocess.Popen(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, encoding="utf-8", errors="replace",
                                creationflags=HIDDEN, close_fds=True)
        record = {"kind": kind, "workerPid": os.getpid(), "childPid": proc.pid,
                  "startedAt": dt.datetime.now(dt.timezone.utc).isoformat(), "log": str(log), "state": "running"}
        record["workerIdentity"] = process_identity(os.getpid())
        record["childIdentity"] = process_identity(proc.pid)
        if not matching_process(record["workerIdentity"], worker_command(kind, log)):
            raise RuntimeError("GM supervisor identity could not be recorded")
        atomic_json(receipt, record)
        for line in proc.stdout:
            for value in hidden_values:
                line = line.replace(value, "[redacted]")
            output.write(line)
            output.flush()
        record.update(state="exited", exitCode=proc.wait())
        atomic_json(receipt, record)


def main():
    parser = argparse.ArgumentParser(description="Independent local Jeecg GM runtime")
    parser.add_argument("action", nargs="?", choices=("prepare", "infra", "backend", "frontend", "status"), default="prepare")
    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--log", type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.action == "status":
        print(json.dumps(web_preflight(), ensure_ascii=False))
        return
    if args.worker:
        if args.action not in ("backend", "frontend") or args.log is None:
            raise RuntimeError("Invalid supervisor invocation")
        worker(args.action, args.log, read_private())
    elif args.action == "prepare":
        materials()
        print(f"GM private credentials prepared; administrator: admin; private file: {PRIVATE}")
    elif args.action == "infra":
        infra(materials())
    else:
        launch(args.action)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        print(f"GM startup failed: {type(error).__name__}. Check local prerequisites; no secrets were printed.", file=sys.stderr)
        sys.exit(1)
