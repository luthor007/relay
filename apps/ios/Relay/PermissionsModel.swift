import AVFoundation
import CoreBluetooth
import Foundation
import UserNotifications

/// Tracks the three permissions capture cannot run without.
///
/// Bluetooth is the awkward one: on iOS there is no "check without asking" API.
/// `CBManager.authorization` reports the state, but the *prompt* fires the
/// moment a `CBCentralManager` is instantiated — so merely constructing one to
/// read the status is what triggers the dialog. The manager is therefore created
/// only from ``requestBluetooth()``, after the explanation screen, rather than
/// eagerly at launch.
@MainActor
final class PermissionsModel: NSObject, ObservableObject {

    @Published private(set) var microphone: Bool = false
    @Published private(set) var notifications: Bool = false
    @Published private(set) var bluetooth: Bool = false

    private var central: CBCentralManager?

    var allGranted: Bool { microphone && notifications && bluetooth }

    func refresh() async {
        microphone = AVAudioApplication.shared.recordPermission == .granted
        bluetooth = CBManager.authorization == .allowedAlways

        let settings = await UNUserNotificationCenter.current().notificationSettings()
        notifications = settings.authorizationStatus == .authorized
            || settings.authorizationStatus == .provisional
    }

    func requestAll() async {
        await requestMicrophone()
        await requestNotifications()
        requestBluetooth()
        await refresh()
    }

    private func requestMicrophone() async {
        _ = await AVAudioApplication.requestRecordPermission()
    }

    private func requestNotifications() async {
        _ = try? await UNUserNotificationCenter.current()
            .requestAuthorization(options: [.alert, .sound, .badge])
    }

    private func requestBluetooth() {
        guard central == nil else { return }
        central = CBCentralManager(delegate: self, queue: nil)
    }
}

extension PermissionsModel: CBCentralManagerDelegate {
    nonisolated func centralManagerDidUpdateState(_ central: CBCentralManager) {
        Task { @MainActor in await refresh() }
    }
}
