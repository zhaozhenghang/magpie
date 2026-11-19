#!/usr/bin/env python3
"""Install and update the macOS Weekly Balance build."""

import argparse
import contextlib
import ctypes
import fcntl
import hashlib
import http.server
import json
import os
import plistlib
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from dataclasses import dataclass
from pathlib import Path

REPO = "zhaozhenghang/magpie"
WORKFLOW = f"{REPO}/.github/workflows/weekly-balance-release.yml"
APP = "magpie.app"
BINARY = Path("Contents/MacOS/magpie")
ASSET = "magpie-darwin-arm64.zip"
PLUGIN = "weekly-balance.gateway.js"
STRATEGY = "weekly-balance/weekly-balance"
APP_ID = "io.github.zhaozhenghang.magpie.weekly-balance"
OFFICIAL_ID = "com.yetone.magpie"
VERSION = re.compile(r"(\d+)\.(\d+)\.(\d+)-weekly\.([1-9]\d*)")


@dataclass(frozen=True)
class Installation:
    root: Path
    config: Path
    state: Path
    in_place: bool

    @property
    def app(self):
        return self.root / APP

    @property
    def plugin(self):
        return self.state / PLUGIN


def run(*args, capture=False, env=None, timeout=120):
    result = subprocess.run(args, check=True, text=True, capture_output=capture,
                            env=env, timeout=timeout)
    return result.stdout if capture else None


def version_key(version):
    match = VERSION.fullmatch(version)
    if not match:
        raise ValueError(f"Not a Weekly Balance version: {version}")
    return tuple(map(int, match.groups()))


def latest_version():
    result = run("gh", "release", "list", "-R", REPO, "--exclude-drafts",
                 "--exclude-pre-releases", "--limit", "100", "--json", "tagName", capture=True)
    versions = [row["tagName"][6:] for row in json.loads(result)
                if row["tagName"].startswith("fork-v") and VERSION.fullmatch(row["tagName"][6:])]
    if not versions:
        raise RuntimeError("No published Weekly Balance release")
    return max(versions, key=version_key)


def download(version, staging):
    run("gh", "release", "download", f"fork-v{version}", "-R", REPO,
        "--pattern", ASSET, "--pattern", PLUGIN, "--dir", str(staging), timeout=300)
    for name in (ASSET, PLUGIN):
        run("gh", "attestation", "verify", str(staging / name), "--repo", REPO,
            "--signer-workflow", WORKFLOW, "--source-ref", "refs/heads/weekly-balance",
            "--deny-self-hosted-runners")


def bundle_info(app):
    if app.is_symlink():
        raise RuntimeError("Refusing to replace an app symlink")
    return plistlib.loads((app / "Contents/Info.plist").read_bytes())


def app_version(app):
    info = bundle_info(app)
    if info["CFBundleIdentifier"] != APP_ID:
        raise RuntimeError("This app is outside the Weekly Balance update channel")
    version = info["MagpieWeeklyBalanceVersion"]
    version_key(version)
    return version


def check_bundle(app, version):
    run("/usr/bin/codesign", "--verify", "--deep", "--strict", str(app))
    if app_version(app) != version or run(str(app / BINARY), "--version", capture=True).strip() != f"magpie {version}":
        raise RuntimeError("Bundle and executable versions do not match the release")


def check_plugin(app, plugin):
    result = run(str(app / BINARY), "plugin", "list", "--json", capture=True)
    listing = json.loads(result)
    state = listing.get("middleware", {}).get(str(plugin)) if isinstance(listing, dict) else None
    if not state or state.get("error") or STRATEGY not in state.get("strategies", []):
        raise RuntimeError("The Weekly Balance plugin did not load")


def status(root, address):
    raw = run(str(root / APP / BINARY), "status", "--json", "--addr", address, capture=True, timeout=12)
    return json.loads(raw)


def wait_for_version(root, address, version, timeout):
    until = time.monotonic() + timeout
    while time.monotonic() < until:
        if status(root, address).get("version") == version:
            return
        time.sleep(2)
    raise RuntimeError(f"Gateway did not return healthy on {version}")


def http_local():
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


def gateway_idle(address):
    with http_local().open(f"http://{address}/v1/magpie/concurrency", timeout=5) as response:
        load = json.load(response)
    return load.get("object") == "concurrency" and all(
        load.get(key) == 0 for key in ("requests", "tools", "waiting"))


