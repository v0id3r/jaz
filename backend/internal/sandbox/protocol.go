package sandbox

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Request is the first frame of a runner connection; afterwards the
// connection carries the process's stdio, signals and exit status as frames.
type Request struct {
	Args      []string `json:"args"`
	Env       []string `json:"env"`
	Dir       string   `json:"dir"`
	ReadOnly  []string `json:"readOnly,omitempty"`
	ReadWrite []string `json:"readWrite,omitempty"`
}

const (
	frameRequest byte = iota + 1
	frameStdin
	frameStdout
	frameStderr
	frameCloseStdin
	frameSignal
	frameExit
)

const maxFramePayload = 1 << 20

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) write(kind byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	header := [5]byte{kind}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := f.w.Write(header[:]); err != nil {
		return err
	}
	_, err := f.w.Write(payload)
	return err
}

func (f *frameWriter) writeInt(kind byte, value int) error {
	return f.write(kind, binary.BigEndian.AppendUint32(nil, uint32(int32(value))))
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maxFramePayload {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the limit", size)
	}
	payload := make([]byte, size)
	_, err := io.ReadFull(r, payload)
	return header[0], payload, err
}

func frameInt(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("malformed integer frame")
	}
	return int(int32(binary.BigEndian.Uint32(payload))), nil
}

// pump copies r into frames of the given kind until r is exhausted.
func pump(dst *frameWriter, kind byte, r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := dst.write(kind, buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
