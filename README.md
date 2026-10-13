# agent-session-compress

Coding agents keep every session on disk as JSONL transcripts and tool logs, and those directories grow without bound. `agent-session-compress` stores idle session files with your filesystem's built-in transparent compression. Every file keeps its path, bytes, and modification time, and the OS decompresses it on read, so Claude Code, Codex, Gemini CLI, opencode, Oh My Pi, and Pi still list, resume, and search old sessions with no change.

A copy of real Codex history on macOS:

```console
$ agent-session-compress --path ~/codex-copy
backend: apfs (afsctool lzfse) (dry run)
  /Users/me/codex-copy
scanned 25 files: 24 to compress (665.2 MB, 665.2 MB on disk), 0 already compressed, 1 modified within 7 days, 0 open
dry run: pass --apply to compress

$ agent-session-compress --path ~/codex-copy --apply
...
compressed 24 files, 0 left as-is; on disk 665.2 MB -> 432.9 MB
```

## Install

```sh
go install github.com/wolfiesch/agent-session-compress@latest
```

Or download a binary for macOS, Linux, or Windows from [Releases](https://github.com/wolfiesch/agent-session-compress/releases). It is a single static binary with no dependencies.

## Usage

```sh
agent-session-compress                 # dry run over every harness found
agent-session-compress claude codex    # only these harnesses
agent-session-compress --path ~/logs   # any other directory
agent-session-compress --apply         # compress
agent-session-compress --list          # show where each harness keeps sessions
```

| Option | Default | |
|---|---|---|
| `--idle-days N` | 7 | Only touch files unmodified for N days. |
| `--apply` | off | Compress. Without it, report what would be compressed. |
| `--json` | off | Print the report as JSON. |

Run it again whenever you like; files already compressed are skipped. A scheduled run (cron, launchd, Task Scheduler) keeps history compact.

## Harnesses

| Name | Directories |
|---|---|
| `claude` | `~/.claude/projects` (`$CLAUDE_CONFIG_DIR`) |
| `codex` | `~/.codex/sessions`, `~/.codex/archived_sessions` (`$CODEX_HOME`) |
| `gemini` | `~/.gemini/tmp` |
| `opencode` | `~/.local/share/opencode/storage` (`$XDG_DATA_HOME`) |
| `omp` | `~/.omp/agent/sessions` |
| `pi` | `~/.pi/agent/sessions` (`$PI_CODING_AGENT_DIR`) |

Claude Code deletes transcripts older than `cleanupPeriodDays` (30 by default). With history compressed, raising that setting in `~/.claude/settings.json` costs far less disk.

## How it works

| Platform | Filesystem | Mechanism |
|---|---|---|
| macOS | APFS | [`afsctool`](https://github.com/RJVB/afsctool) with LZFSE when installed (`brew install afsctool`); otherwise the built-in `ditto --hfsCompression` writes a compressed copy that is verified and renamed over the original |
| Windows | NTFS | Built-in `compact /c /exe:lzx` (Windows Overlay Filter compression) |
| Linux | btrfs | `btrfs filesystem defragment -czstd` (no root needed for files you own). btrfs does not report compressed sizes per file; check savings with [`compsize`](https://github.com/kilobyte/compsize). |

ext4 and XFS have no per-file transparent compression and are reported as unsupported. On ZFS, set `compression=zstd` on the dataset instead.

Safety:

- Dry run by default.
- Only files unmodified for `--idle-days` are touched. On macOS, files any process holds open (per `lsof`) are skipped too.
- Each file's SHA-256 is taken before compression and checked afterwards; the original mtime is restored.
- Writing to a compressed file makes the filesystem store it uncompressed again, so resuming an old session costs only that file's savings.
- Already-compressed formats (`.gz`, images, SQLite databases) and single-block files are skipped.

## Measured savings

| Data | Backend | Before | After |
|---|---|---|---|
| Oh My Pi session tree, 137,919 files | APFS, afsctool LZFSE | 20.3 GB | 5.8 GB |
| Codex sessions, 25 files | APFS, afsctool LZFSE | 686 MB | 447 MB |
| Codex sessions, 25 files | APFS, ditto | 686 MB | 656 MB |
| One Oh My Pi transcript | NTFS, compact LZX | 8.0 MB | 2.3 MB |

Codex rollouts carry encrypted reasoning blobs, which compress poorly. On macOS, install `afsctool`: `ditto` uses zlib and leaves files it cannot shrink enough uncompressed.

## License

MIT
