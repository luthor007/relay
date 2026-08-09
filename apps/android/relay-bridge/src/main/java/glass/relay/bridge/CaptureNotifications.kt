package glass.relay.bridge

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.os.Build

/**
 * The ongoing notification.
 *
 * This is not chrome. A device with a camera and two microphones is recording,
 * and the notification is the user's standing, glanceable proof of it — plus the
 * one-tap way to stop. It always states the truth, including when the link is
 * down but the glasses are still recording to their own storage, which is
 * exactly the case a vaguer "Relay is running" would hide.
 *
 * IMPORTANCE_LOW: permanently visible, never makes a sound. A capture session
 * that pings every reconnect gets muted by the user, and a muted capture
 * indicator is worse than none.
 */
internal class CaptureNotifications(private val context: Context) {

    private val manager =
        context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager

    init {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                context.getString(R.string.relay_channel_name),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = context.getString(R.string.relay_channel_description)
                setShowBadge(false)
                enableVibration(false)
                setSound(null, null)
            }
            manager.createNotificationChannel(channel)
        }
    }

    fun build(state: CaptureState): Notification {
        val stopIntent = PendingIntent.getService(
            context,
            0,
            Intent(context, RelayCaptureService::class.java)
                .setAction(RelayCaptureService.ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )

        val builder = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            Notification.Builder(context, CHANNEL_ID)
        } else {
            @Suppress("DEPRECATION")
            Notification.Builder(context)
        }

        return builder
            .setContentTitle(title(state))
            .setContentText(detail(state))
            .setSmallIcon(R.drawable.ic_relay_capture)
            .setOngoing(true)
            .setShowWhen(false)
            .setOnlyAlertOnce(true)
            .addAction(
                Notification.Action.Builder(
                    null,
                    context.getString(R.string.relay_action_stop),
                    stopIntent,
                ).build(),
            )
            .build()
    }

    fun update(state: CaptureState) = manager.notify(ID, build(state))

    /** Never claims to be recording when it is not, and vice versa. */
    private fun title(state: CaptureState): String = context.getString(
        when {
            state.recording -> R.string.relay_title_recording
            state.connection == ConnectionState.Connected -> R.string.relay_title_connected
            state.connection == ConnectionState.Reconnecting -> R.string.relay_title_reconnecting
            state.connection == ConnectionState.Connecting -> R.string.relay_title_connecting
            else -> R.string.relay_title_idle
        },
    )

    private fun detail(state: CaptureState): String {
        val parts = buildList {
            if (state.recording && state.connection != ConnectionState.Connected) {
                // The important, non-obvious case: recording continues on the
                // glasses themselves while the phone is out of range.
                add(context.getString(R.string.relay_detail_recording_offline))
            }
            if (!state.worn && state.connection == ConnectionState.Connected) {
                add(context.getString(R.string.relay_detail_not_worn))
            }
            state.batteryPercent?.let { add("$it%") }
        }
        return parts.joinToString(" · ").ifEmpty {
            context.getString(R.string.relay_detail_default)
        }
    }

    companion object {
        const val ID = 0x3E61
        private const val CHANNEL_ID = "relay.capture"
    }
}
