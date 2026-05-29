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

// Client wraps the final SSH connection plus any intermediate hop connections
// that must be kept alive. chain[0] is the first hop, chain[N-1] is the
// second-to-last hop; conn is the target.
type Client struct {
	conn  *ssh.Client
	chain []*ssh.Client
}

// Connect opens the SSH connection chain, prompting the user interactively for
// unknown hosts.
func Connect(cfg config.SessionConfig) (*Client, error) {
	return connect(cfg, false)
}

// ConnectNoPrompt opens the SSH connection chain but refuses unknown hosts
// instead of prompting. Used by rexd (which has no terminal). rex falls back
// to Connect so the user can be prompted on first use.
func ConnectNoPrompt(cfg config.SessionConfig) (*Client, error) {
	return connect(cfg, true)
}

func connect(cfg config.SessionConfig, noPrompt bool) (*Client, error) {
	if len(cfg.Nodes) == 0 {
		return nil, fmt.Errorf("session has no nodes configured")
	}

	hkc, err := makeHostKeyCallback(noPrompt)
	if err != nil {
		return nil, err
	}

	// Dial the first node directly over TCP.
	first := cfg.Nodes[0]
	conn, err := dialDirect(first.Host, first.User, effectivePort(first.Port), first.Identity, hkc, noPrompt)
	if err != nil {
		return nil, err
	}

	// For each subsequent node, tunnel through the previous connection.
	chain := make([]*ssh.Client, 0, len(cfg.Nodes)-1)
	for i, node := range cfg.Nodes[1:] {
		port := effectivePort(node.Port)
		targetAddr := net.JoinHostPort(node.Host, strconv.Itoa(port))

		chanConn, err := conn.Dial("tcp", targetAddr)
		if err != nil {
			closeChain(conn, chain)
			return nil, fmt.Errorf("hop %d: reach %s: %w", i+2, node.Host, err)
		}

		auth, err := authMethods(node.Identity, noPrompt)
		if err != nil {
			chanConn.Close()
			closeChain(conn, chain)
			return nil, fmt.Errorf("hop %d auth: %w", i+2, err)
		}

		ncc, chans, reqs, err := ssh.NewClientConn(chanConn, targetAddr, &ssh.ClientConfig{
			User:            node.User,
			Auth:            auth,
			HostKeyCallback: hkc,
		})
		if err != nil {
			chanConn.Close()
			closeChain(conn, chain)
			return nil, fmt.Errorf("SSH to %s: %w", node.Host, err)
		}

		chain = append(chain, conn)
		conn = ssh.NewClient(ncc, chans, reqs)
	}

	return &Client{conn: conn, chain: chain}, nil
}

// dialDirect opens a single direct TCP+SSH connection.
func dialDirect(host, user string, port int, identity string, hkc ssh.HostKeyCallback, noPrompt bool) (*ssh.Client, error) {
	auth, err := authMethods(identity, noPrompt)
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
			return err // key mismatch — possible MITM, always reject
		}

		// New host — add automatically (TOFU: trust on first use).
		// Key mismatches are rejected above, so this is safe for first connections.
		if !noPrompt {
			fmt.Fprintf(os.Stderr, "Warning: Permanently added %q (%s) to the list of known hosts.\n", hostname, key.Type())
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

// loadKey reads and parses a private key file, returning nil if the file does
// not exist or cannot be parsed (e.g. encrypted with a passphrase).
func loadKey(path string) ssh.Signer {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil
	}
	return s
}

