import Foundation
import ActivityKit

@available(iOS 16.1, *)
@MainActor
final class LiveActivityManager {
    static let shared = LiveActivityManager()

    private let defaults = AppGroup.sharedDefaults ?? UserDefaults.standard
    private var activity: Activity<OBhoDLiveActivityAttributes>?
    private var lastState: OBhoDLiveActivityAttributes.ContentState?
    private var lastUpdateAt = Date.distantPast

    private init() {
        activity = Activity<OBhoDLiveActivityAttributes>.activities.first
    }

    func sync(
        isRunning: Bool,
        activeConnections: Int,
        connectedSince: Date?,
        isRecovering: Bool
    ) {
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return }

        guard isRunning, let connectedSince else {
            endIfNeeded()
            return
        }

        let state = makeState(
            activeConnections: activeConnections,
            isRecovering: isRecovering
        )

        if activity == nil {
            start(connectedSince: connectedSince, state: state)
            return
        }

        updateIfNeeded(state)
    }

    private func makeState(
        activeConnections: Int,
        isRecovering: Bool
    ) -> OBhoDLiveActivityAttributes.ContentState {
        let pingValue: Int?
        if defaults.object(forKey: AppGroup.Keys.lastPingMilliseconds) != nil {
            pingValue = defaults.integer(forKey: AppGroup.Keys.lastPingMilliseconds)
        } else {
            pingValue = nil
        }

        let network = defaults.string(forKey: AppGroup.Keys.physicalNetworkLabel) ?? "—"

        return OBhoDLiveActivityAttributes.ContentState(
            activeConnections: max(0, activeConnections),
            pingMilliseconds: pingValue,
            network: network,
            isRecovering: isRecovering
        )
    }

    private func start(
        connectedSince: Date,
        state: OBhoDLiveActivityAttributes.ContentState
    ) {
        let attributes = OBhoDLiveActivityAttributes(connectedSince: connectedSince)

        do {
            if #available(iOS 16.2, *) {
                activity = try Activity.request(
                    attributes: attributes,
                    content: ActivityContent(state: state, staleDate: nil),
                    pushType: nil
                )
            } else {
                activity = try Activity.request(
                    attributes: attributes,
                    contentState: state,
                    pushType: nil
                )
            }
            lastState = state
            lastUpdateAt = Date()
        } catch {
            // Live Activities may be disabled by the user or unavailable on the device.
            // VPN operation must never depend on ActivityKit.
        }
    }

    private func updateIfNeeded(_ state: OBhoDLiveActivityAttributes.ContentState) {
        guard let activity else { return }
        guard state != lastState else { return }

        let recoveryChanged = state.isRecovering != lastState?.isRecovering
        guard recoveryChanged || Date().timeIntervalSince(lastUpdateAt) >= 4 else { return }

        lastState = state
        lastUpdateAt = Date()

        Task {
            if #available(iOS 16.2, *) {
                await activity.update(ActivityContent(state: state, staleDate: nil))
            } else {
                await activity.update(using: state)
            }
        }
    }

    private func endIfNeeded() {
        let activities = Activity<OBhoDLiveActivityAttributes>.activities
        guard !activities.isEmpty else {
            activity = nil
            lastState = nil
            return
        }

        let finalState = OBhoDLiveActivityAttributes.ContentState(
            activeConnections: 0,
            pingMilliseconds: nil,
            network: defaults.string(forKey: AppGroup.Keys.physicalNetworkLabel) ?? "—",
            isRecovering: false
        )

        activity = nil
        lastState = nil
        lastUpdateAt = Date.distantPast

        Task {
            for item in activities {
                if #available(iOS 16.2, *) {
                    await item.end(
                        ActivityContent(state: finalState, staleDate: nil),
                        dismissalPolicy: .immediate
                    )
                } else {
                    await item.end(using: finalState, dismissalPolicy: .immediate)
                }
            }
        }
    }
}
