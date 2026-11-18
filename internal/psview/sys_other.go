//go:build !unix

package psview

import "errors"

var errUnsupported = errors.New("not on this system")

func kill(int, bool) error { return errUnsupported }

func execResume(string, string) error { return errUnsupported }
