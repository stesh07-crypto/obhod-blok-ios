//go:build ios

package main

import (
	"os"
	"sync"

	"golang.zx2c4.com/wireguard/tun"
)

type NativeTun struct {
	events    chan tun.Event
	mtu       int
	closed    chan struct{}
	closeOnce sync.Once
}

func NewNativeTun(mtu int) tun.Device {
	t := &NativeTun{
		events: make(chan tun.Event, 5),
		mtu:    mtu,
		closed: make(chan struct{}),
	}
	t.events <- tun.EventUp
	return t
}

func (t *NativeTun) File() *os.File { return nil }

func (t *NativeTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-t.closed:
		return 0, os.ErrClosed
	case pkt := <-packetsFromSwift:
		if len(bufs) == 0 || len(sizes) == 0 || len(bufs[0]) < offset+len(pkt) {
			return 0, nil
		}
		copy(bufs[0][offset:], pkt)
		sizes[0] = len(pkt)
		return 1, nil
	}
}

func (t *NativeTun) Write(bufs [][]byte, offset int) (int, error) {
	select {
	case <-t.closed:
		return 0, os.ErrClosed
	default:
	}

	for _, buf := range bufs {
		if offset < len(buf) {
			sendPacketToSwift(buf[offset:])
		}
	}
	return len(bufs), nil
}

func (t *NativeTun) Flush() error            { return nil }
func (t *NativeTun) MTU() (int, error)       { return t.mtu, nil }
func (t *NativeTun) Name() (string, error)   { return "ios", nil }
func (t *NativeTun) Events() <-chan tun.Event { return t.events }
func (t *NativeTun) BatchSize() int          { return 1 }

func (t *NativeTun) Close() error {
	t.closeOnce.Do(func() {
		select {
		case t.events <- tun.EventDown:
		default:
		}
		close(t.closed)
	})
	return nil
}
