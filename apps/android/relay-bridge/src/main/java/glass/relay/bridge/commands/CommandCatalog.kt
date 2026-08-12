package glass.relay.bridge.commands

import glass.relay.bridge.protocol.Command

/**
 * Every glasses command, described well enough to put a control on a screen.
 *
 * `ORCHESTRATOR.md` §5 job 2: *every glasses command, by hand*. "Voice is the
 * point, but a product where the only input is speech fails in a quiet room, a
 * loud room, and on a bad day."
 *
 * The Android app satisfies that by **generating** its command screen from this
 * table rather than by someone remembering to add a button. `CommandCatalogTest`
 * asserts the table covers exactly the 92 ids in `glasses/protocol/commands.py`,
 * once each — so a command cannot exist without a way to reach it.
 *
 * ## Roles, and why some rows have no button
 *
 * Roles are the same five as `COMMAND_CATALOG` in
 * `glasses/bridge/src/commands.ts`, taken from that file rather than re-decided:
 *
 *  - `Command`     APP -> DEV; the UI sends it
 *  - `Report`      DEV -> APP; the UI *displays* it, there is nothing to send
 *  - `Both`        meaningful in both directions
 *  - `Unused`      spec says 未使用 — refused locally, never put on the wire
 *  - `Deprecated`  spec says 已弃用 — same
 *
 * Refusing rather than hiding is deliberate. A command the firmware still
 * reports needs a name in the log; a command the spec retired needs to be
 * visibly retired, not quietly missing.
 *
 * ## Payloads: attested vs not
 *
 * `APPS-SCOPE.md` §5.1 draws the line and this table keeps it. The command ids,
 * the framing, the CRC and the enumerated single-byte control values are
 * attested — from the spec and from the shipping SDK headers. The byte layout
 * of most *response* payloads is not. So a row carries an [ArgSpec] only where
 * the request layout is known; everything else is [ArgSpec.Unattested] and the
 * console will not pretend it can build one.
 */
object CommandCatalog {

    /** Screen grouping. Taken from the spec's own section headings. */
    enum class Category { Identity, Power, Audio, Capture, Input, Device, Voice, Network, Files }

    enum class CommandRole { Command, Report, Both, Unused, Deprecated }

    /**
     * What the UI must collect before a command can be sent.
     *
     * Every variant except [Unattested] can be encoded to bytes here, so the
     * console builds a real frame rather than handing the vendor SDK a guess.
     */
    sealed interface ArgSpec {
        /** Send it as-is. */
        data object None : ArgSpec

        /** One byte, 0x00 / 0x01. Shared by 0x090A, 0x090B, 0x0919, 0x0E04 and friends. */
        data class Toggle(val onLabel: String = "On", val offLabel: String = "Off") : ArgSpec

        /** One byte from a fixed, named list — the SDK's own enums. */
        data class Choice(val options: List<Option>) : ArgSpec {
            data class Option(val label: String, val value: Int)
        }

        /** 0x0F03: repeating `Index(1) Enabled(1)`. Selection only — never a phrase. */
        data object WakeWordSelection : ArgSpec

        /**
         * The request layout is not attested by the spec or the SDK headers.
         *
         * The console shows the row, names the command, and refuses to send.
         * A guessed layout that looks like a fact is how someone spends a day
         * debugging the BLE stack.
         */
        data class Unattested(val whatIsMissing: String) : ArgSpec
    }

    data class Entry(
        val id: Int,
        val name: String,
        val role: CommandRole,
        val category: Category,
        /** The spec's own term, which is the authoritative label. */
        val specName: String,
        val label: String,
        /** Destroys user data. The console requires an explicit confirmation. */
        val destructive: Boolean = false,
        val args: ArgSpec = ArgSpec.Unattested("no request payload layout recorded"),
        val note: String? = null,
    ) {
        /** True when the app has a legitimate reason to put this on the wire. */
        val sendable: Boolean
            get() = (role == CommandRole.Command || role == CommandRole.Both) &&
                args !is ArgSpec.Unattested
    }

    // --- attested payload vocabularies ---------------------------------------
    //
    // Values are from the shipping SDK headers (`QCOperatorDeviceMode`,
    // `QGAISpeakMode`, `QGAIImageSharpnessLevel`) and from the protocol spec's
    // enumerated control bytes — the same source `glasses/bridge/src/commands.ts`
    // uses. They are the only payloads this app builds itself.

