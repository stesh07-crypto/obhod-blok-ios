import Foundation

// MARK: – Swift wrapper over libwdttclient.a (Go C exports)

/// Bridges Swift ↔ Go via C FFI (libwdttclient.a compiled from go_client/)
enum GoClient {

    // MARK: – Callback storage

    private static var logHandler:   ((String, Bool) -> Void)?
    private static var statsHandler: ((String) -> Void)?

    // MARK: – Setup

    static func setLogHandler(_ handler: @escaping (String, Bool) -> Void) {
        logHandler = handler
        // Set C callback into Go
        WDTT_SetLogCallback { linePtr, isError in
            guard let ptr = linePtr else { return }
            let line = String(cString: ptr)
            GoClient.logHandler?(line, isError != 0)
        }
    }

    static func setStatsHandler(_ handler: @escaping (String) -> Void) {
        statsHandler = handler
        WDTT_SetStatsCallback { statsPtr in
            guard let ptr = statsPtr else { return }
            let s = String(cString: ptr)
            GoClient.statsHandler?(s)
        }
    }

    // MARK: – Control

    /// Returns 0 on success, -1 if already running
    @discardableResult
    static func start(
        peer: String,
        hashes: String,
        password: String,
        port: Int,
        workers: Int,
        deviceID: String,
        dns: String,
        obfsMode: String,
        vkAnonPath: String
    ) -> Int32 {
        return WDTT_Start(
            peer,
            hashes,
            password,
            Int32(port),
            Int32(workers),
            deviceID,
            dns,
            obfsMode,
            vkAnonPath
        )
    }

    static func stop() {
        WDTT_Stop()
    }

    static var isRunning: Bool {
        WDTT_IsRunning() != 0
    }
}
