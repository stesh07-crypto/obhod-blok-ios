import Foundation

// MARK: – Models

struct ParsedRemoteSubscription {
    let profiles: [ConnectionProfile]
    let subscriptionName: String?
    let description: String
    let trafficUsedMb: Double
    let trafficLimitMb: Double
    let updatedAt: String
    let version: Int
}

// MARK: – SubscriptionImport

enum SubscriptionImport {

    /// Fetch a remote subscription URL (http/https)
    static func fetch(url: URL) async throws -> ParsedRemoteSubscription {
        guard url.scheme == "http" || url.scheme == "https" else {
            throw SubscriptionError.invalidURL
        }

        var request = URLRequest(url: url, timeoutInterval: 20)
        request.setValue("ObhoD_BLOK-Subscription/1.0", forHTTPHeaderField: "User-Agent")
        request.setValue("application/json, text/plain, */*", forHTTPHeaderField: "Accept")

        let (data, response) = try await URLSession.shared.data(for: request)

        guard let http = response as? HTTPURLResponse,
              (200...299).contains(http.statusCode) else {
            throw SubscriptionError.httpError((response as? HTTPURLResponse)?.statusCode ?? -1)
        }

        let body = String(data: data, encoding: .utf8) ?? ""
        guard let parsed = parse(rawText: body) else {
            throw SubscriptionError.invalidFormat
        }
        guard !parsed.profiles.isEmpty else {
            throw SubscriptionError.emptyProfiles
        }
        return parsed
    }

    /// Parse a raw payload (JSON / Base64 / qwdtt:// URI)
    static func parse(rawText: String) -> ParsedRemoteSubscription? {
        var text = rawText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return nil }

        // Try Base64 decode if not obviously JSON or qwdtt://
        if !text.hasPrefix("[") && !text.hasPrefix("{") && !text.hasPrefix("qwdtt:") {
            if let decoded = Data(base64Encoded: text, options: .ignoreUnknownCharacters),
               let str = String(data: decoded, encoding: .utf8) {
                text = str.trimmingCharacters(in: .whitespacesAndNewlines)
            }
        }

        // Single qwdtt://config URI
        if text.hasPrefix("qwdtt://config") || text.hasPrefix("qwdtt:config") {
            let fixed = text.replacingOccurrences(of: "qwdtt:config", with: "qwdtt://config")
            if let profile = parseQwdttURI(fixed) {
                return ParsedRemoteSubscription(
                    profiles: [profile],
                    subscriptionName: profile.name,
                    description: "",
                    trafficUsedMb: 0, trafficLimitMb: 0,
                    updatedAt: "", version: 0
                )
            }
            return nil
        }

        // JSON object { profiles: [...], ... }
        if text.hasPrefix("{"),
           let data = text.data(using: .utf8),
           let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {

            let name = obj["subscriptionName"] as? String ?? obj["groupName"] as? String
            let desc = obj["description"] as? String ?? obj["info"] as? String ?? ""
            let used = obj["trafficUsedMb"] as? Double ?? obj["trafficMb"] as? Double ?? 0
            let limit = obj["trafficLimitMb"] as? Double ?? obj["trafficLimit"] as? Double ?? 0
            let upd = obj["updatedAt"] as? String ?? obj["updated"] as? String ?? ""
            let ver = obj["version"] as? Int ?? 0

            if let arr = obj["profiles"] as? [[String: Any]] ?? obj["servers"] as? [[String: Any]] {
                let profiles = arr.compactMap { parseProfileDict($0) }
                if !profiles.isEmpty {
                    return ParsedRemoteSubscription(
                        profiles: profiles, subscriptionName: name,
                        description: desc, trafficUsedMb: used,
                        trafficLimitMb: limit, updatedAt: upd, version: ver
                    )
                }
            }
        }

        // JSON array [...]
        if text.hasPrefix("["),
           let data = text.data(using: .utf8),
           let arr = try? JSONSerialization.jsonObject(with: data) as? [[String: Any]] {

            let groupName = arr.first?["groupName"] as? String
            let profiles = arr.compactMap { parseProfileDict($0) }
            if !profiles.isEmpty {
                return ParsedRemoteSubscription(
                    profiles: profiles, subscriptionName: groupName,
                    description: "", trafficUsedMb: 0,
                    trafficLimitMb: 0, updatedAt: "", version: 0
                )
            }
        }

        return nil
    }

    // MARK: – Private

    private static func parseProfileDict(_ d: [String: Any]) -> ConnectionProfile? {
        guard let peer = d["peer"] as? String, !peer.isEmpty else { return nil }
        let name    = d["name"] as? String ?? "Imported"
        let hashes  = d["hashes"] as? String ?? d["vkHashes"] as? String ?? ""
        let workers = d["workers"] as? Int ?? d["workersPerHash"] as? Int ?? 16
        let port    = d["port"] as? Int ?? d["listenPort"] as? Int ?? 9000
        let pass    = d["password"] as? String ?? d["pass"] as? String ?? ""
        let traffic = d["trafficMb"] as? Double ?? d["trafficUsedMb"] as? Double ?? 0

        return ConnectionProfile(
            id: UUID().uuidString,
            name: name, peer: peer, vkHashes: hashes,
            workersPerHash: workers, listenPort: port,
            password: pass, trafficMb: max(0, traffic),
            useGlobalHashes: hashes.isEmpty
        )
    }

    private static func parseQwdttURI(_ uriString: String) -> ConnectionProfile? {
        guard let url = URL(string: uriString),
              let comps = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return nil }

        let queryItem: (String) -> String? = { key in
            comps.queryItems?.first(where: { $0.name == key })?.value
        }

        guard let peer = queryItem("peer"), !peer.isEmpty else { return nil }

        return ConnectionProfile(
            id: UUID().uuidString,
            name: queryItem("name") ?? "QR Профиль",
            peer: peer,
            vkHashes: queryItem("hashes") ?? "",
            workersPerHash: Int(queryItem("workers") ?? "18") ?? 18,
            listenPort: Int(queryItem("port") ?? "9000") ?? 9000,
            password: queryItem("pass") ?? queryItem("password") ?? "",
            useGlobalHashes: (queryItem("hashes") ?? "").isEmpty
        )
    }
}

// MARK: – Errors

enum SubscriptionError: LocalizedError {
    case invalidURL
    case httpError(Int)
    case invalidFormat
    case emptyProfiles

    var errorDescription: String? {
        switch self {
        case .invalidURL:      return "Неверный URL подписки"
        case .httpError(let c): return "Ошибка сервера: HTTP \(c)"
        case .invalidFormat:   return "Неверный формат подписки"
        case .emptyProfiles:   return "В подписке нет профилей"
        }
    }
}