func authMethods(identity string, noPrompt bool) ([]ssh.AuthMethod, error) {
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
		if s := loadKey(identity); s != nil {
			methods = append(methods, ssh.PublicKeys(s))
		}
	} else {
		// No explicit identity — try the same defaults as the ssh command.
		home, _ := os.UserHomeDir()
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_dsa"} {
			if s := loadKey(filepath.Join(home, ".ssh", name)); s != nil {
				methods = append(methods, ssh.PublicKeys(s))
			}
		}
	}

	// Allow the server to send password/OTP/confirmation prompts during auth.
	// Skipped in no-prompt mode (daemon has no terminal).
	if !noPrompt {
		methods = append(methods, ssh.KeyboardInteractive(keyboardInteractiveChallenge))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no authentication methods available (no SSH_AUTH_SOCK, no identity file)")
	}
	return methods, nil
}

// keyboardInteractiveChallenge handles server-side auth prompts (passwords,
// OTPs, yes/no confirmations). Echoed questions use plain Scanln; hidden ones
// use term.ReadPassword so the input is not shown.
func keyboardInteractiveChallenge(name, instruction string, questions []string, echos []bool) ([]string, error) {
	if name != "" {
		fmt.Fprintln(os.Stderr, name)
	}
	if instruction != "" {
		fmt.Fprintln(os.Stderr, instruction)
	}
	answers := make([]string, len(questions))
	for i, q := range questions {
		fmt.Fprint(os.Stderr, q)
		if echos[i] {
			fmt.Fscan(os.Stdin, &answers[i])
		} else {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			if err != nil {
				return nil, err
			}
			answers[i] = string(b)
			fmt.Fprintln(os.Stderr)
		}
	}
	return answers, nil
}

func effectivePort(p int) int {
	if p == 0 {
		return 22
	}
	return p
}

// closeChain closes last and then each connection in chain in reverse order.
func closeChain(last *ssh.Client, chain []*ssh.Client) {
	last.Close()
	for i := len(chain) - 1; i >= 0; i-- {
		chain[i].Close()
	}
}

// Run executes cmd on the remote. Allocates a PTY when stdin is a terminal.
// Returns the remote process exit code.
func (c *Client) Run(cmd string) (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return 1, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	if term.IsTerminal(int(os.Stdin.Fd())) {
		restore, err := setupPTY(sess)
		if err != nil {
			return 1, err
		}
		defer restore()
	}

	sess.Stdin, sess.Stdout, sess.Stderr = os.Stdin, os.Stdout, os.Stderr

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go forwardSignals(sess, sigCh)

	return exitCode(sess.Run(cmd))
}

// Shell opens an interactive shell on the remote.
func (c *Client) Shell() (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return 1, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	restore, err := setupPTY(sess)
	if err != nil {
		return 1, err
	}
	defer restore()

	sess.Stdin, sess.Stdout, sess.Stderr = os.Stdin, os.Stdout, os.Stderr

	if err := sess.Shell(); err != nil {
		return 1, err
	}
	return exitCode(sess.Wait())
}

// setupPTY allocates a PTY on the remote, puts the local terminal in raw mode,
// and forwards window resize events. The returned restore function is always
// non-nil and should be deferred by the caller.
func setupPTY(sess *ssh.Session) (restore func(), err error) {
	fd := int(os.Stdin.Fd())
	w, h, _ := term.GetSize(fd)
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", h, w, modes); err != nil {
		return func() {}, fmt.Errorf("request pty: %w", err)
	}
	go watchResize(sess)
	if oldState, err := term.MakeRaw(fd); err == nil {
		return func() { term.Restore(fd, oldState) }, nil
	}
	return func() {}, nil
}

// exitCode converts a session error into a (code, error) pair, treating a
// remote non-zero exit as a normal return rather than an error.
func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		return exitErr.ExitStatus(), nil
	}
	return 1, err
}

// SSHClient exposes the underlying target connection for SFTP use.
func (c *Client) SSHClient() *ssh.Client {
	return c.conn
}

// Close closes the target connection and all intermediate hop connections in
// reverse order so each tunnel stays open until the connection through it
// is done.
func (c *Client) Close() error {
	err := c.conn.Close()
	for i := len(c.chain) - 1; i >= 0; i-- {
		c.chain[i].Close()
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
