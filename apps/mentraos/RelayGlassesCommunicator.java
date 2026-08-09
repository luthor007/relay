package com.augmentos.augmentos_core.smarterglassesmanager.smartglassescommunicators;

import android.content.Context;
import android.util.Log;

import com.augmentos.augmentos_core.smarterglassesmanager.supportedglasses.SmartGlassesDevice;

/**
 * MentraOS driver for Relay One (Shenzhen QC.wireless M01 Pro class hardware).
 *
 * MentraOS integrates new hardware through a SmartGlassesCommunicator: the cloud
 * talks to the phone app, the phone app talks to an SGC, and the SGC talks to the
 * glasses. Adding a device means implementing one of these, registering it in
 * SmartGlassesManager, adding an entry to SmartGlassesOperatingSystem, and
 * wiring the model into the mobile app's picker.
 *
 * WHAT THIS FILE IS
 * -----------------
 * The Relay side of that contract, written against our own protocol work in
 * `glasses/protocol/` — the framing, the CRC-16/MODBUS variant the vendor spec
 * gets wrong, and the 92 command IDs. Every byte we send here is already covered
 * by tests in this repo.
 *
 * WHAT IT IS NOT
 * --------------
 * Compiled or merged. The superclass, its abstract members and the exact
 * registration points live in the MentraOS tree
 * (github.com/Mentra-Community/MentraOS, MIT), so this cannot build standing
 * alone and the signatures below will need reconciling against theirs before a
 * PR. It is deliberately checked in anyway: the protocol knowledge is the hard
 * part and it is portable, whereas the glue is mechanical.
 *
 * Do not describe Relay One as "MentraOS compatible" anywhere a customer can
 * read it until this is merged and someone has run it on real hardware.
 *
 * Upstream path, per their contributing guide:
 *   1. this file under android_core/.../smartglassescommunicators/
 *   2. a RelayOne entry in supportedglasses/
 *   3. a case in SmartGlassesRepresentative.createCommunicator()
 *   4. the model added to the mobile app's glasses list
 */
public class RelayGlassesCommunicator extends SmartGlassesCommunicator {

    private static final String TAG = "RelayGlassesCommunicator";

    // Vendor GATT surface. Verified against the shipping iOS SDK headers, not
    // guessed: one proprietary service, notify from the device, write to it.
    public static final String SERVICE_UUID = "01000100-0000-2000-8000-009078563412";
    public static final String NOTIFY_UUID  = "02000200-0000-2000-8000-009178563412";
    public static final String WRITE_UUID   = "03000300-0000-2000-8000-009278563412";

    // The 0xFF manufacturer payload starts with "HSC" rather than a Bluetooth SIG
    // company id, so every BLE stack mis-splits it the same way — the first two
    // bytes land in the company-id field. Scan filters have to account for that.
    public static final byte[] ADV_MAGIC = { 0x48, 0x53, 0x43 };

    private final Context context;
    private final SmartGlassesDevice device;

    /** Frame codec. Mirrors glasses/protocol/frame.py and its 92 tests. */
    private final RelayFrameCodec codec = new RelayFrameCodec();

    public RelayGlassesCommunicator(Context context, SmartGlassesDevice device) {
        this.context = context;
        this.device = device;
    }

    @Override
    public void connectToSmartGlasses() {
        // Scan for ADV_MAGIC, connect, discover SERVICE_UUID, subscribe to
        // NOTIFY_UUID, then negotiate the largest MTU the phone will grant.
        //
        // MTU matters more here than on most devices: photos arrive chunked over
        // BLE, so a 23-byte default turns a 6 KB thumbnail into a visible wait.
        Log.d(TAG, "connect: scanning for HSC advertisement");
        // TODO: bind to the MentraOS BLE helper once the superclass is available.
    }

    @Override
    public void destroy() {
        Log.d(TAG, "destroy: closing GATT");
    }

    // --- capability gate -----------------------------------------------------

