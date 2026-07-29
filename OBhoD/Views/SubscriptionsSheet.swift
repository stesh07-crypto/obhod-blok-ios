import SwiftUI

struct SubscriptionsSheet: View {
    @EnvironmentObject var profilesStore: ProfilesStore
    @Environment(\.dismiss) private var dismiss

    @State private var urlText = ""
    @State private var isLoading = false
    @State private var errorMessage: String? = nil
    @State private var pendingProfiles: [ConnectionProfile] = []
    @State private var showConfirm = false
    @State private var parsedName: String? = nil

    var body: some View {
        NavigationView {
            Form {
                Section(
                    header: Text("Ссылка подписки"),
                    footer: Text("Ссылка начинается с https:// или qwdtt://\nОтсканируйте QR из бота или вставьте вручную")
                ) {
                    TextField("https://... или qwdtt://import?url=...", text: $urlText)
                        .keyboardType(.URL)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)

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
                    .fontWeight(.semibold)
                }
            }
            .navigationTitle("Добавить подписку")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Закрыть") { dismiss() }
                }
            }
            .alert("Добавить профили?", isPresented: $showConfirm) {
                Button("Добавить \(pendingProfiles.count) профилей", role: .none) {
                    var addedIds: [String] = []
                    let groupName = parsedName ?? "OBhoD_BLOK"
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
                Text(parsedName.map { "Подписка: \($0)" } ?? "")
            }
        }
        .onReceive(NotificationCenter.default.publisher(for: .beginImportFromURL)) { note in
            if let url = note.object as? URL {
                urlText = url.absoluteString
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
                showConfirm = true
            } else {
                errorMessage = "Не удалось разобрать qwdtt:// URI"
            }
            return
        }

        guard let url = URL(string: fetchURL) else {
            errorMessage = "Неверный URL"
            return
        }

        do {
            let result = try await SubscriptionImport.fetch(url: url)
            pendingProfiles = result.profiles
            parsedName = result.subscriptionName
            profilesStore.saveSubscriptionURL(url.absoluteString)
            showConfirm = true
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

// MARK: – Profile Edit View (simple)

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
                Section("Параметры") {
                    TextField("Хэши VK (через запятую)", text: $profile.vkHashes)
                        .font(.system(size: 13, design: .monospaced))
                    TextField("Пароль", text: $profile.password)
                    Stepper("Потоков: \(profile.workersPerHash)", value: $profile.workersPerHash, in: 1...64)
                    Stepper("Порт: \(profile.listenPort)", value: $profile.listenPort, in: 1000...65535, step: 1)
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
