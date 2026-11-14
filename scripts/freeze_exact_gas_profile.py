#!/usr/bin/env python3
"""Freeze the reviewed private exact32 profile. No network, DB, replay or raw writes.

The output is exclusive mode 0600. No private values are printed. A fresh trusted
SHA must be frozen in public source and reviewed before the loader can use it.
This utility cannot approve an arbitrary profile or change the trusted constant.
"""
import argparse
import base64
from decimal import Decimal
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def obj(pairs):
    result = {}
    seen = set()
    for key, value in pairs:
        folded = key.casefold()
        if folded in seen:
            raise ValueError("duplicate or case-aliased member")
        seen.add(folded)
        result[key] = value
    return result


def number(s):
    if not re.fullmatch(r"0|-?[1-9][0-9]*", s):
        raise ValueError("noncanonical integer")
    n = int(s)
    if not -(1 << 63) <= n < 1 << 63:
        raise ValueError("integer overflow")
    return n


def load(raw):
    def no_fraction(_):
        raise ValueError("noninteger JSON number")
    return json.loads(raw, object_pairs_hook=obj, parse_int=number,
                      parse_float=no_fraction, parse_constant=no_fraction)


def archive(path, expected):
    raw = path.read_bytes()
    assert sha(raw) == expected
    with tarfile.open(path) as tar:
        result = {}
        for member in tar:
            assert member.isfile() and member.name not in result
            result[member.name] = tar.extractfile(member).read()
        return result


def b64(raw):
    return base64.b64encode(raw).decode("ascii")


