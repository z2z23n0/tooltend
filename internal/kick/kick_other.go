//go:build !darwin && !linux

package kick

import "errors"

func startDetached(string, ...string) error {
	return errors.New("detached workers are supported only on macOS and Linux")
}
