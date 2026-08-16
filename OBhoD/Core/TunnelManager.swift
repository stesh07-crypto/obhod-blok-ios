import Foundation
import NetworkExtension
import Combine
import UIKit

// MARK: - Log Entry

struct LogEntry: Identifiable {
    let id: UUID
    let key: String
    var message: String
    var isError: Bool
    var count: Int
    var timestamp: Date

    init(
        id: UUID = UUID(),
        key: String,
        message: String,
        isError: Bool,
        count: Int = 1,
        timestamp: Date = Date()
    ) {
        self.id = id
        self.key = key
        self.message = message
        self.isError = isError
        self.count = count
        self.timestamp = timestamp
    }
}

private struct TunnelLiveStatsPayload: Decodable {
    let activeConnections: Int
    let uploadBytes: Int64
    let downloadBytes: Int64
}

// MARK: - TunnelManager

@MainActor
final class TunnelManager: ObservableObject {
    static let shared = TunnelManager()

    // MARK: Published state

    @Published var isRunning = false
    @Published var isConnecting = false
    @Published var stats = "Ожидание данных..."
    @Published var logs: [LogEntry] = []
    @Published var unreadErrors = 0
    @Published var connectedSince: Date? = nil

    // These are real runtime counters from Go, not profile configuration values.
    @Published var activeConnections = 0
    @Published var uploadedBytes: Int64 = 0
    @Published var downloadedBytes: Int64 = 0

    private var isDisconnecting = false
    private var connectionGeneration: UInt64 = 0
    private var lastRequestedProfile: ConnectionProfile?
    private var autoReconnectAttempts = 0
    private var autoReconnectTask: Task<Void, Never>?

    // MARK: NetworkExtension

    private var vpnManager: NETunnelProviderManager?
    private var statusObserver: AnyCancellable?
    private var extensionPoller: AnyCancellable?

    private let defaults = AppGroup.sharedDefaults ?? UserDefaults.standard
    private var remoteLogMirror: [String] = []

    private static let errorLogPrefix = "[[WDTT_ERROR]] "

    private init() {
        observeVPNStatus()
        startExtensionPolling()
        Task { await loadVPNManager() }
    }

    // MARK: Connect / Disconnect

    func connect(profile: ConnectionProfile) {
        if isConnecting {
            disconnect()
            return
        }
        guard !isRunning, !isDisconnecting else { return }

        autoReconnectTask?.cancel()
        autoReconnectTask = nil
        lastRequestedProfile = profile

        connectionGeneration &+= 1
        let requestGeneration = connectionGeneration

        isConnecting = true
        stats = "Подключение…"
        logs.removeAll()
        unreadErrors = 0
        remoteLogMirror.removeAll()
        resetLiveStats()

        defaults.removeObject(forKey: AppGroup.Keys.lastLogLines)
        defaults.removeObject(forKey: AppGroup.Keys.lastStats)
        defaults.set(false, forKey: AppGroup.Keys.tunnelRunning)
        syncSettingsToAppGroup(profile: profile)

        Task { [weak self] in
            guard let self else { return }
            await self.startVPNInterface(profile: profile, generation: requestGeneration)
        }
    }

    func disconnect() {
        guard !isDisconnecting else { return }

        autoReconnectTask?.cancel()
        autoReconnectTask = nil
        autoReconnectAttempts = 0

        connectionGeneration &+= 1
        isDisconnecting = true
        isConnecting = false
        stats = "Отключение…"

        guard let manager = vpnManager else {
            resetDisconnectedState()
            return
        }

        switch manager.connection.status {
        case .disconnected, .invalid:
            resetDisconnectedState()
        default:
            manager.connection.stopVPNTunnel()
        }
    }

    func clearUnreadErrors() {
        unreadErrors = 0
    }

    func clearLogs() {
        logs.removeAll()
        unreadErrors = 0
        remoteLogMirror = defaults.stringArray(forKey: AppGroup.Keys.lastLogLines) ?? []
    }

    // MARK: VPN manager

    private func loadVPNManager() async {
        do {
            let managers = try await NETunnelProviderManager.loadAllFromPreferences()
            if let existing = managers.first(where: {
                ($0.protocolConfiguration as? NETunnelProviderProtocol)?
                    .providerBundleIdentifier == AppGroup.tunnelBundleId
            }) {
                vpnManager = existing
            } else {
                vpnManager = NETunnelProviderManager()
            }
            applyVPNStatus()
        } catch {
            appendLog("Не удалось загрузить VPN-конфигурацию: \(error.localizedDescription)", isError: true)
            isConnecting = false
        }
    }

