# INF-748: sum the mint event of every cosmoshub-4 block, per UTC day.
# Reads block results only (raw cache files, or our node's RPC for heights the
# cache does not hold). Read only; writes one JSON line per unit to --out.
import argparse, base64, bisect, gzip, http.client, json, os, re, sys, time, zlib
from decimal import Decimal
from multiprocessing import Pool

BY_VALIDATOR = False
KNOWN = {b'amount', b'bonded_ratio', b'inflation', b'annual_provisions', b'validator', b'mode',
         b'proposal_id', b'proposal_result', b'proposal_log'}
DEC = json.JSONDecoder()
UATOM = re.compile(r'(?:^|,)([0-9.]+)uatom(?:,|$)')


def attrs(ev):
    out = {}
    for a in ev.get('attributes') or []:
        k, v = a.get('key') or '', a.get('value') or ''
        if k.encode() not in KNOWN:
            try:
                kd = base64.b64decode(k, validate=True)
            except Exception:
                kd = None
            if kd in KNOWN:
                k = kd.decode()
                try:
                    v = base64.b64decode(v, validate=True).decode() if v else ''
                except Exception:
                    pass
        out[k] = v
    return out


B64 = {base64.b64encode(k).decode(): k.decode() for k in KNOWN}


def attr(ev, name):
    """One attribute's value, plain or base64-encoded (Tendermint 0.34)."""
    for a in ev.get('attributes') or []:
        k = a.get('key')
        if k == name:
            return a.get('value') or ''
        if B64.get(k) == name:
            v = a.get('value') or ''
            try:
                return base64.b64decode(v, validate=True).decode() if v else ''
            except Exception:
                return v
    return None


def arr(text, key):
    i = text.find('"%s":' % key)
    if i < 0:
        return None
    j = i + len(key) + 3
    while text[j] in ' \n\r\t':
        j += 1
    v, _ = DEC.raw_decode(text, j)
    return v or []


def parse(raw, height):
    """Return (mint attrs or None, rewards Decimal, commission Decimal, [(proposal_id, result)])."""
    b = raw
    i = b.find(b'"height":"')
    if i < 0 or not b[i + 10:i + 10 + len(str(height)) + 1] == (str(height) + '"').encode():
        raise ValueError('height mismatch')
    starts = [b.find(k) for k in (b'"begin_block_events":', b'"finalize_block_events":', b'"end_block_events":')]
    starts = [s for s in starts if s >= 0]
    if not starts:
        raise ValueError('no block events key')
    text = b[min(starts):].decode()
    begin, end = [], []
    for ev in arr(text, 'begin_block_events') or []:
        begin.append(ev)
    for ev in arr(text, 'end_block_events') or []:
        end.append(ev)
    for ev in arr(text, 'finalize_block_events') or []:
        (end if attr(ev, 'mode') == 'EndBlock' else begin).append(ev)
    mint = None
    rew = com = Decimal(0)
    props = []
    per_val = {}
    for ev in begin:
        t = ev.get('type')
        if t == 'mint':
            if mint is not None:
                raise ValueError('two mint events')
            mint = attrs(ev)
        elif t == 'rewards' or t == 'commission':
            m = UATOM.search(attr(ev, 'amount') or '')
            if m:
                v = Decimal(m.group(1))
                if t == 'rewards':
                    rew += v
                else:
                    com += v
                if BY_VALIDATOR:
                    pv = per_val.setdefault(attr(ev, 'validator') or '?', [Decimal(0), Decimal(0)])
                    pv[0 if t == 'rewards' else 1] += v
    for ev in end:
        if ev.get('type') == 'active_proposal':
            a = attrs(ev)
            props.append((a.get('proposal_id'), a.get('proposal_result')))
    if BY_VALIDATOR:
        return mint, rew, com, props, per_val
    return mint, rew, com, props


def new_day():
    return {'blocks': 0, 'minted': 0, 'rewards': Decimal(0), 'commission': Decimal(0),
            'first': None, 'last': None, 'infl_sum': 0.0, 'bonded_sum': 0.0, 'proposals': []}


def unit_heights(u):
    return range(u[1], u[2] + 1)


