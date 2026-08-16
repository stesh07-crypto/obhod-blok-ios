import SwiftUI

struct LogsView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
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
        .onAppear { tunnelManager.clearUnreadErrors() }
    }

    private var liveStatsHeader: some View {
        HStack(spacing: 12) {
            Label("\(tunnelManager.activeConnections)", systemImage: "point.3.connected.trianglepath.dotted")
                .foregroundColor(tunnelManager.activeConnections > 0 ? .green : .secondary)

            Spacer()

            Text("↓ \(tunnelManager.downloadedMBString)")
                .foregroundColor(.teal)

            Text("↑ \(tunnelManager.uploadedMBString)")
                .foregroundColor(.indigo)
        }
        .font(.system(size: 12, weight: .semibold, design: .rounded))
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background(Color(UIColor.secondarySystemGroupedBackground))
    }
}
