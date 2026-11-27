package diag

import (
	"net"
	"testing"
	"time"
)

// TEMPORARY: measure how much a loopback write to an unaccepted connection absorbs.
func TestLoopbackAbsorb(t *testing.T) {
	for _, chunk := range []int{1 << 20, 32 << 20} {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, chunk)
		total := 0
		start := time.Now()
		for total < 1<<30 {
			_ = c.SetWriteDeadline(time.Now().Add(time.Second))
			n, err := c.Write(buf)
			total += n
			if err != nil {
				t.Logf("chunk=%d: wrote %d bytes in %v before error %v", chunk, total, time.Since(start), err)
				break
			}
			t.Logf("chunk=%d: write ok, total %d after %v", chunk, total, time.Since(start))
		}
		c.Close()
		ln.Close()
	}
}