    private func startVPNInterface(profile: ConnectionProfile, generation: UInt64) async {
        if vpnManager == nil {
            await loadVPNManager()
        }

        guard isCurrentConnectionGeneration(generation), isConnecting else { return }
        guard let manager = vpnManager else {
            isConnecting = false
            appendLog("VPN Manager недоступен", isError: true)
            return
        }

        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = AppGroup.tunnelBundleId
        proto.serverAddress = profile.peer
        proto.providerConfiguration = ["profileId": profile.id]

        manager.protocolConfiguration = proto
        manager.localizedDescription = "OBhoD"
        manager.isEnabled = true

        do {
            try await manager.saveToPreferences()
            guard isCurrentConnectionGeneration(generation), isConnecting else { return }

            try await manager.loadFromPreferences()
            guard isCurrentConnectionGeneration(generation), isConnecting else { return }

            try await Task.sleep(nanoseconds: 500_000_000)
            guard isCurrentConnectionGeneration(generation), isConnecting else { return }

            try manager.connection.startVPNTunnel()
            appendLog("Запуск Network Extension…", isError: false)
            applyVPNStatus()
        } catch is CancellationError {
            if isCurrentConnectionGeneration(generation) {
                resetDisconnectedState()
            }
        } catch {
            guard isCurrentConnectionGeneration(generation) else { return }
            isConnecting = false
            isRunning = false
            appendLog("Ошибка запуска VPN: \(error.localizedDescription)", isError: true)
        }
    }

    private func isCurrentConnectionGeneration(_ value: UInt64) -> Bool {
        connectionGeneration == value
    }

    // MARK: Status source of truth

    private func observeVPNStatus() {
        statusObserver = NotificationCenter.default
            .publisher(for: .NEVPNStatusDidChange)
            .sink { [weak self] _ in
                Task { @MainActor [weak self] in
                    self?.applyVPNStatus()
                }
            }
    }

    private func applyVPNStatus() {
        guard let status = vpnManager?.connection.status else { return }

        switch status {
        case .connected:
            autoReconnectTask?.cancel()
            autoReconnectTask = nil
            autoReconnectAttempts = 0
            isRunning = true
            isConnecting = false
            isDisconnecting = false
            if connectedSince == nil {
                connectedSince = Date()
            }
            if stats == "Подключение…" || stats == "Отключение…" {
                stats = "Подключено"
            }

        case .connecting:
            isRunning = false
            isConnecting = true
            isDisconnecting = false
            stats = "Подключение…"

        case .reasserting:
            isRunning = false
            isConnecting = true
            isDisconnecting = false
            stats = "Переподключение…"

        case .disconnecting:
            isRunning = false
            isConnecting = false
            isDisconnecting = true
            stats = "Отключение…"

        case .disconnected, .invalid:
            let wasEstablished = connectedSince != nil
            let wasManualStop = isDisconnecting
            resetDisconnectedState()

            // Extension-level watchdog repairs normal transport failures without
            // dropping the VPN. This is the final safety net for an actual
            // NetworkExtension termination while the app is awake.
            if wasEstablished && !wasManualStop && SettingsStore.shared.autoReconnect {
                scheduleAutoReconnect()
            }

        @unknown default:
            break
        }
    }

