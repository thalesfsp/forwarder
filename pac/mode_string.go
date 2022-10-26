// Code generated by "stringer -type=Mode"; DO NOT EDIT.

package pac

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[DIRECT-0]
	_ = x[PROXY-1]
	_ = x[HTTP-2]
	_ = x[HTTPS-3]
	_ = x[SOCKS-4]
	_ = x[SOCKS4-5]
	_ = x[SOCKS5-6]
}

const _Mode_name = "DIRECTPROXYHTTPHTTPSSOCKSSOCKS4SOCKS5"

var _Mode_index = [...]uint8{0, 6, 11, 15, 20, 25, 31, 37}

func (i Mode) String() string {
	if i < 0 || i >= Mode(len(_Mode_index)-1) {
		return "Mode(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _Mode_name[_Mode_index[i]:_Mode_index[i+1]]
}
