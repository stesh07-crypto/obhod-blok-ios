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
        goDnsMode    = defaults.string(forKey: "go_dns_mode") ?? "cloudflare"
        obfsMode     = defaults.string(forKey: "obfs_mode") ?? "tls"
        vkAnonPath   = defaults.string(forKey: "vk_anon_path") ?? "/voice/join"
        detailedLogs = defaults.bool(forKey: "detailed_logs")
        autoReconnect = defaults.bool(forKey: "auto_reconnect")
    }

    // DNS presets
    static let dnsPresets: [(title: String, value: String)] = [
        ("Cloudflare (1.1.1.1)", "cloudflare"),
        ("Google (8.8.8.8)", "google"),
        ("Яндекс", "yandex"),
        ("DoH Cloudflare", "doh:https://cloudflare-dns.com/dns-query"),
        ("DoH Google", "doh:https://dns.google/dns-query"),
    ]

    // Obfuscation presets
    static let obfsPresets: [(title: String, value: String)] = [
        ("TLS (рекомендуется)", "tls"),
        ("HTTP", "http"),
        ("Без маскировки", "none"),
    ]
}
