package taxapi

// bindListChunk is how many values one IN ? list binds. pgx sends each value
// as its own parameter and refuses a statement with more than 65535 before it
// reaches the server, so a list sized by a wallet's history is read in
// chunks well under that.
const bindListChunk = 10000

// inChunks calls fn on consecutive slices of values, each at most
// bindListChunk long, in order, and stops at the first error.
func inChunks[T any](values []T, fn func([]T) error) error {
	for start := 0; start < len(values); start += bindListChunk {
		if err := fn(values[start:min(start+bindListChunk, len(values))]); err != nil {
			return err
		}
	}
	return nil
}
