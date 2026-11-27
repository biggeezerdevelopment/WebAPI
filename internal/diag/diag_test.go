package diag

import (
	"io"
	"net"
	"testing"
	"time"
)

// TEMPORARY: does a pending read change how a large write to an unaccepted
// loopback connection completes?
func TestLoopbackAbsorb(t *testing.T) {
	for _, reader := range []bool{false, true} {
		for _, size := range []int{32 << 20, 128 << 20} {
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if reader {
				go func() { _, _ = io.Copy(io.Discard, c) }()
				time.Sleep(50 * time.Millisecond)
			}
			_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			start := time.Now()
			n, err := c.Write(make([]byte, size))
			t.Logf("reader=%v size=%dMiB: n=%d err=%v after %v", reader, size>>20, n, err, time.Since(start))
			c.Close()
			ln.Close()
		}
	}
}
