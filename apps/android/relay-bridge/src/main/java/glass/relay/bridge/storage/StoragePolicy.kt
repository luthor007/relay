package glass.relay.bridge.storage

import kotlin.math.max
import kotlin.math.min

/**
 * Who gets the glasses' 4 GB, and what happens as it runs out.
 *
 * `APPS-SCOPE.md` §3.2 makes this a requirement rather than a nicety, and the
 * arithmetic is why. The device holds 4 GB, shared between audio, photos and
 * video:
 *
 * | | Rate | 4 GB holds |
 * |---|---|---|
 * | Opus audio ~24 kbps | ~10.8 MB/h | ~370 h |
 * | PCM 16 kHz/16-bit | ~115 MB/h | ~35 h |
 * | 1080p video | **~4.5 GB/h** | ~53 min |
 *
 * So **fourteen minutes of video costs a full day of Opus audio**, and video is
 * the only thing on the device that can consume storage faster than a person
 * can notice. That single fact drives every rule below.
 *
 * ## The rules
 *
 *  1. **Reserve headroom for audio.** [Config.reserveAudioHours] of recording
 *     is set aside; video is measured against what is left over, not against
 *     free space.
 *  2. **Warn before video eats it**, with a number: "this will leave you 3
 *     hours of audio". A percentage bar tells nobody anything useful.
 *  3. **Sync and free proactively**, at [Config.syncAtFreeFraction], not when
 *     the device is full. A sync started at 100% full has nowhere to put the
 *     recording that is still running.
 *  4. **Never propose deleting un-uploaded audio.** `0x0911` 清除未上传文件
 *     exists precisely because the firmware tracks that distinction, and the
 *     existence of that command is not permission to use it. `StoragePolicyTest`
 *     asserts that no assessment, at any level of pressure, ever suggests it.
 *
 * Everything here is arithmetic on a snapshot, so it is testable without a
 * device — which matters, because the interesting cases are the ones you cannot
 * reach on a bench without filling a real 4 GB first.
 */
object StoragePolicy {

    /** Bytes per second, from `SYSTEM.md` §3.1 and `APPS-SCOPE.md` §3.1. */
    object Rates {
        /** Opus ~24 kbps mono, the format the SDK's encoders suggest. */
        const val AUDIO_OPUS: Long = 3_000

        /** PCM 16 kHz 16-bit mono — the pessimistic bound. */
        const val AUDIO_PCM16: Long = 32_000

        /** 1080p, ~4.5 GB/h. The one that ruins a day. */
        const val VIDEO_1080P: Long = 1_342_177

        /** A full-resolution stills capture. Rough, and only used for warnings. */
        const val PHOTO_BYTES: Long = 3L * 1024 * 1024
    }

    const val DEVICE_TOTAL_BYTES: Long = 4L * 1024 * 1024 * 1024

    enum class RecordingFormat(val bytesPerSecond: Long) {
        Opus(Rates.AUDIO_OPUS),
        Pcm16(Rates.AUDIO_PCM16),
    }

    data class Config(
        /**
         * Hours of audio to keep room for at all times.
         *
         * A working day plus slack. In Opus this is ~130 MB of a 4 GB device —
         * cheap insurance against a video session that runs long.
         */
        val reserveAudioHours: Double = 12.0,

        /** Start the sync ritual once free space drops below this fraction. */
        val syncAtFreeFraction: Double = 0.25,

        /**
         * Critical once free space falls below this fraction of the audio
         * reserve.
         *
         * Expressed against the reserve rather than against total capacity on
         * purpose. A flat "8% of the disk" is 327 MB on this device, which is
         * two and a half times the twelve-hour reserve — so a percentage
         * threshold would declare a critical state while there was still half a
         * week of audio headroom, and `Low` would be unreachable. Every level
         * here is measured in hours of audio, because that is the unit the
         * product is denominated in.
         */
        val criticalReserveFraction: Double = 0.25,

        val format: RecordingFormat = RecordingFormat.Opus,
    )

    /** What `0x0909` / `0x091C` reports. */
    data class DiskSnapshot(val totalBytes: Long, val freeBytes: Long) {
        val usedBytes: Long get() = max(0, totalBytes - freeBytes)
        val freeFraction: Double get() = if (totalBytes <= 0) 0.0 else freeBytes.toDouble() / totalBytes
    }

    /**
     * What is on the device, from `0x0E01` 获取文件列表.
     *
     * `uploaded` is the *firmware's* answer, not ours. Trusting our own
     * bookkeeping here is how the only copy of something disappears.
     */
    data class Inventory(
        val unuploadedAudioBytes: Long = 0,
        val uploadedAudioBytes: Long = 0,
        val videoBytes: Long = 0,
        val photoBytes: Long = 0,
    ) {
        val reclaimableBytes: Long get() = uploadedAudioBytes
    }

    enum class Level {
        /** Room for the reserve and then some. */
        Healthy,

        /** Reserve intact, but the sync threshold has been crossed. */
        Watch,

        /** The audio reserve is being eaten. */
        Low,

