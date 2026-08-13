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
    let expiresAt: Int64
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
        guard let parsed = parse(rawText: body, sourceUrl: url.absoluteString) else {
            throw SubscriptionError.invalidFormat
        }
        guard !parsed.profiles.isEmpty else {
            throw SubscriptionError.emptyProfiles
        }
        return parsed
    }

    /// Parse a raw payload (JSON / Base64 / qwdtt:// URI)
    static func parse(rawText: String, sourceUrl: String = "") -> ParsedRemoteSubscription? {
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
            if let profile = parseQwdttURI(fixed, sourceUrl: sourceUrl) {
                return ParsedRemoteSubscription(
                    profiles: [profile],
                    subscriptionName: profile.name,
                    description: "",
                    trafficUsedMb: 0,
                    trafficLimitMb: 0,
                    updatedAt: "",
                    version: 0,
                    expiresAt: profile.expiresAt
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
            let exp = parseUnixExpires(from: obj)

            if let arr = obj["profiles"] as? [[String: Any]] ?? obj["servers"] as? [[String: Any]] {
                let profiles = arr.compactMap { parseProfileDict($0, fallbackExpires: exp, sourceUrl: sourceUrl) }
                if !profiles.isEmpty {
                    return ParsedRemoteSubscription(
                        profiles: profiles,
                        subscriptionName: name,
                        description: desc,
                        trafficUsedMb: used,
                        trafficLimitMb: limit,
                        updatedAt: upd,
                        version: ver,
                        expiresAt: exp
                    )
                }
            }
        }

        // JSON array [...]
        if text.hasPrefix("["),
           let data = text.data(using: .utf8),
           let arr = try? JSONSerialization.jsonObject(with: data) as? [[String: Any]] {

            let groupName = arr.first?["groupName"] as? String
            let profiles = arr.compactMap { parseProfileDict($0, fallbackExpires: 0, sourceUrl: sourceUrl) }
            if !profiles.isEmpty {
                return ParsedRemoteSubscription(
                    profiles: profiles,
                    subscriptionName: groupName,
                    description: "",
                    trafficUsedMb: 0,
                    trafficLimitMb: 0,
                    updatedAt: "",
                    version: 0,
                    expiresAt: profiles.first?.expiresAt ?? 0
                )
            }
        }

        return nil
    }

    // MARK: – Expiration & Date Helpers

    static func parseUnixExpires(from dict: [String: Any]) -> Int64 {
        if let val = dict["expires_at"] as? Int64 ?? dict["expiresAt"] as? Int64 ?? dict["expire"] as? Int64 {
            return val
        }
        if let val = dict["expires_at"] as? Int ?? dict["expiresAt"] as? Int ?? dict["expire"] as? Int {
            return Int64(val)
        }
        if let val = dict["expires_at"] as? Double ?? dict["expiresAt"] as? Double ?? dict["expire"] as? Double {
            return Int64(val)
        }
        if let str = dict["expires_at"] as? String ?? dict["expiresAt"] as? String ?? dict["expire"] as? String ?? dict["expireDate"] as? String {
            if let num = Int64(str) { return num }
            return parseDateStringToUnix(str)
        }
        return 0
    }

    static func parseDateStringToUnix(_ rawDate: String) -> Int64 {
        let trimmed = rawDate.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return 0 }

        let formatters: [String] = [
            "yyyy-MM-dd",
            "dd.MM.yyyy",
            "yyyy/MM/dd",
            "dd-MM-yyyy",
            "yyyy-MM-dd'T'HH:mm:ssZ",
            "yyyy-MM-dd'T'HH:mm:ss.SSSZ"
        ]

        let df = DateFormatter()
        df.locale = Locale(identifier: "en_US_POSIX")
        for fmt in formatters {
            df.dateFormat = fmt
            if let date = df.date(from: trimmed) {
                return Int64(date.timeIntervalSince1970)
            }
        }
        return 0
    }

    static func getEndOfDayTimestamp(unixExpiresAt: Int64) -> Int64 {
        guard unixExpiresAt > 0 else { return 0 }
        let rawSec = unixExpiresAt > 100_000_000_000 ? unixExpiresAt / 1000 : unixExpiresAt
        var cal = Calendar.current
        cal.timeZone = TimeZone.current
        let date = Date(timeIntervalSince1970: TimeInterval(rawSec))
        var comps = cal.dateComponents([.year, .month, .day], from: date)
        comps.hour = 23
        comps.minute = 59
        comps.second = 59
        return Int64(cal.date(from: comps)?.timeIntervalSince1970 ?? TimeInterval(rawSec))
    }

    static func formatRemainingDaysBadge(expiresAt: Int64) -> String {
        guard expiresAt > 0 else {
            return "📅 Бессрочно"
        }
        let endOfDaySec = getEndOfDayTimestamp(unixExpiresAt: expiresAt)
        let nowSec = Int64(Date().timeIntervalSince1970)
        let diffSec = endOfDaySec - nowSec

        if diffSec <= 0 {
            return "⏰ Истёк"
        }
        let days = Int(diffSec / 86400)
        if days == 0 {
            return "⏳ Истекает сегодня"
        } else if days == 1 {
            return "⏳ Истекает завтра"
        } else {
            return "⏳ Истекает через \(days) дн."
        }
    }

    static func isExpired(expiresAt: Int64) -> Bool {
        guard expiresAt > 0 else { return false }
        let endOfDaySec = getEndOfDayTimestamp(unixExpiresAt: expiresAt)
        return Int64(Date().timeIntervalSince1970) > endOfDaySec
    }

    // MARK: – Private

    private static func parseProfileDict(_ d: [String: Any], fallbackExpires: Int64, sourceUrl: String) -> ConnectionProfile? {
        guard let peer = d["peer"] as? String, !peer.isEmpty else { return nil }
        let name    = d["name"] as? String ?? "Imported"
        let hashes  = d["hashes"] as? String ?? d["vkHashes"] as? String ?? ""
        let workers = d["workers"] as? Int ?? d["workersPerHash"] as? Int ?? 16
        let port    = d["port"] as? Int ?? d["listenPort"] as? Int ?? 9000
        let pass    = d["password"] as? String ?? d["pass"] as? String ?? ""
        let traffic = d["trafficMb"] as? Double ?? d["trafficUsedMb"] as? Double ?? 0
        let exp     = parseUnixExpires(from: d)

        return ConnectionProfile(
            id: UUID().uuidString,
            name: name,
            peer: peer,
            vkHashes: hashes,
            workersPerHash: workers,
            listenPort: port,
            password: pass,
            trafficMb: max(0, traffic),
            groupId: d["groupId"] as? String ?? "",
            useGlobalHashes: hashes.isEmpty,
            expiresAt: exp > 0 ? exp : fallbackExpires,
            subscriptionUrl: sourceUrl
        )
    }

    private static func parseQwdttURI(_ uriString: String, sourceUrl: String) -> ConnectionProfile? {
        guard let url = URL(string: uriString),
              let comps = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return nil }

        let queryItem: (String) -> String? = { key in
            comps.queryItems?.first(where: { $0.name == key })?.value
        }

        guard let peer = queryItem("peer"), !peer.isEmpty else { return nil }

        var exp: Int64 = 0
        if let expStr = queryItem("expires_at") ?? queryItem("expiresAt") ?? queryItem("expire") {
            exp = Int64(expStr) ?? parseDateStringToUnix(expStr)
        }

        return ConnectionProfile(
            id: UUID().uuidString,
            name: queryItem("name") ?? "QR Профиль",
            peer: peer,
            vkHashes: queryItem("hashes") ?? "",
            workersPerHash: Int(queryItem("workers") ?? "18") ?? 18,
            listenPort: Int(queryItem("port") ?? "9000") ?? 9000,
            password: queryItem("pass") ?? queryItem("password") ?? "",
            groupId: queryItem("group") ?? "",
            useGlobalHashes: (queryItem("hashes") ?? "").isEmpty,
            expiresAt: exp,
            subscriptionUrl: sourceUrl
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

// MARK: – Notification Names

extension Notification.Name {
    static let beginImportFromURL = Notification.Name("beginImportFromURL")
}

