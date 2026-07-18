import AppKit
import Foundation
import UserNotifications

final class AppDelegate: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    private let arguments = Array(CommandLine.arguments.dropFirst())
    private var finished = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 30) {
            self.fail("notification request timed out", code: 75)
        }
        let center = UNUserNotificationCenter.current()
        if arguments == ["--check"] {
            checkAuthorization(with: center)
            return
        }
        guard arguments.count == 2 else {
            fail("expected title and message", code: 64)
            return
        }

        center.delegate = self
        center.getNotificationSettings { settings in
            switch settings.authorizationStatus {
            case .notDetermined:
                center.requestAuthorization(options: [.alert]) { granted, error in
                    if let error {
                        self.fail("notification authorization failed: \(error)", code: 77)
                    } else if !granted {
                        self.fail("notification permission denied", code: 77)
                    } else {
                        self.deliver(with: center)
                    }
                }
            case .authorized, .provisional, .ephemeral:
                self.deliver(with: center)
            case .denied:
                self.fail("notification permission denied; allow ToolTend Notifier in System Settings", code: 77)
            @unknown default:
                self.fail("unknown notification authorization state", code: 70)
            }
        }
    }

    private func checkAuthorization(with center: UNUserNotificationCenter) {
        center.getNotificationSettings { settings in
            switch settings.authorizationStatus {
            case .authorized, .provisional, .ephemeral:
                self.finish(code: 0)
            case .notDetermined:
                self.fail("notification permission has not been requested", code: 77)
            case .denied:
                self.fail("notification permission denied; allow ToolTend Notifier in System Settings", code: 77)
            @unknown default:
                self.fail("unknown notification authorization state", code: 70)
            }
        }
    }

    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .list])
    }

    private func deliver(with center: UNUserNotificationCenter) {
        let content = UNMutableNotificationContent()
        content.title = arguments[0]
        content.body = arguments[1]
        let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
        center.add(request) { error in
            if let error {
                self.fail("notification delivery failed: \(error)", code: 70)
                return
            }
            self.finish(code: 0, delay: 2)
        }
    }

    private func fail(_ message: String, code: Int32) {
        FileHandle.standardError.write(Data((message + "\n").utf8))
        finish(code: code)
    }

    private func finish(code: Int32, delay: TimeInterval = 0) {
        DispatchQueue.main.async {
            guard !self.finished else { return }
            self.finished = true
            DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
                exit(code)
            }
        }
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
