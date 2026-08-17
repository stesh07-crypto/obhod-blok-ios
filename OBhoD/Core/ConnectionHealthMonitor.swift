import Foundation
import Network
import Combine

/// Lightweight UI-side health monitor.
///
/// It never owns or restarts the VPN runtime. The NetworkExtension remains the
/// single owner of TURN/WireGuard; this object only reports the current physical
/// interface and an approximate end-to-end TCP RTT for presentation in the app.
final class ConnectionHealthMonitor: ObservableObject {
    static let shared = ConnectionHealthMonitor()

    @Published private(set) var pingMilliseconds: Int?
    @Published private(set) var networkLabel = "—"
    @Published private(set) var isTransitioning = false
    @Published private(set) var reachabilityPoor = false

    private enum InterfaceKind: Equatable {
        case wifi
        case cellular
        case wired
        case other

        var label: String {
            switch self {
            case .wifi: return "Wi‑Fi"
            case .cellular: return "LTE"
            case .wired: return "Ethernet"
            case .other: return "Сеть"
            }
        }
    }

    private let pathMonitor = NWPathMonitor()
    private let pathQueue = DispatchQueue(label: "net.qwdtt.client.ios.health.path", qos: .utility)
    private let pingQueue = DispatchQueue(label: "net.qwdtt.client.ios.health.ping", qos: .utility)

    private var pingTimer: AnyCancellable?
    private var pingConnection: NWConnection?
    private var lastInterface: InterfaceKind?
    private var transitionGeneration: UInt64 = 0
    private var pingGeneration: UInt64 = 0
    private var tunnelActive = false
    private var consecutivePingFailures = 0

    private init() {
        startPathMonitoring()
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
            measurePingIfNeeded()
        } else {
            pingGeneration &+= 1
            pingConnection?.cancel()
            pingConnection = nil
            pingMilliseconds = nil
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
        // This label is driven by the real transport-recovery state from the
        // NetworkExtension. A mere UI path transition or one zero-worker sample
        // is not enough to claim that the VPN is reconnecting.
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

        // No RTT sample yet is not a failure. In particular, do not turn a
        // brief active-workers=0 sample into a red/unstable UI state.
        return "Стабильное"
    }

    private func startPathMonitoring() {
        pathMonitor.pathUpdateHandler = { [weak self] path in
            let kind: InterfaceKind?
            if path.status != .satisfied {
                kind = nil
            } else if path.usesInterfaceType(.wifi) {
                kind = .wifi
            } else if path.usesInterfaceType(.cellular) {
                kind = .cellular
            } else if path.usesInterfaceType(.wiredEthernet) {
                kind = .wired
            } else {
                kind = .other
            }

            DispatchQueue.main.async { [weak self] in
                self?.applyPath(kind)
            }
        }
        pathMonitor.start(queue: pathQueue)
    }

    private func applyPath(_ newInterface: InterfaceKind?) {
        transitionGeneration &+= 1
        let generation = transitionGeneration

        guard let newInterface else {
            isTransitioning = false
            networkLabel = "Нет сети"
            lastInterface = nil
            return
        }

        let previous = lastInterface
        lastInterface = newInterface

        guard let previous, previous != newInterface else {
            isTransitioning = false
            networkLabel = newInterface.label
            return
        }

        isTransitioning = true
        networkLabel = "\(previous.label) → \(newInterface.label)"

        DispatchQueue.main.asyncAfter(deadline: .now() + 2.0) { [weak self] in
            guard let self, self.transitionGeneration == generation else { return }
            self.isTransitioning = false
            self.networkLabel = newInterface.label
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
