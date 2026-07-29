import SwiftUI

@main
struct OBhoDApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) var delegate
    @StateObject private var tunnelManager = TunnelManager.shared
    @StateObject private var profilesStore = ProfilesStore.shared

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(tunnelManager)
                .environmentObject(profilesStore)
        }
    }
}

// MARK: – AppDelegate

final class AppDelegate: NSObject, UIApplicationDelegate {

    func application(
        _ app: UIApplication,
        open url: URL,
        options: [UIApplication.OpenURLOptionsKey: Any] = [:]
    ) -> Bool {
        handleQwdttURL(url)
        return true
    }

    func application(
        _ application: UIApplication,
        continue userActivity: NSUserActivity,
        restorationHandler: @escaping ([UIUserActivityRestoring]?) -> Void
    ) -> Bool {
        if userActivity.activityType == NSUserActivityTypeBrowsingWeb,
           let url = userActivity.webpageURL {
            handleQwdttURL(url)
            return true
        }
        return false
    }

    // MARK: Private

    private func handleQwdttURL(_ url: URL) {
        let absolute = url.absoluteString

        // Intercept bot links: https://test-36.ru/connect/{uuid}
        if absolute.contains("test-36.ru/connect/") {
            let components = url.pathComponents
            if let uuid = components.last, !uuid.isEmpty, uuid != "connect" {
                let subUrlString = "https://test-36.ru/sub/\(uuid)?format=qwdtt"
                if let subUrl = URL(string: subUrlString) {
                    Task { @MainActor in
                        NotificationCenter.default.post(
                            name: .didReceiveSubscriptionURL,
                            object: subUrl
                        )
                    }
                    return
                }
            }
        }

        guard url.scheme?.lowercased() == "qwdtt" else { return }

        // qwdtt://import?url=<encoded_sub_url>
        if url.host == "import",
           let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
           let subUrlParam = components.queryItems?.first(where: { $0.name == "url" })?.value {
            
            var targetString = subUrlParam
            if targetString.contains("test-36.ru/connect/") {
                let parts = targetString.components(separatedBy: "/connect/")
                if parts.count > 1, let uuid = parts.last?.components(separatedBy: "?").first {
                    targetString = "https://test-36.ru/sub/\(uuid)?format=qwdtt"
                }
            }
            
            if let subUrl = URL(string: targetString) {
                Task { @MainActor in
                    NotificationCenter.default.post(
                        name: .didReceiveSubscriptionURL,
                        object: subUrl
                    )
                }
                return
            }
        }

        // qwdtt://config?peer=...&hashes=...&pass=...  (single profile QR)
        if url.host == "config" || url.absoluteString.hasPrefix("qwdtt://config") {
            Task { @MainActor in
                NotificationCenter.default.post(
                    name: .didReceiveQwdttConfig,
                    object: url
                )
            }
        }
    }
}

// MARK: – Notification Names

extension Notification.Name {
    static let didReceiveSubscriptionURL = Notification.Name("didReceiveSubscriptionURL")
    static let didReceiveQwdttConfig     = Notification.Name("didReceiveQwdttConfig")
}
