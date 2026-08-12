package glass.relay.bridge.storage

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The 4 GB, and the arithmetic that makes video dangerous.
 */
class StoragePolicyTest {

    private val gb = 1024L * 1024 * 1024

    private fun disk(freeGb: Double) = StoragePolicy.DiskSnapshot(
        totalBytes = StoragePolicy.DEVICE_TOTAL_BYTES,
        freeBytes = (freeGb * gb).toLong(),
    )

    @Test
    fun `an empty device is healthy and allows video`() {
        val assessment = StoragePolicy.assess(disk(3.8))
        assertEquals(StoragePolicy.Level.Healthy, assessment.level)
        assertTrue(assessment.videoAllowed)
        assertTrue(assessment.actions.none { it is StoragePolicy.Action.SyncNow })
    }

    @Test
    fun `a minute of 1080p costs seven hours of Opus`() {
        // This one number is the whole of APPS-SCOPE.md §3.2. 4.5 GB/h against
        // ~10.8 MB/h is about 124:1, so one minute of video is a working day's
        // worth of audio minus a bit.
        val hours = StoragePolicy.audioHoursPerVideoMinute()
        assertTrue("expected ~7 h, got $hours", hours > 7.0 && hours < 7.9)
    }

    @Test
    fun `free space is reported as hours of audio, which is what a person can act on`() {
        val assessment = StoragePolicy.assess(disk(1.0))
        // 1 GB of Opus at 3 KB/s is about 99 hours.
        assertTrue(
            "expected ~99 h, got ${assessment.audioHoursRemaining}",
            assessment.audioHoursRemaining > 95 && assessment.audioHoursRemaining < 105,
        )
    }

    @Test
    fun `PCM changes the picture by an order of magnitude`() {
        // The open question in APPS-SCOPE.md §8. If the device records PCM
        // rather than Opus, everything here moves by about 10x — so the policy
        // takes the format as an input rather than assuming the good case.
        val opus = StoragePolicy.assess(disk(1.0))
        val pcm = StoragePolicy.assess(
            disk(1.0),
            config = StoragePolicy.Config(format = StoragePolicy.RecordingFormat.Pcm16),
        )
        assertTrue(opus.audioHoursRemaining > pcm.audioHoursRemaining * 9)
    }

    @Test
    fun `crossing the sync threshold asks for a sync before the device is full`() {
        // A sync started at 100% full has nowhere to put the recording that is
        // still running.
        val assessment = StoragePolicy.assess(disk(0.8))
        assertEquals(StoragePolicy.Level.Watch, assessment.level)
        assertTrue(assessment.actions.any { it is StoragePolicy.Action.SyncNow })
    }

    @Test
    fun `eating into the audio reserve blocks video, not audio`() {
        // 12 h of Opus is ~130 MB. Free space below that is Low.
        val assessment = StoragePolicy.assess(
            StoragePolicy.DiskSnapshot(StoragePolicy.DEVICE_TOTAL_BYTES, 100L * 1024 * 1024),
        )
        assertEquals(StoragePolicy.Level.Low, assessment.level)
        assertFalse("video must be refused before the audio reserve is gone", assessment.videoAllowed)
        assertTrue(assessment.actions.any { it is StoragePolicy.Action.SyncNow })
    }

    @Test
    fun `a nearly full device is critical`() {
        val assessment = StoragePolicy.assess(
            StoragePolicy.DiskSnapshot(StoragePolicy.DEVICE_TOTAL_BYTES, 20L * 1024 * 1024),
        )
        assertEquals(StoragePolicy.Level.Critical, assessment.level)
        assertFalse(assessment.videoAllowed)
    }

    @Test
    fun `the video warning names minutes and what they cost`() {
        val assessment = StoragePolicy.assess(disk(2.0))
        val warning = assessment.actions.filterIsInstance<StoragePolicy.Action.WarnBeforeVideo>().single()

        // 2 GB minus the 12 h reserve, at 4.5 GB/h, is roughly 25 minutes.
        assertTrue("got ${warning.minutesAvailable} minutes", warning.minutesAvailable in 20..30)
        assertTrue("a warning with no consequence is not a warning", warning.audioHoursAfter >= 0)
    }

    @Test
    fun `no level of pressure ever proposes deleting un-uploaded audio`() {
        // 0x0911 exists precisely because the firmware tracks the distinction,
        // and its existence is not permission. This is the single most important
        // assertion in the file.
        val inventory = StoragePolicy.Inventory(
            unuploadedAudioBytes = 900L * 1024 * 1024,
            uploadedAudioBytes = 0,
            videoBytes = 3L * gb,
        )
        for (freeMb in listOf(2000L, 500L, 120L, 40L, 1L)) {
            val assessment = StoragePolicy.assess(
                StoragePolicy.DiskSnapshot(StoragePolicy.DEVICE_TOTAL_BYTES, freeMb * 1024 * 1024),
                inventory,
            )
            val freeing = assessment.actions.filterIsInstance<StoragePolicy.Action.FreeUploadedAudio>()
            assertTrue(
                "at ${freeMb}MB free the policy proposed freeing ${freeing.firstOrNull()?.bytes} " +
                    "bytes with nothing uploaded",
                freeing.all { it.bytes <= inventory.uploadedAudioBytes },
            )
        }
    }

    @Test
    fun `audio the box has confirmed is the only thing offered for deletion`() {
        val inventory = StoragePolicy.Inventory(
            unuploadedAudioBytes = 100L * 1024 * 1024,
            uploadedAudioBytes = 400L * 1024 * 1024,
        )
        val assessment = StoragePolicy.assess(disk(0.5), inventory)
        val freeing = assessment.actions.filterIsInstance<StoragePolicy.Action.FreeUploadedAudio>().single()
        assertEquals(inventory.uploadedAudioBytes, freeing.bytes)
    }

    @Test
    fun `a critical device with un-synced audio says what will not be deleted`() {
        val assessment = StoragePolicy.assess(
            StoragePolicy.DiskSnapshot(StoragePolicy.DEVICE_TOTAL_BYTES, 10L * 1024 * 1024),
            StoragePolicy.Inventory(unuploadedAudioBytes = 200L * 1024 * 1024),
        )
        assertTrue(
            assessment.warnings.any { it.contains("will not be deleted") },
        )
    }

    @Test
    fun `video already dominating the device is called out`() {
        val assessment = StoragePolicy.assess(
            disk(0.4),
            StoragePolicy.Inventory(videoBytes = 3L * gb),
        )
        assertTrue(assessment.warnings.any { it.contains("video already occupies") })
    }
}
