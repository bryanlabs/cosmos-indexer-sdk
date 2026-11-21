"""Label coverage of the busiest Hub wallets. Prints counts only.
Usage: measure.py <workdir> <label set jsonl>"""
import collections, json, os, sys
w, ls = sys.argv[1], sys.argv[2]
P = lambda *a: os.path.join(w, *a)
old = {}
for l in open(P("private-raw", "existing-all.tsv")):
    p = l.rstrip("\n").split("\t")
    if len(p) == 2 and p[0] != "address":
        old[p[0]] = p[1]
new = {}
for l in open(ls):
    r = json.loads(l)
    new[r["address"]] = r["kind"]
sweep = {}
mf = os.path.expanduser("~/.cache/ops847/private/memo-follow-raw.tsv")
if os.path.exists(mf):
    for l in open(mf):
        p = l.rstrip("\n").split("\t")
        if len(p) == 8 and p[0] != p[1]:
            sweep.setdefault(p[1], 0)
            sweep[p[1]] += 1
sets = {"busiest by planner statistics (over 500k rows one way)": open(P("private-busy14.txt")).read().split(),
        "tax_balance_deferred (over 500k attribute rows)": [l.split("\t")[0] for l in open(P("private-deferred40.tsv"))]}
for name, s in sets.items():
    before = collections.Counter(old.get(a, "none") for a in s)
    after = collections.Counter(new.get(a) or old.get(a) or ("sweep_destination(OPS-847)" if a in sweep else "none") for a in s)
    print(name, len(s))
    print("  before:", dict(before))
    print("  after: ", dict(after))
    print("  labelled before", sum(v for k, v in before.items() if k != "none"), "after", sum(v for k, v in after.items() if k != "none"))
