package transport

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type countingConn struct {
	net.Conn
	writes atomic.Int32
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.writes.Add(1)
	return len(b), nil
}

func TestUDPResendBacksOffForAServerThatNeverAnswers(t *testing.T) {
	old := udpResendInterval
	udpResendInterval = 40 * time.Millisecond
	defer func() { udpResendInterval = old }()

	client, server := net.Pipe()
	defer server.Close()
	conn := &countingConn{Conn: client}
	pc := newPipelineConn(conn, NewPipelineTransport(PipelineOpts{}))
	defer pc.c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*udpResendInterval)
	defer cancel()
	if _, err := pc.exchange(ctx, newTestMsg(1, "never.answers.test")); err == nil {
		t.Fatal("expected a timeout")
	}
	if got := conn.writes.Load(); got != 3 {
		t.Fatalf("sent %d copies in 6 resend intervals, want 3 (at 0, 1 and 3 intervals)", got)
	}
}
