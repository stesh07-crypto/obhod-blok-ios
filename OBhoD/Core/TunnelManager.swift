import Foundation
import NetworkExtension
import Combine
import UIKit
import WidgetKit

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
    @Published var isTransportRecovering = false
    @Published var stats = "Ожидание данных..."
    @Published var logs: [LogEntry] = []
    @Published var unreadErrors = 0
    @Published var connectedSince: Date? = nil

    // These are real runtime counters from Go, not profile configuration values.
    @Published var activeConnections = 0
    @Published var uploadedBytes: Int64 = 0
    @Published var downloadedBytes: Int64 = 0
    @Published var currentDownloadSpeed: String = "0 КБ/с"

    private var lastSpeedCheckDate: Date?
    private var lastSpeedCheckBytes: Int64 = 0
    private var lastLiveActivitySync: Date?

    private var isDisconnecting = false
    private var connectionGeneration: UInt64 = 0
    private var lastRequestedProfile: ConnectionProfile?
    private var autoReconnectAttempts = 0
    private var autoReconnectTask: Task<Void, Never>?
    private var isAutoReconnectLaunching = false
    private var recoveringSince: Date?

    // MARK: NetworkExtension

    private var vpnManager: NETunnelProviderManager?
    private var statusObserver: AnyCancellable?
    private var extensionPoller: AnyCancellable?
    private var healthObserver: AnyCancellable?
    private var lastAutomaticRepairAt: Date?

    private let defaults = AppGroup.sharedDefaults ?? UserDefaults.standard
    private var remoteLogMirror: [String] = []

    private static let errorLogPrefix = "[[WDTT_ERROR]] "

    private init() {
        observeVPNStatus()
        startExtensionPolling()
        observeConnectionHealth()
        Task { await loadVPNManager() }
    }

    // MARK: Connect / Disconnect

    func connect(profile: ConnectionProfile) {
        if isConnecting {
            disconnect()
            return
        }
        guard !isRunning, !isDisconnecting else { return }

        if !isAutoReconnectLaunching {
            autoReconnectAttempts = 0
        }

        autoReconnectTask?.cancel()
        autoReconnectTask = nil
        lastRequestedProfile = profile

        connectionGeneration &+= 1
        let requestGeneration = connectionGeneration

        isConnecting = true
        isTransportRecovering = false
        stats = isAutoReconnectLaunching ? "Переподключение…" : "Подключение…"

        if !isAutoReconnectLaunching {
            logs.removeAll()
            unreadErrors = 0
        }
        remoteLogMirror.removeAll()
        resetLiveStats()

        defaults.removeObject(forKey: AppGroup.Keys.lastLogLines)
        defaults.removeObject(forKey: AppGroup.Keys.lastStats)
        defaults.set(false, forKey: AppGroup.Keys.tunnelRunning)
        defaults.set(false, forKey: AppGroup.Keys.transportRecovering)
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
        isAutoReconnectLaunching = false

        connectionGeneration &+= 1
        isDisconnecting = true
        isConnecting = false
        isTransportRecovering = false
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

    /// Ask the living NetworkExtension to rebuild only its Go/TURN transport.
    /// This deliberately does not call stopVPNTunnel/startVPNTunnel and therefore
    /// keeps the iOS VPN session and packet-tunnel configuration alive.
    func reconnectTransport() {
        guard isRunning, !isDisconnecting else { return }
        guard let session = vpnManager?.connection as? NETunnelProviderSession else {
            appendLog("[СЕТЬ] NetworkExtension недоступен для мягкого переподключения", isError: true)
            return
        }

        do {
            try session.sendProviderMessage(Data("reconnect".utf8)) { [weak self] responseData in
                let response = responseData.flatMap { String(data: $0, encoding: .utf8) } ?? ""
                Task { @MainActor [weak self] in
                    guard let self else { return }
                    switch response {
                    case "ok":
                        self.appendLog("[СЕТЬ] Ручное переподключение транспорта запущено", isError: false)
                    case "busy":
                        self.appendLog("[СЕТЬ] Транспорт уже восстанавливается; повторная команда не нужна", isError: false)
                    default:
                        self.appendLog("[СЕТЬ] Extension не подтвердил переподключение", isError: true)
                    }
                }
            }
        } catch {
            appendLog("[СЕТЬ] Не удалось отправить команду переподключения: \(error.localizedDescription)", isError: true)
        }
    }

    /// The app's own probe (TCP through the tunnel) is the only fresh end-to-end
    /// signal. When it fails while the tunnel claims to be running, ask Go to
    /// rebuild the data plane. Go applies its own cooldown and attempt budget;
    /// the local guard keeps repeated UI-driven requests away.
    private func observeConnectionHealth() {
        healthObserver = ConnectionHealthMonitor.shared.$reachabilityPoor
            .receive(on: DispatchQueue.main)
            .sink { [weak self] poor in
                guard poor else { return }
                Task { @MainActor [weak self] in
                    self?.requestAutomaticRepair()
                }
            }
    }

    private func requestAutomaticRepair() {
        guard isRunning, !isDisconnecting, !isConnecting, !isTransportRecovering else { return }
        if let last = lastAutomaticRepairAt, Date().timeIntervalSince(last) < 90 { return }
        guard let session = vpnManager?.connection as? NETunnelProviderSession else { return }

        lastAutomaticRepairAt = Date()
        do {
            try session.sendProviderMessage(Data("repair".utf8)) { [weak self] responseData in
                let response = responseData.flatMap { String(data: $0, encoding: .utf8) } ?? ""
                Task { @MainActor [weak self] in
                    guard let self else { return }
                    switch response {
                    case "ok":
                        self.appendLog("[СЕТЬ] Проверка связи не прошла — запущен ремонт транспорта", isError: false)
                    case "rejected":
                        self.appendLog("[ДИАГ] Ремонт отклонён защитой от циклических перезапусков", isError: false)
                    default:
                        self.appendLog("[СЕТЬ] Extension не подтвердил ремонт транспорта", isError: true)
                    }
                }
            }
        } catch {
            appendLog("[СЕТЬ] Не удалось отправить команду ремонта: \(error.localizedDescription)", isError: true)
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
            let isUserCancel = isDisconnecting ||
                               error.localizedDescription.localizedCaseInsensitiveContains("отмен") ||
                               error.localizedDescription.localizedCaseInsensitiveContains("cancel")
            if !isUserCancel {
                appendLog("Ошибка запуска VPN: \(error.localizedDescription)", isError: true)
            } else {
                appendLog("Запуск VPN отменён", isError: false)
                resetDisconnectedState()
            }
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
            isAutoReconnectLaunching = false
            isRunning = true
            isConnecting = false
            isDisconnecting = false
            if connectedSince == nil {
                connectedSince = Date()
            }
            if stats == "Подключение…" || stats == "Переподключение…" || stats == "Отключение…" {
                stats = "Подключено"
            }
            if !defaults.bool(forKey: AppGroup.Keys.transportRecovering) {
                isTransportRecovering = false
                recoveringSince = nil
            }
            writeWidgetStatus("connected")

        case .connecting:
            isRunning = false
            isConnecting = true
            isDisconnecting = false
            if stats != "Переподключение…" {
                stats = "Подключение…"
            }
            writeWidgetStatus("connecting")

        case .reasserting:
            isRunning = false
            isConnecting = true
            isDisconnecting = false
            isTransportRecovering = true
            stats = "Переподключение…"
            writeWidgetStatus("connecting")

        case .disconnecting:
            isRunning = false
            isConnecting = false
            isDisconnecting = true
            isTransportRecovering = false
            stats = "Отключение…"
            writeWidgetStatus("disconnected")

        case .disconnected, .invalid:
            let wasEstablished = connectedSince != nil
            let wasManualStop = isDisconnecting
            let recoveryAlreadyInProgress = autoReconnectAttempts > 0
            resetDisconnectedState()
            writeWidgetStatus("disconnected")

            // Normal TURN failures are repaired inside TunnelExtension. This is
            // a second safety net for a real extension exit/crash while the app
            // is available to relaunch the VPN session.
            if (wasEstablished || recoveryAlreadyInProgress),
               !wasManualStop,
               SettingsStore.shared.autoReconnect {
                scheduleAutoReconnect()
            }

        @unknown default:
            break
        }
    }

    /// Writes the current tunnel status string into the shared App Group so the
    /// WidgetKit extension can reflect it on the Home Screen / Lock Screen widget,
    /// and synchronizes the iOS Lock Screen Live Activity banner.
    private func writeWidgetStatus(_ status: String) {
        defaults.set(status, forKey: AppGroup.Keys.widgetStatus)
        defaults.set(Date().timeIntervalSince1970, forKey: AppGroup.Keys.widgetUpdatedAt)
        WidgetKit.WidgetCenter.shared.reloadTimelines(ofKind: "OBhoDWidget")

        syncLiveActivity(status: status)
    }

    private func syncLiveActivity(status: String) {
        guard SettingsStore.shared.liveActivityEnabled else {
            LiveActivityBridge.end()
            return
        }

        if status == "connected" {
            LiveActivityBridge.sync(
                isConnected: true,
                statusText: "Подключено",
                connectedSince: connectedSince ?? Date(),
                downloadSpeed: currentDownloadSpeed
            )
        } else if status == "connecting" {
            LiveActivityBridge.sync(
                isConnected: false,
                statusText: "Подключение…",
                connectedSince: nil,
                downloadSpeed: ""
            )
        } else {
            lastLiveActivitySync = nil
            currentDownloadSpeed = "0 КБ/с"
            lastSpeedCheckDate = nil
            lastSpeedCheckBytes = 0
            LiveActivityBridge.end()
        }
    }

    private func formatSpeed(_ bytesPerSec: Double) -> String {
        if bytesPerSec >= 1024 * 1024 {
            return String(format: "%.1f МБ/с", bytesPerSec / (1024.0 * 1024.0))
        } else if bytesPerSec >= 1024 {
            return String(format: "%.0f КБ/с", bytesPerSec / 1024.0)
        } else {
            return "0 КБ/с"
        }
    }


    private func scheduleAutoReconnect() {
        guard autoReconnectTask == nil, autoReconnectAttempts < 3 else { return }
        guard let profile = lastRequestedProfile ?? storedActiveProfile() else { return }

        autoReconnectAttempts += 1
        let attempt = autoReconnectAttempts
        let delays: [UInt64] = [2, 5, 10]
        let delaySeconds = delays[min(attempt - 1, delays.count - 1)]
        appendLog("[СЕТЬ] VPN неожиданно остановлен. Автовосстановление \(attempt)/3 через \(delaySeconds)с…", isError: false)

        autoReconnectTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: delaySeconds * 1_000_000_000)
            guard !Task.isCancelled, let self else { return }
            guard !self.isRunning, !self.isConnecting, !self.isDisconnecting else {
                self.autoReconnectTask = nil
                return
            }

            self.autoReconnectTask = nil
            self.isAutoReconnectLaunching = true
            self.connect(profile: profile)
            self.isAutoReconnectLaunching = false
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
        isTransportRecovering = false
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
        if isRunning || isConnecting {
            let recovering = defaults.bool(forKey: AppGroup.Keys.transportRecovering)
            if recovering {
                if recoveringSince == nil {
                    recoveringSince = Date()
                } else if let since = recoveringSince, Date().timeIntervalSince(since) > 45.0 {
                    // Watchdog: clear zombie recovering state in AppGroup and UI
                    defaults.set(false, forKey: AppGroup.Keys.transportRecovering)
                    recoveringSince = nil
                    isTransportRecovering = false
                } else if isTransportRecovering != recovering {
                    isTransportRecovering = recovering
                }
            } else {
                recoveringSince = nil
                if isTransportRecovering {
                    isTransportRecovering = false
                }
            }
        } else if isTransportRecovering {
            recoveringSince = nil
            isTransportRecovering = false
        }

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
            let now = Date()
            let newDownloadBytes = max(0, payload.downloadBytes)

            if let lastDate = lastSpeedCheckDate {
                let interval = now.timeIntervalSince(lastDate)
                if interval >= 0.8 {
                    let diff = max(0, newDownloadBytes - lastSpeedCheckBytes)
                    let bytesPerSec = Double(diff) / interval
                    currentDownloadSpeed = formatSpeed(bytesPerSec)
                    lastSpeedCheckDate = now
                    lastSpeedCheckBytes = newDownloadBytes
                }
            } else {
                lastSpeedCheckDate = now
                lastSpeedCheckBytes = newDownloadBytes
                currentDownloadSpeed = "0 КБ/с"
            }

            activeConnections = max(0, payload.activeConnections)
            uploadedBytes = max(0, payload.uploadBytes)
            downloadedBytes = newDownloadBytes
            stats = "Активных: \(activeConnections) • ↓ \(downloadedMBString) • ↑ \(uploadedMBString)"

            if isRunning {
                if lastLiveActivitySync == nil || now.timeIntervalSince(lastLiveActivitySync!) >= 1.5 {
                    lastLiveActivitySync = now
                    syncLiveActivity(status: "connected")
                }
            }
            return
        }

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
        if message.contains("[DTLS]") || message.contains("DTLS handshake timeout") { return "dtls_config" }
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