    object Toggle {
        const val OFF = 0x00
        const val ON = 0x01
    }

    /** 0x0A02 音频控制. Uplink only — the downlink is 0x0A03. */
    object AudioControl {
        const val STOP_UPLINK = 0x00
        const val START_UPLINK = 0x01
    }

    /** `QCOperatorDeviceMode`, the modes 0x0D01 accepts. Not exhaustive by design. */
    object DeviceMode {
        const val PHOTO = 0x01
        const val VIDEO = 0x02
        const val VIDEO_STOP = 0x03
        const val TRANSFER = 0x04
        const val OTA = 0x05
        const val AI_PHOTO = 0x06
        const val FACTORY_RESET = 0x0A
        const val FIND_DEVICE = 0x0D
        const val RESTART = 0x0E
        const val SPEAK_START = 0x10
        const val SPEAK_STOP = 0x11
        const val SHIPPING = 0x16
    }

    /** `QGAISpeakMode` — what the glasses do while the assistant talks. */
    object SpeakMode {
        const val START = 0x01
        const val HOLD = 0x02
        const val STOP = 0x03
        const val THINKING_START = 0x04
        const val THINKING_HOLD = 0x05
        const val THINKING_STOP = 0x06
        const val NO_NETWORK = 0xF1
    }

    private val TOGGLE = ArgSpec.Toggle()

    private val DEVICE_MODES = ArgSpec.Choice(
        listOf(
            ArgSpec.Choice.Option("Take photo", DeviceMode.PHOTO),
            ArgSpec.Choice.Option("Start video", DeviceMode.VIDEO),
            ArgSpec.Choice.Option("Stop video", DeviceMode.VIDEO_STOP),
            ArgSpec.Choice.Option("Transfer mode", DeviceMode.TRANSFER),
            ArgSpec.Choice.Option("OTA mode", DeviceMode.OTA),
            ArgSpec.Choice.Option("AI photo", DeviceMode.AI_PHOTO),
            ArgSpec.Choice.Option("Find device", DeviceMode.FIND_DEVICE),
            ArgSpec.Choice.Option("Speak start", DeviceMode.SPEAK_START),
            ArgSpec.Choice.Option("Speak stop", DeviceMode.SPEAK_STOP),
            ArgSpec.Choice.Option("Restart", DeviceMode.RESTART),
            ArgSpec.Choice.Option("Shipping mode", DeviceMode.SHIPPING),
            ArgSpec.Choice.Option("Factory reset", DeviceMode.FACTORY_RESET),
        ),
    )

    private val AUDIO_UPLINK = ArgSpec.Choice(
        listOf(
            ArgSpec.Choice.Option("Open mic uplink", AudioControl.START_UPLINK),
            ArgSpec.Choice.Option("Close mic uplink", AudioControl.STOP_UPLINK),
        ),
    )

