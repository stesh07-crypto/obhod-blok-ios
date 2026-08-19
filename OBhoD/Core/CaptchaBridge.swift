import Foundation
import SwiftUI
import WebKit

struct IOSCaptchaRequest: Codable, Identifiable, Equatable {
    let id: String
    let mode: String
    let redirectURI: String
    let sessionToken: String
    let createdAtMS: Int64

    enum CodingKeys: String, CodingKey {
        case id
        case mode
        case redirectURI = "redirect_uri"
        case sessionToken = "session_token"
        case createdAtMS = "created_at_ms"
    }
}

private struct IOSCaptchaResult: Codable {
    let id: String
    let result: String
}

@MainActor
final class CaptchaBridge: ObservableObject {
    static let shared = CaptchaBridge()

    @Published var pendingRequest: IOSCaptchaRequest?

    private let requestName = "wdtt_captcha_request.json"
    private let resultName = "wdtt_captcha_result.json"
    private var timer: Timer?

    private init() {}

    func start() {
        guard timer == nil else { return }
        poll()
        let timer = Timer(timeInterval: 0.20, repeats: true) { [weak self] _ in
            Task { @MainActor in
                self?.poll()
            }
        }
        RunLoop.main.add(timer, forMode: .common)
        self.timer = timer
    }

    func stop() {
        timer?.invalidate()
        timer = nil
    }

    func complete(_ request: IOSCaptchaRequest, result: String) {
        guard !result.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return }
        guard let resultURL = fileURL(named: resultName) else { return }
        let payload = IOSCaptchaResult(id: request.id, result: result)
        do {
            let data = try JSONEncoder().encode(payload)
            try data.write(to: resultURL, options: .atomic)
            if pendingRequest?.id == request.id {
                pendingRequest = nil
            }
        } catch {
            NSLog("[WDTT-Captcha] result write failed: \(error.localizedDescription)")
        }
    }

    func cancel(_ request: IOSCaptchaRequest) {
        complete(request, result: "error:cancelled")
    }

    private func poll() {
        guard let requestURL = fileURL(named: requestName) else { return }
        guard let data = try? Data(contentsOf: requestURL),
              let request = try? JSONDecoder().decode(IOSCaptchaRequest.self, from: data)
        else {
            if pendingRequest != nil {
                pendingRequest = nil
            }
            return
        }

        // Do not resurrect abandoned challenge files after a crash/reinstall.
        let ageMS = Int64(Date().timeIntervalSince1970 * 1000) - request.createdAtMS
        guard ageMS >= 0, ageMS < 180_000 else {
            try? FileManager.default.removeItem(at: requestURL)
            if pendingRequest?.id == request.id { pendingRequest = nil }
            return
        }

        if pendingRequest?.id != request.id {
            pendingRequest = request
        }
    }

    private func fileURL(named name: String) -> URL? {
        FileManager.default
            .containerURL(forSecurityApplicationGroupIdentifier: AppGroup.identifier)?
            .appendingPathComponent(name, isDirectory: false)
    }
}

struct CaptchaChallengeView: View {
    let request: IOSCaptchaRequest
    @ObservedObject private var bridge = CaptchaBridge.shared

    var body: some View {
        NavigationView {
            CaptchaWebView(
                request: request,
                onSuccess: { token in
                    bridge.complete(request, result: token)
                },
                onFailure: { message in
                    bridge.complete(request, result: "error:\(message)")
                }
            )
            .ignoresSafeArea(edges: .bottom)
            .navigationTitle("Проверка VK")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("Отмена") {
                        bridge.cancel(request)
                    }
                    .foregroundColor(.red)
                }
            }
        }
        .interactiveDismissDisabled(true)
    }
}

private struct CaptchaWebView: UIViewRepresentable {
    let request: IOSCaptchaRequest
    let onSuccess: (String) -> Void
    let onFailure: (String) -> Void

    func makeCoordinator() -> Coordinator {
        Coordinator(onSuccess: onSuccess, onFailure: onFailure)
    }

