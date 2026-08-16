import SwiftUI

struct ProfilesView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
    @EnvironmentObject var profilesStore: ProfilesStore

    @State private var showSubscriptionsSheet = false
    @State private var initialURL: String? = nil
    @State private var editingProfile: ConnectionProfile? = nil
    @State private var showToast = false
    @State private var toastMessage = ""

    var body: some View {
        NavigationView {
            ScrollView {
                VStack(spacing: 16) {
                    // ── 1. 3D Neon Quick Connect Card ──────────────────────────
                    NeonConnectHeroCard(
                        currentProfile: profilesStore.currentProfile,
                        isRunning: tunnelManager.isRunning,
                        isConnecting: tunnelManager.isConnecting,
                        onToggle: toggleConnection
                    )
                    .padding(.horizontal)
                    .padding(.top, 8)

                    // ── 2. Real live stats from TunnelExtension / Go ───────────
                    LiveStatsGrid(
                        isRunning: tunnelManager.isRunning,
                        isConnecting: tunnelManager.isConnecting,
                        uptime: tunnelManager.uptimeString,
                        activeConnections: tunnelManager.activeConnections,
                        downloaded: tunnelManager.downloadedMBString,
                        uploaded: tunnelManager.uploadedMBString
                    )
                    .padding(.horizontal)

                    // ── 3. Header & Action Row ─────────────────────────────────
                    HStack {
                        Text("Серверы и профили")
                            .font(.system(size: 18, weight: .bold))
                            .foregroundColor(.primary)

                        Spacer()

                        Button {
                            showSubscriptionsSheet = true
                        } label: {
                            HStack(spacing: 4) {
                                Image(systemName: "plus.circle.fill")
                                Text("Добавить")
                            }
                            .font(.system(size: 14, weight: .semibold))
                            .foregroundColor(.orange)
                        }
                    }
                    .padding(.horizontal)
                    .padding(.top, 8)

                    // ── 4. Profile List ────────────────────────────────────────
                    if profilesStore.profiles.isEmpty {
                        EmptyProfilesCard(showSheet: $showSubscriptionsSheet)
                            .padding(.horizontal)
                    } else {
                        LazyVStack(spacing: 12) {
                            ForEach(profilesStore.profiles) { profile in
                                ProfileCardView(
                                    profile: profile,
                                    isActive: profile.id == profilesStore.currentProfileId,
                                    isRunning: tunnelManager.isRunning && profile.id == profilesStore.currentProfileId,
                                    onSelect: { selectProfile(profile) },
                                    onEdit: { editingProfile = profile },
                                    onDelete: { profilesStore.remove(id: profile.id) }
                                )
                            }
                        }
                        .padding(.horizontal)
                    }
                }
                .padding(.bottom, 24)
            }
            .refreshable {
                let res = await profilesStore.refreshSubscriptions()
                if res.refreshed > 0 {
                    toastMessage = "Обновлено \(res.refreshed) профилей"
                    showToast = true
                } else if res.failed > 0 {
                    toastMessage = "Не удалось обновить подписку"
                    showToast = true
                }
            }
            .navigationTitle("OBhoD")
            .toolbar {
                ToolbarItemGroup(placement: .navigationBarTrailing) {
                    if profilesStore.isRefreshing {
                        ProgressView()
                            .tint(.orange)
                    } else {
                        Button {
                            Task {
                                let res = await profilesStore.refreshSubscriptions()
                                if res.refreshed > 0 {
                                    toastMessage = "Обновлено \(res.refreshed) профилей"
                                    showToast = true
                                }
                            }
                        } label: {
                            Image(systemName: "arrow.clockwise")
                                .foregroundColor(.primary)
                        }
                    }

                    Button {
                        showSubscriptionsSheet = true
                    } label: {
                        Image(systemName: "qrcode.viewfinder")
                            .foregroundColor(.orange)
                    }
                }
            }
            .sheet(isPresented: $showSubscriptionsSheet, onDismiss: {
                initialURL = nil
            }) {
                SubscriptionsSheet(initialURL: initialURL)
            }
            .sheet(item: $editingProfile) { profile in
                ProfileEditView(profile: profile)
            }
            .overlay(alignment: .bottom) {
                if showToast {
                    ToastBanner(message: toastMessage)
                        .transition(.move(edge: .bottom).combined(with: .opacity))
                        .onAppear {
                            DispatchQueue.main.asyncAfter(deadline: .now() + 2.5) {
                                withAnimation { showToast = false }
                            }
                        }
                        .padding(.bottom, 16)
                }
            }
        }
        .onReceive(NotificationCenter.default.publisher(for: .importSubscriptionURL)) { note in
            if let url = note.object as? URL {
                initialURL = url.absoluteString
                showSubscriptionsSheet = true
            }
        }
    }

    private func toggleConnection() {
        if tunnelManager.isRunning {
            tunnelManager.disconnect()
        } else if let profile = profilesStore.currentProfile {
            tunnelManager.connect(profile: profile)
        } else if let first = profilesStore.profiles.first {
            profilesStore.setActive(id: first.id)
            tunnelManager.connect(profile: first)
        } else {
            showSubscriptionsSheet = true
        }
    }

    private func selectProfile(_ profile: ConnectionProfile) {
        if tunnelManager.isRunning && profilesStore.currentProfileId == profile.id {
            tunnelManager.disconnect()
            return
        }
        if tunnelManager.isRunning {
            tunnelManager.disconnect()
        }
        profilesStore.setActive(id: profile.id)
        tunnelManager.connect(profile: profile)
    }
}

