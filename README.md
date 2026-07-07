
<p align="center">
  <img src="https://i.ibb.co/MDLkCcrf/snowfast-v9-snowflake-white-darffk.jpg" alt="SnowFastULP" width="500" />
</p>

<h1 align="center">SnowFastULP</h1>

[![GitHub](https://img.shields.io/badge/GitHub-snowx--dev%2FSnowFastULP-181717?style=for-the-badge&logo=github&logoColor=white)](https://github.com/snowx-dev/SnowFastULP)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev/)
[![Platforms](https://img.shields.io/badge/Platforms-linux%20%7C%20macos%20%7C%20windows-2ea043?style=for-the-badge)](#try-it)
[![Docker](https://img.shields.io/badge/Docker-supported-2496ED?style=for-the-badge&logo=docker&logoColor=white)](https://snowfast.todo/docs)
[![CI](https://img.shields.io/github/actions/workflow/status/snowx-dev/SnowFastULP/ci.yml?branch=main&label=CI&style=for-the-badge&logo=githubactions&logoColor=white)](https://github.com/snowx-dev/SnowFastULP/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/snowx-dev/SnowFastULP?display_name=tag&sort=date&style=for-the-badge&logo=github&logoColor=white)](https://github.com/snowx-dev/SnowFastULP/releases/latest)

SnowFastULP cleans big ULP `.txt` dumps and logs fast, without filling your RAM.

Three commands:

- `sfu`: clean and deduplicate ULP/LPU `.txt` files.
- `sfs`: search plain text or `.zst` archives.
- `sfl`: pull ULP lines out of stealer log folders and archives, including the password-protected ones.

<p align="center">
  <img src="demo/sfu-complete.gif" alt="sfu cleaning a dump" width="700" />
</p>

## Try it

**Linux amd64 / macOS arm64:**

```bash
curl -fsSL https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.sh | bash
sfu ./dump.txt -o ./cleaned/
```

**Windows amd64 (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.ps1 | iex
sfu .\dump.txt -o .\cleaned\
```

After installing, open a new terminal so PATH is updated. Point `sfu` at a file or folder, keep the result somewhere useful. That is the whole first run.

```bash
sfu ./path-to-ulp-dumps/ -od ./library/          # clean + dedup into a searchable library
sfs ./library "facebook.com:"       # search .zst archives without decompressing
sfl ./path-to-logs/ -od ./library/  # extract ULP from folders and archives
```


Curious for more? Check out the online page and docs, take it to the next level.

<p align="center">
  <a href="https://snowfast.snowx.dev" target="_blank" rel="noopener">
    <img src="demo/button.png" alt="Link to online docs" width="300" />
  </a>
</p>



## Why use it

- **Build an antipublic.** `sfu -od ./library/` turns repeat log and ulp dumps into one compressed, deduped, searchable ulp archive. Later runs skip lines already in the library.
- **Search compressed archives.** `sfs ./library "facebook.com:"` queries `.zst` without decompressing first.
- **Stealer logs in, ULP out.** `sfl` walks extracted folders and passworded zip/rar/7z archives, recurses nested archives, and merges into your library.
- **Secret scanning built in.** Flag tokens and keys during extraction.
- **Low RAM, big inputs.** Two-pass disk bucketing keeps memory flat on huge dumps.
- **UNIX philosophy.** Focused, efficient tools that excel at one job each. No bloated all-in-one programs, just clean work.

<p align="center">
  <img src="demo/sfs-search.gif" alt="sfs searching a compressed library" width="700" />
</p>

Flags, config, build, FAQ, and the full `sfu` / `sfs` / `sfl` references [live in the docs](https://snowfast.snowx.dev).


## Shoutouts

- [vulnerose](https://t.me/aeryals) // Parser inspo
- [Prequel](https://eternally.blue) // Search inspo
- [lateralmovement](https://guns.lol/lateralmovement) // Cleaner inspo + data golbin
- Duckyhax // Beta testing
- [Logstester](https://t.me/logstester) com // Excellent peers & feedbacks

## License

SnowFastULP is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0). Copyright (C) 2026 Snow Dev. Use it only with data you are allowed to process. No warranty.



---
<p align="center">
  <a href="https://snowfast.snowx.dev" target="_blank" rel="noopener">
    <img src="demo/button.png" alt="Link to online docs" width="300" />
  </a>
</p>

