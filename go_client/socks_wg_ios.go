//go:build ios

package main

import (
	"fmt"
	"log"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// startUserspaceWireGuard поднимает WG через NativeTun (для iOS).
func startUserspaceWireGuard(conf string) (*device.Device, *netstack.Net, error) {
	cfg, err := parseWgQuick(conf)
	if err != nil {
		return nil, nil, err
	}
	tunDev := NewNativeTun(cfg.mtu)
	
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "[WG-IOS] "))
	if err := dev.IpcSet(cfg.ipcRequest()); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("IpcSet: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("Up: %w", err)
	}
	log.Printf("[IOS-TUN] Userspace WireGuard up (addr=%v endpoint=%s)", cfg.addresses, cfg.endpoint)
	return dev, nil, nil
}
