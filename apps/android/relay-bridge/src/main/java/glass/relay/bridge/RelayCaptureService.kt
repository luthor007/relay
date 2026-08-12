package glass.relay.bridge

import android.Manifest
import android.app.NotificationManager
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import androidx.core.content.ContextCompat
import glass.relay.bridge.capture.LocalRecordingController
import glass.relay.bridge.capture.VoiceSession
import glass.relay.bridge.consent.ConsentGate
import glass.relay.bridge.link.JvmWebSocketFactory
import glass.relay.bridge.link.LinkAuth
import glass.relay.bridge.link.LinkException
import glass.relay.bridge.link.RelaydLink
import glass.relay.bridge.link.RelaydRouter
import glass.relay.bridge.link.SystemLinkScheduler
import glass.relay.bridge.link.endpoints
import glass.relay.bridge.session.ApprovalQueue
import glass.relay.bridge.session.SessionRegistry
import glass.relay.bridge.storage.StoragePolicy
import glass.relay.bridge.ui.AppView
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/**
 * Keeps the glasses connected and capturing while the app is not in the foreground.
 *
 * This is the piece the vendor SDK does not provide. Their sample is Activity-only,
 * so capture stops the moment the user switches apps — which for an all-day capture
 * product means it does not work at all.
 *
 * Design notes that are not obvious:
 *
 *  - **Typed foreground service.** From API 34 a service must declare its type and
 *    hold the matching permission, and getting it wrong throws at runtime rather
 *    than degrading — so [blockingPermissions] is checked before
 *    `startForeground`. It runs as `connectedDevice` only; see
 *    [promoteToForeground] for why `microphone` is added later or not at all.
 *
 *  - **START_STICKY is not enough.** OEM skins kill and do not restart. The wake
 *    lock, boot receiver and battery-optimisation exemption are all there for the
 *    same reason: staying alive on Android is an integration problem, not an API
 *    call. See [BatteryOptimisation].
 *
 *  - **The notification is not decoration.** It is the user-visible proof that a
 *    device with a camera and microphone is recording. It always reflects real
 *    state and is never silenced. It is also the *only* recording indicator this
 *    platform is known to have, which is why [ConsentGate] is told whether it can
 *    be shown and refuses capture outright when it cannot.
 *
 *  - **Consent is a decision function, not a stored boolean.** Everything that
 *    can record — [LocalRecordingController] and [VoiceSession] — is gated on
 *    `consent.verdict.value.capture` and on nothing else. `ARCHITECTURE.md` §6
 *    requires capture to default off in a new location or with new voices
 *    present, until confirmed, and a boolean in `SharedPreferences` cannot say
 *    where or with whom it was granted. See [glass.relay.bridge.consent.ConsentGate].
 */
