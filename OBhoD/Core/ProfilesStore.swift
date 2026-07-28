import Foundation

// MARK: – Data models (mirror Android ConnectionProfile)

struct ConnectionProfile: Identifiable, Codable, Equatable {
    var id: String         = UUID().uuidString
    var name: String       = "Профиль"
    var peer: String       = ""          // IP:port of wdtt server
    var vkHashes: String   = ""          // comma-separated VK call hashes
    var workersPerHash: Int = 16
    var listenPort: Int    = 9000
    var password: String   = ""
    var trafficMb: Double  = 0
    var groupId: String    = ""
    var useGlobalHashes: Bool = false
}

struct ProfileGroup: Identifiable, Codable {
    var id: String   = UUID().uuidString
    var name: String = "Группа"
}

// MARK: – ProfilesStore

@MainActor
final class ProfilesStore: ObservableObject {
    static let shared = ProfilesStore()

    @Published private(set) var profiles: [ConnectionProfile] = []
    @Published private(set) var groups: [ProfileGroup] = []
    @Published var currentProfileId: String = ""

    private let defaults = AppGroup.sharedDefaults ?? UserDefaults.standard
    private let profilesKey = "profiles_v1"
    private let groupsKey   = "groups_v1"
    private let currentKey  = "current_profile_id"

    private init() {
        load()
    }

    // MARK: – Public API

    var currentProfile: ConnectionProfile? {
        profiles.first { $0.id == currentProfileId }
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
        save()
    }

    func move(from source: IndexSet, to destination: Int) {
        profiles.move(fromOffsets: source, toOffset: destination)
        save()
    }

    func setActive(id: String) {
        currentProfileId = id
        defaults.set(id, forKey: currentKey)
        // Write active profile JSON so TunnelExtension can read it
        if let profile = profiles.first(where: { $0.id == id }),
           let data = try? JSONEncoder().encode(profile) {
            defaults.set(data, forKey: AppGroup.Keys.activeProfileJSON)
        }
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

    // MARK: – Persistence

    private func load() {
        if let data = defaults.data(forKey: profilesKey),
           let decoded = try? JSONDecoder().decode([ConnectionProfile].self, from: data) {
            profiles = decoded
        }
        if let data = defaults.data(forKey: groupsKey),
           let decoded = try? JSONDecoder().decode([ProfileGroup].self, from: data) {
            groups = decoded
        }
        currentProfileId = defaults.string(forKey: currentKey) ?? ""
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