def restart(root, address, expected, target):
    running = status(root, address)
    if not running["running"]:
        run("/usr/bin/open", "-a", str(root / APP), "--args", "tray")
    elif running["version"] == expected:
        request = urllib.request.Request(f"http://{address}/v1/magpie/restart",
                                         data=json.dumps({"version": target}).encode(),
                                         headers={"Content-Type": "application/json"})
        with http_local().open(request, timeout=12) as response:
            if json.load(response)["state"] not in ("waiting", "restarting", "current"):
                raise RuntimeError("Gateway did not accept a graceful restart")


def exchange(a, b):
    # renamex_np with RENAME_SWAP keeps one valid app at the installed path.
    rename = ctypes.CDLL(None, use_errno=True).renamex_np
    if rename(os.fsencode(a), os.fsencode(b), 2) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))


def unpack(staging, version):
    run("/usr/bin/ditto", "-x", "-k", str(staging / ASSET), str(staging / "unpacked"))
    app = staging / "unpacked" / APP
    check_bundle(app, version)
    return app


def write_atomic(path, data):
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as file:
        pending = Path(file.name)
        try:
            file.write(data)
            file.close()
            pending.replace(path)
        finally:
            pending.unlink(missing_ok=True)


@contextlib.contextmanager
def verified_feed(staging, version):
    archive = staging / ASSET
    with archive.open("rb") as file:
        digest = hashlib.sha256()
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)

    class Feed(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/feed" or self.path.startswith("/feed?"):
                body = json.dumps({
                    "version": version,
                    "url": f"https://github.com/{REPO}/releases/tag/fork-v{version}",
                    "assets": {ASSET: {"url": f"{origin}/{ASSET}", "size": archive.stat().st_size,
                                      "sha256": digest.hexdigest()}},
                }).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            elif self.path == f"/{ASSET}":
                self.send_response(200)
                self.send_header("Content-Length", str(archive.stat().st_size))
                self.end_headers()
                with archive.open("rb") as file:
                    shutil.copyfileobj(file, self.wfile)
            else:
                self.send_error(404)

    with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Feed) as server:
        origin = f"http://127.0.0.1:{server.server_port}"
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield f"{origin}/feed"
        finally:
            server.shutdown()
            thread.join()


def install(root, address, staging, version):
    app = unpack(staging, version)
    (root / ".portable").touch(exist_ok=True)
    app.rename(root / APP)
    data = root / "data"
    created_data = False
    try:
        data.mkdir(mode=0o700)
        created_data = True
        (data / "settings.json").write_text(json.dumps({
            "port": int(address.split(":")[1]),
            "noAutoUpdate": True,
        }) + "\n")
        (staging / PLUGIN).rename(root / PLUGIN)
        binary = str(root / APP / BINARY)
        run(binary, "plugin", "add", str(root / PLUGIN))
        check_plugin(root / APP, root / PLUGIN)
    except Exception:
        shutil.rmtree(root / APP)
        (root / PLUGIN).unlink(missing_ok=True)
        if created_data:
            shutil.rmtree(data)
        raise
    print(f"Installed {version} in {root}")


def check_official(layout, address):
    if bundle_info(layout.app)["CFBundleIdentifier"] != OFFICIAL_ID:
        raise RuntimeError("Expected the installed official Magpie app")
    installed = run(str(layout.app / BINARY), "--version", capture=True).strip().removeprefix("magpie ")
    if not re.fullmatch(r"\d+\.\d+\.\d+", installed):
        raise RuntimeError("Expected a released official Magpie version")
    live = status(layout.root, address)
    if live["running"]:
        if live["version"] != installed or live["role"] not in ("app", "tray"):
            raise RuntimeError("The selected gateway is not the official app")
        exe = run("/bin/ps", "-p", str(live["pid"]), "-o", "comm=", capture=True).strip()
        if Path(exe).resolve() != layout.app / BINARY:
            raise RuntimeError("The selected gateway is running from a different path")
        if not gateway_idle(address):
            raise RuntimeError("The Magpie gateway is busy. Wait for its requests to finish and retry.")
    return installed


