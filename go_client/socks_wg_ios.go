//go:build ios

package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const iosTunFDScanLimit = 1024

// findIOSTunFileDescriptor locates the utun socket created by
// NEPacketTunnelProvider after setTunnelNetworkSettings succeeds. We use the
// kernel control socket option that wireguard-go itself uses to identify utun
// interfaces instead of routing every IP packet through Swift callbacks.
func findIOSTunFileDescriptor() (int, string, error) {
	for fd := 0; fd <= iosTunFDScanLimit; fd++ {
		name, err := unix.GetsockoptString(
			fd,
			2, // SYSPROTO_CONTROL
			2, // UTUN_OPT_IFNAME
		)
		if err == nil && strings.HasPrefix(name, "utun") {
			return fd, name, nil
		}
	}
	return -1, "", fmt.Errorf("utun file descriptor not found")
}

// startUserspaceWireGuard attaches wireguard-go directly to iOS' utun fd.
// The fd is duplicated so closing/recreating WireGuard never closes the
// NetworkExtension-owned descriptor. This removes the old per-packet
// Go -> C -> Swift Data -> NEPacketTunnelFlow data path.
func startUserspaceWireGuard(conf string) (*device.Device, *netstack.Net, error) {
	cfg, err := parseWgQuick(conf)
	if err != nil {
		return nil, nil, err
	}

	tunFD, tunName, err := findIOSTunFileDescriptor()
	if err != nil {
		return nil, nil, fmt.Errorf("find iOS TUN: %w", err)
	}

	dupFD, err := unix.Dup(tunFD)
	if err != nil {
		return nil, nil, fmt.Errorf("dup iOS TUN fd %d: %w", tunFD, err)
	}
	if err := unix.SetNonblock(dupFD, true); err != nil {
		_ = unix.Close(dupFD)
		return nil, nil, fmt.Errorf("set iOS TUN nonblock: %w", err)
	}

	tunFile := os.NewFile(uintptr(dupFD), tunName)
	if tunFile == nil {
		_ = unix.Close(dupFD)
		return nil, nil, fmt.Errorf("wrap iOS TUN fd %d", dupFD)
	}

	// MTU is already installed by NEPacketTunnelNetworkSettings. Passing 0
	// preserves that iOS-owned value while CreateTUNFromFile still exposes the
	// real interface MTU to wireguard-go.
	tunDev, err := tun.CreateTUNFromFile(tunFile, 0)
	if err != nil {
		_ = tunFile.Close()
		return nil, nil, fmt.Errorf("CreateTUNFromFile(%s): %w", tunName, err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "[WG-IOS] "))
	if err := dev.IpcSet(cfg.ipcRequest()); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("IpcSet: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("Up: %w", err)
	}

	log.Printf("[IOS-TUN] Direct TUN WireGuard up (if=%s sourceFD=%d addr=%v endpoint=%s)", tunName, tunFD, cfg.addresses, cfg.endpoint)
	return dev, nil, nil
}
