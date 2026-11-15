// scanclosed validates every JSON record and exact gzip closure without retaining records.
package main

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func scan(path string) (map[string]interface{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, err
	}
	gz.Multistream(false)
	h := sha256.New()
	records, length := int64(0), int64(0)
	reader := bufio.NewReaderSize(gz, 1<<20)
	var last json.RawMessage
	for {
		var line []byte
		var e error
		for {
			fragment, next := reader.ReadSlice('\n')
			if len(line)+len(fragment) > 16<<20 {
				return nil, fmt.Errorf("record_buffer_limit")
			}
			line = append(line, fragment...)
			e = next
			if e != bufio.ErrBufferFull {
				break
			}
		}
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				return nil, fmt.Errorf("record_unterminated")
			}
			if !json.Valid(line) {
				return nil, fmt.Errorf("record_json")
			}
			h.Write(line)
			records++
			length += int64(len(line))
			last = append(last[:0], line...)
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("gzip_decode_or_footer")
		}
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	if _, err = br.ReadByte(); err != io.EOF {
		return nil, fmt.Errorf("gzip_trailing_data")
	}
	if len(last) == 0 {
		return nil, fmt.Errorf("completion_trailer")
	}
	var trailer map[string]interface{}
	if json.Unmarshal(last, &trailer) != nil {
		return nil, fmt.Errorf("completion_trailer")
	}
	return map[string]interface{}{"records": records, "raw_bytes": length, "raw_sha256": hex.EncodeToString(h.Sum(nil)), "completion_kind": trailer["kind"], "height": trailer["height"]}, nil
}
func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	value, err := scan(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "closed_scan_refused")
		os.Exit(1)
	}
	json.NewEncoder(os.Stdout).Encode(value)
}
