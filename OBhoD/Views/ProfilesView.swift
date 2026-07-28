import SwiftUI

struct ProfilesView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
    @EnvironmentObject var profilesStore: ProfilesStore

    @State private var showSubscriptionsSheet = false
    @State private var editingProfile: ConnectionProfile? = nil
    @State private var showAddProfile = false

    var body: some View {
        NavigationView {
            VStack(spacing: 0) {
                // ── Status Banner ──────────────────────────────────────────
                StatusBanner()
                    .padding(.horizontal)
                    .padding(.top, 12)

                // ── Profile List ───────────────────────────────────────────
                if profilesStore.profiles.isEmpty {
                    Spacer()
                    EmptyProfilesView(showSheet: $showSubscriptionsSheet)
                    Spacer()
                } else {
                    List {
                        ForEach(profilesStore.profiles) { profile in
                            ProfileRow(
                                profile: profile,
                                isActive: profile.id == profilesStore.currentProfileId,
                                isRunning: tunnelManager.isRunning && profile.id == profilesStore.currentProfileId,
                                onTap: { selectAndConnect(profile) },
                                onEdit: { editingProfile = profile }
                            )
                        }
                        .onDelete { profilesStore.remove(at: $0) }
                        .onMove { profilesStore.move(from: $0, to: $1) }
                    }
                    .listStyle(.insetGrouped)
                }
            }
            .navigationTitle("OBhoD")
            .toolbar {
                ToolbarItemGroup(placement: .navigationBarLeading) {
                    if tunnelManager.isRunning || tunnelManager.isConnecting {
                        Button(role: .destructive) {
                            tunnelManager.disconnect()
                        } label: {
                            Label("Отключить", systemImage: "stop.circle.fill")
                                .foregroundColor(.red)
                        }
                    }
                }
                ToolbarItemGroup(placement: .navigationBarTrailing) {
                    EditButton()
                    Button { showSubscriptionsSheet = true } label: {
                        Image(systemName: "plus.circle.fill")
                    }
                }
            }
            .sheet(isPresented: $showSubscriptionsSheet) {
                SubscriptionsSheet()
            }
            .sheet(item: $editingProfile) { profile in
                ProfileEditView(profile: profile)
            }
        }
        .onReceive(NotificationCenter.default.publisher(for: .importSubscriptionURL)) { note in
            if let url = note.object as? URL {
                showSubscriptionsSheet = true
                // Pass URL to sheet via notification (SubscriptionsSheet handles it)
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) {
                    NotificationCenter.default.post(name: .beginImportFromURL, object: url)
                }
            }
        }
    }

    private func selectAndConnect(_ profile: ConnectionProfile) {
        if tunnelManager.isRunning && profilesStore.currentProfileId == profile.id {
            tunnelManager.disconnect()
            return
        }
        if tunnelManager.isRunning { tunnelManager.disconnect() }
        profilesStore.setActive(id: profile.id)
        tunnelManager.connect(profile: profile)
    }
}

// MARK: – Status Banner

private struct StatusBanner: View {
    @EnvironmentObject var tunnelManager: TunnelManager

    var body: some View {
        HStack(spacing: 12) {
            Circle()
                .fill(statusColor)
                .frame(width: 10, height: 10)
                .shadow(color: statusColor.opacity(0.6), radius: 4)

            VStack(alignment: .leading, spacing: 2) {
                Text(statusTitle)
                    .font(.system(size: 14, weight: .semibold))
                if tunnelManager.isRunning {
                    Text(tunnelManager.stats)
                        .font(.system(size: 11, design: .monospaced))
                        .foregroundColor(.secondary)
                        .lineLimit(1)
                }
            }

            Spacer()

            if tunnelManager.isRunning, let since = tunnelManager.connectedSince {
                TimelineView(.periodic(from: since, by: 1)) { _ in
                    Text(tunnelManager.uptimeString)
                        .font(.system(size: 12, design: .monospaced))
                        .foregroundColor(.secondary)
                }
            }
        }
        .padding(12)
        .background(
            RoundedRectangle(cornerRadius: 14, style: .continuous)
                .fill(statusColor.opacity(0.12))
        )
        .animation(.easeInOut(duration: 0.3), value: tunnelManager.isRunning)
    }

    private var statusColor: Color {
        if tunnelManager.isRunning   { return .green }
        if tunnelManager.isConnecting { return .orange }
        return .gray
    }

    private var statusTitle: String {
        if tunnelManager.isConnecting { return "Подключение…" }
        if tunnelManager.isRunning    { return "🛡️ Подключено" }
        return "Отключено"
    }
}

// MARK: – Profile Row

private struct ProfileRow: View {
    let profile: ConnectionProfile
    let isActive: Bool
    let isRunning: Bool
    let onTap: () -> Void
    let onEdit: () -> Void

    var body: some View {
        HStack(spacing: 12) {
            // Connect button
            Button(action: onTap) {
                ZStack {
                    Circle()
                        .fill(isRunning ? Color.green : Color.orange)
                        .frame(width: 44, height: 44)
                    Image(systemName: isRunning ? "stop.fill" : "play.fill")
                        .foregroundColor(.white)
                        .font(.system(size: 16, weight: .bold))
                }
            }
            .buttonStyle(.plain)

            VStack(alignment: .leading, spacing: 3) {
                Text(profile.name)
                    .font(.system(size: 15, weight: .semibold))
                Text(profile.peer)
                    .font(.system(size: 12, design: .monospaced))
                    .foregroundColor(.secondary)
                    .lineLimit(1)
                HStack(spacing: 6) {
                    Label("\(profile.workersPerHash) потоков", systemImage: "cpu")
                    if profile.trafficMb > 0 {
                        Label(String(format: "%.1f МБ", profile.trafficMb), systemImage: "arrow.up.arrow.down")
                    }
                }
                .font(.system(size: 11))
                .foregroundColor(.secondary)
            }

            Spacer()

            Button(action: onEdit) {
                Image(systemName: "pencil.circle")
                    .foregroundColor(.secondary)
            }
            .buttonStyle(.plain)
        }
        .padding(.vertical, 6)
        .listRowBackground(isActive ? Color.orange.opacity(0.08) : Color.clear)
    }
}

// MARK: – Empty state

private struct EmptyProfilesView: View {
    @Binding var showSheet: Bool

    var body: some View {
        VStack(spacing: 16) {
            Image(systemName: "shield.slash")
                .font(.system(size: 52))
                .foregroundColor(.orange.opacity(0.7))
            Text("Нет профилей")
                .font(.title3).fontWeight(.semibold)
            Text("Добавьте подписку из бота\nили вставьте ссылку qwdtt://")
                .multilineTextAlignment(.center)
                .foregroundColor(.secondary)
            Button {
                showSheet = true
            } label: {
                Label("Добавить подписку", systemImage: "plus.circle")
                    .font(.system(size: 15, weight: .semibold))
                    .padding(.horizontal, 24).padding(.vertical, 12)
                    .background(Color.orange)
                    .foregroundColor(.white)
                    .clipShape(Capsule())
            }
        }
    }
}

// MARK: – Notification

extension Notification.Name {
    static let beginImportFromURL = Notification.Name("beginImportFromURL")
}
