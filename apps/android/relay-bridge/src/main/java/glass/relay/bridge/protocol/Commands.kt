package glass.relay.bridge.protocol

/**
 * Every command ID in 通信协议 v2.0.17 — all 92, including the ones the spec
 * marks 已弃用 (deprecated) or 未使用 (unused), because a device on older
 * firmware may still emit them.
 *
 * **Generated from `glasses/protocol/commands.py`, not transcribed.** That file
 * is the authority (92 Python tests), and
 * `glass.relay.bridge.commands.CommandCatalogTest.ids match glasses protocol
 * commands py` re-parses it on every run and fails if this table has drifted
 * from it. A hand-typed copy of 92 hex constants is a bug waiting for a quiet
 * afternoon.
 *
 * The name matters: if you go looking for the guard and cannot find it, the
 * next step is to assume there isn't one and delete the invariant.
 *
 * Direction annotations in the comments are the spec's own: APP->DEV is
 * phone-to-glasses, DEV->APP is a report coming back.
 */
object Command {



    // 1. 获取信息 — device identity
    const val GET_PRODUCT_INFO: Int = 0x0001 // 获取产品信息
    const val GET_PRODUCT_MODEL: Int = 0x0002 // 获取产品型号
    const val GET_VERSION: Int = 0x0003 // 获取版本号
    const val GET_HARDWARE_INFO: Int = 0x0004 // 获取硬件信息
    const val GET_SUPPORTED_FEATURES: Int = 0x0005 // 获取支持功能 — capability bitmap; call this first
    const val GET_DEVICE_NAME: Int = 0x0006 // 获取设备名称
    const val HEARTBEAT: Int = 0x0007 // 心跳包

    // 2. 电量显示
    const val GET_BATTERY: Int = 0x0101 // 获取电量
    const val BATTERY_REPORT: Int = 0x0102 // 电量上报 (DEV->APP)

    // 3. 降噪 — active noise cancellation
    const val GET_ANC_STATE: Int = 0x0201 // 获取降噪状态
    const val SET_ANC: Int = 0x0202 // 降噪切换
    const val ANC_REPORT: Int = 0x0203 // 降噪状态上报 (DEV->APP)

    // 4. 佩戴检测 — wear detection
    const val GET_WEAR_DETECTION: Int = 0x0301 // 获取佩戴检测状态
    const val SET_WEAR_DETECTION: Int = 0x0302 // 佩戴检测开关

    // 5. 游戏模式 — low-latency audio mode
    const val GET_GAME_MODE: Int = 0x0401 // 获取游戏模式状态
    const val SET_GAME_MODE: Int = 0x0402 // 游戏模式开关
    const val GAME_MODE_REPORT: Int = 0x0403 // 游戏模式上报 (DEV->APP)

    // 6. EQ 音效
    const val GET_EQ: Int = 0x0501 // 获取当前 EQ 音效
    const val SET_EQ: Int = 0x0502 // EQ 音效设置

    // 7. 按键设置 — remappable touch gestures
    const val GET_KEY_FUNCTIONS: Int = 0x0601 // 获取按键功能
    const val SET_KEY_FUNCTIONS: Int = 0x0602 // 按键功能设置

    // 8. 设备查找
    const val FIND_DEVICE: Int = 0x0701 // 查找设备
    const val FIND_DEVICE_REPORT: Int = 0x0702 // 查找耳机状态上报 (DEV->APP)

    // 9. AI 对话 — 0x0801..0x0804 are marked 未使用 (unused) in v2.0.17
    const val AI_CHAT_MODE_UNUSED: Int = 0x0801 // 对话模式 (未使用)
    const val AI_CHAT_DEVICE_MODE_UNUSED: Int = 0x0802 // 设备 AI 对话模式 (未使用)
    const val AI_CHAT_EVENT_UNUSED: Int = 0x0803 // 对话事件触发 (未使用)
    const val AI_CHAT_ASR_START_UNUSED: Int = 0x0804 // 对话语音识别开始提示 (未使用)
    const val AI_CHAT_TRIGGER: Int = 0x0805 // AI 实时语音对话事件触发 — 0x00 stop, 0x01 start
    const val AI_CHAT_PROMPT: Int = 0x0806 // AI 实时语音对话提示 (DEV->APP)

