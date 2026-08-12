package glass.relay.bridge.audio

/**
 * What to send, in what order, having already sent some of it.
 *
 * `APPS-SCOPE.md` §4.2 asks for "chunked, resumable upload with ordering and
 * dedupe". `ConnectorClient.upload` does the transport; this does the thinking,
 * so the decisions are testable without a network:
 *
 *  - **Ordering is not cosmetic.** The box segments episodes by time, so chunks
 *    go out in index order and a failure stops the run rather than skipping
 *    ahead. Reordering someone's day is worse than delaying it.
 *  - **Dedupe is unconditional.** A phone that uploaded 39 of 40 chunks before
 *    the train went into a tunnel has no idea that it did. Asking the box what
 *    it already holds costs one small round trip and can save an hour of radio.
 *  - **A resumed upload must not re-send what arrived.** The box answers with
 *    the set it has; anything in that set is skipped, and skipped bytes still
 *    count toward progress or the UI reports 3% for a nearly-finished transfer.
 */
object UploadPlan {

    data class Chunk(val index: Int, val offset: Int, val length: Int)

    data class Plan(
        val toSend: List<Chunk>,
        val skipped: List<Chunk>,
        val totalChunks: Int,
        val totalBytes: Long,
    ) {
        val bytesToSend: Long get() = toSend.sumOf { it.length.toLong() }

        /** Bytes the box already holds. Counted as progress, because it is. */
        val bytesAlreadyThere: Long get() = skipped.sumOf { it.length.toLong() }

        val complete: Boolean get() = toSend.isEmpty()
    }

    /**
     * Split [totalBytes] into [chunkBytes] pieces and drop the ones the box
     * already has.
     *
     * [received] is the box's own answer, not our bookkeeping — the whole point
     * of asking is that our bookkeeping is what the crash destroyed.
     */
    fun forSession(
        totalBytes: Long,
        chunkBytes: Int,
        received: Set<Int> = emptySet(),
    ): Plan {
        require(chunkBytes > 0) { "chunk size must be positive" }
        require(totalBytes >= 0) { "byte count cannot be negative" }

        val totalChunks = if (totalBytes == 0L) 0 else ((totalBytes + chunkBytes - 1) / chunkBytes).toInt()
        val toSend = mutableListOf<Chunk>()
        val skipped = mutableListOf<Chunk>()

        for (index in 0 until totalChunks) {
            val offset = index.toLong() * chunkBytes
            val length = minOf(chunkBytes.toLong(), totalBytes - offset).toInt()
            val chunk = Chunk(index, offset.toInt(), length)
            if (index in received) skipped += chunk else toSend += chunk
        }

        return Plan(toSend, skipped, totalChunks, totalBytes)
    }

    /**
     * Whether to re-encode audio before uploading.
     *
     * `APPS-SCOPE.md` §4.2: "Pass Opus through un-transcoded where possible —
     * re-encoding costs battery and quality." The device already produced Opus
     * at ~24 kbps; decoding and re-encoding it on the phone spends CPU to make
     * it worse. The only honest reason to transcode is a destination that
     * cannot read the source format at all.
     *
     * Written as a function rather than left implicit so that the day someone
     * adds a transcode step, they have to change a line that says why.
     */
    fun shouldTranscode(sourceFormat: String, destinationAccepts: Set<String>): Boolean {
        if (sourceFormat in destinationAccepts) return false
        return true
    }
}
