"""M5 API-to-real-probe acceptance on a disposable localhost MariaDB *_smoke DB.

Uses only local targets and webhook recipients. Retains private logs/identity
under --output and a redacted report; always terminates its own processes.
"""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from probe_runtime_smoke import PartitionRelay


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app-binary", required=True, type=Path)
    parser.add_argument("--probe-binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--port", type=int, default=39180)
    args = parser.parse_args()
    dsn = os.environ.get("DB_DSN", "")
    if not re.fullmatch(r"[^@]+@tcp\(127\.0\.0\.1:\d+\)/[a-zA-Z0-9_]+_smoke\?.+", dsn):
        parser.error("DB_DSN must name a disposable localhost database ending in _smoke")
    output = args.output.resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    key = output / "hub.key"
    key.write_bytes(secrets.token_bytes(32)); key.chmod(0o600)
    policy = output / "endpoints.json"
    policy.write_text('{"allowed_cidrs":["127.0.0.1/32"]}')
    password, token = secrets.token_urlsafe(24), ""
    env = dict(os.environ, DB_ENGINE="mariadb", DB_DSN=dsn, HOST="127.0.0.1", PORT=str(args.port), MODE="all",
               BOOTSTRAP_USERNAME="m5-smoke", BOOTSTRAP_PASSWORD=password, JWT_SECRET=secrets.token_urlsafe(40),
               PROBES_ENABLED="true", PROBE_SECRET_KEY_FILE=str(key), PROBE_ENDPOINT_POLICY_FILE=str(policy),
               PROBE_HUB_ID="", REDIS_URL="", OIDC_ISSUER="", PUBLIC_URL="", PRODUCTION="false", SHARD_POLL_EVERY="1")
    processes, logs, hooks, passed = [], [], [], []
    target_status = 200

    class Target(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(target_status); self.end_headers(); self.wfile.write(b"local fixture")
        def do_POST(self):
            hooks.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
            self.send_response(200); self.end_headers()
        def log_message(self, *_):
            pass

    target = ThreadingHTTPServer(("127.0.0.1", args.port+1), Target)
    relay = PartitionRelay(("127.0.0.1", args.port+2), ("127.0.0.1", args.port+3))
    for server in (target, relay):
        threading.Thread(target=server.serve_forever, daemon=True).start()

    def start(name, binary, arguments, process_env):
        log = (output / (name+".log")).open("ab"); logs.append(log)
        processes.append(subprocess.Popen([str(binary.resolve()), *arguments], env=process_env, stdout=log, stderr=subprocess.STDOUT))

    def api(method, path, body=None):
        headers = {"Content-Type":"application/json"}
        if token: headers["Authorization"]="Bearer "+token
        req=urllib.request.Request(f"http://127.0.0.1:{args.port}"+path, method=method, headers=headers,
                                   data=json.dumps(body).encode() if body is not None else None)
        with urllib.request.urlopen(req, timeout=15) as response:
            data=response.read(); return json.loads(data) if data else None

    def wait(label, predicate, timeout=45):
        deadline=time.monotonic()+timeout
        while time.monotonic()<deadline:
            try:
                result=predicate()
                if result:
                    passed.append(label); print(label, flush=True); return result
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                pass
            time.sleep(.5)
        raise AssertionError("timeout: "+label)

    try:
        edge_dir=output/"edge"
        init=subprocess.run([str(args.probe_binary.resolve()),"init","--data-dir",str(edge_dir)],
                            env=dict(env,PROBE_SECRET_KEY_FILE=""), capture_output=True, check=True, timeout=20)
        identity=json.loads(init.stdout)
        enrollment_token=identity.pop("enrollment_token")
        start("edge",args.probe_binary,["run","--data-dir",str(edge_dir),"--listen",f"127.0.0.1:{args.port+3}"],dict(env,PROBE_SECRET_KEY_FILE=""))
        start("hub",args.app_binary,[],env)
        wait("hub ready",lambda: api("GET","/api/health/ready")["status"]=="ready")
        token=api("POST","/api/auth/login",{"username":"m5-smoke","password":password})["token"]
        assert api("GET","/api/monitors")==[], "requires a fresh disposable database"
        registered=api("POST","/api/probes",{"probe_id":identity["probe_id"],"key":"m5-vm","name":"M5 VM","location":"Local acceptance",
                       "endpoint":f"127.0.0.1:{args.port+2}","tls_fingerprint":identity["certificate_fingerprint"]})
        probe_id=registered["id"]
        assert probe_id==identity["probe_id"]
        enrollment=api("POST",f"/api/probes/{probe_id}/enroll",{"enrollment_token":enrollment_token,"stream_id":identity["stream_id"]})
        enrollment_token=""
        assert enrollment["status"]=="succeeded", enrollment
        assert api("GET","/api/probe-operations/"+enrollment["operation_id"])==enrollment
        wait("real API enrollment activates pinned TLS runtime",lambda: api("GET",f"/api/probes/{probe_id}")["enrollment_state"]=="active")
        sink=f"http://127.0.0.1:{args.port+1}"
        monitor=api("POST","/api/monitors",{"name":"M5 target","type":"http","active":True,"interval":1,"retry_interval":1,
                    "max_retries":1,"timeout":2,"resend_interval":0,"config":{"url":sink+"/target"}})["id"]
        notification=api("POST","/api/notifications",{"name":"M5 local sink","type":"webhook","active":True,"config":{"url":sink+"/hook"}})["id"]
        api("POST",f"/api/notifications/{notification}/monitor/{monitor}",{"include_target":False})
        current=api("GET",f"/api/monitors/{monitor}/probes")
        assigned=api("PUT",f"/api/monitors/{monitor}/probes",{"expected_revision":current["revision"],"probe_ids":["local",probe_id],"health_policy":"any_down","alert_delivery":"regional"})
        def health(): return api("GET",f"/api/monitors/{monitor}/health")
        wait("two live regional streams are UP",lambda: len(health()["regions"])==2 and all(row["status"]=="up" for row in health()["regions"]),90)
        for region in ("local",probe_id):
            assert api("GET",f"/api/monitors/{monitor}/probes/{region}/heartbeats?hours=1")
        def applied():
            rows=api("GET",f"/api/monitors/{monitor}/probes")["assignments"]
            return next(row for row in rows if row["probe_id"]==probe_id)["sync_status"]=="applied"
        wait("desired configuration has a matching source receipt",applied)
        target_status=503
        incident=wait("remote incident reaches attributed API",lambda: next((row for row in api("GET",f"/api/monitors/{monitor}/probe-alerts") if row["probe_id"]==probe_id and row["status"]=="firing"),None),90)
        relay.partition(True)
        disconnected=time.monotonic()
        api("PUT",f"/api/monitors/{monitor}",{"name":"M5 edited while offline"})
        command_id=str(uuid.uuid4())
        ack_path=f"/api/monitors/{monitor}/probe-alerts/{incident['source_alert_id']}/ack"
        queued=api("POST",ack_path,{"command_id":command_id,"note":"local acceptance"})
        assert queued=={"command_id":command_id,"status":"pending","remote_confirmed":False}
        assert api("POST",ack_path,{"command_id":command_id,"note":"local acceptance"})==queued
        wait("offline desired configuration stays pending",lambda: not applied())
        unknown=wait("disconnected regional evidence expires to UNKNOWN",lambda: next((row for row in health()["regions"] if row["probe_id"]==probe_id and row["status"]=="unknown"),None),125)
        assert api("GET",ack_path+"/"+command_id)==queued
        partition_seconds=time.monotonic()-disconnected
        relay.partition(False)
        wait("reconnect applies pending configuration",applied,100)
        confirmed=wait("reconnect confirms the source ACK",lambda: (row if (row:=api("GET",ack_path+"/"+command_id))["remote_confirmed"] else None),100)
        assert confirmed["status"]=="applied"
        wait("mirrored incident retains source ACK identity",lambda: next((row for row in api("GET",f"/api/monitors/{monitor}/probe-alerts") if row["source_alert_id"]==incident["source_alert_id"] and row["status"]=="acked"),None))
        target_status=200
        wait("both regions recover",lambda: all(row["status"]=="up" for row in health()["regions"]),90)
        time.sleep(3)
        incidents=api("GET",f"/api/monitors/{monitor}/probe-alerts")
        assert len(incidents)==1 and incidents[0]["source_alert_id"]==incident["source_alert_id"], incidents
        remote_down=[h for h in hooks if h.get("source_alert_id")==incident["source_alert_id"] and h.get("status")==0]
        assert len(remote_down)==1, "duplicate or missing remote outage delivery"
        report={"passed":passed,"probe_id":probe_id,"monitor_id":monitor,"assignment_revision":assigned["revision"],
                "partition_seconds":partition_seconds,"stale_reason":unknown["reason"],"receipt":confirmed,"remote_incidents":len(incidents),"remote_down_deliveries":len(remote_down)}
        (output/"report.json").write_text(json.dumps(report,indent=2)+"\n")
    finally:
        (output/"webhooks.json").write_text(json.dumps(hooks,indent=2)+"\n")
        for process in reversed(processes):
            process.terminate()
            try: process.wait(timeout=25)
            except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)
        for log in logs: log.close()
        for server in (target,relay): server.shutdown(); server.server_close()


if __name__=="__main__":
    main()
