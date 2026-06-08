package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"rex/config"
	"rex/internal/daemon"
	rexsftp "rex/internal/sftp"
	"rex/internal/session"
	rexssh "rex/internal/ssh"
)

type jsonResult struct {
	ExitCode int     `json:"exit_code"`
	Duration float64 `json:"duration_s"`
	Session  string  `json:"session"`
	Error    string  `json:"error,omitempty"`
}

var remoteExitCode int

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(remoteExitCode)
}

func newRootCmd() *cobra.Command {
	var (
		flagSetSession bool
		flagSessions   bool
		flagUpload     bool
		flagDownload   bool
		flagCopy       bool
		flagRecursive  bool
		flagJSON       bool
		flagTarget     string
	)

	cmd := &cobra.Command{
		Use:   "rex <machine> [command] [flags]",
		Short: "Remote command execution over SSH",
		Long: `rex — run commands and transfer files on remote servers over SSH.

Usage modes:
  rex <machine> [command]            run a command (no command = interactive shell)
  rex --set-session [name] user@host register a session
  rex --sessions                     list saved sessions
  rex --upload [-r] <machine> <local> <remote>   upload file or directory
  rex --download [-r] <machine> <remote> <local> download file or directory
  rex --copy <session1:/path> <session2:/path>   copy between two sessions

The target machine can also be given with -T so remote flags are not consumed:
  rex -T <machine> [command] [flags]`,
		Example: `  rex --set-session work alice@myserver.com
  rex work ls -la
  rex work git log --oneline
  rex -T work --json uptime
  rex --upload work ./app /opt/app
  rex --upload -r work ./dist /var/www
  rex --download work /var/log/syslog ./
  rex --copy work:/data backup:/data`,
		SilenceUsage: true,
		Args:         cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := config.DefaultPath()
			cfg, err := session.Load(cfgPath)
			if err != nil {
				return err
			}

			switch {
			case flagSetSession:
				return runSetSession(cfg, cfgPath, args)
			case flagSessions:
				return runListSessions(cfg)
			case flagCopy:
				return runCopy(cfg, args)
			case flagUpload:
				machine, rest, err := splitMachineArgs(flagTarget, args)
				if err != nil || len(rest) != 2 {
					return fmt.Errorf("usage: rex --upload [-r] [-T <machine>] <local> <remote>")
				}
				return runUpload(cfg, machine, rest, flagRecursive)
			case flagDownload:
				machine, rest, err := splitMachineArgs(flagTarget, args)
				if err != nil || len(rest) != 2 {
					return fmt.Errorf("usage: rex --download [-r] [-T <machine>] <remote> <local>")
				}
				return runDownload(cfg, machine, rest, flagRecursive)
			default:
				machine, rest, err := splitMachineArgs(flagTarget, args)
				if err != nil {
					return cmd.Help()
				}
				if len(rest) == 0 {
					code, err := runShell(cfg, machine)
					remoteExitCode = code
					return err
				}
				code, err := runCommand(cfg, machine, strings.Join(rest, " "), flagJSON)
				remoteExitCode = code
				return err
			}
		},
	}

	f := cmd.Flags()
	// SetInterspersed(false): stop flag parsing at first non-flag arg so flags
	// in the remote command (e.g. "rex work git log --oneline") are not parsed by rex.
	f.SetInterspersed(false)
	f.BoolVar(&flagSetSession, "set-session", false, "register a session: [name] user@host[:port]")
	f.BoolVar(&flagSessions, "sessions", false, "list saved sessions")
	f.BoolVar(&flagUpload, "upload", false, "upload file or directory: <machine> <local> <remote>")
	f.BoolVar(&flagDownload, "download", false, "download file or directory: <machine> <remote> <local>")
	f.BoolVar(&flagCopy, "copy", false, "copy between sessions: <session1:/path> <session2:/path>")
	f.BoolVarP(&flagRecursive, "recursive", "r", false, "recursive transfer (for --upload / --download)")
	f.BoolVar(&flagJSON, "json", false, "print result as JSON (exit code, duration, session)")
	f.StringVarP(&flagTarget, "target", "T", "", "target machine (alternative to first positional arg)")

	return cmd
}

