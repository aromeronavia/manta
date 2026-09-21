package replayfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestLoadSniffsHeader(t *testing.T) {
	dir := t.TempDir()

	junk := filepath.Join(dir, "junk.dem")
	if err := os.WriteFile(junk, []byte("not a replay at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(junk); err == nil || !strings.Contains(err.Error(), "unrecognized header") {
		t.Errorf("junk file: got err %v, want unrecognized header", err)
	}

	plain := filepath.Join(dir, "plain.bin")
	want := append([]byte("PBDEMS2\x00"), 1, 2, 3)
	if err := os.WriteFile(plain, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(plain)
	if err != nil || string(got) != string(want) {
		t.Errorf("plain demo: got %q, %v", got, err)
	}
}

func TestDecodeZstd(t *testing.T) {
	want := append([]byte("PBDEMS2\x00"), bytes.Repeat([]byte{7}, 1000)...)
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	enc.Write(want)
	enc.Close()

	got, err := Decode(&buf)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Decode zstd: len=%d err=%v", len(got), err)
	}

	junk := bytes.NewReader(append([]byte{0x28, 0xb5, 0x2f, 0xfd}, []byte("not zstd really")...))
	if _, err := Decode(junk); err == nil {
		t.Error("corrupt zstd stream should fail")
	}
}
