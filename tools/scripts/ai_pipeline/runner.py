#!/usr/bin/env python3
"""任务驱动的本地验证入口；只编排现有检查，不承担发布或业务规则。"""
from __future__ import annotations

import argparse
import contextlib
from datetime import datetime, timezone
import glob
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time
import uuid
import xml.etree.ElementTree as ET


ROOT = Path(__file__).resolve().parents[3]
KINDS = {"static", "build", "unit", "contract", "integration", "e2e", "visual", "performance"}
REPOSITORIES = {"server_cpp", "server_java", "client"}
IDENTIFIER = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$")
TOKEN = re.compile(r"\{(python|run_dir|repo\.[a-zA-Z0-9_]+|tool\.[a-zA-Z0-9_]+)\}")


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def read_json(path):
    with Path(path).open(encoding="utf-8-sig") as stream:
        return json.load(stream)


def write_json(path, value):
    # 同一目录原子替换，避免中途读取半份状态。
    path = Path(path)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def require(condition, message):
    if not condition:
        raise ValueError(message)


def string(value):
    return isinstance(value, str) and bool(value.strip())


def relative_path(value):
    require(string(value), "Paths must be nonempty strings")
    normalized = value.replace("\\", "/")
    require(not Path(normalized).is_absolute() and not re.match(r"^[A-Za-z]:", normalized)
            and ".." not in normalized.split("/"), f"Path must stay inside its repository: {value}")
    return normalized


def inside(root, value):
    path = (root / relative_path(value)).resolve()
    require(path.is_relative_to(root.resolve()), f"Path escapes repository: {value}")
    return path


def validate(task, catalog):
    require(isinstance(task, dict) and task.get("schema_version") == 1, "Unsupported task schema")
    require(isinstance(catalog, dict) and catalog.get("schema_version") == 1, "Unsupported catalog schema")
    require(isinstance(catalog.get("checks"), dict), "Catalog checks must be an object")
    for key in ("id", "title", "goal"):
        require(string(task.get(key)), f"Missing task {key}")
    require(IDENTIFIER.fullmatch(task["id"]), "Invalid task id")
    repos = task.get("repositories")
    require(isinstance(repos, list) and repos and all(isinstance(r, str) for r in repos), "Missing repositories")
    require(len(repos) == len(set(repos)) and set(repos) <= REPOSITORIES, "Unknown or duplicate repository")
    scope = task.get("scope")
    require(isinstance(scope, dict) and set(scope) == set(repos), "Scope must cover exactly the task repositories")
    for paths in scope.values():
        require(isinstance(paths, list) and paths, "Each repository needs explicit source scope")
        for path in paths:
            relative_path(path)
    checks = task.get("checks")
    require(isinstance(checks, list) and checks, "Task must contain checks")
    ids = set()
    required = set()
    for selection in checks:
        require(isinstance(selection, dict), "Invalid check selection")
        check_id = selection.get("id")
        require(isinstance(check_id, str) and IDENTIFIER.fullmatch(check_id), "Invalid check id")
        require(check_id not in ids, f"Duplicate check: {check_id}")
        ids.add(check_id)
        require(type(selection.get("required")) is bool, f"Check {check_id} needs required=true/false")
        if selection["required"]:
            required.add(check_id)
        if "skip_reason" in selection:
            require(string(selection["skip_reason"]), "Empty skip reason")
        definition = catalog["checks"].get(check_id)
        require(isinstance(definition, dict), f"Unknown check: {check_id}")
        require(definition.get("repository") in repos, f"Undeclared repository for {check_id}")
        relative_path(definition.get("cwd", "."))
        argv = definition.get("argv")
        require(isinstance(argv, list) and argv and all(string(a) for a in argv), f"Invalid argv: {check_id}")
        timeout = definition.get("timeout_seconds")
        require(type(timeout) in (int, float) and 0 < timeout <= 14400, f"Invalid timeout: {check_id}")
        verifier = definition.get("verifier", {})
        require(isinstance(verifier, dict), f"Invalid verifier: {check_id}")
        require(verifier.get("type") in {"exit_code", "unittest", "go_json", "junit", "regex"}, f"Invalid verifier: {check_id}")
        if verifier["type"] == "regex":
            require(string(verifier.get("pattern")), f"Missing success pattern: {check_id}")
            re.compile(verifier["pattern"])
        if "reject_pattern" in verifier:
            re.compile(verifier["reject_pattern"])
        if verifier["type"] == "junit":
            paths = verifier.get("paths", [verifier.get("path")])
            require(isinstance(paths, list) and paths and all(string(p) for p in paths), "JUnit requires report paths")
        env = definition.get("env", {})
        require(isinstance(env, dict) and all(string(k) and isinstance(v, str) for k, v in env.items()), "Invalid check environment")
        require(isinstance(definition.get("inputs", []), list), "Check inputs must be a list")
        for value in definition.get("inputs", []):
            relative_path(value)
    require(required, "At least one check must be required")
    acceptance = task.get("acceptance")
    require(isinstance(acceptance, list) and acceptance, "Missing acceptance criteria")
    acceptance_ids = set()
    for criterion in acceptance:
        require(isinstance(criterion, dict) and string(criterion.get("id")) and string(criterion.get("description")), "Invalid acceptance criterion")
        require(criterion["id"] not in acceptance_ids, "Duplicate acceptance id")
        acceptance_ids.add(criterion["id"])
        linked = criterion.get("checks")
        require(isinstance(linked, list) and linked and all(isinstance(c, str) for c in linked)
                and set(linked) <= required, "Acceptance criteria must bind required checks")
    matrix = task.get("test_matrix")
    require(isinstance(matrix, dict) and set(matrix) == KINDS, "Test matrix must classify all eight test kinds")
    for kind, coverage in matrix.items():
        require(isinstance(coverage, dict), f"Invalid test matrix entry: {kind}")
        linked = coverage.get("checks")
        if linked is not None:
            require("not_applicable" not in coverage and isinstance(linked, list) and linked
                    and all(isinstance(c, str) for c in linked) and set(linked) <= ids, f"Invalid test mapping: {kind}")
        else:
            require(string(coverage.get("not_applicable")), f"Explain why {kind} is not applicable")
    require(isinstance(task.get("delivery"), dict) and task["delivery"].get("kind") == "development",
            "V1 only produces development verification deliveries")
    require(isinstance(task["delivery"].get("notes", ""), str), "Delivery notes must be a string")


