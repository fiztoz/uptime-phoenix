# M6 load-window analyzer

`scripts/m6_load_analyzer.py` turns an export of probe observations into one
evidence JSON for a measurement window (for example the `single-probe-100` or
`ten-probe-1000` load windows). Standard library only, offline, no database or
network access: you export the rows, the analyzer reads the file.

## Completeness rule

The heartbeat-completeness threshold (≥ 99.5 % per monitor/probe pair) is
measured against the **observed check cadence**, not the configured interval.
Probes schedule checks slightly slower than configured (about 20.25 s at a
20 s interval), so dividing the window by the configured interval reports
missing heartbeats that never were due.

Per (monitor, probe) pair inside `[start, end)`:

1. **Cadence** = median of the gaps that look like one interval
   (`0 < gap ≤ 1.5 × configured`). Gaps from missed checks or outages are
   excluded, so they cannot stretch the cadence. With fewer than 3 such gaps
   the configured interval is used and the pair is marked
   `cadence_fallback: true`.
2. **Missing slots**: an interior gap `g` misses `round(g / cadence) − 1`
   slots; each window edge (start → first row, last row → end) misses
   `floor(edge / (1.05 × cadence))` slots.
3. **expected = seen + missing**, **completeness = seen / expected**.

A single missed heartbeat always counts as missing (in a 30-minute window at
about 20 s it fails 99.5 % on its own), and a perfect cadence scores 100 %
wherever the window starts. The threshold itself is unchanged.

## Evidence

`completeness.per_pair[]` has, for every monitor/probe pair:
`monitor_id`, `probe_id`, `configured_interval_s`, `observed_cadence_s`,
`cadence_gaps_used`, `cadence_fallback`, `seen`, `missing`, `expected`,
`completeness`, `max_gap_s`.

The aggregate keeps `min_completeness`, `mean_completeness`,
`pairs_below_threshold`, `pass`, the cadence median/min/max, and
`legacy_configured_interval` (window ÷ configured interval) for comparison.
The file also records ingest delay percentiles (`received_at − observed_at`),
the largest gap, pairs with a gap over 2 × configured, and duplicate
(monitor, probe, observed_at) rows.

## Usage

Export the observations (hub database, UTC session):

```sql
SET time_zone = '+00:00';
SELECT monitor_id, probe_id,
       UNIX_TIMESTAMP(observed_at), UNIX_TIMESTAMP(received_at)
FROM probe_observations
WHERE observed_at >= FROM_UNIXTIME(<start>) AND observed_at < FROM_UNIXTIME(<end>);
```

For example with `mariadb --batch --skip-column-names … > observations.tsv`,
using your own client options file for credentials. Then:

```sh
python3 -B scripts/m6_load_analyzer.py \
  --observations observations.tsv \
  --start <epoch> --end <epoch> \
  --configured-interval 20 \
  --label ten-probe-1000 \
  --out evidence.json
```

`--probes id1,id2` limits the analysis to some probes. The exit status is 0
when every pair meets the threshold and 1 otherwise; the summary line goes to
stderr.

## Tests

```sh
make m6-load-analyzer-test
# or
python3 -B -m unittest discover -s scripts -p test_m6_load_analyzer.py -v
```
