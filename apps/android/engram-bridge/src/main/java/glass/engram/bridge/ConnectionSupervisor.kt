package glass.engram.bridge

import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlin.math.min
import kotlin.random.Random

/**
 * Keeps a link to the glasses, and keeps capture running across the ways it fails.
 *
 * A day of real use is not one connection. The user walks out of range, the phone
 * sleeps, the glasses come off and go back on, Bluetooth flaps. The supervisor's
 * job is that none of that requires the user to notice.
 *
 * Two decisions worth stating:
 *
 *  - **Backoff is capped and jittered.** Uncapped exponential backoff means a
 *    device that dropped at 09:00 is retrying every 20 minutes by lunch, so a
 *    user who walks back to their desk waits ages for nothing. Capped at 30s.
 *    The jitter matters because reconnection is often triggered by a *shared*
 *    cause (Bluetooth toggled, phone woke) across every app on the device.
 *
 *  - **Capture follows wear, not the connection.** Reconnecting does not restart
 *    recording — the glasses keep recording to their own storage regardless of
 *    whether the phone is listening. That is the whole reason capture lives on
 *    the device: an out-of-range phone must not create a hole in the day.
 */
class ConnectionSupervisor(
    private val transport: GlassesTransport,
    private val scope: CoroutineScope,
) {

    private val _state = MutableStateFlow(ConnectionState.Disconnected)
    val state: StateFlow<ConnectionState> = _state.asStateFlow()

    private val _capture = MutableStateFlow(CaptureSnapshot())
    val capture: StateFlow<CaptureSnapshot> = _capture.asStateFlow()

    private var supervisorJob: Job? = null
    private var attempt = 0
    private var captureAllowed = true

    fun start() {
        if (supervisorJob?.isActive == true) return
        supervisorJob = scope.launch {
            observeEvents()
            maintainConnection()
        }
    }

    suspend fun stop() {
        supervisorJob?.cancel()
        supervisorJob = null
        runCatching { transport.stopLocalRecording() }
        runCatching { transport.disconnect() }
        _state.value = ConnectionState.Disconnected
        _capture.value = CaptureSnapshot()
    }

    /** User-initiated pause. Survives reconnects until capture is resumed. */
    suspend fun pauseCapture() {
        captureAllowed = false
        runCatching { transport.stopLocalRecording() }
        _capture.value = _capture.value.copy(recording = false)
    }

    suspend fun resumeCapture() {
        captureAllowed = true
        if (_capture.value.worn) startRecordingIfAllowed()
    }

    private fun observeEvents() {
        scope.launch {
            transport.events.collect { event ->
                when (event) {
                    is GlassesEvent.Wear -> {
                        _capture.value = _capture.value.copy(worn = event.worn)
                        if (event.worn) startRecordingIfAllowed() else stopRecording()
                    }
                    is GlassesEvent.Battery -> {
                        _capture.value = _capture.value.copy(
                            batteryPercent = event.percent,
                            charging = event.charging,
                        )
                    }
                    is GlassesEvent.RecordingState -> {
                        _capture.value = _capture.value.copy(recording = event.recording)
                    }
                    is GlassesEvent.Disconnected -> {
                        // Do not clear `recording`: the glasses are still writing
                        // to their own storage. Only our visibility was lost.
                        _state.value = ConnectionState.Reconnecting
                    }
                    else -> Unit
                }
            }
        }
    }

    private suspend fun maintainConnection() {
        while (scope.isActive) {
            if (_state.value == ConnectionState.Connected) {
                delay(HEARTBEAT_INTERVAL_MS)
                if (!runCatching { transport.heartbeat() }.getOrDefault(false)) {
                    Log.w(TAG, "heartbeat failed, dropping link")
                    _state.value = ConnectionState.Reconnecting
                }
                continue
            }

            _state.value = if (attempt == 0) ConnectionState.Connecting else ConnectionState.Reconnecting

            val connected = runCatching { transport.connect() }
                .onFailure { Log.w(TAG, "connect failed: ${it.message}") }
                .getOrDefault(false)

            if (connected) {
                attempt = 0
                _state.value = ConnectionState.Connected
                onConnected()
            } else {
                val wait = backoffMs(attempt++)
                Log.d(TAG, "retry in ${wait}ms (attempt $attempt)")
                delay(wait)
            }
        }
    }

    private suspend fun onConnected() {
        // Capability gate first: firmware revisions differ in what they honour,
        // and issuing an unsupported command is how you get a silent no-op.
        runCatching { transport.features() }
            .onSuccess { _capture.value = _capture.value.copy(features = it) }

        // The device clock drifts. Every transcript timestamp depends on this.
        runCatching { transport.syncTime() }

        runCatching { transport.battery() }.getOrNull()?.let {
            _capture.value = _capture.value.copy(
                batteryPercent = it.percent,
                charging = it.charging,
            )
        }

        if (_capture.value.worn) startRecordingIfAllowed()
    }

    private suspend fun startRecordingIfAllowed() {
        if (!captureAllowed) return
        if (_capture.value.recording) return
        runCatching { transport.startLocalRecording() }
            .onSuccess { _capture.value = _capture.value.copy(recording = true) }
            .onFailure { Log.w(TAG, "could not start recording: ${it.message}") }
    }

    private suspend fun stopRecording() {
        if (!_capture.value.recording) return
        runCatching { transport.stopLocalRecording() }
        _capture.value = _capture.value.copy(recording = false)
    }

    internal companion object {
        private const val TAG = "EngramSupervisor"
        private const val HEARTBEAT_INTERVAL_MS = 30_000L
        private const val BASE_BACKOFF_MS = 1_000L
        private const val MAX_BACKOFF_MS = 30_000L

        /**
         * Exponential with full jitter, capped. Exposed for tests — reconnection
         * timing is the kind of thing that silently regresses.
         */
        fun backoffMs(attempt: Int, random: Random = Random.Default): Long {
            val ceiling = min(MAX_BACKOFF_MS, BASE_BACKOFF_MS shl min(attempt, 16))
            return random.nextLong(BASE_BACKOFF_MS, ceiling + 1)
        }
    }
}

enum class ConnectionState { Disconnected, Connecting, Connected, Reconnecting }

data class CaptureSnapshot(
    val worn: Boolean = false,
    val recording: Boolean = false,
    val batteryPercent: Int? = null,
    val charging: Boolean = false,
    val features: Features? = null,
)

data class CaptureState(
    val connection: ConnectionState = ConnectionState.Disconnected,
    val recording: Boolean = false,
    val worn: Boolean = false,
    val batteryPercent: Int? = null,
) {
    companion object {
        val Idle = CaptureState()
    }
}