    /**
     * Protocol 0x0005 获取支持功能.
     *
     * Call before issuing anything else. Firmware revisions differ in what they
     * honour, and an unsupported command is a silent no-op rather than an error —
     * which is the worst failure mode to debug.
     */
    public void requestSupportedFeatures() {
        send(RelayCommand.GET_SUPPORTED_FEATURES, new byte[0]);
    }

    // --- what MentraOS actually asks a communicator to do --------------------

    @Override
    public void displayReferenceCardSimple(String title, String body) {
        // Relay One has no display. Speak it instead, so text-first MentraOS
        // apps still do something useful rather than silently no-op.
        speak(title == null || title.isEmpty() ? body : title + ". " + body);
    }

    /**
     * Route text to the glasses' speaker.
     *
     * Audio out is the easy half: the device is an ordinary Bluetooth audio sink,
     * and 0x0D01 picks whether playback lands on the glasses or the phone. The
     * synthesis itself belongs to the host app, not here.
     */
    public void speak(String text) {
        Log.d(TAG, "speak: " + text);
        // TODO: route via the host's TTS, then setSpeakerPlaybackStatus(glasses).
    }

    /**
     * Capture a still.
     *
     * Two different jobs share one camera, and picking wrong is the common
     * mistake:
     *   immediate = false  shutter to the device's own 4 GB, full resolution,
     *                      returns at once, syncs later over the AP. Correct for
     *                      anything nobody is waiting on.
     *   immediate = true   0x0906/0x0907, delivered chunked over BLE. Seconds,
     *                      not milliseconds. Only when the pixels answer a
     *                      question being asked right now.
     */
    public void requestPhoto(boolean immediate) {
        send(immediate ? RelayCommand.AI_PHOTO_START : RelayCommand.DEVICE_CONTROL, new byte[]{0x01});
    }

    /** Protocol 0x0E04. Records to the glasses, not over the radio. */
    public void setLocalRecording(boolean on) {
        send(RelayCommand.LOCAL_RECORDING_CONTROL, new byte[]{(byte) (on ? 0x01 : 0x00)});
    }

    /**
     * Open the device access point for bulk transfer (0x090B).
     *
     * Costs the phone its WiFi uplink for the duration, so this is a deliberate
     * foreground operation. A day of audio is 173 MB to 1.8 GB; over BLE that is
     * 16 hours to a week, which is why sync never rides BLE.
     */
    public void openAccessPoint() {
        send(RelayCommand.WIFI_AP_CONTROL, new byte[]{0x01});
    }

    // --- framing -------------------------------------------------------------

    private void send(RelayCommand command, byte[] payload) {
        byte[] frame = codec.encode(command.id, RelayFrameCodec.TYPE_REQUEST, codec.nextSeq(), payload);
        Log.d(TAG, "tx " + command + " " + toHex(frame));
        // TODO: writeCharacteristic(WRITE_UUID, frame)
    }

    /** Feed every notification here; BLE gives no message boundaries. */
    public void onNotify(byte[] chunk) {
        for (byte[] data : codec.feed(chunk)) {
            int commandId = ((data[1] & 0xFF) << 8) | (data[0] & 0xFF);
            Log.d(TAG, "rx 0x" + String.format("%04X", commandId));
            // TODO: dispatch to MentraOS event callbacks.
        }
    }

    private static String toHex(byte[] b) {
        StringBuilder sb = new StringBuilder(b.length * 2);
        for (byte x : b) sb.append(String.format("%02x", x));
        return sb.toString();
    }

    /** The commands this driver uses. Full set in glasses/protocol/commands.py. */
    enum RelayCommand {
        GET_SUPPORTED_FEATURES(0x0005),
        SET_TIME(0x0903),
        AI_CHAT_TRIGGER(0x0805),
        AI_PHOTO_START(0x0906),
        WIFI_AP_CONTROL(0x090B),
        DEVICE_CONTROL(0x0D01),
        LOCAL_RECORDING_CONTROL(0x0E04);

        final int id;
        RelayCommand(int id) { this.id = id; }
    }
}
