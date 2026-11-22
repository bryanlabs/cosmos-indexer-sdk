# Staking inflation data (INF-748)

Builds `taxapi/stakingdata/inflation_daily.json`, the daily ATOM issuance file
behind `GET /staking/inflation`. Everything here is read only.

1. `boundaries.py LO HI > bounds.json`: first height of each UTC day, from the
   indexed `blocks` table (`BEGIN READ ONLY`). Set `POD` to the current tax
   Postgres pod.
2. `extract.py`: sums each block's `mint` event plus the `rewards` and
   `commission` begin-block events per UTC day, and records passed
   `active_proposal` tallies. `--source cache` reads the raw
   `block_results` copies on the `cosmos-hub4-archive-cache` PVC (mount it
   with volumeMount `readOnly`, never a readOnly claim); `--source rpc` reads
   our node for heights the cache does not hold. Reruns skip finished units.
3. For each day a community tax proposal passed (88 at 13,315,659 and 996 at
   25,738,453), run `extract.py` again from the day's first height to the tally
   height into `split.jsonl`. The tally block still mints at the old rate; the
   next block uses the new one (`find_tally.py` finds the height).
4. `merge.py --bounds bounds.json --split split.jsonl --out merged.json
   cache.jsonl rpc.jsonl ...` checks that every height is counted once, no
   block is missing, unreadable or without a mint, each day's block count
   matches its bounds, and each day's supply change equals its mint (the
   report lists every day that differs, which are burns). Then
   `python3 -c "import merge; merge.export('merged.json', 'inflation_daily.json', AS_OF)"`.

Supply at a block is `annual_provisions / inflation` from the mint event,
which is exact because the chain computes provisions from the bank supply.
