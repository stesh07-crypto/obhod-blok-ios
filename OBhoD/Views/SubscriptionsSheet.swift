import SwiftUI

struct SubscriptionsSheet: View {
    @EnvironmentObject var profilesStore: ProfilesStore
    @Environment(\.dismiss) private var dismiss

    @State private var urlText: String
    @State private var isLoading = false
    @State private var errorMessage: String? = nil
    @State private var pendingProfiles: [ConnectionProfile] = []
    @State private var showConfirm = false
    @State private var parsedName: String? = nil
    @State private var parsedExpires: Int64 = 0

    init(initialURL: String? = nil) {
        _urlText = State(initialValue: initialURL ?? "")
    }

    var body: some View {
        NavigationView {
            Form {
                Section(
                    header: Text("Ссылка подписки"),
                    footer: Text("Вставьте ссылку https://, qwdtt:// или скопируйте ключ из бота.")
                ) {
                    HStack {
                        TextField("https://... или qwdtt://...", text: $urlText)
                            .keyboardType(.URL)
                            .autocorrectionDisabled()
                            .textInputAutocapitalization(.never)

                        if !urlText.isEmpty {
                            Button {
                                urlText = ""
                            } label: {
                                Image(systemName: "xmark.circle.fill")
                                    .foregroundColor(.secondary)
                            }
                            .buttonStyle(.plain)
                        }
                    }

                    Button {
                        if let clip = UIPasteboard.general.string {
                            urlText = clip.trimmingCharacters(in: .whitespacesAndNewlines)
                        }
                    } label: {
                        HStack {
                            Image(systemName: "doc.on.clipboard")
                            Text("Вставить из буфера")
                        }
                        .font(.system(size: 14))
                        .foregroundColor(.orange)
                    }

                    if let error = errorMessage {
                        Label(error, systemImage: "exclamationmark.circle")
                            .foregroundColor(.red)
                            .font(.caption)
                    }
                }

                Section {
                    Button {
                        Task { await importFromURL() }
                    } label: {
                        HStack {
                            Spacer()
                            if isLoading {
                                ProgressView().tint(.white)
                            } else {
                                Label("Импортировать", systemImage: "arrow.down.circle.fill")
                            }
                            Spacer()
                        }
                    }
                    .disabled(urlText.trimmingCharacters(in: .whitespaces).isEmpty || isLoading)
                    .listRowBackground(Color.orange)
                    .foregroundColor(.white)
                    .font(.body.bold())
                }

                Section(header: Text("Где взять подписку?")) {
                    Button {
                        if let botURL = URL(string: "https://t.me/OBHOD_INT_BOT?start=profile") {
                            UIApplication.shared.open(botURL)
                        }
                    } label: {
                        HStack {
                            Image(systemName: "paperplane.fill")
                                .foregroundColor(.blue)
                            Text("Получить ключ в @OBHOD_INT_BOT")
                                .foregroundColor(.primary)
                            Spacer()
                            Image(systemName: "arrow.up.forward.app")
                                .foregroundColor(.secondary)
                        }
                    }
                }
            }
            .navigationTitle("Добавить подписку")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Закрыть") { dismiss() }
                }
            }
            .alert("Импорт подписки", isPresented: $showConfirm) {
                Button("Добавить (\(pendingProfiles.count))", role: .none) {
                    var addedIds: [String] = []
                    let groupName = parsedName ?? "OBhoD"
                    for var p in pendingProfiles {
                        if p.name.isEmpty || p.name == "Imported" || p.name.range(of: "^[0-9a-f]{8}-[0-9a-f]{4}-", options: .regularExpression) != nil {
                            p.name = groupName
                        }
                        profilesStore.add(p)
                        addedIds.append(p.id)
                    }
                    if let firstId = addedIds.first {
                        profilesStore.setActive(id: firstId)
                    }
                    dismiss()
                }
                Button("Отмена", role: .cancel) {}
            } message: {
                let badge = SubscriptionImport.formatRemainingDaysBadge(expiresAt: parsedExpires)
                Text("\(parsedName ?? "OBhoD")\nНайдено профилей: \(pendingProfiles.count)\nСтатус: \(badge)")
            }
        }
        .onAppear {
            if !urlText.isEmpty {
                Task { await importFromURL() }
            }
        }
    }

    // MARK: – Import

    private func importFromURL() async {
        errorMessage = nil
        let raw = urlText.trimmingCharacters(in: .whitespaces)
        guard !raw.isEmpty else { return }

        isLoading = true
        defer { isLoading = false }

        // Handle qwdtt://import?url=<subURL>
        var fetchURL = raw
        if raw.lowercased().hasPrefix("qwdtt://import"),
           let comps = URLComponents(string: raw),
           let subUrlParam = comps.queryItems?.first(where: { $0.name == "url" })?.value {
            fetchURL = subUrlParam
        }

        // Handle direct qwdtt://config URI (single profile)
        if fetchURL.lowercased().hasPrefix("qwdtt://config") || fetchURL.lowercased().hasPrefix("qwdtt:config") {
            if let result = SubscriptionImport.parse(rawText: fetchURL) {
                pendingProfiles = result.profiles
                parsedName = result.subscriptionName
                parsedExpires = result.expiresAt
                showConfirm = true
            } else {
                errorMessage = "Не удалось разобрать конфигурацию qwdtt://"
            }
            return
        }

        guard let url = URL(string: fetchURL) else {
            errorMessage = "Неверный формат адреса URL"
            return
        }

        do {
            let result = try await SubscriptionImport.fetch(url: url)
            pendingProfiles = result.profiles
            parsedName = result.subscriptionName
            parsedExpires = result.expiresAt
            profilesStore.saveSubscriptionURL(url.absoluteString)
            showConfirm = true
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

// MARK: – Profile Edit View

struct ProfileEditView: View {
    @EnvironmentObject var profilesStore: ProfilesStore
    @Environment(\.dismiss) private var dismiss
    @State var profile: ConnectionProfile

    var body: some View {
        NavigationView {
            Form {
                Section("Основное") {
                    TextField("Название", text: $profile.name)
                    TextField("Сервер (IP:порт)", text: $profile.peer)
                        .keyboardType(.URL)
                        .autocorrectionDisabled()
                }

                Section("Параметры соединения") {
                    TextField("Хэши VK (через запятую)", text: $profile.vkHashes)
                        .font(.system(size: 13, design: .monospaced))
                    TextField("Пароль", text: $profile.password)
                    Stepper("Потоков на хэш: \(profile.workersPerHash)", value: $profile.workersPerHash, in: 1...64)
                    Stepper("Порт: \(profile.listenPort)", value: $profile.listenPort, in: 1000...65535, step: 1)
                }

                Section("Статистика") {
                    HStack {
                        Text("Срок действия")
                        Spacer()
                        Text(profile.expirationBadge)
                            .foregroundColor(profile.isExpired ? .red : .secondary)
                    }
                    if profile.trafficMb > 0 {
                        HStack {
                            Text("Трафик")
                            Spacer()
                            Text(String(format: "%.1f МБ", profile.trafficMb))
                                .foregroundColor(.secondary)
                        }
                    }
                }
            }
            .navigationTitle("Редактировать")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Сохранить") {
                        profilesStore.update(profile)
                        dismiss()
                    }
                }
                ToolbarItem(placement: .cancellationAction) {
                    Button("Отмена") { dismiss() }
                }
            }
        }
    }
}
