# OFA — One For All

A single command to detect, version-check, and update every coding harness on your machine.

`ofa` knows about Claude Code, Codex, opencode, Kimi, Cursor's agent, GitHub Copilot, Antigravity (`agy`), Devin, CodeRabbit, and the rest of the popular CLI harnesses. It scans what's installed, asks each package manager / self-updater whether anything's available, and runs the update — interactive picker or one-shot.

```
$ ofa check
scan: 13 harnesses, 0 with updates, 8 updaters available
  agy          1.3.4          [self]    agy → updater available
  claude       2.1.300        [self]    claude → updater available
  coderabbit   0.7.6          [self]    coderabbit → updater available
  copilot      1.0.80         [self]    copilot → updater available
  cursor-agent 2026.10.14-... [self]    cursor-agent → updater available
  devin        3000.11.3      [self]    devin → updater available
  kimi         2.2.0          [self]    kimi → updater available
  mmx          1.0.25         [self]    mmx → updater available
```

Exit code is `1` if any install is **known to be outdated**, `0` otherwise — pipe it into your shell scripts. An available updater (a self-tier harness with no version to compare) is reported separately and deliberately does **not** make the exit code `1`, so `ofa check && …` fires when nothing is actually pending.

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

Go 1.26+ required.

### Verifying what you installed

Releases since `0.2.1` are built and published entirely by GitHub Actions,
using npm trusted publishing — the workflow authenticates over OIDC, so no
long-lived publish token exists anywhere in the pipeline. Each published
tarball carries a SLSA provenance statement naming the repository and workflow
that built it.

Confirm an attestation exists (no install needed — this follows whatever
`latest` points at):

```sh
$ npm view @khaentertainment/ofa dist.attestations
{
  url: 'https://registry.npmjs.org/-/npm/v1/attestations/@khaentertainment%2fofa@0.2.1',
  provenance: { predicateType: 'https://slsa.dev/provenance/v1' }
}
```

To actually verify the signatures, install it as a dependency of a project —
`npm audit signatures` deliberately refuses global installs (`EAUDITGLOBAL`):

```sh
$ mkdir verify && cd verify && npm init -y
$ npm install @khaentertainment/ofa
$ npm audit signatures

1 package has a verified registry signature

1 package has a verified attestation
```

