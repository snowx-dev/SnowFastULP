
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

SnowFastULP cleans big ULP `.txt` dumps fast, without filling your RAM.

Three commands:

- `sfu`: clean and deduplicate ULP/LPU `.txt` files.
- `sfs`: search plain text or `.zst` archives.
- `sfl`: pull ULP lines out of stealer-log folders and archives.

<p align="center">
  <img src="demo/sfu-dedup.gif" alt="sfu cleaning a dump" width="700" />
</p>

## Try it

```bash
curl -fsSL https://raw.githubusercontent.com/snowx-dev/SnowFastULP/main/scripts/install.sh | bash
sfu ./dump.txt -o ./cleaned/
```

Point `sfu` at a file or folder, keep the result somewhere useful. That is the whole first run.

## Why people keep it around

- **Build an antipublic.** `sfu -od ./library/` turns repeat dumps into one compressed, deduped, searchable archive. Later runs skip lines already in the library.
- **Search compressed archives.** `sfs ./library "facebook.com:"` queries `.zst` without decompressing first.
- **Stealer logs in, ULP out.** `sfl` walks extracted folders and passworded zip/rar/7z archives, recurses nested archives, and merges into your library.
- **Secret scanning built in.** Flag tokens and keys during extraction.
- **Low RAM, big inputs.** Two-pass disk bucketing keeps memory flat on huge dumps.

<p align="center">
  <img src="demo/sfs-search.gif" alt="sfs searching a compressed library" width="700" />
</p>

Flags, config, build, FAQ, and the full `sfu` / `sfs` / `sfl` references live in the docs:

→ **https://snowfast.todo/docs**

## Shoutouts

- [vulnerose](https://t.me/aeryals) // Parser inspo
- [Prequel](https://eternally.blue) // Search inspo
- [lateralmovement](https://guns.lol/lateralmovement) // Cleaner inspo + data golbin
- Duckyhax // Beta testing

## License

SnowFastULP is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0). Copyright (C) 2026 Snow Dev. Use it only with data you are allowed to process. No warranty.
