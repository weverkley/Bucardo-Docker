package logger

import (
	"io"
)

// MultiWriter acts as an io.Writer that duplicates its writes to all the
// provided writers, similar to io.MultiWriter but custom if needed.
type MultiWriter struct {
	writers []io.Writer
}

// NewMultiWriter creates a new MultiWriter.
func NewMultiWriter(writers ...io.Writer) *MultiWriter {
	return &MultiWriter{writers: writers}
}

func (t *MultiWriter) Write(p []byte) (n int, err error) {
	for _, w := range t.writers {
		n, err = w.Write(p)
		if err != nil {
			return
		}
		if n != len(p) {
			err = io.ErrShortWrite
			return
		}
	}
	return len(p), nil
}
