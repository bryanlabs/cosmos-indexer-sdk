"""Pure fail-closed adapter for the existing read-only observer and coordinator.
No watcher, native execution, SQL, or cluster mutation. Engineering comparison
bands are explicit, not provider limits. Historical 08:15 floors are diagnostic.
"""
import datetime
import hashlib
import math

LABELS = ("hub2_main", "hub2_record", "hub2_range_2200001_2400000")
POLICY_SHA = "db99f8986e75282c27ea81479ce379122c021452c1f6b6ee451c4d49060549f4"
DAY_START = datetime.datetime(2026, 11, 15, 11, 30, tzinfo=datetime.timezone.utc).timestamp()
GLOBAL_LOCK = "/Users/danb/.cache/corrected324-acceleration-20261115/resource-sampler.lock"
MAIN_HEALTHY_FLOOR = 9.0
PG_BOOTSTRAP_RESERVE = 2.1
DENSITY_BAND = .25


def epoch(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def finite(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def policy_verified(cm, cron):
    try:
        script = cm["data"]["quiet_hours_cpu.sh"]
        spec = cron["spec"]
        pod = spec["jobTemplate"]["spec"]["template"]["spec"]
        container = next(c for c in pod["containers"] if c["name"] == "resize")
        env = {e["name"]: e.get("value") for e in container["env"]}
        return (hashlib.sha256(script.encode()).hexdigest() == POLICY_SHA
                and spec["schedule"] == "*/2 * * * *" and not spec.get("suspend", False)
                and container["command"] == ["/bin/bash", "/tools/quiet_hours_cpu.sh"]
                and env["POSTGRES_CPU"] == "10" and env["POSTGRES_DAY_CPU"] in ("16", "24")
                and any(v.get("configMap", {}).get("name") == "cosmos-tax-quiet-hours"
                        for v in pod["volumes"]))
    except (KeyError, TypeError, StopIteration):
        return False


def identity(actor):
    return (actor["pod_uid"], tuple(sorted((p["pid"], p["birth_ticks"])
                                          for p in actor["native_processes"])))


def adapt(raw, state, now, actual_quota, expected_actors, policy_ok, demand_started=False, pg_reserve=PG_BOOTSTRAP_RESERVE):
    """State is owned by ONE coordinator. No baseline resets after first permit.

    Bootstrap works at the actual quiet10 or day quota, with source windows entirely after the observed
    quota transition, and two distinct healthy windows before new demand. Rates
    then stay above the observed healthy envelope with matched transaction
    density. Density mismatch is UNKNOWN, never an invented normalized capacity.
    """
    result = dict(ObservedAt=datetime.datetime.fromtimestamp(now, datetime.timezone.utc).isoformat(),
                  PublicHealthy=False, ReplayHealthy=False, NodeCPUHeadroom=False,
                  ComparableReplayWindow=False, CPUWriterPolicyAuthorized=bool(policy_ok),
                  ReplayBaselinePreserved=False, AdmissionGranted=False, FailReasons=[],
                  ActualPGQuotaCores=actual_quota, H1Status="unknown", FinancialAcceptance=False,
                  ReplayHealthyScope="actual-quota, no-new-demand bootstrap, then matched-density non-regression",
                  EngineeringPolicy=dict(historical_recovery_diagnostic_floor=MAIN_HEALTHY_FLOOR,
                                         pg_bootstrap_reserve_cores=pg_reserve,
                                         density_relative_band=DENSITY_BAND, source_window_seconds=[30, 61.5]),
                  RawObserverReplayHealthy=raw.get("ReplayHealthy"))
    reasons = result["FailReasons"]
    try:
        if not 0 <= now-epoch(raw["ObservedAt"]) <= 30:
            raise ValueError("stale_receipt")
        result["PublicHealthy"] = raw["PublicHealthy"] is True and raw["ReadOnlyDBSelect1"] is True and raw["PublicHTTP"]["http_status"] == 200
        nodes = raw["NodeCPU"]
        result["NodeCPUHeadroom"] = raw["NodeHeadroom"] is True and all(
            0 <= now-nodes[n]["observed_epoch"] <= 30
            and 30 <= nodes[n]["window_seconds"] <= 65
            and finite(nodes[n]["used_cores"]) and nodes[n]["used_cores"] >= 0
            and finite(nodes[n]["allocatable_cores"]) and nodes[n]["allocatable_cores"] > 0
            and abs(nodes[n]["allocatable_cores"]-nodes[n]["used_cores"]-nodes[n]["headroom_cores"]) < .01
            and nodes[n]["headroom_cores"] >= nodes[n]["reserve_cores"] >= .1
            and nodes[n]["headroom_ok"] is True for n in ("worker-1", "controller-1"))
        actors = raw["Actors"]
        for label in ("hub1_main",)+LABELS:
            a = actors[label]
            if (identity(a) != identity(expected_actors[label]) or a["pod_phase"] != "Running"
                    or not a["container_status"] or any(c["restarts"] != 0 or "running" not in c["state"] for c in a["container_status"])):
                raise ValueError("actor_identity_changed:"+label)
        h1 = actors["hub1_main"]["native_progress"]
        if actors["hub1_main"]["native_processes"] or h1["watcher"]["step"] != "hold" or h1["watcher"]["fresh"] is not True:
            raise ValueError("hub1_phase_unknown")
        result["H1Status"] = "known hold/no-native, neutral, NOT healthy zero-rate"
        pg = raw["Postgres"]
        quota, period = map(int, pg["kernel_cgroup"]["cpu_max"].split())
        if (not finite(actual_quota) or actual_quota <= 0 or quota/period != actual_quota
                or pg["uid"] != expected_actors["postgres_uid"]
                or pg["restarts"] != 0 or not 0 <= now-epoch(pg["observed_at"]) <= 30):
            raise ValueError("actual_quota_or_pg_identity_unknown")
        if state.get("pg_uid", pg["uid"]) != pg["uid"]:
            raise ValueError("pg_uid_changed")
        state["pg_uid"] = pg["uid"]
        rates = raw["ReplayRates"]
        for label in LABELS:
            r = rates[label]
            if (r["pod_uid_and_pid_birth_match"] is not True or not 30 <= r["window_seconds"] <= 61.5
                    or not 0 <= now-r["source_end"] <= 45 or r["source_start"] >= r["source_end"]
                    or abs((r["source_end"]-r["source_start"])-r["window_seconds"]) > .1
                    or not finite(r["verified_per_second"]) or r["verified_per_second"] <= 0
                    or not finite(r["transactions_per_block"]) or r["transactions_per_block"] < 0
                    or not finite(r["verified_delta"]) or r["verified_delta"] <= 0
                    or not finite(r["transactions_delta"]) or r["transactions_delta"] < 0
                    or abs(r["verified_delta"]/r["window_seconds"]-r["verified_per_second"]) > .00001
                    or abs(r["transactions_delta"]/r["verified_delta"]-r["transactions_per_block"]) > .00001):
                raise ValueError("source_window_unknown:"+label)
        result["ComparableReplayWindow"] = True
        result["SourceProof"] = dict(observer_at=raw["ObservedAt"], pg_uid=pg["uid"],
                                    kernel_cpu_max=pg["kernel_cgroup"]["cpu_max"], rates=rates, nodes=nodes)
        if not result["PublicHealthy"] or not result["NodeCPUHeadroom"] or not policy_ok:
            raise ValueError("public_node_or_policy_not_passing")
        if raw.get("Errors"):
            raise ValueError("observer_errors")
        if now < DAY_START and actual_quota != 10:
            raise ValueError("quiet_policy_requires_actual10_not_override")
        if "day_quota_observed_at" not in state:
            state["day_quota_observed_at"] = now
        if state.get("bootstrap_quota", actual_quota) != actual_quota and not demand_started:
            state["day_quota_observed_at"] = now
            state.pop("first_window", None)
            state.pop("baseline", None)
            state.pop("baseline_source_windows", None)
            state["pg_points"] = []
        state["bootstrap_quota"] = actual_quota
        pg_time = epoch(pg["observed_at"])
        points = state.setdefault("pg_points", [])
        if not points or pg_time != points[-1][0]:
            points.append([pg_time, pg["kernel_cgroup"]["usage_usec"]])
        points[:] = [p for p in points if pg_time-p[0] <= 75]
        old = next((p for p in points if 30 <= pg_time-p[0] <= 65), None)
        if old is None:
            raise ValueError("need_comparable_pg_cpu_window")
        used = (points[-1][1]-old[1])/1e6/(pg_time-old[0])
        result["SourceProof"]["pg_average_used_cores"] = used
        if not 0 <= used <= actual_quota-pg_reserve:
            raise ValueError("pg_cpu_headroom_not_proven")
        result["Historical9FloorRecovered"] = rates["hub2_main"]["verified_per_second"] >= MAIN_HEALTHY_FLOOR
        result["BaselineMethod"] = "fresh actual-quota zero-new-demand measured CPU/headroom and matched-workload windows; NOT a claim density explains historical slowdown"
        if any(rates[k]["source_start"] < state["day_quota_observed_at"] for k in LABELS):
            raise ValueError("need_fresh_actual_quota_source_windows")
        signature = [rates[k]["source_end"] for k in LABELS]
        baseline = state.get("baseline")
        if baseline is None:
            if demand_started:
                raise ValueError("cannot_bootstrap_baseline_under_new_demand")
            first = state.get("first_window")
            if first is None:
                state["first_window"] = {k: dict(rates[k]) for k in LABELS}
                raise ValueError("need_second_distinct_healthy_window")
            if not all(signature[i] > first[k]["source_end"] for i,k in enumerate(LABELS)):
                raise ValueError("need_second_distinct_healthy_window")
            if not all(density_matches(first[k]["transactions_per_block"], rates[k]["transactions_per_block"]) for k in LABELS):
                state["first_window"] = {k: dict(rates[k]) for k in LABELS}
                raise ValueError("bootstrap_density_mismatch_need_second_new_context_window")
            baseline = {k: dict(rate_floor=min(first[k]["verified_per_second"], rates[k]["verified_per_second"]),
                                density=first[k]["transactions_per_block"]) for k in LABELS}
            state["baseline"] = baseline
            state["baseline_source_windows"] = [first, {k: dict(rates[k]) for k in LABELS}]
        if not all(density_matches(baseline[k]["density"], rates[k]["transactions_per_block"]) for k in LABELS):
            raise ValueError("density_mismatch_no_source_CPU_normalization_proof")
        if not all(rates[k]["verified_per_second"] >= baseline[k]["rate_floor"] for k in LABELS):
            raise ValueError("measured_replay_regression")
        result["ReplayBaselinePreserved"] = True
        result["ReplayHealthy"] = True
        result["AdmissionGranted"] = True
        last_permit = state.get("last_permit_at")
        result["StageReady"] = last_permit is None or (now-last_permit >= 60 and all(rates[k]["source_start"] >= last_permit for k in LABELS))
        result["BaselineProof"] = state["baseline_source_windows"]
    except (KeyError, TypeError, ValueError, ZeroDivisionError) as exc:
        reasons.append(str(exc) if isinstance(exc, ValueError) else "partial_or_invalid_schema:"+type(exc).__name__)
    return result


def density_matches(reference, current):
    if reference == 0:
        return current == 0
    return abs(current-reference)/reference <= DENSITY_BAND
