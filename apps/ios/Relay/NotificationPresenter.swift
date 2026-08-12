import Foundation
import RelayKit
import UserNotifications

/// The app's half of `notify` — `docs/SYSTEM.md` §6.1.
///
/// Deliberately thin. Every decision the frame implies — what to call an
/// untitled notification, whether it makes a sound, and what collapses with what
/// — lives in `NotifyFrame.presentation` inside `RelayKit`, where
/// `RelayKitTests` reaches it without a phone. This file does one thing:
/// hand those fields to `UNUserNotificationCenter`.
///
/// The frame exists because `docs/ADAPTERS.md` §7 needs a channel that reaches
/// the user **without speech**: under quiet hours the box holds the speaking and
/// still sends this, so `silent` means present-and-soundless rather than absent.
/// Suppressing the notification here would be the quiet-hours behaviour failing
/// with nothing in the log — which is exactly what the phone did while `notify`
/// was missing from the vocabulary and fell through to `unknownType`.
struct NotificationPresenter: Sendable {

    /// Injected so a test host can substitute one. Defaults to the real centre.
    var deliver: @Sendable (UNNotificationRequest) -> Void = { request in
        UNUserNotificationCenter.current().add(request, withCompletionHandler: nil)
    }

    func present(_ frame: NotifyFrame) {
        let plan = frame.presentation

        let content = UNMutableNotificationContent()
        content.title = plan.title
        content.body = plan.body
        // Nil rather than `.default`: quiet hours are the reason this frame is
        // separate from `speak` at all.
        content.sound = plan.playSound ? .default : nil
        if let threadId = plan.threadId { content.threadIdentifier = threadId }

        // No trigger: deliver now. The identifier is the ping id where there is
        // one, so relayd's two-minute re-ping replaces the banner rather than
        // adding another.
        deliver(UNNotificationRequest(identifier: plan.identifier, content: content, trigger: nil))
    }
}
