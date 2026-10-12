import ActivityKit
import SwiftUI
import WidgetKit

@available(iOS 16.1, *)
struct OBhoDLiveActivity: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: OBhoDActivityAttributes.self) { context in
            // Lock Screen and Notification Center Banner ("Шторка")
            OBhoDLockScreenBannerView(context: context)
                .activityBackgroundTint(Color(red: 0.05, green: 0.10, blue: 0.22))
                .activitySystemActionForegroundColor(.white)
        } dynamicIsland: { context in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    HStack(spacing: 6) {
                        Image("AppLogo")
                            .resizable()
                            .aspectRatio(contentMode: .fit)
                            .frame(width: 20, height: 20)
                            .clipShape(Circle())
                        Text("OBhoD")
                            .font(.system(size: 14, weight: .bold, design: .rounded))
                            .foregroundStyle(.white)
                    }
                    .padding(.leading, 4)
                }

                DynamicIslandExpandedRegion(.trailing) {
                    if let since = context.state.connectedSince {
                        Text(since, style: .timer)
                            .font(.system(size: 14, weight: .semibold, design: .monospaced))
                            .foregroundStyle(Color(red: 0.4, green: 0.8, blue: 1.0))
                            .padding(.trailing, 4)
                    } else {
                        Text(context.state.statusText)
                            .font(.system(size: 13, weight: .medium))
                            .foregroundStyle(.secondary)
                    }
                }

                DynamicIslandExpandedRegion(.bottom) {
                    HStack {
                        HStack(spacing: 5) {
                            Circle()
                                .fill(context.state.isConnected ? Color(red: 0.2, green: 0.85, blue: 0.45) : Color(red: 1.0, green: 0.75, blue: 0.0))
                                .frame(width: 8, height: 8)
                            Text(context.state.statusText)
                                .font(.system(size: 13, weight: .medium))
                                .foregroundStyle(.white.opacity(0.9))
                        }

                        Spacer()

                        if #available(iOS 17.0, *) {
                            Button(intent: DisconnectVPNIntent()) {
                                HStack(spacing: 4) {
                                    Image(systemName: "power")
                                        .font(.system(size: 10, weight: .bold))
                                    Text("Отключить")
                                        .font(.system(size: 12, weight: .semibold))
                                }
                                .padding(.horizontal, 10)
                                .padding(.vertical, 5)
                                .background(Color(red: 0.85, green: 0.22, blue: 0.22))
                                .foregroundStyle(.white)
                                .clipShape(Capsule())
                            }
                            .buttonStyle(.plain)
                        } else {
                            Link(destination: URL(string: "obhod://disconnect")!) {
                                HStack(spacing: 4) {
                                    Image(systemName: "power")
                                        .font(.system(size: 10, weight: .bold))
                                    Text("Отключить")
                                        .font(.system(size: 12, weight: .semibold))
                                }
                                .padding(.horizontal, 10)
                                .padding(.vertical, 5)
                                .background(Color(red: 0.85, green: 0.22, blue: 0.22))
                                .foregroundStyle(.white)
                                .clipShape(Capsule())
                            }
                        }
                    }
                    .padding(.horizontal, 6)
                    .padding(.top, 4)
                }
            } compactLeading: {
                Image("AppLogo")
                    .resizable()
                    .aspectRatio(contentMode: .fit)
                    .frame(width: 20, height: 20)
                    .clipShape(Circle())
            } compactTrailing: {
                Text("ВКЛ")
                    .font(.system(size: 12, weight: .heavy, design: .rounded))
                    .foregroundStyle(Color(red: 0.2, green: 0.85, blue: 0.45))
                    .padding(.trailing, 2)
            } minimal: {
                Image("AppLogo")
                    .resizable()
                    .aspectRatio(contentMode: .fit)
                    .frame(width: 16, height: 16)
                    .clipShape(Circle())
            }
        }
    }
}

// MARK: - Lock Screen / Notification Center Banner View ("Шторка")

@available(iOS 16.1, *)
struct OBhoDLockScreenBannerView: View {
    let context: ActivityViewContext<OBhoDActivityAttributes>