class Src:
    def __init__(self, kind, root):
        self.kind, self.root, self.conn = kind, root, None

    def get(self, h):
        if self.kind == 'cache':
            p = '%s/%d/%d-block_results.raw.gz' % (self.root, h // 10000, h)
            try:
                with open(p, 'rb') as f:
                    data = f.read()
                    try:
                        os.posix_fadvise(f.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)
                    except Exception:
                        pass
            except FileNotFoundError:
                return None
            return gzip.decompress(data)
        for attempt in range(6):
            try:
                if self.conn is None:
                    host, port = self.root.split(':')
                    self.conn = http.client.HTTPConnection(host, int(port), timeout=60)
                self.conn.request('GET', '/block_results?height=%d' % h)
                r = self.conn.getresponse()
                body = r.read()
                if r.status != 200:
                    raise IOError('status %d' % r.status)
                return body
            except Exception:
                self.conn = None
                time.sleep(1 + attempt)
        raise IOError('rpc failed at %d' % h)


BOUNDS = None
SRC = None


def init(bounds_path, kind, root, by_validator=False):
    global BOUNDS, SRC, BY_VALIDATOR
    BY_VALIDATOR = by_validator
    d = json.load(open(bounds_path))
    BOUNDS = ([h for _, h in d['days']], [k for k, _ in d['days']])
    SRC = Src(kind, root)


def run_unit(u):
    hs, ks = BOUNDS
    days, missing, nomint, bad = {}, [], [], []
    t0 = time.time()
    for h in unit_heights(u):
        raw = SRC.get(h)
        if raw is None:
            missing.append(h)
            continue
        try:
            parsed = parse(raw, h)
            mint, rew, com, props = parsed[:4]
        except Exception as e:
            bad.append([h, str(e)[:120]])
            continue
        k = ks[bisect.bisect_right(hs, h) - 1]
        d = days.setdefault(k, new_day())
        d['blocks'] += 1
        d['rewards'] += rew
        d['commission'] += com
        for p in props:
            d['proposals'].append([h, p[0], p[1]])
        if BY_VALIDATOR:
            vals = d.setdefault('validators', {})
            for v, (vr, vc) in parsed[4].items():
                t = vals.setdefault(v, [Decimal(0), Decimal(0)])
                t[0] += vr
                t[1] += vc
        if mint is None:
            nomint.append(h)
            continue
        amt = int(mint['amount'])
        d['minted'] += amt
        snap = [h, mint.get('inflation'), mint.get('bonded_ratio'), mint.get('annual_provisions'), amt]
        if d['first'] is None:
            d['first'] = snap
        d['last'] = snap
        d['infl_sum'] += float(mint['inflation'])
        d['bonded_sum'] += float(mint['bonded_ratio'])
    for d in days.values():
        d['rewards'] = str(d['rewards'])
        d['commission'] = str(d['commission'])
        if 'validators' in d:
            d['validators'] = {v: [str(a), str(b)] for v, (a, b) in d['validators'].items()}
    return {'unit': u[0], 'start': u[1], 'end': u[2], 'days': days, 'missing': missing,
            'no_mint': nomint, 'bad': bad, 'secs': round(time.time() - t0, 1)}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--bounds', required=True)
    ap.add_argument('--source', choices=['cache', 'rpc'], required=True)
    ap.add_argument('--root', required=True, help='cache dir or host:port')
    ap.add_argument('--start', type=int, required=True)
    ap.add_argument('--end', type=int, required=True)
    ap.add_argument('--unit', type=int, default=10000)
    ap.add_argument('--procs', type=int, default=8)
    ap.add_argument('--out', required=True)
    ap.add_argument('--by-validator', action='store_true', help='also keep rewards and commission per validator per day')
    a = ap.parse_args()
    done = set()
    if os.path.exists(a.out):
        for line in open(a.out):
            done.add(json.loads(line)['unit'])
    units = []
    h = a.start
    while h <= a.end:
        e = min(a.end, (h // a.unit + 1) * a.unit - 1)
        name = '%s:%d-%d' % (a.source, h, e)
        if name not in done:
            units.append((name, h, e))
        h = e + 1
    print(json.dumps({'units': len(units), 'skipped': len(done)}), flush=True)
    n = 0
    with Pool(a.procs, initializer=init, initargs=(a.bounds, a.source, a.root, a.by_validator)) as p, open(a.out, 'a') as out:
        for r in p.imap_unordered(run_unit, units):
            out.write(json.dumps(r) + '\n')
            out.flush()
            n += 1
            if n % 20 == 0 or r['missing'] or r['bad'] or r['no_mint']:
                print(json.dumps({'n': n, 'of': len(units), 'unit': r['unit'], 'secs': r['secs'],
                                  'missing': len(r['missing']), 'bad': len(r['bad']), 'no_mint': len(r['no_mint'])}),
                      flush=True)
    print(json.dumps({'finished': n}), flush=True)


if __name__ == '__main__':
    main()
