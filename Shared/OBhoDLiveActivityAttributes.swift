import Foundation
import ActivityKit

@available(iOS 16.1, *)
struct OBhoDLiveActivityAttributes: ActivityAttributes {
    struct ContentState: Codable, Hashable {
        let activeConnections: Int
        let pingMilliseconds: Int?
        let network: String
        let isRecovering: Bool
    }

    let connectedSince: Date
}
