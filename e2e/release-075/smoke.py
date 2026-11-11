"""Opt-in real-image smoke test; never uses the operator's CLI home.

Requires Python 3, Docker and a built CLI. Leaves the test deployment running
for browser checks; the printed home belongs only to this test. See README.
"""

import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--workdir", required=True, type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    work = args.workdir.resolve()
    if work.exists():
        raise RuntimeError("workdir must not exist; refusing to reuse any CLI state")

    def docker(*command):
        return subprocess.check_output(["docker", *command], text=True, stderr=subprocess.STDOUT, timeout=60).strip()

    # Fixed product names cannot be isolated with COMPOSE_PROJECT_NAME alone.
    containers = docker("ps", "-a", "--format", "{{.Names}}").splitlines()
    if any(name.startswith("versola-") for name in containers):
        raise RuntimeError("existing Versola containers: use an empty Docker daemon")
    volumes = docker("volume", "ls", "--format", "{{.Name}}").splitlines()
    if any(name == "versola-openbao-file-local" or name.startswith("versola-local_") for name in volumes):
        raise RuntimeError("existing local Versola volumes: use an empty Docker daemon")
    networks = docker("network", "ls", "--format", "{{.Name}}").splitlines()
    if any(name.startswith("versola-local") for name in networks):
        raise RuntimeError("existing local Versola network: use an empty Docker daemon")
    for port in (2821, 8200, 8081, 8091, 8096):
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", port))

    work.mkdir(parents=True, mode=0o700)
    home = work / "home"
    home.mkdir(mode=0o700)
    env = os.environ.copy()
    original_home = Path.home()
    env["DOCKER_CONFIG"] = env.get("DOCKER_CONFIG", str(original_home / ".docker"))
    env["HOME"] = str(home)
    env["USERPROFILE"] = str(home)
    env.pop("COMPOSE_PROJECT_NAME", None)
    env.pop("COMPOSE_FILE", None)

    def cli(label, *command):
        with (work / (label + ".log")).open("w", encoding="utf-8") as log:
            result = subprocess.run([str(binary), *command], env=env, cwd=work,
                                    stdin=subprocess.DEVNULL, stdout=log,
                                    stderr=subprocess.STDOUT, timeout=1200)
        if result.returncode:
            raise RuntimeError(f"{label}: exit {result.returncode}; inspect the private test log locally")
        print("PASS", label, flush=True)

    def request(path, status=200, headers=None):
        req = urllib.request.Request("http://localhost:2821" + path, headers=headers or {})
        try:
            response = urllib.request.urlopen(req, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            assert response.status == status, (path, response.status, status)
            return response.headers, response.read()

    cli("doctor", "doctor")
    cli("bootstrap", "bootstrap", "local", "0.7.5", "--no-browser")
    cli("status", "status")
    _, discovery = request("/.well-known/openid-configuration")
    assert json.loads(discovery)["issuer"].startswith("http://localhost:2821")
    request("/permissions/me", 401)
    for path, status in (("/central/admin/", 200), ("/central/admin/release-test-missing.js", 404)):
        headers, _ = request(path, status, {"X-Forwarded-Proto": "https"})
        for key, expected in {
            "X-Content-Type-Options": "nosniff",
            "Referrer-Policy": "no-referrer",
            "X-Frame-Options": "DENY",
            "Content-Security-Policy": "frame-ancestors 'none'; base-uri 'none'; object-src 'none'",
        }.items():
            assert headers.get(key) == expected, (path, key)
        assert headers.get("Strict-Transport-Security") is None
    marker = "release075-access-log-probe"
    canary = "release075-query-must-not-appear"
    request(f"/central/admin/{marker}.js?code={canary}", 404,
            {"Referer": f"http://localhost:2821/central/admin/?link_token={canary}"})
    deadline = time.monotonic() + 10
    while True:
        logs = docker("logs", "versola-proxy-local")
        if marker in logs:
            assert canary not in logs, "query or Referer query leaked into proxy logs"
            break
        if time.monotonic() >= deadline:
            raise RuntimeError("proxy access-log marker never appeared")
        time.sleep(0.2)
    print("PASS discovery, unauthorized API, console headers (200/404), query-free logs", flush=True)
    docker("exec", "versola-postgres", "psql", "-U", "versola", "-d", "auth", "-v", "ON_ERROR_STOP=1", "-c",
           "CREATE TABLE public.cli_release_probe (id integer PRIMARY KEY); INSERT INTO public.cli_release_probe VALUES (735);")
    before = docker("inspect", "versola-postgres", "--format", "{{.Id}}")
    cli("down", "down")
    assert docker("inspect", "versola-postgres", "--format", "{{.State.Status}}") == "exited"
    cli("up", "up", "--no-browser")
    request("/.well-known/openid-configuration")
    request("/central/admin/")
    assert docker("inspect", "versola-postgres", "--format", "{{.Id}}") == before
    assert docker("exec", "versola-postgres", "psql", "-U", "versola", "-d", "auth", "-Atc",
                  "SELECT id FROM public.cli_release_probe") == "735"
    cli("migrate-again", "migrate")
    print("PASS stop/start and idempotent migrations", flush=True)
    (work / "result.json").write_text(json.dumps({"versola": "0.7.5", "passed": True,
        "home": str(home), "baselineVolumes": volumes}, indent=2), encoding="utf-8")
    print("Test deployment retained for browser checks. Test home:", home, flush=True)


if __name__ == "__main__":
    main()