The same attestation is browsable as **View provenance** on the
[package page](https://www.npmjs.com/package/@khaentertainment/ofa).

### One PATH caveat

`ofa` resolves to the **first** match on your `PATH`. If you built from source
into `~/.local/bin` and later installed via npm or Homebrew, the older local
binary keeps winning — and will keep reporting its own version:

```sh
$ which -a ofa            # every copy, in PATH order
$ ofa --version           # what you are actually running
```

Remove or update the stale copy to let the packaged binary take over. This is
the same shadowing OFA reports for other harnesses.

## Usage

### Interactive picker (default)

```
$ ofa
  ▓▓▓▓▓▓╗   ▓▓▓▓▓▓▓▓▓▓╗   ▓▓▓▓▓▓╗
▓▓╔═════▓▓╗ ▓▓╔═══════╝ ▓▓╔═════▓▓╗
▓▓║     ▓▓║ ▓▓║         ▓▓║     ▓▓║
▓▓║     ▓▓║ ▓▓▓▓▓▓▓▓╗   ▓▓▓▓▓▓▓▓▓▓║
▓▓║     ▓▓║ ▓▓╔═════╝   ▓▓╔═════▓▓║
▓▓║     ▓▓║ ▓▓║         ▓▓║     ▓▓║
╚═▓▓▓▓▓▓╔═╝ ▓▓║         ▓▓║     ▓▓║
  ╚═════╝   ╚═╝         ╚═╝     ╚═╝

> [ ] agy            1.3.4           agy → updater available
  [ ] claude         2.1.300         claude → updater available
  [ ] coderabbit     0.7.6           coderabbit → updater available
  ...

↑/↓ navigate · space toggle · a all · enter apply · q quit
```

The "OFA" logomark above is rendered in 24-bit color in a real terminal —
the letters fade from `#e67a4c` (orange) on the left to `#4282d7` (blue)
on the right. The banner is generated at build time by
[`scripts/gen-banner/`](./scripts/gen-banner) from
[ascii.rest](https://ascii.rest)'s `tall` font and baked into the binary
as a constant string, so the CLI has no ascii.rest runtime and no network
dependency. `NO_COLOR=1` strips the escapes for plain logs.

First run on a machine with no `~/.config/ofa/state.json` auto-scans first, so the picker has data to work with.

### Non-interactive

```sh
$ ofa check             # what's outdated? exit 1 if anything pending
$ ofa check --json      # jq-friendly
$ ofa check --refresh   # bypass cached latest versions (--refresh also on apply and `ofa`)
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
  "update_count": 0,
  "updater_available_count": 8,
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

Resolution is per-tier and runs concurrently: the single `brew outdated --json` call overlaps the per-package lookups instead of queueing behind them. `npm` queries the public registry's JSON API directly — one HTTP GET per package, no `npm view` process spawn — and falls back to `npm view <pkg> version` only when a custom registry is configured in `.npmrc`/env, where npm itself must apply the config. `pypi` resolves via the PyPI JSON API; `self` reports "updater available" without fabricating a version diff; `none` is report-only.

Resolved latest versions are cached in `state.json` for 12 hours, so a warm `ofa check` re-answers from cache instead of re-querying six registries. `--refresh` (accepted by `ofa`, `ofa check`, `ofa apply`) bypasses fresh entries, and when a refresh fails offline the stale cached answer is served with a warning on stderr — never a hard failure.

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

## Roadmap

Distribution is done: npm, the Homebrew tap, and tag-triggered releases all ship
(see [Install](#install)). What follows is tentative, roughly in priority order,
and uncommitted to any date.

### Medium-term: platform reach

- **Linux detector backends.** `apt`, `dnf`, `snap`, plus the existing npm/pipx/uv/native ones. The `Detector` interface is already the seam; new backends add a file, not a rewrite.
- **Windows (WSL) and native Windows.** WSL reuses the Linux backends; native Windows adds `scoop`, `winget`, and Windows registry probing for native installs.

### Long-term: agent-assisted Catalog (`ofa doctor`)

When a scan finds a binary on PATH that doesn't match any Catalog entry, or a Catalog entry fails to detect a known install, OFA needs help from a human-shaped intelligence to write the TOML. The design avoids any API key or rate-limit dependency on OFA's side:

1. OFA writes the facts it knows into a shared context folder under its config directory: `~/.config/ofa/doctor/CLAUDE.md` (always loaded by Claude Code), `~/.config/ofa/doctor/context.md` (per-invocation facts: which binary, its `--version` output, the gaps it fills).
2. OFA spawns a headless session of an installed agent — e.g. `claude -p "Read ~/.config/ofa/doctor/CLAUDE.md and ~/.config/ofa/doctor/context.md. Draft a Catalog entry."` — whichever harness the user has installed. No model API, no vendor lock-in; OFA is the orchestrator, the agent does the thinking.
3. The proposed TOML lands at `~/.config/ofa/doctor/history/<timestamp>/proposed-catalog.toml`. The user reviews it; if approved, copies it into the user Catalog (`~/.config/ofa/catalog/<id>.toml`) or upstream via PR for shared coverage.

Doctor is opt-in (`ofa doctor` only) and never runs in the default `ofa` path. Its directory is created on first use, not at install.

### Open ideas (not scheduled)

- **Update hooks.** Run a shell command before/after each update (`~/.config/ofa/hooks.toml`).
- **Version pinning.** Skip updates for harnesses the user wants frozen (`--pin <id>@<version>`).
- **Schedule.** Optional cron-style background scan + report (no auto-apply — OFA never applies without explicit user action).

## Contributing

The Catalog is the contribution surface. Adding a harness:

```sh
$ cp internal/catalog/data/agy.toml internal/catalog/data/your-harness.toml
$ # edit id, name, installs (method/bin/package/version/latest/update)
$ go test ./...
$ # open a PR
```

If `ofa doctor` (see [Roadmap](#roadmap)) finds a binary on your machine that doesn't match a catalog entry, it drafts a TOML for you to review and contribute upstream. Doctor is an opt-in command — not part of the default `ofa` path.

Cutting a release (version bump, tag, staged npm publish, Homebrew tap bump) is
documented in [`docs/RELEASING.md`](https://github.com/KHAEntertainment/ofa/blob/main/docs/RELEASING.md).

## License

MIT — see [LICENSE](./LICENSE).

## Acknowledgments

- The [bubbletea](https://github.com/charmbracelet/bubbletea) TUI framework (v2, imported as `charm.land/bubbletea/v2`) powers the interactive picker.
- Each catalog entry is a thin pointer at the actual upstream tool that does the work. OFA is glue, not a reimplementation.