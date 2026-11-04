//go:build !unix

package hisn

import "os"

func checkOwner(os.FileInfo) error { return nil }
