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
    
    private static var packetHandler: ((Data) -> Void)?

    static func setPacketHandler(_ handler: @escaping (Data) -> Void) {
        packetHandler = handler
        let callback: @convention(c) (UnsafeRawPointer?, Int32) -> Void = { dataPtr, length in
            guard let ptr = dataPtr, length > 0 else { return }
            let data = Data(bytes: ptr, count: Int(length))
            GoClient.packetHandler?(data)
        }
        WDTT_SetWriteCallback(callback)
    }

    static func writePacket(_ data: Data) {
        data.withUnsafeBytes { rawBuffer in
            if let baseAddress = rawBuffer.baseAddress {
                WDTT_WritePacket(UnsafeMutableRawPointer(mutating: baseAddress), Int32(data.count))
            }
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
        let pPeer = strdup(peer)
        let pHashes = strdup(hashes)
        let pPassword = strdup(password)
        let pDeviceID = strdup(deviceID)
        let pDns = strdup(dns)
        let pObfsMode = strdup(obfsMode)
        let pVkAnonPath = strdup(vkAnonPath)

        defer {
            free(pPeer)
            free(pHashes)
            free(pPassword)
            free(pDeviceID)
            free(pDns)
            free(pObfsMode)
            free(pVkAnonPath)
        }

        return WDTT_Start(
            pPeer,
            pHashes,
            pPassword,
            Int32(port),
            Int32(workers),
            pDeviceID,
            pDns,
            pObfsMode,
            pVkAnonPath
        )
    }

    static func stop() {
        WDTT_Stop()
    }

    static var isRunning: Bool {
        WDTT_IsRunning() != 0
    }
}
