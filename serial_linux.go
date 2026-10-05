package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// openSerial opens the chip UART raw at 115200 8N1, blocking reads.
func openSerial(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		f.Close()
		return nil, err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS | unix.CBAUD
	t.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD | unix.B115200
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