// MARK: – 1. 3D Neon Connect Hero Card

private struct NeonConnectHeroCard: View {
    let currentProfile: ConnectionProfile?
    let isRunning: Bool
    let isConnecting: Bool
    let onToggle: () -> Void

    @State private var isPulsing = false

    var body: some View {
        VStack(spacing: 12) {
            Button(action: onToggle) {
                ZStack {
                    Circle()
                        .stroke(ringColor.opacity(0.3), lineWidth: 10)
                        .frame(width: 140, height: 140)
                        .scaleEffect(isConnecting ? (isPulsing ? 1.15 : 0.95) : 1.0)
                        .animation(isConnecting ? .easeInOut(duration: 0.8).repeatForever(autoreverses: true) : .default, value: isPulsing)

                    Circle()
                        .fill(
                            LinearGradient(
                                colors: isRunning
                                    ? [Color.green.opacity(0.85), Color.mint]
                                    : (isConnecting
                                        ? [Color.orange.opacity(0.85), Color.yellow]
                                        : [Color.orange, Color.red.opacity(0.85)]),
                                startPoint: .topLeading,
                                endPoint: .bottomTrailing
                            )
                        )
                        .frame(width: 110, height: 110)
                        .shadow(color: ringColor.opacity(0.6), radius: isRunning ? 16 : 8, x: 0, y: 0)

                    VStack(spacing: 4) {
                        Image(systemName: isRunning ? "shield.checkmark.fill" : (isConnecting ? "arrow.triangle.2.circlepath" : "power"))
                            .font(.system(size: 34, weight: .bold))
                            .foregroundColor(.white)
                            .rotationEffect(isConnecting ? .degrees(isPulsing ? 360 : 0) : .degrees(0))
                            .animation(isConnecting ? .linear(duration: 1.5).repeatForever(autoreverses: false) : .default, value: isPulsing)

                        Text(statusLabel)
                            .font(.system(size: 12, weight: .bold))
                            .foregroundColor(.white.opacity(0.95))
                    }
                }
            }
            .buttonStyle(.plain)
            .onAppear { isPulsing = true }

            // Profile name intentionally removed from under the connect button.
            if let expBadge = currentProfile?.expirationBadge {
                Text(expBadge)
                    .font(.system(size: 11, weight: .medium))
                    .foregroundColor(currentProfile?.isExpired == true ? .red : .secondary)
            }
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 20)
        .background(
            RoundedRectangle(cornerRadius: 24, style: .continuous)
                .fill(Color(UIColor.secondarySystemGroupedBackground))
                .shadow(color: Color.black.opacity(0.06), radius: 10, x: 0, y: 4)
        )
    }

    private var ringColor: Color {
        if isRunning { return .green }
        if isConnecting { return .orange }
        return .orange.opacity(0.6)
    }

    private var statusLabel: String {
        if isConnecting { return "ПОДКЛЮЧЕНИЕ" }
        if isRunning { return "АКТИВЕН" }
        return "ВКЛЮЧИТЬ"
    }
}

// MARK: – 2. Live Stats 2x2 Grid

private struct LiveStatsGrid: View {
    let isRunning: Bool
    let isConnecting: Bool
    let uptime: String
    let activeConnections: Int
    let downloaded: String
    let uploaded: String

    var body: some View {
        LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: 12) {
            StatCard(
                icon: "timer",
                title: "Время сессии",
                value: isRunning ? uptime : "00:00",
                color: .blue
            )
            StatCard(
                icon: "point.3.connected.trianglepath.dotted",
                title: "Активных",
                value: "\(activeConnections)",
                color: activeConnections > 0 ? .green : (isConnecting ? .orange : .purple)
            )
            StatCard(
                icon: "arrow.down.circle.fill",
                title: "Скачано",
                value: downloaded,
                color: .teal
            )
            StatCard(
                icon: "arrow.up.circle.fill",
                title: "Загружено",
                value: uploaded,
                color: .indigo
            )
        }
    }
}

