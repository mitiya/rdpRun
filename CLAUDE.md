# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`rdprun` is a headless RDP client that logs into a remote Windows host, opens a shell through the Win+R Run dialog, types a command, and optionally captures its output back through the clipboard channel. There is no interactive UI — the whole session is a scripted keyboard macro driven by watching the incoming RDP framebuffer.

## Build and test

```bash
go build -o rdprun.exe .        # single Windows binary
build.cmd                       # both rdprun.exe and rdprun-linux-amd64
go test ./...                   # all tests
go test -run TestWatchUAC       # one test (tests live in uac_test.go)
```

Builds are always `CGO_ENABLED=0`. `go.mod` has a `replace` directive pointing `github.com/tomatome/grdp` at the vendored `third_party/grdp/`, whose only change from upstream is removing a vestigial `import "C"` so the project cross-compiles to Linux without a C toolchain. Don't add a dependency that reintroduces cgo.

## Architecture

The run is a fixed sequence in `main.go`, and each step waits on visual confirmation from the framebuffer rather than fixed sleeps alone:

1. `newSession` (`connect.go`) assembles the grdp protocol stack by hand (tpkt → x224 → mcs → sec → pdu + channels) instead of using grdp's high-level client wrapper. This is deliberate: the manual stack keeps direct access to the PDU layer (for scancode/unicode input events) and the clipboard channel, both of which the wrapper hides. Blocks until the PDU `ready` event fires.
2. Preflight: `Esc` then best-effort `Win+D` to clear focus.
3. Open Run dialog with `Win+R`, then **verify** it before typing (`watchRunDialog`). Retries `Win+R` until `--desktop-timeout` expires. Never types until the Run dialog is positively identified — otherwise launcher text lands in whatever window had focus.
4. Type the launcher (`cmd`/`powershell`), Enter, wait for shell readiness (PowerShell needs a longer settle so its startup banner doesn't eat the leading characters of the command).
5. Type the command (wrapped to pipe output to the clipboard when `--capture`), then Enter.
6. UAC handling (`watchUAC`): watch for the protected-desktop transition + centered dialog, send `Alt+Y`. One fallback `Alt+Y` if nothing is confirmed before `--uac-timeout` (set `--uac-timeout=0` to disable both).
7. Capture (`capture.go`): poll the remote clipboard with `CB_FORMAT_DATA_REQUEST` for `CF_UNICODETEXT`. It polls directly rather than waiting for a server `CB_FORMAT_LIST`, because some servers don't announce clipboard changes unless a local viewer is registered.

### Visual detection (`uac.go`) — the non-obvious core

All screen-state decisions run off `bitmapAccumulator`, which composites incoming RDP bitmap tiles into a full BGRX frame buffer. The `update` PDU handler feeds it; every other module reads it.

- Detection is **structural, not OCR** — it never reads UI text, so it is independent of the Windows display language. Two PNG references are embedded via `//go:embed` from `assets/`: `uac-reference.png` (centered UAC dialog) and `run-dialog-reference.png` (lower-left Run dialog). A user PNG can override the UAC one with `--uac-template`.
- Matching is normalized cross-correlation: a reference is downsampled to a fixed 32×24 grid, mean/stddev-normalized, and correlated against a candidate region. `templateSimilarityNear` searches a small offset window around the expected location. The UAC template is checked at screen center; the Run dialog at a fixed lower-left origin scaled from 1024×768.
- `isUACFrame` is a coarse geometric prefilter (global brightness/blue shift + a brighter-center-than-edges signature) used alongside template matching.
- A match must persist across **two distinct framebuffer revisions** (`revision` counter) before it counts, to reject transient frames. `stats()` ignores pixels whose alpha is 0 (never-received tiles) so partial updates don't skew brightness.

If you change detection thresholds, note the knobs are surfaced as flags: `--run-dialog-threshold` (default 0.72) and the UAC score constant `uacTemplateScore`. `uac_test.go` synthesizes frames with `setFrame` and asserts the watchers — run it after any change here.

### Input (`input.go`)

Text is always sent as **Unicode key events** (`typeString`/`typeRune`), which insert the literal code point regardless of the remote keyboard layout, so no Shift handling is needed. Scancodes (`connect.go` `keyDown`/`keyUp`) are used only for keys with no Unicode form: the Win/Alt modifiers and Enter/Esc/Backspace. Runes outside the BMP go out as UTF-16 surrogate pairs.

### CLI parsing (`config.go`)

Go's `flag` package stops at the first non-flag argument, which breaks the positional form `rdprun host user pass "cmd" --capture`. `reorderFlags` works around this by moving all flag tokens ahead of positional args before parsing (respecting `--` and single `-`). If you add a flag that takes a value, also add it to the `flagsWithValues` map in `reorderFlags`, or its value will be misparsed as a positional argument.

## Conventions

- `--debug` saves numbered `shot_NN_*.png` frames at each step and prints brightness/coverage/similarity diagnostics; `--verbose` turns on grdp library logging. The library is silenced by default (it emits harmless capability warnings).
- Timing is tunable via `--key-delay` and `--step-delay`; hardware and network latency vary, so these are left as knobs rather than hardcoded.
