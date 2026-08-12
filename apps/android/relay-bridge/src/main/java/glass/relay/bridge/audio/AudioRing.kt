package glass.relay.bridge.audio

import glass.relay.bridge.capture.AudioFrame

/**
 * The bounded buffer between the BLE callback thread and whatever is slower.
 *
 * `APPS-SCOPE.md` §4.2: "Ring buffer with backpressure; never drop silently."
 * The second half is the requirement. Audio arrives on the SDK's callback
 * thread at ~3 KB/s and cannot be made to wait — suspending there stalls the
 * BLE stack — so when the consumer falls behind, something has to give. The
 * only unacceptable answer is giving quietly.
 *
 * ## The policy, stated
 *
 * | Situation | What happens | Why |
 * |---|---|---|
 * | Below [Config.highWaterFrames] | accepted | — |
 * | At or above it | accepted, `shouldThrottle` goes true | the producer can ask the device to slow before anything is lost |
 * | Full, [Overflow.Refuse] | the **newest** frame is refused, counted, reported | matches the store-and-forward queue: refusing is a state the UI can report, losing the oldest is not |
 * | Full, [Overflow.DropOldest] | the oldest frames go, and a [Gap] is recorded | for the live path, where the newest audio is the sentence being spoken |
 *
 * A dropped frame under `DropOldest` leaves a [Gap] carrying the sequence range
 * and the byte count. That matters more than the counter: without it the
 * consumer splices the remaining frames together and produces a transcript that
 * reads as continuous and is not. With it, the box can mark the hole.
 *
 * Not thread-safe by itself — one producer, one consumer, and the service owns
 * the handoff. Synchronising here would put a lock on the BLE callback thread.
 */
class AudioRing(private val config: Config = Config()) {

    data class Config(
        /** Frames held before the newest is refused or the oldest is dropped. */
        val capacityFrames: Int = 512,

        /** Where the producer should start asking the device to slow down. */
        val highWaterFrames: Int = 384,

        /** Where `shouldThrottle` goes back to false. Hysteresis, not a cliff. */
        val lowWaterFrames: Int = 128,

        val overflow: Overflow = Overflow.Refuse,
    ) {
        init {
            require(capacityFrames > 0) { "capacity must be positive" }
            require(highWaterFrames in 1..capacityFrames) { "high water must sit inside the ring" }
            require(lowWaterFrames in 0 until highWaterFrames) { "low water must sit below high water" }
        }
    }

    enum class Overflow {
        /** Refuse the newest. The default, and the queue's policy. */
        Refuse,

        /**
         * Drop the oldest and record a [Gap].
         *
         * Only correct for the live voice loop, where the useful audio is the
         * words being said right now. Never for stored capture: that buffer
         * holds the only copy of something that already happened.
         */
        DropOldest,
    }

    sealed interface Result {
        data object Accepted : Result

        /** Accepted, but the producer should slow the device down. */
        data object AcceptedThrottle : Result

        /** Refused. The caller still owns the frame and must not discard it. */
        data class Refused(val sequence: Int) : Result

        /** Accepted after evicting [gap]. Never silent. */
        data class AcceptedWithGap(val gap: Gap) : Result
    }

    /** A hole in the audio, with enough detail for the box to mark it. */
    data class Gap(
        val fromSequence: Int,
        val toSequence: Int,
        val frames: Int,
        val bytes: Int,
        val atMs: Long,
    )

    private val frames = ArrayDeque<AudioFrame>()
    private var bytes = 0L
    private var throttling = false

    val size: Int get() = frames.size
    val usedBytes: Long get() = bytes

    /** True while the producer should be asking the device to send less. */
    val shouldThrottle: Boolean get() = throttling

    /** Every hole, in order. Empty is the healthy state. */
    val gaps: MutableList<Gap> = mutableListOf()

    var refusedFrames: Int = 0
        private set

    var droppedFrames: Int = 0
        private set

    fun offer(frame: AudioFrame): Result {
        if (frames.size >= config.capacityFrames) {
            return when (config.overflow) {
                Overflow.Refuse -> {
                    refusedFrames += 1
                    Result.Refused(frame.sequence)
                }
                Overflow.DropOldest -> {
                    val gap = evictOldest(frame.deviceTimeMs)
                    push(frame)
                    Result.AcceptedWithGap(gap)
                }
            }
        }

        push(frame)
        if (frames.size >= config.highWaterFrames) throttling = true
        return if (throttling) Result.AcceptedThrottle else Result.Accepted
    }

    /** Take up to [max] frames, oldest first. */
    fun drain(max: Int = Int.MAX_VALUE): List<AudioFrame> {
        val taken = mutableListOf<AudioFrame>()
        while (taken.size < max && frames.isNotEmpty()) {
            val frame = frames.removeFirst()
            bytes -= frame.data.size
            taken += frame
        }
        if (frames.size <= config.lowWaterFrames) throttling = false
        return taken
    }

    fun clear() {
        frames.clear()
        bytes = 0
        throttling = false
    }

    private fun push(frame: AudioFrame) {
        frames.addLast(frame)
        bytes += frame.data.size
    }

    /**
     * Evict enough of the oldest to make room, as one recorded gap.
     *
     * A batch rather than one frame at a time: a consumer that has fallen this
     * far behind will not catch up within a frame, and a thousand one-frame
     * gaps is not a description of anything.
     */
    private fun evictOldest(atMs: Long): Gap {
        val target = maxOf(1, config.capacityFrames - config.lowWaterFrames)
        var evictedBytes = 0
        var evicted = 0
        var first = -1
        var last = -1
        while (evicted < target) {
            val frame = frames.removeFirstOrNull() ?: break
            if (first < 0) first = frame.sequence
            last = frame.sequence
            evictedBytes += frame.data.size
            bytes -= frame.data.size
            evicted += 1
            droppedFrames += 1
        }
        val gap = Gap(
            fromSequence = first,
            toSequence = last,
            frames = evicted,
            bytes = evictedBytes,
            atMs = atMs,
        )
        gaps += gap
        return gap
    }
}
