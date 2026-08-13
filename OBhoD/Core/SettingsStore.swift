import Foundation

/// App-level settings (mirrors Android SettingsStore)
@MainActor
final class SettingsStore: ObservableObject {
    static let shared = SettingsStore()

    private let defaults = UserDefaults.standard

    @Published var goDnsMode: String {
        didSet { defaults.set(goDnsMode, forKey: "go_dns_mode") }
    }
    @Published var obfsMode: String {
        didSet { defaults.set(obfsMode, forKey: "obfs_mode") }
    }
    @Published var vkAnonPath: String {
        didSet { defaults.set(vkAnonPath, forKey: "vk_anon_path") }
    }
    @Published var detailedLogs: Bool {
        didSet { defaults.set(detailedLogs, forKey: "detailed_logs") }
    }
    @Published var autoReconnect: Bool {
        didSet { defaults.set(autoReconnect, forKey: "auto_reconnect") }
    }

    private init() {
        goDnsMode     = defaults.string(forKey: "go_dns_mode") ?? "cloudflare"
        obfsMode      = defaults.string(forKey: "obfs_mode") ?? "tls"
        vkAnonPath    = defaults.string(forKey: "vk_anon_path") ?? "/voice/join"
        detailedLogs  = defaults.bool(forKey: "detailed_logs")
        autoReconnect = defaults.object(forKey: "auto_reconnect") == nil ? true : defaults.bool(forKey: "auto_reconnect")
    }

    // DNS presets
    static let dnsPresets: [(title: String, subtitle: String, value: String)] = [
        ("Cloudflare", "1.1.1.1 — Быстрый и приватный", "cloudflare"),
        ("Google DNS", "8.8.8.8 — Стабильный глобальный", "google"),
        ("AdGuard DoH", "Блокировка рекламы и трекеров", "doh:https://dns.adguard-dns.com/dns-query"),
        ("Cloudflare DoH", "Шифрованный DNS через HTTPS", "doh:https://cloudflare-dns.com/dns-query"),
        ("Google DoH", "Шифрованный Google DNS через HTTPS", "doh:https://dns.google/dns-query"),
        ("Яндекс DNS", "Быстрый DNS для РФ", "yandex"),
    ]

    // Obfuscation presets
    static let obfsPresets: [(title: String, subtitle: String, value: String)] = [
        ("TLS Маскировка (Рекомендуется)", "Имитирует защищенный HTTPS трафик", "tls"),
        ("HTTP Маскировка", "Обычный веб-трафик", "http"),
        ("Без маскировки", "Прямой TURN протокол", "none"),
    ]
}
