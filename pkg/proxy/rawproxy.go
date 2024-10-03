package proxy

import (
	"errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"net"
)

func getConnFd(conn *net.UnixConn) (int, error) {
	file, err := conn.File()
	if err != nil {
		return -1, err
	}
	return int(file.Fd()), nil
}

func setNonBlocking(fd int) error {
	// Get current flags
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}

	// Add O_NONBLOCK to the flags
	_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK)
	if err != nil {
		return err
	}

	return nil
}

func forwardData(readFds *unix.FdSet, buffer *[]byte, fd int, src, dst *net.UnixConn, logger *log.Entry) error {
	if readFds.IsSet(fd) {
		n, err := src.Read(*buffer)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				// No data to read
				return nil
			}
			return err
		}

		if n > 0 {
			if _, err := dst.Write((*buffer)[:n]); err != nil {
				return err
			}
		}
	}
	return nil
}

//goland:noinspection GoNameStartsWithPackageName
func ProxyFileConn(c1, c2 *net.UnixConn, logger *log.Entry) error {
	fd1, err := getConnFd(c1)
	if err != nil {
		return err
	}
	fd2, err := getConnFd(c2)
	if err != nil {
		return err
	}

	if err := setNonBlocking(fd1); err != nil {
		return err
	}
	if err := setNonBlocking(fd2); err != nil {
		return err
	}

	buffer := make([]byte, 4096)

	for {
		// Set up file descriptor sets
		readFds := &unix.FdSet{}
		readFds.Set(fd1)
		readFds.Set(fd2)

		// Wait for either connection to have data
		_, err := unix.Select(max(fd1, fd2)+1, readFds, nil, nil, nil)
		if err != nil {
			return err
		}

		// Check if c1 is ready to read
		if err := forwardData(readFds, &buffer, fd1, c1, c2, logger); err != nil {
			return err
		}

		// Check if c2 is ready to read
		if err = forwardData(readFds, &buffer, fd2, c2, c1, logger); err != nil {
			return err
		}
	}
}
