//go:build !darwin && !linux

package lockfile

import (
	"errors"
	"os"
)

func tryLock(*os.File) error { return errors.New("file locks are supported only on macOS and Linux") }
func unlock(*os.File) error  { return nil }
