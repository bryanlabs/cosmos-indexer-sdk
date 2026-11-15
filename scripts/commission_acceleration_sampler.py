"""One global staged admission controller, adopted by the existing check.
Reads the actual PG cgroup quota on every sample. It never changes quota or old
actors. Lease loss cancels only new children; all partial attempts are retained.
"""
import argparse
import concurrent.futures
import datetime
import fcntl
import json
import pathlib
import subprocess
import time
import os
import hashlib
import re
from commission_health_protocol import adapt, policy_verified, GLOBAL_LOCK


def call(args, data=None):
    return subprocess.check_output(
        ["kubectl", "--context", "bryanlabs", "-n", "apps"] + args,
        input=data, stderr=subprocess.DEVNULL, timeout=3)


def cgroup(pod, pilot_terminal=False):
    script = ('for f in cpu.stat cpu.max memory.stat memory.current memory.max memory.events memory.pressure; '
              'do printf "FILE:%s\\n" "$f"; while IFS= read -r line; '
              'do printf "%s\\n" "$line"; done <"/sys/fs/cgroup/$f"; done')
    if pilot_terminal:
        script += ('; printf "FILE:pilot_terminal\\n"; done=0; '
                   'for f in /private/native.exit /private/native-attempt-*/native.exit; do '
                   'if test -f "$f"; then done=1; fi; done; printf "%s\\n" "$done"')
    text = call(["exec", pod, "--", "sh", "-c", script]).decode()
    return {x.split("\n", 1)[0]: x.split("\n", 1)[1]
            for x in text.split("FILE:")[1:]}


def pairs(text):
    return dict(line.split() for line in text.splitlines())


def live_policy():
    cm = json.loads(call(["get", "configmap", "cosmos-tax-quiet-hours", "-o", "json"]))
    cron = json.loads(call(["get", "cronjob", "cosmos-tax-quiet-hours", "-o", "json"]))
    return policy_verified(cm, cron)


def atomic_json(path, value):
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(value, indent=2)+"\n")
    temp.replace(path)


def health_admission_allowed(health, fresh):
    return (fresh and health.get("PublicHealthy") is True and health.get("ReplayHealthy") is True
            and health.get("NodeCPUHeadroom") is True and health.get("ComparableReplayWindow") is True
            and health.get("CPUWriterPolicyAuthorized") is True and health.get("ReplayBaselinePreserved") is True)


def record_order(record):
    job = record["job"]
    match = re.fullmatch(r"cosmos-tax-corrected-accel-(four|later)-s(\d{2})-20261115-v1", job)
    pilot = job == "cosmos-tax-queryfix-pilot-20261115-v1"
    if (not match and not pilot) or (match and not 1 <= int(match[2]) <= (7 if match[1]=="four" else 3)):
        raise ValueError("unapproved coordinator job name")
    priority = record.get("priority", 0 if pilot else 100)
    if type(priority) is not int:
        raise ValueError("invalid record priority")
    return (priority, int(match[2]) if match else 0, 0 if match and match[1]=="four" else 1, job)


def load_records(path):
    records = json.loads(pathlib.Path(path).read_text())["records"]
    if not records or len(records)>11 or len({r["job"] for r in records}) != len(records):
        raise ValueError("invalid global job inventory")
    for record in records:
        if record["job"] == "cosmos-tax-queryfix-pilot-20261115-v1":
            status = record.get("pilot_state", "ready")
            if status not in ("pending", "ready", "completed", "failed"):
                raise ValueError("invalid pilot state")
            if record.get("native_binary_sha256") != "8ab0b0a9cafdc564a3399c42e05805ba922409e59eb9226398e218f68f0e99a5":
                raise ValueError("unreviewed pilot native hash")
            if not str(record.get("image", "")).endswith("@sha256:2c9eaa53d21b104df307924c1d06275f5cf4535496ae71594fe7012090d75a55"):
                raise ValueError("unreviewed pilot image digest")
            required = ("job_uid", "native_binary_sha256", "image", "intent")
            if status == "ready":
                required += ("pod_uid",)
            for key in required:
                if not isinstance(record.get(key), str) or not record[key]:
                    raise ValueError("pilot registration requires exact identity: "+key)
    return sorted(records, key=record_order)


