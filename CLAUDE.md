# rex — Remote EXecution CLI

## What is rex?

`rex` is a CLI tool for running commands and transferring files on a remote server over SSH,
with persistent named sessions so you don't have to type connection details every time.

## Core Usage

```bash
rex --set-session [name] user@host[:port]   # register + activate a session
rex --sessions                              # list all saved sessions
rex --use <name>                            # switch active session
rex <command>                               # run command on active session
rex --session <name> <command>             # one-off on a named session
rex --upload [-r] <local> <remote>         # upload file/dir
rex --download [-r] <remote> <local>       # download file/dir
rex --shell                                # drop into interactive shell
rex --copy <session1:path> <session2:path> # remote-to-remote copy
```

## Design Decisions

### Language & Stack
- **Language:** Go — single static binary, cross-platform, excellent SSH support
- **SSH:** `golang.org/x/crypto/ssh`
- **SFTP:** `github.com/pkg/sftp` (preferred over SCP — more reliable, supports resumption)
- **Config:** TOML via `github.com/BurntSushi/toml`
- **CLI:** `github.com/spf13/cobra`
- **Progress bars:** `github.com/schollz/progressbar`
- **Persistence:** Unix socket daemon (`rexd`) for connection reuse

### Architecture: CLI + Daemon
- `rex` is the CLI the user types
- `rexd` is a background daemon that holds open SSH connections
- `rex` communicates with `rexd` via a local Unix socket
- This avoids the overhead of a new SSH handshake per command

### Session Config
Stored at `~/.config/rex/config.toml` (XDG-compliant, fallback `~/.rex/`).
Respect `REX_CONFIG` env var for overrides. File stored with `chmod 600`.

```toml
[active]
session = "work"

[sessions.work]
host = "myserver.com"
user = "alice"
port = 22
identity = "~/.ssh/id_ed25519"

[sessions.homelab]
host = "192.168.1.10"
user = "root"
port = 22
```

### Connection Persistence
- Connection pooling via `rexd` daemon (preferred)
- Alternative fallback: SSH ControlMaster via Unix socket if daemon not running

### Command Execution
- Propagate remote **exit codes** as `rex`'s exit code (essential for scripting)
- **TTY allocation:** auto-detect (check if stdin is a TTY) — allocate for interactive commands, skip for piped usage
- **Signals:** forward Ctrl+C to the remote process
- **Stderr/stdout:** keep separate so piping works correctly
- **Shell invocation:** join args, pass to remote `/bin/sh -c` for flexibility
- Default remote working directory: remote home dir

### File Transfer
- Use SFTP over the existing SSH connection
- Support recursive transfers with `-r`
- Show progress bars for large files
- Support glob expansion
- Flags: `--force` (skip overwrite prompt), `--preserve` (timestamps/permissions)

### Error UX
- Clear error when no session set: `"No active session. Run: rex --set-session user@host"`
- Distinguish connection errors from command errors
- Timeout handling with a useful message

### Security
- No password storage — key-based auth only; prompt and use ssh-agent otherwise
- Respect `~/.ssh/known_hosts` — never silently accept unknown hosts
- Config files stored with `chmod 600`

### Output
- `--json` flag for machine-readable output (exit code, duration, session used)
- Useful when `rex` is used inside scripts

## Project Structure (suggested)

```
rex/
├── cmd/
│   └── rex/
│       └── main.go          # CLI entrypoint (cobra)
├── cmd/
│   └── rexd/
│       └── main.go          # Daemon entrypoint
├── internal/
│   ├── session/             # Session config load/save
│   ├── ssh/                 # SSH client + connection pool
│   ├── sftp/                # File transfer logic
│   ├── daemon/              # Unix socket IPC
│   └── cli/                 # Command handlers
├── config/
│   └── config.go            # Config schema + defaults
├── go.mod
├── go.sum
└── CLAUDE.md                # This file
```

## Status
Complete. All components implemented.

- Session config read/write (`internal/session`, `config/`)
- SSH command execution with TTY detection, signal forwarding, SIGWINCH resize (`internal/ssh`)
- File transfer: upload/download with progress bars, recursive support (`internal/sftp`)
- Full CLI (`cmd/rex/main.go`): all flags from the spec
- `rexd` daemon (`cmd/rexd`, `internal/daemon`): Unix socket IPC, SSH connection pool,
  auto-start from `rex`, graceful fallback to direct SSH if daemon unavailable
