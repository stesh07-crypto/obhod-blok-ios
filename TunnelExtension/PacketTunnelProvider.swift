import NetworkExtension
import Foundation

/// Packet Tunnel Provider — runs in separate process, called by iOS VPN subsystem.
/// Communicates with the main app via App Group shared UserDefaults.
final class PacketTunnelProvider: NEPacketTunnelProvider {

    private let defaults = AppGroup.sharedDefaults
    private var statsTimer: Timer?

    // MARK: – Lifecycle

    override func startTunnel(
        options: [String: NSObject]?,
        completionHandler: @escaping (Error?) -> Void
    ) {
        NSLog("[WDTT-Ext] startTunnel called")

        guard
            let data = defaults?.data(forKey: AppGroup.Keys.activeProfileJSON),
            let profile = try? JSONDecoder().decode(ConnectionProfile.self, from: data)
        else {
            NSLog("[WDTT-Ext] No active profile found")
            completionHandler(TunnelError.noActiveProfile)
            return
        }

        // Set up Go callbacks FIRST
        GoClient.setLogHandler { line, isError in
            self.appendLog(line)
        }
        GoClient.setStatsHandler { [weak self] stats in
            self?.defaults?.set(stats, forKey: AppGroup.Keys.lastStats)
        }

        // Fetch dynamic user settings from AppGroup defaults
        let dns = self.defaults?.string(forKey: "goDnsMode") ?? "cloudflare"
        let obfsMode = self.defaults?.string(forKey: "obfsMode") ?? "tls"
        let vkAnonPath = self.defaults?.string(forKey: "vkAnonPath") ?? "/voice/join"
        let deviceID = self.protocolConfiguration.serverAddress ?? "unknown-ext"

        // Start Go tunnel engine while network interface is in standard state
        let result = GoClient.start(
            peer: profile.peer,
            hashes: profile.vkHashes,
            password: profile.password,
            port: profile.listenPort,
            workers: profile.workersPerHash,
            deviceID: deviceID,
            dns: dns,
            obfsMode: obfsMode,
            vkAnonPath: vkAnonPath
        )

        guard result == 0 else {
            completionHandler(TunnelError.startFailed)
            return
        }

        // Configure network settings
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: profile.peer)
        settings.mtu = 1420

        let ipv4 = NEIPv4Settings(addresses: ["10.77.0.2"], subnetMasks: ["255.255.255.0"])
        // Do NOT include default route to avoid intercepting Go traffic (GetCreds loop)
        ipv4.includedRoutes = []
        settings.ipv4Settings = ipv4

        // Setup PAC file to route traffic to SOCKS5 proxy raised by Go
        let proxySettings = NEProxySettings()
        proxySettings.autoProxyConfigurationEnabled = true
        let pacScript = """
        function FindProxyForURL(url, host) {
            return "SOCKS5 127.0.0.1:1080; SOCKS 127.0.0.1:1080; DIRECT";
        }
        """
        proxySettings.proxyAutoConfigurationJavaScript = pacScript
        proxySettings.excludeSimpleHostnames = true
        proxySettings.matchDomains = [""]
        settings.proxySettings = proxySettings

        let dnsSettings = NEDNSSettings(servers: ["1.1.1.1", "8.8.8.8"])
        dnsSettings.matchDomains = [""]
        settings.dnsSettings = dnsSettings

        setTunnelNetworkSettings(settings) { [weak self] error in
            guard let self else { return }
            if let error {
                NSLog("[WDTT-Ext] setTunnelNetworkSettings error: \(error)")
                completionHandler(error)
                return
            }

            self.defaults?.set(true, forKey: AppGroup.Keys.tunnelRunning)
            NSLog("[WDTT-Ext] Tunnel started successfully")
            completionHandler(nil)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        NSLog("[WDTT-Ext] stopTunnel: \(reason.rawValue)")
        GoClient.stop()
        statsTimer?.invalidate()
        defaults?.set(false, forKey: AppGroup.Keys.tunnelRunning)
        completionHandler()
    }

    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)?) {
        // Can be used for bidirectional IPC in the future
        completionHandler?(nil)
    }

    // MARK: – Private

    private func appendLog(_ line: String) {
        var lines = (defaults?.stringArray(forKey: AppGroup.Keys.lastLogLines) ?? [])
        lines.append(line)
        if lines.count > 100 { lines = Array(lines.suffix(100)) }
        defaults?.set(lines, forKey: AppGroup.Keys.lastLogLines)
    }
}

// MARK: – Errors

enum TunnelError: Error {
    case noActiveProfile
    case startFailed
}
