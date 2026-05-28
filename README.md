# rex

Run commands and transfer files on remote servers over SSH. Named sessions mean you never type connection details twice. A background daemon keeps connections alive so repeated commands feel instant.

```
$ rex --set-session work alice@myserver.com
Session "work" registered and activated (alice@myserver.com)

$ rex whoami
alice

$ rex git log --oneline -5
a1b2c3d fix typo in config
...
```

---

## Install

**From source** (requires Go 1.22+):

```bash
git clone https://github.com/you/rex
cd rex
go build -o ~/.local/bin/rex     ./cmd/rex
go build -o ~/.local/bin/rexd    ./cmd/rexd
```

Both binaries must live in the same directory. `rex` auto-starts `rexd` on first use.

---

## Quick start

### 1. Register a session

```bash
rex --set-session alice@myserver.com          # name defaults to the hostname
rex --set-session work alice@myserver.com     # or give it a name
rex --set-session work alice@myserver.com:22  # custom port
```

The session is saved and made active immediately. Config is stored at
`~/.config/rex/config.toml` (mode `600`).

### 2. Run a command

```bash
rex uptime
rex df -h
rex systemctl status nginx
```

`rex` passes the arguments to the remote shell as-is (`/bin/sh -c "your command"`), so quoting, pipes, and redirects all work:

```bash
rex 'ps aux | grep python'
rex 'cat /etc/os-release | head -3'
```

### 3. Open an interactive shell

```bash
rex --shell
```

Drops you into a full interactive shell on the active session. Your terminal is put into raw mode and resize events are forwarded, so editors like `vim` and `htop` work correctly.

---

## Sessions

```bash
# Register sessions
rex --set-session prod     deploy@prod.example.com
rex --set-session staging  deploy@staging.example.com
rex --set-session homelab  root@192.168.1.10:2222

# List all sessions
rex --sessions
#   prod:     deploy@prod.example.com:22  (active)
#   staging:  deploy@staging.example.com:22
#   homelab:  root@192.168.1.10:2222

# Switch the active session
rex --use staging

# Run a one-off command on a specific session without switching
rex --session prod systemctl status app
```

The active session is remembered in the config file. All plain `rex <command>` invocations use it.

---

## File transfer

### Upload

```bash
rex --upload deploy.tar.gz /home/deploy/releases/deploy.tar.gz
rex --upload -r ./build/   /var/www/html/
```

### Download

```bash
rex --download /var/log/app.log ./logs/app.log
rex --download -r /var/www/html/ ./backup/
```

Progress bars are shown for large files. Use `--preserve` to keep timestamps and permissions, `--force` to skip the overwrite prompt.

### Copy between sessions

```bash
rex --copy prod:/var/backups/db.sql staging:/tmp/db.sql
```

Downloads from the source session and uploads to the destination. Useful for promoting data between environments.

---

## Scripting

### Exit codes

`rex` propagates the remote process exit code as its own:

```bash
rex test -f /etc/important-file && echo "exists" || echo "missing"

if rex systemctl is-active nginx; then
    echo "nginx is up"
fi
```

### JSON output

```bash
rex --json uptime
```

```json
{
  "exit_code": 0,
  "duration_s": 0.312,
  "session": "work"
}
```

Combine with `jq`:

```bash
duration=$(rex --json -- sleep 1 | jq .duration_s)
```

### Piping

When stdin is not a terminal, `rex` skips TTY allocation so piping works correctly:

```bash
# Stream remote log to local grep
rex tail -f /var/log/app.log | grep ERROR

# Pipe local data into a remote command
cat dump.sql | rex 'psql mydb'

# Use in a loop
for host in web1 web2 web3; do
    rex --session $host uptime
done
```

---

## Authentication

Rex uses key-based authentication only — no passwords are stored.

**Automatic** (recommended): if `SSH_AUTH_SOCK` is set, rex uses your running ssh-agent. This works out of the box on most desktop Linux and macOS setups.

