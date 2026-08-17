import SwiftUI

struct ContentView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
    @EnvironmentObject var profilesStore: ProfilesStore
    @State private var selectedTab = 0

    var body: some View {
        TabView(selection: $selectedTab) {
            ProfilesView()
                .tabItem {
                    Label("Профили", systemImage: "server.rack")
                }
                .tag(0)

            LogsView()
                .tabItem {
                    Label("Логи", systemImage: tunnelManager.unreadErrors > 0
                          ? "exclamationmark.bubble.fill"
                          : "text.alignleft")
                }
                .badge(tunnelManager.unreadErrors > 0 ? tunnelManager.unreadErrors : 0)
                .tag(1)

            SettingsView()
                .tabItem {
                    Label("Настройки", systemImage: "gearshape")
                }
                .tag(2)
        }
        .accentColor(.orange)
        .onReceive(NotificationCenter.default.publisher(for: .didReceiveSubscriptionURL)) { note in
            if let url = note.object as? URL {
                selectedTab = 0
                NotificationCenter.default.post(name: .importSubscriptionURL, object: url)
            }
        }
        .onReceive(NotificationCenter.default.publisher(for: .didAutoImportSubscription)) { _ in
            selectedTab = 0
        }
        .onReceive(NotificationCenter.default.publisher(for: .didReceiveQwdttConfig)) { note in
            if let url = note.object as? URL {
                selectedTab = 0
                NotificationCenter.default.post(name: .importQwdttConfig, object: url)
            }
        }
    }
}

extension Notification.Name {
    static let importSubscriptionURL = Notification.Name("importSubscriptionURL")
    static let importQwdttConfig     = Notification.Name("importQwdttConfig")
}
