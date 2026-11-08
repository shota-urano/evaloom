import Foundation
import UserNotifications

// The URL allowlist of spec 12 3.6 (Q77). The helper opens only
//   http://127.0.0.1:<port>/tasks/<taskId>  (task notifications), or
//   http://127.0.0.1:<port>/                (the Dashboard, spec 12 3.5).
// Every condition must hold; any other URL is rejected. When the allowlist
// changes, change spec 12 3.6 and the URLs evaloomd's Notifier passes together.
//
// This file has no top-level code so that tests can compile it with a small
// checker (backend/launchd/testdata/url-policy-check) and exercise the same
// functions the helper uses.
func allowedNotificationURL(_ value: String) -> URL? {
    guard let parts = URLComponents(string: value),
          parts.scheme == "http",
          parts.host == "127.0.0.1",
          let port = parts.port, (1...65535).contains(port),
          parts.user == nil, parts.password == nil,
          parts.query == nil, parts.fragment == nil,
          allowedNotificationPath(parts.percentEncodedPath)
    else { return nil }
    return parts.url
}

// "/" exactly, or "/tasks/<taskId>" with a non-empty taskId. evaloomd
// percent-encodes the task ID, so a raw "/" after the prefix is another path.
private func allowedNotificationPath(_ path: String) -> Bool {
    if path == "/" {
        return true
    }
    let prefix = "/tasks/"
    guard path.hasPrefix(prefix) else { return false }
    let taskID = path.dropFirst(prefix.count)
    return !taskID.isEmpty && !taskID.contains("/")
}

// The key under which the helper stores the URL in the notification.
let notificationURLKey = "taskURL"

// launchURL validates the helper's command line: <helper> <title> <body> <URL>.
// nil means the helper must not post a notification and must report
// invalid_arguments.
func launchURL(_ arguments: [String]) -> URL? {
    guard arguments.count == 4 else { return nil }
    return allowedNotificationURL(arguments[3])
}

// clickURL returns the URL to open for a notification response. The stored
// URL is checked again against the allowlist; dismissals and anything outside
// the allowlist open nothing.
func clickURL(actionIdentifier: String, userInfo: [AnyHashable: Any]) -> URL? {
    guard actionIdentifier == UNNotificationDefaultActionIdentifier,
          let value = userInfo[notificationURLKey] as? String
    else { return nil }
    return allowedNotificationURL(value)
}