    func makeUIView(context: Context) -> WKWebView {
        let controller = WKUserContentController()
        controller.add(context.coordinator, name: "wdttCaptcha")
        controller.addUserScript(
            WKUserScript(
                source: Self.interceptorScript,
                injectionTime: .atDocumentStart,
                forMainFrameOnly: false
            )
        )

        let config = WKWebViewConfiguration()
        config.userContentController = controller
        config.websiteDataStore = .default()
        config.defaultWebpagePreferences.allowsContentJavaScript = true

        let webView = WKWebView(frame: .zero, configuration: config)
        webView.navigationDelegate = context.coordinator
        webView.allowsBackForwardNavigationGestures = false
        webView.customUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15"
        webView.scrollView.contentInsetAdjustmentBehavior = .automatic

        if let url = URL(string: request.redirectURI) {
            webView.load(URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 30))
        } else {
            DispatchQueue.main.async {
                onFailure("invalid redirect URI")
            }
        }
        return webView
    }

    func updateUIView(_ uiView: WKWebView, context: Context) {}

    static func dismantleUIView(_ uiView: WKWebView, coordinator: Coordinator) {
        uiView.stopLoading()
        uiView.configuration.userContentController.removeScriptMessageHandler(forName: "wdttCaptcha")
        uiView.navigationDelegate = nil
    }

    final class Coordinator: NSObject, WKScriptMessageHandler, WKNavigationDelegate {
        private let onSuccess: (String) -> Void
        private let onFailure: (String) -> Void
        private var finished = false

        init(onSuccess: @escaping (String) -> Void, onFailure: @escaping (String) -> Void) {
            self.onSuccess = onSuccess
            self.onFailure = onFailure
        }

        func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
            guard !finished, message.name == "wdttCaptcha" else { return }
            guard let body = message.body as? [String: Any],
                  let type = body["type"] as? String
            else { return }

            switch type {
            case "success":
                guard let token = body["token"] as? String, !token.isEmpty else { return }
                finished = true
                onSuccess(token)
            case "error":
                let value = (body["message"] as? String) ?? "VK captcha error"
                finished = true
                onFailure(value)
            default:
                break
            }
        }

        func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
            guard !finished else { return }
            // Ignore transient WebKit cancellation while VK redirects between its
            // captcha pages. Real terminal failures arrive through didFailProvisional.
            if (error as NSError).code == NSURLErrorCancelled { return }
        }

        func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
            guard !finished else { return }
            finished = true
            onFailure(error.localizedDescription)
        }
    }

    private static let interceptorScript = #"""
    (function() {
        if (window.__wdtt_ios_interceptor_installed) return;
        window.__wdtt_ios_interceptor_installed = true;

        function send(type, value) {
            try {
                window.webkit.messageHandlers.wdttCaptcha.postMessage(
                    type === 'success'
                        ? {type: 'success', token: value}
                        : {type: 'error', message: value}
                );
            } catch (_) {}
        }

        const origFetch = window.fetch;
        if (origFetch) {
            window.fetch = async function() {
                const args = arguments;
                const target = args[0] || '';
                const response = await origFetch.apply(this, args);
                try {
                    const url = typeof target === 'string' ? target : (target && target.url ? target.url : '');
                    if (url.includes('captchaNotRobot.check')) {
                        const clone = response.clone();
                        const data = await clone.json();
                        if (data && data.response && data.response.success_token) {
                            send('success', data.response.success_token);
                        } else if (data && data.error) {
                            send('error', JSON.stringify(data.error));
                        }
                    }
                } catch (_) {}
                return response;
            };
        }

        const origOpen = XMLHttpRequest.prototype.open;
        const origSend = XMLHttpRequest.prototype.send;
        XMLHttpRequest.prototype.open = function(method, url) {
            this.__wdtt_url = url || '';
            return origOpen.apply(this, arguments);
        };
        XMLHttpRequest.prototype.send = function() {
            const xhr = this;
            if ((xhr.__wdtt_url || '').includes('captchaNotRobot.check')) {
                xhr.addEventListener('load', function() {
                    try {
                        const data = JSON.parse(xhr.responseText || '{}');
                        if (data && data.response && data.response.success_token) {
                            send('success', data.response.success_token);
                        } else if (data && data.error) {
                            send('error', JSON.stringify(data.error));
                        }
                    } catch (_) {}
                });
            }
            return origSend.apply(this, arguments);
        };
    })();
    """#
}
