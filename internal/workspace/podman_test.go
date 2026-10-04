package workspace

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTransportOutputSubprocess(t *testing.T) {
	value := os.Getenv("MCP_TRANSPORT_TEST_SIZE")
	if value == "" {
		return
	}
	size, err := strconv.Atoi(value)
	if err != nil || size < 0 {
		os.Exit(2)
	}
	if _, err := io.WriteString(os.Stdout, strings.Repeat("x", size)); err != nil {
		os.Exit(1)
	}
	if _, err := io.WriteString(os.Stderr, strings.Repeat("y", size)); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestBoundedTransportSubprocess(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 3, 4, 5, 9, 300000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestTransportOutputSubprocess$")
			cmd.Env = append(os.Environ(), "MCP_TRANSPORT_TEST_SIZE="+strconv.Itoa(size))
			stdout := &limitedBuffer{limit: 4}
			stderr := &limitedBuffer{limit: 5}
			// Match Operate's os/exec pipe-copy path, including io.Copy's
			// optional ReaderFrom optimization; direct Write tests miss it.
			cmd.Stdout, cmd.Stderr = stdout, stderr
			cmd.WaitDelay = time.Second
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			for _, stream := range []struct {
				name      string
				buffer    *limitedBuffer
				character string
			}{{"stdout", stdout, "x"}, {"stderr", stderr, "y"}} {
				want := strings.Repeat(stream.character, min(size, int(stream.buffer.limit)))
				if stream.buffer.String() != want || stream.buffer.overflow != (int64(size) > stream.buffer.limit) {
					t.Errorf("%s: retained %d bytes, overflow=%v; want %d bytes, overflow=%v", stream.name, len(stream.buffer.Bytes()), stream.buffer.overflow, len(want), int64(size) > stream.buffer.limit)
				}
			}
		})
	}
}