    private func scheduleAutoReconnect() {
        guard autoReconnectTask == nil, autoReconnectAttempts < 3 else { return }
        guard let profile = lastRequestedProfile ?? storedActiveProfile() else { return }

        autoReconnectAttempts += 1
        let attempt = autoReconnectAttempts
        let delaySeconds: UInt64 = [2, 5, 10][min(attempt - 1, 2)]
        appendLog("[СЕТЬ] VPN неожиданно остановлен. Автовосстановление \(attempt)/3 через \(delaySeconds)с…", isError: false)

        autoReconnectTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: delaySeconds * 1_000_000_000)
            guard !Task.isCancelled else { return }
            await MainActor.run {
                guard let self, !self.isRunning, !self.isConnecting, !self.isDisconnecting else { return }
                self.autoReconnectTask = nil
                self.connect(profile: profile)
            }
        }
    }

    private func storedActiveProfile() -> ConnectionProfile? {
        guard let data = defaults.data(forKey: AppGroup.Keys.activeProfileJSON) else { return nil }
        return try? JSONDecoder().decode(ConnectionProfile.self, from: data)
    }

    private func resetDisconnectedState() {
        isRunning = false
        isConnecting = false
        isDisconnecting = false
        connectedSince = nil
        stats = "Ожидание данных..."
        resetLiveStats()
    }

    private func resetLiveStats() {
        activeConnections = 0
        uploadedBytes = 0
        downloadedBytes = 0
    }

    // MARK: Extension logs / stats

    private func startExtensionPolling() {
        extensionPoller = Timer.publish(every: 1.0, on: .main, in: .common)
            .autoconnect()
            .sink { [weak self] _ in
                Task { @MainActor [weak self] in
                    self?.syncExtensionState()
                }
            }
    }

    private func syncExtensionState() {
        if let extensionStats = defaults.string(forKey: AppGroup.Keys.lastStats),
           !extensionStats.isEmpty,
           (isRunning || isConnecting) {
            applyLiveStats(extensionStats)
        }

        let remote = defaults.stringArray(forKey: AppGroup.Keys.lastLogLines) ?? []
        guard remote != remoteLogMirror else { return }

        let overlap = logOverlap(old: remoteLogMirror, new: remote)
        for rawLine in remote.dropFirst(overlap) {
            let isError = rawLine.hasPrefix(Self.errorLogPrefix)
            let line = isError ? String(rawLine.dropFirst(Self.errorLogPrefix.count)) : rawLine
            appendLog(line, isError: isError)
        }
        remoteLogMirror = remote
    }

    private func applyLiveStats(_ raw: String) {
        if let data = raw.data(using: .utf8),
           let payload = try? JSONDecoder().decode(TunnelLiveStatsPayload.self, from: data) {
            activeConnections = max(0, payload.activeConnections)
            uploadedBytes = max(0, payload.uploadBytes)
            downloadedBytes = max(0, payload.downloadBytes)
            stats = "Активных: \(activeConnections) • ↓ \(downloadedMBString) • ↑ \(uploadedMBString)"
            return
        }

        // Startup/reconnect text shares the same App Group slot until the first
        // structured live-stat snapshot arrives.
        stats = raw
    }

    private func logOverlap(old: [String], new: [String]) -> Int {
        let maxOverlap = min(old.count, new.count)
        guard maxOverlap > 0 else { return 0 }

        for count in stride(from: maxOverlap, through: 1, by: -1) {
            if Array(old.suffix(count)) == Array(new.prefix(count)) {
                return count
            }
        }
        return 0
    }

    // MARK: Android-style grouped logs

    private func appendLog(_ message: String, isError: Bool) {
        let key = logKey(for: message, isError: isError)

        if let index = logs.firstIndex(where: { $0.key == key }) {
            logs[index].message = message
            logs[index].isError = logs[index].isError || isError
            logs[index].count += 1
            logs[index].timestamp = Date()
            return
        }

        logs.append(LogEntry(key: key, message: message, isError: isError))
        if logs.count > 100 {
            logs.removeFirst(logs.count - 100)
        }
        if isError {
            unreadErrors += 1
        }
    }

    private func logKey(for message: String, isError: Bool) -> String {
        let lower = message.lowercased()

        if isError {
            if lower.contains("fatal_auth") || lower.contains("авторизац") || lower.contains("парол") { return "err_auth" }
            if lower.contains("connection refused") || lower.contains("refused") { return "err_refused" }
            if lower.contains("timeout") || lower.contains("deadline") { return "err_timeout" }
            if lower.contains("dtls") { return "err_dtls" }
            if lower.contains("кред") || lower.contains("credential") { return "err_creds" }
            if lower.contains("dns") { return "err_dns" }
            return "err_" + normalizedLogKey(message)
        }

        if message.hasPrefix("[СЕТЬ]") { return "network_" + normalizedLogKey(message) }
        if message.hasPrefix("[VPN]") { return "vpn_" + normalizedLogKey(message) }
        if message.hasPrefix("[КАПЧА]") { return "captcha_" + normalizedLogKey(message) }
        if message.contains("WireGuard") || message.hasPrefix("[IOS-TUN]") { return "wireguard_" + normalizedLogKey(message) }
        if message.hasPrefix("[КОНФИГ]") { return "config_" + normalizedLogKey(message) }
        return normalizedLogKey(message)
    }

    private func normalizedLogKey(_ message: String) -> String {
        var value = message.lowercased()
        value = value.replacingOccurrences(of: #"#\d+"#, with: "#", options: .regularExpression)
        value = value.replacingOccurrences(of: #"\b\d{1,3}(?:\.\d{1,3}){3}:\d+\b"#, with: "host:port", options: .regularExpression)
        value = value.replacingOccurrences(of: #"\b\d+\b"#, with: "n", options: .regularExpression)
        return value
    }

    // MARK: Live stats formatting

    var downloadedMBString: String { formatMB(downloadedBytes) }
    var uploadedMBString: String { formatMB(uploadedBytes) }
    var totalMBString: String { formatMB(downloadedBytes + uploadedBytes) }

    private func formatMB(_ bytes: Int64) -> String {
        String(format: "%.2f МБ", Double(max(0, bytes)) / (1024.0 * 1024.0))
    }

    // MARK: Uptime

    var uptimeString: String {
        guard let since = connectedSince else { return "" }
        let elapsed = Int(-since.timeIntervalSinceNow)
        let h = elapsed / 3600
        let m = (elapsed % 3600) / 60
        let s = elapsed % 60
        if h > 0 { return String(format: "%d:%02d:%02d", h, m, s) }
        return String(format: "%d:%02d", m, s)
    }

    // MARK: Shared configuration

    private func syncSettingsToAppGroup(profile: ConnectionProfile) {
        if let encoded = try? JSONEncoder().encode(profile) {
            defaults.set(encoded, forKey: AppGroup.Keys.activeProfileJSON)
        }

        let deviceID = UIDevice.current.identifierForVendor?.uuidString ?? "unknown-ios"
        defaults.set(deviceID, forKey: AppGroup.Keys.deviceID)
        defaults.set(SettingsStore.shared.goDnsMode, forKey: AppGroup.Keys.goDnsMode)
        defaults.set(SettingsStore.shared.obfsMode, forKey: AppGroup.Keys.obfsMode)
        defaults.set(SettingsStore.shared.vkAnonPath, forKey: AppGroup.Keys.vkAnonPath)
        defaults.set(SettingsStore.shared.detailedLogs, forKey: AppGroup.Keys.detailedLogs)
    }
}
