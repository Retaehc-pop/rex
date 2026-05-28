package ssh

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
	"rex/config"
)

type Client struct {
	conn *ssh.Client
	jump *ssh.Client // non-nil when a jump host was used; closed alongside conn
}

// Connect opens an SSH connection, prompting the user interactively for unknown hosts.
func Connect(cfg config.SessionConfig) (*Client, error) {
	return connect(cfg, false)
}

// ConnectNoPrompt opens an SSH connection but refuses unknown hosts instead of
// prompting. Used by rexd (which has no terminal). rex falls back to Connect
// so the user can be prompted on first use.
func ConnectNoPrompt(cfg config.SessionConfig) (*Client, error) {
	return connect(cfg, true)
}

func connect(cfg config.SessionConfig, noPrompt bool) (*Client, error) {
	if cfg.JumpHost == "" {
		conn, err := dialDirect(cfg.Host, cfg.User, cfg.Port, cfg.Identity, noPrompt)
		if err != nil {
			return nil, err
		}
		return &Client{conn: conn}, nil
	}

	// Two-hop: local → jump host → target
	jumpPort := cfg.JumpPort
	if jumpPort == 0 {
		jumpPort = 22
	}
	jumpUser := cfg.JumpUser
	if jumpUser == "" {
		jumpUser = cfg.User
	}

	jumpConn, err := dialDirect(cfg.JumpHost, jumpUser, jumpPort, cfg.Identity, noPrompt)
	if err != nil {
		return nil, fmt.Errorf("jump host %s: %w", cfg.JumpHost, err)
	}

	// Open a forwarded TCP channel through the jump host to the target.
	targetAddr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	chanConn, err := jumpConn.Dial("tcp", targetAddr)
	if err != nil {
		jumpConn.Close()
		return nil, fmt.Errorf("reach %s via %s: %w", cfg.Host, cfg.JumpHost, err)
	}

	auth, err := authMethods(cfg.Identity)
	if err != nil {
		chanConn.Close()
		jumpConn.Close()
		return nil, err
	}
	hkc, err := makeHostKeyCallback(noPrompt)
	if err != nil {
		chanConn.Close()
		jumpConn.Close()
		return nil, err
	}

	ncc, chans, reqs, err := ssh.NewClientConn(chanConn, targetAddr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: hkc,
	})
	if err != nil {
		chanConn.Close()
		jumpConn.Close()
		return nil, fmt.Errorf("SSH to %s via %s: %w", cfg.Host, cfg.JumpHost, err)
	}

	return &Client{
		conn: ssh.NewClient(ncc, chans, reqs),
		jump: jumpConn,
	}, nil
}

// dialDirect opens a direct (non-proxied) SSH connection.
func dialDirect(host, user string, port int, identity string, noPrompt bool) (*ssh.Client, error) {
	auth, err := authMethods(identity)
	if err != nil {
		return nil, err
	}
	hkc, err := makeHostKeyCallback(noPrompt)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hkc,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return conn, nil
}

// makeHostKeyCallback builds a host-key verifier backed by ~/.ssh/known_hosts.
// When noPrompt is true, unknown hosts return an error instead of prompting.
func makeHostKeyCallback(noPrompt bool) (ssh.HostKeyCallback, error) {
	home, _ := os.UserHomeDir()
	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")

	if _, err := os.Stat(knownHostsPath); os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Dir(knownHostsPath), 0700)
		f, ferr := os.OpenFile(knownHostsPath, os.O_CREATE, 0600)
		if ferr == nil {
			f.Close()
		}
	}

	strictCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts: %w", err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := strictCallback(hostname, remote, key)
		if err == nil {
			return nil
		}

		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
			// Key mismatch — possible MITM, always reject.
			return err
		}

		if noPrompt {
			return fmt.Errorf("host %q not in known_hosts; connect directly with rex first to verify the key", hostname)
		}

		fingerprint := ssh.FingerprintSHA256(key)
		fmt.Fprintf(os.Stderr, "The authenticity of host %q can't be established.\n", hostname)
		fmt.Fprintf(os.Stderr, "%s key fingerprint is %s.\n", key.Type(), fingerprint)
		fmt.Fprintf(os.Stderr, "Are you sure you want to continue connecting (yes/no)? ")

		var response string
		fmt.Scanln(&response)
		if strings.ToLower(strings.TrimSpace(response)) != "yes" {
			return fmt.Errorf("host key verification failed")
		}

		return addToKnownHosts(knownHostsPath, hostname, key)
	}, nil
}

func addToKnownHosts(path, hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{hostname}, key)
	_, err = fmt.Fprintln(f, line)
	return err
}

func authMethods(identity string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		agentConn, err := net.Dial("unix", sock)
		if err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(agentConn).Signers))
		}
	}

	if identity != "" {
		if strings.HasPrefix(identity, "~") {
			home, _ := os.UserHomeDir()
			identity = home + identity[1:]
		}
		key, err := os.ReadFile(identity)
		if err == nil {
			signer, err := ssh.ParsePrivateKey(key)
			if err == nil {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no authentication methods available (no SSH_AUTH_SOCK, no identity file)")
	}
	return methods, nil
}

// Run executes cmd on the remote. Allocates a PTY when stdin is a terminal.
// Returns the remote process exit code.
func (c *Client) Run(cmd string) (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return 1, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	if isTTY {
		w, h, _ := term.GetSize(int(os.Stdin.Fd()))
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
		if err := sess.RequestPty("xterm-256color", h, w, modes); err != nil {
			return 1, fmt.Errorf("request pty: %w", err)
		}
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			defer term.Restore(int(os.Stdin.Fd()), oldState)
		}
		go watchResize(sess)
	}

	sess.Stdin = os.Stdin
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go forwardSignals(sess, sigCh)
	defer signal.Stop(sigCh)

	if err := sess.Run(cmd); err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			return exitErr.ExitStatus(), nil
		}
		return 1, err
	}
	return 0, nil
}

// Shell opens an interactive shell on the remote.
func (c *Client) Shell() (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return 1, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	w, h, _ := term.GetSize(int(os.Stdin.Fd()))
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", h, w, modes); err != nil {
		return 1, fmt.Errorf("request pty: %w", err)
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return 1, err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	go watchResize(sess)

	sess.Stdin = os.Stdin
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr

	if err := sess.Shell(); err != nil {
		return 1, err
	}
	if err := sess.Wait(); err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			return exitErr.ExitStatus(), nil
		}
		return 1, err
	}
	return 0, nil
}

// SSHClient exposes the underlying connection for SFTP use.
func (c *Client) SSHClient() *ssh.Client {
	return c.conn
}

func (c *Client) Close() error {
	err := c.conn.Close()
	if c.jump != nil {
		c.jump.Close()
	}
	return err
}

func forwardSignals(sess *ssh.Session, ch <-chan os.Signal) {
	for sig := range ch {
		var sshSig ssh.Signal
		switch sig {
		case syscall.SIGINT:
			sshSig = ssh.SIGINT
		case syscall.SIGTERM:
			sshSig = ssh.SIGTERM
		default:
			continue
		}
		_ = sess.Signal(sshSig)
	}
}

func watchResize(sess *ssh.Session) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for range ch {
		w, h, err := term.GetSize(int(os.Stdin.Fd()))
		if err == nil {
			_ = sess.WindowChange(h, w)
		}
	}
}
