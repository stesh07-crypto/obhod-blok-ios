import WidgetKit
import SwiftUI

// MARK: - Shared App Group Keys
private let kAppGroupID = "group.net.qwdtt.client.ios"
private let kStatusKey  = "tunnel_status"         // "connected" | "disconnected" | "connecting"
private let kUpdatedKey = "tunnel_status_updated"  // TimeInterval

// MARK: - Timeline Entry

struct OBhoDEntry: TimelineEntry {
    let date: Date
    let status: TunnelStatus
    let updatedAt: Date?
}

enum TunnelStatus: String {
    case connected    = "connected"
    case connecting   = "connecting"
    case disconnected = "disconnected"

    var label: String {
        switch self {
        case .connected:    return "Подключено"
        case .connecting:   return "Подключение…"
        case .disconnected: return "Отключено"
        }
    }

    var icon: String {
        switch self {
        case .connected:    return "shield.fill"
        case .connecting:   return "shield.lefthalf.filled"
        case .disconnected: return "shield.slash"
        }
    }

    var color: Color {
        switch self {
        case .connected:    return Color(red: 0.20, green: 0.85, blue: 0.45)
        case .connecting:   return Color(red: 1.0,  green: 0.75, blue: 0.0)
        case .disconnected: return Color(red: 0.55, green: 0.55, blue: 0.60)
        }
    }
}

// MARK: - Timeline Provider

struct OBhoDProvider: TimelineProvider {
    private var defaults: UserDefaults? {
        UserDefaults(suiteName: kAppGroupID)
    }

    func placeholder(in context: Context) -> OBhoDEntry {
        OBhoDEntry(date: .now, status: .disconnected, updatedAt: nil)
    }

    func getSnapshot(in context: Context, completion: @escaping (OBhoDEntry) -> Void) {
        completion(currentEntry())
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<OBhoDEntry>) -> Void) {
        let entry = currentEntry()
        let next = Calendar.current.date(byAdding: .minute, value: 5, to: entry.date) ?? .now
        completion(Timeline(entries: [entry], policy: .after(next)))
    }

    private func currentEntry() -> OBhoDEntry {
        let raw    = defaults?.string(forKey: kStatusKey) ?? "disconnected"
        let status = TunnelStatus(rawValue: raw) ?? .disconnected
        let ts     = defaults?.double(forKey: kUpdatedKey) ?? 0
        let updated = ts > 0 ? Date(timeIntervalSince1970: ts) : nil
        return OBhoDEntry(date: .now, status: status, updatedAt: updated)
    }
}

// MARK: - Widget Views

struct OBhoDSmallView: View {
    let entry: OBhoDEntry
    var body: some View {
        ZStack {
            LinearGradient(
                colors: [
                    Color(red: 0.06, green: 0.12, blue: 0.26),
                    Color(red: 0.03, green: 0.07, blue: 0.16)
                ],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )

            VStack(spacing: 6) {
                Image(systemName: entry.status.icon)
                    .font(.system(size: 32, weight: .semibold))
                    .foregroundStyle(entry.status.color)
                Text("OBhoD")
                    .font(.system(size: 13, weight: .bold, design: .rounded))
                    .foregroundStyle(.white)
                Text(entry.status.label)
                    .font(.system(size: 11, weight: .semibold))
                    .foregroundStyle(entry.status.color)
            }
            .padding(8)
        }
    }
}

struct OBhoDCircularView: View {
    let entry: OBhoDEntry
    var body: some View {
        ZStack {
            AccessoryWidgetBackground()
            Image(systemName: entry.status.icon)
                .font(.system(size: 18, weight: .semibold))
                .foregroundStyle(entry.status.color)
        }
    }
}

struct OBhoDRectangularView: View {
    let entry: OBhoDEntry
    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: entry.status.icon)
                .font(.system(size: 16, weight: .semibold))
                .foregroundStyle(entry.status.color)
            VStack(alignment: .leading, spacing: 1) {
                Text("OBhoD")
                    .font(.system(size: 12, weight: .bold))
                Text(entry.status.label)
                    .font(.system(size: 11))
                    .foregroundStyle(entry.status.color)
            }
        }
    }
}

struct OBhoDInlineView: View {
    let entry: OBhoDEntry
    var body: some View {
        Label(entry.status.label, systemImage: entry.status.icon)
    }
}

struct OBhoDWidgetEntryView: View {
    @Environment(\.widgetFamily) var family
    let entry: OBhoDEntry

    var body: some View {
        entryContent
            .applyWidgetContainerBackground()
    }

    @ViewBuilder
    private var entryContent: some View {
        switch family {
        case .systemSmall:          OBhoDSmallView(entry: entry)
        case .accessoryCircular:    OBhoDCircularView(entry: entry)
        case .accessoryRectangular: OBhoDRectangularView(entry: entry)
        case .accessoryInline:      OBhoDInlineView(entry: entry)
        default:                    OBhoDSmallView(entry: entry)
        }
    }
}

// MARK: - Compatibility Background Modifier (iOS 16 & iOS 17+)

extension View {
    @ViewBuilder
    func applyWidgetContainerBackground() -> some View {
        if #available(iOS 17.0, *) {
            self.containerBackground(Color(red: 0.05, green: 0.10, blue: 0.22), for: .widget)
        } else {
            self.background(Color(red: 0.05, green: 0.10, blue: 0.22))
        }
    }
}

// MARK: - Widget Declaration

struct OBhoDWidget: Widget {
    let kind = "OBhoDWidget"
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: kind, provider: OBhoDProvider()) { entry in
            OBhoDWidgetEntryView(entry: entry)
        }
        .configurationDisplayName("OBhoD")
        .description("Статус туннеля OBhoD")
        .supportedFamilies([
            .systemSmall,
            .accessoryCircular,
            .accessoryRectangular,
            .accessoryInline
        ])
    }
}

// MARK: - Widget Bundle (Live Activity + Regular Widgets)

@main
struct OBhoDWidgetBundle: WidgetBundle {
    var body: some Widget {
        OBhoDWidget()
        if #available(iOS 16.1, *) {
            OBhoDLiveActivity()
        }
    }
}
