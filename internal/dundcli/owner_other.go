//go:build !unix

package dundcli

import "io/fs"

// ownedByMe cannot be told here; an upgrade is refused.
func ownedByMe(fs.FileInfo) bool { return false }