    /**
     * Per-command overrides layered on the generated table.
     *
     * Kept separate from the rows so the rows can stay generated: the ids,
     * names, roles and destructive flags all come from files that own them, and
     * only the app-specific judgement lives here.
     */
    private val ARGS: Map<Int, ArgSpec> = mapOf(
        Command.SET_WEAR_DETECTION to TOGGLE,
        Command.SET_GAME_MODE to TOGGLE,
        Command.FIND_DEVICE to TOGGLE,
        Command.PREVIEW_CONTROL to TOGGLE,
        Command.WIFI_AP_CONTROL to TOGGLE,
        Command.WIFI_P2P_CONTROL to TOGGLE,
        Command.SET_STABILIZATION to TOGGLE,
        Command.LOCAL_RECORDING_CONTROL to ArgSpec.Toggle("Start recording", "Stop recording"),
        Command.SET_RECORDING_PROMPT to TOGGLE,
        Command.SET_CALL_AUTO_RECORD to TOGGLE,
        Command.AI_CHAT_TRIGGER to ArgSpec.Toggle("Start", "Stop"),
        Command.AUDIO_CONTROL to AUDIO_UPLINK,
        Command.DEVICE_CONTROL to DEVICE_MODES,
        Command.SET_WAKEWORD_SETTING to ArgSpec.WakeWordSelection,
        // No-payload requests: the id *is* the question.
        Command.GET_PRODUCT_INFO to ArgSpec.None,
        Command.GET_PRODUCT_MODEL to ArgSpec.None,
        Command.GET_VERSION to ArgSpec.None,
        Command.GET_HARDWARE_INFO to ArgSpec.None,
        Command.GET_SUPPORTED_FEATURES to ArgSpec.None,
        Command.GET_DEVICE_NAME to ArgSpec.None,
        Command.HEARTBEAT to ArgSpec.None,
        Command.GET_BATTERY to ArgSpec.None,
        Command.GET_ANC_STATE to ArgSpec.None,
        Command.GET_WEAR_DETECTION to ArgSpec.None,
        Command.GET_GAME_MODE to ArgSpec.None,
        Command.GET_EQ to ArgSpec.None,
        Command.GET_KEY_FUNCTIONS to ArgSpec.None,
        Command.GET_BIND_CODE to ArgSpec.None,
        Command.GET_STABILIZATION to ArgSpec.None,
        Command.GET_FILE_COUNT to ArgSpec.None,
        Command.DISK_INFO to ArgSpec.None,
        Command.GET_DISK_INFO to ArgSpec.None,
        Command.GET_VIDEO_PARAMS to ArgSpec.None,
        Command.GET_PHOTO_PARAMS to ArgSpec.None,
        Command.GET_CALL_STATE to ArgSpec.None,
        Command.GET_OTA_INFO to ArgSpec.None,
        Command.GET_FILE_LIST to ArgSpec.None,
        Command.DELETE_ALL_FILES to ArgSpec.None,
        Command.CLEAR_UNUPLOADED_FILES to ArgSpec.None,
        Command.GET_RECORDING_PROMPT to ArgSpec.None,
        Command.GET_CALL_AUTO_RECORD to ArgSpec.None,
        Command.GET_WAKEWORD_LIST to ArgSpec.None,
        Command.GET_WAKEWORD_SETTING to ArgSpec.None,
    )

    private val NOTES: Map<Int, String> = mapOf(
        Command.GET_SUPPORTED_FEATURES to
            "Call first. Firmware revisions differ in what they honour.",
        Command.SET_TIME to
            "Align the device clock before any capture, or every timestamp is wrong.",
        Command.AUDIO_CONTROL to
            "Uplink only. Path A — expensive on both batteries, so open on intent and close straight after.",
        Command.AUDIO_DATA to
            "Bidirectional: mic up and speech down, ~3 KB/s each way.",
        Command.LOCAL_RECORDING_CONTROL to
            "Path B — the all-day pipeline. Records to the glasses' own 4 GB, survives the phone being out of range.",
        Command.CLEAR_UNUPLOADED_FILES to
            "Deletes exactly the audio that has NOT been synced yet. The last resort for a wedged device, never routine.",
        Command.WIFI_AP_CONTROL to
            "Bulk sync only. Joining the glasses' AP costs the phone its own WiFi uplink.",
        Command.SET_WIFI_SSID_DEPRECATED to
            "已弃用, and it sets the glasses' OWN hotspot — there is no station mode. Refused.",
        Command.SET_WIFI_PASSWORD_DEPRECATED to
            "已弃用. Refused for the same reason.",
        Command.AI_CHAT_TRIGGER to
            "The device reports a trigger; the APP recognises. No command asks the glasses to transcribe.",
        Command.GET_WAKEWORD_LIST to
            "Firmware-fixed list. Selection by index only — there is no command that accepts a new phrase.",
        Command.SET_RECORDING_PROMPT to
            "The audible recording cue. A consent surface — ARCHITECTURE.md §6.",
        Command.SET_CALL_AUTO_RECORD to
            "Default off in two-party-consent jurisdictions, which includes Quebec.",
    )

    private fun row(
        id: Int,
        role: CommandRole,
        category: Category,
        specName: String,
        label: String,
        destructive: Boolean = false,
    ) = Entry(
        id = id,
        name = Command.nameOf(id),
        role = role,
        category = category,
        specName = specName,
        label = label,
        destructive = destructive,
        args = ARGS[id] ?: when (role) {
            CommandRole.Unused -> ArgSpec.Unattested("未使用 in v2.0.17")
            CommandRole.Deprecated -> ArgSpec.Unattested("已弃用 in v2.0.17")
            CommandRole.Report -> ArgSpec.Unattested("device report; nothing to send")
            else -> ArgSpec.Unattested("request payload layout not attested")
        },
        note = NOTES[id],
    )