// connectOrStartDaemon tries to connect to rexd. If the socket is missing, it
// attempts to start rexd (found next to the rex binary) and retries once.
func connectOrStartDaemon() (*daemon.Client, error) {
	socketPath := daemon.SocketPath()
	if dc, err := daemon.Connect(socketPath); err == nil {
		return dc, nil
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	return daemon.Connect(socketPath)
}

func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	rexdPath := filepath.Join(filepath.Dir(exe), "rexd")
	if _, err := os.Stat(rexdPath); err != nil {
		return fmt.Errorf("rexd not found at %s", rexdPath)
	}

	cmd := exec.Command(rexdPath)
	// Detach from current process group so rexd survives rex exiting.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start rexd: %w", err)
	}
	_ = cmd.Process.Release()

	// Wait up to 1 s for the socket to appear.
	socketPath := daemon.SocketPath()
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		if conn, err := net.Dial("unix", socketPath); err == nil {
			conn.Close()
			return nil
		}
	}
	return fmt.Errorf("rexd did not become ready in time")
}

// splitMachineArgs resolves the target machine from either the -T/--target flag
// or the first positional argument, returning the machine name and remaining args.
func splitMachineArgs(target string, args []string) (machine string, rest []string, err error) {
	if target != "" {
		return target, args, nil
	}
	if len(args) == 0 {
		return "", nil, fmt.Errorf("no machine specified: use <machine> as first arg or -T <machine>")
	}
	return args[0], args[1:], nil
}

func runSetSession(cfg *config.Config, cfgPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: rex --set-session [name] user@host[:port] [user@hop2 ...]")
	}

	var name string
	var targets []string

	if !strings.Contains(args[0], "@") {
		name = args[0]
		targets = args[1:]
	} else {
		targets = args
		_, host, _, err := session.ParseTarget(targets[len(targets)-1])
		if err != nil {
			return err
		}
		name = host
	}

	if len(targets) == 0 {
		return fmt.Errorf("usage: rex --set-session [name] user@host[:port] [user@hop2 ...]")
	}

	if err := session.Set(cfg, name, targets); err != nil {
		return err
	}
	if err := session.Save(cfgPath, cfg); err != nil {
		return err
	}
	s := cfg.Sessions[name]
	fmt.Printf("Session %q registered and activated (%s)\n", name, session.ChainString(s))
	return nil
}

func runListSessions(cfg *config.Config) error {
	if len(cfg.Sessions) == 0 {
		fmt.Println("No sessions. Run: rex --set-session user@host")
		return nil
	}
	for name, s := range cfg.Sessions {
		active := ""
		if name == cfg.Active.Session {
			active = " (active)"
		}
		fmt.Printf("  %s: %s%s\n", name, session.ChainString(s), active)
	}
	return nil
}

func resolveSession(cfg *config.Config, name string) (config.SessionConfig, string, error) {
	s, err := session.Get(cfg, name)
	return s, name, err
}

