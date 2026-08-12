package glass.relay.bridge.capture

import android.content.Context
import android.content.Intent
import android.media.AudioFormat
import android.os.Build
import android.os.Bundle
import android.os.ParcelFileDescriptor
import android.speech.RecognitionListener
import android.speech.RecognizerIntent
import android.speech.SpeechRecognizer
import android.util.Log
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.OutputStream

/**
 * The real recogniser, on the phone's own speech stack.
 *
 * ### Why this is not the obvious three lines
 *
 * `SpeechRecognizer` normally opens the device microphone itself, and that is
 * the wrong microphone: our audio arrives over BLE from the glasses. Feeding it
 * external audio needs `RecognizerIntent.EXTRA_AUDIO_SOURCE`, a
 * `ParcelFileDescriptor` the service reads instead of the mic — **API 33
 * (Android 13) and later**. Below that there is no supported way to hand this
 * API somebody else's audio, so [isSupported] is false and the caller is told,
 * rather than the recogniser quietly transcribing the phone's own microphone.
 * That failure would be worse than none: it would work in a demo, held next to
 * the speaker, and capture the wrong room in real use.
 *
 * ### The other thing that is not optional
 *
 * The pipe carries **PCM**, and the glasses stream Opus. [AudioDecoding] is the
 * seam; [OpusDecoder] is the implementation. A recogniser fed Opus bytes
 * described as PCM does not error — it transcribes static and returns nothing,
 * which is indistinguishable from a quiet room. Hence [PassthroughPcm]
 * refusing rather than guessing, and hence the decoder being a constructor
 * argument rather than a detail.
 *
 * ### Offline
 *
 * `EXTRA_PREFER_OFFLINE` is set. `PRODUCT.md`'s promise is that the audio is
 * the user's; sending every utterance to a cloud recogniser by default is a
 * different product. Where no offline model is installed the platform falls
 * back on its own, and [RecognitionPolicy.fromAndroidError] turns the network
 * errors into a sentence a person can act on.
 *
 * Untested against a device — no Android SDK in the environment this was
 * written in. The parts that could be tested without one are in
 * `Recognition.kt` and are.
 */
