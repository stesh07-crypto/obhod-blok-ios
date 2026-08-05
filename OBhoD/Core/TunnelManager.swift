import Foundation
import NetworkExtension
import Combine
import UIKit

// MARK: – Log Entry

struct LogEntry: Identifiable {
    let id = UUID()
    let message: String
    let isError: Bool
    let timestamp = Date()
}

// MARK: – TunnelManager

@MainActor
final class TunnelManager: ObservableObject {
    static let shared = TunnelManager()

    // ── Published state ──────────────────────────────────────────────────
    @Published var isRunning   = false
    @Published var isConnecting = false
    @Published var stats       = "Ожидание данных..."
    @Published var logs: [LogEntry] = []
    @Published var unreadErrors = 0
    @Published var connectedSince: Date? = nil

    // VPN Manager (NetworkExtension)
    private var vpnManager: NETunnelProviderManager?
    private var statusObserver: AnyCancellable?

    private let defaults = AppGroup.sharedDefaults ?? UserDefaults.standard

    private init() {
        setupGoCallbacks()
        Task { await loadVPNManager() }
    }

    // MARK: – Go Callbacks

    private func setupGoCallbacks() {
        GoClient.setLogHandler { [weak self] line, isError in
            Task { @MainActor [weak self] in
                self?.appendLog(line, isError: isError)
            }
        }
        GoClient.setStatsHandler { [weak self] statsLine in
            Task { @MainActor [weak self] in
                self?.stats = statsLine
            }
        }
    }

    // MARK: – Connect / Disconnect

    func connect(profile: ConnectionProfile) {
        guard !isConnecting, !isRunning else { return }

        isConnecting = true
        stats = "Подключение…"
        logs.removeAll()
        unreadErrors = 0

        let deviceID = UIDevice.current.identifierForVendor?.uuidString ?? "unknown-ios"

        Task { [weak self] in
            guard let self else { return }
            let result = GoClient.start(
                peer: profile.peer,
                hashes: profile.vkHashes,
                password: profile.password,
                port: profile.listenPort,
                workers: profile.workersPerHash,
                deviceID: deviceID,
                dns: SettingsStore.shared.goDnsMode,
                obfsMode: SettingsStore.shared.obfsMode,
                vkAnonPath: SettingsStore.shared.vkAnonPath
            )

            await MainActor.run {
                if result == 0 {
                    self.isConnecting = false
                    self.isRunning = true
                    self.connectedSince = Date()
                    // Raise VPN status so iOS shows VPN icon
                    self.startVPNInterface(profile: profile)
                } else {
                    self.isConnecting = false
                    self.appendLog("Ошибка запуска туннеля", isError: true)
                }
            }
        }
    }

    func disconnect() {
        GoClient.stop()
        stopVPNInterface()
        isRunning = false
        isConnecting = false
        connectedSince = nil
        stats = "Ожидание данных..."
    }

    func clearUnreadErrors() {
        unreadErrors = 0
    }

    // MARK: – NetworkExtension (shows VPN icon in status bar)

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
        } catch {
            print("[TunnelManager] loadVPNManager error: \(error)")
        }
    }

    private func startVPNInterface(profile: ConnectionProfile) {
        Task {
            guard let manager = vpnManager else { return }
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = AppGroup.tunnelBundleId
            proto.serverAddress = profile.peer
            proto.providerConfiguration = [
                "profileId": profile.id
            ]
            manager.protocolConfiguration = proto
            manager.localizedDescription = "OBhoD"
            manager.isEnabled = true
            do {
                try await manager.saveToPreferences()
                try manager.connection.startVPNTunnel()
            } catch {
                print("[TunnelManager] startVPNTunnel error: \(error)")
            }
        }
    }

    private func stopVPNInterface() {
        vpnManager?.connection.stopVPNTunnel()
    }

    // MARK: – Log Helpers

    private func appendLog(_ message: String, isError: Bool) {
        let entry = LogEntry(message: message, isError: isError)
        logs.append(entry)
        if logs.count > 150 { logs.removeFirst(logs.count - 150) }
        if isError { unreadErrors += 1 }
    }

    // MARK: – Uptime

    var uptimeString: String {
        guard let since = connectedSince else { return "" }
        let elapsed = Int(-since.timeIntervalSinceNow)
        let h = elapsed / 3600
        let m = (elapsed % 3600) / 60
        let s = elapsed % 60
        if h > 0 { return String(format: "%d:%02d:%02d", h, m, s) }
        return String(format: "%d:%02d", m, s)
    }
}
