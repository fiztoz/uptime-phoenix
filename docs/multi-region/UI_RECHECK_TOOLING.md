# UI recheck tooling (#223: #194, #196, #198)

`scripts/ui_recheck.py` rechecks the three #223 UI gaps on a running hub. It
reads only: API responses, DOM attributes, and SELECT output. It changes
nothing and needs no product hooks. Its offline tests run in
`make m6-backend-harness-gate`.

Exit status: `0` all checks pass, `1` mismatches (printed as JSON
`problems`), `2` bad input.

## #194 Overall Health Timeline widths

The strip lays overall intervals on a time axis. The tool recomputes each
segment's duration from the API alone (start to the next start, the newest to
now, clipped to the window) and compares it with what the strip rendered.

1. Save the API views (session cookie as for any API call):
   - `GET /api/monitors/<id>/heartbeats?hours=24&order=asc&limit=10000` → `rows.json`
     (the full stream; the strip shows the newest 60)
   - `GET /api/monitors/<id>/heartbeats/chart?hours=24` → `chart.json` (optional)
   - `GET /api/monitors/<id>` → `monitor.json` (optional; `created_at` bounds the stream)
2. On the monitor detail page, capture the strip (browser console or Playwright
   `page.evaluate`) and save the result as `dom.json`:

   ```js
   (() => {
     const bar = document.querySelector('[data-testid="recent-check-timeline"]');
     const px = (el) => el.getBoundingClientRect().width;
     const lead = bar.querySelector('[data-testid="recent-check-not-monitored"]');
     return JSON.stringify({
       now_ms: Date.now(),
       width_px: px(bar),
       lead: lead ? { duration_ms: +lead.dataset.durationMs, weight: +lead.dataset.weight, width_px: px(lead) } : null,
       segments: [...bar.querySelectorAll('[data-start]')].map((el) => ({
         status: el.dataset.status, start: el.dataset.start,
         duration_ms: +el.dataset.durationMs, weight: +el.dataset.weight, width_px: px(el),
       })),
     });
   })();
   ```

   Capture the strip and the API within a few seconds of each other. The
   newest segment keeps growing, so `--tol-ms` (default 1000) covers the gap.
   If the bar has padding or gaps, compare weights and leave `width_px` out.
3. Run `python3 scripts/ui_recheck.py timeline --api rows.json --dom dom.json --chart chart.json --monitor monitor.json`.

It checks:

- the segment count and order;
- each status and start;
- each duration and weight, and pixel width within `--tol-px`;
- the not-monitored lead, and that the weights sum to 1;
- that the API stream does not start before `created_at`;
- that no two neighbouring API rows have the same status;
- that the chart's down/pending and unknown runs match the merged rows.

## #198 Insights outage baseline

An outage is a contiguous run of DOWN/PENDING intervals that reaches DOWN and
began inside the window. An UP, UNKNOWN or MAINTENANCE interval, or a hole in
the history, ends the run.

```sh
python3 scripts/ui_recheck.py sql baseline --from 2026-11-08T12:00:00Z --to 2026-11-09T12:00:00Z \
  | mariadb -B <db> > hist.tsv
curl ... '/api/insights?period=24h' > insights.json
python3 scripts/ui_recheck.py baseline --tsv hist.tsv --insights insights.json
```

The output gives these per monitor:

- `outage_count`, `outage_starts` and `downtime_seconds`;
- the status `transitions` inside the window;
- any `problems` where Insights disagrees.

The window defaults to the response's `from`/`to`. Use the same window for
the SQL.

## #196 projection lag

`python3 scripts/ui_recheck.py sql lag | mariadb -t <db>` prints three result
sets (MariaDB syntax):

- the dirty-bucket backlog per resolution, with its oldest bucket and that
  bucket's age;
- the 20 monitors furthest behind on the `overall` resolution;
- the history head per monitor.

The oldest `overall` bucket is the projection lag. The older recheck query
used a `bucket_start` column, but `monitor_health_history` has no such
column. An open row (`ended_at` NULL) is the current state, so an old head
with `open_rows = 1` is not lag on its own. For the `1h` and `1d`
resolutions, the age is measured from the bucket start, so it overstates the
lag by up to one bucket width.