class AndroidSpeechRecognizer(
    private val context: Context,
    private val decoder: AudioDecoding = PassthroughPcm(),
    /**
     * Partial transcripts, on the recogniser's thread. `SYSTEM.md` §7b: these
     * exist so the prompt is ready the moment somebody stops talking.
     */
    private val onPartial: (String) -> Unit = {},
) : SpeechRecognizing {

    private var recognizer: SpeechRecognizer? = null
    private var pipe: OutputStream? = null
    private var result: CompletableDeferred<Outcome>? = null

    private val partials = mutableListOf<String>()
    private var lastEmitted = ""

    /** False on API < 33, where external audio cannot be supplied. */
    val isSupported: Boolean
        get() = Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            SpeechRecognizer.isRecognitionAvailable(context)

    override fun start() {
        check(isSupported) {
            "speech recognition needs Android 13 or later on this path: the glasses' " +
                "audio has to be handed to the recogniser, and older versions only " +
                "let it open the phone's own microphone"
        }
        cancel()

        val (read, write) = ParcelFileDescriptor.createPipe()
        pipe = ParcelFileDescriptor.AutoCloseOutputStream(write)

        val done = CompletableDeferred<Outcome>()
        result = done
        partials.clear()
        lastEmitted = ""

        val sr = SpeechRecognizer.createSpeechRecognizer(context)
        recognizer = sr
        sr.setRecognitionListener(listener(done))
        sr.startListening(intentFor(read))
    }

    private fun intentFor(read: ParcelFileDescriptor): Intent =
        Intent(RecognizerIntent.ACTION_RECOGNIZE_SPEECH).apply {
            putExtra(
                RecognizerIntent.EXTRA_LANGUAGE_MODEL,
                RecognizerIntent.LANGUAGE_MODEL_FREE_FORM,
            )
            putExtra(RecognizerIntent.EXTRA_PARTIAL_RESULTS, true)
            // The audio is the user's. See the class comment.
            putExtra(RecognizerIntent.EXTRA_PREFER_OFFLINE, true)

            putExtra(RecognizerIntent.EXTRA_AUDIO_SOURCE, read)
            putExtra(RecognizerIntent.EXTRA_AUDIO_SOURCE_ENCODING, AudioFormat.ENCODING_PCM_16BIT)
            putExtra(RecognizerIntent.EXTRA_AUDIO_SOURCE_SAMPLING_RATE, decoder.sampleRateHz)
            putExtra(RecognizerIntent.EXTRA_AUDIO_SOURCE_CHANNEL_COUNT, decoder.channelCount)
        }

    private fun listener(done: CompletableDeferred<Outcome>) = object : RecognitionListener {
        override fun onPartialResults(results: Bundle?) {
            val text = firstOf(results) ?: return
            partials += text
            val best = RecognitionPolicy.best(partials)
            if (RecognitionPolicy.shouldEmit(lastEmitted, best)) {
                lastEmitted = best
                onPartial(best)
            }
        }

        override fun onResults(results: Bundle?) {
            val text = firstOf(results) ?: RecognitionPolicy.best(partials)
            done.complete(Outcome.Heard(text.trim()))
        }

        override fun onError(error: Int) {
            // Not every code is a failure — a turn where nobody spoke ends
            // here, and the user must not be apologised at for it.
            val outcome = RecognitionPolicy.fromAndroidError(error)
            if (outcome is Outcome.Unavailable) {
                Log.w(TAG, "speech recognition: ${outcome.reason}")
            }
            done.complete(outcome)
        }

        override fun onReadyForSpeech(params: Bundle?) = Unit
        override fun onBeginningOfSpeech() = Unit
        override fun onRmsChanged(rmsdB: Float) = Unit
        override fun onBufferReceived(buffer: ByteArray?) = Unit
        override fun onEndOfSpeech() = Unit
        override fun onEvent(eventType: Int, params: Bundle?) = Unit
    }

    private fun firstOf(results: Bundle?): String? =
        results?.getStringArrayList(SpeechRecognizer.RESULTS_RECOGNITION)?.firstOrNull()

    override fun append(frame: AudioFrame) {
        val out = pipe ?: return
        val pcm = decoder.toPcm16(frame)
        if (pcm == null) {
            // Loud, because the symptom is silence. A frame the decoder
            // refused is a frame the recogniser will never hear, and a turn
            // made entirely of them returns "" — which reads as a quiet room.
            Log.e(TAG, "dropped ${frame.format} frame ${frame.sequence}: no decoder for it")
            return
        }
        try {
            out.write(pcm)
        } catch (e: Exception) {
            // The recogniser closed its end — it has already decided the turn
            // is over. Nothing to recover, and the outcome is on its way.
            Log.d(TAG, "audio pipe closed mid-turn: ${e.message}")
            pipe = null
        }
    }

    override suspend fun finish(): String {
        val done = result ?: return ""
        // Closing the write end is how the service learns the utterance ended;
        // there is no other signal on this path.
        closePipe()
        val outcome = withContext(Dispatchers.IO) { done.await() }
        release()
        return when (outcome) {
            is Outcome.Heard -> outcome.text
            // An empty string is "nothing was said". A broken recogniser is not
            // that, and the caller has already been told through the log; this
            // returns empty rather than inventing words either way.
            is Outcome.Unavailable -> ""
            Outcome.Cancelled -> ""
        }
    }

    override fun cancel() {
        result?.complete(Outcome.Cancelled)
        closePipe()
        release()
    }

    private fun closePipe() {
        try {
            pipe?.close()
        } catch (_: Exception) {
        }
        pipe = null
    }

    private fun release() {
        recognizer?.let {
            try {
                it.cancel()
                it.destroy()
            } catch (_: Exception) {
            }
        }
        recognizer = null
        result = null
    }

    private companion object {
        const val TAG = "RelayASR"
    }
}
