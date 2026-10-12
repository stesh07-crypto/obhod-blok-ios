import Foundation
#if canImport(ActivityKit)
import ActivityKit

@available(iOS 16.1, *)
public struct OBhoDActivityAttributes: ActivityAttributes {
    public struct ContentState: Codable, Hashable {
        public var statusText: String
        public var isConnected: Bool
        public var connectedSince: Date?

        public init(statusText: String, isConnected: Bool, connectedSince: Date?) {
            self.statusText = statusText
            self.isConnected = isConnected
            self.connectedSince = connectedSince
        }
    }

    public var appName: String

    public init(appName: String = "OBhoD") {
        self.appName = appName
    }
}
#endif
