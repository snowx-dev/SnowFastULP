
<p align="center">
  <img src="https://i.ibb.co/MDLkCcrf/snowfast-v9-snowflake-white-darffk.jpg" alt="Description" width="500" />
</p>

<h1 align="center">SnowFastULP</h1>   



[![GitHub](https://img.shields.io/badge/GitHub-snowx--dev%2FSnowFastULP-181717?style=for-the-badge&logo=github&logoColor=white)](https://github.com/snowx-dev/SnowFastULP)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev/)
[![Platforms](https://img.shields.io/badge/Platforms-linux%20%7C%20macos%20%7C%20windows-2ea043?style=for-the-badge)](#install)
[![Docker](https://img.shields.io/badge/Docker-supported-2496ED?style=for-the-badge&logo=docker&logoColor=white)](#build-from-source)
[![CI](https://img.shields.io/github/actions/workflow/status/snowx-dev/SnowFastULP/ci.yml?branch=main&label=CI&style=for-the-badge&logo=githubactions&logoColor=white)](https://github.com/snowx-dev/SnowFastULP/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/snowx-dev/SnowFastULP?display_name=tag&sort=date&style=for-the-badge&logo=github&logoColor=white)](https://github.com/snowx-dev/SnowFastULP/releases/latest)

**SnowFastULP cleans small to huge ULP txt dumps fast, without filling up your RAM.**  

  
It ships with three small commands:  

- `sfu` cleans ULP/LPU `.txt` files, removes duplicates, and writes clean output.
- `sfs` searches plain `.txt` files or compressed `.zst` archives.
- `sfl` extracts stealer logs into ULP lines, from extracted folders or archives.
  
➡️ **Basically**: download `sfu`, point it at a file or folder, and keep the cleaned result somewhere useful. If you want to search, grab `sfs` too. If you start from raw stealer logs, use `sfl`.  
  
💡 `sfu` stands for **S**now**F**ast**U**LP, `sfs` stands for **S**now**F**ast**S**earch, and `sfl` stands for **S**now**F**ast**L**og.  


---


## Contents

- [Quick start](#-quick-start)
- [Common flags](#common-flags)
- [Library mode](#library-mode)
- [Extracting stealer logs with `sfl`](#extracting-stealer-logs-with-sfl)
- [Searching with `sfs`](#searching-with-sfs)
- [Configuration](#configuration)
- [Install](#install)
- [Build from source](#build-from-source)
- [FAQ](#faq)
- [Shoutouts](#-shoutouts)

  ---

<p align="center">
  <img src="https://i.ibb.co/F4gJWc4K/screenshot-rocks3.png" alt="sfu dedup phase" width="700" />
</p>
<p align="center">sfu dedup phase</p>   

<p align="center">
  <img src="https://i.ibb.co/tTPj502K/screenshot-rocks.png" alt="sfu job summary" width="700" />
</p>
<p align="center">sfu job summary</p>   


## 🌱 Quick start

Install `sfu`, `sfs`, and `sfl` with the repo-hosted installer:

```bash
curl -fsSL https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.sh | bash
```

Or download the `SnowFastULP`, `SnowFastSearch`, and `SnowFastLog` binaries for your platform from the [latest GitHub Release](https://github.com/snowx-dev/SnowFastULP/releases/latest), rename them to `sfu`, `sfs`, and `sfl`, and put them somewhere convenient.

Then run:

```bash
sfu ./ulp-public-cloud.txt -o ./cleaned/
```

🔥 That's it 🔥

---

Clean a whole folder the same way:

```bash
sfu ./folder-full-of-dumps/ -o ./cleaned/
```


🔍 Search plain text output or raw text dumps with `sfs`:

```bash
sfs -txt ./cleaned/ "facebook.com:"
sfs -txt ./cleaned/ -o hits.txt "user@example.com"
```

🧊 Extract raw stealer logs with `sfl`:

```bash
sfl ./extracted-log-folder/ -o ./ulp/
sfl ./folder-of-archives/ -od ./library/ -p passwords.txt
```

Prefer building it yourself? See [Build from source](#build-from-source).



---



➡️ If you start cleaning dumps often, try the [library workflow](#library-mode):

```bash
./sfu ./weekly-dump/ -od ./library/
```

That one folder becomes a compressed archive library, but still being searchable. It's awesome 🔥    
Later runs using **the same folder** skips lines that are already there, and `sfs` can search it (without `-txt`):

```bash
./sfs ./library "facebook.com:"
```

That is enough for the first try. Use `sfu -h`, `sfs -h`, and `sfl -h` when you want the full flag list.



## What you get

- One clean output archive per `sfu` run
- Recursive folder scans for `.txt` input
- Deduplication
- Plain `.txt` output by default when you use `-o`
- Optional compressed library output when you use `-od`
- `sfs` search for both `.txt` dumps and `.zst` archives
- `sfl` extraction from stealer log folders, archive folders, and passworded zip/rar/7z archives
- Nice TUI by default, with plain output available for scripts

## Common flags

Most runs only need one or two of these:

| Flag       | Use it when                                                                                    |
| ---------- | ---------------------------------------------------------------------------------------------- |
| `-o DIR/`  | You want to choose where normal cleaned output goes. Start here.                               |
| `-od DIR/` | You want a reusable compressed library that dedups against past runs.                          |
| `-zst`     | You want compressed output without using library mode.                                         |
| `-no-uri`  | You want shorter `host:login:password` lines instead of full URLs.                             |
| `-no-tui`  | You prefer plain progress output, usually for scripts or narrow terminals.                     |
| `-del`     | You want input `.txt` files deleted after a successful run. Deletes *only* when run is done and successful. |


## Library mode

`-o` is the normal place to start. Use `-od` when you process new dumps often and want one growing archive:

```bash
./sfu ./new-stuff/ -od ./library/
```

The first run creates compressed `sfu_*.txt.zst` output in `./library/`. Later runs compare new lines against the older archives in that same folder and skip duplicates.  
    
You now have a production grade ULP library 🎉

**Rule of thumb:**

- Use `-o` for a one-off clean.
- Use `-od` for a library you plan to keep and search.

## Extracting stealer logs with `sfl`

`sfl` is for analyst workflows where the input is not already clean ULP. You can point it at:

- one extracted stealer log folder,
- a folder containing many extracted logs,
- a single archive,
- a folder containing archives.

Classic extraction writes ULP text:

```bash
./sfl ./logs/ -o ./ulp/
```

Library ingest extracts ULPs, then merges them into the library in-process using the same dedup and sidecar engine as `sfu -od`, so the result is identical to normal library mode (one cohesive TUI, no subprocess):

```bash
./sfl ./archives/ -od ./library/ -p passwords.txt
```

`-p` accepts either a literal archive password or a text file with one password per line. Password values are not printed in the summary.

Archives nested inside archives (a zip-of-zips, a `.7z` holding `.rar` files, etc.) are recursed automatically up to a small depth limit, each nested archive resolving its own password from the same wordlist. A nested archive that can't be opened (wrong password, corrupt) is reported as an issue in the summary and skipped — it never aborts the run.

## Searching with `sfs`

`sfs` searches either plain `.txt` files or compressed `.zst` archives.

Search plain text:

```bash
./sfs -txt ./cleaned "user@example.com"
```

Search compressed archives:

```bash
./sfs ./library "user@example.com"
```

Write hits to a file:

```bash
./sfs ./library -o hits.txt "facebook.com:"
```

Patterns are literal strings, not regular expressions. Each hit is printed as one matching ULP line. You can use `rg` for regex search.  

## Configuration

Configuration is optional. CLI flags always win.

`sfu`, `sfs`, and `sfl` can read a TOML config from:

| Source            | When used                                                                                     |
| ----------------- | --------------------------------------------------------------------------------------------- |
| `-config PATH`    | Explicit config file. Must exist.                                                             |
| `SNOWFAST_CONFIG` | Explicit config file from the environment. Must exist.                                        |
| Default path      | `~/.config/snowfast/config.toml` on Linux/macOS, `%AppData%\snowfast\config.toml` on Windows. |


Copy [`config.toml.example`](config.toml.example) as a starting point.
  
Relative paths in the config file are resolved from the config file's directory. A leading `~/` expands to your home directory.

## Install

### Install with curl or PowerShell

The installers resolve the latest version from the SnowFast update manifest at `https://sfu-update.snowx.dev/`, download `sfu`, `sfs`, and `sfl`, verify them against the hashes served from that controlled domain, install them into a user PATH folder, and create a fully commented config file if you do not already have one.

Linux amd64 and macOS arm64:
If the install directory is not already on PATH, it can update bash, zsh, or fish shell startup files.

```bash
curl -fsSL https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.sh | bash
```

Windows amd64:

```powershell
irm https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.ps1 | iex
```

Preview the scripts first if you want to inspect them:

```bash
curl -fsSL https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.sh | less
```

```powershell
irm https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.ps1 | more
```

For a Windows dry run:

```powershell
irm https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.ps1 -OutFile install.ps1
.\install.ps1 -DryRun
```

The installer prints a short summary at the end explaining what each command does, where it installed the binaries, and where it wrote or found the config file. Default config path:

- Linux/macOS: `~/.config/snowfast/config.toml` unless `XDG_CONFIG_HOME` is set.
- Windows: `%AppData%\snowfast\config.toml`.

Docs: `https://snowfast.todo/docs`

The same update manifest is used by `sfu update`, `sfs update`, `sfl update`, and the background update notice shown after successful runs. GitHub Releases can still host the binary files, but the expected version and hashes come from `sfu-update.snowx.dev`.

For installer testing, set `SNOWFAST_UPDATE_URL` to a local or staging manifest URL before running `install.sh --dry-run` or `install.ps1 -DryRun`.

### Download executables manually

Binaries for Linux, macOS, and Windows are published on the [Releases page](https://github.com/snowx-dev/SnowFastULP/releases). Each release is built reproducibly via GitHub Actions and ships a `SHA256SUMS` file you can verify against:

```bash
sha256sum -c SHA256SUMS
```

Builds are reproducible via GitHub Actions. Every release is built twice and verified to produce identical hashes before publishing.

### Install with Go

```bash
go install github.com/snowx-dev/SnowFastULP/cmd/sfu@latest
go install github.com/snowx-dev/SnowFastULP/cmd/sfs@latest
go install github.com/snowx-dev/SnowFastULP/cmd/sfl@latest
```

## Build from source

You need Go 1.25+ and/or Docker. From the repo root, run `make` or `make help` to see every target.

Common targets:

| Target                              | What you get                                                |
| ----------------------------------- | ----------------------------------------------------------- |
| `make build`                        | Build `sfu`, `sfs`, and `sfl` for your current OS/arch in `./bin/`. |
| `make build-sfu` / `make build-sfs` / `make build-sfl` | Build one binary only.                                      |
| `make build-all`                    | Cross-compile Linux amd64, macOS arm64, and Windows amd64.  |
| `make release-assets`               | Create flat release downloads in `./dist/`.                 |
| `make test`                         | Run unit tests with the race detector.                      |
| `make vet`                          | Run `go vet` and check `gofmt`.                             |
| `make clean`                        | Remove build artifacts.                                     |

Docker targets:

| Target                           | What you get                                             |
| -------------------------------- | -------------------------------------------------------- |
| `make docker-build`              | Runtime image (`sfu:local`) with `sfu`, `sfs`, and `sfl`. |
| `make docker-run ARGS='...'`     | Run `sfu` in a container.                                |
| `make docker-run-sfs ARGS='...'` | Run `sfs` in the same image.                             |
| `make docker-run-sfl ARGS='...'` | Run `sfl` in the same image.                             |
| `make docker-build-all`          | Build release binaries via Docker, no local Go required. |


## Under the hood

Big inputs are processed in two stages: first a fast pass sorts lines into buckets on disk, then a second pass removes duplicates and writes the final output.

You usually do not need to tune anything. `sfu` picks sensible settings from your machine unless you override them.

With `-od`, `sfu` also loads or rebuilds small index files beside older archives so new lines can be compared against the whole library without fully re-reading every old file.  

  
`sfu` keeps small helper indexes next to the library when using `-od` library mode:

- `sfu_dedup_idx/` helps `sfu` tell whether a line already exists in older output using hashes. These hashes are loaded into memory during cleaning, so when you start the deduping process, things go fast.
- `sfu_search_idx/` helps `sfs` search compressed archives faster. Based on Prequel's [Zindex](https://github.com/Pre-quel/Zindex) implem.

These sidecar folders are safe to leave alone. If you delete them, the tools rebuild what they need on the next run.


## FAQ

### 🔷 How do I search compressed outputs?

Use `sfs`:

```bash
./sfs ./library -o hits.txt "PATTERN-HERE"
```

Best results come from a library built with `sfu -od`.

`ripgrep` also works for quick one-offs:

```bash
rg -z -s --text --no-filename -N "PATTERN-HERE" ./library/
```

### 🔷 Should I use `-o` or `-od`?

Use `-o` for one-off output. You get a fresh `sfu_...` file each run, and new output is not compared to older runs.

Use `-od` when you keep adding dumps to the same library folder and want new lines deduped against everything already there. Compression is always on in library mode.

### 🔷 Will `sfu` overwrite my inputs or old outputs?

No, unless you ask it to. Inputs are not touched by default, and each run writes a new output file.

The exception is `-del`, which removes input `.txt` files after a successful run. Do not use it until you are comfortable with the workflow.

### 🔷 What can I point `sfu` at?

A single `.txt` file or a folder. Folders are scanned recursively for `.txt` files. Other extensions are ignored.

### 🔷 Why did `-o` reject my path?

`-o` expects a directory, not a filename. Use an existing folder or end a new folder path with `/` so `sfu` knows it should create a directory:

```bash
./sfu ./data/ -o ./cleaned/
```

### 🔷 What are the sidecar index folders?

`sfu_dedup_idx/` helps `sfu` dedup against older library archives. `sfu_search_idx/` helps `sfs` search compressed archives faster.

They are safe to keep. They are also safe to delete; the next relevant run rebuilds them.

### 🔷 I stopped a run halfway. Is anything broken?

Usually no. Pressing Ctrl+C once asks `sfu` to clean up temp folders and unfinished output from that run. Your input files are left alone.

Pressing Ctrl+C twice force-exits. Cleanup may be skipped, and `sfu` prints any paths you may want to remove manually.

### 🔷 Lines are missing or rejected

The default parser is strict on purpose. For messier dumps, try `-loose`. To inspect dropped lines, use `-debug-reject`.

### 🔷 macOS says the binary cannot be opened

The release binary is unsigned. If you downloaded it from a trusted source, run these commands in the folder where the binary lives:

```bash
chmod +x ./sfu
xattr -d com.apple.quarantine ./sfu
```

Then run:

```bash
./sfu --help
```

## 💞 Shoutouts

- [vulnerose](https://t.me/aeryals) // Parser inspo
- [Prequel](https://eternally.blue) // Search inspo
- [lateralmovement](https://guns.lol/lateralmovement) // Cleaner inspo + data golbin
- Duckyhax // Beta testing

## License and disclaimer

SnowFastULP is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0).

Copyright (C) 2026 Snow Dev.

Use it only with data you are allowed to process. The software is provided as-is, without warranty. See [LICENSE](LICENSE) for the full terms.
