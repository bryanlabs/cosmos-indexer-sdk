package taxapi

import "golang.org/x/sync/errgroup"

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

// goChunks queues fn in g on consecutive chunks of values, at most size
// (and bindListChunk) long, and returns the slots their results land in, in
// chunk order, to read after g.Wait.
func goChunks[T, R any](g *errgroup.Group, values []T, size int, fn func([]T) ([]R, error)) [][]R {
	size = max(1, min(size, bindListChunk))
	parts := make([][]R, (len(values)+size-1)/size)
	for i := range parts {
		chunk := values[i*size : min((i+1)*size, len(values))]
		g.Go(func() error {
			r, err := fn(chunk)
			parts[i] = r
			return err
		})
	}
	return parts
}