def configuration(path):
    client = ROOT.parent / "mmorpg-client"
    version_path = client / "ProjectSettings/ProjectVersion.txt"
    version = ""
    if version_path.is_file():
        match = re.search(r"^m_EditorVersion:\s*(\S+)", version_path.read_text(encoding="utf-8-sig"), re.M)
        version = match[1] if match else ""
    config = {
        "repositories": {"server_cpp": str(ROOT), "client": str(client), "server_java": str(ROOT.parent / "xuanming-server-mmo")},
        "tools": {"unity_root": str(ROOT.parent / "tools/Unity" / version),
                  "jdk21": str(ROOT.parent / "tools/jdk21"), "go": "go"},
    }
    go_version = ROOT / "go/shared/go.mod"
    if go_version.is_file():
        match = re.search(r"^go\s+(\S+)", go_version.read_text(encoding="utf-8"), re.M)
        if match:
            candidate = ROOT.parent / f"tools/go{match[1]}/go/bin" / ("go.exe" if os.name == "nt" else "go")
            if candidate.is_file():
                config["tools"]["go"] = str(candidate)
    if path:
        supplied = read_json(path)
        require(isinstance(supplied, dict), "Invalid repository config")
        for key in ("repositories", "tools"):
            values = supplied.get(key, {})
            require(isinstance(values, dict) and all(string(k) and string(v) for k, v in values.items()), f"Invalid {key} config")
            if key == "repositories":
                require(set(values) <= REPOSITORIES, "Unknown repository in config")
                values = {k: str((Path(path).resolve().parent / v).resolve()) for k, v in values.items()}
            config[key].update(values)
    return config


def expand(value, values):
    def substitute(match):
        require(match[1] in values, f"Unknown placeholder: {match[0]}")
        return values[match[1]]
    # 不解释一般花括号；例如 Python -c 中的 dict 是普通参数。只识别本工具的命名空间。
    unknown = re.findall(r"\{(?:repo|tool)\.[^{}]+\}", value)
    for token in unknown:
        require(token[1:-1] in values, f"Unknown placeholder: {token}")
    return TOKEN.sub(substitute, value)


