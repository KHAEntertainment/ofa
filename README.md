# OFA — One For All

A single command to detect, version-check, and update every coding harness on your machine.

`ofa` knows about Claude Code, Codex, opencode, Kimi, Cursor's agent, GitHub Copilot, Antigravity (`agy`), Devin, CodeRabbit, and the rest of the popular CLI harnesses. It scans what's installed, asks each package manager / self-updater whether anything's available, and runs the update — interactive picker or one-shot.

```
$ ofa check
Scan: 13 harnesses, 8 with updates
  agy          1.3.4          [self]    agy → updater available
  claude       2.1.300        [self]    claude → updater available
  coderabbit   0.7.6          [self]    coderabbit → updater available
  copilot      1.0.80         [self]    copilot → updater available
  cursor-agent 2026.10.14-... [self]    cursor-agent → updater available
  devin        3000.11.3      [self]    devin → updater available
  kimi         2.2.0          [self]    kimi → updater available
  mmx          1.0.25         [self]    mmx → updater available
```

Exit code is `1` if anything's pending, `0` otherwise — pipe it into your shell scripts.

## Why

Every coding harness has its own update story. Claude Code has `claude update`. Codex is `brew upgrade --cask codex`. opencode lives in three places simultaneously — npm, a brew tap, and a beta npm package — and only one is on your PATH. Tracking which is current, and which is shadowed, is friction OFA absorbs.

OFA never reimplements an updater. It runs the native one (`brew upgrade`, `npm i -g <pkg>@latest`, `claude update`, `agy update`, etc.) and reports the outcome.

## Install

### macOS (Homebrew)

```sh
$ brew install KHAEntertainment/tap/ofa
```

### npm (any platform)

```sh
$ npm install -g @khaentertainment/ofa
```

### From source

```sh
$ git clone https://github.com/KHAEntertainment/ofa
$ cd ofa && go build -o ~/.local/bin/ofa ./cmd/ofa
```

Go 1.25+ required.

## Usage

### Interactive picker (default)

```
$ ofa
ofa — select updates to apply

> [ ] agy            1.3.4           agy → updater available
  [ ] claude         2.1.300         claude → updater available
  [ ] coderabbit     0.7.6           coderabbit → updater available
  ...

↑/↓ navigate · space toggle · a all · enter apply · q quit
```

First run on a machine with no `~/.config/ofa/state.json` auto-scans first, so the picker has data to work with.

### Non-interactive

```sh
$ ofa check             # what's outdated? exit 1 if anything pending
$ ofa check --json      # jq-friendly
$ ofa apply --dry-run   # show the plan without running
$ ofa apply --yes       # run all available updates
$ ofa apply --only pi   # narrow to one harness
$ ofa apply --verbose   # stream each installer's full output (default: progress lines)
$ ofa scan              # detect + write state, report drift since last scan
```

`ofa check --json` is the scriptable variant:

```json
{
  "scanned_at": "2026-10-15T20:31:42Z",
  "harness_count": 13,
  "update_count": 8,
  "harnesses": {
    "claude": {
      "name": "Claude Code",
      "installs": [
        {"version": "2.1.300", "tier": "self", "outdated": false, "updater_available": true}
      ]
    }
  }
}
```

## How it works

```
catalog.Embedded()  →  detect.Scan()  →  resolve.Resolve()  →  picker / CLI
                                              ↓
                                       update.Apply()  →  DefaultExecutor
```

Two-layer registry:

1. **Catalog** — a directory of TOML files, one per harness, embedded into the binary at build time. Adding a new harness is adding a file.
2. **State** — `~/.config/ofa/state.json`, the result of the last scan on this machine, used to compute drift (added / removed / changed).

Detection is deterministic. Each install method (`brew-formula`, `brew-cask`, `npm`, `pipx`, `uv`, `native`) has its own backend behind a `Detector` interface; new methods add a backend, not new code in the resolver.

Resolution is per-tier. `brew` resolves via `brew outdated --json`; `npm` via `npm view <pkg> version`; `pypi` (covers `pipx` and `uv`) via the PyPI JSON API; `self` reports "updater available" without fabricating a version diff; `none` is report-only.

Updates are delegated to native updaters. `ofa apply` never reimplements an update — it runs `brew upgrade <name>`, `npm install -g <pkg>@latest`, or `<bin> update` and reports the outcome.

## The shadow problem

`opencode`, `qwen`, and `kimi` can each be installed multiple ways on the same machine (e.g., opencode via npm *and* via the `anomalyco/tap` brew formula, both writing to `/opt/homebrew/bin`). `which opencode` answers one of them — the wrong one is the one you didn't update. OFA enumerates every install and marks the active one (the `which`-winner) versus the shadowed ones:

```
$ ofa check
  opencode     1.18.35       [npm]  opencode → 1.19.1        (active, stale)
  opencode     1.19.0        [brew] opencode (at latest)      (shadowed)
```

## Supported harnesses

| Harness                | Install methods                              | Update path |
|------------------------|----------------------------------------------|-------------|
| Claude Code            | native                                       | `claude update` |
| Codex                  | brew cask                                    | `brew upgrade --cask codex` |
| Cursor Agent           | native                                       | `cursor-agent update` |
| Devin                  | native                                       | `devin update` |
| GitHub Copilot CLI     | npm                                          | `copilot update` |
| Kimi Code              | uv, native                                    | `uv tool upgrade kimi-cli`, `kimi upgrade --yes` |
| Kilo Code              | npm                                          | `npm i -g @kilocode/cli@latest` |
| MiniMax (`mmx`)        | npm                                          | `mmx update` |
| opencode               | npm, brew formula, npm (beta)                | `npm i -g ...`, `brew upgrade opencode` |
| Pi Coding Agent        | npm                                          | `npm i -g @earendil-works/pi-coding-agent@latest` |
| Qwen Code              | npm, brew formula                            | `npm i -g ...`, `brew upgrade qwen-code` |
| Antigravity (`agy`)    | native                                       | `agy update` |
| CodeRabbit CLI         | native                                       | `coderabbit update` |

Adding a harness is a PR with a TOML file. See [`internal/catalog/data/agy.toml`](./internal/catalog/data/agy.toml) for the simplest example.

## Platform

v1 is macOS-only. The seam for cross-platform is the `Detector` interface — Windows (WSL) and Linux are additive, not a rewrite. The `update.cmd` field is data, not code, so a Linux update path is a new field on the same catalog entry.

## Contributing

The Catalog is the contribution surface. Adding a harness:

```sh
$ cp internal/catalog/data/agy.toml internal/catalog/data/your-harness.toml
$ # edit id, name, installs (method/bin/package/version/latest/update)
$ go test ./...
$ # open a PR
```

If `ofa doctor` finds a binary on your machine that doesn't match a catalog entry, it drafts a TOML for you to review and contribute upstream. (Doctor is an opt-in command — not part of the default `ofa` path.)

## License

MIT — see [LICENSE](./LICENSE).

## Acknowledgments

- The [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea) TUI framework powers the interactive picker.
- Each catalog entry is a thin pointer at the actual upstream tool that does the work. OFA is glue, not a reimplementation.