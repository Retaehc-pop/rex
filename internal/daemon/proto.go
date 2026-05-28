package daemon

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// MsgType identifies the kind of framed message exchanged between rex and rexd.
type MsgType uint8

const (
	// client → server
	MsgExecRequest  MsgType = 1
	MsgShellRequest MsgType = 2
	MsgStdin        MsgType = 3
	MsgStdinEOF     MsgType = 4
	MsgResize       MsgType = 5
	MsgSignal       MsgType = 6

	// server → client
	MsgStdout   MsgType = 10
	MsgStderr   MsgType = 11
	MsgExitCode MsgType = 12
	MsgError    MsgType = 13
)

// Wire format: [1 byte type][4 bytes uint32 big-endian length][length bytes payload]

func WriteMsg(w io.Writer, t MsgType, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

func WriteMsgJSON(w io.Writer, t MsgType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteMsg(w, t, b)
}

func ReadMsg(r io.Reader) (MsgType, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	t := MsgType(hdr[0])
	n := binary.BigEndian.Uint32(hdr[1:])
	if n == 0 {
		return t, nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return t, payload, nil
}

// SocketPath returns the Unix socket path for rexd.
// Uses $XDG_RUNTIME_DIR/rex/rexd.sock, falling back to /tmp/rex-$UID/rexd.sock.
func SocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir != "" {
		return filepath.Join(dir, "rex", "rexd.sock")
	}
	return filepath.Join(os.TempDir(), "rex-"+strconv.Itoa(os.Getuid()), "rexd.sock")
}

// JSON payload types

type ExecRequest struct {
	Session string `json:"session"`
	Cmd     string `json:"cmd"`
	TTY     bool   `json:"tty"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type ShellRequest struct {
	Session string `json:"session"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type ResizeMsg struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type SignalMsg struct {
	Name string `json:"name"` // "INT", "TERM"
}

type ExitMsg struct {
	Code int `json:"code"`
}

type ErrorMsg struct {
	Message string `json:"message"`
}
