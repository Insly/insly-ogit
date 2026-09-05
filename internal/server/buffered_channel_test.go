package server

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

type brokenChannel struct {
	ssh.Channel
	closed bool
}

func (*brokenChannel) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (c *brokenChannel) Close() error            { c.closed = true; return nil }

func TestBufferedChannelReportsFailedDelivery(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		raw := &brokenChannel{}
		channel := newBufferedChannel(raw)
		_, err := channel.Write([]byte("Git packet"))
		require.True(t, errors.Is(err, io.ErrClosedPipe), "failed flush must not report successful Git delivery")
	})
	t.Run("close", func(t *testing.T) {
		raw := &brokenChannel{}
		channel := newBufferedChannel(raw)
		_, err := channel.writer.Write([]byte("pending Git packet"))
		require.NoError(t, err)
		require.ErrorIs(t, channel.Close(), io.ErrClosedPipe)
		require.True(t, raw.closed, "transport must still close when flushing fails")
	})
}
