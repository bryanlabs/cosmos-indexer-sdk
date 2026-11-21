"""Build the OPS-849 private label set (JSONL) from the raw pulls in a work directory.

Usage: build_labels.py <workdir> <set version> <out.jsonl>

Every row keeps the AddressLabel columns (chain, address, name, kind, source,
evidence, created_at) plus rule, basis (derived or published), ref (the query or
source document), license, as_of and tags, so the commission tracer's checks can
read the derived classes and a report can say how each label is known.
No paid label source, explorer tags or vendor data are used.
"""
import csv, json, os, struct, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import derive as d  # noqa: E402

work, version, out = sys.argv[1], sys.argv[2], sys.argv[3]
CREATED = "2026-11-21T00:00:00Z"
REG = "Cosmos chain-registry (CC-BY-4.0)"
rows = {}


def add(chain, address, name, kind, source, evidence, rule, basis, ref, license_, as_of, tags=()):
    key = (chain, address)
    if key in rows:
        r = rows[key]
        r["tags"] = sorted(set(r["tags"]) | set(tags) | {kind})
        r["evidence"] += "; " + evidence
        if kind == "bridge_escrow":
            r["kind"], r["name"], r["ref"], r["rule"] = kind, name, r["ref"] + "; " + ref, r["rule"] + "+" + rule
            r["license"] = r["license"] + "; " + license_
        if basis == "published" or r["basis"] == "published":
            r["basis_detail"] = "derived and published"
        return
    rows[key] = {"chain": chain, "address": address, "name": name, "kind": kind, "source": source, "evidence": evidence,
                 "created_at": CREATED, "rule": rule, "basis": basis, "ref": ref, "license": license_, "as_of": as_of,
                 "tags": sorted(set(tags) | {kind}), "set_version": version}


def P(*a):
    return os.path.join(work, *a)


# 1. Existing derived labels (module, ibc_escrow, validator_operator) carried over from tax_address_labels.
with open(P("private-raw", "existing-derived.tsv")) as f:
    for line in f:
        p = line.rstrip("\n").split("\t")
        if len(p) != 7 or p[0] != "mainnet":
            continue
        chain, addr, name, kind, source, ev, created = p
        add(chain, addr, name, kind, source, ev, source, "derived", "tax_address_labels on cosmoshub-4", "own derivation", created[:10])

# 2. Validator operator accounts for every validator the Hub node knows (bonded, unbonding, unbonded).
for v in json.load(open(P("private-raw", "hub-validators.json")))["validators"]:
    hrp, raw = d.decode(v["operator_address"])
    add("mainnet", d.encode("cosmos", raw), v["operator_address"], "validator_operator", "derived:operator-account",
        "account bytes of " + v["operator_address"], "derived:operator-account", "derived",
        "own cosmoshub-4 node: /cosmos/staking/v1beta1/validators", "own derivation", "2026-11-21")

# 3. Interchain accounts hosted on the Hub, from the ICA channel handshakes.
for e in json.load(open(P("private-raw", "hub-ica.json"))):
    owner = e["controller_port"].removeprefix("icacontroller-")
    add("mainnet", e["address"], f"interchain account of {e['chain_id'] or 'unknown chain'} ({owner})", "interchain_account",
        "derived:ica-handshake/v1",
        f"icahost channel version metadata on {e['connection']} ({', '.join(sorted(set(e['channels'])))}; states {', '.join(sorted(set(e['states'])))}); "
        f"node account check: {e['node_check']}; legacy address derivation matches: {e['legacy_derivation_matches']}",
        "derived:ica-handshake/v1", "derived",
        "own cosmoshub-4 node: /ibc/core/channel/v1/channels (port icahost, version.address) and /cosmos/auth/v1beta1/accounts",
        "own derivation", "2026-11-21", tags=("contract_like",))

