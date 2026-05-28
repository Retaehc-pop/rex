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
}

// Connect opens an SSH connection, prompting the user interactively for
// unknown hosts.
func Connect(cfg config.SessionConfig) (*Client, error) {
	return connect(cfg, false)
}

// ConnectNoPrompt opens an SSH connection but refuses unknown hosts instead of
// prompting. Used by rexd (which has no terminal). If the host is unknown,
// the caller should fall back to Connect so the user can be prompted.
func ConnectNoPrompt(cfg config.SessionConfig) (*Client, error) {
	return connect(cfg, true)
}

func connect(cfg config.SessionConfig, noPrompt bool) (*Client, error) {
	auth, err := authMethods(cfg.Identity)
	if err != nil {
		return nil, err
	}

	home, _ := os.UserHomeDir()
	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")

	// Ensure known_hosts exists so knownhosts.New doesn't fail on fresh systems.
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

	hostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
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

		// Unknown host — prompt user.
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
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return &Client{conn: conn}, nil
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
	return c.conn.Close()
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
