//go:build !linux

package main

import (
	"errors"
	"io"
)

func openSerial(path string, rtscts bool) (io.ReadWriter, error) {
	return nil, errors.New("serial ports are only supported on Linux; use -tcp")
}
