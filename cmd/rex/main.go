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
		flagUse        string
		flagSession    string
		flagUpload     bool
		flagDownload   bool
		flagShell      bool
		flagCopy       bool
		flagRecursive  bool
		flagForce      bool
		flagPreserve   bool
		flagJSON       bool
	)

	cmd := &cobra.Command{
		Use:          "rex",
		Short:        "Remote command execution over SSH",
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
			case flagUse != "":
				return runUse(cfg, cfgPath, flagUse)
			case flagShell:
				code, err := runShell(cfg, flagSession)
				remoteExitCode = code
				return err
			case flagUpload:
				return runUpload(cfg, flagSession, args, flagRecursive, flagForce, flagPreserve)
			case flagDownload:
				return runDownload(cfg, flagSession, args, flagRecursive, flagForce, flagPreserve)
			case flagCopy:
				return runCopy(cfg, args)
			default:
				if len(args) == 0 {
					return cmd.Help()
				}
				code, err := runCommand(cfg, flagSession, strings.Join(args, " "), flagJSON)
				remoteExitCode = code
				return err
			}
		},
	}

	f := cmd.Flags()
	// SetInterspersed(false): stop flag parsing at first non-flag arg.
	// This lets "rex --session foo git log --oneline" work correctly —
	// "--oneline" is passed through as part of the remote command, not parsed as a rex flag.
	f.SetInterspersed(false)
	f.BoolVar(&flagSetSession, "set-session", false, "register a session: [name] user@host[:port]")
	f.BoolVar(&flagSessions, "sessions", false, "list saved sessions")
	f.StringVar(&flagUse, "use", "", "switch active session by name")
	f.StringVar(&flagSession, "session", "", "use a named session for this command")
	f.BoolVar(&flagUpload, "upload", false, "upload file/dir to remote")
	f.BoolVar(&flagDownload, "download", false, "download file/dir from remote")
	f.BoolVar(&flagShell, "shell", false, "open interactive shell on remote")
	f.BoolVar(&flagCopy, "copy", false, "copy between sessions: session1:/path session2:/path")
	f.BoolVarP(&flagRecursive, "recursive", "r", false, "recursive file transfer")
	f.BoolVar(&flagForce, "force", false, "skip overwrite confirmation")
	f.BoolVar(&flagPreserve, "preserve", false, "preserve timestamps and permissions")
	f.BoolVar(&flagJSON, "json", false, "output machine-readable JSON result")

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

func runSetSession(cfg *config.Config, cfgPath string, args []string) error {
	var name, target, jump string

	switch len(args) {
	case 1:
		// rex --set-session user@host
		target = args[0]
		_, host, _, err := session.ParseTarget(target)
		if err != nil {
			return err
		}
		name = host

	case 2:
		if strings.Contains(args[0], "@") {
			// rex --set-session user1@jump user2@target
			jump, target = args[0], args[1]
			_, host, _, err := session.ParseTarget(target)
			if err != nil {
				return err
			}
			name = host
		} else {
			// rex --set-session name user@host
			name, target = args[0], args[1]
		}

	case 3:
		// rex --set-session name user1@jump user2@target
		name, jump, target = args[0], args[1], args[2]

	default:
		return fmt.Errorf("usage: rex --set-session [name] [user@jump] user@host[:port]")
	}

	if err := session.Set(cfg, name, target, jump); err != nil {
		return err
	}
	if err := session.Save(cfgPath, cfg); err != nil {
		return err
	}
	if jump != "" {
		fmt.Printf("Session %q registered and activated (%s via %s)\n", name, target, jump)
	} else {
		fmt.Printf("Session %q registered and activated (%s)\n", name, target)
	}
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
		fmt.Printf("  %s: %s@%s:%d%s\n", name, s.User, s.Host, s.Port, active)
	}
	return nil
}

func runUse(cfg *config.Config, cfgPath, name string) error {
	if err := session.Use(cfg, name); err != nil {
		return err
	}
	if err := session.Save(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("Switched to session %q\n", name)
	return nil
}

func resolveSession(cfg *config.Config, name string) (config.SessionConfig, string, error) {
	if name != "" {
		s, err := session.Get(cfg, name)
		return s, name, err
	}
	s, err := session.Active(cfg)
	return s, cfg.Active.Session, err
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

	var exitCode int
	var runErr error

	if dc, daemonErr := connectOrStartDaemon(); daemonErr == nil {
		defer dc.Close()
		if isTTY {
			if oldState, e := term.MakeRaw(int(os.Stdin.Fd())); e == nil {
				defer term.Restore(int(os.Stdin.Fd()), oldState)
			}
		}
		exitCode, runErr = dc.Exec(daemon.ExecRequest{
			Session: sName,
			Cmd:     cmd,
			TTY:     isTTY,
			Width:   w,
			Height:  h,
		})
	} else {
		// Daemon unavailable — connect directly.
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

	if dc, daemonErr := connectOrStartDaemon(); daemonErr == nil {
		defer dc.Close()
		if oldState, e := term.MakeRaw(int(os.Stdin.Fd())); e == nil {
			defer term.Restore(int(os.Stdin.Fd()), oldState)
		}
		return dc.Shell(daemon.ShellRequest{Session: sName, Width: w, Height: h})
	}

	// Daemon unavailable — connect directly.
	client, err := rexssh.Connect(s)
	if err != nil {
		return 1, err
	}
	defer client.Close()
	return client.Shell()
}

func runUpload(cfg *config.Config, sessionName string, args []string, recursive, force, preserve bool) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rex --upload [-r] <local> <remote>")
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
	return rexsftp.Upload(client.SSHClient(), args[0], args[1], recursive, preserve)
}

func runDownload(cfg *config.Config, sessionName string, args []string, recursive, force, preserve bool) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rex --download [-r] <remote> <local>")
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
	return rexsftp.Download(client.SSHClient(), args[0], args[1], recursive, preserve)
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

	if err := rexsftp.Download(srcClient.SSHClient(), srcPath, tmpPath, false, false); err != nil {
		return fmt.Errorf("download from source: %w", err)
	}

	dstClient, err := rexssh.Connect(dstSess)
	if err != nil {
		return err
	}
	defer dstClient.Close()

	return rexsftp.Upload(dstClient.SSHClient(), tmpPath, dstPath, false, false)
}

func parseSessionPath(s string) (sess, path string, err error) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("expected session:/path, got %q", s)
	}
	return s[:idx], s[idx+1:], nil
}
