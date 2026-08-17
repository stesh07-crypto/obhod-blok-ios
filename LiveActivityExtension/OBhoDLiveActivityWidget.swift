import ActivityKit
import WidgetKit
import SwiftUI

@available(iOSApplicationExtension 16.1, *)
struct OBhoDLiveActivityWidget: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: OBhoDLiveActivityAttributes.self) { context in
            lockScreenView(context)
                .activityBackgroundTint(Color.black.opacity(0.88))
                .activitySystemActionForegroundColor(.white)
        } dynamicIsland: { context in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    Label {
                        Text(context.state.isRecovering ? "Переподключение" : "OBhoD")
                            .font(.caption.bold())
                    } icon: {
                        Image(systemName: context.state.isRecovering
                              ? "arrow.triangle.2.circlepath"
                              : "shield.checkered")
                    }
                }

                DynamicIslandExpandedRegion(.trailing) {
                    VStack(alignment: .trailing, spacing: 1) {
                        Text("PING")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                        Text(pingText(context.state.pingMilliseconds))
                            .font(.caption.bold())
                    }
                }

                DynamicIslandExpandedRegion(.bottom) {
                    HStack(spacing: 14) {
                        metric(
                            icon: "point.3.connected.trianglepath.dotted",
                            title: "Активных",
                            value: "\(context.state.activeConnections)"
                        )
                        metric(
                            icon: "timer",
                            title: "Сессия",
                            timerDate: context.attributes.connectedSince
                        )
                        metric(
                            icon: "network",
                            title: "Сеть",
                            value: context.state.network
                        )
                    }
                    .padding(.top, 2)
                }
            } compactLeading: {
                Image(systemName: context.state.isRecovering
                      ? "arrow.triangle.2.circlepath"
                      : "shield.fill")
                    .foregroundStyle(context.state.isRecovering ? .orange : .green)
            } compactTrailing: {
                Text("\(context.state.activeConnections)")
                    .font(.caption2.bold())
                    .monospacedDigit()
            } minimal: {
                Image(systemName: context.state.isRecovering
                      ? "arrow.triangle.2.circlepath"
                      : "shield.fill")
                    .foregroundStyle(context.state.isRecovering ? .orange : .green)
            }
            .keylineTint(context.state.isRecovering ? .orange : .green)
        }
    }

    @ViewBuilder
    private func lockScreenView(
        _ context: ActivityViewContext<OBhoDLiveActivityAttributes>
    ) -> some View {
        VStack(spacing: 12) {
            HStack {
                HStack(spacing: 8) {
                    Image(systemName: context.state.isRecovering
                          ? "arrow.triangle.2.circlepath"
                          : "shield.checkered")
                        .font(.title3.bold())
                        .foregroundStyle(context.state.isRecovering ? .orange : .green)

                    VStack(alignment: .leading, spacing: 1) {
                        Text("OBhoD")
                            .font(.headline)
                        Text(context.state.isRecovering ? "Переподключение" : "VPN активен")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }

                Spacer()

                Text(context.state.network)
                    .font(.caption.bold())
                    .lineLimit(1)
            }

            HStack(spacing: 10) {
                liveMetric(
                    title: "Активных",
                    value: "\(context.state.activeConnections)"
                )
                liveMetric(
                    title: "Ping",
                    value: pingText(context.state.pingMilliseconds)
                )

                VStack(alignment: .leading, spacing: 2) {
                    Text("Сессия")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                    Text(context.attributes.connectedSince, style: .timer)
                        .font(.callout.bold())
                        .monospacedDigit()
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .padding(14)
    }

    @ViewBuilder
    private func liveMetric(title: String, value: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
                .font(.caption2)
                .foregroundStyle(.secondary)
            Text(value)
                .font(.callout.bold())
                .monospacedDigit()
                .lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder
    private func metric(
        icon: String,
        title: String,
        value: String? = nil,
        timerDate: Date? = nil
    ) -> some View {
        HStack(spacing: 5) {
            Image(systemName: icon)
                .font(.caption2)
                .foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 0) {
                Text(title)
                    .font(.system(size: 9))
                    .foregroundStyle(.secondary)
                if let timerDate {
                    Text(timerDate, style: .timer)
                        .font(.caption2.bold())
                        .monospacedDigit()
                } else {
                    Text(value ?? "—")
                        .font(.caption2.bold())
                        .lineLimit(1)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func pingText(_ ping: Int?) -> String {
        guard let ping, ping > 0 else { return "—" }
        return "\(ping) мс"
    }
}

@available(iOSApplicationExtension 16.1, *)
@main
struct OBhoDLiveActivityBundle: WidgetBundle {
    var body: some Widget {
        OBhoDLiveActivityWidget()
    }
}
