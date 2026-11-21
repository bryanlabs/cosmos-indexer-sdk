"""Read every CosmWasm contract's label, creator, admin and code id from public LCD endpoints.

Usage: fetch_wasm.py <chain> <out.jsonl> <lcd base> [<lcd base> ...]
Resumable: contracts already in out.jsonl are skipped. Endpoints come from the
Cosmos chain-registry (CC-BY-4.0) and are used round robin.
"""
import concurrent.futures, itertools, json, os, sys, threading, time, urllib.parse, urllib.request

chain, out, bases = sys.argv[1], sys.argv[2], sys.argv[3:]
rr = itertools.cycle(bases)
lock = threading.Lock()


def get(path, tries=int(os.environ.get("OPS849_TRIES", "12"))):
    err = None
    for i in range(tries):
        with lock:
            base = next(rr)
        try:
            req = urllib.request.Request(base + path, headers={"User-Agent": "bryanlabs-ops849-labels"})
            with urllib.request.urlopen(req, timeout=60) as r:
                return json.load(r)
        except Exception as e:  # retry on another endpoint
            err = e
            time.sleep(min(60, 2 ** i))
    raise RuntimeError(f"{path}: {err}")


def paged(path, key):
    nk = None
    while True:
        q = path + ("&" if "?" in path else "?") + "pagination.limit=1000"
        if nk:
            q += "&pagination.key=" + urllib.parse.quote(nk)
        d = get(q)
        yield from d.get(key) or []
        nk = (d.get("pagination") or {}).get("next_key")
        if not nk:
            return


codes = [c["code_id"] for c in paged("/cosmwasm/wasm/v1/code", "code_infos")]
print(chain, "codes", len(codes), flush=True)


def contracts_of(code):
    return code, list(paged(f"/cosmwasm/wasm/v1/code/{code}/contracts", "contracts"))


addrs = []
with concurrent.futures.ThreadPoolExecutor(int(os.environ.get("OPS849_PER_ENDPOINT", "4")) * len(bases)) as ex:
    for code, cs in ex.map(contracts_of, codes):
        addrs += [(code, a) for a in cs]
print(chain, "contracts", len(addrs), flush=True)

done = set()
if os.path.exists(out):
    for line in open(out):
        try:
            done.add(json.loads(line)["address"])
        except Exception:
            pass
todo = [(c, a) for c, a in addrs if a not in done]
print(chain, "todo", len(todo), flush=True)


def info(ca):
    code, a = ca
    try:
        ci = get(f"/cosmwasm/wasm/v1/contract/{a}")["contract_info"]
        return {"address": a, "code_id": ci.get("code_id", code), "label": ci.get("label", ""), "creator": ci.get("creator", ""), "admin": ci.get("admin", ""), "created": ci.get("created")}
    except Exception as e:
        return {"address": a, "code_id": code, "error": str(e)[:200]}


n = 0
with open(out, "a") as f, concurrent.futures.ThreadPoolExecutor(int(os.environ.get("OPS849_PER_ENDPOINT", "4")) * len(bases)) as ex:
    for row in ex.map(info, todo):
        f.write(json.dumps(row) + "\n")
        n += 1
        if n % 200 == 0:
            f.flush()
            print(chain, "info", n, "of", len(todo), flush=True)
print(chain, "finished", n, flush=True)
