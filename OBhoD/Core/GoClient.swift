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

    // MARK: Setup

    static func setLogHandler(_ handler: @escaping (String, Bool) -> Void) {
        logHandler = handler
        WDTT_SetLogCallback { linePtr, isError in
            guard let ptr = linePtr else { return }

            let raw = String(cString: ptr)
            let normalized = normalizedLogLine(raw)

            // Live counters have a dedicated structured callback/UI. Do not add
            // another log row every two seconds.
            if normalized.contains("[СТАТИСТИКА]") {
                return
            }

            let inferredError = isError != 0 || looksLikeError(normalized)
            guard shouldForwardLog(normalized, isError: inferredError) else { return }

            let displayLine = simplifiedDisplayLine(normalized, isError: inferredError)
            GoClient.logHandler?(displayLine, inferredError)
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

    static func notifyNetworkChange() {
        WDTT_NotifyNetworkChange()
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

        // Android-style quiet mode: one useful lifecycle event instead of one
        // line per session/relay/VKCalls step/dispatcher registration.
        if line.contains("[DTLS] Соединение установлено") { return true }
        if line.contains("[READY] Туннель готов") { return true }
        if line.contains("Креды OK") || line.contains("Первые креды") { return true }
        if line.contains("Запрос кредов") { return true }
        if line.contains("капча") || line.contains("КАПЧА") { return true }

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
