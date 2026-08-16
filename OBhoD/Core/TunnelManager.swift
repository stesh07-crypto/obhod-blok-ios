import Foundation
import NetworkExtension
import Combine
import UIKit

// MARK: - Log Entry

struct LogEntry: Identifiable {
    let id = UUID()
    let message: String
    let isError: Bool
    let timestamp = Date()
}

// MARK: - TunnelManager

/// Main-app facade for the VPN.
///
/// Network-core-v2 deliberately keeps the actual Go/WireGuard runtime out of
/// the application process. The app only persists the selected configuration
/// and asks NetworkExtension to start/stop the tunnel.
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

    private var isDisconnecting = false
    private var connectionGeneration: UInt64 = 0

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
        // The same visible button acts as Cancel while a staged bootstrap is in
        // progress. Older builds silently ignored this tap for up to ~75 seconds.
        if isConnecting {
            disconnect()
            return
        }
        guard !isRunning, !isDisconnecting else { return }

        connectionGeneration &+= 1
        let requestGeneration = connectionGeneration

        isConnecting = true
        stats = "Подключение…"
        logs.removeAll()
        unreadErrors = 0
        remoteLogMirror.removeAll()

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

        // Invalidate any delayed save/load/settle continuation so an old CONNECT
        // cannot call startVPNTunnel after the user has already cancelled it.
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
        // Treat the extension's current rolling log as already consumed so it
        // is not re-added on the next polling tick.
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

            // Always reload the object after saving. NetworkExtension preferences
            // are asynchronous and using a stale manager here is a common source
            // of preparing/connecting races on real devices.
            try await manager.loadFromPreferences()
            guard isCurrentConnectionGeneration(generation), isConnecting else { return }

            // Give NECP/preferences a short settling window before asking iOS to
            // launch the extension. This is intentionally modest because we do
            // not enable includeAllNetworks in this version.
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
            // Keep the original uptime while the extension repairs only its
            // transport; prevent another CONNECT request until it settles.
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
            resetDisconnectedState()

        @unknown default:
            break
        }
    }

    private func resetDisconnectedState() {
        isRunning = false
        isConnecting = false
        isDisconnecting = false
        connectedSince = nil
        stats = "Ожидание данных..."
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
            stats = extensionStats
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

    // MARK: Log helpers

    private func appendLog(_ message: String, isError: Bool) {
        logs.append(LogEntry(message: message, isError: isError))
        if logs.count > 150 {
            logs.removeFirst(logs.count - 150)
        }
        if isError {
            unreadErrors += 1
        }
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
    }
}
