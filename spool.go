package history

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Spool buffers a Change stream to a temporary file so it can be replayed
// after a separate blob-scanning pass has built its index.
type Spool struct {
	file   *os.File
	writer *bufio.Writer
	err    error
}

// NewSpool creates a temporary spool file. Prefix is used in the temp file
// name for debugging.
func NewSpool(prefix string) (*Spool, error) {
	file, err := os.CreateTemp("", prefix+"-history-*")
	if err != nil {
		return nil, err
	}
	return &Spool{file: file, writer: bufio.NewWriterSize(file, readerBufferSize)}, nil
}

func spoolFields(c *Change) []*string {
	return []*string{&c.Commit, &c.Date, &c.Subject, &c.OldOID, &c.NewOID, &c.Path, &c.OldMode, &c.NewMode}
}

// Write appends a change to the spool.
func (s *Spool) Write(c Change) {
	if s.err != nil {
		return
	}
	for _, field := range spoolFields(&c) {
		if err := writeSpoolString(s.writer, *field); err != nil {
			s.err = err
			return
		}
	}
}

// Ready flushes the spool and rewinds it for Replay.
func (s *Spool) Ready() error {
	if s.err != nil {
		return s.err
	}
	if err := s.writer.Flush(); err != nil {
		return err
	}
	_, err := s.file.Seek(0, io.SeekStart)
	return err
}

// Replay visits every change written before Ready.
func (s *Spool) Replay(visit func(Change)) error {
	reader := bufio.NewReaderSize(s.file, readerBufferSize)
	for {
		var c Change
		fields := spoolFields(&c)
		first, err := readSpoolString(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		*fields[0] = first
		for _, field := range fields[1:] {
			if *field, err = readSpoolString(reader); err != nil {
				return err
			}
		}
		visit(c)
	}
}

// Close removes the spool file.
func (s *Spool) Close() {
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
}

func writeSpoolString(writer io.Writer, value string) error {
	var encoded [binary.MaxVarintLen64]byte
	size := binary.PutUvarint(encoded[:], uint64(len(value)))
	if _, err := writer.Write(encoded[:size]); err != nil {
		return err
	}
	_, err := io.WriteString(writer, value)
	return err
}

func readSpoolString(reader *bufio.Reader) (string, error) {
	size, err := binary.ReadUvarint(reader)
	if err != nil {
		return "", err
	}
	if size > uint64(int(^uint(0)>>1)) {
		return "", fmt.Errorf("history field is too large: %d", size)
	}
	value := make([]byte, int(size))
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return string(value), nil
}
