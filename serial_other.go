//go:build !linux

package main

import (
	"errors"
	"os"
)

func openSerial(path string) (*os.File, error) {
	return nil, errors.New("serial ports are only supported on Linux; use -tcp")
}
