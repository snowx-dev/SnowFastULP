# Changelog

All notable changes to SnowFastMerge are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### sfs — streaming is the default
- `sfs` now streams hits to **stdout by default**. The previous default (auto
  `sfs_results_*.txt` file + live TUI) is now behind **`-stats`**.
- `-o FILE` without `-stats` **tees** hits to stdout **and** the file. Previously
  `-o` was file-only. Use `-stats -o FILE` (or `-stats` alone) for file-only output.
- Legacy `-s` / `-silent` / `[sfs].stream` / `[sfs].silent` are accepted as
  **no-ops** for parse-compat; they have no effect on mode. `-stats` is the only
  mode switch.

### sfs — `-sec` interactions
- `-sec -o FILE` is **file-only** (no stdout tee), so secrets never leak to
  stdout when an output file is named.
- `[sfs].stats` / `[sfs].txt` from config no longer hard-reject `-sec` unless
  `-stats` / `-txt` is explicitly **enabled** on the CLI. Config-derived values
  are the user's baseline, not an explicit ask for the run.
- `-sec` warnings for ignored `-j` / `-decode-step` / `-max-hits-per-chunk` now
  fire only when the flag is passed on the CLI, not when the value comes from
  config.

### Path UX — config relatives resolve against CWD
- Relative paths in `config.toml` (`[sfu]`, `[sfs]`, `[sfl]` sections) now resolve
  against the **process CWD**, the same as CLI flags — not the config file
  directory. `~` and absolute paths are unchanged.
- `sfu` / `sfl` `-od` / `-odr` are always treated as directories: `MkdirAll`
  creates missing parents on non-dry-run, and an existing **file** at the path
  is rejected with a friendly error instead of failing mid-run with `ENOTDIR`.
  `-o` keeps its dir-hint guard (trailing separator or existing directory) so
  `-o cleaned.txt` is rejected.
- `-odr` (dry-run preview) now runs the same file-exists guard as `-od`, closing
  a hole where `-odr` against a file silently treated it as an empty library.

### Internal
- New shared `internal/outdir` package (`ResolveDir`, `IsDirHint`, `EnsureReady`)
  used by both `sfu` and `sfl` for output-dir validation and readiness.
- New `internal/pathdisp` package (`ForDisplay`) for CWD-relative display paths
  in sfu/sfs/sfl footers.
- Removed unused `config.File.BaseDir()` / `baseDir` field.