# 4. Osmosis pools from poolmanager state.
for x in json.load(open(P("private-raw", "osmosis-pools.json")))["pools"]:
    t = x["@type"].split(".")[1]
    pid = int(x.get("id") or x.get("pool_id"))
    if t == "gamm":
        assets = ",".join(a["token"]["denom"] for a in x.get("pool_assets") or []) or ",".join(c["denom"] for c in x.get("pool_liquidity") or [])
    elif t == "concentratedliquidity":
        assets = x["token0"] + "," + x["token1"]
    else:
        assets = "cosmwasm code " + str(x.get("code_id"))
    kindname = {"gamm": "balancer" if "stableswap" not in x["@type"] else "stableswap", "concentratedliquidity": "concentrated", "cosmwasmpool": "cosmwasm"}[t]
    addr = x.get("address") or x.get("contract_address")
    check = ""
    if t == "gamm":
        check = "; gamm pool-id derivation matches: " + str(d.encode("osmo", d.module_derived("gamm", b"pool" + struct.pack(">Q", pid))) == addr)
    common = dict(rule="derived:osmosis-pool-state/v1", basis="derived", ref="Osmosis poolmanager state /osmosis/poolmanager/v1beta1/all-pools via Allnodes public REST from " + REG,
                  license_="chain state", as_of="2026-11-21")
    add("osmosis-1", addr, f"Osmosis pool {pid} ({kindname}: {assets})", "dex_pool", "derived:osmosis-pool-state/v1",
        f"pool {pid} address in poolmanager state{check}", **common)
    for k, label in (("incentives_address", "incentives"), ("spread_rewards_address", "spread rewards")):
        if x.get(k):
            add("osmosis-1", x[k], f"Osmosis pool {pid} {label} account", "dex_pool", "derived:osmosis-pool-state/v1",
                f"pool {pid} {k} in poolmanager state", **common)

# 5. CosmWasm contracts on Osmosis and Neutron: label and creator from chain state.
for chain, fn in (("osmosis-1", "osmosis-contracts.jsonl"), ("neutron-1", "neutron-contracts.jsonl")):
    path = P("private-raw", fn)
    if not os.path.exists(path):
        continue
    for line in open(path):
        c = json.loads(line)
        if "error" in c:
            continue
        lab = (c.get("label") or "").strip()
        add(chain, c["address"], lab or f"unnamed contract (code {c['code_id']})", "contract", "derived:wasm-contract-info/v1",
            f"code {c['code_id']}, label typed by its creator, creator {c.get('creator')}, admin {c.get('admin') or 'none'}, created at block {((c.get('created') or {}).get('block_height'))}",
            "derived:wasm-contract-info/v1", "derived", f"{chain} /cosmwasm/wasm/v1/contract state via public REST from " + REG,
            "chain state", "2026-11-21", tags=("named_contract",) if lab else ())

# 6. Bridges: Hub escrow of each bridge channel (derived and checked on the node) plus the bridge's own published contracts.
bridges = json.load(open(P("private-raw", "hub-bridges.json")))
bridge_ref = {
    "axelar": ("Axelar", "axelar-cosmoshub.json in " + REG + "; Axelar docs IBC table", "CC-BY-4.0"),
    "gravity-bridge": ("Gravity Bridge", "Gravity-Bridge/Gravity-Docs docs/list_of_IBC_channels.md (cosmoshub-4 channel-17 / channel-281); own node client chain id gravity-bridge-3", "factual address, no license stated"),
    "composable": ("Picasso / Composable IBC bridge", "composable-cosmoshub.json in " + REG + "; own node client chain id centauri-1", "CC-BY-4.0"),
}
for k, b in bridges.items():
    name, ref, lic = bridge_ref[k]
    add("mainnet", b["escrow"], f"{name} bridge escrow (transfer/{b['channel']})", "bridge_escrow", "published:bridge-channel/v1",
        f"ICS-20 escrow of transfer/{b['channel']} (node escrow_address matches derivation: {b['derivation_matches']}); "
        f"Hub flows on our node: {b['packets_sent']} packets sent, {int(b['escrow_uatom']) // 10**6} ATOM escrowed on 2026-11-21",
        "published:bridge-channel/v1", "published", ref, lic, "2026-11-21", tags=("ibc_escrow",))
