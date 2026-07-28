import Foundation

/// Shared constants between the main app and TunnelExtension
enum AppGroup {
    static let identifier       = "group.net.qwdtt.client.ios"
    static let tunnelBundleId   = "net.qwdtt.client.ios.tunnel"
    static let appBundleId      = "net.qwdtt.client.ios"

    // UserDefaults keys (written by app, read by extension and vice-versa)
    enum Keys {
        static let activeProfileJSON    = "active_profile_json"
        static let tunnelRunning        = "tunnel_running"
        static let lastStats            = "last_stats"
        static let lastLogLines         = "last_log_lines"
        static let startRequested       = "start_requested"
        static let stopRequested        = "stop_requested"
    }

    static var sharedDefaults: UserDefaults? {
        UserDefaults(suiteName: identifier)
    }
}
