package wire

import "fmt"

// ErrCode is the RSP_ERR err_code (docs/03 §6.1).
type ErrCode uint16

const (
	ErrUnknownType       ErrCode = 1
	ErrBadBody           ErrCode = 2
	ErrInvalidArg        ErrCode = 3
	ErrChannelNotAllowed ErrCode = 4
	ErrCountryInvalid    ErrCode = 5
	ErrNotSupported      ErrCode = 6
	ErrFilterInvalid     ErrCode = 7
	ErrNoSession         ErrCode = 8
	ErrRadio             ErrCode = 9
	ErrSafeMode          ErrCode = 10
)

var errNames = map[ErrCode]string{
	ErrUnknownType:       "ERR_UNKNOWN_TYPE",
	ErrBadBody:           "ERR_BAD_BODY",
	ErrInvalidArg:        "ERR_INVALID_ARG",
	ErrChannelNotAllowed: "ERR_CHANNEL_NOT_ALLOWED",
	ErrCountryInvalid:    "ERR_COUNTRY_INVALID",
	ErrNotSupported:      "ERR_NOT_SUPPORTED",
	ErrFilterInvalid:     "ERR_FILTER_INVALID",
	ErrNoSession:         "ERR_NO_SESSION",
	ErrRadio:             "ERR_RADIO",
	ErrSafeMode:          "ERR_SAFE_MODE",
}

func (e ErrCode) String() string {
	if n, ok := errNames[e]; ok {
		return n
	}
	return fmt.Sprintf("ERR_%d", uint16(e))
}

// ResetReason (EVT_RESET / DeviceInfo, docs/03 §6.1).
type ResetReason uint8

const (
	ResetUnknown  ResetReason = 0
	ResetPowerOn  ResetReason = 1
	ResetSoftware ResetReason = 2
	ResetPanic    ResetReason = 3
	ResetWatchdog ResetReason = 4
	ResetBrownout ResetReason = 5
	ResetAssert   ResetReason = 6
	ResetExternal ResetReason = 7
)

func (r ResetReason) String() string {
	switch r {
	case ResetUnknown:
		return "UNKNOWN"
	case ResetPowerOn:
		return "POWER_ON"
	case ResetSoftware:
		return "SOFTWARE"
	case ResetPanic:
		return "PANIC"
	case ResetWatchdog:
		return "WATCHDOG"
	case ResetBrownout:
		return "BROWNOUT"
	case ResetAssert:
		return "ASSERT"
	case ResetExternal:
		return "EXTERNAL"
	}
	return fmt.Sprintf("RESET_%d", uint8(r))
}

var espErrNames = map[int32]string{
	0x0000: "ESP_OK",
	-1:     "ESP_FAIL",
	0x0101: "ESP_ERR_NO_MEM",
	0x0102: "ESP_ERR_INVALID_ARG",
	0x0103: "ESP_ERR_INVALID_STATE",
	0x0104: "ESP_ERR_INVALID_SIZE",
	0x0105: "ESP_ERR_NOT_FOUND",
	0x0106: "ESP_ERR_NOT_SUPPORTED",
	0x0107: "ESP_ERR_TIMEOUT",
	0x3001: "ESP_ERR_WIFI_NOT_INIT",
	0x3002: "ESP_ERR_WIFI_NOT_STARTED",
	0x3003: "ESP_ERR_WIFI_NOT_STOPPED",
	0x3004: "ESP_ERR_WIFI_IF",
	0x3005: "ESP_ERR_WIFI_MODE",
	0x3006: "ESP_ERR_WIFI_STATE",
	0x3008: "ESP_ERR_WIFI_NVS",
	0x300A: "ESP_ERR_WIFI_ARG",
}

// EspErrName returns the ESP_ERR_* name of a raw esp_err_t, or hex.
func EspErrName(e int32) string {
	if n, ok := espErrNames[e]; ok {
		return n
	}
	return fmt.Sprintf("0x%X", uint32(e))
}
