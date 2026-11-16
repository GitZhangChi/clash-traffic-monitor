#!/usr/bin/env python3
"""Start the home monitor with live router API settings, without printing secrets."""
import argparse
import json
import os
import re
import subprocess
from pathlib import Path
from urllib.parse import urlsplit

ROUTER_SETTINGS = """import json,os
with open('/etc/sing-box/config.json') as f:
 config=json.load(f)
api=config['experimental']['clash_api']
print(json.dumps({'hostname':os.uname().nodename,'controller':api['external_controller'],'secret':api.get('secret','')}))
"""


def router_settings(config):
    target = config.get("routerSSH", "")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]*", target):
        raise ValueError("routerSSH must be a configured SSH host alias")
    remote = "python3 -c '" + ROUTER_SETTINGS.replace("'", "'\"'\"'") + "'"
    result = subprocess.run(
        ["/usr/bin/ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1", target, remote],
        capture_output=True, text=True, timeout=20, check=True,
    )
    settings = json.loads(result.stdout)
    if not config.get("expectedHostname") or settings["hostname"] != config["expectedHostname"]:
        raise ValueError("router hostname does not match expectedHostname")
    if not settings.get("controller"):
        raise ValueError("router Clash API is not configured")
    return settings


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--runtime-dir", type=Path, required=True)
    parser.add_argument("--listen", default="127.0.0.1:18185")
    args = parser.parse_args()
    os.umask(0o077)
    config_path, binary = args.config.resolve(), args.binary.resolve()
    config = json.loads(config_path.read_text())
    settings = router_settings(config)
    args.runtime_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    args.runtime_dir.chmod(0o700)
    api_url = "http://" + settings["controller"]
    env = os.environ.copy()
    no_proxy = ",".join(filter(None, [env.get("NO_PROXY", ""), "127.0.0.1,localhost", urlsplit(api_url).hostname]))
    env.update({
        "MIHOMO_URL": api_url,
        "MIHOMO_SECRET": settings.get("secret", ""),
        "TRAFFIC_MONITOR_HOME_CONFIG": str(config_path),
        "TRAFFIC_MONITOR_LISTEN": args.listen,
        "NO_PROXY": no_proxy,
        "no_proxy": no_proxy,
    })
    os.chdir(args.runtime_dir)
    # exec preserves the launchd PID and lets SIGTERM reach the Go process.
    os.execve(binary, [str(binary)], env)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Do not include captured router output, URLs with credentials, or env.
        raise SystemExit(f"Home monitor startup failed ({type(error).__name__}); check router connectivity and configuration.")