class RelayCaptureService : Service() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private lateinit var notifications: CaptureNotifications
    private lateinit var supervisor: ConnectionSupervisor
    private lateinit var transport: GlassesTransport
    private lateinit var recording: LocalRecordingController
    private lateinit var diagnostics: CaptureDiagnostics
    private lateinit var preferences: CapturePreferences
    private val audioSink = CaptureAudioSink()
    private var wakeLock: PowerManager.WakeLock? = null

    /**
     * The one place "may we record" is decided.
     *
     * Private, like everything else here: `onBind` returns null on purpose, so
     * the UI and the notification answer through [ACTION_CONSENT_YES] /
     * [ACTION_CONSENT_NO] and read the question off [CaptureBus]. A second copy
     * of consent state in the app module is the failure `CapturePreferences`
     * already exists to prevent.
     */
    private lateinit var consent: ConsentGate

    /**
     * The link to the box. Null until pairing has written a URL and a token.
     *
     * Not opened speculatively: a phone with no box configured must not spend
     * its battery retrying a URL that does not exist, and `RelaydLink` retries
     * forever by design.
     */
    private var link: RelaydLink? = null
    private var router: RelaydRouter? = null
    private var linkScheduler: SystemLinkScheduler? = null

    /** What the box says is running, and what it is asking. `ORCHESTRATOR.md` §5. */
    val sessions = SessionRegistry()
    val approvals = ApprovalQueue()

    /** True only while the *phone's* microphone is open. See [promoteToForeground]. */
    private var micTypeNeeded = false

    /**
     * Path A, when the transport has it.
     *
     * Null on a build whose transport cannot do live audio — the vendor one, at
     * time of writing. The talk button is disabled rather than failing at the
     * moment someone taps it.
     */
    var voice: VoiceSession? = null
        private set

    /** The latest storage read, for the UI and for the sync trigger. */
    private val _storage = MutableStateFlow<StoragePolicy.Assessment?>(null)
    val storage: StateFlow<StoragePolicy.Assessment?> = _storage.asStateFlow()

    private val _state = MutableStateFlow(CaptureState.Idle)
    val state: StateFlow<CaptureState> = _state.asStateFlow()

    override fun onCreate() {
        super.onCreate()
        notifications = CaptureNotifications(this)
        diagnostics = CaptureDiagnostics(this)
        preferences = CapturePreferences(this)
        transport = TransportProvider.get(this)

        consent = ConsentGate(
            initial = preferences.consentState,
            // The ongoing notification *is* the recording indicator on this
            // platform, and ConsentPolicy.indicatorRequired() has no off
            // switch — so a phone that will not let us post it does not get to
            // record. Re-checked in onStartCommand, because the user can
            // revoke the permission while the service is alive.
            indicatorAvailable = canShowIndicator(this),
            persist = { preferences.consentState = it },
        )

        supervisor = ConnectionSupervisor(
            transport = transport,
            scope = scope,
            // The controller below owns 0x0E04. Two components issuing it on
            // the same wear event would fight, and the symptom is capture
            // flapping rather than an error.
            ownsRecording = false,
        )

        recording = LocalRecordingController(
            device = transport.asRecordingDevice(),
            scope = scope,
        )

        voice = transport.asVoiceChannel()?.let { channel ->
            VoiceSession(
                channel = channel,
                scope = scope,
                sink = audioSink,
                // Read at the moment of the trigger, not cached: a yes from
                // this morning is not consent for the conversation happening
                // now, and the gate may have revoked it when the box said the
                // place changed.
                consentGranted = { consent.verdict.value.capture },
            )
        }

        openLink()

        scope.launch {
            supervisor.state.collect { connection ->
                _state.value = _state.value.copy(connection = connection)
                publish()
            }
        }
        scope.launch {
            supervisor.capture.collect { capture ->
                // Wear comes from the supervisor because that is where the
                // device events arrive; what to do about it is the controller's.
                recording.setWorn(capture.worn)
                if (capture.worn != _state.value.worn) {
                    // `wear` is one of §6.1's eight phone→server frames and the
                    // box acts on it. Sent on the edge, not on every poll: the
                    // outbox is bounded and a repeated fact is not news.
                    router?.sendWear(capture.worn)
                }
                _state.value = _state.value.copy(
                    worn = capture.worn,
                    batteryPercent = capture.batteryPercent,
                )
                publish()
            }
        }
        scope.launch {
            recording.state.collect { snapshot ->
                _state.value = _state.value.copy(recording = snapshot.recording)
                publish()
            }
        }
        scope.launch {
            // The gate is the only thing that may turn capture on, and it is
            // watched rather than sampled: a verdict that changes because the
            // box named a new place has to stop a recording already running,
            // not merely refuse the next one.
            consent.verdict.collect { verdict ->
                recording.setConsent(verdict.capture)
                _state.value = _state.value.copy(
                    consentQuestion = verdict.question,
                    consentWhy = verdict.why,
                )
                publish()
            }
        }
        scope.launch { heartbeat() }
        scope.launch { pollStorage() }
    }

    /**
     * Open the link to the box, if there is one to open.
     *
     * `SYSTEM.md` §6.1. Nothing here is speculative: with no paired box the
     * link is simply not built, because `RelaydLink` retries forever by design
     * and a phone reconnecting to a URL that does not exist is a battery
     * complaint with no upside.
     */
    private fun openLink() {
        if (!preferences.boxConfigured) {
            Log.i(TAG, "no box paired; not opening a relayd link")
            return
        }
        val scheduler = SystemLinkScheduler()
        val opened = RelaydLink(
            // Every address the box can be reached at, in the order to try
            // them: the LAN first, the relay second. Passing `boxUrl` alone —
            // which is what this did until SYSTEM.md §7 reached the phone — is
            // a phone that stops reaching its box the moment it leaves the
            // house, retrying a hostname that does not resolve.
            addresses = endpoints(preferences.boxAddress),
            auth = LinkAuth(token = preferences.boxToken),
            socketFactory = JvmWebSocketFactory(),
            scheduler = scheduler,
        )
        linkScheduler = scheduler
        link = opened
        router = RelaydRouter(
            link = opened,
            sessions = sessions,
            approvals = approvals,
            clock = System::currentTimeMillis,
            sink = BoxSink(),
        ).also { it.attach() }
        opened.connect()
    }

    /**
     * The platform half of the router.
     *
     * `speak` has no speaker yet — `GlassesTransport` carries no playback call
     * and there is no phone-side TTS in this module — so it surfaces as the
     * last line from the box rather than being swallowed. That is deliberately
     * a visible partial implementation: silently dropping a `speak` would make
     * the box look broken from the phone, which is the harder bug to find.
     */
    private inner class BoxSink : RelaydRouter.Sink {

        override fun speak(text: String, sessionId: String?, interrupt: Boolean) {
            _state.value = _state.value.copy(lastFromBox = text)
            publish()
        }

        override fun notify(title: String, body: String, silent: Boolean) {
            _state.value = _state.value.copy(lastFromBox = "$title — $body")
            publish()
        }

        /**
         * A mini-app drew something.
         *
         * Surfaced as its text projection, for the same reason `speak` is: this
         * module has no Compose tree, and a card that arrives and vanishes makes
         * the box look broken from the phone. The real rendering — a titled
         * panel, a list, two buttons — is the host app's job and needs the
         * Android SDK, which is `APPS-SCOPE.md`'s open row and not this file's.
         *
         * A question is already in [ApprovalQueue] by the time this runs, so it
         * reaches the approvals screen through the ordinary path and can be
         * answered there even before anything draws it properly.
         */
        override fun render(view: AppView.Rendered) {
            _state.value = _state.value.copy(lastFromBox = view.text())
            publish()
        }

        override fun renderRefused(reason: String) {
            // Not surfaced to the user, because there is nothing they can do
            // about a version mismatch from a card screen — but not swallowed
            // either, because "the app does nothing" is otherwise unanswerable.
            Log.w(TAG, "ui.render refused: $reason")
        }

        override fun linkState(state: RelaydLink.State) {
            _state.value = _state.value.copy(
                boxConnection = when (state) {
                    RelaydLink.State.Open -> ConnectionState.Connected
                    RelaydLink.State.Connecting -> ConnectionState.Connecting
                    RelaydLink.State.Reconnecting -> ConnectionState.Reconnecting
                    RelaydLink.State.Idle, RelaydLink.State.Closed -> ConnectionState.Disconnected
                },
            )
            publish()
        }

        override fun linkError(error: LinkException) {
            // Logged rather than surfaced: a refused frame and a dropped socket
            // are both normal on a phone, and a UI that reports every one of
            // them trains people to ignore it.
            Log.w(TAG, "link: ${error.code} ${error.message}")
        }

        override fun sessionsChanged() = CaptureBus.publishSessions(sessions.all())

        override fun approvalsChanged() =
            CaptureBus.publishApprovals(approvals.pending(System.currentTimeMillis()))
    }

    /**
     * Leave proof that capture ran.
     *
     * The OEM problem is not that background services get killed — it is that
     * they get killed *silently*, so the first evidence is a hole in yesterday.
     * [CaptureDiagnostics] turns that hole into something the UI can name, and
     * name the fix for. See `OemPolicy` for who does it and what to tell them.
     */
    private suspend fun heartbeat() {
        while (scope.isActive) {
            diagnostics.beat(captureIntended = preferences.captureEnabled)
            delay(CaptureDiagnostics.BEAT_INTERVAL_MS)
        }
    }

    /**
     * Poll `0x0909` / `0x091C` and keep the storage verdict fresh.
     *
     * Every five minutes, not continuously: the number that matters moves
     * slowly for audio and fast only while video is recording, and a BLE round
     * trip on a loop is a battery cost with no payoff. APPS-SCOPE.md §3.2.
     */
    private suspend fun pollStorage() {
        while (scope.isActive) {
            val probe = transport as? DiskProbe
            if (probe != null) {
                runCatching {
                    StoragePolicy.assess(probe.diskInfo(), probe.inventory())
                }.onSuccess { _storage.value = it }
            }
            delay(STORAGE_POLL_MS)
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopCapture()
                return START_NOT_STICKY
            }
            ACTION_PAUSE_CAPTURE -> {
                scope.launch {
                    supervisor.pauseCapture()
                    recording.pause()
                }
                return START_STICKY
            }
            ACTION_RESUME_CAPTURE -> {
                scope.launch {
                    supervisor.resumeCapture()
                    recording.resume()
                }
                return START_STICKY
            }
            ACTION_CONSENT_YES -> {
                consent.answer(approve = true)
                return START_STICKY
            }
            ACTION_CONSENT_NO -> {
                // A refusal stops a recording in progress, because the gate's
                // verdict is what LocalRecordingController is collecting. It
                // does not stop the service: the link, the battery reading and
                // the notification all stay, and the user can say yes later.
                consent.answer(approve = false)
                return START_STICKY
            }
            ACTION_REVOKE_CONSENT -> {
                consent.revoke()
                stopCapture()
                return START_NOT_STICKY
            }
            ACTION_APPROVE, ACTION_DENY -> {
                val approve = intent?.action == ACTION_APPROVE
                val actionId: String? = intent?.getStringExtra(EXTRA_ACTION_ID)
                if (actionId != null) {
                    // The router refuses a second answer and sends nothing for
                    // one the box has already timed out — that check belongs
                    // there, not in the caller, because there are two callers.
                    router?.answer(actionId, approve = approve)
                }
                return START_STICKY
            }
        }

        val missing = blockingPermissions(this)
        if (missing.isNotEmpty()) {
            // Starting a typed foreground service without the matching
            // permission throws rather than degrading. Fail loudly here rather
            // than crash inside startForeground.
            Log.e(TAG, "refusing to start, missing: $missing")
            stopSelf()
            return START_NOT_STICKY
        }

        promoteToForeground()
        acquireWakeLock()
        // Persisted here rather than in the UI: this is the moment capture
        // actually starts, and BootReceiver reads exactly this flag to decide
        // whether to come back after a reboot. Setting it anywhere else leaves
        // a boot receiver that can never fire — which is what it was before.
        preferences.captureEnabled = true

        // Re-read rather than trusted from onCreate: notification permission
        // can be revoked while the service is alive, and the indicator is a
        // precondition rather than a nicety (ARCHITECTURE.md §6).
        consent.setIndicatorAvailable(canShowIndicator(this))

        // The app came back, or the OS restarted us. Either way this is the
        // moment to collapse a backoff rather than wait it out — see
        // `RelaydLink.wake`. A no-op on a link that is already open.
        link?.wake()

        if (intent?.getBooleanExtra(EXTRA_USER_INITIATED, false) == true) {
            // Someone tapped a button. That is the wearer starting a
            // conversation, and it is the one consent signal the phone can
            // observe for itself — the place and the voices come from the box.
            // A boot restart deliberately does not carry it.
            consent.startSession()
        }

        scope.launch { supervisor.start() }
        return START_STICKY
    }

    /**
     * Take the foreground, declaring only the types actually in use.
     *
     * Two reasons this is not simply `connectedDevice or microphone`, and both
     * bite in production rather than in a test:
     *
     * 1. **We do not use the phone's microphone by default.** The audio comes
     *    off the glasses over BLE, and `AudioRecord` is never opened for
     *    all-day capture. Declaring `microphone` anyway asks the user for a
     *    justification the app cannot make, and puts a permanent microphone
     *    chip in their status bar for a device that is not listening through
     *    the phone. It is added by [micTypeNeeded] only while a phone-side
     *    recorder is genuinely open — the wake-word spotter of
     *    `ARCHITECTURE.md` §5.2b, if that is ever built.
     *
     * 2. **A boot start cannot declare it.** Android restricts which foreground
     *    service types may be started from `BOOT_COMPLETED`, and `microphone`
     *    is on the restricted list for apps targeting recent releases. Starting
     *    one there throws `ForegroundServiceStartNotAllowedException`, which
     *    would turn the boot receiver from a safety net into a crash. Starting
     *    as `connectedDevice` sidesteps it entirely.
     *
     * **Unverified**: the exact restricted-type list per API level has not been
     * checked against the platform on this machine — there is no SDK here.
     * `connectedDevice` alone is the conservative choice under every version of
     * that list, which is why it is the default rather than an optimisation.
     */
    private fun promoteToForeground() {
        val notification = notifications.build(_state.value)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            var types = ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE
            if (micTypeNeeded) types = types or ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE
            startForeground(CaptureNotifications.ID, notification, types)
        } else {
            startForeground(CaptureNotifications.ID, notification)
        }
    }

    /**
     * Re-declare with the microphone type, for a phone-side recorder.
     *
     * Must not be called from a boot start — see [promoteToForeground]. The
     * caller is a user-visible action, which is also the only context in which
     * asking for the microphone is honest.
     */
    fun useMicrophone(needed: Boolean) {
        if (micTypeNeeded == needed) return
        if (needed && ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) !=
            PackageManager.PERMISSION_GRANTED
        ) {
            // Declaring the microphone type without the permission throws
            // rather than degrading. Refuse here, where the caller can react.
            Log.e(TAG, "cannot declare the microphone service type without RECORD_AUDIO")
            return
        }
        micTypeNeeded = needed
        runCatching { promoteToForeground() }
            .onFailure { Log.e(TAG, "could not change foreground service type", it) }
    }

    /**
     * A partial wake lock keeps the CPU available for the BLE callback thread
     * while the screen is off. It does not keep the screen on, and it is released
     * the moment capture stops — an unreleased wake lock is a battery complaint
     * and a one-star review.
     */
    private fun acquireWakeLock() {
        if (wakeLock?.isHeld == true) return
        val power = getSystemService(Context.POWER_SERVICE) as PowerManager
        wakeLock = power.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, WAKE_LOCK_TAG).apply {
            setReferenceCounted(false)
            acquire(WAKE_LOCK_TIMEOUT_MS)
        }
    }

    /** Keeps the notification and the UI reading from one source of truth. */
    private fun publish() {
        notifications.update(_state.value)
        CaptureBus.publish(_state.value)
    }

    private fun stopCapture() {
        preferences.captureEnabled = false
        // Session consent covers one conversation. Letting it survive the stop
        // would mean the next start — including one the user did not ask for —
        // inherits a yes given for something that is over.
        consent.endSession()
        diagnostics.beat(captureIntended = false)
        scope.launch {
            recording.stop()
            supervisor.stop()
        }
        CaptureBus.clear()
        wakeLock?.takeIf { it.isHeld }?.release()
        wakeLock = null
        ServiceCompat_stopForegroundRemove(this)
        stopSelf()
    }

    /**
     * Shut the link down.
     *
     * `RelaydLink.close` keeps whatever is queued: the app being closed is not a
     * reason to throw away an utterance the user already said. What must go is
     * the socket and the scheduler thread — a daemon thread that outlives the
     * service is the leak nobody attributes to the right component.
     */
    private fun closeLink() {
        router?.detach()
        router = null
        link?.close()
        link = null
        linkScheduler?.shutdown()
        linkScheduler = null
    }

    override fun onDestroy() {
        closeLink()
        wakeLock?.takeIf { it.isHeld }?.release()
        wakeLock = null
        scope.cancel()
        super.onDestroy()
    }

    /**
     * Swiping the host task away must not silently end a day of capture — but it
     * also must not resurrect a service the user deliberately stopped. Restart
     * only if capture was actually running.
     */
    override fun onTaskRemoved(rootIntent: Intent?) {
        if (_state.value.recording) {
            val restart = Intent(applicationContext, RelayCaptureService::class.java)
            ContextCompat.startForegroundService(applicationContext, restart)
        }
        super.onTaskRemoved(rootIntent)
    }

    override fun onBind(intent: Intent?): IBinder? = null

    companion object {
        private const val TAG = "RelayCapture"
        private const val WAKE_LOCK_TAG = "relay:capture"

        /** Renewed by the supervisor; a bounded lock cannot leak forever. */
        private const val WAKE_LOCK_TIMEOUT_MS = 12L * 60 * 60 * 1000

        /** Storage moves slowly for audio and fast only while video records. */
        private const val STORAGE_POLL_MS = 5L * 60 * 1000

        const val ACTION_STOP = "glass.relay.bridge.STOP"
        const val ACTION_PAUSE_CAPTURE = "glass.relay.bridge.PAUSE_CAPTURE"
        const val ACTION_RESUME_CAPTURE = "glass.relay.bridge.RESUME_CAPTURE"

        /** Answers the question in `CaptureState.consentQuestion`. */
        const val ACTION_CONSENT_YES = "glass.relay.bridge.CONSENT_YES"
        const val ACTION_CONSENT_NO = "glass.relay.bridge.CONSENT_NO"

        /** Withdraws consent entirely and stops. Forgets every confirmed place. */
        const val ACTION_REVOKE_CONSENT = "glass.relay.bridge.REVOKE_CONSENT"

        /** Answers a `confirm.request` from the box. `ORCHESTRATOR.md` §5 job 3. */
        const val ACTION_APPROVE = "glass.relay.bridge.APPROVE"
        const val ACTION_DENY = "glass.relay.bridge.DENY"
        const val EXTRA_ACTION_ID = "glass.relay.bridge.ACTION_ID"

        /**
         * Set only when a human tapped something.
         *
         * The boot receiver and `onTaskRemoved` deliberately leave it off:
         * neither is the wearer starting a conversation, and treating them as
         * one would make `ConsentPolicy.Scope.Session` mean "always".
         */
        const val EXTRA_USER_INITIATED = "glass.relay.bridge.USER_INITIATED"

        fun pauseCapture(context: Context) = sendAction(context, ACTION_PAUSE_CAPTURE)

        fun resumeCapture(context: Context) = sendAction(context, ACTION_RESUME_CAPTURE)

        /** Answer the pending consent question. Yes covers this place only. */
        fun answerConsent(context: Context, approve: Boolean) =
            sendAction(context, if (approve) ACTION_CONSENT_YES else ACTION_CONSENT_NO)

        fun revokeConsent(context: Context) = sendAction(context, ACTION_REVOKE_CONSENT)

        /** Answer a confirmation the box is blocked on. */
        fun answerApproval(context: Context, actionId: String, approve: Boolean) {
            context.startService(
                Intent(context, RelayCaptureService::class.java)
                    .setAction(if (approve) ACTION_APPROVE else ACTION_DENY)
                    .putExtra(EXTRA_ACTION_ID, actionId),
            )
        }

        private fun sendAction(context: Context, action: String) {
            context.startService(
                Intent(context, RelayCaptureService::class.java).setAction(action),
            )
        }

        /**
         * Whether the bystander-visible recording indicator can be shown.
         *
         * On this platform the indicator is the ongoing notification, so the
         * question is "can we post it": the runtime permission on API 33+, and
         * whether the user has blocked our notifications on any version.
         * `ConsentPolicy.indicatorRequired()` has no off switch, so a false
         * here means capture does not run — see `ConsentGate`.
         */
        fun canShowIndicator(context: Context): Boolean {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
                !granted(context, Manifest.permission.POST_NOTIFICATIONS)
            ) {
                return false
            }
            val manager = context.getSystemService(NotificationManager::class.java)
            return manager?.areNotificationsEnabled() ?: false
        }

        /**
         * Everything the app asks for, granted or not.
         *
         * Use this to decide what to *request*. Use [blockingPermissions] to
         * decide what stops capture — they are not the same list, and treating
         * them as one is how a user who declines the microphone ends up unable
         * to record anything at all.
         */
        fun missingPermissions(context: Context): List<String> =
            (blockingPermissions(context) + optionalPermissions(context)).distinct()

        /**
         * Permissions without which the service cannot legally start.
         *
         * `RECORD_AUDIO` is deliberately **not** here. All-day capture reads
         * audio off the glasses over BLE and never opens `AudioRecord`, and the
         * running service declares `connectedDevice` only — see
         * [promoteToForeground]. The microphone is needed for the phone-side
         * recogniser, which is opt-in, so declining it should cost the voice
         * loop and nothing else.
         */
        fun blockingPermissions(context: Context): List<String> {
            val required = buildList {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                    add(Manifest.permission.BLUETOOTH_CONNECT)
                    add(Manifest.permission.BLUETOOTH_SCAN)
                }
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                    // The ongoing notification is the recording indicator, and
                    // ARCHITECTURE.md §6 makes that non-optional.
                    add(Manifest.permission.POST_NOTIFICATIONS)
                }
            }
            return required.filterNot { granted(context, it) }
        }

        /** Wanted, but capture runs without them. */
        fun optionalPermissions(context: Context): List<String> =
            listOf(Manifest.permission.RECORD_AUDIO).filterNot { granted(context, it) }

        private fun granted(context: Context, permission: String): Boolean =
            ContextCompat.checkSelfPermission(context, permission) == PackageManager.PERMISSION_GRANTED

        /**
         * Start capture.
         *
         * [userInitiated] defaults to false so that a caller has to say so.
         * `BootReceiver` and `onTaskRemoved` are the two that must not — see
         * [EXTRA_USER_INITIATED].
         */
        fun start(context: Context, userInitiated: Boolean = false) {
            val missing = blockingPermissions(context)
            require(missing.isEmpty()) { "grant these before starting capture: $missing" }
            ContextCompat.startForegroundService(
                context,
                Intent(context, RelayCaptureService::class.java)
                    .putExtra(EXTRA_USER_INITIATED, userInitiated),
            )
        }

        fun stop(context: Context) {
            context.startService(
                Intent(context, RelayCaptureService::class.java).setAction(ACTION_STOP),
            )
        }
    }
}

/** `stopForeground(STOP_FOREGROUND_REMOVE)` across API levels. */
private fun ServiceCompat_stopForegroundRemove(service: Service) {
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
        service.stopForeground(Service.STOP_FOREGROUND_REMOVE)
    } else {
        @Suppress("DEPRECATION")
        service.stopForeground(true)
    }
}
