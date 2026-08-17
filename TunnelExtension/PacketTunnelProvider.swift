import NetworkExtension
import Network
import Foundation
import Darwin

/// Packet Tunnel Provider — the single owner of the Go/TURN/WireGuard runtime.
///
/// Startup is intentionally staged:
/// 1. Bootstrap TURN/DTLS and receive WireGuard configuration.
/// 2. Install iOS packet-tunnel routes.
/// 3. Raise WireGuard.
/// 4. Start the packet pump and report success to NetworkExtension.
final class PacketTunnelProvider: NEPacketTunnelProvider {

    private let defaults = AppGroup.sharedDefaults
    private let workQueue = DispatchQueue(label: "net.qwdtt.client.ios.tunnel.bootstrap", qos: .userInitiated)
    private let logQueue = DispatchQueue(label: "net.qwdtt.client.ios.tunnel.logs")
    private let pathQueue = DispatchQueue(label: "net.qwdtt.client.ios.tunnel.path")

    private let lifecycleLock = NSLock()
    private var generation: UInt64 = 0
    private var pendingStartCompletion: ((Error?) -> Void)?

    private var pathMonitor: NWPathMonitor?
    private var pathDebounceWorkItem: DispatchWorkItem?
    private var lastStablePath: PhysicalPath?
    private var pathWasUnavailable = false

    private static let errorLogPrefix = "[[WDTT_ERROR]] "

    // MARK: Lifecycle

