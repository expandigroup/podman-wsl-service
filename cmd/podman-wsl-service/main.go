package main

import (
	"errors"
	"fmt"
	"github.com/alecthomas/kong"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/mountinfo"
	log "github.com/sirupsen/logrus"
	"os"
	"podman-wsl-service/pkg/loglib"
	"podman-wsl-service/pkg/wslpath"
	"strings"
)

var Args struct {
	LogLevel        string `help:"Set the log level" default:"info"`
	UpstreamSocket  string `help:"The path to the upstream podman socket" default:"/mnt/wsl/podman-sockets/podman-machine-default/podman-root.sock"`
	DowstreamSocket string `help:"The path to the downstream podman socket" default:"/run/podman/podman.sock"`
}

func getWslDistroName() (string, error) {
	distroName := os.Getenv("WSL_DISTRO_NAME")
	if distroName != "" {
		return distroName, nil
	}
	winRootPath, err := wslpath.ToWindowsForwardSlashes("/")
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(winRootPath, "/"), "/")
	if len(parts) != 2 {
		return "", errors.New("'wslpath -am /' returned an unexpected value")
	}
	return parts[1], nil
}

func getSharedMountpoint() (string, error) {
	distroName, err := getWslDistroName()
	if err != nil {
		return "", err
	}

	mountPoint := "/mnt/wsl/distro-roots/" + distroName
	_, err = os.Stat(mountPoint)
	if os.IsNotExist(err) {
		err = os.MkdirAll(mountPoint, 0755)
	}
	if err != nil {
		return "", err
	}
	mounted, err := mountinfo.Mounted(mountPoint)
	if err != nil {
		return "", err
	}

	err = mount.MakeShared("/")
	if err != nil {
		err = fmt.Errorf("unable to make the root filesystem a shared subtree: %v", err)
		return "", err
	}

	options := "rbind,rslave"
	if mounted {
		options += ",remount"
	}

	err = mount.Mount("/", mountPoint, "", options)
	if err != nil {
		err = fmt.Errorf("unable to mount the root filesystem to %s: %v", mountPoint, err)
		return "", err
	}

	return mountPoint, nil
}

func main() {
	loglib.SetUpCliToolLogging()

	kong.Parse(&Args)
	loglib.SetLogLevel(Args.LogLevel)

	sharedRoot, err := getSharedMountpoint()
	if err != nil {
		log.Fatalf("Unable to get shared WSL mountpoint: %v\n", err)
	}

	proxy := NewPodmanProxy(sharedRoot, Args.UpstreamSocket, Args.DowstreamSocket)
	if err := proxy.TestUpstreamSocket(); err != nil {
		log.Fatalf("Unable to communicate with the upstream socket: %v\n", err)
	}
	if err := proxy.Serve(); err != nil {
		log.Fatalf("Unable to run the socket proxy: %v\n", err)
	}
}
