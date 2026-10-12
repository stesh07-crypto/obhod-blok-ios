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
        // Adopt any ongoing activity if app restarted
        currentActivity = Activity<OBhoDActivityAttributes>.activities.first
    }

    func sync(isConnected: Bool, statusText: String, connectedSince: Date?, downloadSpeed: String = "") {
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return }

        let state = OBhoDActivityAttributes.ContentState(
            statusText: statusText,
            isConnected: isConnected,
            connectedSince: connectedSince,
            downloadSpeed: downloadSpeed
        )

        if let activity = currentActivity {
            Task {
                if #available(iOS 16.2, *) {
                    await activity.update(ActivityContent(state: state, staleDate: nil))
                } else {
                    await activity.update(using: state)
                }
            }
        } else {
            // Only start if connected or connecting
            guard isConnected || statusText.contains("Подключ") else { return }
            guard UIApplication.shared.applicationState != .background else { return }

            do {
                let attrs = OBhoDActivityAttributes(appName: "OBhoD")
                if #available(iOS 16.2, *) {
                    currentActivity = try Activity.request(
                        attributes: attrs,
                        content: ActivityContent(state: state, staleDate: nil),
                        pushType: nil
                    )
                } else {
                    currentActivity = try Activity.request(
                        attributes: attrs,
                        contentState: state,
                        pushType: nil
                    )
                }
            } catch {
                print("[LiveActivity] Request error: \(error.localizedDescription)")
            }
        }
    }

    func end() {
        guard let activity = currentActivity else { return }
        currentActivity = nil
        Task {
            let endState = OBhoDActivityAttributes.ContentState(
                statusText: "Отключено",
                isConnected: false,
                connectedSince: nil,
                downloadSpeed: ""
            )
            if #available(iOS 16.2, *) {
                await activity.end(ActivityContent(state: endState, staleDate: nil), dismissalPolicy: .immediate)
            } else {
                await activity.end(using: endState, dismissalPolicy: .immediate)
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
