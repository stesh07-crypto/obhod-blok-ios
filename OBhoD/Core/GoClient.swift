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

enum GoClient {

    private static var logHandler: ((String, Bool) -> Void)?
    private static var statsHandler: ((String) -> Void)?
    private static var packetHandler: ((Data) -> Void)?

    // C callbacks must not capture Swift context. Keep the callback itself as a
    // static @convention(c) function pointer and route all work through GoClient.
    private static let cLogCallback: @convention(c) (UnsafePointer<CChar>?, Int32) -> Void = { linePtr, isError in
        guard let ptr = linePtr else { return }

        let raw = String(cString: ptr)
        let normalized = GoClient.normalizedLogLine(raw)

        // Live counters have a dedicated structured callback/UI. Do not add
        // another log row every two seconds.
        if normalized.contains("[СТАТИСТИКА]") {
            return
        }

        let inferredError = isError != 0 || GoClient.looksLikeError(normalized)
        guard GoClient.shouldForwardLog(normalized, isError: inferredError) else { return }

        let displayLine = GoClient.simplifiedDisplayLine(normalized, isError: inferredError)
        GoClient.logHandler?(displayLine, inferredError)
    }

    // MARK: Setup

    static func setLogHandler(_ handler: @escaping (String, Bool) -> Void) {
        logHandler = handler
        WDTT_SetLogCallback(cLogCallback)
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

        // Build 166 memory experiment: the active profile currently carries
        // four VK hashes, so 9 workers/hash gives exactly 36 TURN/DTLS workers.
        // Keep the Go-side hard cap and topology intact; this is deliberately a
        // reversible input cap so we can compare 36 against builds 164/165.
        let effectiveWorkersPerHash = min(max(workers, 1), 9)

        return WDTT_Start(
            pPeer,
            pHashes,
            pPassword,
            Int32(port),
            Int32(effectiveWorkersPerHash),
            pDeviceID,
            pDns,
            pObfsMode,
            pVkAnonPath
        )
    }

    static func waitUntilTransportReady(timeout: TimeInterval) -> Bool {
        WDTT_WaitReady(timeoutMilliseconds(timeout)) != 0
    }

    static func networkConfiguration() -> GoTunnelNetworkConfiguration? {
        guard let ptr = WDTT_CopyNetworkConfig() else { return nil }
        defer { WDTT_Free(ptr) }

        let json = String(cString: ptr)
        guard let data = json.data(using: .utf8) else { return nil }
        return try? JSONDecoder().decode(GoTunnelNetworkConfiguration.self, from: data)
    }

    @discardableResult
    static func activateWireGuard() -> Bool {
        WDTT_ActivateWireGuard() == 0
    }

    static func waitUntilWireGuardReady(timeout: TimeInterval) -> Bool {
        WDTT_WaitWireGuardReady(timeoutMilliseconds(timeout)) != 0
    }

    /// A physical-path event is only a hint. Individual TURN workers already
    /// retry transient socket failures on Wi-Fi/cellular handoff while keeping
    /// their credentials. Cancelling the whole runOnce here also closes the
    /// userspace WireGuard device, creating a self-inflicted traffic outage.
    /// Keep the VPN/WireGuard runtime alive and only ask the health watchdog to
    /// re-evaluate the current transport. A real sustained failure can still
    /// request a transport reconnect; manual reconnect remains explicit.
    static func notifyNetworkChange() {
        WDTT_WakeHealthCheck()
    }

    /// Rebuild only the current Go transport attempt. The iOS VPN session,
    /// routes and NetworkExtension stay alive.
    @discardableResult
    static func requestTransportReconnect() -> Bool {
        WDTT_RequestReconnect() != 0
    }

    static func wakeHealthCheck() {
        WDTT_WakeHealthCheck()
    }

    static func stop() {
        WDTT_Stop()
    }

    static var isRunning: Bool {
        WDTT_IsRunning() != 0
    }

    // MARK: Log policy

    private static func normalizedLogLine(_ raw: String) -> String {
        raw.replacingOccurrences(
            of: #"^\d{4}/\d{2}/\d{2}\s+\d{2}:\d{2}:\d{2}(?:\.\d+)?\s*"#,
            with: "",
            options: .regularExpression
        ).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private static func looksLikeError(_ line: String) -> Bool {
        let lower = line.lowercased()
        return lower.contains("ошибка") ||
            lower.contains("fatal") ||
            lower.contains("failed") ||
            lower.contains("timeout") ||
            lower.contains("refused") ||
            lower.contains("denied") ||
            lower.contains("не удалось")
    }

    private static func shouldForwardLog(_ line: String, isError: Bool) -> Bool {
        if isError { return true }

        let detailed = AppGroup.sharedDefaults?.bool(forKey: AppGroup.Keys.detailedLogs) ?? false
        if detailed { return true }

        // Always surface event-based diagnostics even in quiet mode. These are
        // intentionally sparse and are what lets us distinguish a worker/pool
        // failure from a NetworkExtension stop or a physical-path transition.
        if line.hasPrefix("[ДИАГ]") { return true }

        // WorkerGroup already classifies transient iOS socket/handoff failures
        // and TURN allocation problems. Keep those lines visible in the normal
        // Logs screen so a post-mortem does not require Detailed Logs mode.
        if line.contains("[ВОРКЕР #") &&
            (line.contains("[СЕТЬ]") || line.contains("[TURN]")) {
            return true
        }

        // Android-style quiet mode: one useful lifecycle event instead of one
        // line per session/relay/VKCalls step/dispatcher registration.
        if line.contains("[DTLS] Соединение установлено") { return true }
        if line.contains("[READY] Туннель готов") { return true }
        if line.contains("Креды OK") || line.contains("Первые креды") { return true }
        if line.contains("Запрос кредов") { return true }
        if line.lowercased().contains("капча") { return true }

        let importantPrefixes = [
            "[VPN]",
            "[СЕТЬ]",
            "[КЛИЕНТ]",
            "[КОНФИГ]",
            "[IOS-TUN]",
            "[ГО-ВОРКЕР]"
        ]
        if importantPrefixes.contains(where: { line.hasPrefix($0) }) {
            return true
        }

        return line == "Туннель остановлен" || line == "Туннель уже запущен"
    }

    private static func simplifiedDisplayLine(_ line: String, isError: Bool) -> String {
        if isError { return line }

        if line.contains("[DTLS] Соединение установлено") {
            return "[DTLS] Соединение установлено ✓"
        }
        if line.contains("[READY] Туннель готов") {
            return "[READY] Туннель готов к работе ✓"
        }
        if line.contains("Креды OK") || line.contains("Первые креды") {
            return "[ВК] Учетные данные проверены ✓"
        }
        if line.contains("Запрос кредов") {
            return "[ВК] Получение учетных данных…"
        }
        return line
    }

    private static func timeoutMilliseconds(_ timeout: TimeInterval) -> Int32 {
        let milliseconds = max(0, min(timeout * 1_000, Double(Int32.max)))
        return Int32(milliseconds.rounded())
    }
}
