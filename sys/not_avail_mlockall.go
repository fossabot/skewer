// +build android darwin nacl netbsd plan9 windows

package sys

func init() {
	MlockSupported = false
}

func mlockall() error {
	return nil
}
