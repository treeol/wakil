package hisn

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// kvrWait bounds how long Wakil waits for the staging service to answer
// (a variable only so tests can shorten it).
var kvrWait = 5 * time.Second

// PingKVR polls the kvr socket with the wire PING (4-byte big-endian length 1,
// opcode 0x03; reply length 1, 0x10 = OK) until it answers or timeout passes.
// The staging client package is not imported here: this is the same handshake
// DockerExecutor's waitForKVR performs.
func PingKVR(socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if err := pingOnce(socket); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no answer within %s: %w", timeout, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func pingOnce(socket string) error {
	c, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte{0, 0, 0, 1, 3}); err != nil {
		return err
	}
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > 1<<16 {
		return fmt.Errorf("invalid response length %d", n)
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(c, resp); err != nil {
		return err
	}
	if resp[0] != 0x10 {
		return fmt.Errorf("unexpected response opcode 0x%02x", resp[0])
	}
	return nil
}