def fee(raw):
    # Only locates field 2 without converting/rebuilding signed bytes. The Go
    # loader subsequently independently decodes the entire legacy transaction.
    def uint(pos):
        n = 0
        for shift in range(0, 70, 7):
            v = raw[pos]
            pos += 1
            n |= (v & 127) << shift
            if v < 128:
                return n, pos
        raise ValueError("varint overflow")
    size, pos = uint(0)
    assert size == len(raw) - pos
    pos += 4
    found = []
    while pos < len(raw):
        key, pos = uint(pos)
        assert key & 7 == 2
        size, pos = uint(pos)
        end = pos + size
        assert end <= len(raw)
        if key >> 3 == 2:
            found.append(raw[pos:end])
        pos = end
    assert len(found) == 1
    return found[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence-dir", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    source = "09a9e29cf22fa2cf0798f85181459cdb0438bdfb"
    financial = "3e087ab429b75f3b6204ce475908025f6c1bdcfe"
    for commit in (source, financial):
        assert subprocess.check_output(["git", "-C", str(root), "show", "-s", "--format=%H %G?", commit], text=True).strip() == commit + " G"
    tree = "6827545e750972766e164c2823379c5b6d4788e3"
    for commit in (source, financial):
        for path, expected in (("observeapp", tree),
                               ("go.mod", "e3b97c97ce70d21df4ba13f1eb4d2466d84aa25a"),
                               ("go.sum", "d9488514e1fcc1d26edb84ab72b1f2b1520596aa")):
            assert subprocess.check_output(["git", "-C", str(root), "rev-parse", commit + ":legacy/replay034/" + path], text=True).strip() == expected
    evidence = {
        "control_archive": "d4690deb56e38163da68336d36907c24b958cf25d1def4635201219d29fb1d2d",
        "metadata_archive": "16bd9cfc2bcf30c464c7182cd9e18da09ed1d110a9b2c1d8b0070d7312ce0e9c",
        "cache_archive": "ba0d700765519a08950be02d743c9a113638830990ca847a1c00046f9f86c8fb",
        "independent_calculation": "9c6fbbac720eafea573e0c2934b4c48d41bfe1c13dbd77c58df88f55197e9a16",
    }
    directory = args.evidence_dir
    control = archive(directory / "cosmos-tax-historical-gas-control-evidence.private.tar", evidence["control_archive"])
    metadata = archive(directory / "cosmos-tax-historical-gas-control-metadata.private.tar", evidence["metadata_archive"])
    cache = archive(directory / "cosmos-tax-historical-next-all31-gas-inputs.private.tar", evidence["cache_archive"])
    calc_raw = (directory / "cosmos-tax-gas-review-independent-v4.json").read_bytes()
    assert sha(calc_raw) == evidence["independent_calculation"]
    calc = load(calc_raw)
    # Fractional nonfinancial telemetry is kept exact, never converted to float
    # and never passed through the financial/profile decoder.
    state = json.loads(control["home/status.json"], object_pairs_hook=obj,
                       parse_int=number, parse_float=Decimal)
    assert (state["first_height_this_run"], state["last_height_committed"], state["state_hash_verified_through"], state["target"]) == (400001, 482090, 482090, 482090)
    assert state["stop_reason"] == "target state verified by next cached header; next block was not executed"
    assert control["source-before.sha256"] == control["source-after.sha256"]
    assert sha(control["source-before.sha256"]) == "94a0dbbefd70d94120858bd4d72d7cad0ededbe53349a5bd02684e8995bcb73c"
    assert sha(control["home/result-differences.txt"]) == "2ff60b284b334e2b29ff2523a9ab7d961a6ed2ec20f29d2976a294bcc6fb36a6"
    assert sha(control["gas-differences.ndjson"]) == "00bf692b2729d4a3015ed62f610515f624445d3b9396d7d9a661acf34a776e40"
    job = json.loads(metadata["cosmos-tax-historical-gas-control-final-job.json"], object_pairs_hook=obj, parse_float=Decimal)
    pod = json.loads(metadata["cosmos-tax-historical-gas-control-final-pod.json"], object_pairs_hook=obj, parse_float=Decimal)["items"][0]
    assert job["status"]["succeeded"] == 1 and pod["status"]["phase"] == "Succeeded"
    cs = pod["status"]["containerStatuses"][0]
    image = "sha256:c9405f56f5f4da5505987a9594d446dc0741906ca58cd0d490c13fd6f44e1f91"
    assert cs["imageID"].endswith("@" + image) and cs["restartCount"] == 0 and cs["state"]["terminated"]["exitCode"] == 0
    pairs = [load(line) for line in control["gas-differences.ndjson"].splitlines()]
    assert len(pairs) == 32
    calculations = {(r["height"], r["tx_index"]): r for r in calc["rows"]}
    updates = {r["h"]: r["u"] for r in map(load, control["home/validator-updates.jsonl"].splitlines())}
    cases, blocks = [], []
    for summary in calc["blocks"]:
        height = summary["height"]
        prefix = f"cosmoshub-2/{height//10000}/{height}"
        raw_block = gzip.decompress(cache[prefix + "-block.raw.gz"])
        raw_results = gzip.decompress(cache[prefix + "-block_results.raw.gz"])
        assert sha(raw_block) == summary["cached_block_raw_json_sha256"]
        assert sha(raw_results) == summary["cached_results_raw_json_sha256"]
        block, results = load(raw_block)["result"]["block"], load(raw_results)["result"]["results"]
        for p in pairs:
            if p["height"] != height:
                continue
            i = p["tx_index"]
            raw = base64.b64decode(block["data"]["txs"][i], validate=True)
            row = calculations[height, i]
            assert sha(raw) == p["tx_sha256"] == row["signed_tx_sha256"]
            cases.append({"pair": p, "signed_tx": b64(raw), "signed_fee": b64(fee(raw)),
                          "source_attested_nongas": results["DeliverTx"][i]})
        blocks.append({"height": height, "raw_block_envelope": b64(raw_block),
                       "raw_results_envelope": b64(raw_results),
                       "native_block_gas_consumed": summary["native_block_gas_consumed_from_per_tx_meter_caps"],
                       "genesis_max_gas_not_checkpoint_measurement": 2000000,
                       "historical_code_data_results_root": summary["historical_cached_results_hash"],
                       "retained_native_validator_updates": updates.get(height, [])})
    assert len(cases) == 32 and len(blocks) == 31
    profile = {
        "version": "hub2-exact32-source-attested-v1", "chain": "cosmoshub-2",
        "release": state["release"], "diagnostic_source": source, "diagnostic_image": image,
        "financial_recording_lineage": {"source": financial,
            "image": "sha256:656638fa97f3792c9ba3cddb4b7f11703da5025d51b22163cc0036dd280781bd",
            "observer_git_tree": tree, "sdk_mod_git_blob": "e3b97c97ce70d21df4ba13f1eb4d2466d84aa25a",
            "sdk_sum_git_blob": "d9488514e1fcc1d26edb84ab72b1f2b1520596aa"},
        "completion": {"from": 400001, "through": 482090, "verified": 482090, "unexecuted_next_header": 482091,
            "state_checked_by_completed_signed_run": True, "results_checked_by_completed_signed_run": True,
            "validators_checked_by_completed_signed_run": True, "parent_checked_by_completed_signed_run": True,
            "final_header_checked_by_completed_signed_run": True, "next_block_executed": False,
            "full_native_response_raw_retained": False, "fresh_physical_all_header_verification": False,
            "nongas_evidence_method": "source-attested Data/Log/ordered Tags; Info/Codespace constrained by pinned successful SDK constructors; NOT two retained full native/cached raw responses"},
        "original_marker_bytes": b64(control["home/result-differences.txt"]),
        "retained_native_gas_ndjson": b64(control["gas-differences.ndjson"]),
        "cases": cases, "blocks": blocks, "reviewed_artifact_sha256": evidence,
    }
    raw = (json.dumps(profile, sort_keys=True, separators=(",", ":")) + "\n").encode()
    fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as dest:
        dest.write(raw)
    print(json.dumps({"profile_sha256": sha(raw), "cases": 32, "heights": 31,
                      "raw_inputs_unchanged": True, "financial_acceptance": False}))


if __name__ == "__main__":
    main()
