import Foundation
import UIKit
#if canImport(ActivityKit)
import ActivityKit

@available(iOS 16.1, *)
@MainActor
final class OBhoDLiveActivityManager {
    static let shared = OBhoDLiveActivityManager()

    private var currentActivity: Activity<OBhoDActivityAttributes>?

    private init() {
        // Adopt ONLY ongoing activities that are strictly active in the system
        currentActivity = Activity<OBhoDActivityAttributes>.activities.first(where: { $0.activityState == .active })
    }

    func sync(isConnected: Bool, statusText: String, connectedSince: Date?, downloadSpeed: String = "") {
        guard ActivityAuthorizationInfo().areActivitiesEnabled else {
            print("[LiveActivity] Activities are disabled in system settings")
            return
        }

        let state = OBhoDActivityAttributes.ContentState(
            statusText: statusText,
            isConnected: isConnected,
            connectedSince: connectedSince,
            downloadSpeed: downloadSpeed
        )

        // 1. If current activity exists and is genuinely active, update it
        if let activity = currentActivity, activity.activityState == .active {
            Task {
                if #available(iOS 16.2, *) {
                    await activity.update(ActivityContent(state: state, staleDate: nil))
                } else {
                    await activity.update(using: state)
                }
            }
            return
        }

        // 2. If currentActivity is dead or nil, look for any active activity in the system
        if let existingActive = Activity<OBhoDActivityAttributes>.activities.first(where: { $0.activityState == .active }) {
            currentActivity = existingActive
            Task {
                if #available(iOS 16.2, *) {
                    await existingActive.update(ActivityContent(state: state, staleDate: nil))
                } else {
                    await existingActive.update(using: state)
                }
            }
            return
        }

        // 3. Current activity is nil or ended. We need to create a new one!
        currentActivity = nil

        // Only start if connected or connecting
        guard isConnected || statusText.contains("Подключ") else { return }

        // Clean up any stale ended/dismissed activities in background
        for old in Activity<OBhoDActivityAttributes>.activities where old.activityState != .active {
            Task {
                await old.end(nil, dismissalPolicy: .immediate)
            }
        }

        do {
            let attrs = OBhoDActivityAttributes(appName: "OBhoD")
            if #available(iOS 16.2, *) {
                let activity = try Activity.request(
                    attributes: attrs,
                    content: ActivityContent(state: state, staleDate: nil),
                    pushType: nil
                )
                currentActivity = activity
            } else {
                let activity = try Activity.request(
                    attributes: attrs,
                    contentState: state,
                    pushType: nil
                )
                currentActivity = activity
            }
            print("[LiveActivity] Successfully requested activity ID: \(currentActivity?.id ?? "unknown")")
        } catch {
            print("[LiveActivity] Request error: \(error.localizedDescription)")
        }
    }

    func end() {
        currentActivity = nil
        let all = Activity<OBhoDActivityAttributes>.activities
        guard !all.isEmpty else { return }

        Task {
            let endState = OBhoDActivityAttributes.ContentState(
                statusText: "Отключено",
                isConnected: false,
                connectedSince: nil,
                downloadSpeed: ""
            )
            for act in all {
                if #available(iOS 16.2, *) {
                    await act.end(ActivityContent(state: endState, staleDate: nil), dismissalPolicy: .immediate)
                } else {
                    await act.end(using: endState, dismissalPolicy: .immediate)
                }
            }
        }
    }
}
#endif

/// Safe bridge callable from anywhere regardless of iOS version
enum LiveActivityBridge {
    static func sync(isConnected: Bool, statusText: String, connectedSince: Date?, downloadSpeed: String = "") {
        #if canImport(ActivityKit)
        if #available(iOS 16.1, *) {
            Task { @MainActor in
                OBhoDLiveActivityManager.shared.sync(
                    isConnected: isConnected,
                    statusText: statusText,
                    connectedSince: connectedSince,
                    downloadSpeed: downloadSpeed
                )
            }
        }
        #endif
    }

    static func end() {
        #if canImport(ActivityKit)
        if #available(iOS 16.1, *) {
            Task { @MainActor in
                OBhoDLiveActivityManager.shared.end()
            }
        }
        #endif
    }
}
