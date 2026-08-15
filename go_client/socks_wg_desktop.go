//go:build !ios

package main

import (
	"fmt"
	"log"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// startUserspaceWireGuard поднимает WG через netstack (для десктопа/клиента).
func startUserspaceWireGuard(conf string) (*device.Device, *netstack.Net, error) {
	cfg, err := parseWgQuick(conf)
	if err != nil {
		return nil, nil, err
	}
	tunDev, tnet, err := netstack.CreateNetTUN(cfg.addresses, cfg.dns, cfg.mtu)
	if err != nil {
		return nil, nil, err
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "[WG] "))
	if err := dev.IpcSet(cfg.ipcRequest()); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("IpcSet: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("Up: %w", err)
	}
	log.Printf("[WG] Userspace WireGuard up (addr=%v endpoint=%s)", cfg.addresses, cfg.endpoint)
	return dev, tnet, nil
}