    // 10. WiFi / camera / storage
    const val SET_WIFI_SSID_DEPRECATED: Int = 0x0901 // 设置 WIFI SSID (已弃用) — sets the glasses' own hotspot
    const val SET_WIFI_PASSWORD_DEPRECATED: Int = 0x0902 // 设置 WIFI 密码 (已弃用)
    const val SET_TIME: Int = 0x0903 // 设置时间 — sync device clock to phone
    const val CONNECTION_STATE_REPORT: Int = 0x0904 // 上报连接状态 (DEV->APP)
    const val FILE_COUNT_UPDATE: Int = 0x0905 // 文件个数更新 (DEV->APP)
    const val AI_PHOTO_START: Int = 0x0906 // 图像识别拍照开始
    const val AI_PHOTO_COMPLETE: Int = 0x0907 // 图像识别拍照完成 (DEV->APP)
    const val RTSP_URL: Int = 0x0908 // 实时视频 API (DEV->APP) — returns the RTSP stream URL
    const val DISK_INFO: Int = 0x0909 // 磁盘容量 API
    const val PREVIEW_CONTROL: Int = 0x090A // 视频预览控制 — start/stop; success is followed by 0x0908
    const val WIFI_AP_CONTROL: Int = 0x090B // WIFI AP 控制 — open/close the glasses' access point
    const val AP_SSID_REPORT: Int = 0x090C // 上报 AP SSID (DEV->APP)
    const val AP_PASSWORD_REPORT: Int = 0x090D // 上报 AP 密码 (DEV->APP)
    const val WIFI_OPERATION_REPORT: Int = 0x090E // 上报 wifi 操作 API (DEV->APP)
    const val SET_BIND_CODE: Int = 0x090F // 设置绑定码
    const val GET_BIND_CODE: Int = 0x0910 // 获取绑定码
    const val CLEAR_UNUPLOADED_FILES: Int = 0x0911 // 清除未上传文件
    const val CLEAR_RESULT_REPORT: Int = 0x0912 // 清除结果上报 (DEV->APP)
    const val RUN_STATE_REPORT: Int = 0x0913 // 运行状态上报 (DEV->APP)
    const val SET_STABILIZATION: Int = 0x0914 // 设置防抖
    const val GET_STABILIZATION: Int = 0x0915 // 获取防抖设置
    const val GET_FILE_COUNT: Int = 0x0916 // 获取文件个数
    const val AP_MAC_REPORT: Int = 0x0917 // 上报 AP MAC 地址 (DEV->APP)
    const val WIFI_P2P_SUPPORT_REPORT: Int = 0x0918 // 上报 WIFI P2P 功能支持 (DEV->APP)
    const val WIFI_P2P_CONTROL: Int = 0x0919 // WIFI P2P 控制
    const val WIFI_P2P_NAME_REPORT: Int = 0x091A // 上报 WIFI P2P 名称 (DEV->APP)
    const val WIFI_P2P_MAC_REPORT: Int = 0x091B // 上报 WIFI P2P MAC 地址 (DEV->APP)
    const val GET_DISK_INFO: Int = 0x091C // 获取磁盘容量信息
    const val SET_VIDEO_PARAMS: Int = 0x091D // 设置视频录制参数
    const val GET_VIDEO_PARAMS: Int = 0x091E // 获取视频录制参数
    const val SET_PHOTO_PARAMS: Int = 0x091F // 设置拍照参数
    const val GET_PHOTO_PARAMS: Int = 0x0920 // 获取拍照参数
    const val SET_VIDEO_RESOLUTION: Int = 0x0921 // 设置视频分辨率
    const val VIDEO_RESOLUTION_REPORT: Int = 0x0922 // 上报视频分辨率 (DEV->APP)
    const val STABILIZATION_SUPPORT_REPORT: Int = 0x0923 // 上报防抖处理支持 (DEV->APP)

    // 11. 通话 / 音频
    const val GET_CALL_STATE: Int = 0x0A01 // 获取通话状态
    const val AUDIO_CONTROL: Int = 0x0A02 // 音频控制
    const val AUDIO_DATA: Int = 0x0A03 // 音频数据 — mic stream (Opus / PCM 16 kHz mono)

    // 12. OTA
    const val GET_OTA_INFO: Int = 0x0B01 // 获取 OTA 升级信息
    const val FIRMWARE_VERSION_REPORT: Int = 0x0B02 // 上报固件版本号 (DEV->APP)
    const val OTA_START: Int = 0x0B03 // 开始 OTA 升级
    const val OTA_COMPLETE: Int = 0x0B04 // 升级完成

    // 13. 文件传输
    const val FILE_FETCH_START: Int = 0x0C01 // 开始获取文件
    const val FILE_DATA_UPLOAD: Int = 0x0C02 // 文件数据上传 (DEV->APP)
    const val FILE_UPLOAD_END: Int = 0x0C03 // 上传文件结束
    const val FILE_DATA_RETRY: Int = 0x0C04 // 重新获取文件数据
    const val FILE_UPLOAD_ABORT: Int = 0x0C05 // 终止文件上传

    // 14. 设备控制
    const val DEVICE_CONTROL: Int = 0x0D01 // 设备控制命令
    const val LOCAL_VIDEO_STATE_REPORT: Int = 0x0D02 // 本地录像状态上报 (DEV->APP)
    const val LOCAL_AUDIO_STATE_REPORT: Int = 0x0D03 // 本地录音状态上报 (DEV->APP)