**Explicit key**: set the `identity` field in the config:

```toml
[sessions.work]
host     = "myserver.com"
user     = "alice"
port     = 22
identity = "~/.ssh/id_ed25519"
```

**First connection**: rex shows the remote host's fingerprint and asks you to confirm before adding it to `~/.ssh/known_hosts`. Subsequent connections are verified silently. Rex never silently accepts an unknown or changed host key.

---

## How it works

```
you          rex              rexd                  SSH server
 │            │                │                        │
 ├─ rex ls ──►│                │                        │
 │            ├── connect ────►│                        │
 │            │                ├─── (existing conn) ───►│
 │            │◄── stdout ─────┤◄── output ─────────────┤
 │◄─ output ──┤                │                        │
```

`rex` is the command you type. `rexd` is a background daemon that keeps SSH connections alive. The first `rex` command on a new machine starts `rexd` automatically.

Instead of opening a new SSH connection for every command (a ~200ms handshake each time), `rex` sends requests to `rexd` over a local Unix socket. `rexd` multiplexes commands over the existing connection, then streams stdout, stderr, and the exit code back.

The daemon shuts itself down after 30 minutes of inactivity.

If `rexd` is unavailable for any reason, `rex` falls back to a direct SSH connection transparently.

### Socket location

| `$XDG_RUNTIME_DIR` set? | Socket path |
|---|---|
| Yes | `$XDG_RUNTIME_DIR/rex/rexd.sock` |
| No | `/tmp/rex-$UID/rexd.sock` |

Override the config file location with `REX_CONFIG=/path/to/config.toml`.

---

## Jump hosts (bastion / login nodes)

If you can only reach your work machine by SSHing into a login node first, add
`jump_host` to the session config:

```toml
[sessions.cluster]
host = "gpu-node-04.internal"           # the machine you actually want
user = "alice"
jump = "alice@login.cluster.example.com"  # the node you can reach directly
```

Or via `--set-session`:

```bash
rex --set-session cluster alice@login.cluster.example.com alice@gpu-node-04.internal
#                  name    jump                           target
```

rex opens one SSH connection to the jump host, tunnels a TCP channel through it
to the target, and negotiates a second SSH session over that channel — the same
thing `ssh -J login.cluster.example.com gpu-node-04.internal` does.

The jump host and target host are verified against `~/.ssh/known_hosts`
independently. On first connection rex prompts once for each unknown host.

With the daemon running, the two connections are kept open and reused across
commands, so the two-hop overhead only happens once.

---

## Config file reference

`~/.config/rex/config.toml` (or `$REX_CONFIG`):

```toml
[active]
session = "work"          # currently active session

[sessions.work]
host     = "myserver.com"
user     = "alice"
port     = 22             # default: 22
identity = "~/.ssh/id_ed25519"   # optional; uses ssh-agent if omitted

[sessions.cluster]
host = "gpu-node-04.internal"
user = "alice"
jump = "alice@login.cluster.example.com"

[sessions.homelab]
host = "192.168.1.10"
user = "root"
port = 2222
```

The file is written with mode `600`. Do not add passwords — they are not supported.

---

## Reference

```
rex --set-session [name] user@host[:port]   register and activate a session
rex --sessions                              list all sessions
rex --use <name>                            switch active session
rex <command>                               run command on active session
rex --session <name> <command>              run command on a named session
rex --shell                                 open interactive shell
rex --upload [-r] <local> <remote>          upload file or directory
rex --download [-r] <remote> <local>        download file or directory
rex --copy <session1:/path> <session2:/path> copy file between sessions
rex --json                                  emit machine-readable JSON result
rex --force                                 skip overwrite confirmation
rex --preserve                              keep timestamps and permissions
```

When a remote command itself has flags (e.g. `git log --oneline`), place rex's own flags before the command name — rex stops parsing its own flags at the first non-flag word:

```bash
rex --session prod git log --oneline -10    # works fine
```
