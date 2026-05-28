# rex

Run commands and transfer files on remote servers over SSH. Named sessions mean you never type connection details twice. A background daemon keeps connections alive so repeated commands feel instant.

```
$ rex --set-session work alice@myserver.com
Session "work" registered and activated (alice@myserver.com:22)

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
make install          # installs to ~/.local/bin  (or $PREFIX/bin)
```

Both binaries (`rex` and `rexd`) must live in the same directory. `rex` auto-starts `rexd` on first use.

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

## Jump hosts (bastion / login nodes)

Many environments — HPC clusters, corporate networks, air-gapped servers — require
you to SSH into one or more intermediate machines before reaching the target:

```
your laptop  ──SSH──►  login.cluster.com  ──SSH──►  gpu-node-04.internal
              (public)                     (internal network only)
```

Rex handles this automatically. Every session is an **ordered chain of nodes**. The first
node is dialled directly; each subsequent node is reached by tunnelling through the previous one.

### Setting up a jump session

Pass each hop in order, outermost first:

```bash
# One jump host
rex --set-session cluster alice@login.cluster.com alice@gpu-node-04

# Name is optional — defaults to the last node's hostname
rex --set-session alice@login.cluster.com alice@gpu-node-04

# Different users and ports on each hop
rex --set-session cluster bob@login.cluster.com:2222 alice@gpu-node-04

# Two jump hosts (three-hop chain)
rex --set-session deep alice@gateway.com bob@internal-hop alice@final-target
```

### First connection

Because rex connects to each host in turn, it may prompt once per new host:

```
$ rex --set-session cluster alice@login.cluster.com alice@gpu-node-04
Session "cluster" registered and activated (alice@login.cluster.com:22 → alice@gpu-node-04:22)

$ rex hostname
The authenticity of host "login.cluster.com:22" can't be established.
ED25519 key fingerprint is SHA256:abc123...
Are you sure you want to continue connecting (yes/no)? yes

The authenticity of host "gpu-node-04.internal:22" can't be established.
ED25519 key fingerprint is SHA256:xyz789...
Are you sure you want to continue connecting (yes/no)? yes

gpu-node-04
```

All hosts are added to `~/.ssh/known_hosts`. Subsequent commands connect silently.

### Using the session

Once registered, a jump session works exactly like any other:

```bash
rex hostname                          # runs on gpu-node-04
rex nvidia-smi                        # check GPU status
rex --shell                           # interactive shell on gpu-node-04
rex --upload model.py ~/project/      # upload a file through the jump
rex --download ~/results/ ./results/  # download results back
rex --session cluster sbatch job.sh   # run without switching active session
```

### The daemon advantage

With `rexd` running, all SSH connections are established once and kept alive.
Every subsequent `rex` command reuses them — no repeated multi-hop handshakes.
The first command takes a little longer; all others are instant.

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
duration=$(rex --json uptime | jq .duration_s)
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

**Automatic** (recommended): if `SSH_AUTH_SOCK` is set, rex uses your running
ssh-agent. This works out of the box on most desktop Linux and macOS setups.

**Explicit key per node**: set the `identity` field in the config for each node
that needs a specific key:

```toml
[sessions.work]
name = "work"

[[sessions.work.nodes]]
host     = "myserver.com"
user     = "alice"
port     = 22
identity = "~/.ssh/id_ed25519"
```

**First connection**: rex shows each remote host's fingerprint and asks you to
confirm before adding it to `~/.ssh/known_hosts`. Subsequent connections are
verified silently. Rex never silently accepts an unknown or changed host key.

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

`rex` is the command you type. `rexd` is a background daemon that keeps SSH
connections alive. The first `rex` command starts `rexd` automatically.

Instead of opening a new SSH connection for every command (a ~200ms handshake
each time), `rex` sends requests to `rexd` over a local Unix socket. `rexd`
multiplexes commands over the existing connection, then streams stdout, stderr,
and the exit code back.

The daemon shuts itself down after 30 minutes of inactivity. If `rexd` is
unavailable, `rex` falls back to a direct SSH connection transparently.

### Socket location

| `$XDG_RUNTIME_DIR` set? | Socket path |
|---|---|
| Yes | `$XDG_RUNTIME_DIR/rex/rexd.sock` |
| No | `/tmp/rex-$UID/rexd.sock` |

Override the config file location with `REX_CONFIG=/path/to/config.toml`.

---

## Config file reference

`~/.config/rex/config.toml` (or `$REX_CONFIG`):

```toml
[active]
session = "work"          # currently active session

# Direct session — single node
[sessions.work]
name = "work"

[[sessions.work.nodes]]
host     = "myserver.com"
user     = "alice"
port     = 22             # default: 22
identity = "~/.ssh/id_ed25519"   # optional; uses ssh-agent if omitted

# Two-hop session — laptop → login node → gpu node
[sessions.cluster]
name = "cluster"

[[sessions.cluster.nodes]]
name = "login"            # optional human label for this node
host = "login.cluster.com"
user = "alice"

[[sessions.cluster.nodes]]
name = "gpu"
host = "gpu-node-04.internal"
user = "alice"

# Three-hop session — any number of hops supported
[sessions.deep]
name = "deep"

[[sessions.deep.nodes]]
name = "gateway"
host = "gateway.example.com"
user = "alice"

[[sessions.deep.nodes]]
name = "hop"
host = "internal-hop.private"
user = "bob"
port = 2222

[[sessions.deep.nodes]]
name = "target"
host = "final-target.internal"
user = "carol"
identity = "~/.ssh/id_rsa"

# Simple session
[sessions.homelab]
name = "homelab"

[[sessions.homelab.nodes]]
host = "192.168.1.10"
user = "root"
port = 2222
```

Nodes are connected in order: the first node is dialled directly, each subsequent
node is reached by tunnelling through the previous one. The last node is the target
where commands run.

The file is written with mode `600`. Do not add passwords — they are not supported.

---

## Reference

```
rex --set-session [name] user@host[:port] [user@hop2 ...]  register a session
rex --sessions                                              list all sessions
rex --use <name>                                            switch active session
rex <command>                                               run command on active session
rex --session <name> <command>                              run command on a named session
rex --shell                                                 open interactive shell
rex --upload [-r] <local> <remote>                         upload file or directory
rex --download [-r] <remote> <local>                       download file or directory
rex --copy <session1:/path> <session2:/path>               copy file between sessions
rex --json                                                  emit machine-readable JSON result
rex --force                                                 skip overwrite confirmation
rex --preserve                                              keep timestamps and permissions
```

When a remote command itself has flags (e.g. `git log --oneline`), place rex's
own flags before the command name — rex stops parsing its own flags at the first
non-flag word:

```bash
rex --session prod git log --oneline -10    # works fine
rex --session cluster nvidia-smi -l 1      # works fine
```