private struct StatCard: View {
    let icon: String
    let title: String
    let value: String
    let color: Color

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: icon)
                .font(.system(size: 18))
                .foregroundColor(color)
                .frame(width: 32, height: 32)
                .background(color.opacity(0.12))
                .clipShape(RoundedRectangle(cornerRadius: 8))

            VStack(alignment: .leading, spacing: 2) {
                Text(title)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                Text(value)
                    .font(.system(size: 13, weight: .semibold, design: .rounded))
                    .foregroundColor(.primary)
                    .lineLimit(1)
            }
            Spacer()
        }
        .padding(10)
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(Color(UIColor.secondarySystemGroupedBackground))
        )
    }
}

// MARK: – 3. Profile Card View

private struct ProfileCardView: View {
    let profile: ConnectionProfile
    let isActive: Bool
    let isRunning: Bool
    let onSelect: () -> Void
    let onEdit: () -> Void
    let onDelete: () -> Void

    var body: some View {
        HStack(spacing: 12) {
            Button(action: onSelect) {
                ZStack {
                    Circle()
                        .fill(isRunning ? Color.green : (isActive ? Color.orange : Color(UIColor.tertiarySystemFill)))
                        .frame(width: 44, height: 44)

                    Image(systemName: isRunning ? "stop.fill" : "play.fill")
                        .foregroundColor(isRunning || isActive ? .white : .secondary)
                        .font(.system(size: 15, weight: .bold))
                }
            }
            .buttonStyle(.plain)

            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 6) {
                    Text(profile.name)
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundColor(.primary)

                    if isActive {
                        Text("ТЕКУЩИЙ")
                            .font(.system(size: 9, weight: .bold))
                            .padding(.horizontal, 6)
                            .padding(.vertical, 2)
                            .background(Color.orange.opacity(0.15))
                            .foregroundColor(.orange)
                            .clipShape(Capsule())
                    }
                }

                HStack(spacing: 8) {
                    Text(profile.expirationBadge)
                        .font(.system(size: 11, weight: .medium))
                        .foregroundColor(profile.isExpired ? .red : .secondary)

                    if profile.trafficMb > 0 {
                        Text("•")
                            .foregroundColor(.secondary)
                        Text(String(format: "%.1f МБ", profile.trafficMb))
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                    }
                }
            }

            Spacer()

            Menu {
                Button(action: onEdit) {
                    Label("Редактировать", systemImage: "pencil")
                }
                Button(role: .destructive, action: onDelete) {
                    Label("Удалить", systemImage: "trash")
                }
            } label: {
                Image(systemName: "ellipsis")
                    .font(.system(size: 16, weight: .semibold))
                    .foregroundColor(.secondary)
                    .frame(width: 32, height: 32)
            }
        }
        .padding(12)
        .background(
            RoundedRectangle(cornerRadius: 18, style: .continuous)
                .fill(Color(UIColor.secondarySystemGroupedBackground))
                .overlay(
                    RoundedRectangle(cornerRadius: 18, style: .continuous)
                        .stroke(isActive ? Color.orange.opacity(0.4) : Color.clear, lineWidth: 1.5)
                )
        )
    }
}

// MARK: – Empty State

private struct EmptyProfilesCard: View {
    @Binding var showSheet: Bool

    var body: some View {
        VStack(spacing: 12) {
            Image(systemName: "server.rack")
                .font(.system(size: 40))
                .foregroundColor(.orange)

            Text("Список серверов пуст")
                .font(.system(size: 16, weight: .semibold))

            Text("Добавьте профиль или подписку из Telegram-бота")
                .font(.system(size: 13))
                .foregroundColor(.secondary)
                .multilineTextAlignment(.center)

            Button {
                showSheet = true
            } label: {
                Label("Добавить подписку", systemImage: "plus.circle.fill")
                    .font(.system(size: 14, weight: .semibold))
                    .padding(.horizontal, 16)
                    .padding(.vertical, 8)
                    .background(Color.orange)
                    .foregroundColor(.white)
                    .clipShape(Capsule())
            }
            .padding(.top, 4)
        }
        .frame(maxWidth: .infinity)
        .padding(24)
        .background(
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .fill(Color(UIColor.secondarySystemGroupedBackground))
        )
    }
}

// MARK: – Toast Banner

private struct ToastBanner: View {
    let message: String

    var body: some View {
        Text(message)
            .font(.system(size: 13, weight: .medium))
            .foregroundColor(.white)
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
            .background(Color.black.opacity(0.85))
            .clipShape(Capsule())
            .shadow(radius: 6)
    }
}
