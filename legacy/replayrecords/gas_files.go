package replayrecords

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// CheckCachedFiles compares retained raw envelope bytes, never normalized JSON.
// As with Stream, the offline caller must retain immutable files during intake.
func (p *ApprovedGasProfile) CheckCachedFiles(root string, height int64) error {
	for _, b := range p.profile.Blocks {
		if int64(b.Height) == height {
			base := filepath.Join(root, "cosmoshub-2", strconv.FormatInt(height/10000, 10), strconv.FormatInt(height, 10))
			for _, input := range []struct {
				suffix string
				raw    []byte
			}{{"-block.raw.gz", b.RawBlock}, {"-block_results.raw.gz", b.RawResults}} {
				f, e := os.Open(base + input.suffix)
				if e != nil {
					return invalid
				}
				z, e := gzip.NewReader(f)
				if e != nil {
					f.Close()
					return invalid
				}
				raw, e := io.ReadAll(io.LimitReader(z, 16<<20+1))
				ze := z.Close()
				fe := f.Close()
				if e != nil || ze != nil || fe != nil || len(raw) > 16<<20 || !bytes.Equal(raw, input.raw) {
					return invalid
				}
			}
			return nil
		}
	}
	return nil
}
