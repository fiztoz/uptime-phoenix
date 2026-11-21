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

1. **Duplicates**: rows with the same `observed_at` are counted once. The
   number dropped is reported as `duplicates`, so a repeated row can never
   raise completeness.
2. **Cadence** = median of the gaps that look like one interval
   (`gap ≤ 1.5 × configured`). Gaps from missed checks or outages are
   excluded, so they cannot stretch the cadence. With fewer than 3 such gaps
   the configured interval is used and the pair is marked
   `cadence_fallback: true`.
3. **Cadence cap**: the cadence used is at most `1.1 × configured` (22 s at
   20 s). When the median is above that, the pair is marked
   `cadence_capped: true` (the raw median stays in `median_gap_s`) and it is
   also charged for the time it lost: interior missing is at least
   `round((last − first) / cadence) − gaps`.
4. **Missing slots**: an interior gap `g` misses `round(g / cadence) − 1`
   slots, and **at least one** when `g > 1.5 × configured` (too long to feed
   the cadence). Every gap therefore either sets the cadence or is counted
   against it. Each window edge (start → first row, last row → end) misses
   `floor(edge / (1.05 × cadence))` slots.
5. **expected = seen + missing**, **completeness = seen / expected**.

A single missed heartbeat always counts as missing (in a 30-minute window at
about 20 s it fails 99.5 % on its own), and a perfect cadence scores 100 %
wherever the window starts. The threshold itself is unchanged.

### Why the cap is 1.1 × configured

Without a cap, any probe whose gaps all stay under 1.5 × configured scores
100 %: a probe checking every 28 s or 30 s at a 20 s interval (1.4× or 1.5×,
two-thirds of the configured rate) would pass. Measuring against the observed
cadence is meant to absorb scheduling drift, not to hide a slow probe.

- The drift seen in practice is about 20.25 s at a 20 s interval (1.0125×,
  measured pair spacing 20.07–20.54 s). A 10 % cap leaves about eight times
  that headroom, so real drift is never capped.
- Above the cap the probe pays for exactly the time it lost: completeness is
  roughly `1.1 × configured / median`. A probe 0.5 % beyond the cap fails
  99.5 %; at 1.4× it scores about 79 %.
- The cap only applies upward. A probe faster than configured is measured
  against its own faster cadence, which is stricter, never more lenient.
- Below the cap, pairs are judged gap by gap: a late beat under
  `1.5 × cadence` is late, not missing.

The alternative, documenting that a uniformly slow probe up to 1.5× passes,
was rejected because the evidence would then call a probe running at two
thirds of its configured rate complete.

## Evidence

`completeness.per_pair[]` has, for every monitor/probe pair:
`monitor_id`, `probe_id`, `configured_interval_s`, `observed_cadence_s`
(the cadence used, after the cap), `median_gap_s` (raw median, `null` on
fallback), `cadence_gaps_used`, `cadence_fallback`, `cadence_capped`, `seen`
(distinct rows), `duplicates`, `missing`, `expected`, `completeness`,
`max_gap_s`.

The aggregate keeps `min_completeness`, `mean_completeness`,
`pairs_below_threshold`, `pass`, the cadence median/min/max,
`pairs_cadence_fallback`, `pairs_cadence_capped`, total `duplicates`, the
rule constants (`rule`), and
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

`schema_version` is 2 (version 1 had no cap, duplicate handling or
`median_gap_s`).

## Tests

CI runs these in the `backend` job, and `make gate-full` runs them too.

```sh
make m6-load-analyzer-test
# or
python3 -B -m unittest discover -s scripts -p test_m6_load_analyzer.py -v
```
