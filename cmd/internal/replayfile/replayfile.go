// Package replayfile loads Dota 2 replay files into memory, sniffing the
// container by header bytes rather than file extension.
package replayfile

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

var (
	magicDemo  = []byte("PBDEMS2\x00")
	magicBzip2 = []byte("BZh")
	magicZstd  = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// Load reads a replay file fully into memory, transparently decompressing
// bzip2 and Zstandard. The file is sniffed by magic bytes, not by extension,
// since replays are frequently misnamed (Valve serves Zstandard data under a
// .bz2 name).
func Load(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return buf, nil
}

// Decode reads a replay from r, decompressing bzip2 or Zstandard as needed,
// and returns the raw Source 2 demo bytes.
func Decode(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	head, err := br.Peek(8)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, err
	}

	var buf []byte
	switch {
	case bytes.HasPrefix(head, magicDemo):
		buf, err = io.ReadAll(br)
	case bytes.HasPrefix(head, magicBzip2):
		buf, err = io.ReadAll(bzip2.NewReader(br))
	case bytes.HasPrefix(head, magicZstd):
		var dec *zstd.Decoder
		dec, err = zstd.NewReader(br)
		if err != nil {
			return nil, err
		}
		buf, err = io.ReadAll(dec)
		dec.Close()
	default:
		return nil, fmt.Errorf("unrecognized header %q (expected a PBDEMS2 demo, bzip2 or Zstandard)", head)
	}
	if err != nil {
		return nil, err
	}

	if !bytes.HasPrefix(buf, magicDemo) {
		return nil, fmt.Errorf("decompressed data is not a Source 2 demo (header %q)", buf[:min(8, len(buf))])
	}
	return buf, nil
}
