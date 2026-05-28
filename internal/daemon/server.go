package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
	rexssh "rex/internal/ssh"
	"rex/internal/session"
)

const idleTimeout = 30 * time.Minute

type Server struct {
	socketPath string
	cfgPath    string

	mu       sync.Mutex
	pool     map[string]*rexssh.Client // keyed by session name; Close() tears down jump too
	lastUsed time.Time
}

func NewServer(socketPath, cfgPath string) *Server {
	return &Server{
		socketPath: socketPath,
		cfgPath:    cfgPath,
		pool:       make(map[string]*rexssh.Client),
		lastUsed:   time.Now(),
	}
}

func (s *Server) Run() error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0700); err != nil {
		return err
	}
	_ = os.Remove(s.socketPath) // remove stale socket

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	_ = os.Chmod(s.socketPath, 0600)

	log.Printf("rexd listening on %s", s.socketPath)

	go s.idleWatcher(ln)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed
		}
		s.mu.Lock()
		s.lastUsed = time.Now()
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *Server) idleWatcher(ln net.Listener) {
	for range time.Tick(time.Minute) {
		s.mu.Lock()
		idle := time.Since(s.lastUsed)
		s.mu.Unlock()
		if idle > idleTimeout {
			log.Printf("idle for %v, shutting down", idle.Round(time.Second))
			ln.Close()
			return
		}
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	t, payload, err := ReadMsg(conn)
	if err != nil {
		return
	}
	switch t {
	case MsgExecRequest:
		var req ExecRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			s.sendError(conn, err)
			return
		}
		sshConn, err := s.getConn(req.Session)
		if err != nil {
			s.sendError(conn, err)
			return
		}
		s.runSession(conn, sshConn, req.TTY, req.Width, req.Height,
			func(sess *gossh.Session) error { return sess.Run(req.Cmd) })

	case MsgShellRequest:
		var req ShellRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			s.sendError(conn, err)
			return
		}
		sshConn, err := s.getConn(req.Session)
		if err != nil {
			s.sendError(conn, err)
			return
		}
		s.runSession(conn, sshConn, true, req.Width, req.Height,
			func(sess *gossh.Session) error {
				if err := sess.Shell(); err != nil {
					return err
				}
				return sess.Wait()
			})

	default:
		s.sendError(conn, fmt.Errorf("unexpected message type %d", t))
	}
}

// getConn returns the underlying *gossh.Client for an existing or new pooled connection.
func (s *Server) getConn(sessionName string) (*gossh.Client, error) {
	s.mu.Lock()
	if c, ok := s.pool[sessionName]; ok {
		_, _, err := c.SSHClient().SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
			s.mu.Unlock()
			return c.SSHClient(), nil
		}
		c.Close() // closes both target and jump connections
		delete(s.pool, sessionName)
	}
	s.mu.Unlock()

	// Establish a new connection without the mutex held (may be slow).
	cfg, err := session.Load(s.cfgPath)
	if err != nil {
		return nil, err
	}
	sessCfg, err := session.Get(cfg, sessionName)
	if err != nil {
		return nil, err
	}
	// No-prompt: unknown hosts return an error. rex falls back to direct SSH
	// for the first connection so the user can be prompted interactively.
	client, err := rexssh.ConnectNoPrompt(sessCfg)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if existing, ok := s.pool[sessionName]; ok {
		// Another goroutine won the race; discard ours.
		client.Close()
		client = existing
	} else {
		s.pool[sessionName] = client
	}
	s.mu.Unlock()

	return client.SSHClient(), nil
}

func (s *Server) runSession(conn net.Conn, sshConn *gossh.Client, tty bool, w, h int, start func(*gossh.Session) error) {
	sess, err := sshConn.NewSession()
	if err != nil {
		s.sendError(conn, err)
		return
	}
	defer sess.Close()

	if tty {
		modes := gossh.TerminalModes{gossh.ECHO: 1, gossh.TTY_OP_ISPEED: 14400, gossh.TTY_OP_OSPEED: 14400}
		if err := sess.RequestPty("xterm-256color", h, w, modes); err != nil {
			s.sendError(conn, err)
			return
		}
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	sess.Stdin = stdinR
	sess.Stdout = stdoutW
	sess.Stderr = stderrW

	go pumpOut(conn, MsgStdout, stdoutR)
	go pumpOut(conn, MsgStderr, stderrR)

	done := make(chan error, 1)
	go func() {
		err := start(sess)
		stdoutW.Close()
		stderrW.Close()
		done <- err
	}()

	go readClientIO(conn, sess, stdinW)

	runErr := <-done
	exitCode := exitCodeFrom(runErr)
	_ = WriteMsgJSON(conn, MsgExitCode, ExitMsg{Code: exitCode})
}

func (s *Server) sendError(conn net.Conn, err error) {
	_ = WriteMsgJSON(conn, MsgError, ErrorMsg{Message: err.Error()})
}

func pumpOut(conn net.Conn, t MsgType, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_ = WriteMsg(conn, t, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// readClientIO forwards stdin bytes, resize events, and signals from the rex
// side (conn) into the live SSH session.
func readClientIO(conn net.Conn, sess *gossh.Session, stdinW *io.PipeWriter) {
	defer stdinW.Close()
	for {
		t, payload, err := ReadMsg(conn)
		if err != nil {
			return
		}
		switch t {
		case MsgStdin:
			if _, err := stdinW.Write(payload); err != nil {
				return
			}
		case MsgStdinEOF:
			return
		case MsgResize:
			var r ResizeMsg
			if json.Unmarshal(payload, &r) == nil {
				_ = sess.WindowChange(r.Height, r.Width)
			}
		case MsgSignal:
			var sig SignalMsg
			if json.Unmarshal(payload, &sig) == nil {
				var sshSig gossh.Signal
				switch sig.Name {
				case "INT":
					sshSig = gossh.SIGINT
				case "TERM":
					sshSig = gossh.SIGTERM
				}
				if sshSig != "" {
					_ = sess.Signal(sshSig)
				}
			}
		}
	}
}

func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*gossh.ExitError); ok {
		return exitErr.ExitStatus()
	}
	return 1
}
