import SwiftUI

struct LogsView: View {
    @EnvironmentObject var tunnelManager: TunnelManager
    @State private var showOnlyErrors = false
    @State private var scrollProxy: ScrollViewProxy? = nil

    var filteredLogs: [LogEntry] {
        showOnlyErrors ? tunnelManager.logs.filter { $0.isError } : tunnelManager.logs
    }

    var body: some View {
        NavigationView {
            Group {
                if filteredLogs.isEmpty {
                    VStack(spacing: 12) {
                        Image(systemName: "text.alignleft")
                            .font(.system(size: 48))
                            .foregroundColor(.secondary.opacity(0.5))
                        Text(tunnelManager.logs.isEmpty ? "Логи появятся при подключении" : "Нет ошибок")
                            .foregroundColor(.secondary)
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
                        UIPasteboard.general.string = filteredLogs.map { $0.message }.joined(separator: "\n")
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
}