    // 15. 文件管理 / 录音
    const val GET_FILE_LIST: Int = 0x0E01 // 获取文件列表、磁盘信息文件
    const val DELETE_FILE: Int = 0x0E02 // 删除文件
    const val DELETE_ALL_FILES: Int = 0x0E03 // 删除所有文件
    const val LOCAL_RECORDING_CONTROL: Int = 0x0E04 // 本地录音控制
    const val LOCAL_RECORDING_STATE_REPORT: Int = 0x0E05 // 本地录音状态上报 (DEV->APP)
    const val SET_RECORDING_PROMPT: Int = 0x0E06 // 本地录音提示设置
    const val GET_RECORDING_PROMPT: Int = 0x0E07 // 获取本地录音提示状态
    const val RECORDING_FILE_COUNT_REPORT: Int = 0x0E08 // 本地录音文件数量上报 (DEV->APP)
    const val SET_CALL_AUTO_RECORD: Int = 0x0E09 // 通话自动录音设置
    const val GET_CALL_AUTO_RECORD: Int = 0x0E0A // 获取通话自动录音状态

    // 16. 语音唤醒 — wake word
    const val GET_WAKEWORD_LIST: Int = 0x0F01 // 获取语音唤醒功能列表
    const val GET_WAKEWORD_SETTING: Int = 0x0F02 // 获取语音唤醒设置
    const val SET_WAKEWORD_SETTING: Int = 0x0F03 // 设置语音唤醒设置

