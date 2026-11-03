// Package logx builds the node logger: stdout plus a size-capped file that `d256-node logs` reads.
package logx

import (
	"io"
	"log/slog"
	"os"
	"sync"
)

const maxLogBytes = 5 << 20

// rotatingFile keeps node.log below maxLogBytes by moving it to node.log.1.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openRotating(path string) (*rotatingFile, error) {
	r := &rotatingFile{path: path}
	return r, r.open()
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > maxLogBytes {
		r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// New returns a logger writing JSON or text to stdout and, when logPath is writable, to the log file.
func New(format, logPath string) *slog.Logger {
	var w io.Writer = os.Stdout
	if logPath != "" {
		if rf, err := openRotating(logPath); err == nil {
			w = io.MultiWriter(os.Stdout, rf)
		}
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, nil))
	}
	return slog.New(slog.NewTextHandler(w, nil))
}
