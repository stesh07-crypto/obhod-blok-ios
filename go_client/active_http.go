package main

import (
	"log"
	"sync"

	tlsclient "github.com/bogdanfinn/tls-client"
)

var (
	activeHttpClientsMu sync.Mutex
	activeHttpClients   []tlsclient.HttpClient
)

func registerActiveHttpClient(c tlsclient.HttpClient) {
	if c == nil {
		return
	}
	activeHttpClientsMu.Lock()
	defer activeHttpClientsMu.Unlock()
	activeHttpClients = append(activeHttpClients, c)
}

func unregisterActiveHttpClient(c tlsclient.HttpClient) {
	if c == nil {
		return
	}
	activeHttpClientsMu.Lock()
	defer activeHttpClientsMu.Unlock()
	for i, existing := range activeHttpClients {
		if existing == c {
			activeHttpClients = append(activeHttpClients[:i], activeHttpClients[i+1:]...)
			break
		}
	}
}

// RotateVKHttpClients shuts down all idle/pooled HTTP/2 connections across
// physical network interface transitions (e.g. Wi-Fi <-> Cellular) so requests
// do not hang for 20-65 seconds on stale connections bound to dead interfaces.
func RotateVKHttpClients(reason string) {
	activeHttpClientsMu.Lock()
	clients := append([]tlsclient.HttpClient(nil), activeHttpClients...)
	activeHttpClients = nil
	activeHttpClientsMu.Unlock()

	if len(clients) > 0 {
		log.Printf("[СЕТЬ] Сброс %d активных HTTP-клиентов (%s)", len(clients), reason)
		for _, c := range clients {
			client := c
			go func() {
				client.CloseIdleConnections()
			}()
		}
	}
}
