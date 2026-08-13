import SwiftUI

struct SettingsView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
    @StateObject private var settings = SettingsStore.shared

    var body: some View {
        NavigationView {
            Form {
                // ── Network ───────────────────────────────────────────────
                Section(header: Text("🌐 Сеть и DNS")) {
                    Picker("DNS Режим", selection: $settings.goDnsMode) {
                        ForEach(SettingsStore.dnsPresets, id: \.value) { preset in
                            VStack(alignment: .leading) {
                                Text(preset.title)
                            }
                            .tag(preset.value)
                        }
                    }

                    Picker("Маскировка трафика", selection: $settings.obfsMode) {
                        ForEach(SettingsStore.obfsPresets, id: \.value) { preset in
                            Text(preset.title).tag(preset.value)
                        }
                    }
                }

                // ── VK ────────────────────────────────────────────────────
                Section(
                    header: Text("🔑 VK TURN (анонимный режим)"),
                    footer: Text("Путь для анонимного получения TURN без авторизации.")
                ) {
                    HStack {
                        Text("Путь")
                        Spacer()
                        TextField("/voice/join", text: $settings.vkAnonPath)
                            .multilineTextAlignment(.trailing)
                            .font(.system(size: 14, design: .monospaced))
                            .foregroundColor(.secondary)
                    }
                }

                // ── Tunnel Behavior ───────────────────────────────────────
                Section(header: Text("⚙️ Поведение туннеля")) {
                    Toggle("Авто-переподключение", isOn: $settings.autoReconnect)
                    Toggle("Подробные логи", isOn: $settings.detailedLogs)
                }

                // ── Bot Profile Link ──────────────────────────────────────
                Section(
                    header: Text("⚡ Профиль и бот"),
                    footer: Text("Управление подпиской и получение новых серверов через официального Telegram-бота.")
                ) {
                    Button {
                        if let botURL = URL(string: "https://t.me/OBHOD_INT_BOT?start=profile") {
                            UIApplication.shared.open(botURL)
                        }
                    } label: {
                        HStack {
                            Image(systemName: "paperplane.fill")
                                .foregroundColor(.blue)
                            Text("Открыть Telegram-бота")
                                .font(.body.bold())
                                .foregroundColor(.primary)
                            Spacer()
                            Image(systemName: "arrow.up.forward.app")
                                .foregroundColor(.secondary)
                        }
                    }
                }

                // ── About ─────────────────────────────────────────────────
                Section(header: Text("ℹ️ О приложении")) {
                    HStack {
                        Text("Название")
                        Spacer()
                        Text("OBhoD (iOS)")
                            .foregroundColor(.secondary)
                    }

                    HStack {
                        Text("Версия")
                        Spacer()
                        Text("1.4 (b117)")
                            .foregroundColor(.secondary)
                    }

                    HStack {
                        Text("Ядро")
                        Spacer()
                        Text("v1.4.0 (Go/TURN)")
                            .foregroundColor(.secondary)
                    }

                    Link("Поддержка (@OBHOD_INT_BOT)", destination: URL(string: "https://t.me/OBHOD_INT_BOT")!)
                }
            }
            .navigationTitle("Настройки")
        }
    }
}