func runCommand(cfg *config.Config, sessionName, cmd string, jsonOut bool) (int, error) {
	start := time.Now()
	s, sName, err := resolveSession(cfg, sessionName)
	if err != nil {
		return 1, err
	}

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	w, h := 80, 24
	if isTTY {
		if ww, hh, e := term.GetSize(int(os.Stdin.Fd())); e == nil {
			w, h = ww, hh
		}
	}

	// Try the daemon first; fall back to direct SSH on any error (auth
	// failure, stale connection, daemon not available, etc.).  The daemon
	// helper restores terminal state before returning so prompts in the
	// direct path work correctly.
	exitCode, runErr := execViaDaemon(sName, cmd, isTTY, w, h)
	if runErr != nil {
		client, err := rexssh.Connect(s)
		if err != nil {
			return 1, err
		}
		defer client.Close()
		exitCode, runErr = client.Run(cmd)
	}

	if jsonOut {
		r := jsonResult{ExitCode: exitCode, Duration: time.Since(start).Seconds(), Session: sName}
		if runErr != nil {
			r.Error = runErr.Error()
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
	}

	return exitCode, runErr
}

func runShell(cfg *config.Config, sessionName string) (int, error) {
	s, sName, err := resolveSession(cfg, sessionName)
	if err != nil {
		return 1, err
	}

	w, h := 80, 24
	if ww, hh, e := term.GetSize(int(os.Stdin.Fd())); e == nil {
		w, h = ww, hh
	}

	if code, err := shellViaDaemon(sName, w, h); err == nil {
		return code, nil
	}

	// Daemon unavailable or SSH connection failed — connect directly.
	client, err := rexssh.Connect(s)
	if err != nil {
		return 1, err
	}
	defer client.Close()
	return client.Shell()
}

// execViaDaemon attempts to run cmd through rexd. It manages terminal raw mode
// so the state is fully restored before it returns, allowing the caller to fall
// back to direct SSH (which may need to display interactive prompts).
func execViaDaemon(sessionName, cmd string, isTTY bool, w, h int) (int, error) {
	dc, err := connectOrStartDaemon()
	if err != nil {
		return 1, err
	}
	defer dc.Close()
	if isTTY {
		if oldState, e := term.MakeRaw(int(os.Stdin.Fd())); e == nil {
			defer term.Restore(int(os.Stdin.Fd()), oldState)
		}
	}
	return dc.Exec(daemon.ExecRequest{
		Session: sessionName,
		Cmd:     cmd,
		TTY:     isTTY,
		Width:   w,
		Height:  h,
	})
}

// shellViaDaemon attempts to open an interactive shell through rexd.
func shellViaDaemon(sessionName string, w, h int) (int, error) {
	dc, err := connectOrStartDaemon()
	if err != nil {
		return 1, err
	}
	defer dc.Close()
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return 1, err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)
	return dc.Shell(daemon.ShellRequest{Session: sessionName, Width: w, Height: h})
}

func runUpload(cfg *config.Config, sessionName string, args []string, recursive bool) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rex --upload [-r] <machine> <local> <remote>")
	}
	s, _, err := resolveSession(cfg, sessionName)
	if err != nil {
		return err
	}
	client, err := rexssh.Connect(s)
	if err != nil {
		return err
	}
	defer client.Close()
	return rexsftp.Upload(client.SSHClient(), args[0], args[1], recursive)
}

func runDownload(cfg *config.Config, sessionName string, args []string, recursive bool) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rex --download [-r] <machine> <remote> <local>")
	}
	s, _, err := resolveSession(cfg, sessionName)
	if err != nil {
		return err
	}
	client, err := rexssh.Connect(s)
	if err != nil {
		return err
	}
	defer client.Close()
	return rexsftp.Download(client.SSHClient(), args[0], args[1], recursive)
}

func runCopy(cfg *config.Config, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rex --copy session1:/path session2:/path")
	}

	srcName, srcPath, err := parseSessionPath(args[0])
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	dstName, dstPath, err := parseSessionPath(args[1])
	if err != nil {
		return fmt.Errorf("dest: %w", err)
	}

	srcSess, err := session.Get(cfg, srcName)
	if err != nil {
		return fmt.Errorf("source session: %w", err)
	}
	dstSess, err := session.Get(cfg, dstName)
	if err != nil {
		return fmt.Errorf("dest session: %w", err)
	}

	tmp, err := os.CreateTemp("", "rex-copy-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	srcClient, err := rexssh.Connect(srcSess)
	if err != nil {
		return err
	}
	defer srcClient.Close()

	if err := rexsftp.Download(srcClient.SSHClient(), srcPath, tmpPath, false); err != nil {
		return fmt.Errorf("download from source: %w", err)
	}

	dstClient, err := rexssh.Connect(dstSess)
	if err != nil {
		return err
	}
	defer dstClient.Close()

	return rexsftp.Upload(dstClient.SSHClient(), tmpPath, dstPath, false)
}

func parseSessionPath(s string) (sess, path string, err error) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("expected session:/path, got %q", s)
	}
	return s[:idx], s[idx+1:], nil
}
