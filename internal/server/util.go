package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/22or/2nnel/internal/proto"
)

// pipe copies bytes bidirectionally between a and b until either closes.
func pipe(a, b io.ReadWriter) {
	pipeCount(a, b, nil)
}

// pipeCount copies bidirectionally and optionally updates te's byte counters.
// te may be nil (no counting).
func pipeCount(a, b io.ReadWriter, te *tunnelEntry) {
	done := make(chan struct{}, 2)

	cp := func(dst io.Writer, src io.Reader, counter *func(int64)) {
		n, _ := io.Copy(dst, src)
		if counter != nil {
			(*counter)(n)
		}
		done <- struct{}{}
	}

	if te != nil {
		addIn := func(n int64) { te.bytesIn.Add(n) }
		addOut := func(n int64) { te.bytesOut.Add(n) }
		go cp(b, a, &addIn) // a→b = traffic into local service
		go cp(a, b, &addOut) // b→a = traffic out to public
	} else {
		go cp(b, a, nil)
		go cp(a, b, nil)
	}
	<-done
}

// marshalLine serialises v to JSON and appends '\n'.
func marshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// writeStreamHdr writes a proto.StreamHeader as the first line on w.
func writeStreamHdr(w io.Writer, tunnelName, localAddr string) error {
	b, err := marshalLine(proto.StreamHeader{TunnelName: tunnelName, LocalAddr: localAddr})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// newID returns a random 16-byte hex string.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fmtBytes formats byte counts for display.
func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
