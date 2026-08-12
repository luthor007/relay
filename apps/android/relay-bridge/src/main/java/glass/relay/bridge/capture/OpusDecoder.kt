package glass.relay.bridge.capture

import android.media.MediaCodec
import android.media.MediaFormat
import android.util.Log
import java.io.ByteArrayOutputStream
import java.nio.ByteBuffer

/**
 * Opus → PCM16 on the platform decoder.
 *
 * The glasses stream Opus and every speech recogniser wants linear PCM, so this
 * sits between them. It is a separate class from the recogniser because the
 * failure modes are separate: a recogniser that cannot start is a phone
 * problem the user is told about, and a decoder that cannot decode is a
 * firmware or codec-parameter problem that shows up as silence.
 *
 * `MediaCodec` is used synchronously and frame-at-a-time. That is not the
 * fastest way to use it, and it is the right one here: frames arrive at ~20 ms
 * intervals off a BLE link, so throughput was never the constraint, and the
 * asynchronous callback mode would put decode ordering in one place and the
 * pipe write in another for no gain.
 *
 * The codec-delay and seek-preroll values in the initialisation header are the
 * RFC 7845 defaults. If the firmware ever reports its own, they belong here —
 * wrong preroll costs the first ~80 ms of every utterance, which is exactly the
 * part that carries the wake word.
 *
 * Untested against a device: there is no Android SDK in the environment this
 * was written in, and no recorded Opus fixture from the glasses to decode. It
 * is the first thing to check on real hardware, because "the recogniser returns
 * nothing" and "the decoder is misconfigured" look identical from the app.
 */
class OpusDecoder(
    override val sampleRateHz: Int = 16_000,
    override val channelCount: Int = 1,
) : AudioDecoding {

    private var codec: MediaCodec? = null

    private fun codec(): MediaCodec? {
        codec?.let { return it }
        return try {
            val format = MediaFormat.createAudioFormat(
                MediaFormat.MIMETYPE_AUDIO_OPUS, sampleRateHz, channelCount,
            ).apply {
                // Opus needs three csd buffers: the identification header, the
                // codec delay and the seek preroll. Omitting them is the
                // classic way to get a decoder that configures and then
                // produces nothing.
                setByteBuffer("csd-0", identificationHeader())
                setByteBuffer("csd-1", longLe(DEFAULT_CODEC_DELAY_NS))
                setByteBuffer("csd-2", longLe(DEFAULT_SEEK_PREROLL_NS))
            }
            MediaCodec.createDecoderByType(MediaFormat.MIMETYPE_AUDIO_OPUS).also {
                it.configure(format, null, null, 0)
                it.start()
                codec = it
            }
        } catch (e: Exception) {
            Log.e(TAG, "no Opus decoder on this device: ${e.message}")
            null
        }
    }

    override fun toPcm16(frame: AudioFrame): ByteArray? {
        // Already PCM — nothing to do, and transcoding it would be a round trip
        // through a lossy codec for no reason.
        if (frame.format == AudioFrame.PCM16) return frame.data
        if (frame.format != AudioFrame.OPUS) return null

        val mc = codec() ?: return null
        return try {
            decode(mc, frame.data)
        } catch (e: Exception) {
            Log.e(TAG, "decode failed on frame ${frame.sequence}: ${e.message}")
            null
        }
    }

    private fun decode(mc: MediaCodec, packet: ByteArray): ByteArray? {
        val inIndex = mc.dequeueInputBuffer(TIMEOUT_US)
        if (inIndex < 0) return null
        mc.getInputBuffer(inIndex)?.apply {
            clear()
            put(packet)
        }
        mc.queueInputBuffer(inIndex, 0, packet.size, 0, 0)

        val out = ByteArrayOutputStream()
        val info = MediaCodec.BufferInfo()
        while (true) {
            val outIndex = mc.dequeueOutputBuffer(info, TIMEOUT_US)
            if (outIndex < 0) break
            mc.getOutputBuffer(outIndex)?.let { buf ->
                val chunk = ByteArray(info.size)
                buf.position(info.offset)
                buf.get(chunk)
                out.write(chunk)
            }
            mc.releaseOutputBuffer(outIndex, false)
        }
        return out.toByteArray().takeIf { it.isNotEmpty() }
    }

    /** Frees the codec. Safe to call twice. */
    fun close() {
        try {
            codec?.stop()
            codec?.release()
        } catch (_: Exception) {
        }
        codec = null
    }

    /**
     * The 19-byte OpusHead identification header (RFC 7845 §5.1), built rather
     * than hard-coded so the sample rate and channel count cannot drift from
     * the ones the pipe is described with.
     */
    private fun identificationHeader(): ByteBuffer {
        val b = ByteBuffer.allocate(19)
        b.put("OpusHead".toByteArray(Charsets.US_ASCII))
        b.put(1)                                  // version
        b.put(channelCount.toByte())
        b.put(shortLe(DEFAULT_PRE_SKIP))          // pre-skip
        b.put(intLe(sampleRateHz))                // original sample rate
        b.put(shortLe(0))                         // output gain
        b.put(0)                                  // channel mapping family
        b.rewind()
        return b
    }

    private fun shortLe(v: Int) = byteArrayOf((v and 0xFF).toByte(), ((v shr 8) and 0xFF).toByte())

    private fun intLe(v: Int) = byteArrayOf(
        (v and 0xFF).toByte(), ((v shr 8) and 0xFF).toByte(),
        ((v shr 16) and 0xFF).toByte(), ((v shr 24) and 0xFF).toByte(),
    )

    private fun longLe(v: Long): ByteBuffer =
        ByteBuffer.allocate(8).apply {
            for (i in 0 until 8) put(((v shr (8 * i)) and 0xFF).toByte())
            rewind()
        }

    private companion object {
        const val TAG = "RelayOpus"
        const val TIMEOUT_US = 10_000L
        const val DEFAULT_PRE_SKIP = 3840
        const val DEFAULT_CODEC_DELAY_NS = 6_500_000L
        const val DEFAULT_SEEK_PREROLL_NS = 80_000_000L
    }
}
