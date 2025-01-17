package self_test

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	quicproxy "github.com/quic-go/quic-go/integrationtests/tools/proxy"
	"github.com/quic-go/quic-go/internal/protocol"

	"github.com/stretchr/testify/require"
)

func TestConnectionCloseRetransmission(t *testing.T) {
	server, err := quic.Listen(
		newUPDConnLocalhost(t),
		getTLSConfig(),
		getQuicConfig(&quic.Config{DisablePathMTUDiscovery: true}),
	)
	require.NoError(t, err)
	defer server.Close()

	var drop atomic.Bool
	dropped := make(chan []byte, 100)
	proxy, err := quicproxy.NewQuicProxy("localhost:0", &quicproxy.Opts{
		RemoteAddr: fmt.Sprintf("localhost:%d", server.Addr().(*net.UDPAddr).Port),
		DelayPacket: func(quicproxy.Direction, []byte) time.Duration {
			return 5 * time.Millisecond // 10ms RTT
		},
		DropPacket: func(dir quicproxy.Direction, b []byte) bool {
			if drop := drop.Load(); drop && dir == quicproxy.DirectionOutgoing {
				dropped <- b
				return true
			}
			return false
		},
	})
	require.NoError(t, err)
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, newUPDConnLocalhost(t), proxy.LocalAddr(), getTLSClientConfig(), getQuicConfig(nil))
	require.NoError(t, err)
	defer conn.CloseWithError(0, "")

	sconn, err := server.Accept(ctx)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	drop.Store(true)
	sconn.CloseWithError(1337, "closing")

	// send 100 packets
	for i := 0; i < 100; i++ {
		str, err := conn.OpenStream()
		require.NoError(t, err)
		_, err = str.Write([]byte("foobar"))
		require.NoError(t, err)
		time.Sleep(time.Millisecond)
	}

	// Expect retransmissions of the CONNECTION_CLOSE for the
	// 1st, 2nd, 4th, 8th, 16th, 32th, 64th packet: 7 in total (+1 for the original packet)
	var packets [][]byte
	for i := 0; i < 8; i++ {
		select {
		case p := <-dropped:
			packets = append(packets, p)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for CONNECTION_CLOSE retransmission")
		}
	}

	// verify all retransmitted packets were identical
	for i := 1; i < len(packets); i++ {
		require.Equal(t, packets[0], packets[i])
	}
}

func TestDrainServerAcceptQueue(t *testing.T) {
	server, err := quic.Listen(newUPDConnLocalhost(t), getTLSConfig(), getQuicConfig(nil))
	require.NoError(t, err)
	defer server.Close()

	dialer := &quic.Transport{Conn: newUPDConnLocalhost(t)}
	defer dialer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// fill up the accept queue
	conns := make([]quic.Connection, 0, protocol.MaxAcceptQueueSize)
	for i := 0; i < protocol.MaxAcceptQueueSize; i++ {
		conn, err := dialer.Dial(ctx, server.Addr(), getTLSClientConfig(), getQuicConfig(nil))
		require.NoError(t, err)
		conns = append(conns, conn)
	}
	time.Sleep(scaleDuration(25 * time.Millisecond)) // wait for connections to be queued

	server.Close()
	for i := range protocol.MaxAcceptQueueSize {
		c, err := server.Accept(ctx)
		require.NoError(t, err)
		// make sure the connection is not closed
		require.NoError(t, conns[i].Context().Err(), "client connection closed")
		require.NoError(t, c.Context().Err(), "server connection closed")
		conns[i].CloseWithError(0, "")
		c.CloseWithError(0, "")
	}
	_, err = server.Accept(ctx)
	require.ErrorIs(t, err, quic.ErrServerClosed)
}
