# Find the tally height of a proposal within a height range (active_proposal end block event).
import sys
sys.path.insert(0, '/work')
import extract
src = extract.Src('cache', '/cache')
pid, lo, hi = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
for h in range(lo, hi + 1):
    raw = src.get(h)
    if raw is None:
        print('missing', h); continue
    _, _, _, props = extract.parse(raw, h)
    for p, res in props:
        if p == pid:
            print(h, p, res); sys.exit(0)
print('not found')
