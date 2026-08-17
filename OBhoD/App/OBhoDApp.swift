import SwiftUI
import UIKit

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
                .onOpenURL { url in
                    Task { @MainActor in
                        DeepLinkRouter.handle(url)
                    }
                }
                .onContinueUserActivity(NSUserActivityTypeBrowsingWeb) { activity in
                    if let url = activity.webpageURL {
                        Task { @MainActor in
                            DeepLinkRouter.handle(url)
                        }
                    }
                }
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
        Task { @MainActor in
            DeepLinkRouter.handle(url)
        }
        return true
    }

    func application(
        _ application: UIApplication,
        continue userActivity: NSUserActivity,
        restorationHandler: @escaping ([UIUserActivityRestoring]?) -> Void
    ) -> Bool {
        guard userActivity.activityType == NSUserActivityTypeBrowsingWeb,
              let url = userActivity.webpageURL else {
            return false
        }
        Task { @MainActor in
            DeepLinkRouter.handle(url)
        }
        return true
    }
}

// MARK: – Deep links

@MainActor
private enum DeepLinkRouter {
    private static var lastHandledURL = ""
    private static var lastHandledAt = Date.distantPast

    static func handle(_ url: URL) {
        let absolute = url.absoluteString
        let now = Date()

        // SwiftUI and UIApplicationDelegate may both receive the same URL.
        // Treat those callbacks as one event, not two profile imports.
        if absolute == lastHandledURL,
           now.timeIntervalSince(lastHandledAt) < 2.0 {
            return
        }
        lastHandledURL = absolute
        lastHandledAt = now

        // Bot universal link: https://test-36.ru/import?url=<subscription>
        if absolute.contains("test-36.ru/import"),
           let comps = URLComponents(url: url, resolvingAgainstBaseURL: false),
           let target = comps.queryItems?.first(where: { $0.name == "url" })?.value,
           let subURL = URL(string: target) {
            importSubscription(subURL)
            return
        }

        // Legacy bot link: https://test-36.ru/connect/{uuid}
        if absolute.contains("test-36.ru/connect/") {
            let components = url.pathComponents
            if let uuid = components.last, !uuid.isEmpty, uuid != "connect",
               let subURL = URL(string: "https://test-36.ru/sub/\(uuid)?format=qwdtt") {
                importSubscription(subURL)
                return
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

            if let subURL = URL(string: targetString) {
                importSubscription(subURL)
                return
            }
        }

        // qwdtt://config?peer=...&hashes=...&pass=...  (single profile QR)
        if url.host == "config" || url.absoluteString.hasPrefix("qwdtt://config") {
            NotificationCenter.default.post(
                name: .didReceiveQwdttConfig,
                object: url
            )
        }
    }

    /// Import directly into ProfilesStore instead of relying on a transient
    /// NotificationCenter event. This makes cold-start deep links reliable:
    /// the import can finish even before SwiftUI has mounted its onReceive hooks.
    /// If the remote payload cannot be fetched or parsed, fall back to the
    /// existing import sheet and put the URL on the clipboard for recovery.
    private static func importSubscription(_ rawURL: URL) {
        let subURL = normalizedQwdttSubscriptionURL(rawURL)

        Task { @MainActor in
            do {
                let importedCount = try await ProfilesStore.shared.importSubscription(from: subURL)
                NotificationCenter.default.post(
                    name: .didAutoImportSubscription,
                    object: importedCount
                )
            } catch {
                UIPasteboard.general.string = subURL.absoluteString
                NotificationCenter.default.post(
                    name: .didReceiveSubscriptionURL,
                    object: subURL
                )
            }
        }
    }

    /// The bot format for OBhoD is qwdtt. Keep links generated without an
    /// explicit format backward-compatible by adding it client-side.
    private static func normalizedQwdttSubscriptionURL(_ url: URL) -> URL {
        guard url.scheme?.lowercased().hasPrefix("http") == true,
              var components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              components.host?.lowercased() == "test-36.ru",
              components.path.hasPrefix("/sub/") else {
            return url
        }

        var items = components.queryItems ?? []
        if let formatIndex = items.firstIndex(where: { $0.name.lowercased() == "format" }) {
            items[formatIndex] = URLQueryItem(name: "format", value: "qwdtt")
        } else {
            items.append(URLQueryItem(name: "format", value: "qwdtt"))
        }
        components.queryItems = items
        return components.url ?? url
    }
}

// MARK: – Notification Names

extension Notification.Name {
    static let didReceiveSubscriptionURL = Notification.Name("didReceiveSubscriptionURL")
    static let didAutoImportSubscription = Notification.Name("didAutoImportSubscription")
    static let didReceiveQwdttConfig     = Notification.Name("didReceiveQwdttConfig")
}