    override func startTunnel(
        options: [String: NSObject]?,
        completionHandler: @escaping (Error?) -> Void
    ) {
        NSLog("[WDTT-Ext] startTunnel called")
        let startGeneration = beginStart(completionHandler: completionHandler)

        let build = Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "?"
        appendLog("[ДИАГ] NetworkExtension startTunnel: build=\(build), generation=\(startGeneration)", isError: false)

        defaults?.set(false, forKey: AppGroup.Keys.tunnelRunning)
        defaults?.set(false, forKey: AppGroup.Keys.transportRecovering)
        defaults?.set("—", forKey: AppGroup.Keys.physicalNetworkLabel)
        defaults?.set("Подготовка транспорта…", forKey: AppGroup.Keys.lastStats)

        guard
            let data = defaults?.data(forKey: AppGroup.Keys.activeProfileJSON),
            let profile = try? JSONDecoder().decode(ConnectionProfile.self, from: data)
        else {
            appendLog("[ДИАГ] startTunnel abort: активный профиль не прочитан из App Group", isError: true)
            finishStart(generation: startGeneration, error: TunnelError.noActiveProfile)
            return
        }

        installGoCallbacks()

        let dns = defaults?.string(forKey: AppGroup.Keys.goDnsMode) ?? "cloudflare"
        let obfsMode = defaults?.string(forKey: AppGroup.Keys.obfsMode) ?? "tls"
        let vkAnonPath = defaults?.string(forKey: AppGroup.Keys.vkAnonPath) ?? "/voice/join"
        let deviceID = defaults?.string(forKey: AppGroup.Keys.deviceID) ?? "ios-\(profile.id)"

        workQueue.async { [weak self] in
            guard let self, self.isCurrent(startGeneration) else { return }

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
                self.appendLog("[ДИАГ] GoClient.start завершился кодом \(result)", isError: true)
                self.failStart(startGeneration, error: TunnelError.startFailed)
                return
            }

            self.defaults?.set("Подключение TURN/DTLS…", forKey: AppGroup.Keys.lastStats)

            guard GoClient.waitUntilTransportReady(timeout: 75) else {
                self.appendLog("[ДИАГ] Bootstrap timeout: TURN/DTLS не подтвердил ready за 75с", isError: true)
                self.failStart(startGeneration, error: TunnelError.transportTimeout)
                return
            }
            guard self.isCurrent(startGeneration) else { return }

            let networkConfig = GoClient.networkConfiguration()
            let settings = self.makeNetworkSettings(profile: profile, config: networkConfig)
            self.defaults?.set("Настройка VPN-маршрутов…", forKey: AppGroup.Keys.lastStats)

            self.setTunnelNetworkSettings(settings) { [weak self] error in
                guard let self else { return }
                guard self.isCurrent(startGeneration) else { return }

                if let error {
                    NSLog("[WDTT-Ext] setTunnelNetworkSettings error: \(error)")
                    self.appendLog("[ДИАГ] setTunnelNetworkSettings ERROR: \(error.localizedDescription)", isError: true)
                    self.failStart(startGeneration, error: error)
                    return
                }

                self.workQueue.async { [weak self] in
                    guard let self, self.isCurrent(startGeneration) else { return }

                    self.defaults?.set("Запуск WireGuard…", forKey: AppGroup.Keys.lastStats)
                    guard GoClient.activateWireGuard() else {
                        self.appendLog("[ДИАГ] WDTT_ActivateWireGuard вернул ошибку", isError: true)
                        self.failStart(startGeneration, error: TunnelError.wireGuardStartFailed)
                        return
                    }
                    guard GoClient.waitUntilWireGuardReady(timeout: 20) else {
                        self.appendLog("[ДИАГ] WireGuard ready timeout: 20с", isError: true)
                        self.failStart(startGeneration, error: TunnelError.wireGuardTimeout)
                        return
                    }
                    guard self.isCurrent(startGeneration) else { return }

                    self.installPacketBridge(generation: startGeneration)
                    self.startPathMonitor(generation: startGeneration)

                    self.defaults?.set(true, forKey: AppGroup.Keys.tunnelRunning)
                    self.defaults?.set(false, forKey: AppGroup.Keys.transportRecovering)
                    self.defaults?.set("Подключено", forKey: AppGroup.Keys.lastStats)
                    self.appendLog("[VPN] Туннель полностью готов", isError: false)
                    self.appendLog("[ДИАГ] Tunnel ready: NetworkExtension + routes + WireGuard + packet bridge активны", isError: false)
                    NSLog("[WDTT-Ext] Tunnel started successfully")
                    self.finishStart(generation: startGeneration, error: nil)
                }
            }
        }
    }

    override func stopTunnel(
        with reason: NEProviderStopReason,
        completionHandler: @escaping () -> Void
    ) {
        NSLog("[WDTT-Ext] stopTunnel: \(reason.rawValue)")

        let reasonName = stopReasonName(rawValue: reason.rawValue)
        let statsSnapshot = defaults?.string(forKey: AppGroup.Keys.lastStats) ?? "нет stats"
        let goAlive = GoClient.isRunning
        let wasMarkedRunning = defaults?.bool(forKey: AppGroup.Keys.tunnelRunning) ?? false
        let unexpected = reason.rawValue != 0 && reason.rawValue != 1
        appendLog(
            "[ДИАГ] stopTunnel: reason=\(reasonName)(\(reason.rawValue)), goAlive=\(goAlive), tunnelRunning=\(wasMarkedRunning), lastStats=\(statsSnapshot)",
            isError: unexpected
        )

        let pending = invalidateLifecycleForStop()
        pending?(TunnelError.cancelled)

        stopPathMonitor()
        GoClient.clearPacketHandler()
        defaults?.set(false, forKey: AppGroup.Keys.tunnelRunning)
        defaults?.set(false, forKey: AppGroup.Keys.transportRecovering)
        defaults?.set("—", forKey: AppGroup.Keys.physicalNetworkLabel)
        defaults?.set("Отключено", forKey: AppGroup.Keys.lastStats)

        let gate = CompletionGate(completionHandler)
        DispatchQueue.global(qos: .userInitiated).async {
            GoClient.stop()
            gate.finish()
        }
        DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + 3.0) {
            gate.finish()
        }
    }

    override func sleep(completionHandler: @escaping () -> Void) {
        appendLog("[VPN] sleep: соединения сохранены", isError: false)
        appendLog("[ДИАГ] iOS sleep: Go/WireGuard не останавливаем", isError: false)
        completionHandler()
    }

    override func wake() {
        appendLog("[VPN] wake: проверка здоровья транспорта", isError: false)
        appendLog("[ДИАГ] iOS wake: отправлен health-check без teardown", isError: false)
        GoClient.wakeHealthCheck()
    }

    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)?) {
        guard let command = String(data: messageData, encoding: .utf8) else {
            completionHandler?(Data("invalid".utf8))
            return
        }

        switch command {
        case "reconnect":
            let accepted = GoClient.requestTransportReconnect()
            appendLog("[ДИАГ] providerMessage reconnect: accepted=\(accepted)", isError: false)
            completionHandler?(Data((accepted ? "ok" : "busy").utf8))
        default:
            completionHandler?(Data("unsupported".utf8))
        }
    }

    // MARK: Go callbacks

    private func installGoCallbacks() {
        GoClient.setLogHandler { [weak self] line, isError in
            self?.updateTransportRecoveryState(from: line)
            self?.appendLog(line, isError: isError)
        }
        GoClient.setStatsHandler { [weak self] stats in
            self?.defaults?.set(stats, forKey: AppGroup.Keys.lastStats)
        }
    }

    private func updateTransportRecoveryState(from line: String) {
        if line.contains("[СЕТЬ] Перезапуск транспорта:") {
            defaults?.set(true, forKey: AppGroup.Keys.transportRecovering)
            return
        }

        if line.contains("[IOS-TUN] WireGuard поднят") {
            defaults?.set(false, forKey: AppGroup.Keys.transportRecovering)
            return
        }

        if line.contains("[ГО-ВОРКЕР] Все воркеры завершены"),
           defaults?.bool(forKey: AppGroup.Keys.tunnelRunning) == true {
            defaults?.set(true, forKey: AppGroup.Keys.transportRecovering)
        }
    }

    // MARK: Packet bridge

    private func installPacketBridge(generation: UInt64) {
        GoClient.setPacketHandler { [weak self] data in
            guard let self, self.isCurrent(generation) else { return }
            let family = self.protocolFamily(for: data)
            self.packetFlow.writePackets([data], withProtocols: [NSNumber(value: family)])
        }
        readPackets(generation: generation)
    }

    private func readPackets(generation: UInt64) {
        packetFlow.readPackets { [weak self] packets, _ in
            guard let self, self.isCurrent(generation) else { return }
            for packet in packets {
                GoClient.writePacket(packet)
            }
            self.readPackets(generation: generation)
        }
    }

    private func protocolFamily(for packet: Data) -> Int32 {
        guard let first = packet.first else { return AF_INET }
        return (first >> 4) == 6 ? AF_INET6 : AF_INET
    }

    // MARK: Network settings

    private func makeNetworkSettings(
        profile: ConnectionProfile,
        config: GoTunnelNetworkConfiguration?
    ) -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: profile.peer)
        let mtu = min(max(config?.mtu ?? 1280, 576), 1500)
        settings.mtu = NSNumber(value: mtu)

        if let address = config?.ipv4Address, !address.isEmpty {
            let prefix = min(max(config?.ipv4Prefix ?? 32, 0), 32)
            let ipv4 = NEIPv4Settings(
                addresses: [address],
                subnetMasks: [ipv4SubnetMask(prefixLength: prefix)]
            )
            ipv4.includedRoutes = [NEIPv4Route.default()]
            settings.ipv4Settings = ipv4
        } else {
            let ipv4 = NEIPv4Settings(addresses: ["10.77.0.2"], subnetMasks: ["255.255.255.0"])
            ipv4.includedRoutes = [NEIPv4Route.default()]
            settings.ipv4Settings = ipv4
        }

        if let address = config?.ipv6Address, !address.isEmpty {
            let prefix = min(max(config?.ipv6Prefix ?? 128, 0), 128)
            let ipv6 = NEIPv6Settings(
                addresses: [address],
                networkPrefixLengths: [NSNumber(value: prefix)]
            )
            ipv6.includedRoutes = [NEIPv6Route.default()]
            settings.ipv6Settings = ipv6
        }

        let dnsServers = config?.dnsServers.isEmpty == false
            ? (config?.dnsServers ?? [])
            : ["1.1.1.1", "8.8.8.8"]
        let dnsSettings = NEDNSSettings(servers: dnsServers)
        dnsSettings.matchDomains = [""]
        settings.dnsSettings = dnsSettings

        return settings
    }

    private func ipv4SubnetMask(prefixLength: Int) -> String {
        let prefix = min(max(prefixLength, 0), 32)
        let mask: UInt32
        if prefix == 0 {
            mask = 0
        } else {
            mask = UInt32.max << UInt32(32 - prefix)
        }
        return [24, 16, 8, 0].map { shift in
            String((mask >> UInt32(shift)) & 0xff)
        }.joined(separator: ".")
    }

    // MARK: Physical network changes

    private enum PhysicalPath: String {
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

    private func startPathMonitor(generation: UInt64) {
        stopPathMonitor()
        lastStablePath = nil
        pathWasUnavailable = false

        let monitor = NWPathMonitor()
        pathMonitor = monitor
        monitor.pathUpdateHandler = { [weak self] path in
            self?.pathQueue.async { [weak self] in
                self?.handlePath(path, generation: generation)
            }
        }
        monitor.start(queue: pathQueue)
        appendLog("[ДИАГ] NWPathMonitor запущен", isError: false)
    }

    private func handlePath(_ path: Network.NWPath, generation: UInt64) {
        guard isCurrent(generation) else { return }

        pathDebounceWorkItem?.cancel()

        guard path.status == .satisfied else {
            let previous = lastStablePath?.rawValue ?? "none"
            pathWasUnavailable = true
            defaults?.set("Нет сети", forKey: AppGroup.Keys.physicalNetworkLabel)
            appendLog("[СЕТЬ] Физическая сеть временно недоступна", isError: false)
            appendLog("[ДИАГ] NWPath unsatisfied: previous=\(previous), WG остаётся жив; ждём восстановление workers", isError: false)
            return
        }

        let pathType: PhysicalPath
        if path.usesInterfaceType(.wifi) {
            pathType = .wifi
        } else if path.usesInterfaceType(.cellular) {
            pathType = .cellular
        } else if path.usesInterfaceType(.wiredEthernet) {
            pathType = .wired
        } else {
            pathType = .other
        }

        guard pathType != .other else {
            pathWasUnavailable = true
            appendLog("[ДИАГ] NWPath satisfied iface=other: считаем переходным состоянием, WireGuard не трогаем", isError: false)
            return
        }

        let previous = lastStablePath
        let needsReconnect = previous != nil && (previous != pathType || pathWasUnavailable)
        lastStablePath = pathType
        pathWasUnavailable = false

        guard needsReconnect else {
            defaults?.set(pathType.label, forKey: AppGroup.Keys.physicalNetworkLabel)
            appendLog("[СЕТЬ] Активный интерфейс: \(pathType.rawValue)", isError: false)
            return
        }

        if let previous, previous != pathType {
            defaults?.set("\(previous.label) → \(pathType.label)", forKey: AppGroup.Keys.physicalNetworkLabel)
        } else {
            defaults?.set(pathType.label, forKey: AppGroup.Keys.physicalNetworkLabel)
        }

        let item = DispatchWorkItem { [weak self] in
            guard let self, self.isCurrent(generation) else { return }
            let previousName = previous?.rawValue ?? "temporary-unavailable"
            self.appendLog("[СЕТЬ] Стабильный handoff на \(pathType.rawValue); проверяем здоровье транспорта", isError: false)
            self.appendLog("[ДИАГ] Handoff \(previousName) → \(pathType.rawValue): только health-check, TUN/WireGuard не перезапускаются", isError: false)
            GoClient.notifyNetworkChange()

            self.pathQueue.asyncAfter(deadline: .now() + 2.0) { [weak self] in
                guard let self,
                      self.isCurrent(generation),
                      self.lastStablePath == pathType else { return }
                self.defaults?.set(pathType.label, forKey: AppGroup.Keys.physicalNetworkLabel)
            }
        }
        pathDebounceWorkItem = item
        pathQueue.asyncAfter(deadline: .now() + 1.5, execute: item)
    }

    private func stopPathMonitor() {
        pathDebounceWorkItem?.cancel()
        pathDebounceWorkItem = nil
        pathMonitor?.cancel()
        pathMonitor = nil
        lastStablePath = nil
        pathWasUnavailable = false
    }

    // MARK: Lifecycle generation

    private func beginStart(completionHandler: @escaping (Error?) -> Void) -> UInt64 {
        lifecycleLock.lock()
        generation &+= 1
        let value = generation
        pendingStartCompletion = completionHandler
        lifecycleLock.unlock()
        return value
    }

    private func isCurrent(_ value: UInt64) -> Bool {
        lifecycleLock.lock()
        defer { lifecycleLock.unlock() }
        return generation == value
    }

    private func finishStart(generation value: UInt64, error: Error?) {
        lifecycleLock.lock()
        guard generation == value else {
            lifecycleLock.unlock()
            return
        }
        let completion = pendingStartCompletion
        pendingStartCompletion = nil
        lifecycleLock.unlock()
        completion?(error)
    }

    private func failStart(_ generation: UInt64, error: Error) {
        guard isCurrent(generation) else { return }
        defaults?.set(false, forKey: AppGroup.Keys.tunnelRunning)
        defaults?.set(false, forKey: AppGroup.Keys.transportRecovering)
        defaults?.set("—", forKey: AppGroup.Keys.physicalNetworkLabel)
        defaults?.set("Ошибка подключения", forKey: AppGroup.Keys.lastStats)
        appendLog("[VPN] \(error.localizedDescription)", isError: true)
        appendLog("[ДИАГ] failStart: generation=\(generation), error=\(error.localizedDescription)", isError: true)
        GoClient.clearPacketHandler()
        GoClient.stop()
        finishStart(generation: generation, error: error)
    }

    private func invalidateLifecycleForStop() -> ((Error?) -> Void)? {
        lifecycleLock.lock()
        generation &+= 1
        let pending = pendingStartCompletion
        pendingStartCompletion = nil
        lifecycleLock.unlock()
        return pending
    }

    private func stopReasonName(rawValue: Int) -> String {
        switch rawValue {
        case 0: return "none"
        case 1: return "userInitiated"
        case 2: return "providerFailed"
        case 3: return "noNetworkAvailable"
        case 4: return "unrecoverableNetworkChange"
        case 5: return "providerDisabled"
        case 6: return "authenticationCanceled"
        case 7: return "configurationFailed"
        case 8: return "idleTimeout"
        case 9: return "configurationDisabled"
        case 10: return "configurationRemoved"
        case 11: return "superseded"
        case 12: return "userLogout"
        case 13: return "userSwitch"
        case 14: return "connectionFailed"
        case 15: return "sleep"
        case 16: return "appUpdate"
        default: return "unknown"
        }
    }

    // MARK: Logging

    private func appendLog(_ line: String, isError: Bool) {
        logQueue.async { [weak self] in
            guard let self else { return }
            var lines = self.defaults?.stringArray(forKey: AppGroup.Keys.lastLogLines) ?? []
            lines.append(isError ? Self.errorLogPrefix + line : line)
            if lines.count > 100 {
                lines = Array(lines.suffix(100))
            }
            self.defaults?.set(lines, forKey: AppGroup.Keys.lastLogLines)
        }
    }
}

// MARK: Errors

enum TunnelError: LocalizedError {
    case noActiveProfile
    case startFailed
    case transportTimeout
    case wireGuardStartFailed
    case wireGuardTimeout
    case cancelled

    var errorDescription: String? {
        switch self {
        case .noActiveProfile:
            return "Не найден активный профиль"
        case .startFailed:
            return "Не удалось запустить сетевое ядро"
        case .transportTimeout:
            return "TURN/DTLS не стал готов за отведённое время"
        case .wireGuardStartFailed:
            return "Не удалось активировать WireGuard"
        case .wireGuardTimeout:
            return "WireGuard не подтвердил готовность"
        case .cancelled:
            return "Запуск VPN отменён"
        }
    }
}

private final class CompletionGate: @unchecked Sendable {
    private let lock = NSLock()
    private var isFinished = false
    private let completion: () -> Void

    init(_ completion: @escaping () -> Void) {
        self.completion = completion
    }

    func finish() {
        lock.lock()
        guard !isFinished else {
            lock.unlock()
            return
        }
        isFinished = true
        lock.unlock()
        completion()
    }
}
