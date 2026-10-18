# gen-banner

Generates `internal/banner/art.go` — the OFA banner as a baked ANSI string,
so the binary has no ascii.rest runtime and no network dependency.

## Why this exists

The OFA banner ("OFA" in the `tall` font, fading from `#e67a4c` orange to
`#4282d7` blue) is produced by [ascii.rest](https://ascii.rest). At runtime
we don't want any external calls; the rendered frame is baked into the Go
binary at build time.

## What's here

- `banner.ts`, `mount.ts`, `types.ts` — local copies of the ascii.rest
  banner source (MIT). `npx ascii.rest add banner --dir .` refreshes them.
  They are committed so the binary builds without network access.
- `generate.mjs` — drives the local `banner()` function, renders the
  static frame cell-by-cell, and emits a Go file with one constant.
- `ascii.rest.LICENSE` — MIT notice covering the copied sources. Required
  by ascii.rest's licence.

## Regenerating `art.go`

```sh
node scripts/gen-banner/generate.mjs > internal/banner/art.go
```

The generator never makes network calls. It imports `banner.ts` from the
same directory, calls `banner("OFA", { font: "tall", color: [...], effect: "still" })`,
walks the resulting frame, and writes a Go source file with the rendered
string as a package-level constant.

To change the look (different text, different font, different colors),
edit `generate.mjs` and re-run. Then re-build.

To refresh from upstream ascii.rest:

```sh
npx ascii.rest add banner --dir scripts/gen-banner --overwrite
```

The `--overwrite` flag replaces every file the item brings with it, so
commit your `generate.mjs` first if you've tweaked it.

## At build time

`internal/banner/art.go` is generated; do not edit it. `internal/banner/banner.go`
is the hand-written wrapper around it (`Render(w io.Writer) error`) with
`NO_COLOR` handling.