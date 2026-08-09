package glass.engram.bridge

import android.Manifest
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
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
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
 *    hold the matching permission, and `microphone` additionally requires
 *    RECORD_AUDIO to already be granted at `startForeground` time. Getting this
 *    wrong throws at runtime rather than degrading, so [start] checks first.
 *
 *  - **START_STICKY is not enough.** OEM skins kill and do not restart. The wake
 *    lock, boot receiver and battery-optimisation exemption are all there for the
 *    same reason: staying alive on Android is an integration problem, not an API
 *    call. See [BatteryOptimisation].
 *
 *  - **The notification is not decoration.** It is the user-visible proof that a
 *    device with a camera and microphone is recording. It always reflects real
 *    state and is never silenced.
 */
class EngramCaptureService : Service() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private lateinit var notifications: CaptureNotifications
    private lateinit var supervisor: ConnectionSupervisor
    private var wakeLock: PowerManager.WakeLock? = null

    private val _state = MutableStateFlow(CaptureState.Idle)
    val state: StateFlow<CaptureState> = _state.asStateFlow()

    override fun onCreate() {
        super.onCreate()
        notifications = CaptureNotifications(this)
        supervisor = ConnectionSupervisor(
            transport = TransportProvider.get(this),
            scope = scope,
        )

        scope.launch {
            supervisor.state.collect { connection ->
                _state.value = _state.value.copy(connection = connection)
                notifications.update(_state.value)
            }
        }
        scope.launch {
            supervisor.capture.collect { capture ->
                _state.value = _state.value.copy(
                    recording = capture.recording,
                    worn = capture.worn,
                    batteryPercent = capture.batteryPercent,
                )
                notifications.update(_state.value)
            }
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopCapture()
                return START_NOT_STICKY
            }
            ACTION_PAUSE_CAPTURE -> {
                scope.launch { supervisor.pauseCapture() }
                return START_STICKY
            }
        }

        val missing = missingPermissions(this)
        if (missing.isNotEmpty()) {
            // Starting a `microphone` foreground service without RECORD_AUDIO
            // throws SecurityException on API 34+. Fail loudly here rather than
            // crash inside startForeground.
            Log.e(TAG, "refusing to start, missing: $missing")
            stopSelf()
            return START_NOT_STICKY
        }

        promoteToForeground()
        acquireWakeLock()
        scope.launch { supervisor.start() }
        return START_STICKY
    }

    private fun promoteToForeground() {
        val notification = notifications.build(_state.value)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(
                CaptureNotifications.ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE or
                    ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE,
            )
        } else {
            startForeground(CaptureNotifications.ID, notification)
        }
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

    private fun stopCapture() {
        scope.launch { supervisor.stop() }
        wakeLock?.takeIf { it.isHeld }?.release()
        wakeLock = null
        ServiceCompat_stopForegroundRemove(this)
        stopSelf()
    }

    override fun onDestroy() {
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
            val restart = Intent(applicationContext, EngramCaptureService::class.java)
            ContextCompat.startForegroundService(applicationContext, restart)
        }
        super.onTaskRemoved(rootIntent)
    }

    override fun onBind(intent: Intent?): IBinder? = null

    companion object {
        private const val TAG = "EngramCapture"
        private const val WAKE_LOCK_TAG = "engram:capture"

        /** Renewed by the supervisor; a bounded lock cannot leak forever. */
        private const val WAKE_LOCK_TIMEOUT_MS = 12L * 60 * 60 * 1000

        const val ACTION_STOP = "glass.engram.bridge.STOP"
        const val ACTION_PAUSE_CAPTURE = "glass.engram.bridge.PAUSE_CAPTURE"

        /** Permissions that must be granted before the service can legally start. */
        fun missingPermissions(context: Context): List<String> {
            val required = buildList {
                add(Manifest.permission.RECORD_AUDIO)
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                    add(Manifest.permission.BLUETOOTH_CONNECT)
                    add(Manifest.permission.BLUETOOTH_SCAN)
                }
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                    add(Manifest.permission.POST_NOTIFICATIONS)
                }
            }
            return required.filter {
                ContextCompat.checkSelfPermission(context, it) != PackageManager.PERMISSION_GRANTED
            }
        }

        fun start(context: Context) {
            val missing = missingPermissions(context)
            require(missing.isEmpty()) { "grant these before starting capture: $missing" }
            ContextCompat.startForegroundService(
                context,
                Intent(context, EngramCaptureService::class.java),
            )
        }

        fun stop(context: Context) {
            context.startService(
                Intent(context, EngramCaptureService::class.java).setAction(ACTION_STOP),
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