def git(root, *args):
    process = subprocess.run(["git", "-C", str(root), *args], stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, timeout=30, check=False,
                             creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
    if process.returncode:
        raise ValueError(f"Git command failed in {root}: {process.stderr.decode('utf-8', errors='replace').strip()}")
    return process.stdout


def snapshot(root, paths):
    for path in paths:
        inside(root, path)
    head = git(root, "rev-parse", "HEAD").decode().strip()
    status = git(root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
    names = git(root, "--literal-pathspecs", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", *paths)
    selected = set(n.decode("utf-8") for n in names.split(b"\0") if n)
    # 明确点名的输入文件（如 Unity 生成的 csproj）即使被 Git 忽略，也必须参与快照。
    for declared in paths:
        normalized = relative_path(declared).rstrip("/")
        if inside(root, declared).is_file():
            selected.add(normalized)
        require(any(normalized == "." or name == normalized or name.startswith(normalized + "/") for name in selected),
                f"Declared source scope has no matching files: {root / declared}")
    require(bool(selected), f"Source scope has no tracked or unignored files: {root}")
    records = []
    for name in sorted(selected):
        path = inside(root, name)
        records.append({"path": name, "sha256": digest(path) if path.is_file() else None})
    source_hash = hashlib.sha256(json.dumps(records, sort_keys=True).encode()).hexdigest()
    return {"path": str(root), "head": head, "dirty": bool(status),
            "working_tree_status_sha256": hashlib.sha256(status).hexdigest(),
            "scope_sha256": source_hash, "files": records}


@contextlib.contextmanager
def repository_locks(roots, task_id):
    acquired = []
    try:
        directories = set()
        for root in roots:
            common = git(root, "rev-parse", "--git-common-dir").decode().strip()
            directories.add((root / common).resolve())
        for directory in sorted(directories):
            path = directory / "ai-pipeline.lock"
            token = str(uuid.uuid4())
            try:
                fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            except FileExistsError as error:
                raise ValueError(f"Repository is busy; lock already exists: {path}") from error
            with os.fdopen(fd, "w", encoding="utf-8") as stream:
                json.dump({"pid": os.getpid(), "task_id": task_id, "token": token, "created_at": utc_now()}, stream)
            acquired.append((path, token))
        yield
    finally:
        for path, token in reversed(acquired):
            if path.exists() and read_json(path).get("token") == token:
                path.unlink()


def stop_process(process):
    if process.poll() is not None:
        return
    if os.name == "nt":
        # 只结束本次启动且仍存活的进程树，不按名称扫描或终止用户的其他编译器。
        subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15,
                       creationflags=subprocess.CREATE_NO_WINDOW, check=False)
    else:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGKILL)
    if process.poll() is None:
        process.kill()
    process.wait(timeout=15)


def verify(definition, text, values, started):
    verifier = definition["verifier"]
    kind = verifier["type"]
    if verifier.get("reject_pattern") and re.search(verifier["reject_pattern"], text, re.M):
        return False, "Output contains a rejected result", {}
    if kind == "exit_code":
        return True, "Command completed successfully", {}
    if kind == "regex":
        good = re.search(verifier["pattern"], text, re.M) is not None
        return good, "Expected success evidence found" if good else "Expected success evidence missing", {}
    if kind == "unittest":
        counts = re.findall(r"^Ran (\d+) tests? in ", text, re.M)
        skipped = re.search(r"^OK \(.*skipped=([1-9]\d*)", text, re.M)
        good = bool(counts) and int(counts[-1]) > 0 and not skipped and bool(re.search(r"^OK\s*$", text, re.M))
        return good, "Unit tests executed" if good else "Unit tests missing, empty, failed or skipped", {"tests": int(counts[-1]) if counts else 0}
    if kind == "go_json":
        events = []
        for line in text.splitlines():
            try:
                item = json.loads(line)
                if isinstance(item, dict):
                    events.append(item)
            except json.JSONDecodeError:
                continue
        passed = sum(e.get("Action") == "pass" and bool(e.get("Test")) for e in events)
        failed = any(e.get("Action") in {"fail", "skip"} for e in events)
        return passed > 0 and not failed, "Go test events checked", {"tests": passed}
    paths = verifier.get("paths", [verifier.get("path")])
    files = sorted(set(p for pattern in paths for p in glob.glob(expand(pattern, values), recursive=True)))
    if not files:
        return False, "JUnit reports missing", {}
    total = failures = skipped = 0
    evidence = []
    for name in files:
        path = Path(name).resolve()
        if not path.is_relative_to(Path(values["run_dir"])):
            return False, "JUnit reports must be produced inside this run directory", {}
        if path.stat().st_mtime < started - 1:
            return False, "JUnit report is stale", {}
        root = ET.parse(path).getroot()
        suites = [element for element in root.iter() if element.tag.rsplit("}", 1)[-1] == "testsuite"]
        if not suites:
            return False, "No JUnit test suites found", {}
        # 只计叶子 suite，避免 testsuites/嵌套 suite 的汇总重复计数。
        for suite in suites:
            if any(child is not suite and child.tag.rsplit("}", 1)[-1] == "testsuite" for child in suite.iter()):
                continue
            cases = [c for c in suite.iter() if c.tag.rsplit("}", 1)[-1] == "testcase"]
            counts = {key: int(suite.get(key, str(len(cases)) if key == "tests" else "0"))
                      for key in ("tests", "failures", "errors", "skipped", "disabled")}
            require(all(value >= 0 for value in counts.values()), "Negative JUnit counter")
            count = counts["tests"]
            require(count == len(cases), "JUnit count does not match testcase evidence")
            total += count
            failures += max(counts["failures"] + counts["errors"], sum(c.tag.rsplit("}", 1)[-1] in {"failure", "error"} for c in suite.iter()))
            skipped += max(counts["skipped"] + counts["disabled"], sum(c.tag.rsplit("}", 1)[-1] == "skipped" for c in suite.iter()))
        evidence.append({"path": str(path.relative_to(Path(values["run_dir"]))), "sha256": digest(path)})
    good = total > 0 and failures == 0 and skipped == 0
    return good, "JUnit reports checked", {"tests": total, "failures": failures, "skipped": skipped, "reports": evidence}


def run_check(check_id, definition, roots, values, output):
    log = output / "logs" / f"{check_id}.log"
    result = {"id": check_id, "status": "NOT_RUN", "reason": "Not started", "exit_code": None,
              "elapsed_seconds": 0, "log": log.relative_to(output).as_posix()}
    cwd = inside(roots[definition["repository"]], definition.get("cwd", "."))
    argv = [expand(a, values) for a in definition["argv"]]
    result["argv"] = argv
    result["cwd"] = str(cwd)
    if not cwd.is_dir():
        result["reason"] = f"Working directory missing: {cwd}"
        return result
    executable = shutil.which(argv[0])
    if executable is None:
        result["reason"] = f"Executable unavailable: {argv[0]}"
        return result
    argv[0] = executable
    env = os.environ.copy()
    env.update({"PYTHONIOENCODING": "utf-8", "PYTHONUTF8": "1"})
    env.update({key: expand(value, values) for key, value in definition.get("env", {}).items()})
    started = time.time()
    timer = time.monotonic()
    process = None
    try:
        with log.open("wb") as stream:
            kwargs = {"creationflags": subprocess.CREATE_NO_WINDOW | subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {"start_new_session": True}
            process = subprocess.Popen(argv, cwd=cwd, env=env, stdout=stream, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL, **kwargs)
            try:
                result["exit_code"] = process.wait(timeout=definition["timeout_seconds"])
            except subprocess.TimeoutExpired:
                stop_process(process)
                result.update(status="FAIL", reason="Command timed out", exit_code=process.returncode)
        if result["status"] != "FAIL":
            if result["exit_code"] != 0:
                result.update(status="FAIL", reason=f"Command exited with code {result['exit_code']}")
            else:
                ok, reason, metrics = verify(definition, log.read_text(encoding="utf-8", errors="replace"), values, started)
                result.update(status="PASS" if ok else "FAIL", reason=reason, metrics=metrics)
    except (OSError, ValueError, ET.ParseError) as error:
        result.update(status="FAIL", reason=f"Execution or evidence error: {error}")
    except KeyboardInterrupt:
        if process:
            stop_process(process)
        result.update(status="FAIL", reason="Interrupted by user")
    finally:
        if process and process.poll() is None:
            stop_process(process)
        result["elapsed_seconds"] = round(time.monotonic() - timer, 3)
        if log.exists():
            result["log_sha256"] = digest(log)
    return result


def save_report(output, report, task):
    write_json(output / "report.json", report)
    lines = [f"# {task['title']}：交付清单", "", f"- 任务：`{task['id']}`", f"- 目标：{task['goal']}",
             f"- 模式：`{report['mode']}`", f"- 结果：`{report['status']}`",
             f"- 满足本任务开发验收：{'是' if report['delivery_eligible'] else '否'}",
             "- 本清单不代表正式发布、Unity Player 或未列入任务的联网测试已经完成。", "",
             "| 检查 | 必需 | 结果 | 证据 / 原因 |", "|---|---|---|---|"]
    for check in report["checks"]:
        reason = check["reason"].replace("|", "\\|").replace("\n", " ")
        link = f"[日志]({check['log']})；" if check.get("log") and (output / check["log"]).exists() else ""
        lines.append(f"| {check['id']} | {'是' if check['required'] else '否'} | {check['status']} | {link}{reason} |")
    lines += ["", "## 验收项", ""]
    by_id = {c["id"]: c for c in report["checks"]}
    for criterion in task["acceptance"]:
        passed = all(by_id[c]["status"] == "PASS" for c in criterion["checks"])
        lines.append(f"- [{'x' if passed else ' '}] {criterion['id']}：{criterion['description']}（{', '.join(criterion['checks'])}）")
    lines += ["", "## 测试范围", ""]
    for kind, coverage in task["test_matrix"].items():
        detail = ", ".join(coverage["checks"]) if "checks" in coverage else "不适用：" + coverage["not_applicable"]
        lines.append(f"- {kind}：{detail}")
    lines += ["", "## 版本与工作区", ""]
    for repo, record in report.get("repositories", {}).items():
        before = record.get("before", {})
        lines.append(f"- {repo}：提交 `{before.get('head', 'unknown')}`；工作区{'有未提交改动' if before.get('dirty') else '干净'}；范围摘要 `{before.get('scope_sha256', 'unknown')}`。")
    lines += ["", "## 限制与交付说明", "", task["delivery"].get("notes", "")]
    lines.extend(f"- {error}" for error in report.get("errors", []))
    (output / "delivery.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


def execute(args):
    task = read_json(args.task)
    catalog = read_json(args.catalog)
    validate(task, catalog)
    config = configuration(args.repos)
    ids = {c["id"] for c in task["checks"]}
    chosen = set(args.checks) if args.checks is not None else ids
    require(chosen <= ids, "Requested checks are not in this task")
    output = Path(args.output).resolve() if args.output else ROOT / "build/ai-pipeline" / f"{task['id']}-{datetime.now(timezone.utc):%Y%m%dT%H%M%S}-{uuid.uuid4().hex[:8]}"
    roots = {repo: Path(config["repositories"][repo]).resolve() for repo in task["repositories"]}
    values = {"python": sys.executable, "run_dir": str(output)}
    values.update({f"repo.{k}": str(v) for k, v in roots.items()})
    values.update({f"tool.{k}": v for k, v in config["tools"].items()})
    # 在任何执行前核对全部参数，包括此次筛选之外的检查。
    for selection in task["checks"]:
        definition = catalog["checks"][selection["id"]]
        for value in definition["argv"] + list(definition.get("env", {}).values()):
            expand(value, values)
        verifier = definition["verifier"]
        for value in verifier.get("paths", [verifier["path"]] if "path" in verifier else []):
            expand(value, values)
    require(not output.exists(), f"Output directory already exists: {output}")
    output.mkdir(parents=True, exist_ok=False)
    (output / "logs").mkdir()
    write_json(output / "task.json", task)
    write_json(output / "catalog.json", {"schema_version": 1, "checks": {key: catalog["checks"][key] for key in sorted(ids)}})
    report = {"schema_version": 1, "mode": args.mode, "task_id": task["id"], "status": "NOT_RUN",
              "delivery_eligible": False, "started_at": utc_now(), "repositories": {}, "errors": [],
              "task_sha256": digest(output / "task.json"), "catalog_sha256": digest(output / "catalog.json"),
              "runner_sha256": digest(__file__), "checks": [
                  {"id": c["id"], "required": c["required"], "status": "NOT_RUN", "reason": "Not executed",
                   "exit_code": None, "elapsed_seconds": 0, "log": None} for c in task["checks"]]}
    for result in report["checks"]:
        definition = catalog["checks"][result["id"]]
        result["argv"] = [expand(value, values) for value in definition["argv"]]
        result["cwd"] = str(inside(roots[definition["repository"]], definition.get("cwd", ".")))
    save_report(output, report, task)
    try:
        with repository_locks(roots.values(), task["id"]):
            scopes = {repo: list(task["scope"][repo]) for repo in roots}
            for selection in task["checks"]:
                definition = catalog["checks"][selection["id"]]
                scopes[definition["repository"]].extend(definition.get("inputs", []))
            for repo, root in roots.items():
                report["repositories"][repo] = {"before": snapshot(root, sorted(set(scopes[repo])))}
            halted = False
            for selection, result in zip(task["checks"], report["checks"]):
                if args.mode == "plan":
                    result["reason"] = "Plan only; command was not executed"
                    print(f"[pipeline] PLAN {result['id']} cwd={result['cwd']} argv={json.dumps(result['argv'], ensure_ascii=False)}", flush=True)
                elif result["id"] not in chosen:
                    result["reason"] = "Not selected in this run"
                elif halted:
                    result["reason"] = "Stopped after an earlier unsuccessful check"
                elif selection.get("skip_reason"):
                    result.update(status="SKIP", reason=selection["skip_reason"])
                    halted = selection["required"]
                else:
                    print(f"[pipeline] Running {result['id']}", flush=True)
                    outcome = run_check(result["id"], catalog["checks"][result["id"]], roots, values, output)
                    result.update(outcome)
                    halted = result["status"] == "FAIL" or (result["required"] and result["status"] != "PASS")
                    print(f"[pipeline] {result['id']}: {result['status']} ({result['reason']})", flush=True)
                save_report(output, report, task)
            for repo, root in roots.items():
                record = report["repositories"][repo]
                record["after"] = snapshot(root, sorted(set(scopes[repo])))
                if any(record["before"][key] != record["after"][key] for key in ("head", "scope_sha256")):
                    report["errors"].append(f"Source changed during verification: {repo}")
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        report["errors"].append(str(error))
    except KeyboardInterrupt:
        report["errors"].append("Interrupted by user")
    if args.mode == "plan":
        report["status"] = "NOT_RUN"
    elif any(c["status"] == "FAIL" for c in report["checks"]):
        report["status"] = "FAIL"
    elif report["errors"] or any(c["required"] and c["status"] != "PASS" for c in report["checks"]):
        report["status"] = "INCOMPLETE"
    else:
        report["status"] = "PASS"
        report["delivery_eligible"] = True
    report["finished_at"] = utc_now()
    report["artifacts"] = [{"path": str(path.relative_to(output)), "size_bytes": path.stat().st_size,
                            "sha256": digest(path)} for path in sorted(output.rglob("*"))
                           if path.is_file() and path.name not in {"report.json", "delivery.md", "checksums.json"}]
    save_report(output, report, task)
    write_json(output / "checksums.json", {str(path.relative_to(output)): digest(path)
               for path in sorted(output.rglob("*")) if path.is_file() and path.name != "checksums.json"})
    print(f"[pipeline] status={report['status']} report={output / 'report.json'}", flush=True)
    if args.mode == "plan":
        return 2 if report["errors"] else 0
    return 0 if report["status"] == "PASS" else 1 if report["status"] == "FAIL" else 2


def main():
    parser = argparse.ArgumentParser(description="Run task checks and produce traceable development delivery evidence.")
    parser.add_argument("mode", choices=["plan", "run"])
    parser.add_argument("--task", required=True)
    parser.add_argument("--catalog", default=str(Path(__file__).with_name("checks.json")))
    parser.add_argument("--repos", help="Optional local repository/tool path config")
    parser.add_argument("--output", help="New evidence directory; existing directories are refused")
    parser.add_argument("--checks", nargs="+", help="Subset to execute; omitted required checks remain NOT_RUN")
    args = parser.parse_args()
    try:
        return execute(args)
    except (OSError, ValueError, TypeError, KeyError, re.error, subprocess.SubprocessError) as error:
        print(f"[pipeline] ERROR: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