    /**
     * The table.
     *
     * Ids, names, roles and destructive flags are generated: the ids and names
     * from `glasses/protocol/commands.py`, the roles and destructive flags from
     * `COMMAND_CATALOG` in `glasses/bridge/src/commands.ts`. Categories are the
     * spec's own section headings. Nothing here was decided twice.
     */
    val ENTRIES: List<Entry> = with(Command) {
        listOf(
            row(GET_PRODUCT_INFO, CommandRole.Command, Category.Identity, "获取产品信息", "Get product info"),
            row(GET_PRODUCT_MODEL, CommandRole.Command, Category.Identity, "获取产品型号", "Get product model"),
            row(GET_VERSION, CommandRole.Command, Category.Identity, "获取版本号", "Get version"),
            row(GET_HARDWARE_INFO, CommandRole.Command, Category.Identity, "获取硬件信息", "Get hardware info"),
            row(GET_SUPPORTED_FEATURES, CommandRole.Command, Category.Identity, "获取支持功能", "Get supported features"),
            row(GET_DEVICE_NAME, CommandRole.Command, Category.Identity, "获取设备名称", "Get device name"),
            row(HEARTBEAT, CommandRole.Command, Category.Identity, "心跳包", "Heartbeat"),
            row(GET_BATTERY, CommandRole.Command, Category.Power, "获取电量", "Get battery"),
            row(BATTERY_REPORT, CommandRole.Report, Category.Power, "电量上报", "Battery report"),
            row(GET_ANC_STATE, CommandRole.Command, Category.Audio, "获取降噪状态", "Get noise cancellation"),
            row(SET_ANC, CommandRole.Command, Category.Audio, "降噪切换", "Set noise cancellation"),
            row(ANC_REPORT, CommandRole.Report, Category.Audio, "降噪状态上报", "Noise cancellation report"),
            row(GET_WEAR_DETECTION, CommandRole.Command, Category.Capture, "获取佩戴检测状态", "Get wear detection"),
            row(SET_WEAR_DETECTION, CommandRole.Command, Category.Capture, "佩戴检测开关", "Set wear detection"),
            row(GET_GAME_MODE, CommandRole.Command, Category.Audio, "获取游戏模式状态", "Get game mode"),
            row(SET_GAME_MODE, CommandRole.Command, Category.Audio, "游戏模式开关", "Set game mode"),
            row(GAME_MODE_REPORT, CommandRole.Report, Category.Audio, "游戏模式上报", "Game mode report"),
            row(GET_EQ, CommandRole.Command, Category.Audio, "获取当前 EQ 音效", "Get equaliser"),
            row(SET_EQ, CommandRole.Command, Category.Audio, "EQ 音效设置", "Set equaliser"),
            row(GET_KEY_FUNCTIONS, CommandRole.Command, Category.Input, "获取按键功能", "Get key bindings"),
            row(SET_KEY_FUNCTIONS, CommandRole.Command, Category.Input, "按键功能设置", "Set key bindings"),
            row(FIND_DEVICE, CommandRole.Command, Category.Device, "查找设备", "Find device"),
            row(FIND_DEVICE_REPORT, CommandRole.Report, Category.Device, "查找耳机状态上报", "Find device report"),
            row(AI_CHAT_MODE_UNUSED, CommandRole.Unused, Category.Voice, "对话模式", "AI chat mode"),
            row(AI_CHAT_DEVICE_MODE_UNUSED, CommandRole.Unused, Category.Voice, "设备 AI 对话模式", "Device AI chat mode"),
            row(AI_CHAT_EVENT_UNUSED, CommandRole.Report, Category.Voice, "对话事件触发", "AI chat event"),
            row(AI_CHAT_ASR_START_UNUSED, CommandRole.Unused, Category.Voice, "对话语音识别开始提示", "AI chat ASR start"),
            row(AI_CHAT_TRIGGER, CommandRole.Report, Category.Voice, "AI 实时语音对话事件触发", "Voice trigger"),
            row(AI_CHAT_PROMPT, CommandRole.Report, Category.Voice, "AI 实时语音对话提示", "Vendor AI prompt"),
            row(SET_WIFI_SSID_DEPRECATED, CommandRole.Deprecated, Category.Network, "设置 WIFI SSID", "Set AP SSID"),
            row(SET_WIFI_PASSWORD_DEPRECATED, CommandRole.Deprecated, Category.Network, "设置 WIFI 密码", "Set AP password"),
            row(SET_TIME, CommandRole.Command, Category.Device, "设置时间", "Set time"),
            row(CONNECTION_STATE_REPORT, CommandRole.Report, Category.Network, "上报连接状态", "Connection state report"),
            row(FILE_COUNT_UPDATE, CommandRole.Report, Category.Files, "文件个数更新", "File count update"),
            row(AI_PHOTO_START, CommandRole.Command, Category.Capture, "图像识别拍照开始", "Take photo for AI"),
            row(AI_PHOTO_COMPLETE, CommandRole.Report, Category.Capture, "图像识别拍照完成", "AI photo complete"),
            row(RTSP_URL, CommandRole.Report, Category.Network, "实时视频 API", "RTSP URL"),
            row(DISK_INFO, CommandRole.Command, Category.Files, "磁盘容量 API", "Disk info"),
            row(PREVIEW_CONTROL, CommandRole.Command, Category.Network, "视频预览控制", "Live preview"),
            row(WIFI_AP_CONTROL, CommandRole.Command, Category.Network, "WIFI AP 控制", "Glasses access point"),
            row(AP_SSID_REPORT, CommandRole.Report, Category.Network, "上报 AP SSID", "AP SSID report"),
            row(AP_PASSWORD_REPORT, CommandRole.Report, Category.Network, "上报 AP 密码", "AP password report"),
            row(WIFI_OPERATION_REPORT, CommandRole.Report, Category.Network, "上报 wifi 操作 API", "WiFi operation report"),
            row(SET_BIND_CODE, CommandRole.Command, Category.Device, "设置绑定码", "Set bind code"),
            row(GET_BIND_CODE, CommandRole.Command, Category.Device, "获取绑定码", "Get bind code"),
            row(CLEAR_UNUPLOADED_FILES, CommandRole.Command, Category.Files, "清除未上传文件", "Clear un-uploaded files", destructive = true),
            row(CLEAR_RESULT_REPORT, CommandRole.Report, Category.Files, "清除结果上报", "Clear result report"),
            row(RUN_STATE_REPORT, CommandRole.Report, Category.Device, "运行状态上报", "Run state report"),
            row(SET_STABILIZATION, CommandRole.Command, Category.Capture, "设置防抖", "Set stabilisation"),
            row(GET_STABILIZATION, CommandRole.Command, Category.Capture, "获取防抖设置", "Get stabilisation"),
            row(GET_FILE_COUNT, CommandRole.Command, Category.Files, "获取文件个数", "Get file count"),
            row(AP_MAC_REPORT, CommandRole.Report, Category.Network, "上报 AP MAC 地址", "AP MAC report"),
            row(WIFI_P2P_SUPPORT_REPORT, CommandRole.Report, Category.Network, "上报 WIFI P2P 功能支持", "WiFi P2P support report"),
            row(WIFI_P2P_CONTROL, CommandRole.Command, Category.Network, "WIFI P2P 控制", "WiFi P2P"),
            row(WIFI_P2P_NAME_REPORT, CommandRole.Report, Category.Network, "上报 WIFI P2P 名称", "WiFi P2P name report"),
            row(WIFI_P2P_MAC_REPORT, CommandRole.Report, Category.Network, "上报 WIFI P2P MAC 地址", "WiFi P2P MAC report"),
            row(GET_DISK_INFO, CommandRole.Command, Category.Files, "获取磁盘容量信息", "Get disk info"),
            row(SET_VIDEO_PARAMS, CommandRole.Command, Category.Capture, "设置视频录制参数", "Set video params"),
            row(GET_VIDEO_PARAMS, CommandRole.Command, Category.Capture, "获取视频录制参数", "Get video params"),
            row(SET_PHOTO_PARAMS, CommandRole.Command, Category.Capture, "设置拍照参数", "Set photo params"),
            row(GET_PHOTO_PARAMS, CommandRole.Command, Category.Capture, "获取拍照参数", "Get photo params"),
            row(SET_VIDEO_RESOLUTION, CommandRole.Command, Category.Capture, "设置视频分辨率", "Set video resolution"),
            row(VIDEO_RESOLUTION_REPORT, CommandRole.Report, Category.Capture, "上报视频分辨率", "Video resolution report"),
            row(STABILIZATION_SUPPORT_REPORT, CommandRole.Report, Category.Capture, "上报防抖处理支持", "Stabilisation support report"),
            row(GET_CALL_STATE, CommandRole.Command, Category.Audio, "获取通话状态", "Get call state"),
            row(AUDIO_CONTROL, CommandRole.Command, Category.Audio, "音频控制", "Mic uplink"),
            row(AUDIO_DATA, CommandRole.Both, Category.Audio, "音频数据", "Audio data"),
            row(GET_OTA_INFO, CommandRole.Command, Category.Device, "获取 OTA 升级信息", "Get OTA info"),
            row(FIRMWARE_VERSION_REPORT, CommandRole.Report, Category.Device, "上报固件版本号", "Firmware version report"),
            row(OTA_START, CommandRole.Command, Category.Device, "开始 OTA 升级", "Start OTA"),
            row(OTA_COMPLETE, CommandRole.Both, Category.Device, "升级完成", "OTA complete"),
            row(FILE_FETCH_START, CommandRole.Command, Category.Files, "开始获取文件", "Fetch file"),
            row(FILE_DATA_UPLOAD, CommandRole.Report, Category.Files, "文件数据上传", "File data upload"),
            row(FILE_UPLOAD_END, CommandRole.Report, Category.Files, "上传文件结束", "File upload end"),
            row(FILE_DATA_RETRY, CommandRole.Command, Category.Files, "重新获取文件数据", "Retry file chunk"),
            row(FILE_UPLOAD_ABORT, CommandRole.Command, Category.Files, "终止文件上传", "Abort file transfer"),
            row(DEVICE_CONTROL, CommandRole.Command, Category.Device, "设备控制命令", "Device control"),
            row(LOCAL_VIDEO_STATE_REPORT, CommandRole.Report, Category.Capture, "本地录像状态上报", "Local video state report"),
            row(LOCAL_AUDIO_STATE_REPORT, CommandRole.Report, Category.Capture, "本地录音状态上报", "Local audio state report"),
            row(GET_FILE_LIST, CommandRole.Command, Category.Files, "获取文件列表、磁盘信息文件", "Get file list"),
            row(DELETE_FILE, CommandRole.Command, Category.Files, "删除文件", "Delete file", destructive = true),
            row(DELETE_ALL_FILES, CommandRole.Command, Category.Files, "删除所有文件", "Delete all files", destructive = true),
            row(LOCAL_RECORDING_CONTROL, CommandRole.Command, Category.Capture, "本地录音控制", "Local recording"),
            row(LOCAL_RECORDING_STATE_REPORT, CommandRole.Report, Category.Capture, "本地录音状态上报", "Local recording state report"),
            row(SET_RECORDING_PROMPT, CommandRole.Command, Category.Capture, "本地录音提示设置", "Set recording prompt"),
            row(GET_RECORDING_PROMPT, CommandRole.Command, Category.Capture, "获取本地录音提示状态", "Get recording prompt"),
            row(RECORDING_FILE_COUNT_REPORT, CommandRole.Report, Category.Files, "本地录音文件数量上报", "Recording file count report"),
            row(SET_CALL_AUTO_RECORD, CommandRole.Command, Category.Audio, "通话自动录音设置", "Set call auto-record"),
            row(GET_CALL_AUTO_RECORD, CommandRole.Command, Category.Audio, "获取通话自动录音状态", "Get call auto-record"),
            row(GET_WAKEWORD_LIST, CommandRole.Command, Category.Voice, "获取语音唤醒功能列表", "Get wake words"),
            row(GET_WAKEWORD_SETTING, CommandRole.Command, Category.Voice, "获取语音唤醒设置", "Get wake word settings"),
            row(SET_WAKEWORD_SETTING, CommandRole.Command, Category.Voice, "设置语音唤醒设置", "Set wake word settings"),
        )
    }

    private val BY_ID: Map<Int, Entry> = ENTRIES.associateBy { it.id }

    fun describe(id: Int): Entry? = BY_ID[id]

    /** Commands the UI must confirm before sending. */
    fun destructive(): List<Entry> = ENTRIES.filter { it.destructive }

    fun byCategory(): Map<Category, List<Entry>> =
        ENTRIES.groupBy { it.category }.toSortedMap(compareBy { it.ordinal })
}
