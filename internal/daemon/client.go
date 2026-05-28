package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

type Client struct {
	conn net.Conn
}

func Connect(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func (dc *Client) Close() error {
	return dc.conn.Close()
}

func (dc *Client) Exec(req ExecRequest) (int, error) {
	if err := WriteMsgJSON(dc.conn, MsgExecRequest, req); err != nil {
		return 1, err
	}
	return dc.streamIO(req.TTY)
}

func (dc *Client) Shell(req ShellRequest) (int, error) {
	if err := WriteMsgJSON(dc.conn, MsgShellRequest, req); err != nil {
		return 1, err
	}
	return dc.streamIO(true)
}

// streamIO pumps local stdin → daemon and daemon stdout/stderr → local outputs
// until the daemon sends an exit code or error message.
func (dc *Client) streamIO(isTTY bool) (int, error) {
	// Pump stdin → daemon in background.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				_ = WriteMsg(dc.conn, MsgStdin, buf[:n])
			}
			if err != nil {
				_ = WriteMsg(dc.conn, MsgStdinEOF, nil)
				return
			}
		}
	}()

	// Forward OS signals to the daemon.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			name := ""
			switch sig {
			case syscall.SIGINT:
				name = "INT"
			case syscall.SIGTERM:
				name = "TERM"
			}
			_ = WriteMsgJSON(dc.conn, MsgSignal, SignalMsg{Name: name})
		}
	}()
	defer signal.Stop(sigCh)

	// Forward terminal resize events to the daemon.
	if isTTY {
		winchCh := make(chan os.Signal, 1)
		signal.Notify(winchCh, syscall.SIGWINCH)
		go func() {
			for range winchCh {
				w, h, err := term.GetSize(int(os.Stdin.Fd()))
				if err == nil {
					_ = WriteMsgJSON(dc.conn, MsgResize, ResizeMsg{Width: w, Height: h})
				}
			}
		}()
		defer signal.Stop(winchCh)
	}

	// Read server responses until exit code or error.
	for {
		t, payload, err := ReadMsg(dc.conn)
		if err != nil {
			if err == io.EOF {
				return 1, fmt.Errorf("rexd disconnected unexpectedly")
			}
			return 1, err
		}
		switch t {
		case MsgStdout:
			_, _ = os.Stdout.Write(payload)
		case MsgStderr:
			_, _ = os.Stderr.Write(payload)
		case MsgExitCode:
			var exit ExitMsg
			if err := json.Unmarshal(payload, &exit); err != nil {
				return 1, err
			}
			return exit.Code, nil
		case MsgError:
			var e ErrorMsg
			_ = json.Unmarshal(payload, &e)
			return 1, fmt.Errorf("%s", e.Message)
		}
	}
}
