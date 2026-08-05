# rdprun

[![Downloads (Total)](https://img.shields.io/github/downloads/mitiya/rdpRun/total)](https://github.com/mitiya/rdpRun/releases)
[![Downloads (v0.1.2)](https://img.shields.io/github/downloads/mitiya/rdpRun/v0.1.2/total)](https://github.com/mitiya/rdpRun/releases/tag/v0.1.2)
[![rdprun.exe](https://img.shields.io/badge/rdprun.exe-download-blue)](https://github.com/mitiya/rdpRun/releases/download/v0.1.2/rdprun.exe)
[![rdprun-linux-amd64](https://img.shields.io/badge/rdprun--linux--amd64-download-blue)](https://github.com/mitiya/rdpRun/releases/download/v0.1.2/rdprun-linux-amd64)

Run a command on a remote Windows host over RDP and optionally capture text output through the clipboard channel.

`rdprun` opens an RDP session, starts a shell (`cmd` or `powershell`) via Win+R, executes your command, and can return command output to stdout.

## Features

- RDP login with NLA/standard/auto auth modes
- Keyboard macro flow (Win+R -> launch shell -> run command)
- Optional output capture via clipboard (`--capture`)
- Visual UAC detection using an embedded, language-independent image reference and one `Alt+Y` fallback after the timeout
- Verified shell launch: uses `Esc`, a best-effort `Win+D`, then checks for the embedded lower-left Run dialog before typing
- Cross-build for Windows and Linux (`CGO_ENABLED=0`)
- Bundled local `third_party/grdp` copy patched for cgo-free builds

## Build

### Windows (and Linux cross-build from Windows)

```bat
build.cmd
```

Artifacts:

- `rdprun.exe` (Windows amd64)
- `rdprun-linux-amd64` (Linux amd64)

### Go build

```bash
go build -o rdprun.exe .
```

## Usage

```text
rdprun --server host:port --user USER --pass PASS --cmd "command" [options]
rdprun  host:port  USER  PASS  "command"  [options]
```

Examples:

```bash
rdprun --server srv:3389 --user joe --pass pw --cmd "whoami" --capture
rdprun --server srv:3389 --user LAB\\joe --pass pw --cmd "ipconfig /all" --shell cmd --capture
rdprun --server srv:3389 --user joe --pass pw --cmd "Get-Process" --shell powershell --capture
```

## Common options

- `--capture` - capture output via clipboard and print to stdout
- `--shell cmd|powershell` - shell to start
- `--auth nla|standard|auto` - security negotiation mode
- `--timeout` - capture timeout
- `--uac` / `--uac-timeout` - detect a protected-desktop UAC dialog from RDP frames and send `Alt+Y`; if none is confirmed before the timeout, send one fallback `Alt+Y`. Set `--uac-timeout=0` to disable both detection and fallback.
- `--uac-template path.png` - override the embedded UAC screenshot reference. The comparison is structural and does not read UI text, so it is independent of the Windows language.
- `--launch-timeout` - how long to wait for Run dialog verification on each launch attempt (default: `3s`)
- `--launch-retries` - additional Run dialog launch attempts after the first (default: `2`). If all attempts fail, `rdprun` stops before entering the shell or command into an unknown window.
- `--debug` - save diagnostic screenshots
- `--verbose` - verbose protocol logs

Run help:

```bash
rdprun --help
```

## Security notes

- Credentials are passed via CLI args; on multi-user systems, prefer isolated runners.
- For production automation, use dedicated low-privilege accounts.
- Output capture relies on clipboard redirection and may be limited by target policy.

## Project structure

- `main.go`, `config.go` - CLI and config parsing
- `connect.go` - RDP stack assembly and session logic
- `capture.go`, `input.go`, `uac.go` - automation and capture helpers
- `cliprdr/` - portable text-only clipboard channel implementation
- `third_party/grdp/` - vendored RDP protocol library

## License

The project uses a vendored copy of `tomatome/grdp` under its original license in `third_party/grdp/LICENSE`.
