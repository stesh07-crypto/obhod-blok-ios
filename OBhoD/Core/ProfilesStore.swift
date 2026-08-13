import Foundation
import SwiftUI
import Network

// MARK: – Data models (mirrors Android ConnectionProfile)

struct ConnectionProfile: Identifiable, Codable, Equatable {
    var id: String             = UUID().uuidString
    var name: String           = "Профиль"
    var peer: String           = ""          // IP:port of wdtt server
    var vkHashes: String       = ""          // comma-separated VK call hashes
    var workersPerHash: Int    = 16
    var listenPort: Int        = 9000
    var password: String       = ""
    var trafficMb: Double      = 0
    var groupId: String        = ""
    var useGlobalHashes: Bool  = false
    var expiresAt: Int64       = 0           // Unix timestamp (seconds)
    var subscriptionUrl: String = ""
    var pingMs: Int?           = nil

    var isExpired: Bool {
        SubscriptionImport.isExpired(expiresAt: expiresAt)
    }

    var expirationBadge: String {
        SubscriptionImport.formatRemainingDaysBadge(expiresAt: expiresAt)
    }
}

struct ProfileGroup: Identifiable, Codable, Equatable {
    var id: String   = UUID().uuidString
    var name: String = "Группа"
}

struct ProfileSubscription: Identifiable, Codable, Equatable {
    var id: String   = UUID().uuidString
    var name: String = "Подписка"
    var url: String  = ""
    var description: String = ""
    var lastSyncAt: Int64 = 0
    var expiresAt: Int64 = 0
}

// MARK: – ProfilesStore

@MainActor
final class ProfilesStore: ObservableObject {
    static let shared = ProfilesStore()

    @Published private(set) var profiles: [ConnectionProfile] = []
    @Published private(set) var groups: [ProfileGroup] = []
    @Published private(set) var subscriptions: [ProfileSubscription] = []
    @Published var currentProfileId: String = ""
    @Published var lastSubscriptionURL: String = ""
    @Published var isRefreshing = false

    private let defaults = AppGroup.sharedDefaults ?? UserDefaults.standard
    private let profilesKey      = "profiles_v2"
    private let groupsKey        = "groups_v2"
    private let subscriptionsKey = "subscriptions_v2"
    private let currentKey       = "current_profile_id"
    private let subUrlKey        = "last_sub_url"

    private init() {
        load()
    }

    // MARK: – Public API

    var currentProfile: ConnectionProfile? {
        profiles.first { $0.id == currentProfileId } ?? profiles.first
    }

    var displayProfileName: String {
        if let p = currentProfile, !p.name.isEmpty, !p.name.range(of: "^[0-9a-f]{8}-[0-9a-f]{4}-", options: .regularExpression).map({ _ in true })! {
            return p.name
        }
        if let first = profiles.first, !first.name.isEmpty, !first.name.range(of: "^[0-9a-f]{8}-[0-9a-f]{4}-", options: .regularExpression).map({ _ in true })! {
            return first.name
        }
        return "OBhoD"
    }

    func saveSubscriptionURL(_ urlString: String) {
        lastSubscriptionURL = urlString
        defaults.set(urlString, forKey: subUrlKey)
    }

    func add(_ profile: ConnectionProfile) {
        profiles.append(profile)
        save()
    }

    func update(_ profile: ConnectionProfile) {
        if let idx = profiles.firstIndex(where: { $0.id == profile.id }) {
            profiles[idx] = profile
            save()
        }
    }

    func remove(at offsets: IndexSet) {
        profiles.remove(atOffsets: offsets)
        save()
    }

    func remove(id: String) {
        profiles.removeAll { $0.id == id }
        if currentProfileId == id {
            currentProfileId = profiles.first?.id ?? ""
            saveActiveProfile()
        }
        save()
    }

    func move(from source: IndexSet, to destination: Int) {
        profiles.move(fromOffsets: source, toOffset: destination)
        save()
    }

    func setActive(id: String) {
        currentProfileId = id
        defaults.set(id, forKey: currentKey)
        saveActiveProfile()
    }

    func addGroup(name: String) {
        groups.append(ProfileGroup(name: name))
        saveGroups()
    }

    func removeGroup(id: String) {
        groups.removeAll { $0.id == id }
        saveGroups()
    }

    func incrementTraffic(profileId: String, mb: Double) {
        if let idx = profiles.firstIndex(where: { $0.id == profileId }) {
            profiles[idx].trafficMb += mb
            save()
        }
    }

    func updatePing(profileId: String, ping: Int) {
        if let idx = profiles.firstIndex(where: { $0.id == profileId }) {
            profiles[idx].pingMs = ping
            save()
        }
    }

    // MARK: – Refresh Subscriptions

    func refreshSubscriptions() async -> (refreshed: Int, failed: Int) {
        guard !lastSubscriptionURL.isEmpty, let url = URL(string: lastSubscriptionURL) else {
            return (0, 0)
        }

        isRefreshing = true
        defer { isRefreshing = false }

        do {
            let parsed = try await SubscriptionImport.fetch(url: url)
            var newProfiles = profiles

            for p in parsed.profiles {
                if let existingIdx = newProfiles.firstIndex(where: { $0.peer == p.peer || ($0.name == p.name && !p.name.isEmpty) }) {
                    // Update server details while keeping local traffic stats
                    newProfiles[existingIdx].vkHashes = p.vkHashes
                    newProfiles[existingIdx].workersPerHash = p.workersPerHash
                    newProfiles[existingIdx].password = p.password
                    newProfiles[existingIdx].expiresAt = p.expiresAt
                    newProfiles[existingIdx].subscriptionUrl = url.absoluteString
                } else {
                    newProfiles.append(p)
                }
            }

            profiles = newProfiles
            save()
            return (parsed.profiles.count, 0)
        } catch {
            return (0, 1)
        }
    }

    // MARK: – Persistence

    private func load() {
        if let data = defaults.data(forKey: profilesKey),
           let decoded = try? JSONDecoder().decode([ConnectionProfile].self, from: data) {
            profiles = decoded
        } else if let legacyData = defaults.data(forKey: "profiles_v1"),
                  let legacy = try? JSONDecoder().decode([ConnectionProfile].self, from: legacyData) {
            profiles = legacy
        }

        if let data = defaults.data(forKey: groupsKey),
           let decoded = try? JSONDecoder().decode([ProfileGroup].self, from: data) {
            groups = decoded
        }

        if let data = defaults.data(forKey: subscriptionsKey),
           let decoded = try? JSONDecoder().decode([ProfileSubscription].self, from: data) {
            subscriptions = decoded
        }

        currentProfileId = defaults.string(forKey: currentKey) ?? profiles.first?.id ?? ""
        lastSubscriptionURL = defaults.string(forKey: subUrlKey) ?? ""
    }

    private func save() {
        if let data = try? JSONEncoder().encode(profiles) {
            defaults.set(data, forKey: profilesKey)
        }
        saveActiveProfile()
    }

    private func saveGroups() {
        if let data = try? JSONEncoder().encode(groups) {
            defaults.set(data, forKey: groupsKey)
        }
    }

    private func saveActiveProfile() {
        guard let profile = currentProfile,
              let data = try? JSONEncoder().encode(profile) else { return }
        defaults.set(data, forKey: AppGroup.Keys.activeProfileJSON)
    }
}
