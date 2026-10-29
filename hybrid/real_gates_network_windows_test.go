//go:build v2_real_gates && windows

package hybrid

import (
	"errors"
	"golang.org/x/sys/windows"
	"testing"
)

func i0PlatformNetworkCode(err error) string {
	switch {
	case errors.Is(err, windows.WSAECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, windows.WSAETIMEDOUT):
		return "timeout"
	case errors.Is(err, windows.WSAEINVAL), errors.Is(err, windows.WSAEAFNOSUPPORT), errors.Is(err, windows.WSAEADDRNOTAVAIL):
		return "address_invalid"
	default:
		return "unknown"
	}
}
func TestI0WindowsNetworkCodes(t *testing.T) {
	for _, tc := range []struct {
		err     error
		subtype string
	}{{windows.WSAECONNREFUSED, "connection_refused"}, {windows.WSAETIMEDOUT, "timeout"}, {windows.WSAEADDRNOTAVAIL, "address_invalid"}, {windows.ERROR_ACCESS_DENIED, "unknown"}} {
		if i0PlatformNetworkCode(tc.err) != tc.subtype {
			t.Fatal("Windows code category mismatch")
		}
	}
}
