import AppKit
import UserNotifications

// These are implementation time limits, not specification constants.
private let registrationTimeout: TimeInterval = 30
private let responseLifetime: TimeInterval = 60

private func taskURL(_ value: String) -> URL? {
    guard let parts = URLComponents(string: value),
          parts.scheme == "http", parts.host == "127.0.0.1",
          let port = parts.port, (1...65535).contains(port),
          parts.user == nil, parts.password == nil,
          parts.query == nil, parts.fragment == nil,
          parts.path.hasPrefix("/tasks/"), parts.path.count > "/tasks/".count
    else { return nil }
    return parts.url
}

final class NotificationDelegate: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    private var deadline: Timer?
    private var reported = false

    // Only fixed protocol codes cross back to evaloomd. Do not serialize OS
    // errors, task contents or URLs into the result file or system logs.
    private func report(_ code: String) {
        guard !reported else { return }
        reported = true
        if let path = ProcessInfo.processInfo.environment["EVALOOM_NOTIFICATION_RESULT"] {
            do {
                try Data(code.utf8).write(to: URL(fileURLWithPath: path), options: .atomic)
            } catch {
                // The caller will report a timeout if it cannot receive a result.
                NSApp.terminate(nil)
            }
        }
    }

    private func fail(_ code: String) {
        deadline?.invalidate()
        report(code)
        NSApp.terminate(nil)
    }

    func applicationWillFinishLaunching(_ notification: Notification) {
        // Set the delegate before macOS delivers a notification-click response.
        UNUserNotificationCenter.current().delegate = self
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let args = CommandLine.arguments
        if args.count == 1 {
            // macOS may relaunch an exited helper for a Notification Center
            // click. Await that response without registering another notice.
            deadline = Timer.scheduledTimer(withTimeInterval: responseLifetime, repeats: false) { _ in
                NSApp.terminate(nil)
            }
            return
        }
        guard args.count == 4, let url = taskURL(args[3]) else {
            fail("invalid_arguments")
            return
        }
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        deadline = Timer.scheduledTimer(withTimeInterval: registrationTimeout, repeats: false) { _ in
            self.fail("timed_out")
        }
        center.requestAuthorization(options: [.alert]) { granted, error in
            DispatchQueue.main.async {
                guard !self.reported else { return }
                guard error == nil else {
                    self.fail("authorization_failed")
                    return
                }
                guard granted else {
                    self.fail("permission_denied")
                    return
                }
                let content = UNMutableNotificationContent()
                content.title = args[1]
                content.body = args[2]
                content.userInfo = ["taskURL": url.absoluteString]
                let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
                center.add(request) { error in
                    DispatchQueue.main.async {
                        guard !self.reported else { return }
                        guard error == nil else {
                            self.fail("registration_failed")
                            return
                        }
                        self.deadline?.invalidate()
                        self.report("sent")
                        self.deadline = Timer.scheduledTimer(withTimeInterval: responseLifetime, repeats: false) { _ in
                            NSApp.terminate(nil)
                        }
                    }
                }
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

    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        // Use the notification's URL, so concurrent helper instances cannot
        // accidentally open a different task. Dismissal does not open a browser.
        DispatchQueue.main.async {
            if response.actionIdentifier == UNNotificationDefaultActionIdentifier,
               let value = response.notification.request.content.userInfo["taskURL"] as? String,
               let url = taskURL(value) {
                NSWorkspace.shared.open(url)
            }
            completionHandler()
            NSApp.terminate(nil)
        }
    }
}

let app = NSApplication.shared
let delegate = NotificationDelegate()
app.delegate = delegate
app.run()
