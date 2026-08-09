"""Command packets — the `data` block carried inside a wire frame.

Spec §四 协议数据. Every command, in both directions, has the same 6-byte header:

    Offset  Length  Description
    0       2       命令 ID          (command id, little-endian)
    2       1       命令类型          1=request, 2=response, 3=notify
    3       1       Sequence Number  request increments 0-255; response echoes it
    4       2       数据长度          (payload length, little-endian)
    6       N       payload

`Packet` is that structure. `encode`/`decode` move between it and bytes; use
`glasses.protocol.frame.encode_frame` to put one on the wire.

Command IDs below are transcribed from 通信协议 v2.0.17 (2026-01-21) — all 92 of
them, including the ones the spec marks 已弃用 (deprecated) or 未使用 (unused),
because a device on older firmware may still emit them.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import IntEnum

from .frame import MAX_DATA_LEN, ProtocolError, encode_frame

__all__ = ["CommandType", "Command", "Packet", "SequenceCounter"]

HEADER_LEN = 6


class CommandType(IntEnum):
    REQUEST = 1
    RESPONSE = 2
    NOTIFY = 3


class Command(IntEnum):
    """Every command ID in the spec.

    Direction annotations are the spec's own: APP->DEV is phone-to-glasses,
    DEV->APP is a report coming back.
    """

    # 1. 获取信息 — device identity
    GET_PRODUCT_INFO = 0x0001            # 获取产品信息
    GET_PRODUCT_MODEL = 0x0002           # 获取产品型号
    GET_VERSION = 0x0003                 # 获取版本号
    GET_HARDWARE_INFO = 0x0004           # 获取硬件信息
    GET_SUPPORTED_FEATURES = 0x0005      # 获取支持功能 — capability bitmap; call this first
    GET_DEVICE_NAME = 0x0006             # 获取设备名称
    HEARTBEAT = 0x0007                   # 心跳包

    # 2. 电量显示
    GET_BATTERY = 0x0101                 # 获取电量
    BATTERY_REPORT = 0x0102              # 电量上报 (DEV->APP)

    # 3. 降噪 — active noise cancellation
    GET_ANC_STATE = 0x0201               # 获取降噪状态
    SET_ANC = 0x0202                     # 降噪切换
    ANC_REPORT = 0x0203                  # 降噪状态上报 (DEV->APP)

    # 4. 佩戴检测 — wear detection
    GET_WEAR_DETECTION = 0x0301          # 获取佩戴检测状态
    SET_WEAR_DETECTION = 0x0302          # 佩戴检测开关

    # 5. 游戏模式 — low-latency audio mode
    GET_GAME_MODE = 0x0401               # 获取游戏模式状态
    SET_GAME_MODE = 0x0402               # 游戏模式开关
    GAME_MODE_REPORT = 0x0403            # 游戏模式上报 (DEV->APP)

    # 6. EQ 音效
    GET_EQ = 0x0501                      # 获取当前 EQ 音效
    SET_EQ = 0x0502                      # EQ 音效设置

    # 7. 按键设置 — remappable touch gestures
    GET_KEY_FUNCTIONS = 0x0601           # 获取按键功能
    SET_KEY_FUNCTIONS = 0x0602           # 按键功能设置

    # 8. 设备查找
    FIND_DEVICE = 0x0701                 # 查找设备
    FIND_DEVICE_REPORT = 0x0702          # 查找耳机状态上报 (DEV->APP)

    # 9. AI 对话 — 0x0801..0x0804 are marked 未使用 (unused) in v2.0.17
    AI_CHAT_MODE_UNUSED = 0x0801         # 对话模式 (未使用)
    AI_CHAT_DEVICE_MODE_UNUSED = 0x0802  # 设备 AI 对话模式 (未使用)
    AI_CHAT_EVENT_UNUSED = 0x0803        # 对话事件触发 (未使用)
    AI_CHAT_ASR_START_UNUSED = 0x0804    # 对话语音识别开始提示 (未使用)
    AI_CHAT_TRIGGER = 0x0805             # AI 实时语音对话事件触发 — 0x00 stop, 0x01 start
    AI_CHAT_PROMPT = 0x0806              # AI 实时语音对话提示 (DEV->APP)

    # 10. WiFi / camera / storage
    SET_WIFI_SSID_DEPRECATED = 0x0901    # 设置 WIFI SSID (已弃用) — sets the glasses' own hotspot
    SET_WIFI_PASSWORD_DEPRECATED = 0x0902  # 设置 WIFI 密码 (已弃用)
    SET_TIME = 0x0903                    # 设置时间 — sync device clock to phone
    CONNECTION_STATE_REPORT = 0x0904     # 上报连接状态 (DEV->APP)
    FILE_COUNT_UPDATE = 0x0905           # 文件个数更新 (DEV->APP)
    AI_PHOTO_START = 0x0906              # 图像识别拍照开始
    AI_PHOTO_COMPLETE = 0x0907           # 图像识别拍照完成 (DEV->APP)
    RTSP_URL = 0x0908                    # 实时视频 API (DEV->APP) — returns the RTSP stream URL
    DISK_INFO = 0x0909                   # 磁盘容量 API
    PREVIEW_CONTROL = 0x090A             # 视频预览控制 — start/stop; success is followed by 0x0908
    WIFI_AP_CONTROL = 0x090B             # WIFI AP 控制 — open/close the glasses' access point
    AP_SSID_REPORT = 0x090C              # 上报 AP SSID (DEV->APP)
    AP_PASSWORD_REPORT = 0x090D          # 上报 AP 密码 (DEV->APP)
    WIFI_OPERATION_REPORT = 0x090E       # 上报 wifi 操作 API (DEV->APP)
    SET_BIND_CODE = 0x090F               # 设置绑定码
    GET_BIND_CODE = 0x0910               # 获取绑定码
    CLEAR_UNUPLOADED_FILES = 0x0911      # 清除未上传文件
    CLEAR_RESULT_REPORT = 0x0912         # 清除结果上报 (DEV->APP)
    RUN_STATE_REPORT = 0x0913            # 运行状态上报 (DEV->APP)
    SET_STABILIZATION = 0x0914           # 设置防抖
    GET_STABILIZATION = 0x0915           # 获取防抖设置
    GET_FILE_COUNT = 0x0916              # 获取文件个数
    AP_MAC_REPORT = 0x0917               # 上报 AP MAC 地址 (DEV->APP)
    WIFI_P2P_SUPPORT_REPORT = 0x0918     # 上报 WIFI P2P 功能支持 (DEV->APP)
    WIFI_P2P_CONTROL = 0x0919            # WIFI P2P 控制
    WIFI_P2P_NAME_REPORT = 0x091A        # 上报 WIFI P2P 名称 (DEV->APP)
    WIFI_P2P_MAC_REPORT = 0x091B         # 上报 WIFI P2P MAC 地址 (DEV->APP)
    GET_DISK_INFO = 0x091C               # 获取磁盘容量信息
    SET_VIDEO_PARAMS = 0x091D            # 设置视频录制参数
    GET_VIDEO_PARAMS = 0x091E            # 获取视频录制参数
    SET_PHOTO_PARAMS = 0x091F            # 设置拍照参数
    GET_PHOTO_PARAMS = 0x0920            # 获取拍照参数
    SET_VIDEO_RESOLUTION = 0x0921        # 设置视频分辨率
    VIDEO_RESOLUTION_REPORT = 0x0922     # 上报视频分辨率 (DEV->APP)
    STABILIZATION_SUPPORT_REPORT = 0x0923  # 上报防抖处理支持 (DEV->APP)

    # 11. 通话 / 音频
    GET_CALL_STATE = 0x0A01              # 获取通话状态
    AUDIO_CONTROL = 0x0A02               # 音频控制
    AUDIO_DATA = 0x0A03                  # 音频数据 — mic stream (Opus / PCM 16 kHz mono)

    # 12. OTA
    GET_OTA_INFO = 0x0B01                # 获取 OTA 升级信息
    FIRMWARE_VERSION_REPORT = 0x0B02     # 上报固件版本号 (DEV->APP)
    OTA_START = 0x0B03                   # 开始 OTA 升级
    OTA_COMPLETE = 0x0B04                # 升级完成

    # 13. 文件传输
    FILE_FETCH_START = 0x0C01            # 开始获取文件
    FILE_DATA_UPLOAD = 0x0C02            # 文件数据上传 (DEV->APP)
    FILE_UPLOAD_END = 0x0C03             # 上传文件结束
    FILE_DATA_RETRY = 0x0C04             # 重新获取文件数据
    FILE_UPLOAD_ABORT = 0x0C05           # 终止文件上传

    # 14. 设备控制
    DEVICE_CONTROL = 0x0D01              # 设备控制命令
    LOCAL_VIDEO_STATE_REPORT = 0x0D02    # 本地录像状态上报 (DEV->APP)
    LOCAL_AUDIO_STATE_REPORT = 0x0D03    # 本地录音状态上报 (DEV->APP)

    # 15. 文件管理 / 录音
    GET_FILE_LIST = 0x0E01               # 获取文件列表、磁盘信息文件
    DELETE_FILE = 0x0E02                 # 删除文件
    DELETE_ALL_FILES = 0x0E03            # 删除所有文件
    LOCAL_RECORDING_CONTROL = 0x0E04     # 本地录音控制
    LOCAL_RECORDING_STATE_REPORT = 0x0E05  # 本地录音状态上报 (DEV->APP)
    SET_RECORDING_PROMPT = 0x0E06        # 本地录音提示设置
    GET_RECORDING_PROMPT = 0x0E07        # 获取本地录音提示状态
    RECORDING_FILE_COUNT_REPORT = 0x0E08  # 本地录音文件数量上报 (DEV->APP)
    SET_CALL_AUTO_RECORD = 0x0E09        # 通话自动录音设置
    GET_CALL_AUTO_RECORD = 0x0E0A        # 获取通话自动录音状态

    # 16. 语音唤醒 — wake word
    GET_WAKEWORD_LIST = 0x0F01           # 获取语音唤醒功能列表
    GET_WAKEWORD_SETTING = 0x0F02        # 获取语音唤醒设置
    SET_WAKEWORD_SETTING = 0x0F03        # 设置语音唤醒设置


@dataclass(frozen=True)
class Packet:
    """One command packet: the `data` block of a frame."""

    command: int
    type: CommandType
    seq: int
    payload: bytes = field(default=b"")

    def __post_init__(self) -> None:
        if not 0 <= self.command <= 0xFFFF:
            raise ValueError(f"command id out of range: {self.command}")
        if not 0 <= self.seq <= 0xFF:
            raise ValueError(f"sequence number must fit in a byte, got {self.seq}")
        if len(self.payload) > MAX_DATA_LEN - HEADER_LEN:
            raise ValueError(f"payload too long for the frame length field: {len(self.payload)}")

    def encode(self) -> bytes:
        """Serialise to the `data` block (no prefix/length/CRC)."""
        return b"".join(
            (
                int(self.command).to_bytes(2, "little"),
                bytes([int(self.type), self.seq]),
                len(self.payload).to_bytes(2, "little"),
                self.payload,
            )
        )

    def to_frame(self) -> bytes:
        """Serialise all the way to wire bytes, ready to write to the BLE characteristic."""
        return encode_frame(self.encode())

    @classmethod
    def decode(cls, data: bytes) -> "Packet":
        if len(data) < HEADER_LEN:
            raise ProtocolError(f"packet needs {HEADER_LEN} header bytes, got {len(data)}")
        command = int.from_bytes(data[0:2], "little")
        raw_type = data[2]
        seq = data[3]
        declared = int.from_bytes(data[4:6], "little")
        payload = data[HEADER_LEN:]
        if len(payload) != declared:
            raise ProtocolError(
                f"payload length field says {declared}, frame carries {len(payload)}"
            )
        try:
            ctype = CommandType(raw_type)
        except ValueError as exc:
            raise ProtocolError(f"unknown command type {raw_type}") from exc
        return cls(command=command, type=ctype, seq=seq, payload=payload)

    @property
    def name(self) -> str:
        """Human-readable command name, or a hex id if the device sends something new."""
        try:
            return Command(self.command).name
        except ValueError:
            return f"UNKNOWN_0x{self.command:04X}"

    def __repr__(self) -> str:
        return (
            f"Packet({self.name}, {self.type.name}, seq={self.seq}, "
            f"payload={self.payload.hex() or '-'})"
        )


class SequenceCounter:
    """Request sequence numbers: 0-255, wrapping.

    The device echoes a request's sequence number in its response, which is the
    only way to match a reply to a call when several are outstanding.
    """

    def __init__(self, start: int = 0) -> None:
        if not 0 <= start <= 0xFF:
            raise ValueError(f"start must be 0-255, got {start}")
        self._next = start

    def next(self) -> int:
        value = self._next
        self._next = (self._next + 1) & 0xFF
        return value
