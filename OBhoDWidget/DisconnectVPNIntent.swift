import Foundation
import NetworkExtension
#if canImport(AppIntents)
import AppIntents

@available(iOS 16.4, *)
public struct DisconnectVPNIntent: AppIntent {
    public static var title: LocalizedStringResource = "Отключить OBhoD"
    public static var description = IntentDescription("Отключает VPN-соединение без открытия приложения")
    public static var openAppWhenRun: Bool = false

    public init() {}

    public func perform() async throws -> some IntentResult {
        // 1. Флаг в App Group для немедленной остановки внутри туннельного расширения
        if let defaults = UserDefaults(suiteName: "group.net.qwdtt.client.ios") {
            defaults.set(true, forKey: "request_disconnect")
            defaults.set("disconnected", forKey: "widget_status")
        }

        // 2. Остановка туннеля через системный менеджер NetworkExtension
        let managers = try? await NETunnelProviderManager.loadAllFromPreferences()
        for manager in managers ?? [] {
            if manager.connection.status == .connected || manager.connection.status == .connecting {
                manager.connection.stopVPNTunnel()
            }
        }

        return .result()
    }
}
#endif
