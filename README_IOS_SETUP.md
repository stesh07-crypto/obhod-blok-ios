# Настройка GitHub Secrets для сборки OBhoD iOS

## Что нужно сделать в Apple Developer Portal

### 1. Создать App ID

Перейди на [developer.apple.com](https://developer.apple.com) → Certificates, IDs & Profiles → Identifiers → **+**

- **Bundle ID:** `net.qwdtt.client.ios`
- **Capabilities:** ✅ Network Extensions, ✅ App Groups

Создай второй для расширения:
- **Bundle ID:** `net.qwdtt.client.ios.tunnel`
- **Capabilities:** ✅ Network Extensions, ✅ App Groups

### 2. Создать App Group

Identifiers → App Groups → **+**
- **ID:** `group.net.qwdtt.client.ios`

Добавь эту группу к обоим App ID выше.

### 3. Создать Distribution Certificate (если нет)

Certificates → **+** → **Apple Distribution**
- Создай `.p12` файл: открой Keychain Access → найди сертификат → Export → `.p12`

### 4. Создать Provisioning Profiles

#### Для основного приложения:
Profiles → **+** → **App Store Connect** → выбери `net.qwdtt.client.ios`
- Имя профиля: `obhod_app`

#### Для расширения:
Profiles → **+** → **App Store Connect** → выбери `net.qwdtt.client.ios.tunnel`
- Имя профиля: `obhod_ext`

### 5. App Store Connect API Key

Перейди на [appstoreconnect.apple.com](https://appstoreconnect.apple.com) → Users and Access → **Keys** → **+**
- Роль: **Developer** или **App Manager**
- Скачай `.p8` файл (скачивается только один раз!)
- Запомни **Key ID** и **Issuer ID**

### 6. Создать приложение в App Store Connect

Apps → **+** → New App
- Platform: iOS
- Bundle ID: `net.qwdtt.client.ios`

---

## GitHub Secrets

Перейди в репозиторий → **Settings** → **Secrets and variables** → **Actions** → **New repository secret**

| Название секрета | Что вставить |
|-----------------|--------------|
| `IOS_DISTRIBUTION_CERTIFICATE_BASE64` | `.p12` файл в Base64: `base64 -i certificate.p12 \| tr -d '\n'` |
| `IOS_DISTRIBUTION_CERTIFICATE_PASSWORD` | Пароль от `.p12` |
| `KEYCHAIN_PASSWORD` | Любой случайный пароль (например `myKeychain123`) |
| `IOS_APP_PROVISIONING_PROFILE_BASE64` | `.mobileprovision` для `obhod_app` в Base64 |
| `IOS_EXT_PROVISIONING_PROFILE_BASE64` | `.mobileprovision` для `obhod_ext` в Base64 |
| `APP_STORE_CONNECT_API_KEY_ID` | Key ID из App Store Connect |
| `APP_STORE_CONNECT_API_ISSUER_ID` | Issuer ID из App Store Connect |
| `APP_STORE_CONNECT_API_KEY_BASE64` | `.p8` файл в Base64: `base64 -i AuthKey_*.p8 \| tr -d '\n'` |

### Команды для конвертации в Base64 (выполни на Mac):
```bash
# Сертификат
base64 -i certificate.p12 | tr -d '\n' | pbcopy
echo "Скопировано в буфер!"

# Provisioning Profile (основное приложение)
base64 -i ~/Library/MobileDevice/Provisioning\ Profiles/obhod_app.mobileprovision | tr -d '\n' | pbcopy

# Provisioning Profile (расширение)
base64 -i ~/Library/MobileDevice/Provisioning\ Profiles/obhod_ext.mobileprovision | tr -d '\n' | pbcopy

# API Key
base64 -i AuthKey_XXXXXXXXXX.p8 | tr -d '\n' | pbcopy
```

---

## Запуск сборки

После настройки всех секретов:

1. Запушь код в GitHub:
```bash
git add .
git commit -m "feat: add iOS project"
git push origin main
```

2. Перейди в репозиторий → **Actions** → **OBhoD iOS — Build & TestFlight** → **Run workflow**

3. После успешной сборки (≈15-20 минут) IPA автоматически загрузится в TestFlight.

4. В App Store Connect → TestFlight → добавь тестеров по email или создай публичную ссылку.

---

## Структура проекта

```
proxy-turn-vk-ios/
├── .github/workflows/ios.yml      ← GitHub Actions CI/CD
├── OBhoD/                         ← Основное приложение (SwiftUI)
│   ├── App/
│   │   ├── OBhoDApp.swift         ← Точка входа + URL scheme
│   │   ├── Info.plist
│   │   └── OBhoD.entitlements
│   ├── Core/
│   │   ├── TunnelManager.swift    ← Управление туннелем
│   │   ├── GoClient.swift         ← Swift ↔ Go FFI
│   │   ├── ProfilesStore.swift    ← Хранение профилей
│   │   ├── SubscriptionImport.swift ← Импорт qwdtt://
│   │   ├── SettingsStore.swift    ← Настройки
│   │   └── AppGroup.swift         ← Shared constants
│   └── Views/
│       ├── ContentView.swift
│       ├── ProfilesView.swift     ← Список профилей + подключение
│       ├── LogsView.swift         ← Реалтайм логи
│       ├── SettingsView.swift     ← Настройки
│       └── SubscriptionsSheet.swift ← Импорт подписок
├── TunnelExtension/               ← NetworkExtension (VPN иконка)
│   ├── PacketTunnelProvider.swift
│   ├── Info.plist
│   └── TunnelExtension.entitlements
├── go_client/                     ← Go клиент (общий с Android)
│   ├── ios_bridge.go              ← C-экспорты для iOS
│   └── ios_cbridge.go             ← CGO shims
├── Frameworks/                    ← Собирается CI
│   └── libwdttclient.xcframework
├── scripts/
│   ├── build-ios-xcframework.sh   ← Сборка Go → xcframework
│   └── export.plist               ← Настройки IPA export
└── project.yml                    ← XcodeGen конфиг
```
