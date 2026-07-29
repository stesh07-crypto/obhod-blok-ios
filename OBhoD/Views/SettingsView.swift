import SwiftUI

struct SettingsView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
    @StateObject private var settings = SettingsStore.shared

    var body: some View {
        NavigationView {
            Form {
                // ── Network ───────────────────────────────────────────────
                Section(header: Text("🌐 Сеть")) {
                    Picker("DNS", selection: $settings.goDnsMode) {
                        ForEach(SettingsStore.dnsPresets, id: \.value) { preset in
                            Text(preset.title).tag(preset.value)
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
                    header: Text("🔑 VK (анонимный режим)"),
                    footer: Text("Путь для анонимного получения TURN. Обычно оставьте как есть.")
                ) {
                    HStack {
                        Text("Путь VK")
                        Spacer()
                        TextField("/voice/join", text: $settings.vkAnonPath)
                            .multilineTextAlignment(.trailing)
                            .font(.system(size: 14, design: .monospaced))
                            .foregroundColor(.secondary)
                    }
                }

                // ── Logs ──────────────────────────────────────────────────
                Section(header: Text("📋 Логи")) {
                    Toggle("Подробные логи", isOn: $settings.detailedLogs)
                    Toggle("Авто-переподключение", isOn: $settings.autoReconnect)
                }

                // ── Subscriptions ──────────────────────────────────────────
                Section(
                    header: Text("⚡ Профиль и подписка"),
                    footer: Text("Получите профиль из Telegram-бота или обновите текущую подписку.")
                ) {
                    Button {
                        if let botURL = URL(string: "https://t.me/OBHOD_INT_BOT?start=profile") {
                            UIApplication.shared.open(botURL)
                        }
                    } label: {
                        HStack {
                            Label("⚡ Получить мой профиль", systemImage: "bolt.fill")
                                .foregroundColor(.orange)
                                .fontWeight(.semibold)
                            Spacer()
                            Image(systemName: "arrow.up.forward.app")
                                .foregroundColor(.secondary)
                        }
                    }
                }

                // ── About ─────────────────────────────────────────────────
                Section(header: Text("ℹ️ О приложении")) {
                    HStack {
                        Text("OBhoD iOS")
                        Spacer()
                        Text(Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "")
                            .foregroundColor(.secondary)
                    }
                    Link("Поддержка в Telegram (@OBHOD_INT_BOT)", destination: URL(string: "https://t.me/OBHOD_INT_BOT")!)
                }
            }
            .navigationTitle("Настройки")
        }
    }
}
