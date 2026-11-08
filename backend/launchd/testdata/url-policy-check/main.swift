import Foundation
import UserNotifications

// Test-only checker compiled with notification-helper/URLPolicy.swift by
// notification_helper_test.go. It is not part of the deployed helper.
//
//   url <URL>...                 one line per URL: "accept <URL>" or "reject"
//   launch <arg>...              treats the args as helper arguments after argv[0]
//   click default|dismiss <URL>  "open <URL>" or "none"
//   click-non-string             stored value is not a String
let args = Array(CommandLine.arguments.dropFirst())
func show(_ url: URL?, _ yes: String, _ no: String) {
    if let url = url { print("\(yes) \(url.absoluteString)") } else { print(no) }
}
switch args.first {
case "url":
    for value in args.dropFirst() { show(allowedNotificationURL(value), "accept", "reject") }
case "launch":
    show(launchURL(["helper"] + args.dropFirst()), "accept", "invalid_arguments")
case "click" where args.count == 3:
    let action = args[1] == "default" ? UNNotificationDefaultActionIdentifier : UNNotificationDismissActionIdentifier
    show(clickURL(actionIdentifier: action, userInfo: [notificationURLKey: args[2]]), "open", "none")
case "click-non-string":
    show(clickURL(actionIdentifier: UNNotificationDefaultActionIdentifier, userInfo: [notificationURLKey: 1]), "open", "none")
default:
    FileHandle.standardError.write(Data("usage\n".utf8))
    exit(2)
}
