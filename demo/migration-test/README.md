# v0.1 → dev migration visual test

Manual fixture for checking the **v2→v3 sidecar migration** TUI on a real ULP sample before release.

## What you get

- A **v0.1-format library** under `v1-library/` (full archive ingest, legacy **unsorted v2** `.idx` sidecars)
- Data from your archive at  
  `/run/media/bigboi/.../Data_Archive/ulp/raws/txt/`
- `scratch/migration-probe.txt` — symlink to the full `0623_ulp.txt` for the second dev run (re-ingest, mostly duplicates, triggers migration)

Built with **`sfu` v0.1** (from git tag), not the v0.1.1 release zip (that build already writes v3 sidecars).

## Prepare (or refresh) the fixture

```bash
./demo/migration-test/prepare.sh
```

Defaults: **full** `0623_ulp.txt` (~7.9M lines) + full June26 file (~3.2M lines), split at 250k unique lines per part (~45 archives). Expect several minutes of ingest time.

Quick smoke test (small subset):

```bash
SAMPLE_A_LINES=50000 SAMPLE_B_LINES=20000 SPLIT_A=15000 PROBE_LINES=500 ./demo/migration-test/prepare.sh
```

## Visual migration test (dev branch)

Build the dev binary first:

```bash
make build
```

**1. Confirm legacy indexes** (optional sanity check — should print only `2`):

```bash
python3 - <<'PY'
import struct, glob
for p in sorted(glob.glob("demo/migration-test/v1-library/sfu_dedup_idx/*.idx")):
    with open(p, "rb") as f:
        print(p.split("/")[-1], "v", struct.unpack("<H", f.read(6)[4:6])[0])
PY
```

**2. Run with TUI** (watch for *“legacy index detected”* then *“upgrading index format (v2→v3)”*):

```bash
./bin/sfu demo/migration-test/scratch/migration-probe.txt \
  -od demo/migration-test/v1-library/
```

**3. Re-run prepare.sh** if you need to test migration again — upgrade is one-time per sidecar.

## What to look for

| Phase | TUI hint |
|-------|----------|
| Discover | `scanning library · legacy index detected` |
| Upgrade | `upgrading index format (v2→v3)` with parts progress |
| Dedup | skips lines already in library |
| Summary | `Index format upgraded (one-time, N parts)` |

Archives (`.zst` bytes) should be untouched; only `sfu_dedup_idx/*.idx` change to v3.

## Verify after migration

```bash
python3 - <<'PY'
import struct, glob
for p in sorted(glob.glob("demo/migration-test/v1-library/sfu_dedup_idx/*.idx")):
    with open(p, "rb") as f:
        ver = struct.unpack("<H", f.read(6)[4:6])[0]
    print(p.split("/")[-1], "v", ver)
PY
```

All sidecars should report **v3**. A second dev run should **not** show the upgrade phase.

## Notes

- `scratch/` and `v1-library/` contain real credential data — **not committed** (see `.gitignore`).
- Use `-no-tui` + `-debug` for plain-text `[od] upgraded …` events instead of the TUI.