        /** No room for a working day of audio. */
        Critical,
    }

    sealed interface Action {
        /** Run the two-phase WiFi sync now, while it can still finish. */
        data class SyncNow(val why: String) : Action

        /** Delete audio the box has confirmed it holds. Never the rest. */
        data class FreeUploadedAudio(val bytes: Long) : Action

        /** Video is allowed but will cost this much of the audio reserve. */
        data class WarnBeforeVideo(val minutesAvailable: Int, val audioHoursAfter: Double) : Action

        /** Video would break the audio reserve. Refuse it. */
        data class BlockVideo(val why: String) : Action
    }

    data class Assessment(
        val level: Level,
        val freeBytes: Long,
        /** Hours of audio the remaining space holds at the current format. */
        val audioHoursRemaining: Double,
        /** Minutes of 1080p before the audio reserve is broken. Often small. */
        val videoMinutesBeforeReserveLost: Int,
        val actions: List<Action>,
        val warnings: List<String>,
    ) {
        val videoAllowed: Boolean get() = actions.none { it is Action.BlockVideo }
    }

    fun assess(
        disk: DiskSnapshot,
        inventory: Inventory = Inventory(),
        config: Config = Config(),
    ): Assessment {
        val audioRate = config.format.bytesPerSecond
        val reserveBytes = (config.reserveAudioHours * 3600 * audioRate).toLong()
        val free = max(0, disk.freeBytes)

        val audioHoursRemaining = free.toDouble() / audioRate / 3600.0
        val spareOverReserve = free - reserveBytes
        val videoSecondsBeforeReserveLost = max(0, spareOverReserve) / Rates.VIDEO_1080P
        val videoMinutes = (videoSecondsBeforeReserveLost / 60).toInt()

        val level = when {
            free < reserveBytes * config.criticalReserveFraction -> Level.Critical
            free < reserveBytes -> Level.Low
            free < reserveBytes * 3 || disk.freeFraction <= config.syncAtFreeFraction -> Level.Watch
            else -> Level.Healthy
        }

        val actions = mutableListOf<Action>()
        val warnings = mutableListOf<String>()

        if (level != Level.Healthy) {
            actions += Action.SyncNow(
                when (level) {
                    Level.Critical -> "the glasses are nearly full — sync now or capture will stop"
                    Level.Low -> "free space has dropped into the audio reserve"
                    else -> "free space is below ${(config.syncAtFreeFraction * 100).toInt()}%"
                },
            )
        }

        // The only deletion this policy will ever propose. Audio the box has
        // confirmed it holds is a copy; everything else is an original.
        if (inventory.reclaimableBytes > 0 && level != Level.Healthy) {
            actions += Action.FreeUploadedAudio(inventory.reclaimableBytes)
        }

        if (inventory.unuploadedAudioBytes > 0 && level == Level.Critical) {
            warnings += "there is ${mb(inventory.unuploadedAudioBytes)} of audio the box has " +
                "never seen — it will not be deleted, so sync before recording video"
        }

        when {
            spareOverReserve <= 0 -> actions += Action.BlockVideo(
                "recording video would eat into the ${config.reserveAudioHours.toInt()} hours " +
                    "of audio headroom the day needs",
            )
            videoMinutes < 5 -> actions += Action.WarnBeforeVideo(
                minutesAvailable = videoMinutes,
                audioHoursAfter = audioHoursAfterVideo(free, videoMinutes, audioRate),
            )
            else -> {
                // Even with room, say what it costs: a few minutes of 1080p is
                // measured in hours of audio, and nobody guesses that correctly.
                actions += Action.WarnBeforeVideo(
                    minutesAvailable = videoMinutes,
                    audioHoursAfter = audioHoursAfterVideo(free, min(videoMinutes, 10), audioRate),
                )
            }
        }

        if (inventory.videoBytes > free) {
            warnings += "video already occupies ${mb(inventory.videoBytes)}, more than the " +
                "${mb(free)} still free"
        }

        return Assessment(
            level = level,
            freeBytes = free,
            audioHoursRemaining = audioHoursRemaining,
            videoMinutesBeforeReserveLost = videoMinutes,
            actions = actions,
            warnings = warnings,
        )
    }

    /**
     * How long a minute of 1080p costs in hours of audio.
     *
     * The number the warning copy needs. At 4.5 GB/h against Opus at 10.8 MB/h,
     * one minute of video is a little over seven hours of audio — which is the
     * whole argument for §3.2 in one figure.
     */
    fun audioHoursPerVideoMinute(format: RecordingFormat = RecordingFormat.Opus): Double =
        (Rates.VIDEO_1080P * 60.0) / format.bytesPerSecond / 3600.0

    private fun audioHoursAfterVideo(freeBytes: Long, videoMinutes: Int, audioRate: Long): Double {
        val afterVideo = max(0, freeBytes - videoMinutes * 60L * Rates.VIDEO_1080P)
        return afterVideo.toDouble() / audioRate / 3600.0
    }

    private fun mb(bytes: Long): String = "%.0f MB".format(bytes / 1024.0 / 1024.0)
}
