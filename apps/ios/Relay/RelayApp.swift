import BackgroundTasks
import RelayKit
import SwiftUI
import UIKit

@main
struct RelayApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @StateObject private var model = Composition.makeModel()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(model)
                .preferredColorScheme(.dark)
                .task { await delegate.attach(model) }
        }
    }
}

/// The composition root.
///
/// One place that knows which implementations exist, so every other file can be
/// written against the protocol. The `#if targetEnvironment(simulator)` below is
/// not a convenience: the five vendor frameworks are arm64 device-only, so a
/// simulator build cannot link them at all (`docs/APPS-SCOPE.md` §5).
enum Composition {

    static func makeTransport() -> GlassesTransport {
        #if targetEnvironment(simulator)
        return MockTransport()
        #else
        // `VendorTransport` lands here once QCSDK.framework is vendored. Until
        // then a device build gets the mock too, and says so rather than
        // pretending to be talking to hardware that is not wired up.
        return MockTransport()
        #endif
    }

    @MainActor
    static func makeModel() -> CaptureModel {
        // Explicitly typed: `??` between `FileQueueStore?` and `MemoryQueueStore`
        // has no common type to infer without it.
        let store: QueueStore = (try? FileQueueStore.inApplicationSupport()) ?? MemoryQueueStore()
        let queue = StoreAndForwardQueue(store: store)
        // Restoring is async and a composition root is not; the queue is empty
        // and safe to use until it finishes, because `enqueue` dedupes on ids
        // that come back with the restore.
        Task { try? await queue.restore() }

        return CaptureModel(
            glasses: makeTransport(),
            audio: SystemAudioSession(),
            snapshots: FileSnapshotStore.inApplicationSupport(),
            queue: queue,
            network: HotspotSyncNetwork(),
            link: nil
        )
    }
}

/// SwiftUI has no hook for the launch this app cares most about.
///
/// When iOS relaunches a terminated app because a restored `CBCentralManager`
/// had an event, it calls `application(_:didFinishLaunchingWithOptions:)` with
/// `UIApplication.LaunchOptionsKey.bluetoothCentrals` set — and there is no
/// `Scene` phase, because there is no window. `@UIApplicationDelegateAdaptor` is
/// the only way to see it, which is why this class exists at all.
final class AppDelegate: NSObject, UIApplicationDelegate {

    /// `docs/APPS-SCOPE.md` §3.1: the nightly sync is a plugged-in ritual, which
    /// is exactly what `BGProcessingTaskRequest` is for. Must also appear in
    /// `BGTaskSchedulerPermittedIdentifiers` in Info.plist or registration traps.
    static let syncTaskIdentifier = "glass.relay.nightly-sync"

    private(set) var launchKind: LaunchKind = .user
    private var model: CaptureModel?

    func application(
        _ application: UIApplication,
        didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
    ) -> Bool {
        if launchOptions?[.bluetoothCentrals] != nil {
            launchKind = .bluetoothRestoration
        }

        _ = BGTaskScheduler.shared.register(
            forTaskWithIdentifier: Self.syncTaskIdentifier,
            using: nil
        ) { [weak self] task in
            self?.runNightlySync(task)
        }
        scheduleNightlySync()
        return true
    }

    @MainActor
    func attach(_ model: CaptureModel) async {
        guard self.model == nil else { return }
        self.model = model
        // Restoration runs before any screen is shown, and knows not to resume
        // capture for someone who withdrew consent.
        _ = await model.coordinator.applyLaunch(launchKind)
    }

    // MARK: - the nightly window

    func scheduleNightlySync() {
        let request = BGProcessingTaskRequest(identifier: Self.syncTaskIdentifier)
        // Both of these are the point rather than a nicety: the ritual exists
        // because the glasses are charging and the phone is on home WiFi.
        request.requiresExternalPower = true
        request.requiresNetworkConnectivity = true
        try? BGTaskScheduler.shared.submit(request)
    }

    /// The background half of the sync.
    ///
    /// **Only the upload phase.** Joining the glasses' access point needs
    /// `NEHotspotConfigurationManager`, which puts up a system dialog, and there
    /// is nobody to accept it at 3am — `BulkSync` reports that as
    /// `SyncDeferral.needsForeground` rather than hanging until iOS kills the
    /// task.
    private func runNightlySync(_ task: BGTask) {
        scheduleNightlySync() // always re-arm first; a throw below would lose the schedule
        let work = Task { @MainActor [weak self] in
            await self?.model?.runSync()
            task.setTaskCompleted(success: true)
        }
        task.expirationHandler = { work.cancel() }
    }
}

/// consent → permissions → home, in that order and not skippable.
///
/// Asking for a microphone before explaining what will be recorded produces a
/// grant that means nothing, and the system dialog has no room for the part that
/// matters.
///
/// Consent lives in the capture snapshot rather than in `@AppStorage`, because a
/// background relaunch has to be able to read it before any view exists — see
/// `RelayKit/Restoration.swift`.
struct RootView: View {
    @EnvironmentObject private var model: CaptureModel
    @StateObject private var permissions = PermissionsModel()

    var body: some View {
        Group {
            if !model.consentGiven {
                ConsentView { model.grantConsent() }
            } else if !permissions.allGranted {
                PermissionsView(model: permissions)
            } else {
                MainTabs()
            }
        }
        .task { await permissions.refresh() }
    }
}

/// Four tabs, because `docs/ORCHESTRATOR.md` §5 gives the app three jobs and one
/// of them — "every glasses command, by hand" — needs a screen of its own.
struct MainTabs: View {
    @EnvironmentObject private var model: CaptureModel

    var body: some View {
        TabView {
            HomeView(onWithdrawConsent: { await model.withdrawConsent() })
                .tabItem { Label("Capture", systemImage: "record.circle") }
            CommandsView()
                .tabItem { Label("Commands", systemImage: "slider.horizontal.3") }
            SessionsView()
                .tabItem { Label("Sessions", systemImage: "list.bullet.rectangle") }
            SyncView()
                .tabItem { Label("Sync", systemImage: "arrow.triangle.2.circlepath") }
        }
        .tint(Palette.ink)
    }
}