ax = json.load(open(P("published", "axelar-mainnet.json")))["chains"]["ethereum"]["contracts"]
for cname in ("AxelarGateway", "InterchainTokenService"):
    add("ethereum", ax[cname]["address"], f"Axelar {cname} (Ethereum)", "bridge_contract", "published:axelar-deployments/v1",
        "Axelar's own deployment config, chains.ethereum.contracts." + cname, "published:axelar-deployments/v1", "published",
        "axelarnetwork/axelar-contract-deployments axelar-chains-config/info/mainnet.json", "MIT", "2026-11-21")
import re  # noqa: E402
g = open(P("published", "gravity-resources.md")).read()
m = re.search(r"\[(0x[0-9a-fA-F]{40})\s*\]\(https://etherscan", g)
if m:
    add("ethereum", m.group(1), "Gravity Bridge contract (Ethereum)", "bridge_contract", "published:gravity-docs/v1",
        "Gravity Bridge's own docs, resources page, Ethereum contract", "published:gravity-docs/v1", "published",
        "Gravity-Bridge/Gravity-Docs docs/resources.md", "factual address, no license stated", "2026-11-21")

# 7. Community pool spend recipients named by passed Hub governance proposals.
spends = {}
for o in json.load(open(P("private-raw", "hub-cp-spends.json"))):
    spends.setdefault(o["recipient"], []).append(o)
for addr, os_ in spends.items():
    first = sorted(os_, key=lambda o: int(o["proposal"]))[0]
    add("mainnet", addr, f"community pool spend recipient (proposal {first['proposal']}: {first['title'][:80]})", "grant_recipient",
        "derived:community-pool-spend/v1",
        "recipient of passed proposal(s) " + ", ".join(sorted({o["proposal"] for o in os_}, key=int)),
        "derived:community-pool-spend/v1", "derived", "own cosmoshub-4 node: /cosmos/gov/v1/proposals?proposal_status=PASSED",
        "chain state", "2026-11-21")

# 8. Exchange proof-of-reserves wallets where ATOM is covered.
cov = json.load(open(P("por", "coverage.json")))
for e in cov:
    ex = e["exchange"]
    fn = P("por", ex.lower().replace(" ", "").replace(".", ""), "atom_addresses.txt")
    if e.get("atom_address_count") and os.path.exists(fn):
        for a in open(fn).read().split():
            add("mainnet", a, f"{ex} proof-of-reserves wallet", "exchange", f"published:por-{ex.lower()}/v1",
                f"{ex}'s own proof-of-reserves address list, ATOM row (as of {e.get('as_of')}, signed: {e.get('signed')})",
                f"published:por-{ex.lower()}/v1", "published", e.get("source_url", ""), "exchange's own public disclosure", str(e.get("as_of")))

with open(out, "w") as f:
    for r in sorted(rows.values(), key=lambda r: (r["chain"], r["kind"], r["address"])):
        f.write(json.dumps(r, sort_keys=True) + "\n")
# The Hub rows again in the CommissionLabelFile shape the OPS-847 tracer reads
# (AddressLabel field names), for its exchange-like false-positive check.
hub = [r for r in rows.values() if r["chain"] == "mainnet"]
clf = {"Chain": "mainnet", "Rule": "ops849-derived-and-published/" + version, "From": "2019-12-11T00:00:00Z", "AsOf": "2026-11-21T00:00:00Z",
       "Labels": [{"Chain": r["chain"], "Address": r["address"], "Name": r["name"], "Kind": r["kind"], "Source": r["source"],
                   "Evidence": r["evidence"] + " [basis " + r["basis"] + "; ref " + r["ref"] + "; license " + r["license"] + "; as of " + r["as_of"] + "]",
                   "CreatedAt": r["created_at"]} for r in sorted(hub, key=lambda r: (r["kind"], r["address"]))]}
with open(out.removesuffix(".jsonl") + ".mainnet-label-file.json", "w") as f:
    json.dump(clf, f, sort_keys=True)
import collections  # noqa: E402
c = collections.Counter((r["chain"], r["kind"], r["basis"]) for r in rows.values())
for k, v in sorted(c.items()):
    print(*k, v, sep="\t")
print("total", len(rows))
