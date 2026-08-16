import Foundation

// MARK: - Network configuration received from the Go bootstrap

struct GoTunnelNetworkConfiguration: Decodable {
    let ipv4Address: String?
    let ipv4Prefix: Int?
    let ipv6Address: String?
    let ipv6Prefix: Int?
    let dnsServers: [String]
    let mtu: Int
}

// MARK: - Swift wrapper over libwdttclient.a (Go C exports)

/// Bridges Swift <-> Go via C FFI.
///
/// In network-core-v2 the full Go runtime is owned by TunnelExtension only.
/// The main application no longer starts or stops this runtime directly.
enum GoClient {

    // MARK: Callback storage

    private static var logHandler: ((String, Bool) -> Void)?
    private static var statsHandler: ((String) -> Void)?
    private static var packetHandler: ((Data) -> Void)?

    // MARK: Setup

    static func setLogHandler(_ handler: @escaping (String, Bool) -> Void) {
        logHandler = handler
        WDTT_SetLogCallback { linePtr, isError in
            guard let ptr = linePtr else { return }
            let line = String(cString: ptr)
            GoClient.logHandler?(line, isError != 0)
        }
    }

    static func setStatsHandler(_ handler: @escaping (String) -> Void) {
        statsHandler = handler
        WDTT_SetStatsCallback { statsPtr in
            guard let ptr = statsPtr else { return }
            GoClient.statsHandler?(String(cString: ptr))
        }
    }

    static func setPacketHandler(_ handler: @escaping (Data) -> Void) {
        packetHandler = handler
        let callback: @convention(c) (UnsafeRawPointer?, Int32) -> Void = { dataPtr, length in
            guard let ptr = dataPtr, length > 0 else { return }
            GoClient.packetHandler?(Data(bytes: ptr, count: Int(length)))
        }
        WDTT_SetWriteCallback(callback)
    }

    static func clearPacketHandler() {
        packetHandler = nil
        WDTT_SetWriteCallback(nil)
    }

    static func writePacket(_ data: Data) {
        data.withUnsafeBytes { rawBuffer in
            guard let baseAddress = rawBuffer.baseAddress, !data.isEmpty else { return }
            WDTT_WritePacket(UnsafeMutableRawPointer(mutating: baseAddress), Int32(data.count))
        }
    }

    // MARK: Runtime control

    /// Starts the transport bootstrap. A return value of zero means that the
    /// asynchronous bootstrap was accepted, not that the VPN is already ready.
    @discardableResult
    static func start(
        peer: String,
        hashes: String,
        password: String,
        port: Int,
        workers: Int,
        deviceID: String,
        dns: String,
        obfsMode: String,
        vkAnonPath: String
    ) -> Int32 {
        let pPeer = strdup(peer)
        let pHashes = strdup(hashes)
        let pPassword = strdup(password)
        let pDeviceID = strdup(deviceID)
        let pDns = strdup(dns)
        let pObfsMode = strdup(obfsMode)
        let pVkAnonPath = strdup(vkAnonPath)

        defer {
            free(pPeer)
            free(pHashes)
            free(pPassword)
            free(pDeviceID)
            free(pDns)
            free(pObfsMode)
            free(pVkAnonPath)
        }

        return WDTT_Start(
            pPeer,
            pHashes,
            pPassword,
            Int32(port),
            Int32(workers),
            pDeviceID,
            pDns,
            pObfsMode,
            pVkAnonPath
        )
    }

    /// Wait until TURN/DTLS is established and a valid WireGuard configuration
    /// has been received. WireGuard is intentionally not started yet.
    static func waitUntilTransportReady(timeout: TimeInterval) -> Bool {
        WDTT_WaitReady(timeoutMilliseconds(timeout)) != 0
    }

    /// Returns the interface/DNS/MTU information extracted from the WireGuard
    /// configuration received during bootstrap.
    static func networkConfiguration() -> GoTunnelNetworkConfiguration? {
        guard let ptr = WDTT_CopyNetworkConfig() else { return nil }
        defer { WDTT_Free(ptr) }

        let json = String(cString: ptr)
        guard let data = json.data(using: .utf8) else { return nil }
        return try? JSONDecoder().decode(GoTunnelNetworkConfiguration.self, from: data)
    }

    /// Allows the Go runtime to create and raise WireGuard after iOS has
    /// successfully installed the packet-tunnel routes.
    @discardableResult
    static func activateWireGuard() -> Bool {
        WDTT_ActivateWireGuard() == 0
    }

    static func waitUntilWireGuardReady(timeout: TimeInterval) -> Bool {
        WDTT_WaitWireGuardReady(timeoutMilliseconds(timeout)) != 0
    }

    /// Recreate only the transport attempt. The NetworkExtension/TUN remains up.
    static func notifyNetworkChange() {
        WDTT_NotifyNetworkChange()
    }

    /// Ask the Go watchdog to evaluate the tunnel immediately after wake.
    static func wakeHealthCheck() {
        WDTT_WakeHealthCheck()
    }

    static func stop() {
        WDTT_Stop()
    }

    static var isRunning: Bool {
        WDTT_IsRunning() != 0
    }

    private static func timeoutMilliseconds(_ timeout: TimeInterval) -> Int32 {
        let milliseconds = max(0, min(timeout * 1_000, Double(Int32.max)))
        return Int32(milliseconds.rounded())
    }
}
