import Foundation
import Network
import Combine

/// Lightweight UI-side health monitor.
///
/// It never owns or restarts the VPN runtime and deliberately does NOT create a
/// second NWPathMonitor. Physical-interface truth comes from PacketTunnelProvider,
/// the existing single path owner, through App Group shared state. This object
/// only measures a low-cost end-to-end TCP RTT for presentation in the app.
final class ConnectionHealthMonitor: ObservableObject {
    static let shared = ConnectionHealthMonitor()

    @Published private(set) var pingMilliseconds: Int?
    @Published private(set) var networkLabel = "—"
    @Published private(set) var reachabilityPoor = false

    private let defaults = AppGroup.sharedDefaults ?? UserDefaults.standard
    private let pingQueue = DispatchQueue(label: "net.qwdtt.client.ios.health.ping", qos: .utility)

    private var stateTimer: AnyCancellable?
    private var pingTimer: AnyCancellable?
    private var pingConnection: NWConnection?
    private var pingGeneration: UInt64 = 0
    private var tunnelActive = false
    private var consecutivePingFailures = 0

    private init() {
        stateTimer = Timer.publish(every: 1.0, on: .main, in: .common)
            .autoconnect()
            .sink { [weak self] _ in
                self?.syncNetworkLabel()
            }

        pingTimer = Timer.publish(every: 5.0, on: .main, in: .common)
            .autoconnect()
            .sink { [weak self] _ in
                self?.measurePingIfNeeded()
            }
    }

    func setTunnelActive(_ active: Bool) {
        guard tunnelActive != active else { return }
        tunnelActive = active

        if active {
            syncNetworkLabel()
            measurePingIfNeeded()
        } else {
            pingGeneration &+= 1
            pingConnection?.cancel()
            pingConnection = nil
            pingMilliseconds = nil
            networkLabel = "—"
            consecutivePingFailures = 0
            reachabilityPoor = false
        }
    }

    var pingText: String {
        guard let pingMilliseconds else { return "—" }
        return "\(pingMilliseconds) мс"
    }

    func qualityText(
        isRunning: Bool,
        isConnecting: Bool,
        isRecovering: Bool,
        activeConnections: Int
    ) -> String {
        if isRecovering {
            return "Переподключение"
        }
        if isConnecting && !isRunning {
            return "Подключение"
        }
        guard isRunning else { return "—" }
        if reachabilityPoor || networkLabel == "Нет сети" {
            return "Нестабильное"
        }

        if let ping = pingMilliseconds {
            if ping <= 80 && activeConnections > 0 {
                return "Отличное"
            }
            if ping <= 180 {
                return "Стабильное"
            }
            return "Нестабильное"
        }

        // Absence of an RTT sample is not itself a failure. In particular, a
        // brief active-workers=0 sample never turns this state red by itself.
        return "Стабильное"
    }

    private func syncNetworkLabel() {
        guard tunnelActive else {
            if networkLabel != "—" { networkLabel = "—" }
            return
        }

        let value = defaults.string(forKey: AppGroup.Keys.physicalNetworkLabel) ?? "—"
        if networkLabel != value {
            networkLabel = value
        }
    }

    private func measurePingIfNeeded() {
        guard tunnelActive, pingConnection == nil else { return }

        pingGeneration &+= 1
        let generation = pingGeneration
        let started = DispatchTime.now()
        let connection = NWConnection(
            host: NWEndpoint.Host("1.1.1.1"),
            port: NWEndpoint.Port(integerLiteral: 443),
            using: .tcp
        )
        pingConnection = connection

        connection.stateUpdateHandler = { [weak self, weak connection] state in
            guard let self else { return }
            switch state {
            case .ready:
                let elapsed = DispatchTime.now().uptimeNanoseconds - started.uptimeNanoseconds
                DispatchQueue.main.async { [weak self, weak connection] in
                    self?.finishPing(
                        generation: generation,
                        connection: connection,
                        value: Int(elapsed / 1_000_000)
                    )
                }
            case .failed:
                DispatchQueue.main.async { [weak self, weak connection] in
                    self?.finishPing(generation: generation, connection: connection, value: nil)
                }
            default:
                break
            }
        }

        connection.start(queue: pingQueue)
        pingQueue.asyncAfter(deadline: .now() + 2.5) { [weak self, weak connection] in
            DispatchQueue.main.async { [weak self, weak connection] in
                self?.finishPing(generation: generation, connection: connection, value: nil)
            }
        }
    }

    private func finishPing(generation: UInt64, connection: NWConnection?, value: Int?) {
        guard pingGeneration == generation else { return }
        pingGeneration &+= 1

        if pingConnection === connection {
            pingConnection = nil
        }
        connection?.cancel()
        applyPingResult(value)
    }

    private func applyPingResult(_ value: Int?) {
        if let value {
            pingMilliseconds = max(1, value)
            consecutivePingFailures = 0
            reachabilityPoor = false
            return
        }

        consecutivePingFailures += 1
        // Keep the last good value through short packet/path hiccups. Only
        // declare the connection poor after three consecutive failed probes.
        if consecutivePingFailures >= 3 {
            pingMilliseconds = nil
            reachabilityPoor = true
        }
    }
}