    var body: some View {
        ZStack {
            // Фирменный синий градиентный фон OBhoD
            LinearGradient(
                colors: [
                    Color(red: 0.06, green: 0.12, blue: 0.26),
                    Color(red: 0.03, green: 0.07, blue: 0.16)
                ],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )

            VStack(spacing: 12) {
                // Верхний ряд: Логотип + Статус + Кнопка «Отключить»
                HStack(alignment: .center) {
                    HStack(spacing: 8) {
                        Image("AppLogo")
                            .resizable()
                            .aspectRatio(contentMode: .fit)
                            .frame(width: 34, height: 34)
                            .clipShape(Circle())
                            .overlay(
                                Circle()
                                    .stroke(Color.white.opacity(0.15), lineWidth: 1)
                            )

                        VStack(alignment: .leading, spacing: 2) {
                            Text("OBhoD")
                                .font(.system(size: 15, weight: .bold, design: .rounded))
                                .foregroundStyle(.white)

                            HStack(spacing: 5) {
                                Circle()
                                    .fill(
                                        context.state.isConnected ?
                                            Color(red: 0.2, green: 0.85, blue: 0.45) :
                                            Color(red: 1.0, green: 0.75, blue: 0.0)
                                    )
                                    .frame(width: 7, height: 7)

                                Text(context.state.statusText)
                                    .font(.system(size: 12, weight: .semibold))
                                    .foregroundStyle(
                                        context.state.isConnected ?
                                            Color(red: 0.35, green: 0.88, blue: 0.55) :
                                            Color(red: 1.0, green: 0.8, blue: 0.2)
                                    )
                            }
                        }
                    }

                    Spacer()

                    // Фирменная кнопка «Отключить» (работает в фоне без открытия приложения на iOS 17+)
                    if #available(iOS 17.0, *) {
                        Button(intent: DisconnectVPNIntent()) {
                            HStack(spacing: 5) {
                                Image(systemName: "power")
                                    .font(.system(size: 12, weight: .bold))
                                Text("Отключить")
                                    .font(.system(size: 13, weight: .semibold))
                            }
                            .padding(.horizontal, 14)
                            .padding(.vertical, 8)
                            .background(
                                LinearGradient(
                                    colors: [
                                        Color(red: 0.90, green: 0.24, blue: 0.24),
                                        Color(red: 0.75, green: 0.15, blue: 0.15)
                                    ],
                                    startPoint: .top,
                                    endPoint: .bottom
                                )
                            )
                            .foregroundStyle(.white)
                            .clipShape(Capsule())
                            .shadow(color: Color.red.opacity(0.35), radius: 4, x: 0, y: 2)
                        }
                        .buttonStyle(.plain)
                    } else {
                        Link(destination: URL(string: "obhod://disconnect")!) {
                            HStack(spacing: 5) {
                                Image(systemName: "power")
                                    .font(.system(size: 12, weight: .bold))
                                Text("Отключить")
                                    .font(.system(size: 13, weight: .semibold))
                            }
                            .padding(.horizontal, 14)
                            .padding(.vertical, 8)
                            .background(
                                LinearGradient(
                                    colors: [
                                        Color(red: 0.90, green: 0.24, blue: 0.24),
                                        Color(red: 0.75, green: 0.15, blue: 0.15)
                                    ],
                                    startPoint: .top,
                                    endPoint: .bottom
                                )
                            )
                            .foregroundStyle(.white)
                            .clipShape(Capsule())
                            .shadow(color: Color.red.opacity(0.35), radius: 4, x: 0, y: 2)
                        }
                    }
                }

                // Разделитель
                Rectangle()
                    .fill(Color.white.opacity(0.12))
                    .frame(height: 1)

                // Нижний ряд: Таймер текущей сессии + Скорость скачивания
                HStack(alignment: .center) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("ВРЕМЯ СЕССИИ")
                            .font(.system(size: 10, weight: .bold, design: .rounded))
                            .foregroundStyle(Color.white.opacity(0.55))

                        if let since = context.state.connectedSince {
                            Text(since, style: .timer)
                                .font(.system(size: 22, weight: .bold, design: .monospaced))
                                .foregroundStyle(Color(red: 0.4, green: 0.82, blue: 1.0))
                        } else {
                            Text("--:--:--")
                                .font(.system(size: 20, weight: .bold, design: .monospaced))
                                .foregroundStyle(Color.white.opacity(0.4))
                        }
                    }

                    Spacer()

                    // Скорость скачивания без названия
                    HStack(spacing: 5) {
                        Image(systemName: "arrow.down")
                            .font(.system(size: 13, weight: .bold))
                            .foregroundStyle(Color(red: 0.35, green: 0.75, blue: 1.0))
                        Text(context.state.downloadSpeed.isEmpty ? "0 КБ/с" : context.state.downloadSpeed)
                            .font(.system(size: 17, weight: .bold, design: .rounded))
                            .foregroundStyle(.white)
                    }
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .background(Color.white.opacity(0.08))
                    .clipShape(RoundedRectangle(cornerRadius: 10))
                }
            }
            .padding(16)
        }
    }
}
