//go:build !darwin

package network

import (
	"errors"
	"io"
)

func RunNativeHelper(io.Reader, io.Writer) error {
	return errors.New("native private networking currently requires macOS; use infra proxy on this platform")
}