    private val NAMES: Map<Int, String> = mapOf(
        GET_PRODUCT_INFO to "GET_PRODUCT_INFO",
        GET_PRODUCT_MODEL to "GET_PRODUCT_MODEL",
        GET_VERSION to "GET_VERSION",
        GET_HARDWARE_INFO to "GET_HARDWARE_INFO",
        GET_SUPPORTED_FEATURES to "GET_SUPPORTED_FEATURES",
        GET_DEVICE_NAME to "GET_DEVICE_NAME",
        HEARTBEAT to "HEARTBEAT",
        GET_BATTERY to "GET_BATTERY",
        BATTERY_REPORT to "BATTERY_REPORT",
        GET_ANC_STATE to "GET_ANC_STATE",
        SET_ANC to "SET_ANC",
        ANC_REPORT to "ANC_REPORT",
        GET_WEAR_DETECTION to "GET_WEAR_DETECTION",
        SET_WEAR_DETECTION to "SET_WEAR_DETECTION",
        GET_GAME_MODE to "GET_GAME_MODE",
        SET_GAME_MODE to "SET_GAME_MODE",
        GAME_MODE_REPORT to "GAME_MODE_REPORT",
        GET_EQ to "GET_EQ",
        SET_EQ to "SET_EQ",
        GET_KEY_FUNCTIONS to "GET_KEY_FUNCTIONS",
        SET_KEY_FUNCTIONS to "SET_KEY_FUNCTIONS",
        FIND_DEVICE to "FIND_DEVICE",
        FIND_DEVICE_REPORT to "FIND_DEVICE_REPORT",
        AI_CHAT_MODE_UNUSED to "AI_CHAT_MODE_UNUSED",
        AI_CHAT_DEVICE_MODE_UNUSED to "AI_CHAT_DEVICE_MODE_UNUSED",
        AI_CHAT_EVENT_UNUSED to "AI_CHAT_EVENT_UNUSED",
        AI_CHAT_ASR_START_UNUSED to "AI_CHAT_ASR_START_UNUSED",
        AI_CHAT_TRIGGER to "AI_CHAT_TRIGGER",
        AI_CHAT_PROMPT to "AI_CHAT_PROMPT",
        SET_WIFI_SSID_DEPRECATED to "SET_WIFI_SSID_DEPRECATED",
        SET_WIFI_PASSWORD_DEPRECATED to "SET_WIFI_PASSWORD_DEPRECATED",
        SET_TIME to "SET_TIME",
        CONNECTION_STATE_REPORT to "CONNECTION_STATE_REPORT",
        FILE_COUNT_UPDATE to "FILE_COUNT_UPDATE",
        AI_PHOTO_START to "AI_PHOTO_START",
        AI_PHOTO_COMPLETE to "AI_PHOTO_COMPLETE",
        RTSP_URL to "RTSP_URL",
        DISK_INFO to "DISK_INFO",
        PREVIEW_CONTROL to "PREVIEW_CONTROL",
        WIFI_AP_CONTROL to "WIFI_AP_CONTROL",
        AP_SSID_REPORT to "AP_SSID_REPORT",
        AP_PASSWORD_REPORT to "AP_PASSWORD_REPORT",
        WIFI_OPERATION_REPORT to "WIFI_OPERATION_REPORT",
        SET_BIND_CODE to "SET_BIND_CODE",
        GET_BIND_CODE to "GET_BIND_CODE",
        CLEAR_UNUPLOADED_FILES to "CLEAR_UNUPLOADED_FILES",
        CLEAR_RESULT_REPORT to "CLEAR_RESULT_REPORT",
        RUN_STATE_REPORT to "RUN_STATE_REPORT",
        SET_STABILIZATION to "SET_STABILIZATION",
        GET_STABILIZATION to "GET_STABILIZATION",
        GET_FILE_COUNT to "GET_FILE_COUNT",
        AP_MAC_REPORT to "AP_MAC_REPORT",
        WIFI_P2P_SUPPORT_REPORT to "WIFI_P2P_SUPPORT_REPORT",
        WIFI_P2P_CONTROL to "WIFI_P2P_CONTROL",
        WIFI_P2P_NAME_REPORT to "WIFI_P2P_NAME_REPORT",
        WIFI_P2P_MAC_REPORT to "WIFI_P2P_MAC_REPORT",
        GET_DISK_INFO to "GET_DISK_INFO",
        SET_VIDEO_PARAMS to "SET_VIDEO_PARAMS",
        GET_VIDEO_PARAMS to "GET_VIDEO_PARAMS",
        SET_PHOTO_PARAMS to "SET_PHOTO_PARAMS",
        GET_PHOTO_PARAMS to "GET_PHOTO_PARAMS",
        SET_VIDEO_RESOLUTION to "SET_VIDEO_RESOLUTION",
        VIDEO_RESOLUTION_REPORT to "VIDEO_RESOLUTION_REPORT",
        STABILIZATION_SUPPORT_REPORT to "STABILIZATION_SUPPORT_REPORT",
        GET_CALL_STATE to "GET_CALL_STATE",
        AUDIO_CONTROL to "AUDIO_CONTROL",
        AUDIO_DATA to "AUDIO_DATA",
        GET_OTA_INFO to "GET_OTA_INFO",
        FIRMWARE_VERSION_REPORT to "FIRMWARE_VERSION_REPORT",
        OTA_START to "OTA_START",
        OTA_COMPLETE to "OTA_COMPLETE",
        FILE_FETCH_START to "FILE_FETCH_START",
        FILE_DATA_UPLOAD to "FILE_DATA_UPLOAD",
        FILE_UPLOAD_END to "FILE_UPLOAD_END",
        FILE_DATA_RETRY to "FILE_DATA_RETRY",
        FILE_UPLOAD_ABORT to "FILE_UPLOAD_ABORT",
        DEVICE_CONTROL to "DEVICE_CONTROL",
        LOCAL_VIDEO_STATE_REPORT to "LOCAL_VIDEO_STATE_REPORT",
        LOCAL_AUDIO_STATE_REPORT to "LOCAL_AUDIO_STATE_REPORT",
        GET_FILE_LIST to "GET_FILE_LIST",
        DELETE_FILE to "DELETE_FILE",
        DELETE_ALL_FILES to "DELETE_ALL_FILES",
        LOCAL_RECORDING_CONTROL to "LOCAL_RECORDING_CONTROL",
        LOCAL_RECORDING_STATE_REPORT to "LOCAL_RECORDING_STATE_REPORT",
        SET_RECORDING_PROMPT to "SET_RECORDING_PROMPT",
        GET_RECORDING_PROMPT to "GET_RECORDING_PROMPT",
        RECORDING_FILE_COUNT_REPORT to "RECORDING_FILE_COUNT_REPORT",
        SET_CALL_AUTO_RECORD to "SET_CALL_AUTO_RECORD",
        GET_CALL_AUTO_RECORD to "GET_CALL_AUTO_RECORD",
        GET_WAKEWORD_LIST to "GET_WAKEWORD_LIST",
        GET_WAKEWORD_SETTING to "GET_WAKEWORD_SETTING",
        SET_WAKEWORD_SETTING to "SET_WAKEWORD_SETTING",
    )

    /** Every id in the spec, ascending. */
    val ALL: List<Int> = NAMES.keys.sorted()

    fun isKnown(id: Int): Boolean = NAMES.containsKey(id)

    /**
     * Spec name, or `UNKNOWN_0x1234`.
     *
     * Byte-identical to `Packet.name` in the Python and `commandName` in
     * `glasses/bridge/src/commands.ts`, so a log line from any of the three
     * diffs against the others.
     */
    fun nameOf(id: Int): String = NAMES[id] ?: "UNKNOWN_0x%04X".format(id)
}

/** Free function mirroring the TypeScript `commandName(id)`. */
fun commandName(id: Int): String = Command.nameOf(id)