def quiet_pilot_reservation(records, now, completed):
    from commission_health_protocol import DAY_START
    return now < DAY_START and any(r["job"] == "cosmos-tax-queryfix-pilot-20261115-v1"
        and r.get("pilot_state", "ready") in ("pending", "ready") and r["job"] not in completed for r in records)


def scoped_pg_reserve(records, admitted, now, quota, completed):
    pilot = "cosmos-tax-queryfix-pilot-20261115-v1"
    if quota != 10 or not quiet_pilot_reservation(records, now, completed) or set(admitted)-{pilot}:
        return 2.1
    r = next(r for r in records if r["job"] == pilot)
    if (not r.get("job_uid") or not r.get("intent")
        or r.get("native_binary_sha256") != "8ab0b0a9cafdc564a3399c42e05805ba922409e59eb9226398e218f68f0e99a5"
        or not str(r.get("image", "")).endswith("@sha256:2c9eaa53d21b104df307924c1d06275f5cf4535496ae71594fe7012090d75a55")):
        return 2.1
    if admitted:
        return .1 if r.get("pilot_state") == "ready" and r.get("pod_uid") else 2.1
    return 1.1


class Admission:
    def __init__(self, maximum, stage_seconds):
        self.maximum = maximum
        self.stage_seconds = stage_seconds
        self.jobs = []
        self.completed = set()
        self.last_add = float("-inf")

    def mark_terminal(self, job):
        self.completed.add(job)
        self.jobs = [j for j in self.jobs if j != job]

    def update(self, eligible, now, quota, used, healthy):
        eligible = [j for j in eligible if j not in self.completed]
        self.jobs = [job for job in self.jobs if job in eligible]
        # One new slot at a time, only after the preceding stage has run.
        # Already admitted work is included in actual measured PG usage.
        if (healthy and quota > 0 and 0 <= used <= quota - 0.1
                and now - self.last_add >= self.stage_seconds
                and len(self.jobs) < self.maximum):
            next_job = next((j for j in eligible if j not in self.jobs), None)
            if next_job is not None:
                self.jobs.append(next_job)
                self.last_add = now
        return set(self.jobs)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--jobs", required=True)
    parser.add_argument("--health-receipt", required=True)
    parser.add_argument("--pg-pod", default="cosmos-tax-sdk-postgres-85c85559c-864tr")
    parser.add_argument("--protocol-check", action="store_true", help="one finite read-only protocol check, no permits")
    parser.add_argument("--max-admitted", type=int, default=10)
    parser.add_argument("--stage-seconds", type=float, default=60)
    args = parser.parse_args()
    records = load_records(args.jobs)
    order = [r["job"] for r in records]
    if not 1 <= args.max_admitted <= len(order) or args.stage_seconds < 60:
        raise ValueError("invalid staged admission budget")
    home = pathlib.Path("/Users/danb/.cache/corrected324-acceleration-20261115")
    base = json.loads(pathlib.Path("/Users/danb/.cache/commission-finish-20261114T2323/pg-stage-replay-before.safe.json").read_text())["after"]
    expected_actors = dict(base["actors"], postgres_uid=base["postgres"]["uid"])
    state = {}
    policy_at, policy_ok = 0, False
    identity_record = dict(pid=os.getpid(), birth_ticks=int(pathlib.Path("/proc/self/stat").read_text().split()[21]),
                           script_sha256=hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(), lock=GLOBAL_LOCK)
    if args.protocol_check:
        pg = cgroup(args.pg_pod)
        quota, period = map(int, pg["cpu.max"].split())
        health = adapt(json.loads(pathlib.Path(args.health_receipt).read_text()), state, time.time(),
                       quota/period, expected_actors, live_policy())
        print(json.dumps(health))
        return
    lease_lock = pathlib.Path(GLOBAL_LOCK).open("a")
    fcntl.flock(lease_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    # A replacement cannot issue another global permit before old leases expire.
    time.sleep(10)
    admission = Admission(args.max_admitted, args.stage_seconds)
    previous = None
    with concurrent.futures.ThreadPoolExecutor(max_workers=10) as executor:
        while True:
            try:
                records = load_records(args.jobs)
                order = [r["job"] for r in records]
                registry = {r["job"]:r for r in records}
                pg = cgroup(args.pg_pod)
                now = time.monotonic()
                cpu = int(pairs(pg["cpu.stat"])["usage_usec"])
                quota, period = map(int, pg["cpu.max"].split())
                memory = pairs(pg["memory.stat"])
                if previous is None:
                    previous = (now, cpu)
                    time.sleep(1)
                    continue
                duration = now - previous[0]
                used = (cpu - previous[1]) / duration / 1e6
                previous = (now, cpu)
                observed_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
                raw_health = json.loads(pathlib.Path(args.health_receipt).read_text())
                if time.monotonic()-policy_at > 60:
                    policy_ok, policy_at = live_policy(), time.monotonic()
                reserve = scoped_pg_reserve(records, admission.jobs, time.time(), quota/period, admission.completed)
                health = adapt(raw_health, state, time.time(), quota/period, expected_actors,
                               policy_ok, demand_started=bool(admission.jobs), pg_reserve=reserve)
                health["EngineeringReserveScope"] = "quiet exact single pilot" if reserve != 2.1 else "default ten/day"
                health_at = datetime.datetime.fromisoformat(health["ObservedAt"].replace("Z", "+00:00"))
                fresh = 0 <= (datetime.datetime.now(datetime.timezone.utc) - health_at).total_seconds() <= 30
                healthy = health_admission_allowed(health, fresh)
                pods = json.loads(call(["get", "pods", "-o", "json"]))["items"]
                candidates = {}
                for pod in pods:
                    owner = next((x.get("name") for x in pod["metadata"].get("ownerReferences", [])
                                  if x.get("kind") == "Job" and x.get("name") in order), None)
                    running = any(x.get("name") == "flows" and x.get("state", {}).get("running")
                                  for x in pod.get("status", {}).get("containerStatuses", []))
                    if owner and pod["status"].get("phase") in ("Succeeded", "Failed"):
                        admission.mark_terminal(owner)
                    if owner and running and owner not in admission.completed:
                        record = registry[owner]
                        if record.get("pilot_state", "ready") in ("completed", "failed"):
                            admission.mark_terminal(owner)
                            continue
                        if record.get("pilot_state") == "pending":
                            continue
                        if record.get("job_uid") and not any(r.get("uid")==record["job_uid"] for r in pod["metadata"].get("ownerReferences", [])):
                            raise ValueError("registered Job UID mismatch")
                        if record.get("pod_uid") and pod["metadata"]["uid"] != record["pod_uid"]:
                            raise ValueError("registered Pod UID mismatch")
                        if record.get("image") and not any(c["name"]=="flows" and c["image"]==record["image"] for c in pod["spec"]["containers"]):
                            raise ValueError("registered image mismatch")
                        if owner == "cosmos-tax-queryfix-pilot-20261115-v1" and cgroup(pod["metadata"]["name"], pilot_terminal=True).get("pilot_terminal", "").strip() == "1":
                            admission.mark_terminal(owner)
                            continue
                        candidates[owner] = pod["metadata"]["name"]
                admission.maximum = args.max_admitted if quota/period >= 16 else 1
                reservation = quiet_pilot_reservation(records, time.time(), admission.completed)
                eligible = [j for j in order if j in candidates and (not reservation or j == "cosmos-tax-queryfix-pilot-20261115-v1")]
                previous_grants = set(admission.jobs)
                granted = admission.update(eligible, now,
                                           quota / period, used, healthy and health.get("StageReady") is True)
                if granted-previous_grants:
                    state["last_permit_at"] = time.time()
                health["CapacityPassing"] = healthy and 0 <= used <= quota/period-.1
                health["QuietPilotReserved"] = reservation
                health["AdmissionGranted"] = healthy and bool(granted)
                atomic_json(home/"sampler-expected-health.safe.json", health)

                def publish(job):
                    # Unadmitted new Jobs wait without a lease, rather than fail.
                    if job not in granted:
                        return
                    pod = candidates[job]
                    pilot = job == "cosmos-tax-queryfix-pilot-20261115-v1"
                    local = cgroup(pod, pilot_terminal=pilot)
                    if pilot and local.get("pilot_terminal", "").strip() == "1":
                        admission.mark_terminal(job)
                        return
                    script = ('while read -r key value unit; do '
                              'if test "$key" = MemAvailable:; then '
                              'printf "%s" "$value"; break; fi; done </proc/meminfo')
                    available = int(call(["exec", pod, "--", "sh", "-c", script])) * 1024
                    receipt = dict(ObservedAt=observed_at, SampleDuration=int(duration * 1e9),
                        PGQuotaCores=quota / period, PGUsedCores=used,
                        PGResidentBytes=max(0, int(pg["memory.current"])-int(memory["inactive_file"])),
                        PGMemoryCurrentBytes=int(pg["memory.current"]), PGInactiveFileBytes=int(memory["inactive_file"]),
                        PGAnonBytes=int(memory["anon"]), PGShmemBytes=int(memory["shmem"]), PGKernelBytes=int(memory["kernel"]),
                        PGOOMKills=int(pairs(pg["memory.events"])["oom_kill"]),
                        PGMemoryPressureFullAvg10=float(pg["memory.pressure"].splitlines()[1].split()[1].split("=")[1]),
                        PGMemoryLimitBytes=int(pg["memory.max"]),
                        JobUsedBytes=int(local["memory.current"]), JobMemoryLimitBytes=int(local["memory.max"]),
                        NodeAvailableBytes=available, AdmissionGranted=healthy,
                        PublicHealthy=healthy, ReplayHealthy=healthy)
                    writer = ('umask 077; f=$(mktemp /private/.resource-XXXXXX); '
                              'tee "$f" >/dev/null; chmod 600 "$f"; '
                              'mv "$f" /private/resource.sample.json')
                    call(["exec", "-i", pod, "--", "sh", "-c", writer], json.dumps(receipt).encode())
                list(executor.map(publish, candidates))
                (home / "sampler-heartbeat.safe.json").write_text(json.dumps(dict(
                    at=observed_at, pg_used_cores=used, quota=quota / period,
                    health_fresh=fresh, admitted_jobs=sorted(granted-admission.completed), maximum=args.max_admitted,
                    completed_jobs=sorted(admission.completed), quiet_pilot_reserved=reservation,
                    capacity_passing=health["CapacityPassing"],
                    coordinator=identity_record, policy_authorized=policy_ok,
                    fail_reasons=health.get("FailReasons"), native_permission=healthy)) + "\n")
            except Exception as exc:
                denial = dict(ObservedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                              PublicHealthy=False, ReplayHealthy=False, NodeCPUHeadroom=False,
                              ComparableReplayWindow=False, CPUWriterPolicyAuthorized=False,
                              ReplayBaselinePreserved=False, AdmissionGranted=False,
                              FailReasons=["coordinator_input_failure:"+type(exc).__name__])
                atomic_json(home/"sampler-expected-health.safe.json", denial)
                print(type(exc).__name__, flush=True)
            time.sleep(1)


if __name__ == "__main__":
    main()
