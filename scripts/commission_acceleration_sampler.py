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


def call(args, data=None):
    return subprocess.check_output(
        ["kubectl", "--context", "bryanlabs", "-n", "apps"] + args,
        input=data, stderr=subprocess.DEVNULL, timeout=3)


def cgroup(pod):
    script = ('for f in cpu.stat cpu.max memory.stat memory.current memory.max memory.events memory.pressure; '
              'do printf "FILE:%s\\n" "$f"; while IFS= read -r line; '
              'do printf "%s\\n" "$line"; done <"/sys/fs/cgroup/$f"; done')
    text = call(["exec", pod, "--", "sh", "-c", script]).decode()
    return {x.split("\n", 1)[0]: x.split("\n", 1)[1]
            for x in text.split("FILE:")[1:]}


def pairs(text):
    return dict(line.split() for line in text.splitlines())


class Admission:
    def __init__(self, maximum, stage_seconds):
        self.maximum = maximum
        self.stage_seconds = stage_seconds
        self.jobs = []
        self.last_add = float("-inf")

    def update(self, eligible, now, quota, used, healthy):
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
    parser.add_argument("--max-admitted", type=int, default=10)
    parser.add_argument("--stage-seconds", type=float, default=30)
    args = parser.parse_args()
    records = json.loads(pathlib.Path(args.jobs).read_text())["records"]
    # Interleave groups so both corrected queues get early measured slots.
    order = [r["job"] for r in sorted(records,
             key=lambda r: (r["job"].split("-s")[1][:2], "-later-" in r["job"]))]
    if not 1 <= args.max_admitted <= len(order) or args.stage_seconds < 10:
        raise ValueError("invalid staged admission budget")
    home = pathlib.Path(__file__).parent
    lease_lock = (home / "resource-sampler.lock").open("a")
    fcntl.flock(lease_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    # A replacement cannot issue another global permit before old leases expire.
    time.sleep(7)
    admission = Admission(args.max_admitted, args.stage_seconds)
    previous = None
    with concurrent.futures.ThreadPoolExecutor(max_workers=10) as executor:
        while True:
            try:
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
                health = json.loads(pathlib.Path(args.health_receipt).read_text())
                health_at = datetime.datetime.fromisoformat(health["ObservedAt"].replace("Z", "+00:00"))
                fresh = 0 <= (datetime.datetime.now(datetime.timezone.utc) - health_at).total_seconds() <= 30
                healthy = fresh and health.get("PublicHealthy") is True and health.get("ReplayHealthy") is True
                pods = json.loads(call(["get", "pods", "-o", "json"]))["items"]
                candidates = {}
                for pod in pods:
                    owner = next((x.get("name") for x in pod["metadata"].get("ownerReferences", [])
                                  if x.get("kind") == "Job" and x.get("name") in order), None)
                    running = any(x.get("name") == "flows" and x.get("state", {}).get("running")
                                  for x in pod.get("status", {}).get("containerStatuses", []))
                    if owner and running:
                        candidates[owner] = pod["metadata"]["name"]
                granted = admission.update([j for j in order if j in candidates], now,
                                           quota / period, used, healthy)

                def publish(job):
                    # Unadmitted new Jobs wait without a lease, rather than fail.
                    if job not in granted:
                        return
                    pod = candidates[job]
                    local = cgroup(pod)
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
                        NodeAvailableBytes=available, AdmissionGranted=True,
                        PublicHealthy=healthy, ReplayHealthy=healthy)
                    writer = ('umask 077; f=$(mktemp /private/.resource-XXXXXX); '
                              'tee "$f" >/dev/null; chmod 600 "$f"; '
                              'mv "$f" /private/resource.sample.json')
                    call(["exec", "-i", pod, "--", "sh", "-c", writer], json.dumps(receipt).encode())
                list(executor.map(publish, candidates))
                (home / "sampler-heartbeat.safe.json").write_text(json.dumps(dict(
                    at=observed_at, pg_used_cores=used, quota=quota / period,
                    health_fresh=fresh, admitted_jobs=sorted(granted), maximum=args.max_admitted)) + "\n")
            except Exception as exc:
                print(type(exc).__name__, flush=True)
            time.sleep(1)


if __name__ == "__main__":
    main()
