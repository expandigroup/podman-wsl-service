package wslpath

import "os/exec"

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func FromWindows(windowsPath string) (string, error) {
	return runCommand("wslpath", "-au", windowsPath)
}

func ToWindows(wslPath string) (string, error) {
	return runCommand("wslpath", "-aw", wslPath)
}

func ToWindowsForwardSlashes(wslPath string) (string, error) {
	return runCommand("wslpath", "-am", wslPath)
}