def install_in_place(layout, address, staging, version):
    official = check_official(layout, address)
    if version_key(version)[:3] < tuple(map(int, official.split("."))):
        raise RuntimeError("The Weekly Balance release is based on an older official version")
    app = unpack(staging, version)
    original = layout.state / "official" / APP
    if not original.exists():
        saved = staging / "official-backup.app"
        run("/usr/bin/ditto", str(layout.app), str(saved))
        original.parent.mkdir(exist_ok=True)
        saved.rename(original)

    snapshots = {}
    for name in ("settings.json", "plugins.json"):
        path = layout.config / name
        snapshots[path] = path.read_bytes() if path.exists() else None
    settings = layout.config / "settings.json"
    config = json.loads(snapshots[settings])
    if layout.plugin.exists():
        raise RuntimeError("An existing Weekly Balance plugin already occupies the install path")

    with tempfile.TemporaryDirectory(prefix=".magpie-weekly-", dir=layout.root) as folder:
        staged_app = Path(folder) / APP
        run("/usr/bin/ditto", str(app), str(staged_app))
        binary = staged_app / BINARY
        switched = False
        try:
            (staging / PLUGIN).rename(layout.plugin)
            run(str(binary), "plugin", "add", str(layout.plugin), capture=True)
            check_plugin(staged_app, layout.plugin)
            check_official(layout, address)
            config["noAutoUpdate"] = True
            write_atomic(settings, (json.dumps(config, ensure_ascii=False, indent=2) + "\n").encode())
            exchange(layout.app, staged_app)
            switched = True
            restart(layout.root, address, official, version)
            wait_for_version(layout.root, address, version, timeout=180)
            check_plugin(layout.app, layout.plugin)
        except Exception:
            if switched:
                exchange(layout.app, staged_app)
            for path, data in snapshots.items():
                if data is None:
                    path.unlink(missing_ok=True)
                else:
                    write_atomic(path, data)
            layout.plugin.unlink(missing_ok=True)
            if switched:
                restart(layout.root, address, version, official)
                wait_for_version(layout.root, address, official, timeout=180)
            raise
    print(f"Installed {version} in place at {layout.app}")


def rollback(layout, address, previous):
    app = layout.app
    old_app = previous / APP
    current, old = app_version(app), app_version(old_app)
    if current == old:
        raise RuntimeError("No older build available")
    check_bundle(old_app, old)
    live = status(layout.root, address)
    if live["running"] and live["version"] not in (current, old):
        raise RuntimeError("The selected gateway belongs to another copy")
    exchange(layout.plugin, previous / PLUGIN)
    try:
        exchange(app, old_app)
    except OSError:
        exchange(layout.plugin, previous / PLUGIN)
        raise
    restart(layout.root, address, current, old)
    wait_for_version(layout.root, address, old, timeout=660)
    check_plugin(app, layout.plugin)
    (layout.state / "failed-version").write_text(current + "\n")
    print(f"Restored {old}")


def update(layout, address, staging, current, version):
    app, plugin = layout.app, layout.plugin
    live = status(layout.root, address)
    if not live["running"]:
        print("Weekly Balance is closed. Will update when its gateway is running.")
        return
    if live["version"] != current or live["role"] not in ("app", "tray"):
        raise RuntimeError("The selected gateway is not the running Weekly Balance app")
    if not gateway_idle(address):
        print("The gateway has active requests or turns. Will retry on the next check.")
        return
    config = json.loads((layout.config / "settings.json").read_text())
    if not config.get("noAutoUpdate"):
        raise RuntimeError("Turn off the official updater in this Weekly Balance installation first")
    previous = layout.state / "previous"
    backup = staging / "previous"
    backup.mkdir(mode=0o700)
    run("/usr/bin/ditto", str(app), str(backup / APP))
    shutil.copy2(plugin, backup / PLUGIN)
    if previous.exists():
        exchange(backup, previous)
    else:
        backup.rename(previous)

    try:
        with verified_feed(staging, version) as feed:
            env = dict(os.environ, MAGPIE_UPDATE_FEED=feed)
            run(str(app / BINARY), "update", "--addr", address, "--proxy", "direct",
                "--mirror", "off", env=env, timeout=300)
        check_bundle(app, version)
        wait_for_version(layout.root, address, version, timeout=660)
        (staging / PLUGIN).replace(plugin)
        check_plugin(app, plugin)
    except Exception:
        (layout.state / "failed-version").write_text(version + "\n")
        if app_version(app) != current:
            # Keep the rejected pair together even if the app failed before the JS swap.
            if (staging / PLUGIN).exists():
                (staging / PLUGIN).replace(plugin)
            rollback(layout, address, previous)
        raise
    print(f"Updated {current} → {version}")


