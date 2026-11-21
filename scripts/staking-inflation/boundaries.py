# Day start heights for cosmoshub-4 from the indexed blocks table (read only):
# for each UTC day, the first height whose block time is on or after midnight.
import json, subprocess, sys, datetime as dt
POD = 'cosmos-tax-sdk-postgres-85c85559c-864tr'
def q(heights):
    sql = ("BEGIN READ ONLY; SET statement_timeout='120s'; "
           "COPY (SELECT height, to_char(time_stamp AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS') FROM blocks "
           "WHERE chain_id=1 AND height = ANY('{%s}'::bigint[])) TO STDOUT; COMMIT;" % ','.join(map(str, heights)))
    out = subprocess.run(['kubectl', '--context', 'bryanlabs', '-n', 'apps', 'exec', '-i', POD, '-c', 'postgres', '--',
                          'psql', '-U', 'postgres', '-d', 'cosmostax', '-X', '-q', '-v', 'ON_ERROR_STOP=1'],
                         input=sql.encode(), capture_output=True, check=True).stdout.decode()
    r = {}
    for line in out.splitlines():
        if '\t' in line:
            h, t = line.split('\t'); r[int(h)] = t
    return r
lo, hi = int(sys.argv[1]), int(sys.argv[2])
step = 1000
samples = list(range(lo, hi, step)) + [hi]
t = {}
for i in range(0, len(samples), 5000):
    t.update(q(samples[i:i+5000]))
missing = [h for h in samples if h not in t]
if missing:
    sys.exit('missing sample heights: %d, first %s' % (len(missing), missing[:5]))
day = lambda h: t[h][:10]
# open intervals (a, b): day(a) < day(b); find first height with day > day(a)
todo = [(a, b) for a, b in zip(samples, samples[1:]) if day(a) != day(b)]
bounds = {}
while todo:
    mids = sorted({(a + b) // 2 for a, b in todo if b - a > 1})
    if mids:
        t.update(q(mids))
    nxt = []
    for a, b in todo:
        if b - a <= 1:
            # b is the first height of a new day; a day may be skipped only if no block fell in it
            bounds[day(b)] = b
            continue
        m = (a + b) // 2
        if m not in t:
            sys.exit('missing height %d' % m)
        if day(m) == day(a):
            nxt.append((m, b))
        else:
            nxt.append((a, m))
    todo = nxt
bounds[day(lo)] = bounds.get(day(lo), lo)
res = {'first_height': lo, 'last_height': hi, 'last_time': t[hi], 'first_time': t[lo],
       'days': sorted(bounds.items())}
json.dump(res, sys.stdout)
