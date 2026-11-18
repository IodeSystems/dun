package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"github.com/iodesystems/dun/internal/fsutil"
)

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func copyFile(from, to string, perm os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(to, data, perm)
}
