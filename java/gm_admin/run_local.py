"""独立 GM 的本机准备和启动入口；密钥仅进入私有文件、环境与内存。"""
from __future__ import annotations
import argparse
import csv
import ctypes
import datetime as dt
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
RUNTIME = ROOT / "build/gm-admin-runtime"
PRIVATE = RUNTIME / "private.json"
UPSTREAM = ROOT / "build/gm-admin-upstream"
COMPOSE = ROOT / "deploy/gm-admin/compose.yml"
JAVA = Path("D:/luyuan/tools/jdk23/jdk-23.0.2+7/bin/java.exe")
HIDDEN = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0
DOCKER = ["docker", "--context", "desktop-linux"]
SECRET_FIELDS = ("GM_MYSQL_ROOT_PASSWORD", "GM_DB_PASSWORD", "GM_REDIS_PASSWORD", "GM_BOOTSTRAP_PASSWORD")


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
    for key in ("GM_DATA_TARGET", "GM_LOCATOR_TARGET", "GM_SCENE_ADMIN_SECRET", "GM_SCENE_TARGETS_JSON", "GM_MAIL_TARGET", "GM_MAIL_ADMIN_SECRET"):
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


def launch(kind):
    command(kind)
    receipt = RUNTIME / f"{kind}-process.json"
    if receipt.exists() and process_alive(json.loads(receipt.read_text(encoding="utf-8"))["workerPid"]):
        print(f"GM {kind} supervisor is already running")
        return
    port = 18090 if kind == "backend" else 3101
    with socket.socket() as probe:
        try:
            probe.bind(("127.0.0.1", port))
        except OSError:
            raise RuntimeError(f"Loopback port {port} is already in use") from None
    log = RUNTIME / "logs" / f"{kind}-{dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.log"
    child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), kind, "--worker", "--log", str(log)],
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             creationflags=HIDDEN, close_fds=True)
    print(f"GM {kind} startup requested (supervisor PID {child.pid}); log: {log}")


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
        for kind in ("backend", "frontend"):
            receipt = RUNTIME / f"{kind}-process.json"
            if receipt.exists():
                record = json.loads(receipt.read_text(encoding="utf-8"))
                record["supervisorAlive"] = process_alive(record["workerPid"])
                print(json.dumps(record, ensure_ascii=False))
            else:
                print(f"GM {kind} has no local process receipt")
        return
    values = materials()
    if args.worker:
        if args.action not in ("backend", "frontend") or args.log is None:
            raise RuntimeError("Invalid supervisor invocation")
        worker(args.action, args.log, values)
    elif args.action == "prepare":
        print(f"GM private credentials prepared; administrator: admin; private file: {PRIVATE}")
    elif args.action == "infra":
        infra(values)
    else:
        launch(args.action)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        print(f"GM startup failed: {type(error).__name__}. Check local prerequisites; no secrets were printed.", file=sys.stderr)
        sys.exit(1)