def launch_agent(layout, address):
    app_version(layout.app)
    label = APP_ID + ".update"
    folder = Path.home() / "Library/LaunchAgents"
    folder.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = folder / f"{label}.plist"
    command = [sys.executable, str(Path(__file__).resolve()), "update",
               "--root", str(layout.root), "--addr", address]
    environment = {"PATH": os.environ["PATH"], "GH_PROMPT_DISABLED": "1"}
    if layout.in_place:
        script = layout.state / "update.py"
        command[1] = str(script)
        command.append("--in-place")
        environment["XDG_CONFIG_HOME"] = str(layout.config.parent)
    spec = {
        "Label": label,
        "ProgramArguments": command,
        "StartInterval": 6 * 60 * 60,
        "EnvironmentVariables": environment,
        "StandardOutPath": str(layout.state / "updater.log"),
        "StandardErrorPath": str(layout.state / "updater.log"),
    }
    existing = path.exists()
    if existing and plistlib.loads(path.read_bytes()) != spec:
        raise RuntimeError(f"A different Weekly Balance launch agent already exists at {path}")
    if layout.in_place:
        write_atomic(script, Path(__file__).read_bytes())
    service = f"gui/{os.getuid()}/{label}"
    if existing and subprocess.run(["/bin/launchctl", "print", service], capture_output=True).returncode == 0:
        print(f"Already scheduled {label}")
        return
    if not existing:
        with path.open("xb") as file:
            plistlib.dump(spec, file)
    try:
        run("/bin/launchctl", "bootstrap", f"gui/{os.getuid()}", str(path))
    except Exception:
        if not existing:
            path.unlink()
        raise
    print(f"Scheduled {label}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("install", "update", "rollback", "launch-agent"))
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--addr")
    parser.add_argument("--in-place", action="store_true", help="Reuse the installed app's user configuration")
    args = parser.parse_args()
    if args.addr is None:
        args.addr = "127.0.0.1:3425" if args.in_place else "127.0.0.1:13425"
    if not re.fullmatch(r"127\.0\.0\.1:[1-9][0-9]{0,4}", args.addr) or int(args.addr.split(":")[1]) > 65535:
        parser.error("--addr must be a loopback 127.0.0.1 host and port")
    if sys.platform != "darwin" or os.uname().machine != "arm64":
        parser.error("This build is for ARM64 macOS")
    os.umask(0o077)
    root = args.root.expanduser().resolve()
    if args.in_place:
        xdg = Path(os.environ.get("XDG_CONFIG_HOME", ""))
        config = (xdg if xdg.is_absolute() else Path.home() / ".config") / "magpie"
        if (root / ".portable").exists() or (root / "data").is_dir():
            raise RuntimeError("The app directory must use Magpie's installed configuration")
        layout = Installation(root, config, config / "weekly-balance", True)
        if not (layout.app / BINARY).is_file() or not (config / "settings.json").is_file():
            raise RuntimeError("The existing app and its user configuration are required")
    else:
        layout = Installation(root, root / "data", root, False)
    if args.action == "install" and not args.in_place:
        root.mkdir(mode=0o700, parents=True, exist_ok=True)
    elif not args.in_place and not (root / ".portable").is_file():
        raise RuntimeError("Choose the separate portable Weekly Balance install")
    layout.state.mkdir(mode=0o700, exist_ok=True)
    with (layout.state / ".update.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.action == "rollback":
            rollback(layout, args.addr, layout.state / "previous")
            return
        if args.action == "launch-agent":
            launch_agent(layout, args.addr)
            return
        if args.action == "install" and args.in_place:
            check_official(layout, args.addr)
            current = None
        else:
            current = app_version(layout.app) if layout.app.exists() else None
            if args.action == "install" and (current or layout.config.exists() or layout.plugin.exists()):
                raise RuntimeError("Choose a new install directory")
        if args.action == "update" and current is None:
            raise RuntimeError("Install the fork first")
        version = latest_version()
        failed = layout.state / "failed-version"
        if current and (version_key(version) <= version_key(current) or
                        failed.exists() and failed.read_text().strip() == version):
            print(f"No applicable update for {current}")
            return
        with tempfile.TemporaryDirectory(prefix=".update-", dir=layout.state) as folder:
            staging = Path(folder)
            download(version, staging)
            if current:
                update(layout, args.addr, staging, current, version)
            elif args.in_place:
                install_in_place(layout, args.addr, staging, version)
            else:
                install(root, args.addr, staging, version)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
