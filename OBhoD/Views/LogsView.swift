import SwiftUI

struct LogsView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
    @ObservedObject private var connectionHealth = ConnectionHealthMonitor.shared
    @State private var showOnlyErrors = false

    var filteredLogs: [LogEntry] {
        showOnlyErrors ? tunnelManager.logs.filter { $0.isError } : tunnelManager.logs
    }

    var body: some View {
        NavigationView {
            VStack(spacing: 0) {
                liveStatsHeader

                Group {
                    if filteredLogs.isEmpty {
                        VStack(spacing: 12) {
                            Spacer()
                            Image(systemName: "text.alignleft")
                                .font(.system(size: 48))
                                .foregroundColor(.secondary.opacity(0.5))
                            Text(tunnelManager.logs.isEmpty ? "Логи появятся при подключении" : "Нет ошибок")
                                .foregroundColor(.secondary)
                            Spacer()
                        }
                    } else {
                        ScrollViewReader { proxy in
                            List(filteredLogs) { entry in
                                HStack(alignment: .top, spacing: 8) {
                                    Circle()
                                        .fill(entry.isError ? Color.red : Color.green)
                                        .frame(width: 7, height: 7)
                                        .padding(.top, 5)

                                    Text(entry.message)
                                        .font(.system(size: 12, design: .monospaced))
                                        .foregroundColor(entry.isError ? .red : .primary)

                                    Spacer(minLength: 4)

                                    if entry.count > 1 {
                                        Text("×\(entry.count)")
                                            .font(.system(size: 10, weight: .bold, design: .rounded))
                                            .foregroundColor(entry.isError ? .red : .secondary)
                                            .padding(.horizontal, 6)
                                            .padding(.vertical, 2)
                                            .background((entry.isError ? Color.red : Color.secondary).opacity(0.10))
                                            .clipShape(Capsule())
                                    }
                                }
                                .listRowBackground(entry.isError ? Color.red.opacity(0.05) : Color.clear)
                                .listRowSeparator(.hidden)
                                .id(entry.id)
                            }
                            .listStyle(.plain)
                            .onChange(of: tunnelManager.logs.count) { _ in
                                if let last = filteredLogs.last {
                                    withAnimation { proxy.scrollTo(last.id, anchor: .bottom) }
                                }
                            }
                            .onAppear {
                                if let last = filteredLogs.last {
                                    proxy.scrollTo(last.id, anchor: .bottom)
                                }
                            }
                        }
                    }
                }
            }
            .navigationTitle("Логи")
            .toolbar {
                ToolbarItemGroup(placement: .navigationBarLeading) {
                    Toggle(isOn: $showOnlyErrors) {
                        Label("Только ошибки", systemImage: "exclamationmark.triangle")
                    }
                    .toggleStyle(.button)
                    .tint(.red)
                }
                ToolbarItemGroup(placement: .navigationBarTrailing) {
                    Button {
                        tunnelManager.clearUnreadErrors()
                        UIPasteboard.general.string = filteredLogs.map {
                            $0.count > 1 ? "\($0.message) ×\($0.count)" : $0.message
                        }.joined(separator: "\n")
                    } label: {
                        Image(systemName: "doc.on.clipboard")
                    }
                    Button(role: .destructive) {
                        tunnelManager.clearLogs()
                    } label: {
                        Image(systemName: "trash")
                    }
                }
            }
        }
        .onAppear {
            tunnelManager.clearUnreadErrors()
            syncHealthActivity()
        }
        .onChange(of: tunnelManager.isRunning) { _ in
            syncHealthActivity()
        }
        .onChange(of: tunnelManager.isConnecting) { _ in
            syncHealthActivity()
        }
    }

    private func syncHealthActivity() {
        connectionHealth.setTunnelActive(tunnelManager.isRunning || tunnelManager.isConnecting)
    }

    private var liveStatsHeader: some View {
        HStack(spacing: 0) {
            LiveHeaderMetric(title: "Активных", value: "\(tunnelManager.activeConnections)")
            Divider().frame(height: 28)
            LiveHeaderMetric(
                title: "Ping",
                value: (tunnelManager.isRunning || tunnelManager.isConnecting) ? connectionHealth.pingText : "—"
            )
            Divider().frame(height: 28)
            LiveHeaderMetric(title: "↑", value: tunnelManager.uploadedMBString)
            Divider().frame(height: 28)
            LiveHeaderMetric(title: "Сессия", value: tunnelManager.isRunning ? tunnelManager.uptimeString : "00:00")
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 8)
        .background(Color(UIColor.secondarySystemGroupedBackground))
    }
}

private struct LiveHeaderMetric: View {
    let title: String
    let value: String

    var body: some View {
        VStack(spacing: 2) {
            Text(title)
                .font(.system(size: 9, weight: .medium))
                .foregroundColor(.secondary)
                .lineLimit(1)
            Text(value)
                .font(.system(size: 11, weight: .semibold, design: .rounded))
                .foregroundColor(.primary)
                .lineLimit(1)
                .minimumScaleFactor(0.75)
        }
        .frame(maxWidth: .infinity)
    }
}
