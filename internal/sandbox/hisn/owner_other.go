//go:build !linux && !darwin

package hisn

import "os"

func checkOwner(os.FileInfo) error { return nil }
